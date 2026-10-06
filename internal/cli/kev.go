package cli

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/paths"
	"github.com/xinyao27/jevonian/internal/tunnel"
)

const (
	kevRepo              = "https://github.com/jaredpalmer/kev.git"
	defaultKevCheckpoint = "jaredpalmer/kev-4b"
	defaultKevPort       = 8009
	serverSignature      = "kev.serve"
)

func kevBaseURL(port int) string {
	return fmt.Sprintf("http://127.0.0.1:%d/v1/systemone", port)
}

func parseKevPort(raw string) (int, bool) {
	if raw == "" {
		return defaultKevPort, true
	}
	p, err := strconv.Atoi(raw)
	if err != nil || p < 1 || p > 65535 {
		return 0, false
	}
	return p, true
}

func isKevProcess(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// On Unix, FindProcess always succeeds, so test with Signal(0).
	if err := p.Signal(syscall.Signal(0)); err != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	out, err := exec.Command("ps", "-ww", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), serverSignature)
}

func kevRunningPid() int {
	data, err := os.ReadFile(paths.KevPidPath())
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 || !isKevProcess(pid) {
		return 0
	}
	return pid
}

func kevHealth(ctx context.Context, port int) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/v1/models", port), nil)
	if err != nil {
		return false
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func (c commandContext) kevStatus(port int) {
	pid := kevRunningPid()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	up := kevHealth(ctx, port)

	if pid > 0 && up {
		fmt.Fprintf(c.out, "kev running (pid %d): %s\n", pid, kevBaseURL(port))
	} else if pid > 0 {
		fmt.Fprintf(c.out, "kev process %d exists but %s is not answering yet\n", pid, kevBaseURL(port))
	} else if up {
		fmt.Fprintf(c.out, "a server answers on %s, but it was not started by jevonian\n", kevBaseURL(port))
	} else {
		fmt.Fprintln(c.out, "kev is not running; start it with `jevonian kev --start`")
	}
}

func (c commandContext) stopKev() {
	pid := kevRunningPid()
	if pid <= 0 {
		fmt.Fprintln(c.out, "kev is not running.")
	} else {
		killKevProcessGroup(pid)
		fmt.Fprintf(c.out, "stopped kev (pid %d)\n", pid)
	}
	_ = os.Remove(paths.KevPidPath())
}

func (c commandContext) startKev(checkpoint string, port int) int {
	existing := kevRunningPid()
	if existing > 0 {
		fmt.Fprintf(c.out, "kev already running (pid %d); stop it first to change checkpoint or port.\n", existing)
		return existing
	}
	python := filepath.Join(paths.KevDir(), ".venv", "bin", "python")
	if runtime.GOOS == "windows" {
		python = filepath.Join(paths.KevDir(), ".venv", "Scripts", "python.exe")
	}
	if _, err := os.Stat(python); err != nil {
		fmt.Fprintf(c.errOut, "kev is not installed in %s; run `jevonian kev` first.\n", paths.KevDir())
		return 0
	}
	if err := os.MkdirAll(paths.DataDir(), 0755); err != nil {
		fmt.Fprintf(c.errOut, "failed to create data dir: %v\n", err)
		return 0
	}
	logFile, err := os.OpenFile(paths.KevLogPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Fprintf(c.errOut, "failed to open log file: %v\n", err)
		return 0
	}
	defer logFile.Close()

	cmd := exec.Command(python, "-m", "kev.serve", "--run", checkpoint, "--port", strconv.Itoa(port))
	cmd.Dir = paths.KevDir()
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Env = tunnel.WithAugmentedPath(scrubProxyEnv(os.Environ()))

	detachKevCmd(cmd)

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(c.errOut, "failed to start kev.serve: %v\n", err)
		return 0
	}
	pid := cmd.Process.Pid
	_ = os.WriteFile(paths.KevPidPath(), []byte(strconv.Itoa(pid)), 0644)
	fmt.Fprintf(c.out, "kev.serve started (pid %d), logging to %s\n", pid, paths.KevLogPath())
	return pid
}

// scrubProxyEnv removes proxy settings that might disrupt child dials to huggingface/github
func scrubProxyEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		upper := strings.ToUpper(k)
		if upper == "HTTP_PROXY" || upper == "HTTPS_PROXY" || upper == "ALL_PROXY" {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func (c commandContext) withKevBrain(cfg *config.Config, port int, primary bool) config.Config {
	next := *cfg
	brains := append([]config.BrainConfig{}, cfg.Routing.Brains...)
	existingIdx := -1
	for i, b := range brains {
		if b.Channel == "kev" {
			existingIdx = i
			break
		}
	}

	var brain config.BrainConfig
	if existingIdx >= 0 {
		brain = brains[existingIdx]
	} else {
		brain = config.BrainConfig{
			Channel:       "kev",
			TimeoutMs:     config.DefaultBrain.TimeoutMs,
			MinConfidence: 0.4,
			Model:         "kev-latest",
		}
	}
	brain.BaseURL = kevBaseURL(port)
	if brain.Model == "" {
		brain.Model = "kev-latest"
	}

	if existingIdx >= 0 {
		brains = append(brains[:existingIdx], brains[existingIdx+1:]...)
	}
	if primary || existingIdx < 0 {
		brains = append([]config.BrainConfig{brain}, brains...)
	} else {
		// Put back in original position
		brains = append(brains[:existingIdx], append([]config.BrainConfig{brain}, brains[existingIdx:]...)...)
	}

	next.Routing.Brains = brains
	return next
}

func (c commandContext) configureBrain(port int, primary bool) error {
	cfg, cfgPath, err := config.Load()
	if err != nil {
		cfg = config.DefaultConfig()
	}
	hadKev := false
	for _, b := range cfg.Routing.Brains {
		if b.Channel == "kev" {
			hadKev = true
			break
		}
	}
	next := c.withKevBrain(&cfg, port, primary)
	if err := config.Save(cfgPath, &next); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}
	orderParts := make([]string, len(next.Routing.Brains))
	for i, b := range next.Routing.Brains {
		orderParts[i] = b.Channel
	}
	order := strings.Join(orderParts, " → ")
	action := "added"
	if hadKev {
		action = "updated"
	}
	fmt.Fprintf(c.out, "%s the Kev brain (%s); brain order: %s\n", action, kevBaseURL(port), order)
	fmt.Fprintln(c.out, "A running Jevonian picks this up on its next restart: `jevonian restart`.")
	return nil
}

