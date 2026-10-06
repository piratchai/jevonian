package wire

import (
	"bytes"
	"compress/zlib"
	"io"
)

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

func newZlibReader(data []byte) (io.Reader, error) {
	return zlib.NewReader(bytes.NewReader(data))
}
