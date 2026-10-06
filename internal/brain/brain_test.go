package brain

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/xinyao27/jevonian/internal/config"
)

// ---- test transport ------------------------------------------------------

// fakeTransport captures the request and replays canned responses.
type fakeTransport struct {
	responses []*http.Response
	requests  []*http.Request
	bodies    []string
}

func (f *fakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	f.requests = append(f.requests, req)
	body, _ := io.ReadAll(req.Body)
	f.bodies = append(f.bodies, string(body))
	i := len(f.requests) - 1
	if i >= len(f.responses) {
		i = len(f.responses) - 1
	}
	if i < 0 {
		return nil, fmt.Errorf("no response")
	}
	return f.responses[i], nil
}

func jsonResponse(status int, payload any) *http.Response {
	data, _ := json.Marshal(payload)
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(string(data))),
		Header:     http.Header{"Content-Type": {"application/json"}},
	}
}

func textResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{},
	}
}

func clientWith(t *fakeTransport) *Client {
	return &Client{
		HTTP:  &http.Client{Transport: t},
		Sleep: func(time.Duration) {},
	}
}

// ---- parseSystemOneResponse (src/brain.test.ts) --------------------------

func TestParseSystemOneResponseChoice(t *testing.T) {
	parsed := ParseSystemOneResponse(map[string]any{
		"model": "jev-1.13.0",
		"answers": map[string]any{
			"model": map[string]any{
				"choice":     "deepseek-v4.1-flash",
				"confidence": 0.82,
				"probabilities": map[string]any{
					"deepseek-v4-pro": 0.1, "deepseek-v4.1-flash": 0.82,
				},
			},
		},
	})
	if parsed.Model != "deepseek-v4.1-flash" {
		t.Fatalf("model = %q", parsed.Model)
	}
	if parsed.Confidence != 0.82 {
		t.Fatalf("confidence = %v", parsed.Confidence)
	}
	if parsed.Probabilities["deepseek-v4.1-flash"] != 0.82 {
		t.Fatalf("probabilities = %v", parsed.Probabilities)
	}
	if parsed.ModelName != "jev-1.13.0" {
		t.Fatalf("modelName = %q", parsed.ModelName)
	}
}

func TestParseSystemOneResponseChoicesFallback(t *testing.T) {
	parsed := ParseSystemOneResponse(map[string]any{
		"choices": map[string]any{
			"model": map[string]any{
				"choice": "deepseek-v4-pro",
				"probabilities": map[string]any{
					"deepseek-v4-pro": 0.7, "deepseek-v4.1-flash": 0.3,
				},
			},
		},
	})
	if parsed.Model != "deepseek-v4-pro" {
		t.Fatalf("model = %q", parsed.Model)
	}
	if parsed.Confidence != 0.7 {
		t.Fatalf("confidence = %v", parsed.Confidence)
	}
}

func TestParseSystemOneResponseNoneOfTheAbove(t *testing.T) {
	parsed := ParseSystemOneResponse(map[string]any{
		"answers": map[string]any{
			"model": map[string]any{"choice": "none_of_the_above", "confidence": 0.55},
		},
	})
	if parsed.Model != "none_of_the_above" {
		t.Fatalf("model = %q", parsed.Model)
	}
	if parsed.Confidence != 0.55 {
		t.Fatalf("confidence = %v", parsed.Confidence)
	}
	if parsed.Probabilities != nil {
		t.Fatalf("probabilities = %v", parsed.Probabilities)
	}
}

func TestParseSystemOneResponseMissingChoice(t *testing.T) {
	parsed := ParseSystemOneResponse(map[string]any{
		"answers": map[string]any{"model": map[string]any{}},
	})
	if parsed.Model != "" {
		t.Fatalf("model = %q", parsed.Model)
	}
	if parsed.Confidence != 0 {
		t.Fatalf("confidence = %v", parsed.Confidence)
	}
}

