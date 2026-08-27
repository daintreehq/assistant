package commands

import (
	"context"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/app"
	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/config"
	"github.com/daintreehq/assistant/internal/tools"
)

// newSwitchableApp is newOfflineApp WITHOUT a BackendURL override.
//
// The shared harness pins the endpoint, and a pinned session is precisely the one that
// must never be offered a picker — every option would be refused. So the picker tests
// need a session that can actually switch, and the pinned case gets a test of its own.
func newSwitchableApp(t *testing.T) *app.App {
	t.Helper()
	dir := t.TempDir()
	a, err := app.Create(app.CreateOptions{
		Overrides: config.ConfigOverrides{
			Offline:     boolPtr(true),
			StateDir:    strPtr(dir),
			ProjectPath: strPtr(dir),
			Tier:        strPtr("operator"),
		},
		BackendOverride: fakeBackend{},
	})
	if err != nil {
		t.Fatalf("app.Create: %v", err)
	}
	t.Cleanup(func() { _ = a.Shutdown() })
	return a
}

// withAsker wires an AskChoice hook that answers with `pick` (a negative index
// dismisses) and records what it was asked, so a test can check the SHEET as well as
// the outcome.
type asked struct {
	req   tools.AskChoiceRequest
	calls int
}

func withAsker(a *app.App, pick int, err error) *asked {
	rec := &asked{}
	a.SetHooks(app.AppHooks{
		AskChoice: func(_ context.Context, req tools.AskChoiceRequest) (tools.AskChoiceAnswer, error) {
			rec.req, rec.calls = req, rec.calls+1
			if err != nil {
				return tools.AskChoiceAnswer{}, err
			}
			return tools.AskChoiceAnswer{
				Label: req.Options[pick].Label,
				Index: pick,
				Text:  req.Options[pick].Text,
			}, nil
		},
	})
	return rec
}

// The point of the change: on a surface with a real picker, `/backend` with no argument
// ASKS instead of printing a menu shaped for an 80-column terminal.
func TestBackendWithNoArgumentAsksAndAppliesTheAnswer(t *testing.T) {
	a := newSwitchableApp(t)
	rec := withAsker(a, 1, nil) // the second menu entry: the local backend

	res := ui(a, "/backend")
	if !res.Handled {
		t.Fatalf("/backend was not handled: %+v", res)
	}
	if rec.calls != 1 {
		t.Fatalf("the question was put %d times, want exactly 1", rec.calls)
	}
	if got := a.SnapshotConfig().BackendURL; got != backend.LocalBaseURL {
		t.Errorf("the answer did not reach the client: backend is %q", got)
	}
	// The picker and the typed command report in the same words, because they did the
	// same thing — two descriptions of one act is how a user ends up unsure which ran.
	if !strings.Contains(res.Text, "answers from your next message") {
		t.Errorf("the outcome is not reported in the switch's own words: %q", res.Text)
	}
}

// Dismissing is a real outcome. Reporting nothing would read as the command having
// failed; reporting a switch would claim one that never happened.
func TestBackendDismissedChangesNothingAndSaysSo(t *testing.T) {
	a := newSwitchableApp(t)
	before := a.SnapshotConfig().BackendURL
	withAsker(a, 0, tools.ErrQuestionDismissed)

	res := ui(a, "/backend")
	if got := a.SnapshotConfig().BackendURL; got != before {
		t.Errorf("a dismissed question switched the backend to %q", got)
	}
	if !strings.Contains(res.Text, "Nothing changed") {
		t.Errorf("a dismissal is not reported: %q", res.Text)
	}
}

// A surface with no picker (one-shot, a non-TTY) still has to answer the command. The
// printed menu names the live endpoint and says how to switch, so it is a degradation
// in FORM only — and losing it would make /backend unusable exactly where nothing else
// can report the endpoint.
func TestBackendFallsBackToTheMenuWithNoPicker(t *testing.T) {
	a := newSwitchableApp(t)
	// No hooks: App.AskChoice returns ErrNoAskChoiceHook.
	res := ui(a, "/backend")
	if !strings.Contains(res.Text, "Backend endpoint for this session") {
		t.Errorf("no menu was printed for a surface that cannot ask: %q", res.Text)
	}
}

