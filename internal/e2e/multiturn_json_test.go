package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// runMultiTurn builds the binary, feeds it the given stdin lines under
// `--json --multi-turn`, and returns the parsed JSONL stream plus the exit code.
//
// It asserts stdout purity on the way through, because that property is what makes this
// mode usable at all: a single human line — a warning, a confirmation notice, an ANSI
// escape — on the stream a harness is parsing breaks the harness, and multi-turn adds
// several new places (slash commands especially) where one could leak.
func runMultiTurn(t *testing.T, fake *fakeBackend, stdinLines ...string) ([]jsonLine, int, string) {
	t.Helper()
	return runMultiTurnRaw(t, fake, strings.Join(stdinLines, "\n")+"\n")
}

// runMultiTurnRaw is runMultiTurn with the stdin bytes given EXACTLY, so a test can
// drive the cases a []string cannot express — a final prompt with no trailing newline,
// CRLF endings, or a stream that never ends.
func runMultiTurnRaw(t *testing.T, fake *fakeBackend, stdin string, extraArgs ...string) ([]jsonLine, int, string) {
	t.Helper()
	bin := buildBinary(t)
	dir := t.TempDir()

	cmd := exec.Command(bin, append([]string{"--json", "--multi-turn"}, extraArgs...)...)
	cmd.Env = append(cmd.Environ(),
		"DAINTREE_BACKEND_URL="+fake.baseURL(),
		"DAINTREE_ASSISTANT_STATE_DIR="+dir,
		"DAINTREE_ASSISTANT_TIER=operator",
		"DAINTREE_ASSISTANT_DEBUG_LOG=0",
		"DAINTREE_MCP_URL=", // no MCP → clean degraded local mode
		"DAINTREE_MCP_TOKEN=",
	)
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	exitCode := 0
	if err := cmd.Run(); err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run binary: %v (stderr: %s)", err, stderr.String())
		}
		exitCode = ee.ExitCode()
	}

	if strings.Contains(stdout.String(), "\x1b") {
		t.Errorf("stdout contains ANSI escape sequences (impurity):\n%q", stdout.String())
	}
	var lines []jsonLine
	sc := bufio.NewScanner(&stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		text := sc.Text()
		if strings.TrimSpace(text) == "" {
			continue
		}
		var raw map[string]any
		if err := json.Unmarshal([]byte(text), &raw); err != nil {
			t.Fatalf("non-JSON line on stdout (impurity): %q\nfull stdout:\n%s", text, stdout.String())
		}
		typ, _ := raw["type"].(string)
		seqF, _ := raw["seq"].(float64)
		lines = append(lines, jsonLine{Type: typ, Seq: int(seqF), raw: raw})
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan stdout: %v", err)
	}
	if len(lines) == 0 {
		t.Fatalf("no JSONL lines on stdout; stderr:\n%s", stderr.String())
	}
	return lines, exitCode, stderr.String()
}

func linesOfType(lines []jsonLine, typ string) []jsonLine {
	var out []jsonLine
	for _, l := range lines {
		if l.Type == typ {
			out = append(out, l)
		}
	}
	return out
}

// linesOfTypeN is linesOfType with the count asserted up front, so a wrong count FAILS
// rather than panicking at the first index a line later.
func linesOfTypeN(t *testing.T, lines []jsonLine, typ string, want int) []jsonLine {
	t.Helper()
	got := linesOfType(lines, typ)
	if len(got) != want {
		t.Fatalf("%s lines = %d, want %d", typ, len(got), want)
	}
	return got
}

// assertOneTranscript pins the invariants that make the whole stream ONE transcript:
// a single session header first, a single terminal result last, and seq monotonic from
// 0 across every line in between.
func assertOneTranscript(t *testing.T, lines []jsonLine) {
	t.Helper()
	linesOfTypeN(t, lines, "session", 1)
	linesOfTypeN(t, lines, "result", 1)
	if lines[0].Type != "session" {
		t.Errorf("first line = %q, want session", lines[0].Type)
	}
	if last := lines[len(lines)-1]; last.Type != "result" {
		t.Errorf("last line = %q, want result", last.Type)
	}
	for i, l := range lines {
		if l.Seq != i {
			t.Fatalf("line %d (%s) has seq %d, want %d — seq is monotonic across the process",
				i, l.Type, l.Seq, i)
		}
	}
}

