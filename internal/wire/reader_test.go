package wire_test

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/xinyao27/jevonian/internal/wire"
)

type echoTranslator struct{}

func (echoTranslator) Handle(e wire.Body, sink wire.EventSink) { sink.EmitData(e) }
func (echoTranslator) Finish(sink wire.EventSink)              { sink.Emit(wire.SSEDone) }

func TestTranslatedReaderPreservesSmallReadsAndTerminalError(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(map[bool]string{false: "eof", true: "transport-error"}[failure], func(t *testing.T) {
			input := "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n"
			source := io.ReadCloser(io.NopCloser(strings.NewReader(input)))
			boom := errors.New("transport lost")
			if failure {
				source = &errorTail{reader: strings.NewReader(input), err: boom}
			}
			r := wire.TranslateReader(source, echoTranslator{})
			defer r.Close()
			var got strings.Builder
			buf := make([]byte, 1)
			for {
				n, err := r.Read(buf)
				got.Write(buf[:n])
				if err != nil {
					if failure && !errors.Is(err, boom) {
						t.Fatal(err)
					}
					break
				}
			}
			if !strings.Contains(got.String(), "hello") {
				t.Fatalf("small reads dropped data: %s", got.String())
			}
			if strings.Contains(got.String(), "[DONE]") == failure {
				t.Fatalf("terminal error concealed: %s", got.String())
			}
		})
	}
}

type errorTail struct {
	reader io.Reader
	err    error
}

func (r *errorTail) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if err == io.EOF {
		return n, r.err
	}
	return n, err
}
func (*errorTail) Close() error { return nil }
