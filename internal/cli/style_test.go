package cli

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

func TestStyledWriterPlainWriterUnchanged(t *testing.T) {
	var buf bytes.Buffer
	w := styledWriter(&buf)
	if _, ok := w.(terminalWriter); ok {
		t.Fatal("non-TTY writer was wrapped")
	}
}

func TestColorDisabledForNoColorAndDumbTerm(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	if colorEnabled() {
		t.Fatal("color enabled with NO_COLOR")
	}
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "dumb")
	if colorEnabled() {
		t.Fatal("color enabled with TERM=dumb")
	}
}

func TestStyledWriterNoColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	var buf bytes.Buffer
	if _, ok := styledWriter(&buf).(terminalWriter); ok {
		t.Fatal("writer wrapped despite NO_COLOR")
	}
}

func TestTerminalWriterColorsPrefixes(t *testing.T) {
	var buf bytes.Buffer
	tw := terminalWriter{Writer: &buf}
	_, err := tw.Write([]byte("dashboard: http://localhost:1234/\nwarning: something\n"))
	if err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	// Lip Gloss emits ANSI only with a terminal-aware renderer; on the default
	// renderer output may be plain under tests. Assert structure either way.
	if plain := stripANSI(got); !strings.Contains(plain, "dashboard:") || !strings.Contains(plain, "warning:") {
		t.Fatalf("labels lost: %q", plain)
	}
	if strings.Count(got, "http://localhost:1234/") != 1 {
		t.Fatalf("value corrupted: %q", got)
	}
}

func TestTerminalWriterNeverTouchesJSON(t *testing.T) {
	var buf bytes.Buffer
	tw := terminalWriter{Writer: &buf}
	payload := `{"warning":"x","key":"y","pid":1,"dashboard":"z"}`
	for _, value := range []string{payload + "\n", "[\n  {\"warning\": \"x\"}\n]\n"} {
		buf.Reset()
		if _, err := tw.Write([]byte(value)); err != nil {
			t.Fatal(err)
		}
		if buf.String() != value {
			t.Fatalf("JSON payload modified: got %q want %q", buf.String(), value)
		}
	}
}

func TestStderrIsTTYUnwrapsStyledWriter(t *testing.T) {
	if stderrIsTTY(terminalWriter{Writer: os.Stderr}) != stderrIsTTY(os.Stderr) {
		t.Fatal("wrapped stderr changed TTY detection")
	}
}

func TestRenderVersionNonTTY(t *testing.T) {
	if got := renderVersion(false, &bytes.Buffer{}); got != "jevonian "+Version {
		t.Fatalf("plain version changed: %q", got)
	}
}

func TestRenderHelpNonTTYIdentical(t *testing.T) {
	if got := renderHelp(false, &bytes.Buffer{}); got != helpText {
		t.Fatal("plain help text changed")
	}
}

func TestRenderHelpTTYPreservesWords(t *testing.T) {
	var buf bytes.Buffer
	got := renderHelp(true, &buf)
	// Strip ANSI and confirm the visible text equals the original help.
	if plain := stripANSI(got); plain != helpText {
		t.Fatalf("styled help changed visible text: got %q want %q", plain, helpText)
	}
	if got == helpText {
		t.Fatal("styled help emitted no ANSI")
	}
}

func TestRenderVersionTTY(t *testing.T) {
	var buf bytes.Buffer
	got := renderVersion(true, &buf)
	if stripANSI(got) != "jevonian "+Version {
		t.Fatalf("styled version changed text: %q", got)
	}
	if !strings.Contains(got, "\x1b[") {
		t.Fatalf("expected ANSI in styled version: %q", got)
	}
}

type shortWriter struct {
	limit int
}

func (w shortWriter) Write(p []byte) (int, error) {
	if len(p) > w.limit {
		return w.limit, nil
	}
	return len(p), nil
}

func TestTerminalWriterReportsInputBytes(t *testing.T) {
	var buf bytes.Buffer
	tw := terminalWriter{Writer: &buf}
	input := []byte("dashboard: http://x/\\n")
	n, err := tw.Write(input)
	if err != nil || n != len(input) {
		t.Fatalf("Write() = %d, %v; want %d, nil", n, err, len(input))
	}
	if _, err := (terminalWriter{Writer: shortWriter{limit: 1}}).Write(input); err != io.ErrShortWrite {
		t.Fatalf("short write error = %v, want %v", err, io.ErrShortWrite)
	}
}

func TestTerminalWriterEmitsANSIOnLabels(t *testing.T) {
	var buf bytes.Buffer
	tw := terminalWriter{Writer: &buf}
	if _, err := tw.Write([]byte("dashboard: http://x/\nwarning: y\nplain json {\"pid\":1}\n")); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if !strings.Contains(got, "\x1b[") {
		t.Fatalf("expected ANSI: %q", got)
	}
	if stripANSI(got) != "dashboard: http://x/\nwarning: y\nplain json {\"pid\":1}\n" {
		t.Fatalf("visible text changed: %q", stripANSI(got))
	}
}

func TestStyleLinePrefixOnlyLineStart(t *testing.T) {
	pal := ansiStyles(&bytes.Buffer{})
	s := styleLinePrefix("a config: b\nconfig: c", "config:", pal.accent)
	// Only the line-start occurrence may gain escape codes; mid-line must not.
	if stripANSI(s) != "a config: b\nconfig: c" {
		t.Fatalf("visible text changed: %q", stripANSI(s))
	}
	if strings.Count(s, "config:") != 1 {
		t.Fatalf("mid-line occurrence modified: %q", s)
	}
}

// stripANSI removes CSI sequences for visible-text assertions.
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
