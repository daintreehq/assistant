package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/daintreehq/assistant/benchmarks/orchestration/scenario"
)

// The pre-release schema exposes only the precise timing dimensions; ambiguous legacy
// aliases must not creep back into persisted benchmark results.
func TestLatencyJSONUsesCanonicalFields(t *testing.T) {
	result := ScenarioResult{
		FirstRawMetaMS:  600,
		FirstSkillCueMS: 602,
		FirstContentMS:  1400,
		RoundDetail: []scenario.RoundMetric{{
			RawMetaMS:       590,
			SkillCueMS:      592,
			CommittedMetaMS: 1390,
			FirstTokenMS:    1390,
		}},
	}
	b, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"firstRawMetaMs", "firstSkillCueMs", "firstContentMs"} {
		if _, ok := doc[key]; !ok {
			t.Errorf("result JSON missing %q: %s", key, b)
		}
	}
	rounds, ok := doc["roundDetail"].([]any)
	if !ok || len(rounds) != 1 {
		t.Fatalf("roundDetail = %#v, want one row", doc["roundDetail"])
	}
	round, _ := rounds[0].(map[string]any)
	for _, key := range []string{"rawMetaMs", "skillCueMs", "committedMetaMs", "firstTokenMs"} {
		if _, ok := round[key]; !ok {
			t.Errorf("round JSON missing %q: %s", key, b)
		}
	}
	for _, key := range []string{"firstSignalMs", "preStreamMs"} {
		if _, topLevel := doc[key]; topLevel {
			t.Errorf("result JSON contains removed alias %q: %s", key, b)
		}
		if _, perRound := round[key]; perRound {
			t.Errorf("round JSON contains removed alias %q: %s", key, b)
		}
	}
}

// syntheticLog mirrors debuglog.formatLine output for a 2-round turn: round 0 is
// a tool-call round (no content, meta+done together), round 1 streams content.
// Timestamps are chosen so every derived gap is a distinct, assertable value.
const syntheticLog = `2026-07-07T14:25:46.000Z  session.start  sessionId=ses_test
2026-07-07T14:25:46.180Z  turn.start  historyLen=0  promptBytes=100  runId=run_1  sessionId=ses_test
2026-07-07T14:25:46.185Z  backend.respond.request  instructionRevision=0  round=0  runId=run_1  statePresent=false  turnId=turn_1
2026-07-07T14:25:50.000Z  backend.respond.raw_meta  backendRequestId=req_0  model=daintree-assistant  round=0  runId=run_1  turnId=turn_1
2026-07-07T14:25:51.499Z  backend.respond.meta  backendRequestId=req_0  model=daintree-assistant  round=0  runId=run_1  turnId=turn_1
  skills:
    {
      "active": []
    }
2026-07-07T14:25:51.499Z  backend.respond.done  contentChars=0  contentPreview=  durationMs=5314  finishReason=tool_calls  round=0  runId=run_1  toolCallCount=1  turnId=turn_1
  toolCalls:
    [
      {
        "id": "call_0",
        "name": "context.snapshot"
      }
    ]
  usage:
    {
      "cachedTokens": 11648,
      "completionTokens": 66,
      "promptTokens": 32992,
      "totalTokens": 33058
    }
2026-07-07T14:25:51.602Z  tool.call  actor=main  durationMs=102  ok=true  outcome=ok  risk=read  runId=run_1  tool=context.snapshot
2026-07-07T14:25:51.603Z  backend.respond.request  instructionRevision=0  round=1  runId=run_1  statePresent=true  turnId=turn_1
2026-07-07T14:25:53.000Z  backend.respond.raw_meta  backendRequestId=req_1  model=daintree-assistant  round=1  runId=run_1  turnId=turn_1
2026-07-07T14:25:54.483Z  backend.respond.meta  backendRequestId=req_1  model=daintree-assistant  round=1  runId=run_1  turnId=turn_1
2026-07-07T14:25:55.578Z  backend.respond.done  contentChars=22  contentPreview=**Report ID: TH-2231**  durationMs=3975  finishReason=stop  firstTokenMs=2881  round=1  runId=run_1  turnId=turn_1
  usage:
    {
      "cachedTokens": 33536,
      "completionTokens": 21,
      "promptTokens": 33632,
      "totalTokens": 33653
    }
2026-07-07T14:25:55.580Z  turn.end  durationMs=9400  rounds=2  runId=run_1  status=complete  turnId=turn_1
`

