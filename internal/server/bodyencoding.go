package server

import (
	"io"
	"net/http"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	"github.com/xinyao27/jevonian/internal/wire"
)

func init() {
	wire.RegisterDecoder("zstd", func(r io.Reader) (io.Reader, error) {
		d, err := zstd.NewReader(r, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(wire.MaxBodyBytes))
		if err != nil {
			return nil, err
		}
		return d.IOReadCloser(), nil
	})
	wire.RegisterDecoder("br", func(r io.Reader) (io.Reader, error) { return brotli.NewReader(r), nil })
}

// readRequestBody bounds both encoded and decoded bytes before JSON parsing.
func readRequestBody(r *http.Request) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, wire.MaxBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > wire.MaxBodyBytes {
		return nil, wire.ErrBodyTooLarge
	}
	return wire.DecodeBody(raw, r.Header.Get("Content-Encoding"))
}
