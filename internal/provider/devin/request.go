package devin

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/xinyao27/jevonian/internal/wire"
)

// DefaultBaseURL is Devin's public API server.
const DefaultBaseURL = "https://server.codeium.com"

const (
	chatPath       = "/exa.api_server_pb.ApiServerService/GetChatMessage"
	modelsPath     = "/exa.api_server_pb.ApiServerService/GetCliModelConfigs"
	userStatusPath = "/exa.seat_management_pb.SeatManagementService/GetUserStatus"

	clientName           = "chisel"
	defaultClientVersion = "3000.11.3"
	fingerprintBytes     = 366
	defaultMaxOutput     = 16_384
	contextWindow        = 128_000
	defaultTemperature   = 1.0
	// The server answers exactly 0 with "an internal error occurred".
	minTemperature = 0.001
	topK           = 40
	topP           = 0.95
	// Tools with an empty system prompt are rejected upstream (Claude-family selectors).
	toolsSystemPrompt = "You are a helpful assistant. Use the available tools when appropriate."
	maxFrameBytes     = 64 * 1024 * 1024

	flagGzip      = 0x01
	flagEndStream = 0x02

	sourceUser      = 1
	sourceAssistant = 2
	sourceTool      = 4
)

// ─── client identity ────────────────────────────────────────────────────────

func clientVersion() string {
	if v := strings.TrimSpace(os.Getenv("JEVONIAN_DEVIN_CLIENT_VERSION")); v != "" {
		return v
	}
	return defaultClientVersion
}

func installationIDPath() string {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "devin", "cli", "installation_id")
}

// expandSeed is a SHA-256 counter expansion of seed into length bytes.
func expandSeed(seed string, length int) []byte {
	var out []byte
	for counter := 0; len(out) < length; counter++ {
		sum := sha256.Sum256([]byte(seed + "-" + strconv.Itoa(counter)))
		out = append(out, sum[:]...)
	}
	return out[:length]
}

var fingerprint struct {
	sync.Mutex
	random      string
	derivedSeed string
	derivedHex  string
}

