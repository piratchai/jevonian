// Command e2e-verifier runs a live, fully isolated end-to-end verification of
// the ChatGPT Web provider. It proves, with first-class evidence:
//
//  1. real model discovery from the live ChatGPT account,
//  2. exact UI model selection correlated with the outbound request model ID,
//  3. a real non-streaming reply,
//  4. a real streaming reply that ends with [DONE],
//  5. no user browser tab is replaced or closed, and every tab this verifier
//     opens is closed again,
//  6. the production service and production config/data are never touched,
//  7. the isolated development service and its data directory are removed.
//
// It never uses the production port or the production data directory.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/xinyao27/jevonian/internal/catalogsync"
	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/provider/chatgptweb"
	"github.com/xinyao27/jevonian/internal/routing"
	"github.com/xinyao27/jevonian/internal/server"
)

const (
	devPort    = 56859
	prodPort   = 8787
	cdpBase    = "http://127.0.0.1:9222"
	verifyHost = "127.0.0.1"
)

var failures []string

func fail(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	failures = append(failures, msg)
	fmt.Printf("FAIL: %s\n", msg)
}

func pass(format string, args ...any) {
	fmt.Printf("PASS: "+format+"\n", args...)
}

func main() {
	fmt.Println("=== Live ChatGPT Web Provider E2E Verification (fully isolated) ===")

	// ---------------------------------------------------------------------
	// Isolation: a private config file and a private data directory. Nothing
	// this verifier writes can reach the production config or data.
	// ---------------------------------------------------------------------
	isoDir, err := os.MkdirTemp("", "jevonian-e2e-")
	if err != nil {
		fmt.Printf("ERROR: cannot create isolated dir: %v\n", err)
		os.Exit(1)
	}
	dataDir := isoDir + "/data"
	configPath := isoDir + "/config.json"
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		fmt.Printf("ERROR: cannot create isolated data dir: %v\n", err)
		os.Exit(1)
	}
	os.Setenv("JEVONIAN_DATA_DIR", dataDir)
	os.Setenv("JEVONIAN_CONFIG", configPath)
	// Keep body capture inside the isolated dir for auditability.
	os.Setenv("JEVONIAN_CAPTURE_BODIES", "1")
	pass("isolated config=%s data=%s", configPath, dataDir)

	// Record the production health and the production data directory state so
	// we can prove we did not change either.
	prodBefore := probeProdHealth()
	prodBodiesDir := os.Getenv("HOME") + "/.local/share/jevonian/bodies"
	prodLeakBefore := countProviderBodies(prodBodiesDir, "chatgpt-local")

	// Snapshot every existing browser page target before we start.
	beforeTargets := listPageTargets()
	pass("recorded %d pre-existing browser page targets", len(beforeTargets))

	// ---------------------------------------------------------------------
	// Start the isolated development service on the high port.
	// ---------------------------------------------------------------------
	cfg := config.Config{
		Listen:          config.ListenConfig{Host: verifyHost, Port: devPort},
		DefaultProvider: "chatgpt-local",
		Routing:         config.RoutingConfig{Mode: "off"},
		Providers: []config.Provider{
			{
				Name:    "chatgpt-local",
				Type:    config.ProviderTypeChatGPTWeb,
				BaseURL: cdpBase,
				Auth:    config.AuthAPIKey,
				Billing: config.BillingSubscription,
				NoKey:   true,
				Models: []config.ModelEntry{
					{ID: "gpt-5-6-instant"},
					{ID: "gpt-5-6"},
				},
			},
		},
	}
	deps := server.Deps{
		Config:  &cfg,
		Routing: routing.Deps{Identity: catalogsync.NewIdentity()},
	}
	srv := server.New(fmt.Sprintf("%s:%d", verifyHost, devPort), deps)

	ctx, cancel := context.WithCancel(context.Background())
	serverErrCh := make(chan error, 1)
	go func() { serverErrCh <- srv.ListenAndServe(ctx) }()

	baseURL := fmt.Sprintf("http://%s:%d", verifyHost, devPort)
	client := &http.Client{Timeout: 300 * time.Second}
	if !waitHealthy(client, baseURL+"/healthz") {
		fmt.Println("ERROR: isolated server failed to start")
		cleanup(ctx, cancel, srv, serverErrCh, isoDir, prodBefore, prodLeakBefore, beforeTargets)
		os.Exit(1)
	}
	pass("[1/6] isolated server healthy on %s", baseURL)

	// ---------------------------------------------------------------------
	// Requirement: real model discovery from the live ChatGPT account.
	// ---------------------------------------------------------------------
	liveModels, discErr := discoverLiveModels(ctx)
	if discErr != nil {
		fail("live model discovery failed: %v", discErr)
	} else if len(liveModels) == 0 {
		fail("live model discovery returned no models")
	} else {
		pass("[2/6] live ChatGPT account exposed %d models (e.g. %s)", len(liveModels), strings.Join(firstN(modelIDs(liveModels), 5), ", "))
	}

	// The client-facing catalog must advertise the configured models.
	body, code := httpGet(client, baseURL+"/v1/models")
	if code != 200 {
		fail("GET /v1/models returned %d", code)
	} else if !strings.Contains(body, `"gpt-5-6-instant"`) || !strings.Contains(body, `"gpt-5-6"`) {
		fail("/v1/models does not advertise the configured models: %s", body)
	} else {
		pass("[2/6] GET /v1/models advertises the configured models")
	}

	// ---------------------------------------------------------------------
	// Requirement: real non-streaming reply + model correlation evidence.
	// ---------------------------------------------------------------------
	nonStreamBody, nonStreamCode, nsErr := chatRequest(ctx, client, baseURL, false)
	if nsErr != nil {
		fail("non-streaming request transport error: %v", nsErr)
	} else if nonStreamCode != 200 {
		fail("non-streaming request returned %d: %s", nonStreamCode, nonStreamBody)
	} else {
		var completion struct {
			Model   string `json:"model"`
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(nonStreamBody), &completion) != nil || len(completion.Choices) == 0 {
			fail("non-streaming response is not a valid completion: %s", nonStreamBody)
		} else {
			pass("[3/6] non-streaming reply model=%q content=%q", completion.Model, truncate(completion.Choices[0].Message.Content, 60))
		}
		auditTurnEvidence("non-streaming", false)
	}

	// ---------------------------------------------------------------------
	// Requirement: real streaming reply ending with [DONE] + correlation.
	// ---------------------------------------------------------------------
	streamText, streamCode, stErr := streamRequest(ctx, client, baseURL)
	if stErr != nil {
		fail("streaming request transport error: %v", stErr)
	} else if streamCode != 200 {
		fail("streaming request returned %d: %s", streamCode, streamText)
	} else {
		if !strings.Contains(streamText, "data: ") {
			fail("streaming response carried no SSE data frames")
		}
		if !strings.Contains(streamText, "[DONE]") {
			fail("streaming response missing [DONE]")
		} else {
			pass("[4/6] streaming reply produced SSE frames and ended with [DONE]")
		}
		auditTurnEvidence("streaming", true)
	}

	// ---------------------------------------------------------------------
	// Requirement: exact UI selection of a second model with exact slug
	// correlation. "gpt-5-6" canonicalizes to itself, so the outbound request
	// must carry the identical model ID.
	// ---------------------------------------------------------------------
	exactBody, exactCode, exErr := chatRequestFor(ctx, client, baseURL, "gpt-5-6", false)
	if exErr != nil {
		fail("exact-slug non-streaming transport error: %v", exErr)
	} else if exactCode != 200 {
		fail("exact-slug non-streaming returned %d: %s", exactCode, exactBody)
	} else {
		var completion struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal([]byte(exactBody), &completion)
		pass("[5/6] exact-slug reply model=%q", completion.Model)
		ev := chatgptweb.LastTurnEvidence()
		if ev.RequestedModel == "gpt-5-6" && ev.ObservedModel == "gpt-5-6" {
			pass("exact-slug: UI-selected model %q == outbound request model %q", ev.RequestedModel, ev.ObservedModel)
		} else {
			fail("exact-slug: expected requested=observed=\"gpt-5-6\", got requested=%q observed=%q", ev.RequestedModel, ev.ObservedModel)
		}
	}

	// ---------------------------------------------------------------------
	// Requirement: safety and cleanup evidence.
	// ---------------------------------------------------------------------
	cleanup(ctx, cancel, srv, serverErrCh, isoDir, prodBefore, prodLeakBefore, beforeTargets)

	if len(failures) > 0 {
		fmt.Printf("\n=== VERIFICATION FAILED (%d problem(s)) ===\n", len(failures))
		for _, f := range failures {
			fmt.Printf(" - %s\n", f)
		}
		os.Exit(1)
	}
	fmt.Println("\n=== All Live Verification Checks Passed ===")
}

