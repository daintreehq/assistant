//go:build pty

package e2e

import (
	"bytes"
	"fmt"
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
// to an in-process fake Daintree backend SSE server, the same seam binary_test.go uses.

const (
	// composerGlyph is the prompt glyph the live composer renders; its presence in
	// the parsed screen marks the cockpit reaching steady state. Mirrors
	// internal/ui composerPromptGlyph.
	composerGlyph = "›"
	// maxFooterRows bounds the live View height for PROSE turns, which commit line by line so the
	// footer stays small (observed peaks 8-10). Kept TIGHT so it remains a real #1613 regression
	// signal — a line-committed paragraph must never balloon the footer.
	maxFooterRows = 18
	// maxListFooterRows bounds the live View height for a WITHHELD bullet list, which renders whole
	// in the footer (it can't line-commit) up to the raised cap (internal/ui maxLiveRows=16) plus the
	// ~10-row bottom band. maxLiveRows is unexported, so the literal is duplicated with this note.
	maxListFooterRows = 26
	// mastheadText is the distinctive masthead substring committed once per scrollback
	// commit. Mirrors internal/ui render_chrome.go renderMasthead.
	mastheadText = "Daintree Assistant"
	// noDaemonEnv is load-bearing isolation for every PTY cockpit launch, not hygiene.
	//
	// A normal interactive launch takes the project owner lease via
	// supervisor.AcquireOwnership with SpawnDaemon=true, so when no supervisor is
	// running it spawns `<self> daemon` DETACHED (Setsid, own session) against the
	// same DAINTREE_ASSISTANT_STATE_DIR — here, the test's t.TempDir(). That daemon
	// deliberately OUTLIVES the cockpit: the instant /quit releases the owner flock it
	// re-acquires it, reopens state.db (recreating state.db-wal / state.db-shm) and
	// starts ticking the 3s scheduler + 1s coordinator. By then the test body has
	// returned, so Go's t.TempDir() cleanup is calling RemoveAll on a directory a live
	// process is still creating files in — which surfaces as a nondeterministic
	// "TempDir RemoveAll cleanup: … directory not empty" failure on a DIFFERENT test
	// each run, always AFTER that test's own assertions passed. (It also leaks one
	// 15-minute-idle daemon per test onto the machine.)
	//
	// These tests exercise cockpit RENDERING, not supervision handover, so they take
	// the documented kill switch (internal/cli.NoDaemonEnv) and run solo on the flock.
	noDaemonEnv = "DAINTREE_ASSISTANT_NO_DAEMON=1"
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

	// Two scripted rounds streaming MANY short \n\n-terminated paragraphs (round 1 ends
	// in a tool call; round 2 ends with the sentinel). Each paragraph is a separate SSE
	// token, so flush.go commits them one-by-one. The count matters: if the incremental
	// flush ever broke, every completed paragraph would pile into the live footer and the
	// peak-height invariant would blow past its bound — with only one or two paragraphs the
	// footer never gets tall enough to prove the flush is working. Markers sit in their own
	// paragraphs; the total exceeds the screen height so the VT scrollback path is exercised.
	const nFill = 4
	round1 := []string{"Intro " + markerA + " is settled.\n\n"}
	for i := 0; i < nFill; i++ {
		round1 = append(round1, fmt.Sprintf("Filler %d of the first batch streamed and settled.\n\n", i))
	}
	round2 := []string{"Middle " + markerB + " is settled.\n\n"}
	for i := 0; i < nFill; i++ {
		round2 = append(round2, fmt.Sprintf("Filler %d of the second batch streamed and settled.\n\n", i))
	}
	round2 = append(round2, "Wrapped up "+sentinel+" now.\n\n")
	fake := newFakeBackend(t,
		sseRound{
			contentTokens: round1,
			toolName:      "memory__list",
			toolArgs:      `{"limit":3}`,
			usage:         &fakeUsage{prompt: 40, completion: 8, total: 48},
		},
		sseRound{
			contentTokens: round2,
			usage:         &fakeUsage{prompt: 60, completion: 12, total: 72, cached: 20},
		},
	)

	const startRows, startCols = 40, 100
	cmd := exec.Command(bin)
	// Drop DAINTREE_ASCII from the inherited env: its mere PRESENCE (any value, even
	// empty) forces the ASCII glyph set (theme/glyphs.go unicodeOK), which would swap the
	// composer prompt glyph we wait on from "›" to ">" and hang Phase 1 with no real
	// regression. Then force a UTF-8 locale (LC_ALL wins POSIX precedence) so an inherited
	// LANG=C / non-UTF locale can't trip the same fallback.
	env := make([]string, 0, len(os.Environ())+12)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "DAINTREE_ASCII=") {
			continue
		}
		env = append(env, kv)
	}
	cmd.Env = append(env,
		"LC_ALL=C.UTF-8",
		"DAINTREE_BACKEND_URL="+fake.baseURL(),
		"DAINTREE_API_KEY=test-key",
		"DAINTREE_ASSISTANT_STATE_DIR="+t.TempDir(),
		"DAINTREE_ASSISTANT_TIER=operator",
		"DAINTREE_ASSISTANT_DEBUG_LOG=0",
		"DAINTREE_ASSISTANT_NO_SPLASH=1", // skip the raw-ANSI boot splash for stable timing
		noDaemonEnv,
		"DAINTREE_MCP_URL=", // no MCP → clean degraded local mode
		"DAINTREE_MCP_TOKEN=",
		"TERM=xterm-256color",
	)

	ptm, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: startRows, Cols: startCols})
	if err != nil {
		t.Fatalf("start binary under pty: %v", err)
	}
	// Guarantee teardown on every exit path (incl. an early t.Fatal): Kill is idempotent
	// on an exited process and Close is safe to repeat, so this never double-frees.
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = ptm.Close()
	})

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
	rawFrom := func(off int) []byte {
		rawMu.Lock()
		defer rawMu.Unlock()
		return append([]byte(nil), raw.Bytes()[off:]...)
	}

	// --- Phase 1: boot → steady state ---
	// Boot settles in two beats: the live footer (composer glyph) renders first, then
	// one render cycle later CommitArm fires and the masthead commits to scrollback. Wait
	// for BOTH, then let the burst settle, so the masthead-commit count is read clean.
	if !waitForPTY(20*time.Second, func() bool {
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

	// --- Phase 1b: idle resize must repaint an unchanged live footer ---
	// Keep a draft in model state, resize without sending any later input, and prove the
	// host-clear recovery physically writes that same draft again. This is the exact
	// Daintree regression: the resize wipe removed the composer, then the next keypress
	// changed View.Content and made the still-present draft suddenly reappear.
	const resizeDraft = "PTY-RESIZE-DRAFT"
	if _, err := ptm.Write([]byte(resizeDraft)); err != nil {
		t.Fatalf("type pre-resize draft: %v", err)
	}
	if !waitForPTY(5*time.Second, func() bool {
		return strings.Contains(screen.VisiblePlain(), resizeDraft)
	}) {
		t.Fatalf("pre-resize draft never appeared:\n%s", screen.Plain())
	}

	const idleRows, idleCols = 36, 90
	idleResizeStart := rawLen()
	if err := pty.Setsize(ptm, &pty.Winsize{Rows: idleRows, Cols: idleCols}); err != nil {
		t.Fatalf("idle resize pty: %v", err)
	}
	screen.Resize(idleRows, idleCols)
	if !waitForPTY(15*time.Second, func() bool { return rawCount(mastheadText) >= preMasthead+1 }) {
		t.Fatalf("idle resize did not re-commit the masthead (%d occurrences, want >= %d)", rawCount(mastheadText), preMasthead+1)
	}
	waitQuiet(rawLen, 300*time.Millisecond, 3*time.Second)
	idleResizeBytes := rawFrom(idleResizeStart)
	hostClear := []byte("\x1b[2J\x1b[3J\x1b[H")
	idleLastClear := bytes.LastIndex(idleResizeBytes, hostClear)
	if idleLastClear < 0 {
		t.Fatal("idle resize never emitted the expected host viewport+scrollback clear")
	}
	idleAfterClear := idleResizeBytes[idleLastClear+len(hostClear):]
	if !bytes.Contains(idleAfterClear, []byte(resizeDraft)) {
		t.Error("idle resize emitted no draft repaint bytes after the host clear")
	}
	if visible := screen.VisiblePlain(); !strings.Contains(visible, composerGlyph) || !strings.Contains(visible, resizeDraft) {
		t.Errorf("idle resize lost the live composer/draft until keypress:\n%s", screen.Plain())
	}
	if got := rawCount(mastheadText); got != preMasthead+1 {
		t.Fatalf("idle resize re-committed masthead %d times, want exactly one", got-preMasthead)
	}
	preMasthead++

	// Remove the probe without submitting it so the scripted turn below remains unchanged.
	if _, err := ptm.Write(bytes.Repeat([]byte{0x7f}, len(resizeDraft))); err != nil {
		t.Fatalf("erase pre-resize draft: %v", err)
	}
	if !waitForPTY(5*time.Second, func() bool {
		return !strings.Contains(screen.VisiblePlain(), resizeDraft)
	}) {
		t.Fatalf("pre-resize draft did not clear:\n%s", screen.Plain())
	}

	// --- Phase 2: drive a streamed multi-paragraph turn (+ tool batch) ---
	screen.ResetPeakFrameHeight() // measure footer height for the turn only, not the boot footer
	if _, err := ptm.Write([]byte("summarize my memory\r")); err != nil {
		t.Fatalf("write prompt to pty: %v", err)
	}
	if !waitForPTY(30*time.Second, func() bool { return strings.Contains(screen.Plain(), sentinel) }) {
		t.Fatalf("turn never completed (sentinel %q not seen):\n%s", sentinel, screen.Plain())
	}
	// Let the seal's final commit + footer repaint settle.
	waitQuiet(rawLen, 300*time.Millisecond, 4*time.Second)

	// Invariant 1: the live footer stayed bounded — completed prose flushed to
	// scrollback as it streamed rather than piling into the footer. A non-trivial
	// peak (> 0) also proves the measurement is live (a commit was observed), not vacuous.
	peak := screen.PeakFrameHeight()
	t.Logf("turn peak live-View (footer) height: %d rows (bound %d)", peak, maxFooterRows)
	if peak <= 0 {
		t.Errorf("never observed a scrollback commit during the turn (peak frame height = %d) — the height invariant is vacuous", peak)
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
	const newRows, newCols = 24, 72
	resizeRawStart := rawLen()
	if err := pty.Setsize(ptm, &pty.Winsize{Rows: newRows, Cols: newCols}); err != nil {
		t.Fatalf("resize pty: %v", err)
	}
	screen.Resize(newRows, newCols) // keep the VT model's geometry in step with the terminal
	if !waitForPTY(15*time.Second, func() bool { return rawCount(mastheadText) >= preMasthead+1 }) {
		t.Fatalf("resize did not re-commit the masthead (%d occurrences, want >= %d)", rawCount(mastheadText), preMasthead+1)
	}
	// Guard window: ensure no SECOND re-commit follows (the debounce coalesces a
	// resize into exactly one nuclear redraw).
	waitQuiet(rawLen, 400*time.Millisecond, 3*time.Second)
	if got := rawCount(mastheadText); got != preMasthead+1 {
		t.Errorf("resize re-committed the masthead %d time(s) (total %d occurrences), want exactly 1 (total %d)", got-preMasthead, got, preMasthead+1)
	}
	// The resize's host wipe also erases the physical live footer. It must be repainted
	// without relying on a later keypress to change View.Content — the production symptom
	// was an absent composer that reappeared only when the user started typing.
	if !strings.Contains(screen.VisiblePlain(), composerGlyph) {
		t.Errorf("resize left the live composer blank until keypress:\n%s", screen.Plain())
	}
	resizeBytes := rawFrom(resizeRawStart)
	lastClear := bytes.LastIndex(resizeBytes, hostClear)
	if lastClear < 0 {
		t.Fatal("resize never emitted the expected host viewport+scrollback clear")
	}
	afterClear := resizeBytes[lastClear+len(hostClear):]
	if !bytes.Contains(afterClear, []byte(composerGlyph)) {
		t.Error("resize emitted no composer repaint bytes after the host clear")
	}

	// --- Phase 4: clean shutdown via /quit ---
	if _, err := ptm.Write([]byte("/quit\r")); err != nil {
		t.Logf("write /quit: %v", err)
	}
	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()
	select {
	case err := <-waitErr:
		// /quit → onShutdown → tea.Quit → exit 0; surface any unexpected non-zero exit.
		if err != nil {
			t.Logf("cockpit exited with error after /quit: %v", err)
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		<-waitErr
		t.Errorf("cockpit did not exit within 10s after /quit")
	}
	_ = ptm.Close() // unblocks the drain goroutine
	drainWG.Wait()
	runtime.KeepAlive(ptm)
}

// TestPTYSplashConnectKeepsFooter is the real-terminal regression matrix for the
// startup failure where MCP completion followed the raw splash and the
// composer vanished until the next keypress. It spans both sides of the ~740ms
// splash boundary, the reported embedded-pane dimensions, a host-style redundant
// winsize reassertion, and delayed completion. Healthy MCP startup must stay silent.
func TestPTYSplashConnectKeepsFooter(t *testing.T) {
	t.Run("immediate_embedded_pane", func(t *testing.T) {
		runSplashConnectKeepsFooter(t, 0, 29, 94, true, true)
	})
	t.Run("finishes_under_splash", func(t *testing.T) {
		runSplashConnectKeepsFooter(t, 450*time.Millisecond, 40, 100, false, false)
	})
	t.Run("just_after_splash", func(t *testing.T) {
		runSplashConnectKeepsFooter(t, 900*time.Millisecond, 29, 94, false, false)
	})
	t.Run("delayed", func(t *testing.T) {
		runSplashConnectKeepsFooter(t, 2*time.Second, 40, 100, false, false)
	})
	// Mirrors the recording: Daintree's 3s geometry sweep lands while MCP discovery
	// is still pending, then the status changes after Bubble Tea starts.
	t.Run("delayed_embedded_pane", func(t *testing.T) {
		runSplashConnectKeepsFooter(t, 4*time.Second, 29, 94, true, true)
	})
}

func runSplashConnectKeepsFooter(
	t *testing.T,
	connectDelay time.Duration,
	rows, cols uint16,
	embedded bool,
	reassertWinsize bool,
) {
	if testing.Short() {
		t.Skip("splash PTY regression allocates a real pseudoterminal and delayed MCP server")
	}
	bin := buildBinary(t)
	backend := newFakeBackend(t)
	mcpServer := newFakeMCPWithAgentDelay(t, connectDelay)

	cmd := exec.Command(bin)
	env := make([]string, 0, len(os.Environ())+14)
	for _, kv := range os.Environ() {
		// Presence alone changes these startup paths, so remove inherited values rather
		// than relying on a duplicate empty assignment later in the environment.
		if strings.HasPrefix(kv, "DAINTREE_ASCII=") ||
			strings.HasPrefix(kv, "DAINTREE_ASSISTANT_NO_SPLASH=") {
			continue
		}
		env = append(env, kv)
	}
	stateDir := t.TempDir()
	cmd.Env = append(env,
		"LC_ALL=C.UTF-8",
		"TERM=xterm-256color",
		"DAINTREE_BACKEND_URL="+backend.baseURL(),
		"DAINTREE_API_KEY=test-key",
		"DAINTREE_ASSISTANT_STATE_DIR="+stateDir,
		"DAINTREE_ASSISTANT_LOG_DIR="+stateDir+"/logs",
		"DAINTREE_ASSISTANT_DEBUG_LOG=1", // matches the reported logging-badge geometry
		noDaemonEnv,                      // isolate this process's single boot attempt
		"DAINTREE_ASSISTANT_TIER=system",
		"DAINTREE_MCP_URL="+mcpServer.url(),
		"DAINTREE_MCP_TOKEN=fake-token",
	)
	if embedded {
		cmd.Env = append(cmd.Env, "DAINTREE_WINDOW_ID=test-window")
	}

	ptm, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: rows, Cols: cols})
	if err != nil {
		t.Fatalf("start binary under pty: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = ptm.Close()
	})

	screen := newVTScreen(int(rows), int(cols))
	var rawMu sync.Mutex
	var raw bytes.Buffer
	var drainWG sync.WaitGroup
	drainWG.Add(1)
	go func() {
		defer drainWG.Done()
		buf := make([]byte, 32*1024)
		for {
			n, readErr := ptm.Read(buf)
			if n > 0 {
				chunk := append([]byte(nil), buf[:n]...)
				screen.Feed(chunk)
				rawMu.Lock()
				raw.Write(chunk)
				rawMu.Unlock()
			}
			if readErr != nil {
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
	rawSnapshot := func() []byte {
		rawMu.Lock()
		defer rawMu.Unlock()
		return append([]byte(nil), raw.Bytes()...)
	}

	// The atomic hand-off must expose the COMPLETE cockpit before any user input. An
	// embedded launch gets the same short logo budget as every other terminal; it must
	// never wait for Daintree's old three-second geometry sweep.
	composerTimeout := 10 * time.Second
	if embedded {
		composerTimeout = 2 * time.Second
	}
	if !waitForPTY(composerTimeout, func() bool {
		visible := screen.VisiblePlain()
		return strings.Contains(visible, composerGlyph) && strings.Contains(visible, mastheadText)
	}) {
		t.Fatalf("complete post-logo cockpit did not become visible before user input:\n%s", screen.Plain())
	}
	// Byte ordering is the durable regression check for the video flash. Before this
	// fix, Bubble Tea emitted the composer about 120ms before tea.Println emitted the
	// masthead, so the first composer occurrence preceded the first masthead occurrence.
	// The synchronized hand-off writes the masthead first and the composer beneath it in
	// the same output frame.
	startup := rawSnapshot()
	firstHeader := bytes.Index(startup, []byte(mastheadText))
	firstComposer := bytes.Index(startup, []byte(composerGlyph))
	if firstHeader < 0 || firstComposer < 0 || firstHeader > firstComposer {
		t.Fatalf("first post-logo frame was not the complete cockpit (header byte=%d composer byte=%d)", firstHeader, firstComposer)
	}
	// Forty animation frames each clear+home the viewport. Requiring a substantial
	// number proves this test did not accidentally take the no-splash path.
	if got := rawCount("\x1b[2J\x1b[H"); got < 20 {
		t.Fatalf("splash logo was not rendered (viewport-reset frames=%d, want >=20)", got)
	}

	// On the deliberately slow embedded case, type before MCP discovery completes.
	// This is the product contract: the logo may cover the first ~740ms, but a slow
	// connection must never keep the user's draft behind it.
	draft := ""
	if embedded && connectDelay >= 3*time.Second {
		if !waitForPTY(2*time.Second, func() bool {
			return mcpServer.agentListCallCount() == 1
		}) {
			t.Fatalf("MCP discovery did not start under the logo:\n%s", screen.Plain())
		}
		if got := mcpServer.agentListCompletionCount(); got != 0 {
			t.Fatalf("slow MCP discovery completed before interactive-handoff assertion: %d", got)
		}
		draft = "draft while MCP connects"
		if _, err := ptm.Write([]byte(draft)); err != nil {
			t.Fatalf("type draft while MCP connects: %v", err)
		}
		if !waitForPTY(2*time.Second, func() bool {
			return strings.Contains(screen.VisiblePlain(), draft)
		}) {
			t.Fatalf("composer did not accept input while MCP was pending:\n%s", screen.Plain())
		}
		if got := mcpServer.agentListCompletionCount(); got != 0 {
			t.Fatalf("MCP discovery was no longer pending when draft appeared: completions=%d", got)
		}
	}

	clearsAfterCockpit := rawCount("\x1b[2J")
	if !waitForPTY(15*time.Second, func() bool {
		return mcpServer.agentListCompletionCount() == 1
	}) {
		t.Fatalf("MCP discovery never completed:\n%s", screen.Plain())
	}
	if reassertWinsize {
		// Daintree's cached-project reveal path reasserts the current PTY grid even
		// when it did not change. This may emit SIGWINCH; the cockpit must treat it
		// as geometry-idempotent rather than running a destructive clear/replay.
		if err := pty.Setsize(ptm, &pty.Winsize{Rows: rows, Cols: cols}); err != nil {
			t.Fatalf("reassert embedded-pane winsize: %v", err)
		}
	}
	// Let MCP completion and following renderer frames settle WITHOUT sending a key.
	// Before the fix this is exactly where the visible grid lost the composer; typing
	// one more character made it reappear.
	waitQuiet(rawLen, 300*time.Millisecond, 4*time.Second)
	visible := screen.VisiblePlain()
	if !strings.Contains(visible, composerGlyph) {
		t.Fatalf("healthy MCP completion erased the live footer until keypress:\n%s", screen.Plain())
	}
	if rawCount("Connecting to Daintree MCPs") != 0 || rawCount("Connected to Daintree MCPs") != 0 {
		t.Fatalf("healthy startup emitted MCP connection chatter:\n%s", screen.Plain())
	}
	if !strings.Contains(visible, "MCP") {
		t.Fatalf("compact MCP status missing from composer hint row:\n%s", screen.Plain())
	}
	if got := rawCount("\x1b[2J"); got != clearsAfterCockpit {
		t.Fatalf("startup issued a viewport clear after Bubble Tea took ownership: clears %d→%d", clearsAfterCockpit, got)
	}
	if got := rawCount(mastheadText); got != 1 {
		t.Fatalf("masthead committed %d times, want exactly once", got)
	}
	if draft == "" {
		// For the fast cases, type only AFTER the regression assertion. Earlier input can
		// mask a missing first paint by supplying the repaint itself.
		draft = "draft after connect"
		if _, err := ptm.Write([]byte(draft)); err != nil {
			t.Fatalf("type draft after connect: %v", err)
		}
		if !waitForPTY(5*time.Second, func() bool {
			return strings.Contains(screen.VisiblePlain(), draft)
		}) {
			t.Fatalf("composer did not accept input after startup discovery:\n%s", screen.Plain())
		}
	} else if !strings.Contains(screen.VisiblePlain(), draft) {
		t.Fatalf("draft typed during MCP discovery did not survive completion:\n%s", screen.Plain())
	}
	if got := screen.CountLineSubstr(draft); got != 1 {
		t.Fatalf("draft appears on %d composed lines, want one live composer (no frozen copy):\n%s", got, screen.Plain())
	}
	if got := mcpServer.agentListCallCount(); got != 1 {
		t.Fatalf("startup discovery ran agent.listAvailable %d times, want exactly 1 across splash", got)
	}

	// Ctrl+C is independent of the draft buffer and gives the staged shutdown its two
	// presses without changing the footer before the regression assertions above.
	_, _ = ptm.Write([]byte{3, 3})
	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()
	select {
	case <-waitErr:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		<-waitErr
		t.Error("cockpit did not exit within 10s after staged Ctrl+C")
	}
	_ = ptm.Close()
	drainWG.Wait()
	runtime.KeepAlive(ptm)
}

// TestPTYLargePasteScrollback is the regression for the large-paste scrollback corruption:
// pasting a block taller than the viewport made the YOU card commit in a SINGLE tea.Println
// taller than the screen, and Bubble Tea v2's insertAbove then clamped its CursorUp at the
// top of the viewport — freezing a copy of the composer footer ("› [pasted N lines]") into
// native scrollback above a gap of blank rows (bubbletea#1613 class). The fix splits every
// scrollback commit into viewport-sized prints (flush.go chunkPrintlns). This proves it on a
// real terminal: after the turn, the transient "[pasted …]" placeholder must NOT appear in
// committed scrollback, and the pasted message must commit exactly once (not duplicated).
func TestPTYLargePasteScrollback(t *testing.T) {
	if testing.Short() {
		t.Skip("PTY harness allocates a real pseudoterminal; skipped under -short")
	}
	bin := buildBinary(t) // auto-skips under -race

	const (
		pasteMarker = "PASTEMARKERZULU"
		sentinel    = "PASTEDONESENTINEL"
	)
	// A paste FAR taller than the 40-row viewport so the YOU card, committed whole, would
	// overflow a single insertAbove. Line 0 carries the marker; the rest are distinct filler.
	const nPasteLines = 70
	pasteLines := make([]string, nPasteLines)
	pasteLines[0] = pasteMarker + " — heist briefing line zero."
	for i := 1; i < nPasteLines; i++ {
		pasteLines[i] = fmt.Sprintf("Pasted briefing rule line %02d of the score.", i)
	}
	pasteBody := strings.Join(pasteLines, "\n")

	// One scripted round: the submit fires a single model request (no tool call), which
	// streams a short reply ending in the sentinel.
	fake := newFakeBackend(t,
		sseRound{
			contentTokens: []string{"Got the briefing. ", "Wrapped up " + sentinel + " now.\n\n"},
			usage:         &fakeUsage{prompt: 80, completion: 8, total: 88},
		},
	)

	const startRows, startCols = 40, 100
	cmd := exec.Command(bin)
	env := make([]string, 0, len(os.Environ())+12)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "DAINTREE_ASCII=") {
			continue
		}
		env = append(env, kv)
	}
	cmd.Env = append(env,
		"LC_ALL=C.UTF-8",
		"DAINTREE_BACKEND_URL="+fake.baseURL(),
		"DAINTREE_API_KEY=test-key",
		"DAINTREE_ASSISTANT_STATE_DIR="+t.TempDir(),
		"DAINTREE_ASSISTANT_TIER=operator",
		"DAINTREE_ASSISTANT_DEBUG_LOG=0",
		"DAINTREE_ASSISTANT_NO_SPLASH=1",
		noDaemonEnv,
		"DAINTREE_MCP_URL=",
		"DAINTREE_MCP_TOKEN=",
		"TERM=xterm-256color",
	)

	ptm, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: startRows, Cols: startCols})
	if err != nil {
		t.Fatalf("start binary under pty: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = ptm.Close()
	})

	screen := newVTScreen(startRows, startCols)
	var rawMu sync.Mutex
	var raw bytes.Buffer
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
	rawLen := func() int {
		rawMu.Lock()
		defer rawMu.Unlock()
		return raw.Len()
	}
	rawCount := func(sub string) int {
		rawMu.Lock()
		defer rawMu.Unlock()
		return bytes.Count(raw.Bytes(), []byte(sub))
	}

	// Phase 1: boot → steady state (composer glyph present).
	if !waitForPTY(20*time.Second, func() bool { return strings.Contains(screen.Plain(), composerGlyph) }) {
		t.Fatalf("cockpit never reached steady state:\n%s", screen.Plain())
	}
	waitQuiet(rawLen, 300*time.Millisecond, 4*time.Second)

	// Phase 2: bracketed paste of the tall block, then submit. The terminal wraps a paste in
	// ESC[200~ … ESC[201~; the composer recognizes it and stashes a one-line placeholder.
	if _, err := ptm.Write([]byte("\x1b[200~" + pasteBody + "\x1b[201~")); err != nil {
		t.Fatalf("write bracketed paste: %v", err)
	}
	placeholder := fmt.Sprintf("[pasted %d lines #1]", nPasteLines)
	if !waitForPTY(10*time.Second, func() bool { return strings.Contains(screen.Plain(), placeholder) }) {
		t.Fatalf("composer never showed the large-paste placeholder %q:\n%s", placeholder, screen.Plain())
	}
	if _, err := ptm.Write([]byte("\r")); err != nil {
		t.Fatalf("write submit: %v", err)
	}
	if !waitForPTY(30*time.Second, func() bool { return strings.Contains(screen.Plain(), sentinel) }) {
		t.Fatalf("turn never completed (sentinel %q not seen):\n%s", sentinel, screen.Plain())
	}
	waitQuiet(rawLen, 300*time.Millisecond, 4*time.Second)

	// Invariant 1: the transient placeholder must NEVER reach committed scrollback. After the
	// turn the composer has cleared, so any surviving "[pasted …]" line is a FROZEN footer copy
	// dumped by an over-tall insertAbove — the exact bug.
	if got := screen.CountLineSubstr("[pasted "); got != 0 {
		t.Errorf("the paste placeholder appears on %d committed line(s), want 0 — a frozen composer footer leaked into scrollback (#1613):\n%s", got, screen.Plain())
	}
	// Invariant 2: the pasted message commits exactly once — not dropped, not duplicated as a
	// frozen partial.
	if got := screen.CountLineSubstr(pasteMarker); got != 1 {
		t.Errorf("pasted YOU-card marker %q appears on %d composed lines, want 1 (dropped or duplicated in scrollback):\n%s", pasteMarker, got, screen.Plain())
	}

	// Phase 3: settled resize → the redraw RE-COMMITS the whole transcript, including the tall
	// sealed YOU-card turn, through the QUEUE commit path (scrollback.go commitCmd) rather than
	// the active-turn flush path. Shrinking the terminal makes the 70-row card even more over
	// the viewport, so this exercises the queue-path chunking — the resize-redraw vector that
	// the active-turn PTY assertions above don't reach. The same two invariants must hold on
	// the rebuilt scrollback.
	preMasthead := rawCount(mastheadText)
	const newRows, newCols = 20, 90
	if err := pty.Setsize(ptm, &pty.Winsize{Rows: newRows, Cols: newCols}); err != nil {
		t.Fatalf("resize pty: %v", err)
	}
	screen.Resize(newRows, newCols)
	if !waitForPTY(15*time.Second, func() bool { return rawCount(mastheadText) >= preMasthead+1 }) {
		t.Fatalf("resize did not re-commit the masthead (count=%d, want >= %d)", rawCount(mastheadText), preMasthead+1)
	}
	waitQuiet(rawLen, 400*time.Millisecond, 4*time.Second)
	if got := screen.CountLineSubstr("[pasted "); got != 0 {
		t.Errorf("after resize the paste placeholder appears on %d committed line(s), want 0 — the queue re-commit corrupted scrollback (#1613):\n%s", got, screen.Plain())
	}
	if got := screen.CountLineSubstr(pasteMarker); got != 1 {
		t.Errorf("after resize the pasted marker %q appears on %d composed lines, want 1 (queue re-commit dropped/duplicated it):\n%s", pasteMarker, got, screen.Plain())
	}

	// Phase 4: clean shutdown.
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
	_ = ptm.Close()
	drainWG.Wait()
	runtime.KeepAlive(ptm)
}