// deviceFingerprint is ClientMetadata #31: 366 bytes as 732 hex chars. Stable per
// installation when the Devin CLI's installation_id is readable, otherwise
// random once per process.
func deviceFingerprint() string {
	seedBytes, _ := os.ReadFile(installationIDPath())
	seed := string(seedBytes)
	fingerprint.Lock()
	defer fingerprint.Unlock()
	if strings.TrimSpace(seed) != "" {
		if fingerprint.derivedSeed != seed || fingerprint.derivedHex == "" {
			fingerprint.derivedSeed = seed
			fingerprint.derivedHex = hex.EncodeToString(expandSeed(seed, fingerprintBytes))
		}
		return fingerprint.derivedHex
	}
	if fingerprint.random == "" {
		fingerprint.random = hex.EncodeToString(randomBytes(fingerprintBytes))
	}
	return fingerprint.random
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

// clientMetadata embeds the token once (it is doubled only in the auth header).
func clientMetadata(token string) []byte {
	version := clientVersion()
	return Concat(
		StringField(1, clientName),
		StringField(2, version),
		StringField(3, token),
		StringField(4, "en"),
		StringField(5, runtime.GOOS),
		StringField(7, version),
		StringField(12, clientName),
		StringField(31, deviceFingerprint()),
	)
}

func normalizeBaseURL(baseURL string) string {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return strings.TrimRight(baseURL, "/")
}

// ChatURL is the GetChatMessage endpoint under baseURL.
func ChatURL(baseURL string) string {
	return normalizeBaseURL(baseURL) + chatPath
}

// HeaderKind picks the Connect content type: a server stream or a unary call.
type HeaderKind int

const (
	HeadersStream HeaderKind = iota
	HeadersUnary
)

// Headers are the request headers for a Devin call.
func Headers(token string, kind HeaderKind) http.Header {
	h := http.Header{}
	h.Set("authorization", "Basic "+token+"-"+token)
	h.Set("connect-protocol-version", "1")
	h.Set("accept", "*/*")
	h.Set("user-agent", "connect-es/2.0.0")
	if kind == HeadersUnary {
		h.Set("content-type", "application/proto")
		return h
	}
	h.Set("content-type", "application/connect+proto")
	h.Set("connect-accept-encoding", "gzip")
	h.Set("sentry-trace", hex.EncodeToString(randomBytes(16))+"-"+hex.EncodeToString(randomBytes(8))+"-1")
	return h
}

// EncodeFrame wraps payload in one Connect envelope: flags byte + 4-byte
// big-endian length + payload.
func EncodeFrame(payload []byte, flags byte) []byte {
	out := make([]byte, 5+len(payload))
	out[0] = flags
	binary.BigEndian.PutUint32(out[1:5], uint32(len(payload)))
	copy(out[5:], payload)
	return out
}

// ─── request encoding ───────────────────────────────────────────────────────

// ChatOptions tune BuildChatRequest.
type ChatOptions struct {
	// SessionID is stable per conversation so Devin's prompt cache keeps hitting.
	SessionID string
	// MaxOutput is the model's output ceiling, used when the body names none.
	MaxOutput int
	// NoBuiltins disables the built-in rival-prompt rewrites (promptPolicy.builtins=false).
	NoBuiltins bool
}

type image struct {
	data string
	mime string
}

type toolCallInput struct {
	id   string
	name string
	args string
}

type turn struct {
	source     int
	text       string
	images     []image
	toolCalls  []toolCallInput
	toolCallID *string
}

// contentText flattens Chat Completions content (string or parts) into text.
func contentText(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var texts []string
		for _, part := range c {
			if s, ok := part.(string); ok {
				if s != "" {
					texts = append(texts, s)
				}
				continue
			}
			if t, ok := wire.AsRecord(part)["text"].(string); ok && t != "" {
				texts = append(texts, t)
			}
		}
		return strings.Join(texts, "\n")
	case nil:
		return ""
	}
	return jsonString(content)
}

var dataURL = regexp.MustCompile(`(?is)^data:([a-z0-9.+-]+/[a-z0-9.+-]+);base64,(.+)$`)
var whitespace = regexp.MustCompile(`\s`)

// contentImages keeps inline `data:` images only; remote URLs are skipped.
func contentImages(content any) []image {
	parts, ok := content.([]any)
	if !ok {
		return nil
	}
	var images []image
	for _, part := range parts {
		record := wire.AsRecord(part)
		if record["type"] != "image_url" {
			continue
		}
		raw := record["image_url"]
		url, isString := raw.(string)
		if !isString {
			url = wire.AsString(wire.AsRecord(raw)["url"])
		}
		if m := dataURL.FindStringSubmatch(whitespace.ReplaceAllString(url, "")); m != nil {
			images = append(images, image{mime: strings.ToLower(m[1]), data: m[2]})
		}
	}
	return images
}

func argumentsJSON(value any) string {
	if s, ok := value.(string); ok {
		if strings.TrimSpace(s) != "" {
			return s
		}
		return "{}"
	}
	if value == nil {
		return "{}"
	}
	return jsonString(value)
}

func newCallID() string {
	return "call_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:24]
}

func assistantToolCalls(raw any) []toolCallInput {
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	var out []toolCallInput
	for _, entry := range list {
		call := wire.AsRecord(entry)
		fn := wire.AsRecord(call["function"])
		name := wire.FirstNonEmpty(fn["name"], call["name"])
		if name == "" {
			continue
		}
		id := wire.AsString(call["id"])
		if id == "" {
			id = newCallID()
		}
		args, has := fn["arguments"]
		if !has || args == nil {
			args = call["arguments"]
		}
		out = append(out, toolCallInput{id: id, name: name, args: argumentsJSON(args)})
	}
	return out
}