// plainRound is a scripted round that just answers, no tool call.
func plainRound(tokens ...string) sseRound {
	return sseRound{
		contentTokens: tokens,
		usage:         &fakeUsage{prompt: 20, completion: 3, total: 23},
	}
}

// TestBinaryJSONMultiTurnKeepsOneConversationAndOneTranscript is the feature's headline
// claim, end to end through the real binary: several prompts, ONE process, ONE session,
// ONE JSONL transcript — and, decisively, the second backend request carries the first
// exchange, which is precisely what a fresh process per prompt could never do.
func TestBinaryJSONMultiTurnKeepsOneConversationAndOneTranscript(t *testing.T) {
	fake := newFakeBackend(t, plainRound("Two ", "worktrees."), plainRound("The ", "first ", "is ", "clean."))
	lines, exitCode, stderr := runMultiTurn(t, fake,
		"list the worktrees",
		"now check the first one",
	)
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", exitCode, stderr)
	}
	assertOneTranscript(t, lines)

	// Two brackets, in order, each carrying its own prompt.
	prompts := linesOfType(lines, "turn:prompt")
	ends := linesOfType(lines, "turn:end")
	if len(prompts) != 2 || len(ends) != 2 {
		t.Fatalf("turn:prompt/turn:end = %d/%d, want 2/2", len(prompts), len(ends))
	}
	for i, want := range []string{"list the worktrees", "now check the first one"} {
		if got, _ := prompts[i].raw["prompt"].(string); got != want {
			t.Errorf("turn:prompt[%d].prompt = %q, want %q", i, got, want)
		}
		if got, _ := prompts[i].raw["turn"].(float64); int(got) != i {
			t.Errorf("turn:prompt[%d].turn = %v, want %d", i, prompts[i].raw["turn"], i)
		}
		if got, _ := ends[i].raw["status"].(string); got != "success" {
			t.Errorf("turn:end[%d].status = %q, want success", i, got)
		}
	}
	// Each bracket is well-formed: prompt before end, and the turn's assistant events inside.
	assertBracketsWellFormed(t, lines)

	// THE point of the feature: turn 2 was sent to the SAME conversation. The second
	// request must replay turn 1's prompt and answer, which a fresh process cannot do.
	if fake.callCount() < 2 {
		t.Fatalf("backend respond calls = %d, want at least 2 (one per turn)", fake.callCount())
	}
	second := fake.requestMessages(1)
	var roles, blob []string
	for _, m := range second {
		role, _ := m["role"].(string)
		roles = append(roles, role)
		blob = append(blob, fmt.Sprint(m["content"]))
	}
	joined := strings.Join(blob, "\n")
	for _, want := range []string{"list the worktrees", "Two worktrees.", "now check the first one"} {
		if !strings.Contains(joined, want) {
			t.Errorf("second request is missing %q — the conversation did not carry over.\nroles: %v\nmessages:\n%s",
				want, roles, joined)
		}
	}

	// One accounting block for the process, summed across BOTH turns.
	result := linesOfTypeN(t, lines, "result", 1)[0]
	stats, _ := result.raw["stats"].(map[string]any)
	if got, _ := stats["rounds"].(float64); int(got) != 2 {
		t.Errorf("stats.rounds = %v, want 2 (one per turn here)", stats["rounds"])
	}
	if got, _ := stats["totalTokens"].(float64); int(got) != 46 {
		t.Errorf("stats.totalTokens = %v, want 46 (23 per turn, accumulated)", stats["totalTokens"])
	}
	if got, _ := result.raw["content"].(string); got != "The first is clean." {
		t.Errorf("result.content = %q, want the LAST turn's answer", got)
	}
}