// discoverLiveModels fetches the models from the live ChatGPT account through
// the browser session, which is the provider's real discovery path.
func discoverLiveModels(ctx context.Context) ([]chatgptweb.Model, error) {
	p := chatgptweb.NewProvider(config.Provider{
		Name:    "chatgpt-local",
		Type:    config.ProviderTypeChatGPTWeb,
		BaseURL: cdpBase,
		NoKey:   true,
	}, http.DefaultClient)
	return p.Models(ctx)
}

// chatRequest performs one non-streaming completion request.
func chatRequest(ctx context.Context, client *http.Client, baseURL string, stream bool) (string, int, error) {
	return chatRequestFor(ctx, client, baseURL, "gpt-5-6-instant", stream)
}

// chatRequestFor performs one completion request for a specific model.
func chatRequestFor(ctx context.Context, client *http.Client, baseURL, model string, stream bool) (string, int, error) {
	payload := map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "user", "content": "Hello, respond with the exact word PONG and nothing else."},
		},
		"stream": stream,
	}
	data, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/chat/completions", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body), resp.StatusCode, nil
}

// streamRequest performs one streaming completion request and returns the raw SSE text.
func streamRequest(ctx context.Context, client *http.Client, baseURL string) (string, int, error) {
	payload := map[string]any{
		"model": "gpt-5-6-instant",
		"messages": []map[string]string{
			{"role": "user", "content": "Hello, respond with the exact word PONG and nothing else."},
		},
		"stream": true,
	}
	data, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/chat/completions", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	buf := new(bytes.Buffer)
	_, _ = io.Copy(buf, resp.Body)
	return buf.String(), resp.StatusCode, nil
}

