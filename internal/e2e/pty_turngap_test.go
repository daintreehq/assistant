//go:build pty

package e2e

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
)

// pty_turngap_test.go measures the vertical gap BETWEEN consecutive turns in real
// committed scrollback.
//
// The rule is one blank line above each turn's `YOU` card — the shared margin-top every
// sealed cell owns. Anything more is dead space that the user pays for on every exchange,
// and it accumulates: a session is mostly this boundary repeated.
//
// It has to be a PTY test. The gap is produced by the interaction of three things no unit
// test sees together — the incremental row flush, the seal's tail commit (which prints an
// EMPTY block when the turn fully streamed), and the live footer collapsing — and each
// contributes rows through `tea.Println` rather than through any renderer this repo can
// call directly. Reasoning about the arithmetic on paper is exactly how the earlier
// footer-geometry bugs were introduced.

// maxBlankRowsBetweenTurns is the permitted blank run between the end of one turn's
// output and the next turn's `YOU` label. One blank is the intended margin; a second is
// tolerated because the seal of a fully-streamed turn legitimately prints one empty
// block. More than that is the regression this test exists to catch.
const maxBlankRowsBetweenTurns = 2

func TestPTYGapBetweenConsecutiveTurns(t *testing.T) {
	if testing.Short() {
		t.Skip("PTY harness allocates a real pseudoterminal; skipped under -short")
	}
	bin := buildBinary(t)

	const sentinelA = "PTYGAPONE"
	const sentinelB = "PTYGAPTWO"
	// Two prose-only rounds. Short enough to stay on one screen, so the whole boundary
	// is measurable in the visible grid rather than split across scrollback history.
	fake := newFakeBackend(t,
		sseRound{
			contentTokens: []string{"First reply paragraph.\n\n", "Second one " + sentinelA + "."},
			tokenDelay:    10 * time.Millisecond,
			usage:         &fakeUsage{prompt: 40, completion: 8, total: 48},
		},
		sseRound{
			contentTokens: []string{"Another reply " + sentinelB + "."},
			tokenDelay:    10 * time.Millisecond,
			usage:         &fakeUsage{prompt: 40, completion: 8, total: 48},
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

	if !waitForPTY(20*time.Second, func() bool {
		return strings.Contains(screen.Plain(), composerGlyph)
	}) {
		t.Fatalf("cockpit never reached steady state:\n%s", screen.Plain())
	}
	waitQuiet(rawLen, 300*time.Millisecond, 4*time.Second)

	for i, prompt := range []string{"first question\r", "second question\r"} {
		if _, err := ptm.Write([]byte(prompt)); err != nil {
			t.Fatalf("write prompt %d to pty: %v", i, err)
		}
		want := sentinelA
		if i == 1 {
			want = sentinelB
		}
		if !waitForPTY(30*time.Second, func() bool { return strings.Contains(screen.Plain(), want) }) {
			t.Fatalf("turn %d never completed (sentinel %q not seen):\n%s", i, want, screen.Plain())
		}
		// The seal's final commit and the footer collapse both have to land before the
		// next prompt, or the measurement catches a transient frame.
		waitQuiet(rawLen, 500*time.Millisecond, 6*time.Second)
	}

	// Measure on the full transcript (history + visible), since the first turn may have
	// scrolled off on a short terminal.
	lines := strings.Split(screen.Plain(), "\n")
	secondYou := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == "YOU" && strings.Contains(strings.Join(lines[:i], "\n"), sentinelA) {
			secondYou = i
			break
		}
	}
	if secondYou < 0 {
		t.Fatalf("could not locate the second turn's YOU card:\n%s", screen.Plain())
	}
	blanks := 0
	for i := secondYou - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			break
		}
		blanks++
	}
	t.Logf("blank rows between turn 1's output and turn 2's YOU card: %d", blanks)
	if blanks > maxBlankRowsBetweenTurns {
		t.Errorf("%d blank rows between consecutive turns, want at most %d — dead space paid for on every exchange:\n%s",
			blanks, maxBlankRowsBetweenTurns, screen.Plain())
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