// assertBracketsWellFormed checks that every turn:prompt is closed by a turn:end before
// the next turn:prompt, and that assistant events only ever appear inside a bracket. A
// consumer slicing the stream by these boundaries depends on exactly that.
func assertBracketsWellFormed(t *testing.T, lines []jsonLine) {
	t.Helper()
	open := false
	for _, l := range lines {
		switch l.Type {
		case "turn:prompt":
			if open {
				t.Fatalf("turn:prompt at seq %d while a bracket was still open", l.Seq)
			}
			open = true
		case "turn:end":
			if !open {
				t.Fatalf("turn:end at seq %d with no open bracket", l.Seq)
			}
			open = false
		case "assistant:start", "assistant:end", "assistant:cancelled":
			if !open {
				t.Fatalf("%s at seq %d outside any turn bracket", l.Type, l.Seq)
			}
		}
	}
	if open {
		t.Fatal("stream ended with an unclosed turn bracket")
	}
}

// TestBinaryJSONMultiTurnRunsSlashCommandsBetweenTurns: commands work on this stdin the
// way they already do on --classic's, but they reach the stream as DATA rather than
// rendered text — and /clear resets the conversation without touching the transcript.
func TestBinaryJSONMultiTurnRunsSlashCommandsBetweenTurns(t *testing.T) {
	fake := newFakeBackend(t, plainRound("First ", "answer."), plainRound("Second ", "answer."))
	lines, exitCode, stderr := runMultiTurn(t, fake,
		"remember this",
		"/clear",
		"fresh question",
	)
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", exitCode, stderr)
	}
	assertOneTranscript(t, lines)
	assertBracketsWellFormed(t, lines)

	c := linesOfTypeN(t, lines, "command:result", 1)[0].raw
	if got, _ := c["command"].(string); got != "/clear" {
		t.Errorf("command:result.command = %q, want %q", got, "/clear")
	}
	if handled, _ := c["handled"].(bool); !handled {
		t.Errorf("command:result.handled = false, want true for /clear")
	}
	if cleared, _ := c["conversationCleared"].(bool); !cleared {
		t.Errorf("command:result.conversationCleared = false, want true for /clear")
	}

	// A command is not a turn: two prompts still means two brackets, numbered 0 and 1.
	prompts := linesOfType(lines, "turn:prompt")
	if len(prompts) != 2 {
		t.Fatalf("turn:prompt lines = %d, want 2 (a command is not a turn)", len(prompts))
	}
	if got, _ := prompts[1].raw["turn"].(float64); int(got) != 1 {
		t.Errorf("turn after a command = %v, want 1", prompts[1].raw["turn"])
	}

	// /clear cleared the CONVERSATION: the post-clear request must not replay it.
	if fake.callCount() < 2 {
		t.Fatalf("backend respond calls = %d, want at least 2", fake.callCount())
	}
	var blob []string
	for _, m := range fake.requestMessages(1) {
		blob = append(blob, fmt.Sprint(m["content"]))
	}
	joined := strings.Join(blob, "\n")
	if strings.Contains(joined, "remember this") || strings.Contains(joined, "First answer.") {
		t.Errorf("/clear did not clear the conversation; the request still carries it:\n%s", joined)
	}
	if !strings.Contains(joined, "fresh question") {
		t.Errorf("post-clear request is missing the new prompt:\n%s", joined)
	}
}

// TestBinaryJSONMultiTurnEmptyScriptFailsLoudly: an empty stdin is a harness mistake —
// an unset variable, a file that was not there — and a run that reported success for a
// conversation with nothing in it would hide it behind a transcript of nothing.
func TestBinaryJSONMultiTurnEmptyScriptFailsLoudly(t *testing.T) {
	fake := newFakeBackend(t, plainRound("never asked"))
	lines, exitCode, _ := runMultiTurn(t, fake, "", "   ")
	if exitCode != 1 {
		t.Fatalf("exit code = %d, want 1 for a script with no prompts", exitCode)
	}
	if n := fake.callCount(); n != 0 {
		t.Errorf("backend respond calls = %d, want 0 — nothing was asked", n)
	}
	result := linesOfTypeN(t, lines, "result", 1)
	if got, _ := result[0].raw["status"].(string); got != "error" {
		t.Errorf("result.status = %q, want error", got)
	}
	if n := len(linesOfType(lines, "turn:prompt")); n != 0 {
		t.Errorf("turn:prompt lines = %d, want 0", n)
	}
	// The explicit diagnosis, not merely the sink's pessimistic default. Without these
	// two assertions this test would still pass if the emptiness check were deleted
	// outright — a fresh sink already defaults to error/exit 1, and there would still be
	// no backend call and no bracket. What must be proved is that the run SAYS why.
	errLine := linesOfTypeN(t, lines, "error", 1)[0]
	msg, _ := errLine.raw["message"].(string)
	if !strings.Contains(msg, "no prompts on stdin") {
		t.Errorf("error.message = %q, want it to name the empty script", msg)
	}
	errObj, _ := result[0].raw["error"].(map[string]any)
	if errObj == nil || !strings.Contains(fmt.Sprint(errObj["message"]), "no prompts on stdin") {
		t.Errorf("result.error = %v, want the same diagnosis on the terminal line", result[0].raw["error"])
	}
}