func TestParseSystemOneResponseKeepsReportedConfidence(t *testing.T) {
	parsed := ParseSystemOneResponse(map[string]any{
		"answers": map[string]any{
			"model": map[string]any{
				"choice": "plan", "confidence": 0.3,
				"probabilities": map[string]any{"plan": 0.52, "execute": 0.48},
			},
		},
	})
	if parsed.Confidence != 0.3 {
		t.Fatalf("confidence = %v", parsed.Confidence)
	}
}

func TestParseSystemOneResponseEffortDistribution(t *testing.T) {
	parsed := ParseSystemOneResponse(map[string]any{
		"answers": map[string]any{
			"model": map[string]any{
				"choice": "plan", "confidence": 0.37,
				"probabilities": map[string]any{
					"plan": 0.37, "execute": 0.31, "chat": 0.2, "utility": 0.12,
				},
			},
			"effort": map[string]any{
				"choice": "low", "confidence": 0.6,
				"probabilities": map[string]any{
					"none": 0.1, "low": 0.6, "medium": 0.2, "high": 0.1,
				},
			},
		},
	})
	if parsed.Effort != "low" {
		t.Fatalf("effort = %q", parsed.Effort)
	}
	if parsed.EffortProbabilities["low"] != 0.6 {
		t.Fatalf("effortProbabilities = %v", parsed.EffortProbabilities)
	}
}

// ---- askJev (src/brain.test.ts) ------------------------------------------

func TestAskPostsStateAndQuestions(t *testing.T) {
	tr := &fakeTransport{responses: []*http.Response{
		jsonResponse(200, map[string]any{
			"model": "jev-1.13.0",
			"answers": map[string]any{
				"model": map[string]any{
					"choice": "deepseek-v4.1-flash", "confidence": 0.9,
					"probabilities": map[string]any{
						"deepseek-v4.1-flash": 0.9, "deepseek-v4-pro": 0.08,
						"none_of_the_above": 0.02,
					},
				},
			},
		}),
	}}
	t.Setenv("TYPESAFE_API_KEY", "test-key")
	verdict := clientWith(tr).AskVerdict(context.Background(), Input{
		Brain: config.BrainConfig{Channel: "typesafe", TimeoutMs: 1000, MinConfidence: 0.6},
		State: map[string]any{
			"last_user_message": "build a cache",
			"candidates": []any{
				map[string]any{"model": "deepseek-v4-pro", "provider": "deepseek"},
				map[string]any{"model": "deepseek-v4.1-flash", "provider": "deepseek"},
			},
		},
	})
	if verdict == nil {
		t.Fatal("expected verdict")
	}
	if verdict.Model != "deepseek-v4.1-flash" || verdict.Confidence != 0.9 {
		t.Fatalf("verdict = %+v", verdict)
	}
	if verdict.ModelName != "jev-1.13.0" {
		t.Fatalf("modelName = %q", verdict.ModelName)
	}
	if len(tr.requests) != 1 {
		t.Fatalf("requests = %d", len(tr.requests))
	}
	if tr.requests[0].URL.String() != "https://api.typesafe.ai/v1/systemone" {
		t.Fatalf("url = %s", tr.requests[0].URL)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(tr.bodies[0]), &body); err != nil {
		t.Fatal(err)
	}
	if body["model"] != "jev-latest" {
		t.Fatalf("model = %v", body["model"])
	}
	questions := body["questions"].(map[string]any)
	// One request, two questions: which model and how deeply it should think.
	if len(questions) != 2 || questions["model"] == nil || questions["effort"] == nil {
		t.Fatalf("questions = %v", questions)
	}
	modelQ := questions["model"].(map[string]any)
	criteria := modelQ["criteria"].(map[string]any)
	for _, want := range []string{"deepseek-v4-pro", "deepseek-v4.1-flash", "none_of_the_above"} {
		if criteria[want] == nil {
			t.Fatalf("missing criterion %q in %v", want, criteria)
		}
	}
}