func TestParseDebugLogLatencyDecomposition(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "2026-07-07-ses_test.log")
	if err := os.WriteFile(path, []byte(syntheticLog), 0o600); err != nil {
		t.Fatal(err)
	}

	rr := &scenario.RunResult{DebugLogPath: path}
	parseDebugLog(rr)

	if rr.Rounds != 2 {
		t.Fatalf("Rounds = %d, want 2", rr.Rounds)
	}
	if rr.TurnMS != 9400 {
		t.Errorf("TurnMS = %d, want 9400", rr.TurnMS)
	}
	// turn.start 46.180 → round-0 raw meta 50.000.
	if rr.FirstRawMetaMS != 3820 || rr.FirstSkillCueMS != 0 {
		t.Errorf("first raw/cue timings = %d/%d, want 3820/0", rr.FirstRawMetaMS, rr.FirstSkillCueMS)
	}
	// Round 0 is tool-only, so the first content is round 1 request 51.603 + 2881ms.
	if rr.FirstContentMS != 8304 {
		t.Errorf("FirstContentMS = %d, want 8304", rr.FirstContentMS)
	}
	if got, want := rr.Usage.PromptTokens, 32992+33632; got != want {
		t.Errorf("Usage.PromptTokens = %d, want %d", got, want)
	}
	if got, want := rr.Usage.CachedTokens, 11648+33536; got != want {
		t.Errorf("Usage.CachedTokens = %d, want %d", got, want)
	}
	if got, want := rr.Usage.CompletionTokens, 66+21; got != want {
		t.Errorf("Usage.CompletionTokens = %d, want %d", got, want)
	}

	if len(rr.RoundDetail) != 2 {
		t.Fatalf("RoundDetail len = %d, want 2", len(rr.RoundDetail))
	}
	r0, r1 := rr.RoundDetail[0], rr.RoundDetail[1]

	// Round 0: request 46.185, raw meta 50.000, committed meta/done 51.499.
	if r0.GapBeforeMS != 5 {
		t.Errorf("r0.GapBeforeMS = %d, want 5", r0.GapBeforeMS)
	}
	if r0.RawMetaMS != 3815 || r0.SkillCueMS != 0 || r0.CommittedMetaMS != 5314 {
		t.Errorf("r0 meta metrics = raw %d cue %d committed %d, want 3815/0/5314",
			r0.RawMetaMS, r0.SkillCueMS, r0.CommittedMetaMS)
	}
	if r0.FirstTokenMS != 0 {
		t.Errorf("r0.FirstTokenMS = %d, want 0 (tool-call-only round)", r0.FirstTokenMS)
	}
	if r0.TotalMS != 5314 {
		t.Errorf("r0.TotalMS = %d, want 5314", r0.TotalMS)
	}
	if r0.FinishReason != "tool_calls" {
		t.Errorf("r0.FinishReason = %q, want tool_calls", r0.FinishReason)
	}
	if r0.PromptTokens != 32992 || r0.CachedTokens != 11648 || r0.CompletionTokens != 66 {
		t.Errorf("r0 usage = %d/%d/%d, want 32992/11648/66",
			r0.PromptTokens, r0.CachedTokens, r0.CompletionTokens)
	}

	// Round 1: prior done 51.499 → request 51.603 (104ms: tool ran in between),
	// raw meta 53.000, committed meta 54.483, done 55.578.
	if r1.GapBeforeMS != 104 {
		t.Errorf("r1.GapBeforeMS = %d, want 104", r1.GapBeforeMS)
	}
	if r1.RawMetaMS != 1397 || r1.CommittedMetaMS != 2880 {
		t.Errorf("r1 meta metrics = raw %d committed %d, want 1397/2880",
			r1.RawMetaMS, r1.CommittedMetaMS)
	}
	if r1.FirstTokenMS != 2881 {
		t.Errorf("r1.FirstTokenMS = %d, want 2881", r1.FirstTokenMS)
	}
	if r1.TotalMS != 3975 {
		t.Errorf("r1.TotalMS = %d, want 3975", r1.TotalMS)
	}
	if r1.FinishReason != "stop" {
		t.Errorf("r1.FinishReason = %q, want stop", r1.FinishReason)
	}
	if pct := r1.CacheHitPct(); pct < 99 || pct > 100 {
		t.Errorf("r1.CacheHitPct = %f, want ~99.7", pct)
	}
}

