package e2e

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// promptfile_test.go pins --prompt-file END TO END, through the real binary: the file's
// content must arrive at the backend as the user message of the turn.
//
// Nothing else proves that link. The parser tests stop at Options (parseArgs is
// deliberately I/O-free, so it never sees the text), and the reader tests call the reader
// directly — so the one line in RunOneShot that assigns the read result to opts.Prompt
// could be deleted and every one of them would still pass while the backend received an
// empty prompt. This test is the assertion that the flag actually asks the question.

// runPromptFile invokes the binary against a fake backend, returning stderr and the exit
// code. stdin is supplied so the "-" case can stream a prompt in.
func runPromptFile(t *testing.T, fake *fakeBackend, stdin string, args ...string) (string, int) {
	t.Helper()
	bin := buildBinary(t)
	cmd := exec.Command(bin, args...)
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Env = append(cmd.Environ(),
		"DAINTREE_BACKEND_URL="+fake.baseURL(),
		"DAINTREE_ASSISTANT_STATE_DIR="+t.TempDir(),
		"DAINTREE_ASSISTANT_DEBUG_LOG=0",
		"DAINTREE_MCP_URL=", // no MCP → clean degraded local mode
		"DAINTREE_MCP_TOKEN=",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	code := 0
	if runErr != nil {
		ee, ok := runErr.(*exec.ExitError)
		if !ok {
			t.Fatalf("run binary: %v (stderr: %s)", runErr, stderr.String())
		}
		code = ee.ExitCode()
	}
	return stderr.String(), code
}

// lastUserMessage returns the content of the final user-role message of the Nth respond
// request — the turn's actual question, past the structured startup/runtime blocks.
func lastUserMessage(t *testing.T, fake *fakeBackend, n int) string {
	t.Helper()
	msgs := fake.requestMessages(n)
	if len(msgs) == 0 {
		t.Fatalf("no messages recorded on respond request %d", n)
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if role, _ := msgs[i]["role"].(string); role == "user" {
			content, _ := msgs[i]["content"].(string)
			return content
		}
	}
	t.Fatalf("no user message in respond request %d: %+v", n, msgs)
	return ""
}

// TestPromptFileReachesTheBackend: a multi-line prompt read from a file arrives intact.
// Multi-line is the point of the flag — a prompt that only survived as its first line
// would look like a working feature and ask a different question.
func TestPromptFileReachesTheBackend(t *testing.T) {
	fake := newFakeBackend(t, sseRound{contentTokens: []string{"Done."}})

	prompt := "Check every worktree.\n\nThen report which are ready to merge."
	path := filepath.Join(t.TempDir(), "prompt.md")
	if err := os.WriteFile(path, []byte(prompt+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	stderr, code := runPromptFile(t, fake, "", "--json", "--prompt-file", path)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if got := lastUserMessage(t, fake, 0); got != prompt {
		t.Errorf("backend received %q, want the file's content %q", got, prompt)
	}
}

// TestPromptFileFromStdinReachesTheBackend: "-" is the streaming spelling, and it is the
// one that a harness piping a heredoc actually uses.
func TestPromptFileFromStdinReachesTheBackend(t *testing.T) {
	fake := newFakeBackend(t, sseRound{contentTokens: []string{"Done."}})

	stderr, code := runPromptFile(t, fake, "  which agents are stuck?\n", "--json", "--prompt-file", "-")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if got := lastUserMessage(t, fake, 0); got != "which agents are stuck?" {
		t.Errorf("backend received %q, want the stdin prompt", got)
	}
}

// TestPromptFileFailureIsFatalAndNeverAsksAnEmptyQuestion: an unreadable prompt file must
// stop the run, not send an empty turn. Asserting the backend was never called is the
// half that matters — a run that failed AFTER spending a turn would still exit non-zero.
func TestPromptFileFailureIsFatalAndNeverAsksAnEmptyQuestion(t *testing.T) {
	fake := newFakeBackend(t, sseRound{contentTokens: []string{"Done."}})
	missing := filepath.Join(t.TempDir(), "no-such-prompt.md")

	stderr, code := runPromptFile(t, fake, "", "--prompt-file", missing)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stderr, "--prompt-file") || !strings.Contains(stderr, missing) {
		t.Errorf("stderr should name the flag and the path, got %q", stderr)
	}
	if msgs := fake.requestMessages(0); msgs != nil {
		t.Errorf("the backend was called despite an unreadable prompt file: %+v", msgs)
	}
}