// An argument is still an argument. Turning every /backend into a sheet would break the
// one form that is scriptable and the one a returning user types from memory.
func TestBackendWithAnArgumentDoesNotAsk(t *testing.T) {
	a := newSwitchableApp(t)
	rec := withAsker(a, 0, nil)

	res := ui(a, "/backend local")
	if rec.calls != 0 {
		t.Errorf("an explicit target still opened a sheet")
	}
	if got := a.SnapshotConfig().BackendURL; got != backend.LocalBaseURL {
		t.Errorf("the explicit target did not apply: %q", got)
	}
	if !strings.Contains(res.Text, "answers from your next message") {
		t.Errorf("unexpected result: %q", res.Text)
	}
}

// A pinned session cannot switch, so it must not be offered a sheet: presenting a
// decision the session is not allowed to make, and refusing it only after the user has
// committed to one, is worse than printing the listing that explains the pin.
func TestBackendDoesNotAskWhenTheEndpointIsPinned(t *testing.T) {
	a := newOfflineApp(t) // the shared harness pins via a BackendURL override
	rec := withAsker(a, 0, nil)

	res := ui(a, "/backend")
	if rec.calls != 0 {
		t.Error("a pinned session opened a picker it could not act on")
	}
	if !strings.Contains(res.Text, "cannot be switched from in here") {
		t.Errorf("the pin is not explained: %q", res.Text)
	}
}

// A cancelled question — the command interrupted, or the session tearing down — must
// not fall back to printing the menu. Answering with a listing after the user has just
// taken the command back is the one response that reads as the interrupt not working.
func TestBackendCancelledDoesNotPrintTheMenu(t *testing.T) {
	a := newSwitchableApp(t)
	before := a.SnapshotConfig().BackendURL
	withAsker(a, 0, context.Canceled)

	res := ui(a, "/backend")
	if strings.Contains(res.Text, "Backend endpoint for this session") {
		t.Errorf("a cancelled question printed the menu: %q", res.Text)
	}
	if got := a.SnapshotConfig().BackendURL; got != before {
		t.Errorf("a cancelled question switched the backend to %q", got)
	}
	if !strings.Contains(res.Text, "Nothing changed") {
		t.Errorf("a cancelled question reported nothing: %q", res.Text)
	}
}

// The picker is refused BEFORE it opens when the session is not free, rather than
// opening, being read, being answered, and only then reporting that the switch could not
// be made — the one outcome a question must never produce.
//
// The occupant here is another endpoint reservation, which is the second picker case and
// is reachable on its own. A RUNNING TURN reaches the same refusal through the same
// check and is covered where that check lives (agent.TestReserveEndpointRefusedDuringATurn);
// holding a real turn open from here would mean scripting the model round trip for the
// sake of re-asserting one branch of an `if`.
func TestBackendDoesNotAskWhileTheSessionIsOccupied(t *testing.T) {
	a := newSwitchableApp(t)
	rec := withAsker(a, 0, nil)
	release := holdSession(t, a)
	defer release()

	res := ui(a, "/backend")
	if rec.calls != 0 {
		t.Error("a picker opened while the session was occupied; its answer could not have applied")
	}
	if !strings.Contains(res.Text, "A turn is running") {
		t.Errorf("the refusal does not say why: %q", res.Text)
	}
}

// An UNRESERVED switch to the endpoint already in use must not slip past the picker
// either. It writes the same durable preference a parked picker is about to write, and
// it used to take a fast path that returned before the session was ever consulted —
// reporting success, and pinning the endpoint, while somebody else owned the decision.
func TestBackendCurrentTargetStillGoesThroughTheReservation(t *testing.T) {
	a := newSwitchableApp(t)
	release := holdSession(t, a)
	defer release()

	current := a.SnapshotConfig().BackendURL
	res := BackendSwitchText(a, current)
	if !strings.Contains(res, "A turn is running") {
		t.Errorf("re-pinning the current endpoint bypassed the reservation: %q", res)
	}
}

