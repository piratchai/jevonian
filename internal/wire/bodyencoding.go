package wire

import (
	"compress/flate"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
)

// MaxBodyBytes bounds a decoded request body. Codex bodies are large (full
// transcripts plus tool schemas), but a decompression bomb must not exhaust
// memory, so the decoded size is checked rather than trusted.
// Port of MAX_BODY_BYTES in src/body-encoding.ts.
const MaxBodyBytes = 64 * 1024 * 1024

// ErrBodyTooLarge is returned when a decoded body exceeds MaxBodyBytes.
var ErrBodyTooLarge = fmt.Errorf("request body exceeds %d bytes", MaxBodyBytes)

// Decoder wraps a compressed reader into a plain one.
type Decoder func(io.Reader) (io.Reader, error)

var (
	decodersMu sync.RWMutex
	decoders   = map[string]Decoder{
		"gzip":   func(r io.Reader) (io.Reader, error) { return gzip.NewReader(r) },
		"x-gzip": func(r io.Reader) (io.Reader, error) { return gzip.NewReader(r) },
		// TS uses zlib's inflateSync, which expects the zlib wrapper; real
		// clients that say "deflate" send either form, so accept both below.
		"deflate": nil,
	}
)

// RegisterDecoder installs a decoder for a content-coding. The standard library
// has no zstd or brotli decoder; the server wires "zstd" (Codex compresses
// /v1/responses with it) and "br" here once a decoder dependency is available.
func RegisterDecoder(name string, d Decoder) {
	decodersMu.Lock()
	defer decodersMu.Unlock()
	decoders[strings.ToLower(strings.TrimSpace(name))] = d
}

func decoderFor(name string) (Decoder, bool) {
	decodersMu.RLock()
	defer decodersMu.RUnlock()
	d, ok := decoders[name]
	if name == "deflate" {
		return inflate, true
	}
	return d, ok && d != nil
}

// inflate reads a zlib-wrapped deflate stream, falling back to raw deflate.
func inflate(r io.Reader) (io.Reader, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if zr, zerr := newZlibReader(data); zerr == nil {
		return zr, nil
	}
	return flate.NewReader(bytesReader(data)), nil
}

// DecodeBody decodes a request body according to its Content-Encoding header.
//
// Encodings are applied in reverse order because the last one listed was applied
// last by the sender (RFC 9110). "identity" and empty entries are ignored. An
// unknown encoding is an error, not garbage passed through, and every stage is
// size-checked against MaxBodyBytes. Port of decodeBody in src/body-encoding.ts.
func DecodeBody(raw []byte, contentEncoding string) ([]byte, error) {
	var encodings []string
	for _, part := range strings.Split(contentEncoding, ",") {
		part = strings.ToLower(strings.TrimSpace(part))
		if part != "" && part != "identity" {
			encodings = append(encodings, part)
		}
	}
	if len(encodings) == 0 {
		return checkSize(raw)
	}
	decoded := raw
	for i := len(encodings) - 1; i >= 0; i-- {
		decode, ok := decoderFor(encodings[i])
		if !ok {
			return nil, fmt.Errorf("unsupported content-encoding: %s", encodings[i])
		}
		reader, err := decode(bytesReader(decoded))
		if err != nil {
			return nil, err
		}
		out, err := io.ReadAll(io.LimitReader(reader, MaxBodyBytes+1))
		if err != nil {
			return nil, err
		}
		if c, ok := reader.(io.Closer); ok {
			_ = c.Close()
		}
		if decoded, err = checkSize(out); err != nil {
			return nil, err
		}
	}
	return decoded, nil
}

func checkSize(b []byte) ([]byte, error) {
	if len(b) > MaxBodyBytes {
		return nil, errors.Join(ErrBodyTooLarge)
	}
	return b, nil
}
