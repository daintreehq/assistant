package costledger

import (
	"sync"
	"testing"

	"github.com/daintreehq/assistant/internal/backend"
)

func usd(v float64) *float64 { return &v }

func ev(op string, amount *float64, complete bool) backend.CostEvent {
	return backend.CostEvent{Op: op, Amount: amount, Complete: complete}
}

// The whole point of the ledger is arithmetic nobody has to check, so check it.
func TestLedgerSumsReportedSpend(t *testing.T) {
	l := New()
	l.Record(ev(RespondOp, usd(0.0000089), true))
	l.Record(ev(RespondOp, usd(0.0000111), true))
	l.Record(ev("terminal_summarize", usd(0.00002), true))

	s := l.Snapshot()
	if got, want := s.Observed, 0.00004; !nearly(got, want) {
		t.Errorf("Observed = %v, want %v", got, want)
	}
	if s.LowerBound {
		t.Error("a total made only of complete, reported calls is a SUM, not a lower bound")
	}
	if s.Calls != 3 {
		t.Errorf("Calls = %d, want 3", s.Calls)
	}
	// Turns and tasks answer different questions and must not be pooled: task spend is
	// the half a user has no other way to see.
	if got, want := s.Turns.Amount, 0.00002; !nearly(got, want) {
		t.Errorf("Turns.Amount = %v, want %v", got, want)
	}
	if got, want := s.Tasks.Amount, 0.00002; !nearly(got, want) {
		t.Errorf("Tasks.Amount = %v, want %v", got, want)
	}
	if s.Turns.Calls != 2 || s.Tasks.Calls != 1 {
		t.Errorf("split = %d turns / %d tasks, want 2 / 1", s.Turns.Calls, s.Tasks.Calls)
	}
}

// ABSENT MEANS UNKNOWN, NEVER FREE. This is the rule the whole package exists to
// enforce: a nil amount must be counted as a call that happened and could not be
// measured, which makes the total a floor — not dropped, and above all not read as zero.
func TestUnreportedCostMakesTheTotalALowerBound(t *testing.T) {
	l := New()
	l.Record(ev(RespondOp, usd(0.01), true))
	l.Record(ev(RespondOp, nil, true))

	s := l.Snapshot()
	if !s.LowerBound {
		t.Error("a call that reported no cost must make the total a lower bound")
	}
	if s.Unreported != 1 {
		t.Errorf("Unreported = %d, want 1", s.Unreported)
	}
	// The call still counts — dropping it would hide that spend happened at all.
	if s.Calls != 2 {
		t.Errorf("Calls = %d, want 2 (the unmeasured call still happened)", s.Calls)
	}
	if got, want := s.Observed, 0.01; !nearly(got, want) {
		t.Errorf("Observed = %v, want %v — an unknown amount must not perturb the sum", got, want)
	}
}

// `complete: false` is the backend telling us its OWN total is partial: a call ran whose
// cost it could not measure. It is a distinct signal from a wholly unreported call, and
// both have to reach the reader.
func TestIncompleteCostMakesTheTotalALowerBound(t *testing.T) {
	l := New()
	l.Record(ev(RespondOp, usd(0.01), false))

	s := l.Snapshot()
	if !s.LowerBound {
		t.Error("an incomplete turn cost must make the total a lower bound")
	}
	if s.Incomplete != 1 {
		t.Errorf("Incomplete = %d, want 1", s.Incomplete)
	}
	if s.Unreported != 0 {
		t.Errorf("Unreported = %d, want 0 — a partial figure is still a figure", s.Unreported)
	}
	// It still contributes what it DID report.
	if got, want := s.Observed, 0.01; !nearly(got, want) {
		t.Errorf("Observed = %v, want %v", got, want)
	}
}