// New traces separate raw SSE meta arrival, the optional eager skill cue, committed
// metadata, and first content. This is the distinction the speculation benchmark
// needs: committed OnMeta intentionally waits until the attempt is safe to keep.
func TestParseDebugLogEagerSkillTiming(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "eager.log")
	log := "2026-07-07T14:25:46.000Z  turn.start  runId=run_1  sessionId=ses_x\n" +
		"2026-07-07T14:25:46.010Z  backend.respond.request  round=0  runId=run_1  turnId=turn_1\n" +
		"2026-07-07T14:25:46.600Z  backend.respond.raw_meta  backendRequestId=req_first  round=0  runId=run_1  turnId=turn_1\n" +
		"2026-07-07T14:25:46.602Z  backend.respond.skill_cue  round=0  runId=run_1  turnId=turn_1\n" +
		// A failed attempt may produce another raw meta; the first observation remains
		// the pre-stream measurement, while committed meta belongs to the kept attempt.
		"2026-07-07T14:25:46.900Z  backend.respond.raw_meta  backendRequestId=req_retry  round=0  runId=run_1  turnId=turn_1\n" +
		"2026-07-07T14:25:47.400Z  backend.respond.meta  backendRequestId=req_retry  round=0  runId=run_1  turnId=turn_1\n" +
		"2026-07-07T14:25:47.800Z  backend.respond.done  contentChars=2  contentPreview=ok  durationMs=1790  finishReason=stop  firstTokenMs=1390  round=0  runId=run_1  turnId=turn_1\n" +
		"2026-07-07T14:25:47.810Z  turn.end  durationMs=1810  rounds=1  runId=run_1  status=complete  turnId=turn_1\n"
	if err := os.WriteFile(path, []byte(log), 0o600); err != nil {
		t.Fatal(err)
	}

	rr := &scenario.RunResult{DebugLogPath: path}
	parseDebugLog(rr)

	if rr.FirstRawMetaMS != 600 || rr.FirstSkillCueMS != 602 || rr.FirstContentMS != 1400 {
		t.Errorf("first timings = raw %d cue %d content %d, want 600/602/1400",
			rr.FirstRawMetaMS, rr.FirstSkillCueMS, rr.FirstContentMS)
	}
	if len(rr.RoundDetail) != 1 {
		t.Fatalf("RoundDetail len = %d, want 1", len(rr.RoundDetail))
	}
	r0 := rr.RoundDetail[0]
	if r0.GapBeforeMS != 10 || r0.RawMetaMS != 590 ||
		r0.SkillCueMS != 592 || r0.CommittedMetaMS != 1390 || r0.FirstTokenMS != 1390 {
		t.Errorf("round timings = %+v, want gap/raw/cue/commit/token 10/590/592/1390/1390", r0)
	}
}

// A log with no backend rounds (e.g. the turn failed before the first request)
// must parse to zeros, not panic or invent rounds.
func TestParseDebugLogEmptyTurn(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.log")
	log := "2026-07-07T14:25:46.000Z  session.start  sessionId=ses_x\n" +
		"2026-07-07T14:25:46.180Z  turn.start  runId=run_1  sessionId=ses_x\n"
	if err := os.WriteFile(path, []byte(log), 0o600); err != nil {
		t.Fatal(err)
	}
	rr := &scenario.RunResult{DebugLogPath: path}
	parseDebugLog(rr)
	if rr.Rounds != 0 || len(rr.RoundDetail) != 0 || rr.FirstRawMetaMS != 0 {
		t.Errorf("empty turn parsed to rounds=%d detail=%d firstRawMeta=%d, want all zero",
			rr.Rounds, len(rr.RoundDetail), rr.FirstRawMetaMS)
	}
}