const kevHelpText = `Usage: jevonian kev [--start] [--stop] [--status] [--run CHECKPOINT] [--port P] [--no-config]

  (no flags)   clone or update kev, install it, ask to start it, add the brain
  --start      also start the server without asking
  --stop       stop the server started by jevonian
  --status     report whether the server answers on --port
  --run        checkpoint: jaredpalmer/kev-4b (default), kev-9b, kev-27b
  --port       server port (default 8009)
  --no-config  do not add the Kev brain to config.json
`

func (c commandContext) kev(a arguments) error {
	if a.has("help") || a.has("h") {
		fmt.Fprint(c.out, kevHelpText)
		return nil
	}
	port, ok := parseKevPort(a.flags["port"])
	if !ok {
		return fmt.Errorf("invalid --port %q; use a number between 1 and 65535.", a.flags["port"])
	}
	if a.has("stop") {
		c.stopKev()
		return nil
	}
	if a.has("status") {
		c.kevStatus(port)
		return nil
	}

	checkpoint := defaultKevCheckpoint
	if cp, ok := a.flags["run"]; ok && cp != "" {
		checkpoint = cp
	}

	dir := paths.KevDir()
	_ = os.MkdirAll(paths.DataDir(), 0755)

	// Check required tools: git, uv
	for _, tool := range []string{"git", "uv"} {
		if _, err := exec.LookPath(tool); err != nil {
			hint := "Install git"
			if tool == "uv" {
				hint = "Install uv (https://docs.astral.sh/uv/)"
			}
			return fmt.Errorf("%s is required. %s, then re-run `jevonian kev`", tool, hint)
		}
	}

	gitDir := filepath.Join(dir, ".git")
	if _, err := os.Stat(gitDir); err == nil {
		fmt.Fprintf(c.out, "updating %s\n", dir)
		cmd := exec.Command("git", "-C", dir, "pull", "--ff-only")
		cmd.Stdout = c.out
		cmd.Stderr = c.errOut
		if err := cmd.Run(); err != nil {
			fmt.Fprintln(c.errOut, "git pull failed; using the existing checkout.")
		}
	} else {
		fmt.Fprintf(c.out, "cloning kev into %s\n", dir)
		cmd := exec.Command("git", "clone", "--depth", "1", kevRepo, dir)
		cmd.Stdout = c.out
		cmd.Stderr = c.errOut
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("clone failed; check network access to github.com")
		}
	}

	fmt.Fprintln(c.out, "installing serve dependencies (uv sync --extra serve)")
	syncCmd := exec.Command("uv", "sync", "--extra", "serve")
	syncCmd.Dir = dir
	syncCmd.Stdout = c.out
	syncCmd.Stderr = c.errOut
	if err := syncCmd.Run(); err != nil {
		return fmt.Errorf("uv sync failed; see the output above")
	}

	shouldStart := a.has("start")
	if !shouldStart && c.interactive(a) {
		reader := bufio.NewReader(c.in)
		answer, _ := c.ask(reader, fmt.Sprintf("Start kev now on :%d? (y/n)", port), "y")
		shouldStart = strings.HasPrefix(strings.ToLower(answer), "y")
	}
	if shouldStart {
		fmt.Fprintf(c.out, "The first start downloads %s and its Qwen base model once (~8GB for kev-4b).\n", checkpoint)
		pid := c.startKev(checkpoint, port)
		if pid <= 0 {
			return fmt.Errorf("failed to start kev")
		}
		fmt.Fprintln(c.out, "waiting for /v1/models (up to 5 minutes while weights download)…")
		up := false
		for attempt := 0; attempt < 150 && !up; attempt++ {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			up = kevHealth(ctx, port)
			cancel()
			if !up {
				time.Sleep(2 * time.Second)
			}
		}
		if up {
			fmt.Fprintf(c.out, "kev is up: %s\n", kevBaseURL(port))
		} else {
			fmt.Fprintf(c.out, "kev has not answered yet; tail %s\n", paths.KevLogPath())
		}
	}

	if a.has("no-config") {
		fmt.Fprintf(c.out, "\nAdd the brain yourself: channel \"Kev (local)\", endpoint %s.\n", kevBaseURL(port))
		return nil
	}

	return c.configureBrain(port, true)
}