// TestPTYStreamingMarkdownNoChurn is the end-to-end regression for the cockpit-footer churn the
// user reported: a long MARKDOWN paragraph streamed token by token used to be WITHHELD from
// scrollback until it sealed on "\n\n", so it piled into the height-capped live footer — its early
// rows scrolled off the top of the ~8-row window into nowhere and only landed in scrollback, all at
// once, when the paragraph finished ("a 5-line window that scrolls, then flicks over").
//
// The signal that distinguishes fixed from broken on a REAL terminal: with the line-level commit,
// early prose reaches native scrollback SECONDS before the paragraph seals; with withhold-until-seal
// the early prose and the closing sentinel land in the SAME seal burst. So we stream ONE long
// markdown paragraph slowly and assert EARLYMARKER appears in scrollback well before SENTINEL. (The
// existing TestPTYCockpitRenderHarness can't catch this — it streams many SHORT "\n\n"-terminated
// paragraphs, which committed fine even before the fix.) We also assert the high-frequency
// line-commit cadence stays #1613-safe: the footer never grows tall, and nothing is duplicated.
func TestPTYStreamingMarkdownNoChurn(t *testing.T) {
	if testing.Short() {
		t.Skip("PTY harness allocates a real pseudoterminal and drives a slow streamed turn; skipped under -short")
	}
	bin := buildBinary(t) // auto-skips under -race

	const (
		earlyMarker = "EARLYMARKERZULU"
		sentinel    = "SENTINELOMEGA"
	)
	// ONE long markdown paragraph (no "\n\n" until the very end) streamed word by word, with
	// **bold** and `code` spans so it travels the markdown commit path. On the short terminal below
	// it wraps to MORE than a screen of rows, so its early lines must scroll off the top into native
	// scrollback HISTORY — but only if they were committed line by line. The closing blank line seals
	// it and a final paragraph carries SENTINEL.
	tokens := []string{"The ", "**streaming** ", earlyMarker + " ", "report-line ", "opens ", "with ", "a ", "`code` ", "span ", "and "}
	for i := 0; i < 100; i++ {
		w := fmt.Sprintf("flowing-word-%03d ", i) // long words → few per row → many wrapped rows
		if i%9 == 4 {
			w = fmt.Sprintf("**emphasis-word-%03d** ", i)
		} else if i%9 == 7 {
			w = fmt.Sprintf("`code-word-%03d` ", i)
		}
		tokens = append(tokens, w)
	}
	tokens = append(tokens, "and it wraps on and on.\n\n", "All "+sentinel+" wrapped up.\n\n")

	fake := newFakeBackend(t, sseRound{
		contentTokens: tokens,
		tokenDelay:    15 * time.Millisecond, // ~1.7s stream so we can observe it mid-flight
		usage:         &fakeUsage{prompt: 40, completion: 110, total: 150},
	})

	// A SHORT, narrow terminal so the long paragraph overflows the screen and its early committed
	// lines scroll into scrollback HISTORY while the turn is still streaming.
	const startRows, startCols = 24, 72
	cmd := exec.Command(bin)
	env := make([]string, 0, len(os.Environ())+12)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "DAINTREE_ASCII=") {
			continue
		}
		env = append(env, kv)
	}
	cmd.Env = append(env,
		"LC_ALL=C.UTF-8",
		"DAINTREE_BACKEND_URL="+fake.baseURL(),
		"DAINTREE_API_KEY=test-key",
		"DAINTREE_ASSISTANT_STATE_DIR="+t.TempDir(),
		"DAINTREE_ASSISTANT_TIER=operator",
		"DAINTREE_ASSISTANT_DEBUG_LOG=0",
		"DAINTREE_ASSISTANT_NO_SPLASH=1",
		noDaemonEnv,
		"DAINTREE_MCP_URL=",
		"DAINTREE_MCP_TOKEN=",
		"TERM=xterm-256color",
	)

	ptm, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: startRows, Cols: startCols})
	if err != nil {
		t.Fatalf("start binary under pty: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = ptm.Close()
	})

	screen := newVTScreen(startRows, startCols)
	var rawMu sync.Mutex
	var raw bytes.Buffer
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
	rawLen := func() int {
		rawMu.Lock()
		defer rawMu.Unlock()
		return raw.Len()
	}

	// Phase 1: boot → steady state.
	if !waitForPTY(20*time.Second, func() bool { return strings.Contains(screen.Plain(), composerGlyph) }) {
		t.Fatalf("cockpit never reached steady state:\n%s", screen.Plain())
	}
	waitQuiet(rawLen, 300*time.Millisecond, 4*time.Second)

	// Phase 2: drive the slow streamed paragraph and TIME when each marker reaches the screen.
	screen.ResetPeakFrameHeight()
	if _, err := ptm.Write([]byte("write me a long report\r")); err != nil {
		t.Fatalf("write prompt to pty: %v", err)
	}
	// Measure WHEN the early prose reaches committed scrollback HISTORY (not the live footer — see
	// CountHistorySubstr) vs WHEN the paragraph seals (SENTINEL appears). With line-level commit the
	// early prose scrolls into history seconds before the seal; with withhold-until-seal it never
	// reaches history until the whole paragraph dumps in at the seal — so the two times collapse.
	var earlyHistAt, sentinelAt time.Time
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if earlyHistAt.IsZero() && screen.CountHistorySubstr(earlyMarker) >= 1 {
			earlyHistAt = time.Now()
		}
		if screen.CountLineSubstr(sentinel) >= 1 {
			sentinelAt = time.Now()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if sentinelAt.IsZero() {
		t.Fatalf("turn never completed (sentinel %q not seen):\n%s", sentinel, screen.Plain())
	}
	if earlyHistAt.IsZero() {
		t.Fatalf("early prose %q never reached committed scrollback history — it was withheld in the live footer (the churn bug):\n%s", earlyMarker, screen.Plain())
	}
	// THE churn assertion: early prose was COMMITTED to scrollback well before the paragraph sealed.
	gap := sentinelAt.Sub(earlyHistAt)
	t.Logf("EARLYMARKER reached committed scrollback %v before SENTINEL", gap)
	// Threshold with wide margin: line-commit yields hundreds of ms (server-paced streaming);
	// withhold-until-seal yields TENS OF MICROSECONDS (both land in the same seal burst). 300ms sits
	// 4 orders of magnitude above the broken case, so it can't false-pass even on a slow worker.
	if gap < 300*time.Millisecond {
		t.Errorf("early prose reached committed scrollback only %v before the paragraph sealed — it was withheld and churned in the capped footer instead of committing line by line (want a multi-second gap)", gap)
	}
	waitQuiet(rawLen, 300*time.Millisecond, 4*time.Second)

	// #1613 safety under the high-frequency line-commit cadence: the footer never grew tall.
	peak := screen.PeakFrameHeight()
	t.Logf("streamed-paragraph peak live-View height: %d rows (bound %d)", peak, maxFooterRows)
	if peak <= 0 {
		t.Errorf("never observed a scrollback commit during the turn (peak frame height = %d) — vacuous", peak)
	}
	if peak > maxFooterRows {
		t.Errorf("live View peaked at %d rows, want <= %d — the footer swallowed the stream (#1613 class)", peak, maxFooterRows)
	}
	// No duplication: the markers each land on exactly one composed line.
	for _, m := range []string{earlyMarker, sentinel} {
		if got := screen.CountLineSubstr(m); got != 1 {
			t.Errorf("marker %q appears on %d composed lines, want 1 (duplicated/lost in scrollback):\n%s", m, got, screen.Plain())
		}
	}

	// Phase 3: clean shutdown.
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
	_ = ptm.Close()
	drainWG.Wait()
	runtime.KeepAlive(ptm)
}