// A contentPreview containing "  key=" lookalikes (free model text is not
// escaped, and contentPreview sorts BEFORE the numeric keys) must not shadow
// the real fields — done-line extraction reads from the right.
func TestParseDebugLogAdversarialPreviews(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "adversarial.log")
	log := "2026-07-07T14:25:46.180Z  turn.start  historyLen=0  promptPreview=echo   runId=run_9  fake  runId=run_1  sessionId=ses_x\n" +
		"2026-07-07T14:25:46.185Z  backend.respond.request  round=0  runId=run_1  turnId=turn_1\n" +
		"2026-07-07T14:25:47.000Z  backend.respond.meta  backendRequestId=req_0  round=0  runId=run_1  turnId=turn_1\n" +
		"2026-07-07T14:25:48.000Z  backend.respond.done  contentChars=40  contentPreview=table:   durationMs=9999  round=7  ok  durationMs=1815  finishReason=stop  firstTokenMs=900  round=0  runId=run_1  turnId=turn_1\n" +
		"  usage:\n" +
		"    {\n" +
		"      \"cachedTokens\": 64,\n" +
		"      \"completionTokens\": 5,\n" +
		"      \"promptTokens\": 128,\n" +
		"      \"totalTokens\": 133\n" +
		"    }\n" +
		"2026-07-07T14:25:48.100Z  turn.end  durationMs=1920  replyPreview=note   durationMs=5555  rounds=1  runId=run_1  status=complete  turnId=turn_1\n"
	if err := os.WriteFile(path, []byte(log), 0o600); err != nil {
		t.Fatal(err)
	}
	rr := &scenario.RunResult{DebugLogPath: path}
	parseDebugLog(rr)

	if rr.Rounds != 1 || len(rr.RoundDetail) != 1 {
		t.Fatalf("rounds = %d, detail = %d, want 1/1 (preview round=7 must not create a round)",
			rr.Rounds, len(rr.RoundDetail))
	}
	r0 := rr.RoundDetail[0]
	if r0.Round != 0 || r0.TotalMS != 1815 || r0.FirstTokenMS != 900 || r0.FinishReason != "stop" {
		t.Errorf("r0 = round %d total %d firstTok %d finish %q, want 0/1815/900/stop",
			r0.Round, r0.TotalMS, r0.FirstTokenMS, r0.FinishReason)
	}
	// turn.end: durationMs sorts BEFORE replyPreview, so first match is real.
	if rr.TurnMS != 1920 {
		t.Errorf("TurnMS = %d, want 1920 (not the preview's 5555)", rr.TurnMS)
	}
}

// A round that ends in backend.respond.error (no done) must appear in the
// detail as an errored round with an end for gap chaining, and must NOT count
// toward Rounds (completed-round semantics).
func TestParseDebugLogErroredRound(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "error.log")
	log := "2026-07-07T14:25:46.180Z  turn.start  runId=run_1  sessionId=ses_x\n" +
		"2026-07-07T14:25:46.185Z  backend.respond.request  round=0  runId=run_1  turnId=turn_1\n" +
		"2026-07-07T14:25:49.185Z  backend.respond.error  durationMs=3000  round=0  runId=run_1  turnId=turn_1\n" +
		"2026-07-07T14:25:49.285Z  backend.respond.request  round=1  runId=run_1  turnId=turn_1\n" +
		"2026-07-07T14:25:50.285Z  backend.respond.meta  round=1  runId=run_1  turnId=turn_1\n" +
		"2026-07-07T14:25:50.485Z  backend.respond.done  contentChars=2  contentPreview=ok  durationMs=1200  finishReason=stop  round=1  runId=run_1  turnId=turn_1\n"
	if err := os.WriteFile(path, []byte(log), 0o600); err != nil {
		t.Fatal(err)
	}
	rr := &scenario.RunResult{DebugLogPath: path}
	parseDebugLog(rr)

	if rr.Rounds != 1 {
		t.Errorf("Rounds = %d, want 1 (errored round is not a completed round)", rr.Rounds)
	}
	if len(rr.RoundDetail) != 2 {
		t.Fatalf("RoundDetail len = %d, want 2", len(rr.RoundDetail))
	}
	r0, r1 := rr.RoundDetail[0], rr.RoundDetail[1]
	if r0.FinishReason != "error" || r0.TotalMS != 3000 {
		t.Errorf("r0 = finish %q total %d, want error/3000", r0.FinishReason, r0.TotalMS)
	}
	// r1's gap chains from the ERROR instant (49.185), not from r0's request.
	if r1.GapBeforeMS != 100 {
		t.Errorf("r1.GapBeforeMS = %d, want 100", r1.GapBeforeMS)
	}
}

