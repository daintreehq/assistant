package ui

import (
	"os"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/charmbracelet/x/ansi"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// render_harness_test.go is the headless cockpit harness. It constructs the root Model
// (no real App — the View/commit path never touches the app), drives the same message
// sequence Bubble Tea would (WindowSize → CommitArm → masthead scrollback commit + ack)
// to STEADY STATE, and returns View().Content. The splash now plays OUTSIDE Bubble Tea
// (boot_splash.go), so the program opens directly in the cockpit; this harness is the
// permanent regression test that the live footer (composer) survives the first masthead
// commit and never collapses to empty.

// harnessModel builds a Model wired like a real cockpit but with no App: a real event
// pump + a controller with no app (its cmds are never run here), and a masthead seeded
// the way newModel seeds it. The composer carries the slash palette so the steady-state
// footer matches the live shell. The model opens non-booting (splash already played).
func harnessModel() Model {
	m := testModel(100)
	pump := newEventPump()
	m.pump = pump
	m.controller = &controller{pump: pump, inject: &fakeInjector{}}
	m.composer.SetCommands(paletteCommands())
	return m
}

// fakeInjector is the test stand-in for *agent.Session's mid-turn injection buffer. It
// mirrors the Session's LIFO retract / clear semantics so UI tests exercise the real
// composer→controller path without a live App.
type fakeInjector struct{ buf []string }

func (f *fakeInjector) InjectPrompt(text string) { f.buf = append(f.buf, text) }
func (f *fakeInjector) RetractPendingInjection() (string, bool) {
	n := len(f.buf)
	if n == 0 {
		return "", false
	}
	text := f.buf[n-1]
	f.buf = f.buf[:n-1]
	return text, true
}
func (f *fakeInjector) DiscardPendingInjections() { f.buf = nil }

// step applies one msg through the REAL Update and returns the next Model + any cmd.
// It re-asserts the concrete type so the harness can keep folding messages.
func step(t *testing.T, m Model, msg tea.Msg) (Model, tea.Cmd) {
	t.Helper()
	next, cmd := m.Update(msg)
	mm, ok := next.(Model)
	if !ok {
		t.Fatalf("Update returned %T, want ui.Model", next)
	}
	return mm, cmd
}

// drainCommits resolves the scrollback commit queue to quiescence: a scheduleCommit
// emits ONE commit at a time, each ack'd by a ScrollbackCommittedMsg that pops the
// head and schedules the next. We can't run tea.Println for real headless, so we
// observe the in-flight block id off the queue and synthesize its ack. The masthead
// (and any sealed cell) commits this way; loops until nothing is in flight.
func drainCommits(t *testing.T, m Model) Model {
	t.Helper()
	for i := 0; i < 64; i++ {
		// Schedule the next commit; if the queue arms a commit it flips inFlight=true
		// and records which block id is in flight (header first, then sealed cells).
		if !m.queue.inFlight {
			// Ask the queue to arm the next eligible commit (mirrors afterStateChange).
			_ = m.scheduleCommit()
		}
		if !m.queue.inFlight {
			return m // nothing left to commit — steady state
		}
		// Ack the in-flight block. The masthead's id is headerID; a sealed cell's id is
		// its cell id. We don't have the block here, so ack by the queue's own state:
		// the header acks first (headerDone false), else the cell at the committed cursor.
		id := headerID
		if m.queue.headerDone {
			idx := m.queue.liveStart(len(m.transcript))
			if idx < len(m.transcript) {
				id = m.transcript[idx].ID()
			}
		}
		var next Model
		next, _ = step(t, m, ScrollbackCommittedMsg{ID: id, Gen: m.queue.gen})
		m = next
	}
	t.Fatal("drainCommits did not converge")
	return m
}

// bootToSteadyState drives launch → steady-state cockpit and returns the Model with the
// masthead committed and the composer live. The program opens in the cockpit; the only
// gate on the first scrollback commit is CommitArmMsg (one render cycle in — see
// scheduleCommit). MCP/project/snapshot messages add the connected note + dashboard.
func bootToSteadyState(t *testing.T) Model {
	t.Helper()
	m := harnessModel()
	m, _ = step(t, m, tea.WindowSizeMsg{Width: 100, Height: 40})
	// Arm scrollback commits (BT has flushed the short footer by now).
	m, _ = step(t, m, CommitArmMsg{})
	// Settle the async startup metadata (note + dashboard); none of these gate the UI.
	m, _ = step(t, m, MCPConnectedMsg{Transport: "http", ToolCount: 3})
	m, _ = step(t, m, DashboardSnapshotMsg{Snapshot: Dashboard{}})
	m, _ = step(t, m, ProjectNameMsg{Name: "demo"})

	m = drainCommits(t, m)
	return m
}

// steadyStateView is the public dump helper for the verify phase: it returns the
// steady-state View().Content. With DAINTREE_UI_DUMP set it also prints the frame so
// the verify phase can snapshot it.
func steadyStateView(t *testing.T) string {
	t.Helper()
	m := bootToSteadyState(t)
	out := m.View().Content
	if os.Getenv("DAINTREE_UI_DUMP") != "" {
		t.Logf("\n===== STEADY-STATE VIEW =====\n%s\n=============================", out)
	}
	return out
}

// TestSteadyStateShowsComposer is the regression lock for the boot-handoff bug: after
// the masthead commits to scrollback, the live footer must STILL render the composer
// (prompt glyph) and must not collapse to empty/whitespace.
func TestSteadyStateShowsComposer(t *testing.T) {
	out := steadyStateView(t)
	if strings.TrimSpace(ansi.Strip(out)) == "" {
		t.Fatalf("steady-state View() is empty/whitespace — footer collapsed after boot handoff")
	}
	// The composer prompt glyph is the unambiguous "the composer is present" marker.
	if !strings.Contains(out, composerPromptGlyph) {
		t.Fatalf("steady-state View() is missing the composer prompt glyph %q:\n%s", composerPromptGlyph, ansi.Strip(out))
	}
}

// TestSteadyStateMastheadCommitted proves the masthead committed to scrollback
// (headerDone) once the cockpit settled.
func TestSteadyStateMastheadCommitted(t *testing.T) {
	m := bootToSteadyState(t)
	if !m.queue.headerDone {
		t.Fatal("masthead never committed to scrollback (headerDone=false)")
	}
}

// TestNoScrollbackCommitUntilArmed is the regression lock for the commit-arm gate
// (charmbracelet/bubbletea#1613): the FIRST scrollback commit must wait one render
// cycle (CommitArmMsg) so Bubble Tea has flushed the short footer and sized its cell
// buffer to it. If a commit fired before that, tea.Println would lay the masthead out
// with a stale (zero/terminal) height and wipe the footer. So no message before
// CommitArmMsg may arm a commit; the masthead may commit only after it.
func TestNoScrollbackCommitUntilArmed(t *testing.T) {
	m := harnessModel()
	m, _ = step(t, m, tea.WindowSizeMsg{Width: 100, Height: 40})

	// Every startup message that can arrive BEFORE the commit-arm tick must NOT commit.
	premsgs := []tea.Msg{
		MCPConnectedMsg{Transport: "http", ToolCount: 3},
		DashboardSnapshotMsg{Snapshot: Dashboard{}},
		DashboardTickMsg{},
		ProjectNameMsg{Name: "demo"},
	}
	for _, msg := range premsgs {
		m, _ = step(t, m, msg)
		if m.commitArmed {
			t.Fatalf("commitArmed flipped true after %T (before the CommitArm tick)", msg)
		}
		if m.queue.inFlight || m.queue.headerDone {
			t.Fatalf("scrollback commit armed before CommitArmMsg after %T (inFlight=%v headerDone=%v) — masthead would print at a stale height and wipe the footer",
				msg, m.queue.inFlight, m.queue.headerDone)
		}
	}

	// The arm tick lands: commits are now allowed and the masthead commits.
	m, _ = step(t, m, CommitArmMsg{})
	if !m.commitArmed {
		t.Fatal("commitArmed still false after CommitArmMsg")
	}
	m = drainCommits(t, m)
	if !m.queue.headerDone {
		t.Fatal("masthead never committed after the commit arm")
	}
}

// composerPromptGlyph is the leading prompt glyph the composer renders; asserting on
// it proves the composer is in the footer. Kept in sync with composer.Model.View.
const composerPromptGlyph = "›"

// (domain import kept for symmetry with other harness tests that may seed phases.)
var _ = domain.PhaseReceived
