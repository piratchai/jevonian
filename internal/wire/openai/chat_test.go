package openai

import "testing"

func TestCompletionUsageIncludesCachedTokens(t *testing.T) {
	usage := CompletionUsage([]byte(`{"usage":{"prompt_tokens":1200,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":900}}}`))
	if usage.PromptTokens != 1200 || usage.CompletionTokens != 20 || usage.CacheRead != 900 {
		t.Fatalf("usage = %+v", usage)
	}
}