// The breakdown must be ordered deterministically. It is built from a map, so without an
// explicit sort two reads of an unchanged ledger would render differently and a user
// would reasonably conclude the numbers were moving.
func TestByOpIsOrderedMostExpensiveFirstAndStable(t *testing.T) {
	l := New()
	l.Record(ev("terminal_extract_json", usd(0.03), true))
	l.Record(ev(RespondOp, usd(0.05), true))
	l.Record(ev("checkpoint", usd(0.01), true))
	// Two ops on the same amount: the name breaks the tie, so the order is total.
	l.Record(ev("aaa_tie", usd(0.01), true))

	want := []string{RespondOp, "terminal_extract_json", "aaa_tie", "checkpoint"}
	for i := 0; i < 5; i++ {
		got := l.Snapshot().ByOp
		if len(got) != len(want) {
			t.Fatalf("ByOp has %d rows, want %d", len(got), len(want))
		}
		for j, row := range got {
			if row.Op != want[j] {
				t.Fatalf("read %d: ByOp[%d] = %q, want %q", i, j, row.Op, want[j])
			}
		}
	}
}

// The cache ratio explains the bill. "No prompt tokens yet" has to be distinguishable
// from "0% hit rate" — the first is silence, the second is an alarm.
func TestCacheHitRatio(t *testing.T) {
	empty := New().Snapshot()
	if _, ok := empty.CacheHitRatio(); ok {
		t.Error("an empty ledger reported a cache ratio it cannot have")
	}

	l := New()
	l.Record(backend.CostEvent{Op: RespondOp, Amount: usd(0.01), Complete: true, PromptTokens: 1000, CachedTokens: 900})
	l.Record(backend.CostEvent{Op: RespondOp, Amount: usd(0.01), Complete: true, PromptTokens: 1000, CachedTokens: 700})
	ratio, ok := l.Snapshot().CacheHitRatio()
	if !ok {
		t.Fatal("no ratio reported after two calls with prompt tokens")
	}
	if !nearly(ratio, 0.8) {
		t.Errorf("ratio = %v, want 0.8", ratio)
	}
}

// /clear discards the conversation, so it discards its bill too. Reset must also restore
// the lower-bound flag — a stale "≥" surviving into a fresh session would caveat a total
// that has nothing to caveat.
func TestResetClearsEverything(t *testing.T) {
	l := New()
	l.Record(ev(RespondOp, nil, false))
	l.Record(ev("checkpoint", usd(0.02), true))
	l.Reset()

	s := l.Snapshot()
	if s.Calls != 0 || s.Observed != 0 || s.LowerBound || len(s.ByOp) != 0 {
		t.Errorf("ledger not fully cleared: %+v", s)
	}
	// And it still works afterwards — Reset must not leave the map nil.
	l.Record(ev(RespondOp, usd(0.5), true))
	if got := l.Snapshot().Observed; !nearly(got, 0.5) {
		t.Errorf("Observed after reset+record = %v, want 0.5", got)
	}
}

// A nil ledger is the "cost tracking is off" case (the sign-in probe client passes one).
// Every method must tolerate it rather than making each caller nil-check.
func TestNilLedgerIsSafe(t *testing.T) {
	var l *Ledger
	l.Record(ev(RespondOp, usd(1), true))
	l.Reset()
	if s := l.Snapshot(); s.Calls != 0 {
		t.Errorf("nil ledger reported %d calls", s.Calls)
	}
}

// Cost events arrive from the turn goroutine, the watcher engine, the async coordinator
// and the scheduler — concurrently. Run under -race.
func TestConcurrentRecordAndSnapshot(t *testing.T) {
	l := New()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				l.Record(ev(RespondOp, usd(0.001), true))
				_ = l.Snapshot()
			}
		}()
	}
	wg.Wait()
	if got := l.Snapshot().Calls; got != 800 {
		t.Errorf("Calls = %d, want 800", got)
	}
}

// nearly compares floats at a tolerance far below the amounts involved. Exact equality
// would make these tests hostage to float addition order.
func nearly(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-12
}
