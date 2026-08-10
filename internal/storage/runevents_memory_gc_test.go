package storage

import (
	"encoding/json"
	"regexp"
	"testing"
	"time"

	"github.com/daintreehq/assistant/internal/domain"
)

// ---- run_events: round-trip, payload verbatim, UNIQUE(runId,seq), listRuns, scope ----

func TestRunEventsRoundTripAndScope(t *testing.T) {
	idRe := regexp.MustCompile(`^rne_[0-9a-f]{8}$`)
	s := openTest(t, 1234)
	rec, _ := s.InsertRunEvent(domain.RunEventRecord{RunID: "run_1", Seq: 0, Type: "assistant:start"})
	if !idRe.MatchString(rec.ID) || rec.Ts != 1234 {
		t.Fatalf("run event defaults wrong: %+v", rec)
	}
	got, _ := s.ListRunEvents("run_1")
	if len(got) != 1 || got[0].Type != "assistant:start" || got[0].Payload != nil {
		t.Fatalf("round-trip wrong: %+v", got)
	}

	// payload preserved verbatim.
	s.InsertRunEvent(domain.RunEventRecord{RunID: "run_1", Seq: 1, Type: "tool:call", Payload: strPtr(`{"name":"fs.read","id":"c1"}`)})
	got, _ = s.ListRunEvents("run_1")
	var p map[string]any
	json.Unmarshal([]byte(*got[1].Payload), &p)
	if p["name"] != "fs.read" || p["id"] != "c1" {
		t.Fatalf("payload not preserved: %v", p)
	}

	// scoped + seq ASC order.
	s.InsertRunEvent(domain.RunEventRecord{RunID: "run_a", Seq: 2, Type: "assistant:end"})
	s.InsertRunEvent(domain.RunEventRecord{RunID: "run_a", Seq: 0, Type: "assistant:start"})
	s.InsertRunEvent(domain.RunEventRecord{RunID: "run_a", Seq: 1, Type: "tool:call"})
	a, _ := s.ListRunEvents("run_a")
	if len(a) != 3 || a[0].Seq != 0 || a[1].Seq != 1 || a[2].Seq != 2 {
		t.Fatalf("seq order wrong: %+v", a)
	}
	if missing, _ := s.ListRunEvents("run_missing"); len(missing) != 0 {
		t.Fatalf("unknown run should be empty")
	}

	// UNIQUE(runId, seq) backstop.
	if _, err := s.InsertRunEvent(domain.RunEventRecord{RunID: "run_a", Seq: 0, Type: "dup"}); err == nil {
		t.Fatalf("duplicate (runId,seq) must error")
	}
}

func TestListRunsAggregateAndLimit(t *testing.T) {
	s := openTest(t, 1)
	if runs, _ := s.ListRuns(0); len(runs) != 0 {
		t.Fatalf("no runs yet")
	}
	s.InsertRunEvent(domain.RunEventRecord{RunID: "run_old", Seq: 0, Ts: 1000, Type: "assistant:start"})
	s.InsertRunEvent(domain.RunEventRecord{RunID: "run_old", Seq: 1, Ts: 1500, Type: "assistant:end"})
	s.InsertRunEvent(domain.RunEventRecord{RunID: "run_new", Seq: 0, Ts: 2000, Type: "assistant:start"})

	runs, _ := s.ListRuns(0)
	if len(runs) != 2 || runs[0].RunID != "run_new" {
		t.Fatalf("listRuns lastTs DESC wrong: %+v", runs)
	}
	var old domain.RunSummaryRecord
	for _, r := range runs {
		if r.RunID == "run_old" {
			old = r
		}
	}
	if old.FirstTs != 1000 || old.LastTs != 1500 || old.EventCount != 2 {
		t.Fatalf("run aggregate wrong: %+v", old)
	}

	for i := 0; i < 5; i++ {
		s.InsertRunEvent(domain.RunEventRecord{RunID: "r" + string(rune('a'+i)), Seq: 0, Ts: int64(3000 + i), Type: "assistant:start"})
	}
	if limited, _ := s.ListRuns(2); len(limited) != 2 {
		t.Fatalf("listRuns limit want 2, got %d", len(limited))
	}
}

