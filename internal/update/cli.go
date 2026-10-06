package update

import (
	"context"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// FormatUpdateNotice renders the TS update-notifier box. The caller decides
// whether to use colors and where to write it (usually stderr).
func FormatUpdateNotice(status Status, colors bool) string {
	if status.Latest == "" {
		return ""
	}
	lines := []string{"Update available " + status.Current + " → " + status.Latest, "Run jevonian update to update"}
	width := 0
	for _, line := range lines {
		if n := utf8.RuneCountInString(line); n > width {
			width = n
		}
	}
	if colors {
		lines[0] = "Update available \x1b[2m" + status.Current + "\x1b[0m → \x1b[32m" + status.Latest + "\x1b[0m"
		lines[1] = "Run \x1b[36mjevonian update\x1b[0m to update"
	}
	border := func(s string) string {
		if colors {
			return "\x1b[33m" + s + "\x1b[0m"
		}
		return s
	}
	rows := []string{border("╭" + strings.Repeat("─", width+2) + "╮"), border("│") + " " + strings.Repeat(" ", width) + " " + border("│")}
	plain := []string{"Update available " + status.Current + " → " + status.Latest, "Run jevonian update to update"}
	for i, line := range lines {
		gap := width - utf8.RuneCountInString(plain[i])
		rows = append(rows, border("│")+" "+strings.Repeat(" ", gap/2)+line+strings.Repeat(" ", gap-gap/2)+" "+border("│"))
	}
	rows = append(rows, rows[1], border("╰"+strings.Repeat("─", width+2)+"╯"))
	return "\n" + strings.Join(rows, "\n") + "\n"
}

type CommandOptions struct {
	CheckOnly bool
	Colors    bool
	// The CLI owner injects service probing/control; nil means manual restart.
	RunningVersion    func(context.Context) string
	RestartBackground func(context.Context) bool
}

// Command implements `jevonian update [--check]` output without owning global
// flags, process exit, config, or service state. Errors map to CLI exit code 1.
func (m *Manager) Command(ctx context.Context, out, errOut io.Writer, o CommandOptions) error {
	before := m.Status()
	var status Status
	var err error
	if o.CheckOnly {
		status = m.Check(ctx, true)
	} else {
		status, err = m.Install(ctx)
	}
	fmt.Fprintf(out, "current: %s\nchannel: %s\n", status.Current, status.Channel)
	if status.Installed != status.Current {
		fmt.Fprintf(out, "installed: %s\n", status.Installed)
	}
	if status.Latest != "" {
		fmt.Fprintf(out, "latest:  %s\n", status.Latest)
	}
	if err != nil {
		return err
	}
	if status.Error != "" {
		return fmt.Errorf("update check failed: %s", status.Error)
	}
	if o.CheckOnly {
		if status.RestartRequired {
			fmt.Fprintf(out, "Jevonian %s is on disk; restart the running process (still %s) to apply it.\n", status.Installed, status.Current)
		} else if status.UpdateAvailable {
			fmt.Fprint(errOut, FormatUpdateNotice(status, o.Colors))
		} else {
			fmt.Fprintln(out, "Jevonian is up to date.")
		}
		return nil
	}
	stale := false
	if o.RunningVersion != nil {
		stale = IsNewerVersion(status.Installed, o.RunningVersion(ctx))
	}
	advanced := IsNewerVersion(status.Installed, before.Current)
	if !advanced && !before.RestartRequired && !stale {
		fmt.Fprintln(out, "Jevonian is up to date.")
		return nil
	}
	if o.RestartBackground != nil && o.RestartBackground(ctx) {
		if advanced {
			fmt.Fprintf(out, "updated to %s and restarted the background service.\n", status.Installed)
		} else {
			fmt.Fprintf(out, "restarted the background service onto %s.\n", status.Installed)
		}
	} else if advanced {
		fmt.Fprintf(out, "updated to %s. Restart Jevonian to use the new version.\n", status.Installed)
	} else {
		fmt.Fprintf(out, "Jevonian %s is already installed. Restart Jevonian to use the new version.\n", status.Installed)
	}
	return nil
}
