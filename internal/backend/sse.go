package backend

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// StreamCallbacks receives streamed events as they arrive. All are optional. The
// final assembled message + usage are returned by the stream parser regardless;
// these callbacks exist for live UI (token streaming, surfacing the newly-loaded
// skill titles up front). They are invoked synchronously on the reader goroutine.
type StreamCallbacks struct {
	// OnMeta fires once, before any content — carries the refreshed state token,
	// the skills outcome, and version markers.
	OnMeta func(StreamMeta)
	// OnContent fires for each visible content fragment, in order.
	OnContent func(string)
	// OnReasoning fires for each chain-of-thought fragment (DeepSeek thinking mode),
	// in order, before the first content fragment. Optional; the parser accumulates
	// reasoning into the final message regardless. Empty stream when thinking is off.
	OnReasoning func(string)
	// OnStatus fires for each `status` event — once, with phase "thinking", the
	// instant chain-of-thought begins. Optional; never fires when thinking is off.
	OnStatus func(StreamStatus)
	// OnToolCallDelta fires for each raw tool-call fragment (optional; the parser
	// accumulates these internally regardless).
	//
	// REPLAY CONTRACT: RespondStream retries transient failures that occur before any
	// content streams, and a failed attempt may already have emitted tool-call
	// fragments — so this callback can fire for fragments from an attempt that is then
	// discarded and replayed. Treat the RETURNED RespondResult.Message.ToolCalls (built
	// from the final attempt's own fresh accumulator) as authoritative; do NOT execute
	// or accumulate tool calls off these raw fragments across the call.
	OnToolCallDelta func(ToolCallDelta)
}

// parseRespondStream reads a named-event SSE stream (meta / delta / done / error)
// and returns the accumulated RespondResult. Guarantees enforced:
//   - meta must arrive (a stream that ends without it is an error);
//   - done terminates successfully; a terminal error event terminates with that
//     error; EOF before done is an "interrupted" error — the parser NEVER
//     fabricates a successful finish the backend did not send;
//   - tool-call delta fragments are accumulated by index into whole tool calls.
//
// There is no OpenAI choices envelope and no [DONE] sentinel.
func parseRespondStream(r io.Reader, cb StreamCallbacks) (RespondResult, error) {
	reader := bufio.NewReaderSize(r, 64*1024)

	var (
		result    RespondResult
		eventName string
		dataLines []string
		content   strings.Builder
		reasoning strings.Builder
		acc       = toolCallAccumulator{byIndex: map[int]*toolAccEntry{}}
		metaSeen  bool
		doneSeen  bool
	)

	// dispatch decodes and applies one fully-buffered event, then clears the
	// buffer. A terminal `error` event returns a non-nil *Error.
	dispatch := func() error {
		name := eventName
		data := strings.Join(dataLines, "\n")
		eventName = ""
		dataLines = nil
		if name == "" && strings.TrimSpace(data) == "" {
			return nil
		}
		switch name {
		case "meta":
			var m StreamMeta
			if err := json.Unmarshal([]byte(data), &m); err != nil {
				return decodeErr("meta", err)
			}
			result.Meta = m
			metaSeen = true
			if cb.OnMeta != nil {
				cb.OnMeta(m)
			}
		case "status":
			// Optional thinking-phase marker (DeepSeek thinking mode). Decode for the
			// UX hook; a malformed/unknown status is ignored, never an error.
			var st StreamStatus
			if json.Unmarshal([]byte(data), &st) == nil && cb.OnStatus != nil {
				cb.OnStatus(st)
			}
		case "delta":
			var d StreamDelta
			if err := json.Unmarshal([]byte(data), &d); err != nil {
				return decodeErr("delta", err)
			}
			if d.ReasoningContent != "" {
				reasoning.WriteString(d.ReasoningContent)
				if cb.OnReasoning != nil {
					cb.OnReasoning(d.ReasoningContent)
				}
			}
			if d.Content != "" {
				content.WriteString(d.Content)
				if cb.OnContent != nil {
					cb.OnContent(d.Content)
				}
			}
			for _, tc := range d.ToolCalls {
				acc.add(tc)
				if cb.OnToolCallDelta != nil {
					cb.OnToolCallDelta(tc)
				}
			}
		case "done":
			var dn StreamDone
			if err := json.Unmarshal([]byte(data), &dn); err != nil {
				return decodeErr("done", err)
			}
			result.FinishReason = dn.FinishReason
			result.Usage = dn.Usage
			doneSeen = true
		case "error":
			var env Envelope
			// Best-effort decode; a malformed error payload still terminates the
			// stream with a generic error rather than a fake success.
			_ = json.Unmarshal([]byte(data), &env)
			if env.Error.Message == "" && env.Error.Code == "" {
				env.Error.Code = "stream_error"
				env.Error.Message = "backend stream emitted an error event"
			}
			return newError(0, env, 0, true)
		default:
			// Unknown event name: ignore for forward compatibility.
		}
		return nil
	}

	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			trimmed := strings.TrimRight(line, "\r\n")
			switch {
			case trimmed == "":
				if derr := dispatch(); derr != nil {
					return result, derr
				}
				// `done` is terminal: stop reading immediately rather than blocking on
				// ReadString for an EOF the server may never send promptly (keep-alive /
				// proxy buffering would otherwise hang the whole turn).
				if doneSeen {
					goto finish
				}
			case strings.HasPrefix(trimmed, ":"):
				// SSE comment line; ignore.
			case strings.HasPrefix(trimmed, "event:"):
				eventName = strings.TrimSpace(trimmed[len("event:"):])
			case strings.HasPrefix(trimmed, "data:"):
				d := strings.TrimPrefix(trimmed[len("data:"):], " ")
				dataLines = append(dataLines, d)
			}
		}
		if err != nil {
			// Flush a trailing event that had no terminating blank line.
			if eventName != "" || len(dataLines) > 0 {
				if derr := dispatch(); derr != nil {
					return result, derr
				}
			}
			if err == io.EOF {
				break
			}
			return result, &Error{Code: "stream_read", Message: "stream read failed: " + err.Error(), Stream: true}
		}
	}