func TestListRunsLabelsFromTurnPrompt(t *testing.T) {
	s := openTest(t, 1)
	// run WITH a turn:prompt → labeled by the prompt (verbatim, untruncated in storage).
	s.InsertRunEvent(domain.RunEventRecord{RunID: "run_lbl", Seq: 0, Ts: 1000, Type: "turn:prompt", Payload: strPtr(`{"prompt":"which worktrees are ready?"}`)})
	s.InsertRunEvent(domain.RunEventRecord{RunID: "run_lbl", Seq: 1, Ts: 1100, Type: "assistant:start"})
	// run with NO turn:prompt → empty label.
	s.InsertRunEvent(domain.RunEventRecord{RunID: "run_nolbl", Seq: 0, Ts: 900, Type: "assistant:start"})
	// run whose FIRST turn:prompt (lowest seq) wins over a later one.
	s.InsertRunEvent(domain.RunEventRecord{RunID: "run_two", Seq: 1, Ts: 800, Type: "turn:prompt", Payload: strPtr(`{"prompt":"second"}`)})
	s.InsertRunEvent(domain.RunEventRecord{RunID: "run_two", Seq: 0, Ts: 700, Type: "turn:prompt", Payload: strPtr(`{"prompt":"first"}`)})

	runs, _ := s.ListRuns(0)
	byID := map[string]domain.RunSummaryRecord{}
	for _, r := range runs {
		byID[r.RunID] = r
	}
	if byID["run_lbl"].Label != "which worktrees are ready?" {
		t.Fatalf("labeled run wrong: %q", byID["run_lbl"].Label)
	}
	if byID["run_nolbl"].Label != "" {
		t.Fatalf("unlabeled run should have empty label, got %q", byID["run_nolbl"].Label)
	}
	if byID["run_two"].Label != "first" {
		t.Fatalf("first turn:prompt (lowest seq) should win, got %q", byID["run_two"].Label)
	}
	// turn:prompt counts as an event in the aggregate.
	if byID["run_lbl"].EventCount != 2 {
		t.Fatalf("event count should include turn:prompt, got %d", byID["run_lbl"].EventCount)
	}
}

func TestListAuditByRunIDScopedAndOrdered(t *testing.T) {
	s := openTest(t, 1)
	s.InsertAudit(domain.AuditRecord{Ts: 200, Actor: domain.ActorMain, ToolName: "fs.read", ArgsJson: "{}", Outcome: "ok", DurationMs: 1, Summary: "b", RunID: strPtr("run_x")})
	s.InsertAudit(domain.AuditRecord{Ts: 100, Actor: domain.ActorMain, ToolName: "fs.list", ArgsJson: "{}", Outcome: "ok", DurationMs: 1, Summary: "a", RunID: strPtr("run_x")})
	s.InsertAudit(domain.AuditRecord{Ts: 150, Actor: domain.ActorMain, ToolName: "git.commit", ArgsJson: "{}", Outcome: "ok", DurationMs: 1, Summary: "c", RunID: strPtr("run_y")})

	x, _ := s.ListAuditByRunID("run_x")
	if len(x) != 2 || x[0].ToolName != "fs.list" || x[1].ToolName != "fs.read" {
		t.Fatalf("listAuditByRunId should be ts ASC + scoped: %+v", x)
	}
	if y, _ := s.ListAuditByRunID("run_y"); len(y) != 1 || y[0].ToolName != "git.commit" {
		t.Fatalf("run_y scope wrong: %+v", y)
	}
	if none, _ := s.ListAuditByRunID("run_none"); len(none) != 0 {
		t.Fatalf("unknown run should be empty")
	}
}

// ---- conversation: persistence + seq order survives reopen ----

