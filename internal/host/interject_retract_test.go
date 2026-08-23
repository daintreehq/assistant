package host

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/daintreehq/assistant/internal/agent"
)

// Escape-retract of a buffered follow-up, the cockpit's own affordance.
//
// The LIFO order itself belongs to agent.Session and is covered there
// (agent.TestRetractPendingInjection_LIFO). What is host logic — and what these pin — is
// that the session's answer reaches the parent UNCHANGED in both directions. The
// "nothing to take back" answer carries the weight: a host that read it as a success
// would blank the composer over a retract that never happened, eating what the user
// typed.

// scriptedSession answers retracts from a fixed list and does nothing else.
type scriptedSession struct {
	answers []struct {
		text string
		ok   bool
	}
}

func (s *scriptedSession) Send(context.Context, string, agent.SendOptions) (string, error) {
	return "", nil
}
func (s *scriptedSession) InjectPrompt(string)       {}
func (s *scriptedSession) DiscardPendingInjections() {}
func (s *scriptedSession) RetractPendingInjection() (string, bool) {
	if len(s.answers) == 0 {
		return "", false
	}
	a := s.answers[0]
	s.answers = s.answers[1:]
	return a.text, a.ok
}

func TestInterjectRetractedEncodesText(t *testing.T) {
	raw, err := EvInterjectRetracted{Retracted: true, Text: "actually check the tests"}.encode("ses", 7)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["type"] != "interject:retracted" {
		t.Fatalf("type = %v", got["type"])
	}
	if got["retracted"] != true {
		t.Fatalf("retracted = %v, want true", got["retracted"])
	}
	// Verbatim: it is the user's own text going back into their composer, and a masked
	// version of what they just typed would be worse than not returning it at all.
	if got["text"] != "actually check the tests" {
		t.Fatalf("text = %v, want the original verbatim", got["text"])
	}
}

func TestInterjectRetractedOmitsTextWhenNothingTaken(t *testing.T) {
	raw, err := EvInterjectRetracted{Retracted: false}.encode("ses", 8)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["retracted"] != false {
		t.Fatalf("retracted = %v, want false", got["retracted"])
	}
	// Absent rather than empty, so a host cannot read a blank string as "here is your
	// text back" and clear the composer with it.
	if _, present := got["text"]; present {
		t.Fatal("text present on a failed retract")
	}
}

func TestInterjectRetractCommandParses(t *testing.T) {
	cmd, err := ParseCommand([]byte(`{"sessionId":"ses","type":"interject:retract"}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cmd.Type != CmdInterjectRetract {
		t.Fatalf("type = %v, want %v", cmd.Type, CmdInterjectRetract)
	}
}

func TestRetractInjectionForwardsBothAnswers(t *testing.T) {
	var posted []HostEvent
	b := NewBridge(BridgeOptions{
		SessionID: "s",
		Post:      func(e HostEvent) { posted = append(posted, e) },
	})
	sess := &scriptedSession{answers: []struct {
		text string
		ok   bool
	}{{"take this back", true}, {"", false}}}
	h := &Host{bridge: b, session: sess}

	h.retractInjection()
	h.retractInjection()

	if len(posted) != 2 {
		t.Fatalf("posted %d events, want 2 — a silent non-answer strands the composer", len(posted))
	}
	first, ok := posted[0].(EvInterjectRetracted)
	if !ok || !first.Retracted || first.Text != "take this back" {
		t.Fatalf("first answer = %+v", posted[0])
	}
	second, ok := posted[1].(EvInterjectRetracted)
	if !ok || second.Retracted {
		t.Fatalf("second answer = %+v, want retracted=false", posted[1])
	}
}
