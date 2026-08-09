package e2e

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

// inlineForbiddenEscapes are the terminal control sequences the assistant must
// NEVER emit (the Claude-Code inline-cockpit contract): switching to the alternate
// screen buffer, or enabling any mouse-tracking mode would steal the host
// terminal's native scrolling / selection / copy-paste. The cockpit's View-level
// guarantee is enforced in internal/ui/view_test.go (AltScreen==false,
// MouseMode==MouseModeNone, and View() free of \x1b[?1049h); here we close the gap
// at the BINARY boundary for the non-cockpit one-shot paths, which share the same
// process and must likewise keep stdout/stderr clean of these escapes.
var inlineForbiddenEscapes = map[string]string{
	"\x1b[?1049h": "alt-screen enter",
	"\x1b[?1049l": "alt-screen leave",
	"\x1b[?1000h": "mouse X10 tracking",
	"\x1b[?1002h": "mouse button tracking",
	"\x1b[?1003h": "mouse any-motion tracking",
	"\x1b[?1006h": "SGR mouse mode",
	"\x1b[?1015h": "urxvt mouse mode",
}

// assertNoForbiddenEscapes fails if the captured output carries any alt-screen or
// mouse-mode escape.
func assertNoForbiddenEscapes(t *testing.T, stream, output string) {
	t.Helper()
	for seq, name := range inlineForbiddenEscapes {
		if strings.Contains(output, seq) {
			t.Errorf("%s emitted %s escape (%q) — violates the inline-cockpit contract", stream, name, seq)
		}
	}
}

// TestBinaryRunNeverEmitsAltScreenOrMouse runs the real binary in the --json
// one-shot path (driven against the fake Daintree backend so it completes a full
// turn) and asserts NEITHER stdout NOR stderr ever contains an alt-screen switch or
// a mouse-tracking enable. This is the binary-level analogue of the unit-level
// inline guarantee; a true PTY harness driving the live Bubble Tea cockpit is
// impractical in-package (it needs an allocated TTY + the full program loop), so the
// cockpit's own View() contract stays guarded by internal/ui/view_test.go
// (TestViewOptions_NoAltScreenNoMouse + the View()-string scan), and this test
// covers the shared one-shot process boundary end-to-end.
func TestBinaryRunNeverEmitsAltScreenOrMouse(t *testing.T) {
	bin := buildBinary(t)

	fake := newFakeBackend(t,
		sseRound{
			contentTokens: []string{"Done."},
			usage:         &fakeUsage{prompt: 30, completion: 2, total: 32, cached: 0},
		},
	)

	dir := t.TempDir()
	cmd := exec.Command(bin, "--json", "hello")
	cmd.Env = append(cmd.Environ(),
		"DAINTREE_BACKEND_URL="+fake.baseURL(),
		"DAINTREE_API_KEY=test-key",
		"DAINTREE_ASSISTANT_STATE_DIR="+dir,
		"DAINTREE_ASSISTANT_TIER=operator",
		"DAINTREE_ASSISTANT_DEBUG_LOG=0",
		"DAINTREE_MCP_URL=",
		"DAINTREE_MCP_TOKEN=",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			t.Fatalf("run binary: %v (stderr: %s)", err, stderr.String())
		}
	}

	assertNoForbiddenEscapes(t, "stdout", stdout.String())
	assertNoForbiddenEscapes(t, "stderr", stderr.String())
}