func TestConversationPersistsAndOrdersBySeq(t *testing.T) {
	s := openTest(t, 1)
	s.InsertMessage(domain.ConversationMessageRecord{SessionID: "ses_x", Seq: 2, Role: "assistant", Content: "two"})
	s.InsertMessage(domain.ConversationMessageRecord{SessionID: "ses_x", Seq: 0, Role: "system", Content: "zero"})
	s.InsertMessage(domain.ConversationMessageRecord{SessionID: "ses_x", Seq: 1, Role: "user", Content: "one"})
	s.InsertMessage(domain.ConversationMessageRecord{SessionID: "ses_other", Seq: 0, Role: "user", Content: "elsewhere"})

	msgs, _ := s.ListMessages("ses_x")
	if len(msgs) != 3 || msgs[0].Content != "zero" || msgs[1].Content != "one" || msgs[2].Content != "two" {
		t.Fatalf("messages must be seq ASC + session scoped: %+v", msgs)
	}
	// tool-call columns round-trip as nil when absent.
	if msgs[0].ToolCallsJson != nil || msgs[0].ToolCallID != nil {
		t.Fatalf("absent tool-call columns must be nil")
	}
}

// ---- memory FTS edges ----

func TestRecallMemoriesAndOrEdges(t *testing.T) {
	s := openTest(t, 1)
	rec, _ := s.InsertMemory(domain.MemoryRecord{Content: "the CI pipeline runs vitest and tsc"})
	// multi-word AND-of-terms (non-adjacent still matches).
	if got, _ := s.RecallMemories("vitest tsc", MemoryRecallOptions{}); len(got) != 1 || got[0].ID != rec.ID {
		t.Fatalf("AND-of-terms recall failed: %v", got)
	}
	// a term absent from the row excludes it.
	if got, _ := s.RecallMemories("vitest playwright", MemoryRecallOptions{}); len(got) != 0 {
		t.Fatalf("missing term must exclude (AND), got %d", len(got))
	}
	// FTS operator/quote injection never errors.
	for _, q := range []string{`"`, `a "b" c`, "watch OR operators", "near NEAR(x)", "watch*", "(unbalanced"} {
		if _, err := s.RecallMemories(q, MemoryRecallOptions{}); err != nil {
			t.Fatalf("injection-unsafe query %q errored: %v", q, err)
		}
	}
	// blank/whitespace → empty.
	if got, _ := s.RecallMemories("   ", MemoryRecallOptions{}); got != nil {
		t.Fatalf("blank query must be empty")
	}
}

func TestRecallCategoryFilterAndPinOrdering(t *testing.T) {
	s := openTest(t, 1)
	s.InsertMemory(domain.MemoryRecord{Content: "use NodeNext imports", Category: strPtr("convention")})
	s.InsertMemory(domain.MemoryRecord{Content: "NodeNext is also fine here", Category: strPtr("note")})
	conv := "convention"
	if got, _ := s.RecallMemories("NodeNext", MemoryRecallOptions{Category: &conv}); len(got) != 1 || got[0].Category == nil || *got[0].Category != "convention" {
		t.Fatalf("category filter failed: %v", got)
	}

	// pin floats to top; re-pin is a no-op that keeps order stable.
	a, _ := s.InsertMemory(domain.MemoryRecord{Content: "alpha"})
	b, _ := s.InsertMemory(domain.MemoryRecord{Content: "beta"})
	s.PinMemory(a.ID, 100)
	s.PinMemory(b.ID, 200)
	order := func() []string {
		ms, _ := s.ListMemories(MemoryListOptions{})
		var ids []string
		for _, m := range ms {
			ids = append(ids, m.ID)
		}
		return ids
	}
	got := order()
	if len(got) < 2 || got[0] != b.ID || got[1] != a.ID {
		t.Fatalf("most-recently-pinned first: %v", got)
	}
	// re-pinning A must NOT rewrite pinnedAt and jump ahead of B.
	s.PinMemory(a.ID, 300)
	got = order()
	if got[0] != b.ID || got[1] != a.ID {
		t.Fatalf("re-pin should be a true no-op, got %v", got)
	}
}

