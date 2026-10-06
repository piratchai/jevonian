package devin

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"
)

// Minimal protobuf helpers — the same hand-rolled codec src/devin.ts carries.
// The wire only needs field-level read/write, so no generated code or protobuf
// dependency is pulled in.

// Concat joins byte slices into one contiguous buffer.
func Concat(parts ...[]byte) []byte {
	var length int
	for _, part := range parts {
		length += len(part)
	}
	out := make([]byte, 0, length)
	for _, part := range parts {
		out = append(out, part...)
	}
	return out
}

// Varint encodes a non-negative integer as a protobuf varint.
func Varint(value uint64) []byte {
	var out []byte
	for value > 0x7f {
		out = append(out, byte(value&0x7f)|0x80)
		value >>= 7
	}
	return append(out, byte(value))
}

func tag(field int, wireType int) []byte {
	return Varint(uint64(field*8 + wireType))
}

// VarintField encodes `field: value` as a varint field.
func VarintField(field int, value uint64) []byte {
	return Concat(tag(field, 0), Varint(value))
}

// BytesField encodes `field: value` as a length-delimited field.
func BytesField(field int, value []byte) []byte {
	return Concat(tag(field, 2), Varint(uint64(len(value))), value)
}

// StringField is BytesField for strings.
func StringField(field int, value string) []byte {
	return BytesField(field, []byte(value))
}

// DoubleField encodes `field: value` as a 64-bit fixed field.
func DoubleField(field int, value float64) []byte {
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], math.Float64bits(value))
	return Concat(tag(field, 1), buf[:])
}

// float32Field encodes `field: value` as a 32-bit fixed field (used by price rows).
func float32Field(field int, value float32) []byte {
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], math.Float32bits(value))
	return Concat(tag(field, 5), buf[:])
}

// pbField is one decoded protobuf field: Num/Wire plus Int (wire 0) or
// Bytes (wire 1, 2, 5).
type pbField struct {
	num   int
	wire  int
	int   uint64
	bytes []byte
}

var (
	errTruncatedVarint = errors.New("devin: truncated varint")
	errVarintTooLong   = errors.New("devin: varint too long")
	errTruncatedField  = errors.New("devin: truncated field")
	errFieldZero       = errors.New("devin: invalid field number 0")
)

// decodePb decodes one protobuf message level. It fails on truncation or
// group wire types, mirroring decodePb in src/devin.ts.
func decodePb(buf []byte) ([]pbField, error) {
	var out []pbField
	pos := 0
	readVarint := func() (uint64, error) {
		var result uint64
		var shift uint
		for i := 0; i < 10; i++ {
			if pos >= len(buf) {
				return 0, errTruncatedVarint
			}
			b := buf[pos]
			pos++
			result |= uint64(b&0x7f) << shift
			if b&0x80 == 0 {
				return result, nil
			}
			shift += 7
		}
		return 0, errVarintTooLong
	}
	take := func(length int) ([]byte, error) {
		if pos+length > len(buf) {
			return nil, errTruncatedField
		}
		b := buf[pos : pos+length]
		pos += length
		return b, nil
	}
	for pos < len(buf) {
		key, err := readVarint()
		if err != nil {
			return nil, err
		}
		num := int(key >> 3)
		wire := int(key & 7)
		if num == 0 {
			return nil, errFieldZero
		}
		switch wire {
		case 0:
			v, err := readVarint()
			if err != nil {
				return nil, err
			}
			out = append(out, pbField{num: num, wire: wire, int: v})
		case 2:
			length, err := readVarint()
			if err != nil {
				return nil, err
			}
			b, err := take(int(length))
			if err != nil {
				return nil, err
			}
			out = append(out, pbField{num: num, wire: wire, bytes: b})
		case 1:
			b, err := take(8)
			if err != nil {
				return nil, err
			}
			out = append(out, pbField{num: num, wire: wire, bytes: b})
		case 5:
			b, err := take(4)
			if err != nil {
				return nil, err
			}
			out = append(out, pbField{num: num, wire: wire, bytes: b})
		default:
			return nil, fmt.Errorf("devin: unsupported wire type %d", wire)
		}
	}
	return out, nil
}

// tryDecodePb is decodePb returning nil instead of an error.
func tryDecodePb(buf []byte) []pbField {
	if buf == nil {
		return nil
	}
	fields, err := decodePb(buf)
	if err != nil {
		return nil
	}
	return fields
}

func pbBytes(fields []pbField, num int) []byte {
	for _, f := range fields {
		if f.num == num && f.wire == 2 {
			return f.bytes
		}
	}
	return nil
}

func pbString(fields []pbField, num int) (string, bool) {
	b := pbBytes(fields, num)
	if b == nil {
		return "", false
	}
	return string(b), true
}

func pbInt(fields []pbField, num int) (uint64, bool) {
	for _, f := range fields {
		if f.num == num && f.wire == 0 {
			return f.int, true
		}
	}
	return 0, false
}

func pbFloat32(fields []pbField, num int) (float64, bool) {
	for _, f := range fields {
		if f.num == num && f.wire == 5 && len(f.bytes) == 4 {
			v := math.Float32frombits(binary.LittleEndian.Uint32(f.bytes))
			return float64(v), true
		}
	}
	return 0, false
}

// pbSub decodes the length-delimited field `num` as a nested message.
func pbSub(fields []pbField, num int) []pbField {
	return tryDecodePb(pbBytes(fields, num))
}

// utf8Stream decodes UTF-8 incrementally like the TS TextDecoder({stream:true}):
// a trailing incomplete sequence is held back until more bytes (or flush)
// arrive; invalid bytes become U+FFFD.
type utf8Stream struct {
	pending []byte
}

func (d *utf8Stream) decode(chunk []byte) string {
	buf := append(d.pending, chunk...)
	cut := len(buf) - incompleteTail(buf)
	d.pending = append([]byte(nil), buf[cut:]...)
	return strings.ToValidUTF8(string(buf[:cut]), "\uFFFD")
}

func (d *utf8Stream) flush() string {
	if len(d.pending) == 0 {
		return ""
	}
	out := strings.ToValidUTF8(string(d.pending), "\uFFFD")
	d.pending = nil
	return out
}

// incompleteTail is how many trailing bytes of buf begin a UTF-8 sequence that
// is not finished yet.
func incompleteTail(buf []byte) int {
	n := len(buf)
	for k := 1; k <= 3 && k <= n; k++ {
		b := buf[n-k]
		if b&0xc0 == 0x80 {
			continue
		}
		need := 0
		switch {
		case b&0xe0 == 0xc0:
			need = 2
		case b&0xf0 == 0xe0:
			need = 3
		case b&0xf8 == 0xf0:
			need = 4
		}
		if need > k {
			return k
		}
		return 0
	}
	return 0
}