finish:
	result.Message = RespondMessage{
		Role:             "assistant",
		Content:          content.String(),
		ReasoningContent: reasoning.String(),
		ToolCalls:        acc.build(),
	}

	if !metaSeen {
		return result, &Error{Code: "stream_no_meta", Message: "stream ended without a meta event", Stream: true}
	}
	if !doneSeen {
		// A truncated upstream stream is an error, never a silent success.
		return result, &Error{Code: "stream_interrupted", Message: "stream ended before a done event", Stream: true}
	}
	return result, nil
}

func decodeErr(event string, err error) *Error {
	return &Error{
		Code:    "stream_decode",
		Message: fmt.Sprintf("could not decode %s event: %v", event, err),
		Stream:  true,
	}
}

// --------------------------------------------------------------------------
// Tool-call delta accumulation (by index, OpenAI-style).
// --------------------------------------------------------------------------

type toolAccEntry struct {
	id   string
	typ  string
	name string
	args strings.Builder
}

type toolCallAccumulator struct {
	order   []int
	byIndex map[int]*toolAccEntry
}

// add folds one streamed fragment into the accumulator. Fragments for the same
// call share an index; id/name/type arrive once and argument text streams in
// pieces, so each non-empty field overwrites and arguments concatenate.
func (a *toolCallAccumulator) add(tc ToolCallDelta) {
	idx := 0
	if tc.Index != nil {
		idx = *tc.Index
	}
	e := a.byIndex[idx]
	if e == nil {
		e = &toolAccEntry{}
		a.byIndex[idx] = e
		a.order = append(a.order, idx)
	}
	if tc.ID != "" {
		e.id = tc.ID
	}
	if tc.Type != "" {
		e.typ = tc.Type
	}
	if tc.Function.Name != "" {
		e.name = tc.Function.Name
	}
	if tc.Function.Arguments != "" {
		e.args.WriteString(tc.Function.Arguments)
	}
}

// build materializes the accumulated fragments into whole tool calls, sorted by
// index. Nameless entries are dropped; a missing id is synthesized stably from the
// index (so the assistant message and its tool-result messages still pair); empty
// arguments default to "{}" (a parameterless call).
func (a *toolCallAccumulator) build() []ToolCall {
	if len(a.order) == 0 {
		return nil
	}
	order := append([]int(nil), a.order...)
	sort.Ints(order)
	out := make([]ToolCall, 0, len(order))
	for _, idx := range order {
		e := a.byIndex[idx]
		if e.name == "" {
			continue
		}
		id := e.id
		if id == "" {
			id = fmt.Sprintf("call_%d", idx)
		}
		args := e.args.String()
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		out = append(out, ToolCall{ID: id, Type: "function", Function: FunctionCall{Name: e.name, Arguments: args}})
	}
	return out
}
