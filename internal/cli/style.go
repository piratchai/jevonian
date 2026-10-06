package cli

import (
	"encoding/json"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-isatty"
	"github.com/muesli/termenv"
)

// Subtle palette shared by every command. Colors adapt to light and dark
// terminal themes. Styles are constructed per-call through ansiStyles so tests
// and non-TTY environments never inherit a stale global profile.
const (
	colorAccentLight  = "#2457A6"
	colorAccentDark   = "#8AB4F8"
	colorSuccessLight = "#18794E"
	colorSuccessDark  = "#63D297"
	colorWarnLight    = "#9A6700"
	colorWarnDark     = "#E3B341"
	colorErrorLight   = "#B42318"
	colorErrorDark    = "#F47067"
	colorDimLight     = "#6E7781"
	colorDimDark      = "#8B949E"
)

type palette struct {
	accent, success, warn, err, dim, emph lipgloss.Style
}

// ansiStyles builds the palette on a renderer bound to w with the color
// profile pinned to ANSI 256. The caller only invokes it after confirming a
// color TTY, so rendering never depends on ambient profile detection.
func ansiStyles(w io.Writer) palette {
	r := lipgloss.NewRenderer(w)
	r.SetColorProfile(termenv.ANSI256)
	return palette{
		accent:  r.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: colorAccentLight, Dark: colorAccentDark}),
		success: r.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: colorSuccessLight, Dark: colorSuccessDark}),
		warn:    r.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: colorWarnLight, Dark: colorWarnDark}),
		err:     r.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: colorErrorLight, Dark: colorErrorDark}),
		dim:     r.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: colorDimLight, Dark: colorDimDark}),
		emph:    r.NewStyle().Bold(true),
	}
}

// termEnv records whether styledWriter wrapped each stream. ANSI lives only in
// styled calls, so --json payloads and non-TTY output are byte-identical.
type termEnv struct {
	out, err bool
}

// newTermEnv wraps real terminals in styledWriter; anything else is returned
// unchanged so injected writers and tests never see escape codes.
func newTermEnv(out, errOut io.Writer) (io.Writer, io.Writer, termEnv) {
	return styledWriter(out), styledWriter(errOut), termEnv{
		out: isColorTTY(out),
		err: isColorTTY(errOut),
	}
}

func isColorTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && colorEnabled() && isatty.IsTerminal(f.Fd())
}

func styledWriter(w io.Writer) io.Writer {
	if !isColorTTY(w) {
		return w
	}
	return terminalWriter{Writer: w}
}

func rawWriter(w io.Writer) io.Writer {
	if tw, ok := w.(terminalWriter); ok {
		return tw.Writer
	}
	return w
}

func colorEnabled() bool {
	return os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb"
}

func (c commandContext) styleValue(value string) string {
	if !c.term.out {
		return value
	}
	return ansiStyles(rawWriter(c.out)).accent.Render(value)
}

// terminalWriter colors fixed line prefixes produced by the CLI. It leaves the
// rest of each line untouched, so values, URLs, tables, and keys stay intact.
type terminalWriter struct {
	io.Writer
}

// statusLabels are the fixed words this CLI prints before a colon or space.
var statusLabels = []string{
	"dashboard", "pid", "log", "stop", "status", "foreground",
	"label", "plist", "loaded", "binary", "detail", "lan",
	"config", "data", "ledger", "catalog", "pricing", "routing",
	"baseline", "brains", "service", "tunnel", "quota", "refresh",
	"key", "env", "auth", "billing", "login", "next", "models",
	"leaderboard", "provider", "providers", "identity", "actual",
	"savings", "remaining", "balance", "spend", "note", "token saver",
}

func (w terminalWriter) Write(p []byte) (int, error) {
	text := string(p)
	if looksLikeJSON(text) {
		return writeOriginal(w.Writer, p)
	}
	// terminalWriter only wraps a real color TTY; ansiStyles pins ANSI output
	// even when the global renderer resolved Ascii off a non-terminal start.
	pal := ansiStyles(w.Writer)
	for _, label := range statusLabels {
		text = styleLinePrefix(text, label+":", pal.accent)
	}
	text = styleLinePrefix(text, "warning:", pal.warn)
	text = styleLinePrefix(text, "error:", pal.err)
	text = styleLinePrefix(text, "failed:", pal.err)
	text = styleLinePrefix(text, "No ", pal.dim)
	text = styleLinePrefix(text, "jevonian ", pal.emph)
	for _, word := range []string{"Added provider", "Removed provider", "Created", "Jevonian service", "Jevonian is now running"} {
		text = styleLinePrefix(text, word, pal.success)
	}
	for _, word := range []string{"listening", "running", "started", "restarted", "added", "removed", "created", "stopped", "saved", "configured", "enabled", "disabled", "up to date", "signed in", "wrote"} {
		text = styleLinePrefix(text, word, pal.success)
	}
	for _, word := range []string{"fetching", "refreshing", "probing", "updating", "cloning", "installing", "waiting", "starting"} {
		text = styleLinePrefix(text, word, pal.accent)
	}
	return writeStyled(w.Writer, p, []byte(text))
}

// writeOriginal preserves the io.Writer byte-count contract for unchanged data.
func writeOriginal(dst io.Writer, src []byte) (int, error) {
	n, err := dst.Write(src)
	if n > len(src) {
		return 0, io.ErrShortWrite
	}
	return n, err
}

// writeStyled maps successful output back to the original input byte count.
// If the destination short-writes, report an error because styled byte offsets
// cannot be mapped safely to the unstyled input.
func writeStyled(dst io.Writer, src, rendered []byte) (int, error) {
	n, err := dst.Write(rendered)
	if err != nil {
		return 0, err
	}
	if n != len(rendered) {
		return 0, io.ErrShortWrite
	}
	return len(src), nil
}

// looksLikeJSON protects JSON payloads even when a terminal writer is used.
// Only a complete write containing a JSON object or array is bypassed.
func looksLikeJSON(text string) bool {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "{") && !strings.HasPrefix(trimmed, "[") {
		return false
	}
	return json.Valid([]byte(trimmed))
}

// styleLinePrefix styles `prefix` when it begins a line (after \n or start).
func styleLinePrefix(text, prefix string, style lipgloss.Style) string {
	need := len(prefix)
	var b strings.Builder
	b.Grow(len(text) + 16)
	i := 0
	start := true // at line start
	for i < len(text) {
		if start && strings.HasPrefix(text[i:], prefix) {
			b.WriteString(style.Render(strings.TrimSuffix(prefix, ":")))
			if strings.HasSuffix(prefix, ":") {
				b.WriteString(":")
			}
			i += need
			start = false
			continue
		}
		c := text[i]
		b.WriteByte(c)
		i++
		start = c == '\n'
	}
	return b.String()
}
