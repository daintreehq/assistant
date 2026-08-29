package timers

import (
	"errors"
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
)

type fakeStore struct {
	rows      []domain.TimerRecord
	grants    map[string][]domain.AutomationGrantRecord
	patches   map[string][]map[string]any
	revoked   []string
	revokeN   int
	revokeErr error
	listErr   error
	grantsErr error
	claimErr  error
	// claimBlocks makes the next N claims fail, standing in for the scheduler
	// winning the row; onClaimBlocked runs at that moment so a test can mutate the
	// row exactly the way a real fire would.
	claimBlocks    int
	claimAttempts  int
	onClaimBlocked func()
}

func newFake(rows ...domain.TimerRecord) *fakeStore {
	return &fakeStore{rows: rows, patches: map[string][]map[string]any{},
		grants: map[string][]domain.AutomationGrantRecord{}}
}

func (f *fakeStore) GetTimer(id string) (*domain.TimerRecord, error) {
	for i := range f.rows {
		if f.rows[i].ID == id {
			return &f.rows[i], nil
		}
	}
	return nil, nil
}

func (f *fakeStore) ListTimers(status string) ([]domain.TimerRecord, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []domain.TimerRecord
	for _, r := range f.rows {
		if status == "" || r.Status == status {
			out = append(out, r)
		}
	}
	return out, nil
}

// ClaimDueTimer mirrors the real store's guard: it applies only while the row is
// still scheduled at the fireAt the caller read. `claimBlocks` makes the claim fail
// the first N times, which is how the scheduler-wins race is driven.
func (f *fakeStore) ClaimDueTimer(id string, expectFireAt int64, patch map[string]any) (bool, error) {
	if f.claimErr != nil {
		return false, f.claimErr
	}
	f.claimAttempts++
	if f.claimBlocks > 0 {
		f.claimBlocks--
		if f.onClaimBlocked != nil {
			f.onClaimBlocked()
		}
		return false, nil
	}
	for i := range f.rows {
		if f.rows[i].ID != id || f.rows[i].Status != StatusScheduled || f.rows[i].FireAt != expectFireAt {
			continue
		}
		f.patches[id] = append(f.patches[id], patch)
		if st, ok := patch["status"].(string); ok {
			f.rows[i].Status = st
		}
		return true, nil
	}
	return false, nil
}

func (f *fakeStore) RevokeGrantsByActor(actorID string, _ int64) (int, error) {
	f.revoked = append(f.revoked, actorID)
	if f.revokeErr != nil {
		return 0, f.revokeErr
	}
	return f.revokeN, nil
}

func (f *fakeStore) ListGrants(actorID string, _ int64) ([]domain.AutomationGrantRecord, error) {
	if f.grantsErr != nil {
		return nil, f.grantsErr
	}
	return f.grants[actorID], nil
}

func ptrI64(v int64) *int64   { return &v }
func ptrInt(v int) *int       { return &v }
func ptrStr(v string) *string { return &v }

// Cancelling a live timer retires it AND revokes its grants. The pair is the
// whole reason this operation is shared: a surface that did only the first half
// would leave spendable unattended authority behind.
func TestCancelRetiresAndRevokes(t *testing.T) {
	f := newFake(domain.TimerRecord{ID: "tmr_1", Status: StatusScheduled})
	f.revokeN = 3

	out, err := Cancel(f, "tmr_1", 1000)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if out.AlreadyInactive {
		t.Errorf("a scheduled timer is not already inactive")
	}
	if out.PriorStatus != StatusScheduled {
		t.Errorf("priorStatus = %q", out.PriorStatus)
	}
	if got := f.rows[0].Status; got != "cancelled" {
		t.Errorf("row status = %q, want cancelled", got)
	}
	if len(f.revoked) != 1 || f.revoked[0] != "tmr_1" {
		t.Errorf("grants revoked for %v, want [tmr_1]", f.revoked)
	}
	// The count has to be carried, not recomputed: it is what a confirmation
	// reports back as "and this withdrew N grants".
	if out.RevokedGrants != 3 {
		t.Errorf("revokedGrants = %d, want the store's 3", out.RevokedGrants)
	}
}