// Events from a SECOND turn in the same log (different runId — e.g. a wake)
// must not merge into the first turn's timeline.
func TestParseDebugLogIgnoresLaterTurns(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "two-turns.log")
	log := "2026-07-07T14:25:46.180Z  turn.start  runId=run_1  sessionId=ses_x\n" +
		"2026-07-07T14:25:46.185Z  backend.respond.request  round=0  runId=run_1  turnId=turn_1\n" +
		"2026-07-07T14:25:47.185Z  backend.respond.meta  round=0  runId=run_1  turnId=turn_1\n" +
		"2026-07-07T14:25:47.385Z  backend.respond.done  contentChars=2  contentPreview=ok  durationMs=1200  finishReason=stop  round=0  runId=run_1  turnId=turn_1\n" +
		"2026-07-07T14:25:47.400Z  turn.end  durationMs=1220  rounds=1  runId=run_1  status=complete  turnId=turn_1\n" +
		"2026-07-07T14:26:00.000Z  turn.start  runId=run_2  sessionId=ses_x\n" +
		"2026-07-07T14:26:00.005Z  backend.respond.request  round=0  runId=run_2  turnId=turn_2\n" +
		"2026-07-07T14:26:09.005Z  backend.respond.meta  round=0  runId=run_2  turnId=turn_2\n" +
		"2026-07-07T14:26:09.205Z  backend.respond.done  contentChars=2  contentPreview=ok  durationMs=9200  finishReason=stop  round=0  runId=run_2  turnId=turn_2\n"
	if err := os.WriteFile(path, []byte(log), 0o600); err != nil {
		t.Fatal(err)
	}
	rr := &scenario.RunResult{DebugLogPath: path}
	parseDebugLog(rr)

	if rr.Rounds != 1 || len(rr.RoundDetail) != 1 {
		t.Fatalf("rounds = %d detail = %d, want 1/1 (second turn must be ignored)", rr.Rounds, len(rr.RoundDetail))
	}
	if rr.RoundDetail[0].TotalMS != 1200 {
		t.Errorf("r0.TotalMS = %d, want 1200 (not overwritten by run_2's round 0)", rr.RoundDetail[0].TotalMS)
	}
	if rr.TurnMS != 1220 {
		t.Errorf("TurnMS = %d, want 1220", rr.TurnMS)
	}
}

func TestStrFieldExtraction(t *testing.T) {
	rest := "contentPreview=The terminal is still there  durationMs=3975  finishReason=stop  firstTokenMs=2881  round=1"
	if got := strField(rest, "finishReason"); got != "stop" {
		t.Errorf("finishReason = %q, want stop", got)
	}
	if got := int64Field(rest, "durationMs"); got != 3975 {
		t.Errorf("durationMs = %d, want 3975", got)
	}
	if n, ok := intField(rest, "round"); !ok || n != 1 {
		t.Errorf("round = %d/%v, want 1/true", n, ok)
	}
	// First-position key (no leading two-space separator).
	if got := strField("round=7  durationMs=1", "round"); got != "7" {
		t.Errorf("first-position round = %q, want 7", got)
	}
	// Absent key.
	if got := int64Field(rest, "missingMs"); got != 0 {
		t.Errorf("absent key = %d, want 0", got)
	}
}