func TestAskNoAPIKey(t *testing.T) {
	tr := &fakeTransport{responses: []*http.Response{jsonResponse(200, map[string]any{})}}
	// No env, no credential reader, no explicit key.
	outcome := clientWith(tr).Ask(context.Background(), Input{
		Brain: config.BrainConfig{Channel: "typesafe", TimeoutMs: 1000, MinConfidence: 0.6},
		State: map[string]any{},
	})
	if outcome.Verdict != nil {
		t.Fatalf("expected no verdict, got %+v", outcome.Verdict)
	}
	if len(tr.requests) != 0 {
		t.Fatal("no request should be sent")
	}
}

func TestAskExplicitAPIKeyOverride(t *testing.T) {
	tr := &fakeTransport{responses: []*http.Response{
		jsonResponse(200, map[string]any{
			"answers": map[string]any{
				"model": map[string]any{"choice": "deepseek-v4-pro", "confidence": 0.7},
			},
		}),
	}}
	verdict := clientWith(tr).AskVerdict(context.Background(), Input{
		Brain:  config.BrainConfig{Channel: "typesafe", TimeoutMs: 1000, MinConfidence: 0.6},
		APIKey: "typed-key",
		State: map[string]any{
			"candidates": []any{map[string]any{"model": "deepseek-v4-pro", "provider": "deepseek"}},
		},
	})
	if verdict == nil || verdict.Model != "deepseek-v4-pro" {
		t.Fatalf("verdict = %+v", verdict)
	}
	if tr.requests[0].Header.Get("authorization") != "Bearer typed-key" {
		t.Fatalf("auth = %q", tr.requests[0].Header.Get("authorization"))
	}
}

func TestAskOutcomePerCallFailures(t *testing.T) {
	// One brain answers 402, another 403: each failure rides on its own
	// result, not shared state.
	tr := &fakeTransport{responses: []*http.Response{
		textResponse(402, "nope"),
		textResponse(403, "nope"),
	}}
	ctx := context.Background()
	first := clientWith(tr).Ask(ctx, Input{
		Brain: config.BrainConfig{
			Channel: "custom", BaseURL: "https://first.example/v1",
			TimeoutMs: 1000, MinConfidence: 0.4,
		},
		APIKey: "k",
		State:  map[string]any{},
	})
	second := clientWith(tr).Ask(ctx, Input{
		Brain: config.BrainConfig{
			Channel: "custom", BaseURL: "https://second.example/v1",
			TimeoutMs: 1000, MinConfidence: 0.4,
		},
		APIKey: "k",
		State:  map[string]any{},
	})
	if first.Failure == nil || first.Failure.Status != 402 {
		t.Fatalf("first = %+v", first.Failure)
	}
	if second.Failure == nil || second.Failure.Status != 403 {
		t.Fatalf("second = %+v", second.Failure)
	}
}

func TestAskUpstreamErrorNoVerdict(t *testing.T) {
	tr := &fakeTransport{responses: []*http.Response{
		textResponse(500, "nope"),
		textResponse(500, "nope"), // retry
	}}
	t.Setenv("TYPESAFE_API_KEY", "test-key")
	verdict := clientWith(tr).AskVerdict(context.Background(), Input{
		Brain: config.BrainConfig{Channel: "typesafe", TimeoutMs: 1000, MinConfidence: 0.6},
		State: map[string]any{},
	})
	if verdict != nil {
		t.Fatalf("expected no verdict, got %+v", verdict)
	}
}

func TestAskCloudflareWorkersAI(t *testing.T) {
	tr := &fakeTransport{responses: []*http.Response{
		jsonResponse(200, map[string]any{
			"success": true,
			"result": map[string]any{
				"model": "jev-1.13.0",
				"answers": map[string]any{
					"model": map[string]any{
						"choice": "plan", "confidence": 0.91,
						"probabilities": map[string]any{"plan": 0.91, "execute": 0.09},
					},
				},
			},
		}),
	}}
	verdict := clientWith(tr).AskVerdict(context.Background(), Input{
		Brain: config.BrainConfig{
			Channel: "cloudflare", AccountID: "acct_123",
			TimeoutMs: 1000, MinConfidence: 0.6,
		},
		APIKey: "cf-token",
		State: map[string]any{
			"routings": []any{map[string]any{"id": "plan", "label": "Plan", "description": "planning"}},
		},
	})
	if verdict == nil || verdict.Model != "plan" {
		t.Fatalf("verdict = %+v", verdict)
	}
	if verdict.Confidence != 0.91 {
		t.Fatalf("confidence = %v", verdict.Confidence)
	}
	if tr.requests[0].URL.String() != "https://api.cloudflare.com/client/v4/accounts/acct_123/ai/run" {
		t.Fatalf("url = %s", tr.requests[0].URL)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(tr.bodies[0]), &body); err != nil {
		t.Fatal(err)
	}
	if body["model"] != "typesafe/jev" {
		t.Fatalf("model = %v", body["model"])
	}
	input := body["input"].(map[string]any)
	questions := input["questions"].(map[string]any)
	if len(questions) != 2 {
		t.Fatalf("questions = %v", questions)
	}
}