// holdSession takes an endpoint reservation, the way an open picker does, and returns
// the release.
func holdSession(t *testing.T, a *app.App) func() {
	t.Helper()
	tok, err := a.Session.ReserveEndpoint()
	if err != nil {
		t.Fatalf("could not hold the session for the test: %v", err)
	}
	return func() { a.Session.ReleaseEndpoint(tok) }
}

// EVERY row applies the endpoint it names.
//
// The strongest thing this picker can get wrong, and the quietest: the rows are built in
// one place and the targets in another, so a swap or a mislabel makes the row reading
// "local" switch the session to the deployed backend. Nothing downstream would notice —
// the index is honoured, the switch succeeds, and the user is simply somewhere else.
//
// So each row is answered in turn and checked against the endpoint its own TEXT names,
// rather than against the index that produced it.
func TestEveryPickerRowAppliesTheEndpointItNames(t *testing.T) {
	for i, want := range []struct{ marker, endpoint string }{
		{marker: "official", endpoint: backend.DefaultBaseURL},
		{marker: "local", endpoint: backend.LocalBaseURL},
	} {
		a := newSwitchableApp(t)
		rec := withAsker(a, i, nil)

		res := ui(a, "/backend")
		if rec.calls != 1 {
			t.Fatalf("row %d: the picker was not opened", i)
		}
		if got := len(rec.req.Options); got <= i {
			t.Fatalf("row %d: the sheet offered only %d options", i, got)
		}
		// The row really is the one this case is about, read from what a user would see.
		if text := rec.req.Options[i].Text; !strings.Contains(text, want.marker) {
			t.Fatalf("row %d reads %q, which does not name %q", i, text, want.marker)
		}
		if got := a.SnapshotConfig().BackendURL; got != want.endpoint {
			t.Errorf("choosing the row that reads %q switched to %q, want %q",
				want.marker, got, want.endpoint)
		}
		if strings.Contains(res.Text, "Could not") {
			t.Errorf("row %d was refused: %q", i, res.Text)
		}
	}
}

// The highlight starts on the endpoint that is actually ANSWERING, checked through the
// sheet the user sees rather than through the builder that made it. Enter is the fastest
// key here, so a default pointing anywhere else switches the session by accident.
func TestThePickerDefaultIsTheLiveEndpoint(t *testing.T) {
	a := newSwitchableApp(t)
	if _, err := a.SetBackendURL("local"); err != nil {
		t.Fatalf("SetBackendURL: %v", err)
	}
	rec := withAsker(a, 0, nil)
	ui(a, "/backend")

	if rec.req.Default < 0 || rec.req.Default >= len(rec.req.Options) {
		t.Fatalf("default %d is out of range for %d options", rec.req.Default, len(rec.req.Options))
	}
	row := rec.req.Options[rec.req.Default].Text
	if !strings.Contains(row, "local") {
		t.Errorf("the highlight starts on %q while the session is on the local backend", row)
	}
}

// An answer the surface should never give: an index outside what was offered. Reported
// as the fault it is rather than as a user who declined — nothing changed either way,
// but only one of those two is something somebody needs to go and fix.
func TestBackendRejectsAnOutOfRangeAnswer(t *testing.T) {
	a := newSwitchableApp(t)
	before := a.SnapshotConfig().BackendURL
	rec := &asked{}
	a.SetHooks(app.AppHooks{
		AskChoice: func(_ context.Context, req tools.AskChoiceRequest) (tools.AskChoiceAnswer, error) {
			rec.req, rec.calls = req, rec.calls+1
			return tools.AskChoiceAnswer{Index: len(req.Options)}, nil
		},
	})

	res := ui(a, "/backend")
	if got := a.SnapshotConfig().BackendURL; got != before {
		t.Errorf("an out-of-range answer switched the backend to %q", got)
	}
	if !strings.Contains(res.Text, "Could not switch") {
		t.Errorf("an out-of-range answer was not reported as a fault: %q", res.Text)
	}
}
