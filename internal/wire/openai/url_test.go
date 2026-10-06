package openai

import "testing"

func TestChatCompletionsURL(t *testing.T) {
	if got := ChatCompletionsURL("https://api.example/v1/"); got != "https://api.example/v1/chat/completions" {
		t.Fatalf("got %q", got)
	}
	if got := ChatCompletionsURL("https://api.example/v1/chat/completions"); got != "https://api.example/v1/chat/completions" {
		t.Fatalf("idempotent got %q", got)
	}
}
