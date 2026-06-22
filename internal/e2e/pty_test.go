//go:build pty

package e2e

import (
	"bytes"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
)

// pty_test.go is the REAL render harness: it launches the compiled binary inside
// a pseudoterminal, drives a streamed multi-paragraph turn (with a tool batch) and
// a resize through actual keystrokes + SIGWINCH, and parses the emitted bytes with
// the minimal VT interpreter (vtscreen_test.go). It asserts three invariants the
// headless View()-string harness (internal/ui/render_harness_test.go) structurally
// cannot, because they only emerge once tea.Println, the host's scrollback, and a
// live terminal's cursor geometry are actually in play (the charmbracelet/
// bubbletea#1613 failure class):
//
//  1. The live footer never swallows the streamed response (peak live-View height
//     stays bounded — completed prose flushes to scrollback as it streams).
//  2. No committed paragraph is duplicated in scrollback (the #1613 frozen-partial
//     signature — a marker would land on two composed lines).
//  3. A settled resize re-commits the masthead exactly once at the new width.
//
// It is gated behind `//go:build pty` (run via `make test-pty`), skipped under
// -short, and skips under -race via buildBinary (a separate non-instrumented
// process adds no race coverage). It never touches the network — the binary talks
// to an in-process fake Fireworks SSE server, the same seam binary_test.go uses.

const (
	// composerGlyph is the prompt glyph the live composer renders; its presence in
	// the parsed screen marks the cockpit reaching steady state. Mirrors
	// internal/ui composerPromptGlyph.
	composerGlyph = "›"
	// maxFooterRows bounds the live View height during a turn. Matches the headless
	// flush_test.go convention (internal/ui maxLiveRows=8 + a 10-row bottom band);
	// maxLiveRows is unexported, so the literal is duplicated with this note.
	maxFooterRows = 18
	// mastheadText is the distinctive masthead substring committed once per scrollback
	// commit. Mirrors internal/ui render_chrome.go renderMasthead.
	mastheadText = "Daintree Assistant"
)