// An already-fired timer is NOT re-stamped as cancelled. Overwriting 'fired'
// would erase the record that it ran, and a surface reporting "cancelled" would
// then be describing something that had already done its work.
func TestCancelLeavesAnInactiveTimerAlone(t *testing.T) {
	for _, status := range []string{"fired", "cancelled", "done"} {
		f := newFake(domain.TimerRecord{ID: "tmr_1", Status: status})
		out, err := Cancel(f, "tmr_1", 1000)
		if err != nil {
			t.Fatalf("%s: %v", status, err)
		}
		if !out.AlreadyInactive || out.PriorStatus != status {
			t.Errorf("%s: want alreadyInactive with priorStatus %q, got %+v", status, status, out)
		}
		if len(f.patches["tmr_1"]) != 0 {
			t.Errorf("%s: the row must not be written, got %v", status, f.patches["tmr_1"])
		}
		// The grant sweep still runs. A terminal fire defers its own revoke until
		// after the payload dispatches, so a process that died in that window left
		// a live grant with no timer able to spend it.
		if len(f.revoked) != 1 {
			t.Errorf("%s: the grant sweep must still run, got %v", status, f.revoked)
		}
	}
}

// The scheduler wins the row: fireTimer claims it, dispatches, and defers its grant
// revoke past the dispatch. A read-then-write cancel landing in that window would
// stamp "cancelled" over a timer that had just RUN and yank the grant out from under
// a live dispatch. The conditional claim is what stops both.
func TestCancelLosesTheRowToAConcurrentFire(t *testing.T) {
	f := newFake(domain.TimerRecord{ID: "tmr_1", Status: StatusScheduled, FireAt: 500})
	f.revokeN = 2
	// Every claim fails, and each failure advances the row the way a fire would: a
	// one-shot goes terminal.
	f.claimBlocks = cancelAttempts
	f.onClaimBlocked = func() { f.rows[0].Status = "fired" }

	out, err := Cancel(f, "tmr_1", 1000)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if !out.AlreadyInactive || out.PriorStatus != "fired" {
		t.Fatalf("a timer that fired under us must report as fired, got %+v", out)
	}
	if out.Contended {
		t.Error("a settled outcome is not contended — the row is terminal and honest")
	}
	// The row was NOT re-stamped: the record that it fired survives.
	if f.rows[0].Status != "fired" {
		t.Errorf("row status = %q, want the fire's own status preserved", f.rows[0].Status)
	}
}

// A REPEATING timer that fires under the cancel comes back scheduled, so the retry
// claims the fresh row cleanly. This is the common contended case and it must simply
// work rather than surface anything to the user.
func TestCancelRetriesOntoTheRescheduledRow(t *testing.T) {
	f := newFake(domain.TimerRecord{ID: "tmr_1", Status: StatusScheduled, FireAt: 500})
	f.claimBlocks = 1
	// A fire advanced it to its next occurrence — still scheduled, new fireAt.
	f.onClaimBlocked = func() { f.rows[0].FireAt = 900 }

	out, err := Cancel(f, "tmr_1", 1000)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if out.Contended || out.AlreadyInactive {
		t.Fatalf("the retry should have claimed the rescheduled row, got %+v", out)
	}
	if f.rows[0].Status != "cancelled" {
		t.Errorf("row status = %q, want cancelled", f.rows[0].Status)
	}
	if f.claimAttempts != 2 {
		t.Errorf("claim attempts = %d, want 2 (one lost, one won)", f.claimAttempts)
	}
}

// Contention that outlasts the retry is reported as such, NOT as a cancel. The timer
// is still live and the user's intent was not carried out, so "try again" is the only
// honest answer — and no grant is swept, because whoever won the row owns them.
func TestCancelReportsPersistentContention(t *testing.T) {
	f := newFake(domain.TimerRecord{ID: "tmr_1", Status: StatusScheduled, FireAt: 500})
	f.claimBlocks = cancelAttempts + 1

	out, err := Cancel(f, "tmr_1", 1000)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if !out.Contended {
		t.Fatalf("want a contended outcome, got %+v", out)
	}
	if out.AlreadyInactive {
		t.Error("the timer is still scheduled — it is not inactive")
	}
	if f.rows[0].Status != StatusScheduled {
		t.Errorf("the timer must stay live, got %q", f.rows[0].Status)
	}
	if len(f.revoked) != 0 {
		t.Errorf("a lost row's grants belong to whoever won it, got %v", f.revoked)
	}
}

