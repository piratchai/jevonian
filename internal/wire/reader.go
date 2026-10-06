package wire

import (
	"bytes"
	"io"
	"strings"
	"sync"
)

// TranslateReader owns source and translates incrementally on demand. Unlike
// the writer-based TranslatorStream it needs no producer goroutine and retains
// partial output when the caller uses a small read buffer. Close unblocks an
// upstream read without waiting for the reader's state lock.
func TranslateReader(source io.ReadCloser, translator Translator) io.ReadCloser {
	return &translatedReader{source: source, translator: translator}
}

type translatedReader struct {
	source     io.ReadCloser
	translator Translator
	pending    bytes.Buffer
	rest       string
	terminal   error
	once       sync.Once
	closeErr   error
}

func (r *translatedReader) Emit(b []byte)         { r.pending.Write(b) }
func (r *translatedReader) EmitData(payload any)  { r.Emit(SSEData(payload)) }
func (r *translatedReader) EmitEvent(payload any) { r.Emit(SSEEvent(payload)) }
func (r *translatedReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for r.pending.Len() == 0 {
		if r.terminal != nil {
			return 0, r.terminal
		}
		var buf [32 * 1024]byte
		n, err := r.source.Read(buf[:])
		if n > 0 {
			parsed := SplitSseEvents(r.rest + string(buf[:n]))
			r.rest = parsed.Rest
			for _, e := range parsed.Events {
				r.translator.Handle(e, r)
			}
		}
		if err != nil {
			r.terminal = err
			if err == io.EOF {
				if strings.TrimSpace(r.rest) != "" {
					for _, e := range SplitSseEvents(r.rest + "\n\n").Events {
						r.translator.Handle(e, r)
					}
				}
				r.translator.Finish(r)
			}
			r.rest = ""
		}
	}
	return r.pending.Read(p)
}
func (r *translatedReader) Close() error {
	r.once.Do(func() { r.closeErr = r.source.Close() })
	return r.closeErr
}
