package wire

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateRunesKeepsUTF8Valid(t *testing.T) {
	s := strings.Repeat("额度用完", 100) // 400 runes, 1200 bytes
	got := TruncateRunes(s, 300)
	if !utf8.ValidString(got) {
		t.Fatal("truncation split a UTF-8 sequence")
	}
	if n := utf8.RuneCountInString(got); n != 300 {
		t.Fatalf("runes = %d, want 300", n)
	}
	if TruncateRunes("abc", 10) != "abc" || TruncateRunes("abc", 0) != "" {
		t.Fatal("edge cases")
	}
}

// A non-BMP rune is two UTF-16 units in JS, so the cut lands where TS would.
func TestTruncateRunesCountsSurrogatePairs(t *testing.T) {
	s := "🙂🙂🙂🙂" // four emoji, eight UTF-16 units
	if got := TruncateRunes(s, 4); got != "🙂🙂" {
		t.Fatalf("n=4 got %q", got)
	}
	if got := TruncateRunes(s, 3); got != "🙂" {
		t.Fatalf("n=3 got %q", got)
	}
	if got := TruncateRunes(s, 1); got != "" {
		t.Fatalf("n=1 got %q", got)
	}
	if got := TruncateRunes(s, 8); got != s {
		t.Fatalf("n=8 got %q", got)
	}
}

func TestSoftErrorMessageCJKStaysValid(t *testing.T) {
	msg := SoftErrorMessage(strings.Repeat("上游连接被重置 ", 80))
	if !utf8.ValidString(msg) {
		t.Fatal("soft error message is not valid UTF-8")
	}
	if !strings.HasPrefix(msg, SoftErrorPrefix) {
		t.Fatalf("prefix missing: %q", msg)
	}
}