// TestBinaryJSONMultiTurnContinuesAfterAFailedTurn is the loop's continuation contract
// at the level that can actually regress. The sink test proves the outcome ALGEBRA;
// only this proves the loop keeps reading stdin after a turn fails — a regression that
// stopped at the first failure would still satisfy every other test here.
func TestBinaryJSONMultiTurnContinuesAfterAFailedTurn(t *testing.T) {
	// The failing round is LAST, so the fake replays it for every later request. That is
	// deliberate rather than incidental: the client RETRIES a failed round, and a
	// one-round failure sandwiched between successes gets consumed by that retry — the
	// turn then succeeds on the replayed next round and the test silently asserts
	// nothing. A sticky failure fails the turn however many attempts it takes.
	fake := newFakeBackend(t,
		plainRound("Fine."),
		sseRound{errorMessage: "upstream exploded"},
	)
	lines, exitCode, stderr := runMultiTurn(t, fake, "one", "two", "three")

	if exitCode != 1 {
		t.Fatalf("exit code = %d, want 1 — a run with a failed turn is a failed run (stderr: %s)",
			exitCode, stderr)
	}
	assertOneTranscript(t, lines)
	assertBracketsWellFormed(t, lines)

	// THE assertion: all three prompts ran. A regression that stopped reading stdin at
	// the first failed turn would still satisfy every other test in this file.
	linesOfTypeN(t, lines, "turn:prompt", 3)
	// The call COUNT alone cannot prove this: turn two's own retries inflate it. Look
	// for the third prompt in a recorded request body instead.
	sawThird := false
	for i := 0; i < fake.callCount(); i++ {
		for _, m := range fake.requestMessages(i) {
			if strings.Contains(fmt.Sprint(m["content"]), "three") {
				sawThird = true
			}
		}
	}
	if !sawThird {
		t.Fatalf("no backend request carried the third prompt — the loop stopped at the failed turn")
	}
	ends := linesOfTypeN(t, lines, "turn:end", 3)
	if got, _ := ends[0].raw["status"].(string); got != "success" {
		t.Errorf("turn:end[0].status = %q, want success", got)
	}
	for _, i := range []int{1, 2} {
		if got, _ := ends[i].raw["status"].(string); got != "error" {
			t.Errorf("turn:end[%d].status = %q, want error", i, got)
		}
	}
	result := linesOfTypeN(t, lines, "result", 1)[0]
	if got, _ := result.raw["status"].(string); got != "error" {
		t.Errorf("result.status = %q, want error", got)
	}
}

// TestBinaryJSONMultiTurnRunsAFinalPromptWithNoTrailingNewline: a prompt file that does
// not end in a newline is ordinary, and the last line of one is a real prompt. Dropping
// it would run every prompt but the last — a failure that looks like a working run.
func TestBinaryJSONMultiTurnRunsAFinalPromptWithNoTrailingNewline(t *testing.T) {
	fake := newFakeBackend(t, plainRound("One."), plainRound("Two."))
	lines, exitCode, stderr := runMultiTurnRaw(t, fake, "first\nsecond, unterminated")
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", exitCode, stderr)
	}
	prompts := linesOfTypeN(t, lines, "turn:prompt", 2)
	if got, _ := prompts[1].raw["prompt"].(string); got != "second, unterminated" {
		t.Errorf("turn:prompt[1].prompt = %q, want the unterminated final line", got)
	}
	if n := fake.callCount(); n < 2 {
		t.Fatalf("backend respond calls = %d, want 2 — the final prompt never ran", n)
	}
}

