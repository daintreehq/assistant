package host

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// turn:end is the frame that says the turn is OVER. Refusing it for being oversize left
// the host showing a turn running forever over a conversation that had finished — and
// unlike a truncated answer, that is not recoverable without restarting the session.
func TestAnOversizeTurnEndIsCutRatherThanDropped(t *testing.T) {
	out := &syncBuffer{}
	tr := newTransport(strings.NewReader(""), out, &bytes.Buffer{})
	tr.start()
	defer tr.Close()

	huge := strings.Repeat("a", maxFrameBytes*2)
	tr.send("ses_1", EvTurnEnd{TurnID: "turn_1", EndedAt: 1, Outcome: OutcomeAnswered, Content: huge, HasContent: true})

	// The writer is its own goroutine, so wait for the frame rather than racing it.
	var line string
	for i := 0; i < 400; i++ {
		if s := strings.TrimSpace(out.String()); s != "" {
			line = s
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if line == "" {
		t.Fatal("an oversize turn:end produced no frame at all; the host never learns the turn ended")
	}
	if len(line)+1 > maxFrameBytes {
		t.Fatalf("the emitted frame is %d bytes, past the %d cap", len(line)+1, maxFrameBytes)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("the shrunk frame is not valid JSON: %v", err)
	}
	if got["type"] != "turn:end" {
		t.Fatalf("type = %v, want turn:end", got["type"])
	}
	if got["outcome"] != string(OutcomeAnswered) {
		t.Errorf("the outcome was lost: %v", got["outcome"])
	}
	content, _ := got["content"].(string)
	if !strings.Contains(content, "too large for one protocol frame") {
		t.Error("the content was cut without saying so; the loss must be visible where the answer is read")
	}
	if !utf8.ValidString(content) {
		t.Error("cutting the content produced invalid UTF-8")
	}
}

// The cut lands on a rune boundary. A byte-exact cut through a multi-byte character is
// invalid UTF-8, which the JSON encoder replaces rather than carries.
func TestTurnEndShrinkCutsOnRuneBoundaries(t *testing.T) {
	ev := EvTurnEnd{TurnID: "t", Content: strings.Repeat("界", 500), HasContent: true}
	for _, budget := range []int{100, 101, 102, 250, 999} {
		shrunk, did := ev.shrink(budget)
		if !did {
			t.Fatalf("budget %d did not shrink a 1500-byte content", budget)
		}
		got := shrunk.(EvTurnEnd).Content
		if !utf8.ValidString(got) {
			t.Errorf("budget %d produced invalid UTF-8", budget)
		}
	}
	// Content already inside the budget is untouched.
	small := EvTurnEnd{TurnID: "t", Content: "short", HasContent: true}
	if _, did := small.shrink(4096); did {
		t.Error("a small content was shrunk")
	}
	// A turn with no content has nothing to shrink.
	if _, did := (EvTurnEnd{TurnID: "t"}).shrink(10); did {
		t.Error("a content-less turn:end reported a shrink")
	}
}

// The documentation said control frames never drop. With a finite queue and an unread
// pipe that is not implementable — so the guarantee became: a critical frame is never
// SILENTLY discarded. If one cannot be delivered, the session fails.
func TestAnUndeliverableControlFrameFailsTheSession(t *testing.T) {
	// A writer that blocks forever: the queue fills and never drains, which is exactly
	// what a consumer that stopped reading looks like — no write error to observe.
	blocked := make(chan struct{})
	var once sync.Once
	// blockingWriter (host_test.go) never completes a write until released, modelling a
	// consumer that has stopped reading. It produces no ERROR, which is exactly why the
	// queue-full path has to declare the peer gone on its own.
	w := &blockingWriter{release: blocked}

	var diag bytes.Buffer
	tr := newTransport(strings.NewReader(""), w, &diag)
	failed := make(chan struct{})
	tr.onSendFail = func(error) { once.Do(func() { close(failed) }) }
	tr.start()
	defer func() { close(blocked); tr.Close() }()

	// Flood past the queue depth so a control frame finds no room.
	for i := 0; i < outQueueDepth+64; i++ {
		tr.send("ses_1", EvTurnEnd{TurnID: "turn_1", EndedAt: int64(i), Outcome: OutcomeAnswered})
	}

	select {
	case <-failed:
	case <-time.After(5 * time.Second):
		t.Fatal("a control frame was dropped and the session carried on with a hole in the stream")
	}
	if !strings.Contains(diag.String(), "PROTOCOL GAP") {
		t.Errorf("the drop was not reported on the diagnostic channel: %q", diag.String())
	}
}

// resumeSessionId decides which conversation a session continues. Swallowing a type
// mismatch turned a resume request into "" — indistinguishable from "start fresh" — so a
// host that asked to resume got a blank session and no indication it had been discarded.
func TestResumeSessionIDIsTyped(t *testing.T) {
	base := `{"sessionId":"ses_1","windowId":7,"projectId":"proj_1","cwd":"/tmp","tier":"system","protocolVersion":4`

	d, err := ParseDescriptor([]byte(base + `,"resumeSessionId":"ses_prev"}`))
	if err != nil || d.ResumeSessionID != "ses_prev" {
		t.Fatalf("a valid resume id gave (%q, %v)", d.ResumeSessionID, err)
	}
	// Absent and null both mean "start fresh", which is a legitimate serialization of an
	// absent optional.
	for _, tail := range []string{`}`, `,"resumeSessionId":null}`} {
		d, err := ParseDescriptor([]byte(base + tail))
		if err != nil {
			t.Errorf("%s was rejected: %v", tail, err)
		}
		if d.ResumeSessionID != "" {
			t.Errorf("%s produced resume id %q", tail, d.ResumeSessionID)
		}
	}
	// A wrong type is refused rather than silently discarded.
	for _, tail := range []string{
		`,"resumeSessionId":7}`,
		`,"resumeSessionId":{"id":"ses_prev"}}`,
		`,"resumeSessionId":["ses_prev"]}`,
		`,"resumeSessionId":true}`,
	} {
		if _, err := ParseDescriptor([]byte(base + tail)); err == nil {
			t.Errorf("%s was accepted; a discarded resume request looks exactly like a fresh session", tail)
		}
	}
	// And it is bounded in BYTES — it ends up naming a file on disk.
	long := strings.Repeat("s", maxSessionIDBytes+1)
	if _, err := ParseDescriptor([]byte(base + `,"resumeSessionId":"` + long + `"}`)); err == nil {
		t.Error("an over-long resume id was accepted")
	}
}

// syncBuffer is a bytes.Buffer safe to read while the writer goroutine writes it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// host:shutdown is TERMINAL: no frame ever follows it, and its seq is the highest of the
// session. A consumer that correctly stops reading at that line must not lose the tail of
// the turn — which is why teardown SEALS the stream and drains what is queued before
// stamping the shutdown frame, rather than writing it directly and racing the writer.
//
// The contract was implemented but never stated or tested, which for a cross-repository
// invariant is the same as not having one: nothing stops a later change from emitting a
// frame after it.
func TestNothingFollowsHostShutdown(t *testing.T) {
	out := &syncBuffer{}
	tr := newTransport(strings.NewReader(""), out, &bytes.Buffer{})
	tr.start()

	// A turn's worth of ordinary traffic still queued when teardown begins.
	for i := 0; i < 40; i++ {
		tr.send("ses_1", EvTurnToken{TurnID: "turn_1", Chunk: "tok"})
	}
	tr.sendSync("ses_1", EvShutdown{Reason: ShutdownExit})
	// Anything a late producer tries after the seal must not reach the wire.
	tr.send("ses_1", EvTurnToken{TurnID: "turn_1", Chunk: "too late"})
	tr.send("ses_1", EvTurnEnd{TurnID: "turn_1", Outcome: OutcomeAnswered})
	tr.Close()

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	var shutdownAt = -1
	var maxSeq, shutdownSeq float64
	for i, line := range lines {
		var f map[string]any
		if err := json.Unmarshal([]byte(line), &f); err != nil {
			t.Fatalf("line %d is not valid JSON: %v", i, err)
		}
		seq, _ := f["seq"].(float64)
		if seq > maxSeq {
			maxSeq = seq
		}
		if f["type"] == "host:shutdown" {
			shutdownAt = i
			shutdownSeq = seq
		}
	}
	if shutdownAt < 0 {
		t.Fatal("no host:shutdown frame was written")
	}
	if shutdownAt != len(lines)-1 {
		t.Errorf("host:shutdown is line %d of %d — %d frame(s) follow a terminal event",
			shutdownAt+1, len(lines), len(lines)-1-shutdownAt)
	}
	if shutdownSeq != maxSeq {
		t.Errorf("host:shutdown carries seq %v but the session reached %v; its seq must be the highest",
			shutdownSeq, maxSeq)
	}
	// The full queued tail preceded it rather than being discarded with it — all 40
	// tokens plus the shutdown frame itself, exactly, not merely "more than one".
	if len(lines) != 41 {
		t.Errorf("got %d frames, want exactly 41 (40 queued tokens + host:shutdown); "+
			"teardown dropped part of the queued tail instead of draining all of it", len(lines))
	}
}

// A required identity field that arrives blank is type-correct and says nothing. Every
// consumer downstream trims before comparing, so a whitespace-only projectId passed the
// type check and then skipped the binding cross-check as "not stated" — which is exactly
// the field that check exists to compare.
func TestBlankRequiredDescriptorFieldsAreRejected(t *testing.T) {
	for _, body := range []string{
		`{"sessionId":"","windowId":7,"projectId":"p","cwd":"/tmp","tier":"system","protocolVersion":4}`,
		`{"sessionId":"s","windowId":7,"projectId":"   ","cwd":"/tmp","tier":"system","protocolVersion":4}`,
		`{"sessionId":"s","windowId":7,"projectId":"p","cwd":"","tier":"system","protocolVersion":4}`,
		`{"sessionId":"s","windowId":7,"projectId":"p","cwd":"/tmp","tier":"\t","protocolVersion":4}`,
	} {
		if _, err := ParseDescriptor([]byte(body)); err == nil {
			t.Errorf("a blank required field was accepted: %s", body)
		}
	}
	// A fully-populated descriptor still parses.
	ok := `{"sessionId":"s","windowId":7,"projectId":"p","cwd":"/tmp","tier":"system","protocolVersion":4}`
	if _, err := ParseDescriptor([]byte(ok)); err != nil {
		t.Errorf("a valid descriptor was rejected: %v", err)
	}
}

// The cap applies to ENCODED bytes while a budget can only be expressed in raw ones, and
// the ratio between them is not knowable in advance: a megabyte of ASCII encodes to about
// a megabyte, while a megabyte of control characters encodes to six. Computing a raw
// budget from the encoded overshoot got it wrong in both directions.
func TestShrinkFitsHeavilyEscapedContent(t *testing.T) {
	// A NUL encodes to a six-byte escape, so this is well under the cap raw and far over
	// it encoded. The old arithmetic concluded it already fitted and refused the frame.
	escaped := strings.Repeat("\x00", 900_000)
	out := &syncBuffer{}
	tr := newTransport(strings.NewReader(""), out, &bytes.Buffer{})
	tr.start()
	defer tr.Close()

	tr.send("ses_1", EvTurnEnd{TurnID: "turn_1", EndedAt: 1, Outcome: OutcomeAnswered, Content: escaped, HasContent: true})

	line := awaitLine(t, out)
	if len(line)+1 > maxFrameBytes {
		t.Fatalf("the emitted frame is %d bytes, past the %d cap", len(line)+1, maxFrameBytes)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if got["type"] != "turn:end" {
		t.Fatalf("type = %v; heavily-escaped content still lost the terminal frame", got["type"])
	}
}

// A frame that exceeds the cap only by its ENVELOPE must not lose half its answer. The
// old arithmetic cut the raw budget to roughly half the cap whatever the overshoot was.
func TestNearCapContentKeepsMostOfItself(t *testing.T) {
	content := strings.Repeat("a", maxFrameBytes-200)
	ev := EvTurnEnd{TurnID: "turn_1", EndedAt: 1, Outcome: OutcomeAnswered, Content: content, HasContent: true}

	out := &syncBuffer{}
	tr := newTransport(strings.NewReader(""), out, &bytes.Buffer{})
	tr.start()
	defer tr.Close()
	tr.send("ses_1", ev)

	line := awaitLine(t, out)
	var got map[string]any
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	kept, _ := got["content"].(string)
	// It must keep the great majority — an answer trimmed to fit an envelope should lose
	// bytes, not halves.
	if len(kept) < len(content)*9/10 {
		t.Errorf("kept %d of %d bytes; a frame over the cap by its envelope lost %d%% of the answer",
			len(kept), len(content), 100-100*len(kept)/len(content))
	}
	if len(line)+1 > maxFrameBytes {
		t.Errorf("the emitted frame is %d bytes, past the cap", len(line)+1)
	}
}

// Telemetry is not worth a session. Treating every undeliverable frame as fatal meant a
// burst of optional events against a briefly slow consumer could kill a healthy run.
func TestOnlyCriticalFramesEndTheSession(t *testing.T) {
	for _, ev := range []HostEvent{
		EvTurnEnd{TurnID: "t"},
		EvApprovalRequested{ApprovalID: "a"},
		EvApprovalDecided{ApprovalID: "a"},
		EvQuestionRequested{QuestionID: "q"},
		EvQuestionAnswered{QuestionID: "q"},
		EvShutdown{Reason: ShutdownExit},
		EvError{Code: "x"},
		EvReady{},
		// command:result carries /clear's AUTHORITATIVE conversationCleared outcome —
		// losing it silently leaves the engine and renderer disagreeing about whether
		// the transcript was wiped (doc finding NH-007).
		EvCommandResult{Command: "/clear", ConversationCleared: true},
	} {
		if !criticalFrame(ev) {
			t.Errorf("%T is not treated as critical; losing it leaves the host unable to proceed", ev)
		}
	}
	for _, ev := range []HostEvent{
		EvUsage{},
		EvCost{},
		EvNotice{Level: "info", Message: "m"},
		EvTurnReasoning{TurnID: "t"},
		EvModelRateLimited{},
		EvTurnPhase{TurnID: "t"},
	} {
		if criticalFrame(ev) {
			t.Errorf("%T is treated as critical; a congested consumer would kill the session over telemetry", ev)
		}
	}
}

// awaitLine waits for the writer goroutine to emit one frame.
func awaitLine(t *testing.T, out *syncBuffer) string {
	t.Helper()
	for i := 0; i < 400; i++ {
		if s := strings.TrimSpace(out.String()); s != "" {
			return s
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no frame was written")
	return ""
}