func TestPTYCockpitRenderHarness(t *testing.T) {
	if testing.Short() {
		t.Skip("PTY render harness allocates a real pseudoterminal and drives a full turn; skipped under -short")
	}
	bin := buildBinary(t) // auto-skips under -race

	const (
		markerA  = "PTYMARKERALPHA"
		markerB  = "PTYMARKERBRAVO"
		sentinel = "PTYDONESENTINEL"
	)

	// Two scripted rounds: round 1 streams a completed paragraph then a tool call;
	// round 2 streams a second paragraph and a final paragraph carrying the sentinel.
	// Each marker sits in its own \n\n-terminated paragraph so flush.go commits it.
	fake := newFakeFireworks(t,
		sseRound{
			contentTokens: []string{"Paragraph one " + markerA + " is settled.\n\n"},
			toolName:      "memory__list",
			toolArgs:      `{"limit":3}`,
			usage:         &fakeUsage{prompt: 40, completion: 8, total: 48},
		},
		sseRound{
			contentTokens: []string{
				"Paragraph two " + markerB + " also settled.\n\n",
				"Wrapped up " + sentinel + " now.\n\n",
			},
			usage: &fakeUsage{prompt: 60, completion: 12, total: 72, cached: 20},
		},
	)

	const startRows, startCols = 40, 100
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(),
		"FIREWORKS_BASE_URL="+fake.baseURL(),
		"FIREWORKS_API_KEY=test-key",
		"DAINTREE_ASSISTANT_STATE_DIR="+t.TempDir(),
		"DAINTREE_ASSISTANT_TIER=operator",
		"DAINTREE_ASSISTANT_DEBUG_LOG=0",
		"DAINTREE_ASSISTANT_NO_SPLASH=1", // skip the raw-ANSI boot splash for stable timing
		"DAINTREE_MCP_URL=",              // no MCP → clean degraded local mode
		"DAINTREE_MCP_TOKEN=",
		"TERM=xterm-256color",
	)

	ptm, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: startRows, Cols: startCols})
	if err != nil {
		t.Fatalf("start binary under pty: %v", err)
	}

	screen := newVTScreen(startRows, startCols)
	var rawMu sync.Mutex
	var raw bytes.Buffer

	// Drain goroutine: the PTY master never returns EOF until the slave closes, so it
	// blocks here until ptm.Close() after cmd.Wait(). It feeds both the VT screen and
	// a raw byte accumulator (raw byte counts are the robust signal for masthead
	// re-commits, which never re-enter the live footer).
	var drainWG sync.WaitGroup
	drainWG.Add(1)
	go func() {
		defer drainWG.Done()
		buf := make([]byte, 32*1024)
		for {
			n, rerr := ptm.Read(buf)
			if n > 0 {
				chunk := append([]byte(nil), buf[:n]...)
				screen.Feed(chunk)
				rawMu.Lock()
				raw.Write(chunk)
				rawMu.Unlock()
			}
			if rerr != nil {
				return
			}
		}
	}()

	rawCount := func(sub string) int {
		rawMu.Lock()
		defer rawMu.Unlock()
		return bytes.Count(raw.Bytes(), []byte(sub))
	}
	rawLen := func() int {
		rawMu.Lock()
		defer rawMu.Unlock()
		return raw.Len()
	}

	// --- Phase 1: boot → steady state ---
	// Boot settles in two beats: the live footer (composer glyph) renders first, then
	// one render cycle later CommitArm fires and the masthead commits to scrollback. Wait
	// for BOTH, then let the burst settle, so the masthead-commit count is read clean.
	if !waitFor(20*time.Second, func() bool {
		return rawCount(mastheadText) >= 1 && strings.Contains(screen.Plain(), composerGlyph)
	}) {
		t.Fatalf("cockpit never reached steady state (masthead commits=%d, composer glyph present=%v):\n%s",
			rawCount(mastheadText), strings.Contains(screen.Plain(), composerGlyph), screen.Plain())
	}
	waitQuiet(rawLen, 300*time.Millisecond, 4*time.Second)
	preMasthead := rawCount(mastheadText)
	if preMasthead != 1 {
		t.Fatalf("masthead committed %d times at boot, want exactly 1:\n%s", preMasthead, screen.Plain())
	}

	// --- Phase 2: drive a streamed multi-paragraph turn (+ tool batch) ---
	screen.ResetPeakCursorUp() // measure footer height for the turn only, not the taller pre-commit boot footer
	if _, err := ptm.Write([]byte("summarize my memory\r")); err != nil {
		t.Fatalf("write prompt to pty: %v", err)
	}
	if !waitFor(30*time.Second, func() bool { return strings.Contains(screen.Plain(), sentinel) }) {
		t.Fatalf("turn never completed (sentinel %q not seen):\n%s", sentinel, screen.Plain())
	}
	// Let the seal's final commit + footer repaint settle.
	waitQuiet(rawLen, 300*time.Millisecond, 4*time.Second)

	// Invariant 1: the live footer stayed bounded — completed prose flushed to
	// scrollback as it streamed rather than piling into the footer. A non-trivial
	// peak (> 0) also proves the measurement is live, not vacuous.
	peak := screen.PeakCursorUp()
	t.Logf("turn peak live-View height: %d rows (bound %d)", peak, maxFooterRows)
	if peak <= 0 {
		t.Errorf("never observed a footer repaint during the turn (peak cursor-up = %d) — the height invariant is vacuous", peak)
	}
	if peak > maxFooterRows {
		t.Errorf("live View peaked at %d rows during the turn, want <= %d — the footer swallowed the streamed response (bubbletea#1613 class)", peak, maxFooterRows)
	}
	// Invariant 2: each committed paragraph appears on exactly one composed line
	// (no frozen partial duplicate in scrollback).
	for _, m := range []string{markerA, markerB} {
		if got := screen.CountLineSubstr(m); got != 1 {
			t.Errorf("marker %q appears on %d composed lines, want 1 (duplicated prose in scrollback):\n%s", m, got, screen.Plain())
		}
	}

	// --- Phase 3: settled resize → exactly one masthead re-commit at the new width ---
	if err := pty.Setsize(ptm, &pty.Winsize{Rows: 24, Cols: 72}); err != nil {
		t.Fatalf("resize pty: %v", err)
	}
	if !waitFor(15*time.Second, func() bool { return rawCount(mastheadText) >= preMasthead+1 }) {
		t.Fatalf("resize did not re-commit the masthead (%d occurrences, want >= %d)", rawCount(mastheadText), preMasthead+1)
	}
	// Guard window: ensure no SECOND re-commit follows (the debounce coalesces a
	// resize into exactly one nuclear redraw).
	waitQuiet(rawLen, 400*time.Millisecond, 3*time.Second)
	if got := rawCount(mastheadText); got != preMasthead+1 {
		t.Errorf("resize re-committed the masthead %d time(s) (total %d occurrences), want exactly 1 (total %d)", got-preMasthead, got, preMasthead+1)
	}

	// --- Phase 4: clean shutdown via /quit ---
	if _, err := ptm.Write([]byte("/quit\r")); err != nil {
		t.Logf("write /quit: %v", err)
	}
	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()
	select {
	case <-waitErr:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		<-waitErr
		t.Errorf("cockpit did not exit within 10s after /quit")
	}
	_ = ptm.Close() // unblocks the drain goroutine
	drainWG.Wait()
	runtime.KeepAlive(ptm)
}

// waitFor polls cond every 50ms until it is true or the timeout elapses. Returns
// the final value of cond so the caller can fail with context on a miss.
func waitFor(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return cond()
}

// waitQuiet blocks until the value from size stops changing for `quiet`, or maxWait
// elapses — used to let a render burst settle before asserting on a stable screen.
func waitQuiet(size func() int, quiet, maxWait time.Duration) {
	deadline := time.Now().Add(maxWait)
	last := size()
	stableSince := time.Now()
	for time.Now().Before(deadline) {
		time.Sleep(40 * time.Millisecond)
		if now := size(); now != last {
			last = now
			stableSince = time.Now()
			continue
		}
		if time.Since(stableSince) >= quiet {
			return
		}
	}
}