// TestBinaryJSONMultiTurnHandlesCRLFAndUnknownCommands covers two script-authoring
// realities at once: a prompt file written on Windows, and a typo.
func TestBinaryJSONMultiTurnHandlesCRLFAndUnknownCommands(t *testing.T) {
	fake := newFakeBackend(t, plainRound("Answered."))
	lines, exitCode, stderr := runMultiTurnRaw(t, fake, "/claer\r\nask something\r\n")
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0 — an unknown command is not fatal (stderr: %s)", exitCode, stderr)
	}
	assertOneTranscript(t, lines)

	// CRLF: the \r is trimmed, so the prompt reaches the backend clean.
	prompts := linesOfTypeN(t, lines, "turn:prompt", 1)
	if got, _ := prompts[0].raw["prompt"].(string); got != "ask something" {
		t.Errorf("turn:prompt.prompt = %q, want the CRLF stripped", got)
	}

	// The typo is machine-detectable. This is the field's entire reason to exist: the
	// handler still "handles" an unknown command (the attached session gets a card), so only a
	// registry lookup can tell a script that /claer is not a command at all.
	c := linesOfTypeN(t, lines, "command:result", 1)[0].raw
	if got, _ := c["command"].(string); got != "/claer" {
		t.Errorf("command:result.command = %q, want %q", got, "/claer")
	}
	if handled, _ := c["handled"].(bool); handled {
		t.Errorf("command:result.handled = true for /claer, want false — a typo must be detectable")
	}
}

// TestBinaryJSONMultiTurnQuitStopsTheScript: /quit ends input consumption, so the
// prompts after it must never reach the backend.
func TestBinaryJSONMultiTurnQuitStopsTheScript(t *testing.T) {
	fake := newFakeBackend(t, plainRound("Only ", "answer."))
	lines, exitCode, stderr := runMultiTurn(t, fake, "ask this", "/quit", "never asked")
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", exitCode, stderr)
	}
	assertOneTranscript(t, lines)
	linesOfTypeN(t, lines, "turn:prompt", 1)
	if n := fake.callCount(); n != 1 {
		t.Errorf("backend respond calls = %d, want 1 — /quit must stop the script", n)
	}
	c := linesOfTypeN(t, lines, "command:result", 1)[0].raw
	if quit, _ := c["quit"].(bool); !quit {
		t.Errorf("command:result.quit = false for /quit, want true")
	}
}

// TestBinaryJSONMultiTurnTimeoutBeforeAnyPromptReportsCancelled is the regression for
// the sharpest edge of the bound: it expires while the loop is still waiting for the
// FIRST line. Nothing failed and nothing ran, so the run is cancelled (exit 2) — not the
// error/exit-1 that a pessimistic default sentinel would otherwise report, and not the
// "no prompts on stdin" complaint about a script that may well have had some.
func TestBinaryJSONMultiTurnTimeoutBeforeAnyPromptReportsCancelled(t *testing.T) {
	fake := newFakeBackend(t, plainRound("never asked"))
	// A pipe that is never written to and never closed: stdin exists but no line arrives.
	//
	// os.Pipe, NOT io.Pipe. exec hands a *os.File straight to the child as a file
	// descriptor, but wraps any other reader in a copying goroutine that Wait blocks on —
	// and that goroutine would never finish here, so Run would hang forever after the
	// child had already exited, turning a 3-second test into a suite timeout.
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	t.Cleanup(func() { _ = pw.Close(); _ = pr.Close() })

	bin := buildBinary(t)
	// A watchdog well beyond the product's own 3s bound. If --timeout ever stops
	// working, the child holds an stdin that never closes and Run would block until the
	// whole package times out — the test hanging on precisely the regression it exists
	// to catch. CommandContext kills it instead, so the failure is reported as one.
	watchdog, cancelWatchdog := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancelWatchdog()
	cmd := exec.CommandContext(watchdog, bin, "--json", "--multi-turn", "--timeout", "3s")
	cmd.Env = append(cmd.Environ(),
		"DAINTREE_BACKEND_URL="+fake.baseURL(),
		"DAINTREE_ASSISTANT_STATE_DIR="+t.TempDir(),
		"DAINTREE_ASSISTANT_TIER=operator",
		"DAINTREE_ASSISTANT_DEBUG_LOG=0",
		"DAINTREE_MCP_URL=",
		"DAINTREE_MCP_TOKEN=",
	)
	cmd.Stdin = pr
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	exitCode := 0
	if err := cmd.Run(); err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run binary: %v (stderr: %s)", err, stderr.String())
		}
		exitCode = ee.ExitCode()
	}
	if exitCode != 2 {
		t.Fatalf("exit code = %d, want 2 (cancelled) — nothing failed, the bound expired.\nstdout:\n%s\nstderr:\n%s",
			exitCode, stdout.String(), stderr.String())
	}

	var result map[string]any
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var raw map[string]any
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			t.Fatalf("non-JSON line on stdout: %q", line)
		}
		if raw["type"] == "result" {
			result = raw
		}
		// An assistant event outside a bracket would break the transcript contract, and
		// a setup/idle timeout is exactly where one used to be invented.
		if raw["type"] == "assistant:cancelled" || raw["type"] == "assistant:start" {
			t.Errorf("%v emitted with no turn ever opened:\n%s", raw["type"], stdout.String())
		}
	}
	if result == nil {
		t.Fatalf("no result line on stdout:\n%s", stdout.String())
	}
	if got, _ := result["status"].(string); got != "cancelled" {
		t.Errorf("result.status = %q, want cancelled", got)
	}
}

