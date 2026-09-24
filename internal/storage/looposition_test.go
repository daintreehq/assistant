package storage

import (
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
)

// A rebuilt occurrence carries the same loop position fireTimer stamps. Only fired/done
// rows are recovered and both are terminal, so it is always the loop's LAST tick — the
// one a turn most needs to know is last, or it promises a next check that never comes.
func TestRecoveredMessageCarriesTheLoopPosition(t *testing.T) {
	s := memStoreForTest(t)
	every := int64(10 * 60 * 1000)
	maxRuns := 3
	until := domain.NowMS() + 60*60*1000
	target := `{"workflowRunId":"wfr_queue"}`
	if _, err := s.InsertTimer(domain.TimerRecord{
		ID: "tmr_loop", Title: "queue check", Status: "done", RunCount: 3,
		FireAt:        domain.NowMS() - 10_000,
		LastFiredAt:   ptrI64(domain.NowMS() - 5_000),
		RepeatEveryMs: &every, MaxRuns: &maxRuns, RepeatUntil: &until, TargetJson: &target,
		PayloadType: "message",
		PayloadJson: `{"type":"message","message":"check the queue"}`,
	}); err != nil {
		t.Fatalf("insert timer: %v", err)
	}
	if _, err := s.BeginOwnership(domain.NowMS()); err != nil {
		t.Fatalf("begin ownership: %v", err)
	}
	open, err := s.ListEvents(domain.QueueDigestOptions{})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	for _, e := range open {
		if e.Target == nil || e.Target.TimerID != "tmr_loop" || !e.Target.TimerMessage {
			continue
		}
		tg := e.Target
		if !tg.TimerFinal || tg.TimerEveryMs != every || tg.TimerMaxRuns != maxRuns ||
			tg.TimerRepeatUntil != until || tg.WorkflowRunID != "wfr_queue" || tg.TimerOccurrence != 3 {
			t.Fatalf("recovered target = %+v", tg)
		}
		return
	}
	t.Fatal("no recovered message event was published")
}
