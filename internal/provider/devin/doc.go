// Package devin is the Devin subscription wire core: Connect-RPC + protobuf
// against server.codeium.com, the same transport the Devin CLI ("chisel")
// speaks. It ports src/devin.ts + src/devin-catalog.ts: request encoding,
// response-frame decoding, error classification, the OpenAI Chat Completions
// adapters, model discovery and the on-disk model metadata cache. It holds no
// routing or credential state — a token is passed in per call.
//
// Seam with internal/server: server folds the client body into a Chat
// Completions JSON object first (Anthropic/Responses collapse onto it, image
// blocks become one message per part), resolves the token via ReadToken, then:
//
//	req := devin.BuildChatRequest(token, chatBody, model, devin.ChatOptions{SessionID: session})
//	httpReq, _ := http.NewRequest(ctx, "POST", devin.ChatURL(provider.BaseURL), bytes.NewReader(req))
//	httpReq.Header = devin.Headers(token, devin.HeadersStream)
//	resp, err := httpClient.Do(httpReq)                       // same-host retry allowed; rebuild req per attempt
//
//	if resp.StatusCode >= 400                          → devin.ClassifyError(status, bodyText, token)
//	else                                               → devin.Peek(resp.Body, token)   // leading trailers
//	    err != nil                                     → classified refusal (failover / surface)
//	    stream                                         → devin.ToChatStream(model, stream, devin.StreamOptions{...})
//	                                                    or devin.ChatCompletion(stream, model, token)
//
// A content_policy refusal may be retried once with
// devin.StripAgentSystemMessages(messages) as a new BuildChatRequest body.
// Devin meters daily/weekly allowances; quota forwarding is
// UserStatus.Windows() from FetchUserStatus, and refusal cooldowns map to
// quota.Tracker.MarkSpent (model-scoped for per-model limits, which carry a
// ResetsAt on StreamError).
//
// Protobuf is hand-rolled with the stdlib (VarintField/StringField/BytesField/
// DoubleField + Concat), matching the TS encoder byte for byte; no CGO and no
// protobuf dependency. The wire only ever writes field-level messages.
package devin
