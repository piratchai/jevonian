// Package cursor is the Cursor subscription wire: Connect-RPC + protobuf
// against Cursor's agent API (AgentService/Run), the same transport the
// cursor-agent CLI speaks. It owns credential resolution, model discovery,
// request encoding, stream decoding and error classification; it holds no
// routing or config state. Port of src/cursor.ts, src/cursor-catalog.ts and
// the token half of src/cursor-token.ts.
//
// A Cursor account has no plain REST endpoint: the CLI talks a bidirectional
// Connect stream. The conversation is sent whole as AI SDK message JSON, each
// message stored as a sha256-named blob the server asks back for as it reads
// them (kv_server_message); the caller's tools are MCP tools reached through
// Cursor's CallDynamicTool, which the server hands back to the client to run
// (exec_server_message). A heartbeat frame every 5s keeps the stream open.
//
// Server seam: Provider is the entry point — ResolveToken, Chat, Models,
// CachedModels, Account, HasCredential. Chat consumes a wire.Body (OpenAI
// Chat Completions), runs the turn via Run/RunOptions, and hands back either
// a refusal (ChatResult.Error — classify with ShouldFailover, CooldownLabel,
// ErrorTypes, SurfacedStatus) or an answer: ChatResult.Stream as
// chat.completion.chunk SSE, or ChatResult.Completion. The wire-level pieces
// stay exported for callers that need them: Messages/BuildRun/RunDecoder/
// EncodeFrame for the Connect protocol, ConversationBody/LastUser for the
// request mapping, ParseModels/CollapseModels/SplitID/ModelID and
// LoadCatalog/SaveCatalog for the model list, Token/ReadToken/AuthPath/
// Executable/ClientVersion/ParseAbout for credentials.
package cursor