func TestAskCloudflareClefModel(t *testing.T) {
	tr := &fakeTransport{responses: []*http.Response{
		jsonResponse(200, map[string]any{
			"success": true,
			"result": map[string]any{
				"answers": map[string]any{
					"model": map[string]any{"choice": "plan", "confidence": 0.9},
				},
			},
		}),
	}}
	verdict := clientWith(tr).AskVerdict(context.Background(), Input{
		Brain: config.BrainConfig{
			Channel: "cloudflare", AccountID: "acct_123",
			Model:     "@cf/cloudflare/clef-flash",
			TimeoutMs: 1000, MinConfidence: 0.6,
		},
		APIKey: "cf-token",
		State: map[string]any{
			"routings": []any{map[string]any{"id": "plan", "label": "Plan", "description": "planning"}},
		},
	})
	if verdict == nil || verdict.Model != "plan" {
		t.Fatalf("verdict = %+v", verdict)
	}
	want := "https://api.cloudflare.com/client/v4/accounts/acct_123/ai/run/@cf/cloudflare/clef-flash"
	if tr.requests[0].URL.String() != want {
		t.Fatalf("url = %s", tr.requests[0].URL)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(tr.bodies[0]), &body); err != nil {
		t.Fatal(err)
	}
	if body["model"] != "clef-flash" {
		t.Fatalf("model = %v", body["model"])
	}
	if body["state"] == nil {
		t.Fatal("state should be top-level for Clef")
	}
}

func TestAskCloudflareNoAccountID(t *testing.T) {
	tr := &fakeTransport{responses: []*http.Response{jsonResponse(200, map[string]any{})}}
	verdict := clientWith(tr).AskVerdict(context.Background(), Input{
		Brain:  config.BrainConfig{Channel: "cloudflare", TimeoutMs: 1000, MinConfidence: 0.6},
		APIKey: "cf-token",
		State:  map[string]any{},
	})
	if verdict != nil {
		t.Fatalf("expected no verdict, got %+v", verdict)
	}
}

func TestAskRetries429(t *testing.T) {
	tr := &fakeTransport{responses: []*http.Response{
		textResponse(429, "slow down"),
		jsonResponse(200, map[string]any{
			"model": "jev-1.13.0",
			"answers": map[string]any{
				"model": map[string]any{"choice": "execute", "confidence": 0.9},
			},
		}),
	}}
	t.Setenv("TYPESAFE_API_KEY", "test-key")
	verdict := clientWith(tr).AskVerdict(context.Background(), Input{
		Brain:     config.BrainConfig{Channel: "typesafe", TimeoutMs: 5000, MinConfidence: 0.6},
		State:     map[string]any{"last_user_message": "hi"},
		ModelOnly: true,
	})
	if verdict == nil || verdict.Model != "execute" {
		t.Fatalf("verdict = %+v", verdict)
	}
	if len(tr.requests) != 2 {
		t.Fatalf("expected 2 attempts (429 then 200), got %d", len(tr.requests))
	}
}

// ---- cloudflare helpers (src/brain.test.ts) -------------------------------

func TestCloudflareAIRunURL(t *testing.T) {
	if got := CloudflareAIRunURL(" abc "); got != "https://api.cloudflare.com/client/v4/accounts/abc/ai/run" {
		t.Fatalf("url = %q", got)
	}
}