// A claim that cannot even be attempted is a real failure, not a silent no-op.
func TestCancelPropagatesAClaimError(t *testing.T) {
	f := newFake(domain.TimerRecord{ID: "tmr_1", Status: StatusScheduled, FireAt: 500})
	f.claimErr = errors.New("db locked")
	if _, err := Cancel(f, "tmr_1", 1); err == nil {
		t.Fatal("a failed claim must not look like a successful cancel")
	}
	if len(f.revoked) != 0 {
		t.Errorf("nothing was retired, so nothing should be revoked: %v", f.revoked)
	}
}

func TestCancelUnknownID(t *testing.T) {
	f := newFake()
	if _, err := Cancel(f, "tmr_nope", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if len(f.revoked) != 0 {
		t.Errorf("an unknown timer must not trigger a revoke, got %v", f.revoked)
	}
}

// A failed cascade is REPORTED, not swallowed and not fatal. The timer really is
// retired, so failing the call would be the bigger lie — but a silent zero reads
// as "nothing to clean up" while a grant is still live.
func TestCancelReportsAFailedCascade(t *testing.T) {
	f := newFake(domain.TimerRecord{ID: "tmr_1", Status: StatusScheduled})
	f.revokeErr = errors.New("db locked")
	out, err := Cancel(f, "tmr_1", 1)
	if err != nil {
		t.Fatalf("a failed cascade must not fail the cancel: %v", err)
	}
	if out.GrantRevokeErr == nil {
		t.Fatal("the cascade failure must ride the outcome")
	}
	if f.rows[0].Status != "cancelled" {
		t.Errorf("the timer must still be retired, got %q", f.rows[0].Status)
	}
}

// The view describes the payload without carrying it. What a manager needs is
// the SHAPE of the action and the tool's name; what it must not receive is the
// reminder text or the argument object, both model-written free text.
func TestToViewDescribesThePayloadWithoutCarryingIt(t *testing.T) {
	cases := []struct {
		name     string
		rec      domain.TimerRecord
		wantKind PayloadKind
		wantTool string
	}{
		{
			"reminder",
			domain.TimerRecord{PayloadJson: `{"type":"enqueue","message":"secret note"}`},
			KindReminder, "",
		},
		{
			"tool call",
			domain.TimerRecord{PayloadJson: `{"type":"call_safe_tool","toolCall":{"toolName":"terminal.sendCommand","args":{"cmd":"rm -rf /"}}}`},
			KindToolCall, "terminal.sendCommand",
		},
		{
			// A retired payload type still fires, as a plain reminder, so the row
			// stays listable — it just cannot honestly be called either shape.
			"legacy run_check",
			domain.TimerRecord{PayloadJson: `{"type":"run_check","checkPrompt":"done?"}`},
			KindLegacy, "",
		},
		{
			// Unparseable JSON must not drop the row: an undescribable timer is
			// exactly the one a user most wants to be able to cancel.
			"corrupt payload",
			domain.TimerRecord{PayloadJson: `{not json`},
			KindLegacy, "",
		},
		{
			// The typed column is the fallback when the blob does not say.
			"falls back to the column",
			domain.TimerRecord{PayloadJson: `{}`, PayloadType: "enqueue"},
			KindReminder, "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := ToView(nil, tc.rec, 0)
			if v.PayloadKind != tc.wantKind {
				t.Errorf("kind = %q, want %q", v.PayloadKind, tc.wantKind)
			}
			if v.ToolName != tc.wantTool {
				t.Errorf("toolName = %q, want %q", v.ToolName, tc.wantTool)
			}
		})
	}
}