// auditTurnEvidence asserts the requested model and the outbound request model match.
func auditTurnEvidence(label string, streaming bool) {
	// Evidence is recorded before the reply is delivered, so it is present now.
	ev := chatgptweb.LastTurnEvidence()
	if ev.RequestedModel == "" || ev.ObservedModel == "" {
		fail("%s: no model correlation evidence recorded (requested=%q observed=%q)", label, ev.RequestedModel, ev.ObservedModel)
		return
	}
	if !ev.Correlated {
		fail("%s: model mismatch, requested=%q observed=%q", label, ev.RequestedModel, ev.ObservedModel)
		return
	}
	if streaming && !ev.Streaming {
		fail("%s: streaming turn did not mark its evidence as streaming", label)
		return
	}
	pass("%s: UI-selected model %q == outbound request model %q", label, ev.RequestedModel, ev.ObservedModel)
}

// cleanup stops the isolated service and verifies every isolation guarantee.
func cleanup(ctx context.Context, cancel context.CancelFunc, srv *server.Server, errCh chan error, isoDir string, prodBefore prodHealth, prodLeakBefore int, beforeTargets map[string]bool) {
	fmt.Println("\n--- Cleanup and isolation audit ---")
	cancel()
	select {
	case <-errCh:
	case <-time.After(10 * time.Second):
		fail("isolated server did not stop within 10s")
	}
	_ = srv

	// Port released.
	if portListening(devPort) {
		fail("isolated port %d is still listening after shutdown", devPort)
	} else {
		pass("isolated port %d released", devPort)
	}

	// Production health unchanged.
	prodAfter := probeProdHealth()
	if prodAfter.status != 200 || prodAfter.status != prodBefore.status {
		fail("production service changed: before=%d after=%d", prodBefore.status, prodAfter.status)
	} else {
		pass("production service on %d still healthy (HTTP %d)", prodPort, prodAfter.status)
	}

	// Production data directory unchanged: no body carrying the verifier-only
	// provider name may exist in the production data directory.
	prodBodiesDir := os.Getenv("HOME") + "/.local/share/jevonian/bodies"
	prodLeakAfter := countProviderBodies(prodBodiesDir, "chatgpt-local")
	if prodLeakAfter > prodLeakBefore {
		fail("production data leaked %d verifier body file(s): isolation broken", prodLeakAfter-prodLeakBefore)
	} else {
		pass("production data directory has no verifier-written bodies (%d)", prodLeakAfter)
	}

	// No leftover browser targets, and no pre-existing target was lost.
	afterTargets := listPageTargets()
	for id := range afterTargets {
		if !beforeTargets[id] {
			fail("verifier left an extra browser target open: %s", id)
		}
	}
	for id := range beforeTargets {
		if !afterTargets[id] {
			fail("a pre-existing browser target disappeared: %s", id)
		}
	}
	if len(afterTargets) == len(beforeTargets) {
		pass("browser targets balanced: %d before, %d after; no user tab lost", len(beforeTargets), len(afterTargets))
	}

	// Remove the isolated directory.
	if err := os.RemoveAll(isoDir); err != nil {
		fail("could not remove isolated dir %s: %v", isoDir, err)
	} else {
		pass("isolated dir removed: %s", isoDir)
	}
}

type prodHealth struct {
	status int
}

func probeProdHealth() prodHealth {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://%s:%d/healthz", verifyHost, prodPort))
	if err != nil {
		return prodHealth{status: 0}
	}
	defer resp.Body.Close()
	return prodHealth{status: resp.StatusCode}
}

func waitHealthy(client *http.Client, url string) bool {
	for i := 0; i < 40; i++ {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return true
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

func httpGet(client *http.Client, url string) (string, int) {
	resp, err := client.Get(url)
	if err != nil {
		return "", 0
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body), resp.StatusCode
}

func portListening(port int) bool {
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Get(fmt.Sprintf("http://%s:%d/healthz", verifyHost, port))
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

// listPageTargets returns the set of page target IDs currently open in Chrome.
func listPageTargets() map[string]bool {
	out := map[string]bool{}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(cdpBase + "/json/list")
	if err != nil {
		return out
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var targets []struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	if json.Unmarshal(body, &targets) != nil {
		return out
	}
	for _, t := range targets {
		if t.Type == "page" {
			out[t.ID] = true
		}
	}
	return out
}

func countProviderBodies(dir, provider string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	needle := []byte(`"` + provider + `"`)
	count := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(dir + "/" + e.Name())
		if err != nil {
			continue
		}
		if bytes.Contains(data, needle) {
			count++
		}
	}
	return count
}

func modelIDs(models []chatgptweb.Model) []string {
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	sort.Strings(ids)
	return ids
}

func firstN(in []string, n int) []string {
	if len(in) < n {
		return in
	}
	return in[:n]
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
