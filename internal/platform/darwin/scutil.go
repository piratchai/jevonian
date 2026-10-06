// Package darwin holds macOS-specific helpers that compile on every GOOS.
// Callers gate actual scutil execution; this package only parses its text output.
package darwin

import (
	"regexp"
	"strings"
)

var (
	exceptionsEntry = regexp.MustCompile(`^\d+\s*:\s*(.+)$`)
	scalarKey       = regexp.MustCompile(`^([A-Za-z]+)\s*:\s*(.*)$`)
	structuredValue = regexp.MustCompile(`^<.+>\s*\{$`)
)

// SystemProxy is the proxy URL and bypass list derived from scutil --proxy.
type SystemProxy struct {
	// URL is the proxy endpoint, e.g. http://127.0.0.1:1082.
	URL string
	// Bypass lists hosts that must be reached directly (loopback first).
	Bypass []string
}

// LoopbackBypass hosts never leave the machine, so they must never be proxied.
var LoopbackBypass = []string{"localhost", "127.0.0.1", "::1"}

// ParseScutilProxy parses the output of `scutil --proxy`.
//
// The format is an old NeXTSTEP property list — nested `<dictionary> { … }` blocks
// with `key : value` lines. Only flat scalars plus the flat ExceptionsList array
// are needed, so nesting is tracked rather than fully decoded.
func ParseScutilProxy(output string) *SystemProxy {
	scalar := make(map[string]string)
	var exceptions []string
	inExceptions := false

	for _, raw := range strings.Split(output, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if inExceptions {
			if line == "}" {
				inExceptions = false
				continue
			}
			if entry := exceptionsEntry.FindStringSubmatch(line); entry != nil {
				host := strings.TrimSpace(entry[1])
				if !contains(exceptions, host) {
					exceptions = append(exceptions, host)
				}
			}
			continue
		}
		key := scalarKey.FindStringSubmatch(line)
		if key == nil {
			continue
		}
		name, value := key[1], key[2]
		if structuredValue.MatchString(value) {
			// Structured values are only interesting for the exceptions array;
			// anything else nested (a SOCKS proxy block, say) is skipped whole.
			if name == "ExceptionsList" {
				inExceptions = true
			}
			continue
		}
		scalar[name] = value
	}

	var target string
	if scalar["HTTPSEnable"] == "1" && scalar["HTTPSProxy"] != "" {
		target = scalar["HTTPSProxy"] + ":" + scalar["HTTPSPort"]
	} else if scalar["HTTPEnable"] == "1" && scalar["HTTPProxy"] != "" {
		// Providers are https, but an http-only proxy still covers both schemes
		// rather than sending half the traffic direct.
		target = scalar["HTTPProxy"] + ":" + scalar["HTTPPort"]
	}
	if target == "" {
		return nil
	}

	bypass := dedupe(append(append([]string{}, LoopbackBypass...), exceptions...))
	return &SystemProxy{URL: "http://" + target, Bypass: bypass}
}

func contains(list []string, item string) bool {
	for _, v := range list {
		if v == item {
			return true
		}
	}
	return false
}

func dedupe(list []string) []string {
	seen := make(map[string]struct{}, len(list))
	out := make([]string, 0, len(list))
	for _, item := range list {
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	return out
}
