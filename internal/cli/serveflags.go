package cli

import (
	"fmt"
	"strconv"

	"github.com/xinyao27/jevonian/internal/config"
)

// LAN flags persist, whereas tunnel/port overrides apply only to this process.
func applyServeFlags(cfg *config.Config, a arguments, path string) error {
	port := func(flag string) (int, error) {
		n, err := strconv.Atoi(a.flags[flag])
		if err != nil || n < 1 || n > 65535 {
			return 0, fmt.Errorf("--%s must be a port between 1 and 65535", flag)
		}
		return n, nil
	}
	if a.has("lan") && a.has("no-lan") {
		return fmt.Errorf("--lan and --no-lan cannot be combined")
	}
	if a.has("tunnel") && a.has("no-tunnel") {
		return fmt.Errorf("--tunnel and --no-tunnel cannot be combined")
	}
	// Validate every one-shot value before any persistent LAN mutation.
	if a.has("port") {
		if _, err := port("port"); err != nil {
			return err
		}
	}
	if a.has("lan-port") {
		if _, err := port("lan-port"); err != nil {
			return err
		}
	}
	if a.has("lan") {
		cfg.Lan.Enabled = true
	}
	if a.has("no-lan") {
		cfg.Lan.Enabled = false
	}
	if a.has("lan-host") {
		cfg.Lan.Host = a.flags["lan-host"]
	}
	if a.has("lan-port") {
		n, err := port("lan-port")
		if err != nil {
			return err
		}
		cfg.Lan.Port = &n
	}
	if a.has("lan") || a.has("no-lan") || a.has("lan-host") || a.has("lan-port") {
		if err := config.Save(path, cfg); err != nil {
			return err
		}
	}
	if a.has("port") {
		n, err := port("port")
		if err != nil {
			return err
		}
		cfg.Listen.Port = n
	}
	if a.has("tunnel") {
		cfg.Tunnel.Enabled = true
	}
	if a.has("no-tunnel") {
		cfg.Tunnel.Enabled = false
	}
	return nil
}
