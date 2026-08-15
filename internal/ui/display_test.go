package ui

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/daintreehq/assistant/internal/app"
	"github.com/daintreehq/assistant/internal/config"
	"github.com/daintreehq/assistant/internal/domain"
)

func displayTestModel() (*app.App, Model) {
	a := &app.App{Config: config.AppConfig{ProjectPath: "/tmp/x", Tier: domain.TierSystem}}
	return a, newModel(context.Background(), a, newEventPump())
}

// The cockpit is the only party that knows how wide the reply renders, so every resize
// has to reach the App — that value is what the next turn tells the backend.
func TestResizePublishesDisplayGeometry(t *testing.T) {
	a, m := displayTestModel()
	if got := a.DisplaySize(); got != nil {
		t.Fatalf("geometry published before any size was known: %+v", got)
	}

	next, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = next.(Model)

	got := a.DisplaySize()
	if got == nil {
		t.Fatal("the first size did not reach the App")
	}
	// Not the raw terminal width: the reply is wrapped at contentW(), after the left
	// inset and the autowrap gutter. Reporting `columns` here would overstate the line
	// the model is actually writing into by the width of the chrome.
	if want := m.contentW(); got.ContentWidth != want {
		t.Errorf("content width = %d, want the render measure %d", got.ContentWidth, want)
	}
	if got.Columns != 100 {
		t.Errorf("columns = %d, want the terminal width 100", got.Columns)
	}
}

// A later resize republishes, so a window dragged narrower mid-session is reflected on
// the next round rather than on the next launch.
func TestResizeRepublishesOnEveryChange(t *testing.T) {
	a, m := displayTestModel()
	next, _ := m.Update(tea.WindowSizeMsg{Width: 200, Height: 50})
	m = next.(Model)
	wide := a.DisplaySize()

	next, _ = m.Update(tea.WindowSizeMsg{Width: 62, Height: 50})
	m = next.(Model)
	narrow := a.DisplaySize()

	if wide == nil || narrow == nil {
		t.Fatalf("missing geometry: wide=%+v narrow=%+v", wide, narrow)
	}
	if narrow.Columns != 62 || narrow.ContentWidth >= wide.ContentWidth {
		t.Fatalf("shrink not reflected: wide=%+v narrow=%+v", wide, narrow)
	}
	// The prose cap (ContentMax) applies before the number is reported: on a maximized
	// window the reply still wraps at the cap, and claiming the full terminal width
	// would invite output shaped for a line that is never drawn.
	if wide.ContentWidth != ContentMax {
		t.Errorf("wide content width = %d, want the prose cap %d", wide.ContentWidth, ContentMax)
	}
}

// A host that reports zero columns (a detached or repainting PTY) has told us we can no
// longer measure anything. The published geometry has to go with it: holding the last
// good size would keep shaping replies for a window that is not there, and the backend
// would have no way to know it was reading a ghost.
func TestZeroWidthResizeClearsPublishedGeometry(t *testing.T) {
	a, m := displayTestModel()
	next, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = next.(Model)
	if a.DisplaySize() == nil {
		t.Fatal("setup: the first size did not publish")
	}

	next, _ = m.Update(tea.WindowSizeMsg{Width: 0, Height: 0})
	m = next.(Model)
	if got := a.DisplaySize(); got != nil {
		t.Fatalf("stale geometry survived an unmeasurable host: %+v", got)
	}

	// And it comes back on the next real size, rather than staying dark for the session.
	next, _ = m.Update(tea.WindowSizeMsg{Width: 90, Height: 30})
	m = next.(Model)
	if got := a.DisplaySize(); got == nil || got.Columns != 90 {
		t.Fatalf("geometry did not recover after a zero size: %+v", got)
	}
}