// TestPTYStreamingBulletListNoChurn is the real-terminal #1613-SAFETY check for the bullet-list fix.
// A streaming list can't commit line by line (glamour re-flows it as items arrive), so it is WITHHELD
// until "\n\n" and rendered WHOLE into the live footer, which the fix sized the cap to hold. Raising
// the cap means a TALLER live footer, so the risk is that the seal commit (the whole list via
// tea.Println) corrupts scrollback (#1613). This streams a multi-row bullet list and asserts it
// commits CLEANLY end to end: every list item lands in scrollback exactly once (no dup, no loss) and
// the live View stayed within the raised list budget.
//
// NOTE: the streaming-footer churn itself (the list head scrolling off the capped footer) is detected
// authoritatively by the headless TestStreaming_BulletListDoesNotChurn, which fails if maxLiveRows is
// reverted. A real PTY can't reliably assert it: the minimal VT model (vtscreen_test.go) accumulates
// grid+history, so a list line rendered early (when the list was short) lingers in the model after
// the cap later truncates it from the live footer — a false positive. So this test deliberately
// asserts only what a real terminal shows reliably: the committed scrollback after the turn.
func TestPTYStreamingBulletListNoChurn(t *testing.T) {
	if testing.Short() {
		t.Skip("PTY harness allocates a real pseudoterminal and drives a slow streamed turn; skipped under -short")
	}
	bin := buildBinary(t) // auto-skips under -race

	const (
		firstItem = "FIRSTITEMZULU"
		lastItem  = "LASTITEMOMEGA"
		sentinel  = "SENTINELDELTA"
	)
	// A leading paragraph (line-commits) then a 6-item bullet list (withheld until "\n\n"), each item
	// on its own line so the whole list is one block. >8 rendered rows so it would churn under the old
	// cap; <= the raised cap so it renders whole. A blank line seals it, then SENTINEL.
	tokens := []string{"Here is the project summary you asked for.\n\n", "**Key details:**\n"}
	tokens = append(tokens,
		"- "+firstItem+" the branch is main and it is currently stable\n",
		"- the second detail line carries enough text to wrap once here\n",
		"- a third detail line that also runs long enough to wrap around\n",
		"- a fourth detail line continuing the list with more content here\n",
		"- a fifth detail line keeping the list growing well past eight rows\n",
		"- "+lastItem+" the final list item right before the list closes\n",
	)
	tokens = append(tokens, "\nThat is the full ", sentinel+" summary.\n\n")

	fake := newFakeBackend(t, sseRound{
		contentTokens: tokens,
		tokenDelay:    40 * time.Millisecond, // slow enough to observe the list mid-stream, pre-seal
		usage:         &fakeUsage{prompt: 40, completion: 90, total: 130},
	})

	// Tall enough that the footer budget is the raised cap (not the terminal height): budget =
	// rows - bottomBand - 2, so 30 rows leaves the cap (16) binding.
	const startRows, startCols = 30, 72
	cmd := exec.Command(bin)
	env := make([]string, 0, len(os.Environ())+12)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "DAINTREE_ASCII=") {
			continue
		}
		env = append(env, kv)
	}
	cmd.Env = append(env,
		"LC_ALL=C.UTF-8",
		"DAINTREE_BACKEND_URL="+fake.baseURL(),
		"DAINTREE_API_KEY=test-key",
		"DAINTREE_ASSISTANT_STATE_DIR="+t.TempDir(),
		"DAINTREE_ASSISTANT_TIER=operator",
		"DAINTREE_ASSISTANT_DEBUG_LOG=0",
		"DAINTREE_ASSISTANT_NO_SPLASH=1",
		noDaemonEnv,
		"DAINTREE_MCP_URL=",
		"DAINTREE_MCP_TOKEN=",
		"TERM=xterm-256color",
	)

	ptm, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: startRows, Cols: startCols})
	if err != nil {
		t.Fatalf("start binary under pty: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = ptm.Close()
	})

	screen := newVTScreen(startRows, startCols)
	var rawMu sync.Mutex
	var raw bytes.Buffer
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
	rawLen := func() int {
		rawMu.Lock()
		defer rawMu.Unlock()
		return raw.Len()
	}

	// Phase 1: boot → steady state.
	if !waitForPTY(20*time.Second, func() bool { return strings.Contains(screen.Plain(), composerGlyph) }) {
		t.Fatalf("cockpit never reached steady state:\n%s", screen.Plain())
	}
	waitQuiet(rawLen, 300*time.Millisecond, 4*time.Second)

	// Phase 2: stream the whole list turn to completion.
	screen.ResetPeakFrameHeight()
	if _, err := ptm.Write([]byte("summarize the project\r")); err != nil {
		t.Fatalf("write prompt to pty: %v", err)
	}
	if !waitForPTY(30*time.Second, func() bool { return strings.Contains(screen.Plain(), sentinel) }) {
		t.Fatalf("turn never completed (sentinel %q not seen):\n%s", sentinel, screen.Plain())
	}
	waitQuiet(rawLen, 300*time.Millisecond, 4*time.Second)

	// #1613 safety: even rendering the whole withheld list, the live View stayed within the raised
	// list budget — a non-trivial peak (> 0) also proves a commit was actually observed.
	peak := screen.PeakFrameHeight()
	t.Logf("bullet-list peak live-View height: %d rows (bound %d)", peak, maxListFooterRows)
	if peak <= 0 {
		t.Errorf("never observed a scrollback commit during the turn (peak frame height = %d) — vacuous", peak)
	}
	if peak > maxListFooterRows {
		t.Errorf("live View peaked at %d rows, want <= %d — the withheld list overflowed the budget (#1613 class)", peak, maxListFooterRows)
	}
	// No dup / no loss: every list item (head, a middle item, and the tail) plus the sentinel lands
	// in committed scrollback on exactly one composed line — the corruption signature of #1613 (a
	// frozen partial) would duplicate or drop one.
	for _, mk := range []string{firstItem, "fifth detail line", lastItem, sentinel} {
		if got := screen.CountLineSubstr(mk); got != 1 {
			t.Errorf("marker %q appears on %d composed lines, want 1 (duplicated/lost in scrollback):\n%s", mk, got, screen.Plain())
		}
	}

	// Phase 3: clean shutdown.
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
	_ = ptm.Close()
	drainWG.Wait()
	runtime.KeepAlive(ptm)
}

// waitFor polls cond every 50ms until it is true or the timeout elapses. Returns
// the final value of cond so the caller can fail with context on a miss.
func waitForPTY(timeout time.Duration, cond func() bool) bool {
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
