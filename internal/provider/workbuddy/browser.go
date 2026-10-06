package workbuddy

import (
	"os/exec"
	"runtime"
)

// openBrowserOnce launches the sign-in URL in the user's browser. Port of the
// default launch in src/browser.ts (the "once" state marker is the CLI layer's
// concern and lives outside this package).
func openBrowserOnce(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", "", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	// Fire-and-forget: like TS detached+unref, a failure to open only means the
	// user must open the printed URL themselves.
	_ = cmd.Start()
	if cmd.Process != nil {
		_ = cmd.Process.Release()
	}
}