// TestBinaryJSONSinglePromptEmitsNoTurnLines is the backward-compatibility pin at the
// binary level, and the reason the new lines are opt-in: an ordinary `--json "prompt"`
// run must be exactly what it was, so an existing consumer never meets a line type it
// has not been taught. TestBinaryJSONOneShot covers the rest of that stream; this one
// covers only the part this change could have broken.
func TestBinaryJSONSinglePromptEmitsNoTurnLines(t *testing.T) {
	bin := buildBinary(t)
	fake := newFakeBackend(t, plainRound("An ", "answer."))
	dir := t.TempDir()

	cmd := exec.Command(bin, "--json", "a question")
	cmd.Env = append(cmd.Environ(),
		"DAINTREE_BACKEND_URL="+fake.baseURL(),
		"DAINTREE_ASSISTANT_STATE_DIR="+dir,
		"DAINTREE_ASSISTANT_TIER=operator",
		"DAINTREE_ASSISTANT_DEBUG_LOG=0",
		"DAINTREE_MCP_URL=",
		"DAINTREE_MCP_TOKEN=",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("run binary: %v (stderr: %s)", err, stderr.String())
	}

	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var raw map[string]any
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			t.Fatalf("non-JSON line on stdout: %q", line)
		}
		switch raw["type"] {
		case "turn:prompt", "turn:end", "command:result":
			t.Errorf("multi-turn line %v leaked into an ordinary --json run:\n%s", raw["type"], stdout.String())
		}
	}
}

// TestBinaryJSONMultiTurnRejectsAPromptArgument: --multi-turn is a third prompt source,
// so naming another alongside it is a usage error (exit 2, stderr) rather than a silent
// precedence rule that would run a prompt the caller can see they also passed the other
// way. It must fail at the argument boundary, before any run begins.
func TestBinaryJSONMultiTurnRejectsAPromptArgument(t *testing.T) {
	bin := buildBinary(t)
	for _, args := range [][]string{
		{"--json", "--multi-turn", "a prompt"},
		{"--multi-turn"}, // without --json
	} {
		cmd := exec.Command(bin, args...)
		cmd.Env = append(cmd.Environ(), "DAINTREE_ASSISTANT_STATE_DIR="+t.TempDir())
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("%v: expected a usage failure, got %v", args, err)
		}
		if ee.ExitCode() != 2 {
			t.Errorf("%v: exit code = %d, want 2 (usage error)", args, ee.ExitCode())
		}
		if stdout.Len() != 0 {
			t.Errorf("%v: usage error wrote to stdout, which must stay pure: %q", args, stdout.String())
		}
		if !strings.Contains(stderr.String(), "multi-turn") {
			t.Errorf("%v: stderr does not name the flag: %q", args, stderr.String())
		}
	}
}
