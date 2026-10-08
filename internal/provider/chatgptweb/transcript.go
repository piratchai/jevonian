package chatgptweb

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/xinyao27/jevonian/internal/wire"
)

// MaxTranscriptBytes defines the hard limit for transcript text payload size.
const MaxTranscriptBytes = 128 * 1024 // 128 KiB

const systemRoleWarning = "(Warning: System instructions are presented as transcript text, not privileged browser system messages.)"

// BuildTranscript validates the request body and constructs a literal transcript.
// It explicitly rejects tool-calling, multimodal contents (images, audio), and oversized payloads.
func BuildTranscript(body wire.Body) (string, *Error) {
	// 1. Reject native tool-calling
	if tools := wire.AsSlice(body["tools"]); len(tools) > 0 {
		return "", &Error{
			Status:  http.StatusBadRequest,
			Kind:    KindInvalid,
			Message: "native tool-calling is not supported by the chatgptweb provider; tools must not be configured",
		}
	}

	if toolChoice := body["tool_choice"]; toolChoice != nil {
		choiceStr, isStr := toolChoice.(string)
		if !isStr || (choiceStr != "none" && choiceStr != "") {
			return "", &Error{
				Status:  http.StatusBadRequest,
				Kind:    KindInvalid,
				Message: "native tool_choice is not supported by the chatgptweb provider",
			}
		}
	}

	// 2. Extract and validate messages
	rawMessages := wire.AsSlice(body["messages"])
	if len(rawMessages) == 0 {
		return "", &Error{
			Status:  http.StatusBadRequest,
			Kind:    KindInvalid,
			Message: "request body must include at least one message in 'messages'",
		}
	}

	var sections []string
	hasSystemOrDev := false

	for idx, rawMsg := range rawMessages {
		msg := wire.AsRecord(rawMsg)
		if len(msg) == 0 {
			return "", &Error{
				Status:  http.StatusBadRequest,
				Kind:    KindInvalid,
				Message: fmt.Sprintf("message[%d] is invalid or not an object", idx),
			}
		}

		if toolCalls := wire.AsSlice(msg["tool_calls"]); len(toolCalls) > 0 {
			return "", &Error{
				Status:  http.StatusBadRequest,
				Kind:    KindInvalid,
				Message: fmt.Sprintf("message[%d] contains tool_calls, which are not supported", idx),
			}
		}

		if msg["function_call"] != nil {
			return "", &Error{
				Status:  http.StatusBadRequest,
				Kind:    KindInvalid,
				Message: fmt.Sprintf("message[%d] contains function_call, which is not supported", idx),
			}
		}

		role := strings.ToLower(wire.AsString(msg["role"]))
		if role == "" {
			role = "user"
		}

		contentVal := msg["content"]
		text, err := extractTextContent(contentVal, idx)
		if err != nil {
			return "", err
		}

		switch role {
		case "system", "developer":
			hasSystemOrDev = true
			sections = append(sections, fmt.Sprintf("[%s]: %s\n%s", strings.Title(role), text, systemRoleWarning))
		case "user":
			sections = append(sections, fmt.Sprintf("[User]: %s", text))
		case "assistant":
			sections = append(sections, fmt.Sprintf("[Assistant]: %s", text))
		default:
			sections = append(sections, fmt.Sprintf("[%s]: %s", strings.Title(role), text))
		}
	}

	// If single user message with no prior system/developer context, we can send it directly
	// or with the role label. But when system context is present, preserving role headers ensures accuracy.
	var transcript string
	if len(sections) == 1 && !hasSystemOrDev && len(rawMessages) == 1 {
		// Single turn user prompt: extract raw text directly
		msg := wire.AsRecord(rawMessages[0])
		rawText, _ := extractTextContent(msg["content"], 0)
		transcript = rawText
	} else {
		transcript = strings.Join(sections, "\n\n")
	}

	// 3. Enforce payload size limit
	if len(transcript) > MaxTranscriptBytes {
		return "", &Error{
			Status:  http.StatusBadRequest,
			Kind:    KindInvalid,
			Message: fmt.Sprintf("transcript payload size (%d bytes) exceeds maximum allowed limit (%d bytes)", len(transcript), MaxTranscriptBytes),
		}
	}

	return transcript, nil
}

func extractTextContent(val any, msgIndex int) (string, *Error) {
	if val == nil {
		return "", nil
	}

	if str, ok := val.(string); ok {
		return str, nil
	}

	if parts, ok := val.([]any); ok {
		var texts []string
		for pIdx, part := range parts {
			partMap := wire.AsRecord(part)
			partType := wire.AsString(partMap["type"])
			if partType == "" && partMap["text"] != nil {
				partType = "text"
			}

			if partType != "text" {
				return "", &Error{
					Status:  http.StatusBadRequest,
					Kind:    KindInvalid,
					Message: fmt.Sprintf("message[%d].content[%d] specifies unsupported type %q; non-text modalities (image, audio) are rejected", msgIndex, pIdx, partType),
				}
			}

			text := wire.AsString(partMap["text"])
			texts = append(texts, text)
		}
		return strings.Join(texts, "\n"), nil
	}

	return "", &Error{
		Status:  http.StatusBadRequest,
		Kind:    KindInvalid,
		Message: fmt.Sprintf("message[%d].content must be a string or array of text parts", msgIndex),
	}
}
