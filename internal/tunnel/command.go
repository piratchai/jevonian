// Package tunnel runs the public tunnel (cloudflared quick tunnel, ngrok, or a
// custom command) that exposes Jevonian's /v1 surface, and the LAN listener
// helpers. Port of src/tunnel.ts, src/lan.ts, and src/user-path.ts.
package tunnel

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/xinyao27/jevonian/internal/config"
)

// Provider is a tunnel backend.
type Provider = string

const (
	ProviderCloudflare Provider = "cloudflare"
	ProviderNgrok      Provider = "ngrok"
	ProviderCustom     Provider = "custom"
)

// Status is the tunnel lifecycle state.
type Status string

const (
	StatusOff      Status = "off"
	StatusStarting Status = "starting"
	StatusOn       Status = "on"
	StatusError    Status = "error"
)

var urlPatterns = map[Provider]*regexp.Regexp{
	ProviderCloudflare: regexp.MustCompile(`https://[a-z0-9-]+\.trycloudflare\.com`),
	// free static domains use .ngrok-free.dev; random tunnels still use
	// .ngrok-free.app / .ngrok.app
	ProviderNgrok:  regexp.MustCompile(`https://[a-z0-9-]+\.ngrok(?:-free)?\.(?:app|dev)`),
	ProviderCustom: regexp.MustCompile("https?://[^\\s\"'`]+"),
}

// ExtractURL finds the provider's public URL in process output, or "".
func ExtractURL(provider Provider, text string) string {
	re, ok := urlPatterns[provider]
	if !ok {
		re = urlPatterns[ProviderCloudflare]
	}
	return re.FindString(text)
}

// UsesConfiguredURL reports whether cfg.URL is a known stable address (named /
// reserved), not a discovered quick-tunnel host.
func UsesConfiguredURL(cfg config.TunnelConfig) bool {
	return cfg.URL != "" && (cfg.Provider == ProviderCustom || cfg.Provider == ProviderNgrok)
}

// Command is the binary and argv for a tunnel.
type Command struct {
	Name string
	Args []string
}

// Label is the `name args…` string recorded and matched against `ps`.
func (c Command) Label() string {
	return strings.TrimSpace(c.Name + " " + strings.Join(c.Args, " "))
}

// BuildCommand returns the command a tunnel runs for publicPort.
func BuildCommand(cfg config.TunnelConfig, publicPort int) (Command, error) {
	if cfg.Provider == ProviderCustom || cfg.Command != "" {
		if cfg.Command == "" {
			return Command{}, fmt.Errorf("custom tunnel needs a command")
		}
		return Command{
			Name: "sh",
			Args: []string{"-c", strings.ReplaceAll(cfg.Command, "{port}", fmt.Sprint(publicPort))},
		}, nil
	}
	if cfg.Provider == ProviderNgrok {
		// Pin IPv4 loopback. A bare port makes ngrok dial `localhost`, which on
		// macOS often resolves to `::1` while the public surface only listens on
		// 127.0.0.1 — during reconnects that shows up as `dial tcp [::1]:…:
		// connection refused`.
		args := []string{"http", fmt.Sprintf("127.0.0.1:%d", publicPort)}
		// Reserved / static domain: bind the known hostname instead of minting one.
		if cfg.URL != "" {
			args = append(args, "--url", cfg.URL)
		}
		args = append(args, "--log", "stdout")
		return Command{Name: "ngrok", Args: args}, nil
	}
	return Command{
		Name: "cloudflared",
		Args: []string{"tunnel", "--url", fmt.Sprintf("http://127.0.0.1:%d", publicPort), "--no-autoupdate"},
	}, nil
}

var (
	shellWrapper = regexp.MustCompile(`^(?:/\w+/)?(?:ba|z|a)?sh\s+-c\s+`)
	execPrefix   = regexp.MustCompile(`^exec\s+`)
)

// commandVariants is the command a process actually reports to `ps` for a
// given spawn label.
//
// BuildCommand wraps a custom command as `sh -c <command>`; sh then exec's into
// the tunnel binary, so ps shows `cloudflared tunnel … run`, never the `sh -c `
// prefix. Strip the known shell wrappers (sh -c, bash -c, exec, quoted forms)
// and match against both the recorded label and the unwrapped inner command.
func commandVariants(command string) []string {
	seen := map[string]bool{command: true}
	variants := []string{command}
	inner := strings.TrimSpace(command)
	for depth := 0; depth < 4; depth++ {
		stripped := shellWrapper.ReplaceAllString(inner, "")
		stripped = execPrefix.ReplaceAllString(stripped, "")
		unquoted := stripped
		if len(stripped) >= 2 {
			q := stripped[0]
			if (q == '\'' || q == '"') && stripped[len(stripped)-1] == q {
				unquoted = stripped[1 : len(stripped)-1]
			}
		}
		next := strings.TrimSpace(unquoted)
		if next == inner {
			break
		}
		inner = next
		if !seen[inner] {
			seen[inner] = true
			variants = append(variants, inner)
		}
	}
	out := variants[:0]
	for _, v := range variants {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// CommandMatches reports whether psLine runs command, accepting the recorded
// label or any shell-unwrapped form of it. The match must end the line, so a
// configured command that is a strict prefix of a *different* running command
// (`tunnel run` vs `tunnel run --config other.yml`) does not match and cannot
// kill an unrelated tunnel.
func CommandMatches(psLine, command string) bool {
	line := strings.TrimSpace(psLine)
	for _, variant := range commandVariants(command) {
		at := strings.Index(line, variant)
		if at < 0 {
			continue
		}
		if strings.TrimSpace(line[at+len(variant):]) == "" {
			return true
		}
	}
	return false
}