func TestUnwrapCloudflarePayload(t *testing.T) {
	result, ok := UnwrapCloudflarePayload(map[string]any{
		"success": true,
		"result":  map[string]any{"answers": map[string]any{"model": map[string]any{"choice": "plan"}}},
	})
	if !ok {
		t.Fatal("should unwrap")
	}
	if result.(map[string]any)["answers"] == nil {
		t.Fatalf("result = %v", result)
	}
	if _, ok := UnwrapCloudflarePayload(map[string]any{"success": false, "result": map[string]any{}}); ok {
		t.Fatal("failure envelope should not unwrap")
	}
	passthrough, ok := UnwrapCloudflarePayload(map[string]any{
		"answers": map[string]any{"model": map[string]any{"choice": "plan"}},
	})
	if !ok || passthrough.(map[string]any)["answers"] == nil {
		t.Fatal("non-envelope payload should pass through")
	}
}

func TestIsWorkersAIModelID(t *testing.T) {
	if IsWorkersAIModelID("typesafe/jev") {
		t.Fatal("typesafe/jev is not a Workers AI id")
	}
	if !IsWorkersAIModelID(" @cf/cloudflare/clef ") {
		t.Fatal("@cf/... is a Workers AI id")
	}
}

func TestCloudflareAIRunModelURL(t *testing.T) {
	got := CloudflareAIRunModelURL("acct_123", "@cf/cloudflare/clef-flash")
	want := "https://api.cloudflare.com/client/v4/accounts/acct_123/ai/run/@cf/cloudflare/clef-flash"
	if got != want {
		t.Fatalf("got %q", got)
	}
}

func TestCloudflareModelSelector(t *testing.T) {
	if CloudflareModelSelector("@cf/cloudflare/clef") != "clef" {
		t.Fatal("clef")
	}
	if CloudflareModelSelector("@cf/cloudflare/clef-flash") != "clef-flash" {
		t.Fatal("clef-flash")
	}
}

// ---- channels (src/brain.test.ts) -----------------------------------------

func TestFindChannel(t *testing.T) {
	if FindChannel("typesafe").BaseURL != "https://api.typesafe.ai/v1/systemone" {
		t.Fatal("typesafe baseUrl")
	}
	if FindChannel("openrouter").Model != "typesafe/jev-1.13" {
		t.Fatal("openrouter model")
	}
	if !strings.Contains(FindChannel("opencode-zen").BaseURL, "/systemone") {
		t.Fatal("opencode-zen baseUrl")
	}
	if FindChannel("vercel").Model != "typesafe-ai/jev" {
		t.Fatal("vercel model")
	}
	if !FindChannel("cloudflare").RequiresAccountID {
		t.Fatal("cloudflare requiresAccountId")
	}
	if FindChannel("cloudflare").Model != "typesafe/jev" {
		t.Fatal("cloudflare model")
	}
	var ids []string
	for _, m := range FindChannel("cloudflare").Models {
		ids = append(ids, m.ID)
	}
	want := []string{"typesafe/jev", "@cf/cloudflare/clef", "@cf/cloudflare/clef-flash"}
	if len(ids) != len(want) {
		t.Fatalf("cloudflare models = %v", ids)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("cloudflare models = %v", ids)
		}
	}
	var hasBaseURL bool
	for _, ch := range Channels {
		if ch.RequiresBaseURL {
			hasBaseURL = true
		}
	}
	if !hasBaseURL {
		t.Fatal("some channel should require baseUrl")
	}
}

func TestChannelDefaultModelFirst(t *testing.T) {
	for _, ch := range Channels {
		if len(ch.Models) == 0 {
			continue
		}
		if ch.Models[0].ID != ch.Model {
			t.Fatalf("%s: first preset %q != default %q", ch.ID, ch.Models[0].ID, ch.Model)
		}
	}
}

// ---- normalizeEvaluationResult (src/brain.test.ts) -------------------------

