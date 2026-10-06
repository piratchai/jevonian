package openai

import "encoding/json"

// ChatRequest is the minimal Chat Completions body fields the router needs.
type ChatRequest struct {
	Model    string          `json:"model"`
	Messages json.RawMessage `json:"messages"`
	Stream   bool            `json:"stream"`
}

// ParseChatRequest decodes a Chat Completions JSON body.
func ParseChatRequest(body []byte) (ChatRequest, error) {
	var req ChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return ChatRequest{}, err
	}
	return req, nil
}

// Usage is the OpenAI usage object (token counts).
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// CompletionUsage extracts usage from a Chat Completions JSON body.
func CompletionUsage(body []byte) Usage {
	var envelope struct {
		Usage Usage `json:"usage"`
	}
	_ = json.Unmarshal(body, &envelope)
	return envelope.Usage
}