func TestForgetAndPinIdempotencyOnDeleted(t *testing.T) {
	s := openTest(t, 1000)
	rec, _ := s.InsertMemory(domain.MemoryRecord{Content: "temporary"})
	if ok, _ := s.ForgetMemory(rec.ID, 1000); !ok {
		t.Fatalf("forget should change a row")
	}
	if ok, _ := s.ForgetMemory(rec.ID, 1000); ok {
		t.Fatalf("forget again must be a no-op")
	}
	if m, _ := s.GetMemory(rec.ID, false); m != nil {
		t.Fatalf("soft-deleted memory hidden by default")
	}
	if m, _ := s.GetMemory(rec.ID, true); m == nil || m.DeletedAt == nil {
		t.Fatalf("includeDeleted should surface a stamped deletedAt")
	}
	// pin/unpin on a forgotten memory yield no live row.
	if m, _ := s.PinMemory(rec.ID, 2000); m != nil {
		t.Fatalf("pin on forgotten memory should be nil")
	}
	if m, _ := s.UnpinMemory(rec.ID, 2000); m != nil {
		t.Fatalf("unpin on forgotten memory should be nil")
	}
	if ok, _ := s.ForgetMemory("mem_deadbeef", 1000); ok {
		t.Fatalf("forget unknown id returns false")
	}
}

func TestRecallSurvivesPinUnpinCycle(t *testing.T) {
	s := openTest(t, 1)
	rec, _ := s.InsertMemory(domain.MemoryRecord{Content: "uniqueterm about deployment"})
	if got, _ := s.RecallMemories("uniqueterm", MemoryRecallOptions{}); len(got) != 1 {
		t.Fatalf("pre-pin recall failed")
	}
	s.PinMemory(rec.ID, 100)
	s.UnpinMemory(rec.ID, 200)
	// AFTER UPDATE trigger keeps FTS in sync.
	if got, _ := s.RecallMemories("uniqueterm", MemoryRecallOptions{}); len(got) != 1 {
		t.Fatalf("recall must survive a pin/unpin cycle")
	}
}

// ---- gcRetentionSweep full matrix ----

const gcNOW = int64(9_000_000_000_000)
const gcOLD = int64(1000)