func TestNormalizeEvaluationResult(t *testing.T) {
	normalized := NormalizeEvaluationResult(map[string]any{
		"model": map[string]any{
			"type":   "choice",
			"choice": "deepseek-v4.1-flash",
			"probabilities": map[string]any{
				"deepseek-v4.1-flash": 0.9, "deepseek-v4-pro": 0.1,
			},
		},
	}, "typesafe-ai/jev")
	parsed := ParseSystemOneResponse(normalized)
	if parsed.Model != "deepseek-v4.1-flash" {
		t.Fatalf("model = %q", parsed.Model)
	}
	if parsed.Confidence != 0.9 {
		t.Fatalf("confidence = %v", parsed.Confidence)
	}
	if parsed.ModelName != "typesafe-ai/jev" {
		t.Fatalf("modelName = %q", parsed.ModelName)
	}
}

// ---- routingCriteria (src/brain.test.ts) -----------------------------------

func TestRoutingCriteria(t *testing.T) {
	criteria := RoutingCriteria([]RoutingDescriptor{
		{ID: "frontend", Label: "Frontend", Description: "React, CSS, UI polish"},
		{ID: "plan", Label: "Plan", Description: "planning"},
	})
	if *criteria["frontend"] != "Frontend: React, CSS, UI polish" {
		t.Fatalf("frontend = %q", *criteria["frontend"])
	}
	if *criteria["plan"] != "Plan: planning" {
		t.Fatalf("plan = %q", *criteria["plan"])
	}
	if !strings.Contains(*criteria["none_of_the_above"], "No listed routing") {
		t.Fatalf("none_of_the_above = %q", *criteria["none_of_the_above"])
	}
}

// ---- routing instructions (src/brain.test.ts) ------------------------------

func TestHTTPQuestionsPreferenceOrder(t *testing.T) {
	questions := HTTPQuestions(Input{
		Brain: config.BrainConfig{Channel: "typesafe", TimeoutMs: 1000, MinConfidence: 0.6},
		State: map[string]any{
			"routings": []any{map[string]any{"id": "plan", "label": "Plan", "description": "planning"}},
		},
	})
	model, ok := questions["model"]
	if !ok {
		t.Fatal("no model question")
	}
	if !strings.Contains(model.Instructions, "preference order") {
		t.Fatalf("instructions = %q", model.Instructions)
	}
}

// ---- breaker -------------------------------------------------------------

func TestBreakerOpensAfterThreshold(t *testing.T) {
	now := time.Now()
	c := &Client{Now: func() time.Time { return now }}
	if c.BreakerOpen() {
		t.Fatal("closed initially")
	}
	for i := 0; i < BreakerThreshold; i++ {
		c.RecordOutcome(false)
	}
	if !c.BreakerOpen() {
		t.Fatal("should open after threshold")
	}
	c.RecordOutcome(true)
	if c.BreakerOpen() {
		t.Fatal("success resets")
	}
}

func TestVercelChannelFails(t *testing.T) {
	outcome := (&Client{}).Ask(context.Background(), Input{
		Brain: config.BrainConfig{Channel: "vercel"},
		State: map[string]any{},
	})
	if outcome.Failure == nil {
		t.Fatal("expected failure")
	}
	if !strings.Contains(outcome.Failure.Error, "vercel") {
		t.Fatalf("error = %q", outcome.Failure.Error)
	}
}

// ---- OpenRouter attribution ----------------------------------------------

func TestOpenRouterAttribution(t *testing.T) {
	headers := WithOpenRouterAttribution(http.Header{}, "https://openrouter.ai/api/alpha/decisions")
	if headers.Get("HTTP-Referer") != openRouterAppURL {
		t.Fatalf("referer = %q", headers.Get("HTTP-Referer"))
	}
	if headers.Get("X-Title") != openRouterAppTitle {
		t.Fatalf("title = %q", headers.Get("X-Title"))
	}
	other := WithOpenRouterAttribution(http.Header{}, "https://api.typesafe.ai/v1/systemone")
	if other.Get("HTTP-Referer") != "" {
		t.Fatal("non-openrouter host should not get attribution")
	}
}
