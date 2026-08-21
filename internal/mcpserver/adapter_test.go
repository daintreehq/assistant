package mcpserver

import (
	"context"
	"errors"
	"testing"

	"github.com/daintreehq/assistant/internal/agent"
	"github.com/daintreehq/assistant/internal/app"
)

// adapter_test.go pins the WIRING between a run and the assistant, which is where this
// package's worst bug lived: nothing installed the run's Recorder as the event sink, so
// every poll returned an empty timeline and a failed turn reported success.
//
// It could not be caught through the Runtime seam, because a fake Runtime is handed the
// sink as an argument and therefore works whether or not the real adapter installs
// anything. These tests drive the adapter itself.

type fakeHooks struct {
	installed []agent.EventSink
	// atSend records what had been installed by the time Send was called, so the test
	// can prove the ORDER — installing after the turn started would be useless.
	atSend agent.EventSink
	sends  int
}

func (f *fakeHooks) SetHooks(h app.AppHooks) {
	f.installed = append(f.installed, h.AgentEvents)
}

func (f *fakeHooks) Send(_ context.Context, prompt string, _ agent.SendOptions) (string, error) {
	f.sends++
	if n := len(f.installed); n > 0 {
		f.atSend = f.installed[n-1]
	}
	if prompt == "boom" {
		return "", errors.New("send failed")
	}
	return "reply to " + prompt, nil
}

func TestAdapterInstallsTheSinkBeforeSending(t *testing.T) {
	hooks := &fakeHooks{}
	sink := NewRecorder(NewRun("mrun_1", "ses_1", "p", func() {}))

	reply, err := sendWithSink(context.Background(), hooks, hooks, "hello", sink)
	if err != nil {
		t.Fatalf("sendWithSink: %v", err)
	}
	if reply != "reply to hello" {
		t.Errorf("reply = %q", reply)
	}
	if len(hooks.installed) != 1 {
		t.Fatalf("SetHooks called %d times, want exactly 1", len(hooks.installed))
	}
	if hooks.installed[0] == nil {
		t.Fatal("a nil sink was installed — the run would record nothing")
	}
	// The order is the point: a sink installed after Send began would miss the turn.
	if hooks.atSend != agent.EventSink(sink) {
		t.Error("the sink must be installed BEFORE the turn starts")
	}
}

// TestAdapterInstallsEachTurnsOwnSink: a session is single-flight, so re-installing per
// turn is safe — but each turn must get ITS OWN sink or two runs would record into one.
func TestAdapterInstallsEachTurnsOwnSink(t *testing.T) {
	hooks := &fakeHooks{}
	first := NewRecorder(NewRun("mrun_1", "ses_1", "a", func() {}))
	second := NewRecorder(NewRun("mrun_2", "ses_1", "b", func() {}))

	if _, err := sendWithSink(context.Background(), hooks, hooks, "a", first); err != nil {
		t.Fatal(err)
	}
	if _, err := sendWithSink(context.Background(), hooks, hooks, "b", second); err != nil {
		t.Fatal(err)
	}
	if len(hooks.installed) != 2 {
		t.Fatalf("SetHooks called %d times, want one per turn", len(hooks.installed))
	}
	if hooks.installed[0] == hooks.installed[1] {
		t.Error("both turns shared a sink; their timelines would be merged into one run")
	}
	if hooks.atSend != agent.EventSink(second) {
		t.Error("the second turn ran against the first turn's sink")
	}
}
