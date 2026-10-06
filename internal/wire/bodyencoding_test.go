package wire_test

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/xinyao27/jevonian/internal/wire"
)

func gz(t *testing.T, s string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	_, _ = w.Write([]byte(s))
	_ = w.Close()
	return b.Bytes()
}

// Ports of src/body-encoding.test.ts (zstd and br need a decoder dependency the
// module does not carry yet; the registry hook is exercised with a stand-in).
func TestDecodeBody(t *testing.T) {
	raw := []byte(`{"model":"jevonian/auto"}`)
	for _, enc := range []string{"", "identity", " identity ,"} {
		got, err := wire.DecodeBody(raw, enc)
		if err != nil || !bytes.Equal(got, raw) {
			t.Fatalf("encoding %q: %q %v", enc, got, err)
		}
	}
	const json = `{"model":"m"}`
	if got, err := wire.DecodeBody(gz(t, json), "gzip"); err != nil || string(got) != json {
		t.Fatalf("gzip: %q %v", got, err)
	}
	if got, err := wire.DecodeBody(gz(t, json), " X-GZIP "); err != nil || string(got) != json {
		t.Fatalf("x-gzip / casing: %q %v", got, err)
	}
	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	_, _ = zw.Write([]byte(json))
	_ = zw.Close()
	if got, err := wire.DecodeBody(z.Bytes(), "deflate"); err != nil || string(got) != json {
		t.Fatalf("deflate (zlib): %q %v", got, err)
	}
	var f bytes.Buffer
	fw, _ := flate.NewWriter(&f, flate.DefaultCompression)
	_, _ = fw.Write([]byte(json))
	_ = fw.Close()
	if got, err := wire.DecodeBody(f.Bytes(), "deflate"); err != nil || string(got) != json {
		t.Fatalf("deflate (raw): %q %v", got, err)
	}
}

func TestDecodeBodyAppliesEncodingsInReverse(t *testing.T) {
	// "x-fake, gzip": the sender applied x-fake first, then gzip, so gzip is undone first.
	wire.RegisterDecoder("x-fake", func(r io.Reader) (io.Reader, error) {
		data, _ := io.ReadAll(r)
		return strings.NewReader(strings.TrimPrefix(string(data), "FAKE:")), nil
	})
	body := gz(t, "FAKE:"+`{"a":1}`)
	got, err := wire.DecodeBody(body, "x-fake, gzip")
	if err != nil || string(got) != `{"a":1}` {
		t.Fatalf("got %q %v", got, err)
	}
}

func TestDecodeBodyRejectsUnknownAndCorrupt(t *testing.T) {
	if _, err := wire.DecodeBody([]byte("{}"), "snappy"); err == nil || !strings.Contains(err.Error(), "unsupported content-encoding") {
		t.Fatalf("unknown encoding: %v", err)
	}
	if _, err := wire.DecodeBody([]byte("not gzip at all"), "gzip"); err == nil {
		t.Fatal("corrupt body must error")
	}
}

func TestDecodeBodyCapsDecodedSize(t *testing.T) {
	huge := strings.Repeat("a", wire.MaxBodyBytes+1)
	compressed := gz(t, huge)
	if len(compressed) > 1<<20 {
		t.Fatalf("fixture too large: %d", len(compressed))
	}
	_, err := wire.DecodeBody(compressed, "gzip")
	if !errors.Is(err, wire.ErrBodyTooLarge) {
		t.Fatalf("got %v", err)
	}
	if _, err := wire.DecodeBody(make([]byte, wire.MaxBodyBytes+1), ""); !errors.Is(err, wire.ErrBodyTooLarge) {
		t.Fatalf("plain oversize: %v", err)
	}
}
