package cursor

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"math"
	"sort"
)

// Minimal protobuf helpers, hand-rolled like src/cursor.ts (class Pb +
// pbFields). The wire only needs field-level read/write; no protobuf
// dependency and no CGO.

// Pb builds a protobuf message field by field.
type Pb struct {
	buf bytes.Buffer
}

// Varint appends a varint field.
func (p *Pb) Varint(field int, value uint64) *Pb {
	p.buf.Write(cursorTag(field, 0))
	p.buf.Write(cursorVarint(value))
	return p
}

// Bytes appends a length-delimited field.
func (p *Pb) Bytes(field int, value []byte) *Pb {
	p.buf.Write(cursorTag(field, 2))
	p.buf.Write(cursorVarint(uint64(len(value))))
	p.buf.Write(value)
	return p
}

// Str appends a string field.
func (p *Pb) Str(field int, value string) *Pb { return p.Bytes(field, []byte(value)) }

// Double appends a fixed64 field.
func (p *Pb) Double(field int, value float64) *Pb {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], math.Float64bits(value))
	p.buf.Write(cursorTag(field, 1))
	p.buf.Write(b[:])
	return p
}

// Build returns the encoded message.
func (p *Pb) Build() []byte { return p.buf.Bytes() }

// cursorVarint encodes a non-negative integer as a protobuf varint.
func cursorVarint(value uint64) []byte {
	var out []byte
	for value > 0x7f {
		out = append(out, byte(value&0x7f)|0x80)
		value >>= 7
	}
	return append(out, byte(value))
}

func cursorTag(field, wireType int) []byte { return cursorVarint(uint64(field*8 + wireType)) }

// pbField is one decoded field: Num/Wire, plus N (varint) or Data (fixed/LEN).
type pbField struct {
	num  int
	wire int
	n    uint64
	data []byte
}

// readVarint reads a varint from buf starting at pos.
func readVarint(buf []byte, pos int) (value uint64, next int, ok bool) {
	var shift uint
	for i := 0; i < 10; i++ {
		index := pos + i
		if index >= len(buf) {
			return 0, 0, false
		}
		b := buf[index]
		value |= uint64(b&0x7f) << shift
		if b&0x80 == 0 {
			return value, index + 1, true
		}
		shift += 7
	}
	return 0, 0, false
}

// pbFields decodes one message level; a malformed tail is dropped (matching
// the TS decoder, which breaks rather than throws).
func pbFields(buf []byte) []pbField {
	var out []pbField
	pos := 0
	for pos < len(buf) {
		key, next, ok := readVarint(buf, pos)
		if !ok {
			break
		}
		pos = next
		num := int(key >> 3)
		wireType := int(key & 7)
		if num == 0 {
			break
		}
		switch wireType {
		case 0:
			v, next, ok := readVarint(buf, pos)
			if !ok {
				return out
			}
			out = append(out, pbField{num: num, wire: wireType, n: v})
			pos = next
		case 2:
			l, next, ok := readVarint(buf, pos)
			if !ok {
				return out
			}
			pos = next
			if pos+int(l) > len(buf) {
				return out
			}
			out = append(out, pbField{num: num, wire: wireType, data: buf[pos : pos+int(l)]})
			pos += int(l)
		case 1:
			if pos+8 > len(buf) {
				return out
			}
			out = append(out, pbField{num: num, wire: wireType, data: buf[pos : pos+8]})
			pos += 8
		case 5:
			if pos+4 > len(buf) {
				return out
			}
			out = append(out, pbField{num: num, wire: wireType, data: buf[pos : pos+4]})
			pos += 4
		default:
			return out
		}
	}
	return out
}

func pbStr(fields []pbField, num int) (string, bool) {
	for _, f := range fields {
		if f.num == num && f.wire == 2 {
			return string(f.data), true
		}
	}
	return "", false
}

func pbNum(fields []pbField, num int) (uint64, bool) {
	for _, f := range fields {
		if f.num == num && f.wire == 0 {
			return f.n, true
		}
	}
	return 0, false
}

// pbValue encodes a google.protobuf.Value.
func pbValue(value any) []byte {
	switch v := value.(type) {
	case nil:
		return new(Pb).Varint(1, 0).Build()
	case string:
		return new(Pb).Str(3, v).Build()
	case bool:
		if v {
			return new(Pb).Varint(4, 1).Build()
		}
		return new(Pb).Varint(4, 0).Build()
	case []any:
		list := new(Pb)
		for _, entry := range v {
			list.Bytes(1, pbValue(entry))
		}
		return new(Pb).Bytes(6, list.Build()).Build()
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		st := new(Pb)
		for _, k := range keys {
			st.Bytes(1, new(Pb).Str(1, k).Bytes(2, pbValue(v[k])).Build())
		}
		return new(Pb).Bytes(5, st.Build()).Build()
	default:
		// Numbers (and any other JSON scalar) land in field 2.
		if isNumber(v) {
			return new(Pb).Double(2, toNumber(v)).Build()
		}
		return new(Pb).Varint(1, 0).Build()
	}
}

// pbAny decodes a google.protobuf.Value.
func pbAny(buf []byte) any {
	for _, f := range pbFields(buf) {
		switch {
		case f.num == 1:
			return nil
		case f.num == 2 && f.wire == 1 && f.data != nil:
			return math.Float64frombits(binary.LittleEndian.Uint64(f.data))
		case f.num == 3 && f.data != nil:
			return string(f.data)
		case f.num == 4:
			return f.n != 0
		case f.num == 5 && f.data != nil:
			out := map[string]any{}
			for _, entry := range pbFields(f.data) {
				if entry.num != 1 || entry.data == nil {
					continue
				}
				var key string
				var value any
				for _, kv := range pbFields(entry.data) {
					if kv.num == 1 && kv.data != nil {
						key = string(kv.data)
					}
					if kv.num == 2 && kv.data != nil {
						value = pbAny(kv.data)
					}
				}
				out[key] = value
			}
			return out
		case f.num == 6 && f.data != nil:
			list := []any{}
			for _, entry := range pbFields(f.data) {
				if entry.num == 1 && entry.data != nil {
					list = append(list, pbAny(entry.data))
				}
			}
			return list
		}
	}
	return nil
}

func isNumber(v any) bool {
	switch v.(type) {
	case float64, float32, int, int64, uint64, json.Number:
		return true
	}
	return false
}

func toNumber(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case uint64:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	}
	return 0
}