func isPlainText(t *turn) bool {
	return (t.source == sourceUser || t.source == sourceAssistant) &&
		len(t.images) == 0 && len(t.toolCalls) == 0 && t.toolCallID == nil
}

// toTurns splits Chat Completions messages into the system prompt and Devin turns.
func toTurns(messages []any) (string, []*turn) {
	var systemParts []string
	var turns []*turn
	push := func(t *turn) {
		if n := len(turns); n > 0 {
			last := turns[n-1]
			// The server rejects runs of >=3 same-source text-only turns; merging keeps the text.
			if last.source == t.source && isPlainText(last) && isPlainText(t) {
				var joined []string
				for _, s := range []string{last.text, t.text} {
					if s != "" {
						joined = append(joined, s)
					}
				}
				last.text = strings.Join(joined, "\n\n")
				return
			}
		}
		turns = append(turns, t)
	}
	for _, raw := range messages {
		message := wire.AsRecord(raw)
		role := message["role"]
		text := contentText(message["content"])
		switch role {
		case "system", "developer":
			if text != "" {
				systemParts = append(systemParts, text)
			}
			continue
		case "tool", "function":
			callID := wire.AsString(message["tool_call_id"])
			images := contentImages(message["content"])
			if callID == "" {
				push(&turn{source: sourceUser, text: "[tool result]: " + text, images: images})
			} else {
				if text == "" {
					text = "(no output)"
				}
				id := callID
				push(&turn{source: sourceTool, text: text, images: images, toolCallID: &id})
			}
			continue
		case "assistant":
			calls := assistantToolCalls(message["tool_calls"])
			// Empty assistant turns carry nothing and upstream rejects them on some selectors.
			if strings.TrimSpace(text) == "" && len(calls) == 0 {
				continue
			}
			push(&turn{source: sourceAssistant, text: text, toolCalls: calls})
			continue
		}
		push(&turn{source: sourceUser, text: text, images: contentImages(message["content"])})
	}
	return strings.Join(systemParts, "\n\n"), turns
}

func encodeTurn(t *turn) []byte {
	parts := [][]byte{
		StringField(1, uuid.NewString()),
		VarintField(2, uint64(t.source)),
		StringField(3, t.text),
	}
	for _, call := range t.toolCalls {
		parts = append(parts, BytesField(6, Concat(
			StringField(1, call.id), StringField(2, call.name), StringField(3, call.args),
		)))
	}
	if t.toolCallID != nil {
		parts = append(parts, StringField(7, *t.toolCallID))
	}
	for _, img := range t.images {
		parts = append(parts, BytesField(10, Concat(StringField(1, img.data), StringField(2, img.mime))))
	}
	return Concat(parts...)
}

// stripSchemaDescriptions drops `description` annotations that Devin's MCP
// validator rejects, keeping property names (a property literally named
// "description" survives) and schema constraints.
func stripSchemaDescriptions(value any, propertyMap bool) any {
	switch v := value.(type) {
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = stripSchemaDescriptions(item, false)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			if !propertyMap && key == "description" {
				continue
			}
			out[key] = stripSchemaDescriptions(item, key == "properties")
		}
		return out
	}
	return value
}

