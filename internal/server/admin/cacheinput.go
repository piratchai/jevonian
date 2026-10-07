package admin

// cacheInput resolves how many of a row's input tokens were *not* served from
// the prompt cache — the "uncached input" half of the coverage denominator.
//
// The ledger stores prompt_tokens in whichever convention the serving wire
// used. OpenAI and Responses count cache reads *inside* prompt_tokens (so
// prompt is already the full input and uncached = prompt - cacheRead).
// Anthropic and the Connect-RPC hosts count cache reads *outside* prompt_tokens
// (so prompt is already the uncached part and uncached = prompt).
//
// exclusiveInput, recorded per row at write time, tells the two apart. Rows
// written before the field existed carry NULL and keep the historical
// "prompt is uncached" reading, so totals never go negative and legacy rows
// stay comparable to the metric they always reported.
func uncachedInputTokens(rec LogRecord) (uncached float64, known bool) {
	cached := number(rec["cacheReadTokens"])
	prompt := number(rec["promptTokens"])
	// A turn with no input accounting carries nothing to divide by.
	if cached <= 0 && prompt <= 0 {
		return 0, false
	}
	if v, ok := rec["exclusiveInput"].(bool); ok && !v {
		return maxF(prompt-cached, 0), true
	}
	// exclusiveInput true or absent: prompt already is the uncached share.
	return prompt, true
}

func maxF(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