// gcStore opens an in-memory store whose constructor sweep is a no-op (huge
// windows) so tests drive GCRetentionSweep explicitly with overridden retention.
func gcStore(t *testing.T, ret Retention) *Store {
	t.Helper()
	s, err := Open(":memory:", &Options{
		Now:       func() int64 { return gcNOW },
		Retention: &ret,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// baseRet is DefaultRetention so each case overrides only the floor it probes.
func baseRet() Retention { return DefaultRetention }

func ftsCount(t *testing.T, s *Store, term string) int {
	t.Helper()
	var n int
	if err := s.DB().QueryRow(
		"SELECT count(*) FROM memories_fts WHERE memories_fts MATCH ?", term).Scan(&n); err != nil {
		t.Fatalf("fts probe: %v", err)
	}
	return n
}

func TestGCAuditAgeAndCountFloor(t *testing.T) {
	// age window prunes ancient, keeps recent (keepRows floor=1).
	ret := baseRet()
	ret.AuditLogKeepRows = 1
	s := gcStore(t, ret)
	s.InsertAudit(domain.AuditRecord{Ts: gcOLD, Actor: domain.ActorMain, ToolName: "x", ArgsJson: "{}", Outcome: "ok", DurationMs: 1, Summary: "old"})
	recent, _ := s.InsertAudit(domain.AuditRecord{Ts: gcNOW - 1000, Actor: domain.ActorMain, ToolName: "x", ArgsJson: "{}", Outcome: "ok", DurationMs: 1, Summary: "new"})
	if err := s.GCRetentionSweep(gcNOW); err != nil {
		t.Fatal(err)
	}
	kept, _ := s.ListAudit(50)
	if len(kept) != 1 || kept[0].ID != recent.ID {
		t.Fatalf("age sweep should keep only the recent row, got %+v", kept)
	}
}

func TestGCAuditCountFloorWhenAllOld(t *testing.T) {
	ret := baseRet()
	ret.AuditLogKeepRows = 2
	s := gcStore(t, ret)
	for i := 0; i < 5; i++ {
		s.InsertAudit(domain.AuditRecord{Ts: gcOLD + int64(i), Actor: domain.ActorMain, ToolName: "x", ArgsJson: "{}", Outcome: "ok", DurationMs: 1, Summary: "x"})
	}
	s.GCRetentionSweep(gcNOW)
	kept, _ := s.ListAudit(50)
	if len(kept) != 2 || kept[0].Ts != gcOLD+4 || kept[1].Ts != gcOLD+3 {
		t.Fatalf("count floor should keep 2 newest, got %+v", kept)
	}
}

func TestGCKeepRowsZeroRemovesAll(t *testing.T) {
	ret := baseRet()
	ret.AuditLogKeepRows = 0
	s := gcStore(t, ret)
	s.InsertAudit(domain.AuditRecord{Ts: gcOLD, Actor: domain.ActorMain, ToolName: "x", ArgsJson: "{}", Outcome: "ok", DurationMs: 1, Summary: "x"})
	s.InsertAudit(domain.AuditRecord{Ts: gcOLD + 1, Actor: domain.ActorMain, ToolName: "x", ArgsJson: "{}", Outcome: "ok", DurationMs: 1, Summary: "x"})
	s.GCRetentionSweep(gcNOW)
	if kept, _ := s.ListAudit(50); len(kept) != 0 {
		t.Fatalf("keepRows=0 removes all past cutoff, got %d", len(kept))
	}
}

func TestGCPrunesRunByWholeRunAndCoPrunesAudit(t *testing.T) {
	ret := baseRet()
	ret.RunEventsKeepRuns = 0
	s := gcStore(t, ret)
	s.InsertRunEvent(domain.RunEventRecord{RunID: "old", Seq: 0, Ts: gcOLD, Type: "start"})
	s.InsertRunEvent(domain.RunEventRecord{RunID: "old", Seq: 1, Ts: gcOLD + 1, Type: "end"})
	s.InsertRunEvent(domain.RunEventRecord{RunID: "fresh", Seq: 0, Ts: gcNOW - 100, Type: "start"})
	s.InsertRunEvent(domain.RunEventRecord{RunID: "fresh", Seq: 1, Ts: gcNOW - 90, Type: "end"})
	// old run's audit row stamped RECENT — only the run co-prune (keyed on runId) removes it.
	s.InsertAudit(domain.AuditRecord{Ts: gcNOW - 50, Actor: domain.ActorMain, ToolName: "x", ArgsJson: "{}", Outcome: "ok", DurationMs: 1, Summary: "x", RunID: strPtr("old")})
	s.InsertAudit(domain.AuditRecord{Ts: gcNOW - 40, Actor: domain.ActorMain, ToolName: "x", ArgsJson: "{}", Outcome: "ok", DurationMs: 1, Summary: "x", RunID: strPtr("fresh")})

	s.GCRetentionSweep(gcNOW)

	if re, _ := s.ListRunEvents("old"); len(re) != 0 {
		t.Fatalf("old run should be pruned")
	}
	if re, _ := s.ListRunEvents("fresh"); len(re) != 2 {
		t.Fatalf("fresh run must be fully retained")
	}
	if au, _ := s.ListAuditByRunID("old"); len(au) != 0 {
		t.Fatalf("old run's audit must be co-pruned despite recent ts")
	}
	if au, _ := s.ListAuditByRunID("fresh"); len(au) != 1 {
		t.Fatalf("fresh run's audit must survive")
	}
}

func TestGCNeverCoPrunesNullRunIDAudit(t *testing.T) {
	ret := baseRet()
	ret.RunEventsKeepRuns = 0
	s := gcStore(t, ret)
	s.InsertRunEvent(domain.RunEventRecord{RunID: "old", Seq: 0, Ts: gcOLD, Type: "start"})
	s.InsertAudit(domain.AuditRecord{Ts: gcNOW - 10, Actor: domain.ActorMain, ToolName: "x", ArgsJson: "{}", Outcome: "ok", DurationMs: 1, Summary: "x", RunID: strPtr("old")})
	orphan, _ := s.InsertAudit(domain.AuditRecord{Ts: gcNOW - 5, Actor: domain.ActorMain, ToolName: "x", ArgsJson: "{}", Outcome: "ok", DurationMs: 1, Summary: "x"})

	s.GCRetentionSweep(gcNOW)

	if au, _ := s.ListAuditByRunID("old"); len(au) != 0 {
		t.Fatalf("old run's audit co-pruned")
	}
	rows, _ := s.ListAudit(50)
	var sawOrphan bool
	for _, r := range rows {
		if r.ID == orphan.ID {
			sawOrphan = true
		}
	}
	if !sawOrphan {
		t.Fatalf("null-runId audit must survive (NULL never matches IN)")
	}
}

func TestGCRunCountFloorAndQuietTail(t *testing.T) {
	// 3 expired + 2 active; keepRuns=2 keeps the 2 newest expired + both active.
	ret := baseRet()
	ret.RunEventsKeepRuns = 2
	s := gcStore(t, ret)
	for i := 0; i < 3; i++ {
		s.InsertRunEvent(domain.RunEventRecord{RunID: "exp" + string(rune('0'+i)), Seq: 0, Ts: gcOLD + int64(i), Type: "start"})
	}
	s.InsertRunEvent(domain.RunEventRecord{RunID: "act0", Seq: 0, Ts: gcNOW - 200, Type: "start"})
	s.InsertRunEvent(domain.RunEventRecord{RunID: "act1", Seq: 0, Ts: gcNOW - 100, Type: "start"})
	s.GCRetentionSweep(gcNOW)
	if re, _ := s.ListRunEvents("act0"); len(re) != 1 {
		t.Fatalf("active run never a candidate")
	}
	if re, _ := s.ListRunEvents("act1"); len(re) != 1 {
		t.Fatalf("active run never a candidate")
	}
	if re, _ := s.ListRunEvents("exp0"); len(re) != 0 {
		t.Fatalf("oldest expired run should be pruned")
	}
	if re, _ := s.ListRunEvents("exp1"); len(re) != 1 {
		t.Fatalf("exp1 within count floor must survive")
	}
	if re, _ := s.ListRunEvents("exp2"); len(re) != 1 {
		t.Fatalf("exp2 within count floor must survive")
	}

	// keepRuns greater than expired count deletes nothing.
	ret2 := baseRet()
	ret2.RunEventsKeepRuns = 5
	s2 := gcStore(t, ret2)
	s2.InsertRunEvent(domain.RunEventRecord{RunID: "a", Seq: 0, Ts: gcOLD, Type: "start"})
	s2.InsertRunEvent(domain.RunEventRecord{RunID: "b", Seq: 0, Ts: gcOLD + 1, Type: "start"})
	s2.GCRetentionSweep(gcNOW)
	if re, _ := s2.ListRunEvents("a"); len(re) != 1 {
		t.Fatalf("quiet-project tail: a must survive")
	}
	if re, _ := s2.ListRunEvents("b"); len(re) != 1 {
		t.Fatalf("quiet-project tail: b must survive")
	}
}

func TestGCEventsTerminalAndScalarMaxStamp(t *testing.T) {
	s := gcStore(t, baseRet())
	open, _ := s.UpsertEvent(domain.QueuePublishArgs{Source: domain.SourceSystem, Severity: domain.SeverityInfo, Title: "open", Summary: "s"})
	// resolved long ago → deleted.
	resolvedOld, _ := s.UpsertEvent(domain.QueuePublishArgs{Source: domain.SourceSystem, Severity: domain.SeverityInfo, Title: "ro", Summary: "s"})
	s.DB().Exec("UPDATE events SET createdAt = ?, resolvedAt = ? WHERE id = ?", gcOLD, gcOLD, resolvedOld.ID)
	// recent resolved → kept.
	resolvedRecent, _ := s.UpsertEvent(domain.QueuePublishArgs{Source: domain.SourceSystem, Severity: domain.SeverityInfo, Title: "rr", Summary: "s"})
	s.DB().Exec("UPDATE events SET resolvedAt = ? WHERE id = ?", gcNOW-100, resolvedRecent.ID)
	// resolved old BUT expiring only recently → MAX(stamps) keeps it.
	split, _ := s.UpsertEvent(domain.QueuePublishArgs{Source: domain.SourceSystem, Severity: domain.SeverityInfo, Title: "split", Summary: "s"})
	s.DB().Exec("UPDATE events SET createdAt = ?, resolvedAt = ?, expiresAt = ? WHERE id = ?", gcOLD, gcOLD, gcNOW-1, split.ID)

	s.GCRetentionSweep(gcNOW)

	if e, _ := s.GetEvent(open.ID); e == nil {
		t.Fatalf("never-resolved event must be kept")
	}
	if e, _ := s.GetEvent(resolvedOld.ID); e != nil {
		t.Fatalf("old resolved event must be hard-deleted")
	}
	if e, _ := s.GetEvent(resolvedRecent.ID); e == nil {
		t.Fatalf("recent resolved event must be kept")
	}
	if e, _ := s.GetEvent(split.ID); e == nil {
		t.Fatalf("event expiring recently must be kept (scalar MAX of stamps)")
	}
}

func TestGCMemoriesAndFTSEviction(t *testing.T) {
	s := gcStore(t, baseRet())
	gone, _ := s.InsertMemory(domain.MemoryRecord{Content: "uniquewidget alpha"})
	live, _ := s.InsertMemory(domain.MemoryRecord{Content: "uniquewidget beta"})
	s.ForgetMemory(gone.ID, gcOLD) // soft-deleted long ago
	if ftsCount(t, s, "uniquewidget") != 2 {
		t.Fatalf("both rows indexed before sweep")
	}
	s.GCRetentionSweep(gcNOW)
	if m, _ := s.GetMemory(gone.ID, true); m != nil {
		t.Fatalf("soft-deleted memory should be hard-deleted")
	}
	if ftsCount(t, s, "uniquewidget") != 1 {
		t.Fatalf("AFTER DELETE trigger must evict the hard-deleted row from FTS")
	}
	if m, _ := s.GetMemory(live.ID, false); m == nil {
		t.Fatalf("live memory must survive")
	}

	// a recently soft-deleted memory stays inside the undo window.
	s2 := gcStore(t, baseRet())
	rec, _ := s2.InsertMemory(domain.MemoryRecord{Content: "recent forget"})
	s2.ForgetMemory(rec.ID, gcNOW-100)
	s2.GCRetentionSweep(gcNOW)
	if m, _ := s2.GetMemory(rec.ID, true); m == nil || m.DeletedAt == nil || *m.DeletedAt != gcNOW-100 {
		t.Fatalf("recently soft-deleted memory must be retained: %v", m)
	}
}

func TestGCConstructorSweepWithOptions(t *testing.T) {
	// A tiny window so the constructor sweep prunes ancient rows down to the floor.
	ret := DefaultRetention
	ret.AuditLogMaxAge = time.Millisecond
	ret.AuditLogKeepRows = 1
	s := gcStore(t, ret)
	s.InsertAudit(domain.AuditRecord{Ts: gcOLD, Actor: domain.ActorMain, ToolName: "t", ArgsJson: "{}", Outcome: "ok", DurationMs: 1, Summary: "old"})
	s.InsertAudit(domain.AuditRecord{Ts: gcOLD + 1, Actor: domain.ActorMain, ToolName: "t", ArgsJson: "{}", Outcome: "ok", DurationMs: 1, Summary: "old2"})
	// Explicit sweep at the far-future clock = what the constructor would do on reopen.
	s.GCRetentionSweep(gcNOW)
	if kept, _ := s.ListAudit(50); len(kept) != 1 {
		t.Fatalf("constructor-equivalent sweep should leave 1 row, got %d", len(kept))
	}
}
