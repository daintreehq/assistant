package host

import (
	"context"
	"encoding/json"
	"testing"
)

// decodeEvent encodes an event the way the transport does and decodes the field map.
func decodeEvent(t *testing.T, e interface {
	encode(string, uint64) ([]byte, error)
},
) map[string]any {
	t.Helper()
	raw, err := e.encode("sess", 1)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

// conversationCleared is the flag a host gates its transcript reset on, so it is ALWAYS
// on the wire — unlike the omit-when-false booleans beside it.
//
// An absent field has to mean exactly one thing: an engine too old to report it. If the
// false case were also absent, "the engine refused the clear" and "the engine has never
// heard of this field" would be the same bytes, and the host would have to guess. That
// guess is the bug: Daintree's panel wiped a live transcript on a refused /clear.
func TestCommandResultAlwaysCarriesConversationCleared(t *testing.T) {
	refused := decodeEvent(t, EvCommandResult{Command: "/clear", Text: "Can't clear while a turn is in progress"})
	got, present := refused["conversationCleared"]
	if !present {
		t.Fatal("conversationCleared missing on a refused clear — the host cannot tell refusal from an old engine")
	}
	if got != false {
		t.Fatalf("conversationCleared = %v on a refused clear, want false", got)
	}

	cleared := decodeEvent(t, EvCommandResult{Command: "/clear", Text: "Conversation cleared", ConversationCleared: true})
	if cleared["conversationCleared"] != true {
		t.Fatalf("conversationCleared = %v on a real clear, want true", cleared["conversationCleared"])
	}

	// Every other command reports false rather than nothing, so the host never has to
	// special-case which commands can clear.
	status := decodeEvent(t, EvCommandResult{Command: "/status", Text: "ok"})
	if status["conversationCleared"] != false {
		t.Fatalf("conversationCleared = %v on an unrelated command, want false", status["conversationCleared"])
	}
	// The neighbouring booleans keep their omit-when-false shape.
	if _, ok := status["quit"]; ok {
		t.Fatal("quit must stay omitted when false")
	}
}

// End to end over the real stdio protocol: the host loop must carry the ENGINE's
// outcome onto the wire. It previously built the command:result event without this
// field at all, which is exactly where the flag was lost.
func TestHostCarriesConversationClearedOntoTheWire(t *testing.T) {
	for _, cleared := range []bool{true, false} {
		factory := func(context.Context, AppParams) (App, error) {
			return &fakeApp{command: CommandOutcome{Text: "x", ConversationCleared: cleared}}, nil
		}
		desc := `{"sessionId":"s","windowId":1,"projectId":"p","cwd":"/x","tier":"system","protocolVersion":3}`
		lines := driveHost(t, factory, []string{
			desc,
			`{"type":"command","sessionId":"s","line":"/clear"}`,
			`{"type":"shutdown","sessionId":"s"}`,
		})

		found := false
		for _, m := range lines {
			if m["type"] != "command:result" {
				continue
			}
			found = true
			if m["conversationCleared"] != cleared {
				t.Fatalf("engine said cleared=%v, wire said %v", cleared, m["conversationCleared"])
			}
		}
		if !found {
			t.Fatalf("no command:result on the stream: %+v", lines)
		}
	}
}
