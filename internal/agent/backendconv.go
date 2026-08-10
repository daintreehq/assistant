package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/models"
)

// backendToolNameRE is the function-name shape the backend enforces
// (^[A-Za-z0-9_-]{1,64}$). The registry projects internal dotted names to
// wire-safe double-underscore names (fs.read → fs__read), which match this; a
// leaked dotted name is rejected here BEFORE the request, giving a clear local
// failure instead of a backend 400.
const backendToolNamePattern = `^[A-Za-z0-9_-]{1,64}$`

// reservedBackendTools are tool names the backend rejects outright: skill
// find/load (selection is server-owned) in both dotted and wire forms.
var reservedBackendTools = map[string]struct{}{
	"skill.find":  {},
	"skill.load":  {},
	"skill__find": {},
	"skill__load": {},
}

// reservedBackendToolPrefix is the reserved internal-tool prefix the backend
// rejects.
const reservedBackendToolPrefix = "daintree_internal__"

// backendToolDescriptionMaxRunes mirrors the backend's FunctionDef description
// max_length (pydantic counts characters, i.e. runes). The backend REJECTS an
// over-limit description with a 422 — it never truncates — which would sink every
// turn until the tool is fixed. Checked locally so an overgrown description fails
// loudly at the first request with the offending tool named.
const backendToolDescriptionMaxRunes = 8192

// toBackendMessages converts the local conversation to the backend wire shape and
// rejects any non-conversational role up front. The backend validator forbids
// system/developer messages; catching them here turns a would-be 400 into an
// immediate, precise error. Content is encoded exactly as the old DeepSeek wire
// encoder did: a tool result flattens to text; an assistant tool-call turn keeps an
// explicit null content; multimodal parts are forwarded verbatim.
func toBackendMessages(messages []models.ChatMessage) ([]backend.Message, error) {
	out := make([]backend.Message, 0, len(messages))
	for i, m := range messages {
		switch m.Role {
		case "user", "assistant", "tool":
			// allowed
		case "system", "developer":
			return nil, fmt.Errorf("forbidden message role %q for backend at index %d (system text is server-owned)", m.Role, i)
		default:
			return nil, fmt.Errorf("unsupported message role %q for backend at index %d", m.Role, i)
		}

		bm := backend.Message{Role: m.Role}
		switch m.Role {
		case "tool":
			c, err := json.Marshal(m.ContentToText())
			if err != nil {
				return nil, err
			}
			bm.Content = c
			bm.ToolCallID = m.ToolCallID
		case "assistant":
			// An assistant tool-call turn with no prose carries explicit null
			// content (matches the backend's content: str | None contract).
			if m.ContentNull && m.Parts == nil {
				bm.Content = json.RawMessage("null")
			} else {
				c, err := backendStringOrParts(m)
				if err != nil {
					return nil, err
				}
				bm.Content = c
			}
			// Replay the chain-of-thought verbatim. DeepSeek REQUIRES it on every later
			// request for an assistant turn that did a tool call (else 400); it is
			// harmless (omitempty) on a turn that has none, so "always echo" is the
			// simplest correct rule.
			bm.ReasoningContent = m.ReasoningContent
			if len(m.ToolCalls) > 0 {
				bm.ToolCalls = make([]backend.ToolCall, 0, len(m.ToolCalls))
				for _, t := range m.ToolCalls {
					bm.ToolCalls = append(bm.ToolCalls, backend.ToolCall{
						ID:   t.ID,
						Type: "function",
						Function: backend.FunctionCall{
							Name:      t.Function.Name,
							Arguments: coerceToolArgs(t.Function.Arguments),
						},
					})
				}
			}
		default: // user
			c, err := backendStringOrParts(m)
			if err != nil {
				return nil, err
			}
			bm.Content = c
		}
		out = append(out, bm)
	}
	return out, nil
}

// backendStringOrParts marshals a message's content: a multimodal parts array
// verbatim (ChatContentPart emits the exact OpenAI wire shape), else the string
// content (a null coalesced to "").
func backendStringOrParts(m models.ChatMessage) (json.RawMessage, error) {
	if m.Parts != nil {
		return json.Marshal(m.Parts)
	}
	s := m.StringContent
	if m.ContentNull {
		s = ""
	}
	return json.Marshal(s)
}

// coerceToolArgs guarantees an assistant tool call's arguments are a valid JSON
// OBJECT string on the wire. The backend forwards replayed history to the upstream
// model, which rejects the whole request if any assistant tool call's arguments
// aren't a JSON object — so a single malformed call the model emitted would
// otherwise poison every later round. The LOCAL executor still sees the raw
// arguments unchanged; only the wire copy is coerced to "{}" so history stays
// acceptable and the model can recover.
func coerceToolArgs(args string) string {
	trimmed := strings.TrimSpace(args)
	if len(trimmed) < 2 || trimmed[0] != '{' || trimmed[len(trimmed)-1] != '}' ||
		!json.Valid([]byte(trimmed)) {
		return "{}"
	}
	return args
}

// toBackendTools converts projected model tool specs to the backend tool inventory
// and validates names. nil tools project to nil (no tools offered).
func toBackendTools(tools []models.ChatTool) ([]backend.Tool, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	out := make([]backend.Tool, 0, len(tools))
	for _, t := range tools {
		out = append(out, backend.Tool{
			Type: "function",
			Function: backend.FunctionDef{
				Name:        t.Function.Name,
				Description: t.Function.Description,
				Parameters:  t.Function.Parameters,
			},
		})
	}
	if err := validateBackendTools(out); err != nil {
		return nil, err
	}
	return out, nil
}

// validateBackendTools rejects reserved/invalid tool names before a request is
// sent, mirroring the backend's rules so a misconfiguration fails clearly here.
func validateBackendTools(tools []backend.Tool) error {
	for _, t := range tools {
		name := t.Function.Name
		if _, reserved := reservedBackendTools[name]; reserved {
			return fmt.Errorf("forbidden backend tool exposed: %s (skill selection is server-owned)", name)
		}
		if strings.HasPrefix(name, reservedBackendToolPrefix) {
			return fmt.Errorf("reserved backend tool exposed: %s", name)
		}
		if !validBackendToolName(name) {
			return fmt.Errorf("invalid backend tool name: %q (must match %s)", name, backendToolNamePattern)
		}
		if n := utf8.RuneCountInString(t.Function.Description); n > backendToolDescriptionMaxRunes {
			return fmt.Errorf("tool %s description is %d chars, over the backend's %d limit (the backend rejects, never truncates — shorten the Description)",
				name, n, backendToolDescriptionMaxRunes)
		}
	}
	return nil
}

// validBackendToolName is the backend's ASCII function-name rule without the
// regular-expression engine overhead on every tool in every inventory validation.
func validBackendToolName(name string) bool {
	if len(name) < 1 || len(name) > 64 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '_' || c == '-' {
			continue
		}
		return false
	}
	return true
}