func encodeTools(raw any) (definitions [][]byte, descriptions []string) {
	list, ok := raw.([]any)
	if !ok {
		return nil, nil
	}
	for _, entry := range list {
		tool := wire.AsRecord(entry)
		if t, has := tool["type"]; has && t != "function" {
			continue
		}
		fn := wire.AsRecord(tool["function"])
		name := wire.AsString(fn["name"])
		if name == "" {
			continue
		}
		if description := wire.AsString(fn["description"]); description != "" {
			descriptions = append(descriptions, name+": "+description)
		}
		parameters, has := fn["parameters"]
		if !has {
			parameters = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		definitions = append(definitions, BytesField(10, Concat(
			StringField(1, name),
			StringField(2, name),
			StringField(3, jsonString(stripSchemaDescriptions(parameters, false))),
		)))
	}
	return definitions, descriptions
}

var uuidPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// sessionUUID keeps a UUID-shaped session id as-is and maps arbitrary caller
// keys to a stable UUID-shaped digest.
func sessionUUID(sessionID string) string {
	trimmed := strings.TrimSpace(sessionID)
	if trimmed == "" {
		return uuid.NewString()
	}
	if uuidPattern.MatchString(trimmed) {
		return strings.ToLower(trimmed)
	}
	sum := sha256.Sum256([]byte(trimmed))
	h := hex.EncodeToString(sum[:])
	nibble, _ := strconv.ParseUint(h[16:17], 16, 8)
	variant := strconv.FormatUint((nibble&0x3)|0x8, 16)
	return fmt.Sprintf("%s-%s-4%s-%s%s-%s", h[0:8], h[8:12], h[13:16], variant, h[17:20], h[20:32])
}

func positiveInt(value any) (int, bool) {
	if !wire.IsNumber(value) {
		return 0, false
	}
	n := wire.Number(value)
	if n >= 1 && !math.IsInf(n, 0) {
		return int(math.Floor(n)), true
	}
	return 0, false
}

// BuildChatRequest turns an OpenAI Chat Completions body into an enveloped
// GetChatMessage frame, ready to POST.
func BuildChatRequest(token string, body wire.Body, model string, opts ChatOptions) []byte {
	messages := wire.AsSlice(body["messages"])
	joinedSystem, turns := toTurns(messages)
	definitions, descriptions := encodeTools(body["tools"])

	system := joinedSystem
	if system != "" && !opts.NoBuiltins {
		system = SanitizeSystemPrompt(system)
	}
	if system == "" && len(definitions) > 0 {
		system = toolsSystemPrompt
	}
	var sections []string
	if system != "" {
		sections = append(sections, system)
	}
	if len(descriptions) > 0 {
		sections = append(sections, "Available tools and when to use them:\n"+strings.Join(descriptions, "\n"))
	}
	system = strings.Join(sections, "\n\n")

	maxTokens, ok := positiveInt(body["max_completion_tokens"])
	if !ok {
		maxTokens, ok = positiveInt(body["max_tokens"])
	}
	if !ok {
		maxTokens, ok = positiveInt(float64(opts.MaxOutput))
	}
	if !ok {
		maxTokens = defaultMaxOutput
	}
	temperature := defaultTemperature
	if wire.IsNumber(body["temperature"]) {
		temperature = wire.Number(body["temperature"])
	}
	temperature = math.Max(minTemperature, temperature)
	session := sessionUUID(opts.SessionID)

	completionConfig := Concat(
		VarintField(1, 1),
		VarintField(2, uint64(maxTokens)),
		VarintField(3, contextWindow),
		DoubleField(5, temperature),
		VarintField(7, topK),
		DoubleField(8, topP),
	)
	parts := [][]byte{
		BytesField(1, clientMetadata(token)),
		StringField(2, system),
	}
	for _, t := range turns {
		parts = append(parts, BytesField(3, encodeTurn(t)))
	}
	parts = append(parts, VarintField(7, 5), BytesField(8, completionConfig))
	parts = append(parts, definitions...)
	parts = append(parts,
		BytesField(15, Concat(StringField(1, session), VarintField(3, 4), VarintField(4, 14))),
		StringField(16, session),
		VarintField(20, 1),
		StringField(21, model),
	)
	// The server rejects gzipped request frames, so the envelope flag is always 0.
	return EncodeFrame(Concat(parts...), 0)
}

// jsonString is JSON.stringify: no HTML escaping, "null" on failure.
func jsonString(value any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return "null"
	}
	return strings.TrimSuffix(buf.String(), "\n")
}
