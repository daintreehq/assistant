//go:build pty

package e2e

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
)

// pty_tailgap_test.go reproduces the "extra scroll area below the composer" report
// (2026-07-11): after a turn finishes, the live footer must sit at the BOTTOM of the
// terminal viewport. If the footer ends N rows above the bottom, the host terminal
// (xterm) exposes those N blank rows as scrollable area BELOW the input — the user
// lands under the composer after every response and has to scroll up.
//
// The invariant only applies once the turn has actually scrolled the screen (before
// that, an inline app legitimately sits high on a fresh viewport), so the test streams
// enough paragraphs to push history and only then asserts the tail gap.

// LastNonBlankRow returns the 0-based index of the last visible-grid row with any
// non-space content, or -1 for an all-blank grid.
func (s *vtScreen) LastNonBlankRow() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	for r := s.rows - 1; r >= 0; r-- {
		if strings.TrimSpace(string(s.grid[r])) != "" {
			return r
		}
	}
	return -1
}

// CursorRow returns the 0-based cursor row.
func (s *vtScreen) CursorRow() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.row
}

// HistoryLen reports how many lines have scrolled off the top — >0 proves the view
// bottom reached the terminal bottom during the turn.
func (s *vtScreen) HistoryLen() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.history)
}

// Rows returns the current grid height.
func (s *vtScreen) Rows() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rows
}

func TestPTYTurnEndFooterAtViewportBottom(t *testing.T) {
	// Two deliveries, both real-world shapes: "instant" arrives in one Update batch (the
	// sealed cell never streams incrementally — the whole turn goes through the sealed-
	// block commit while the footer holds it), "streamed" paces tokens so the incremental
	// line flush runs and the footer stays short until the seal commits the tail.
	t.Run("instant", func(t *testing.T) { runTailGapTurn(t, 0) })
	t.Run("streamed", func(t *testing.T) { runTailGapTurn(t, 20*time.Millisecond) })
}

func runTailGapTurn(t *testing.T, tokenDelay time.Duration) {
	if testing.Short() {
		t.Skip("PTY tail-gap harness allocates a real pseudoterminal; skipped under -short")
	}
	bin := buildBinary(t)

	const sentinel = "PTYTAILSENTINEL"
	// One prose-only round, long enough to scroll a 40-row terminal. The final
	// paragraph is UNTERMINATED (no trailing \n\n): it can't settle while streaming, so
	// the sealed-cell commit prints a real non-empty tail — exercising the selection-
	// time cell drop + re-pin, not just the empty-seal ledger path.
	round := []string{}
	for i := 0; i < 30; i++ {
		round = append(round, fmt.Sprintf("Paragraph %d of the tail-gap turn streamed and settled.\n\n", i))
	}
	round = append(round, "Wrapped up "+sentinel+" now.")
	fake := newFakeBackend(t, sseRound{
		contentTokens: round,
		tokenDelay:    tokenDelay,
		usage:         &fakeUsage{prompt: 40, completion: 8, total: 48},
	})

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

	// Boot → steady state.
	if !waitForPTY(20*time.Second, func() bool {
		return strings.Contains(screen.Plain(), composerGlyph)
	}) {
		t.Fatalf("cockpit never reached steady state:\n%s", screen.Plain())
	}
	waitQuiet(rawLen, 300*time.Millisecond, 4*time.Second)

	// Drive the turn.
	if _, err := ptm.Write([]byte("tail gap check\r")); err != nil {
		t.Fatalf("write prompt to pty: %v", err)
	}
	if !waitForPTY(30*time.Second, func() bool { return strings.Contains(screen.Plain(), sentinel) }) {
		t.Fatalf("turn never completed (sentinel %q not seen):\n%s", sentinel, screen.Plain())
	}
	// Let the seal's final commit + footer collapse fully settle.
	waitQuiet(rawLen, 500*time.Millisecond, 6*time.Second)

	if screen.HistoryLen() == 0 {
		t.Fatalf("turn never scrolled the screen (history empty) — the tail-gap invariant is vacuous:\n%s", screen.Plain())
	}

	// The LIVE composer must still be on the visible grid — a frozen composer copy in
	// history (or a wiped footer) must not satisfy the tail-gap assertion by accident.
	if !strings.Contains(screen.VisiblePlain(), composerGlyph) {
		t.Fatalf("live composer missing from the visible grid after the turn:\n%s", screen.VisiblePlain())
	}
	last := screen.LastNonBlankRow()
	gap := screen.Rows() - 1 - last
	t.Logf("tail gap after turn: %d blank rows below the footer (last content row %d of %d, cursor row %d, history %d)",
		gap, last, screen.Rows(), screen.CursorRow(), screen.HistoryLen())
	// One trailing blank row is tolerated (cursor parking); more means the host terminal
	// shows dead scroll area below the composer.
	if gap > 1 {
		t.Errorf("footer ends %d rows above the viewport bottom — extra scroll area below the composer:\n%s",
			gap, screen.VisiblePlain())
	}

	_, _ = ptm.Write([]byte("/quit\r"))
	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()
	select {
	case <-waitErr:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		<-waitErr
	}
	_ = ptm.Close()
	drainWG.Wait()
}