func TestToViewCarriesRepeatAndTarget(t *testing.T) {
	rec := domain.TimerRecord{
		ID: "tmr_1", Title: "Nightly", FireAt: 500, CreatedAt: 100, RunCount: 4,
		PayloadJson:   `{"type":"enqueue"}`,
		RepeatEveryMs: ptrI64(3600000), MaxRuns: ptrInt(12), RepeatUntil: ptrI64(9000),
		TargetJson: ptrStr(`{"worktreeId":"/p/app","terminalId":"term_7"}`),
	}
	v := ToView(nil, rec, 0)
	if v.Repeat == nil || v.Repeat.EveryMs != 3600000 {
		t.Fatalf("repeat = %+v", v.Repeat)
	}
	if v.Repeat.MaxRuns == nil || *v.Repeat.MaxRuns != 12 {
		t.Errorf("maxRuns = %+v", v.Repeat.MaxRuns)
	}
	if v.Repeat.UntilAt == nil || *v.Repeat.UntilAt != 9000 {
		t.Errorf("untilAt = %+v", v.Repeat.UntilAt)
	}
	if v.Target == nil || v.Target.WorktreeID != "/p/app" || v.Target.TerminalID != "term_7" {
		t.Errorf("target = %+v", v.Target)
	}
	if v.RunCount != 4 {
		t.Errorf("runCount = %d", v.RunCount)
	}
}

// A one-shot timer has no repeat block, and an all-empty target is nil rather
// than an object of blanks a host would have to test field by field.
func TestToViewOmitsAnEmptyRepeatAndTarget(t *testing.T) {
	for _, target := range []*string{nil, ptrStr(""), ptrStr("{}")} {
		v := ToView(nil, domain.TimerRecord{PayloadJson: `{"type":"enqueue"}`, TargetJson: target}, 0)
		if v.Repeat != nil {
			t.Errorf("one-shot timer got a repeat block: %+v", v.Repeat)
		}
		if v.Target != nil {
			t.Errorf("empty target %v became %+v", target, v.Target)
		}
	}
	// A zero interval is not a repeat either — it would render as "every 0ms".
	v := ToView(nil, domain.TimerRecord{PayloadJson: `{}`, RepeatEveryMs: ptrI64(0)}, 0)
	if v.Repeat != nil {
		t.Errorf("a zero interval is not a repeat: %+v", v.Repeat)
	}
}

func TestToViewCountsLiveGrants(t *testing.T) {
	f := newFake()
	f.grants["tmr_1"] = []domain.AutomationGrantRecord{{ID: "grt_a"}, {ID: "grt_b"}}
	v := ToView(f, domain.TimerRecord{ID: "tmr_1", PayloadJson: `{}`}, 0)
	if v.LiveGrants != 2 {
		t.Errorf("liveGrants = %d, want 2", v.LiveGrants)
	}

	if v.GrantsUnknown {
		t.Errorf("a successful read is not unknown")
	}

	// A grants read that fails costs the COUNT, not the row: losing the whole
	// timer list because the grants table hiccuped is the worse outcome. But it must
	// be MARKED unknown rather than collapsing to 0 — the number is quoted in a
	// destructive confirmation, where "holds no authority" and "we could not check"
	// are different sentences and only one of them is true.
	f.grantsErr = errors.New("nope")
	v = ToView(f, domain.TimerRecord{ID: "tmr_1", PayloadJson: `{}`}, 0)
	if !v.GrantsUnknown {
		t.Error("a failed grant read must be reported as unknown, not as zero grants")
	}
	if v.ID != "tmr_1" {
		t.Errorf("the row must survive a failed grant read, got %+v", v)
	}
}

// List returns only what is still scheduled — a fired or cancelled timer is not
// something a manager can act on.
func TestListReturnsOnlyScheduled(t *testing.T) {
	f := newFake(
		domain.TimerRecord{ID: "tmr_1", Status: StatusScheduled, PayloadJson: `{"type":"enqueue"}`},
		domain.TimerRecord{ID: "tmr_2", Status: "fired", PayloadJson: `{"type":"enqueue"}`},
		domain.TimerRecord{ID: "tmr_3", Status: "cancelled", PayloadJson: `{"type":"enqueue"}`},
	)
	got, err := List(f, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].ID != "tmr_1" {
		t.Fatalf("want only the scheduled timer, got %+v", got)
	}
}

func TestListPropagatesAStoreError(t *testing.T) {
	f := newFake()
	f.listErr = errors.New("boom")
	if _, err := List(f, 0); err == nil {
		t.Fatal("a failed list must not look like an empty one")
	}
}

// A nil store is the no-storage session, not a crash.
func TestNilStore(t *testing.T) {
	if rows, err := List(nil, 0); err != nil || rows != nil {
		t.Errorf("List(nil) = %v, %v", rows, err)
	}
	if _, err := Cancel(nil, "tmr_1", 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("Cancel(nil) should report not-found, got %v", err)
	}
}
