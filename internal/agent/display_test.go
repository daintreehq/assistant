package agent

import (
	"context"
	"testing"

	"github.com/daintreehq/assistant/internal/models"
	"github.com/daintreehq/assistant/internal/prompts"
)

// The measured geometry has to survive the hop into the backend contract: it is the only
// way the model learns how wide the reply it is writing will actually be.
func TestBuildRuntimeContextCarriesDisplay(t *testing.T) {
	s := &Session{}
	rc := s.buildRuntimeContext(prompts.MainPromptContext{
		Display: &prompts.DisplayContext{Columns: 180, ContentWidth: 100},
	}, nil)
	if rc.Display == nil {
		t.Fatal("display dropped on the way to the wire")
	}
	if rc.Display.Columns != 180 || rc.Display.ContentWidth != 100 {
		t.Fatalf("display = %+v, want 180x100", rc.Display)
	}

	// Unmeasured stays unmeasured. The backend reads an absent block as "unknown" and
	// applies its own default width, which is the correct answer for a piped one-shot,
	// the stdio host, and the headless daemon.
	if got := s.buildRuntimeContext(prompts.MainPromptContext{}, nil).Display; got != nil {
		t.Fatalf("unmeasured surface reported a geometry: %+v", got)
	}
}

// The runtime block is rebuilt every round from the live prompt context, so a window
// resized mid-session reaches the very next request rather than the next launch.
func TestBuildRuntimeContextDisplayTracksResize(t *testing.T) {
	s := &Session{}
	wide := s.buildRuntimeContext(prompts.MainPromptContext{
		Display: &prompts.DisplayContext{Columns: 200, ContentWidth: 100},
	}, nil)
	narrow := s.buildRuntimeContext(prompts.MainPromptContext{
		Display: &prompts.DisplayContext{Columns: 60, ContentWidth: 57},
	}, nil)
	if wide.Display.ContentWidth == narrow.Display.ContentWidth {
		t.Fatalf("runtime.display did not track the resize: %+v vs %+v", wide.Display, narrow.Display)
	}
}

// "Why did it draw a table that wrapped?" is only answerable from a session log if the
// width the reply was shaped for is IN that log — the one input to the output contract
// that lives entirely on the user's screen.
func TestTraceRecordsDisplayGeometry(t *testing.T) {
	cap := &traceCapture{}
	deps := baseDeps(&fakeRouter{results: []models.ChatResult{{Content: "done"}}}, &fakeTools{})
	deps.Trace = cap.record
	deps.PromptContext = prompts.MainPromptContext{
		Display: &prompts.DisplayContext{Columns: 120, ContentWidth: 97},
	}
	if _, err := NewSession(deps).Send(context.Background(), "hello", SendOptions{}); err != nil {
		t.Fatal(err)
	}

	ev, ok := cap.first("backend.respond.request")
	if !ok {
		t.Fatal("missing backend.respond.request")
	}
	runtime, _ := ev.fields["runtime"].(map[string]any)
	display, _ := runtime["display"].(map[string]any)
	if display == nil {
		t.Fatalf("no display in the request trace: %+v", runtime)
	}
	if display["columns"] != 120 || display["contentWidth"] != 97 {
		t.Errorf("display trace = %+v, want 120x97", display)
	}
}
