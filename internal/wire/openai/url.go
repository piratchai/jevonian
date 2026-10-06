package openai

import "strings"

// ChatCompletionsURL joins a provider baseUrl with the Chat Completions path.
func ChatCompletionsURL(baseURL string) string {
	base := strings.TrimRight(baseURL, "/")
	if strings.HasSuffix(strings.ToLower(base), "/chat/completions") {
		return base
	}
	return base + "/chat/completions"
}
