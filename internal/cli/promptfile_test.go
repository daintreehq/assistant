package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/cli/render"
	"github.com/daintreehq/assistant/internal/config"
	"github.com/daintreehq/assistant/internal/projectinstructions"
)

// writeTemp writes body to a file in a fresh temp dir and returns its path.
func writeTemp(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// discardRenderer is a Renderer whose warnings go nowhere: buildOverrides warns about an
// unreadable DAINTREE.md, and none of these tests are asserting on that channel.
func discardRenderer() *render.Renderer { return render.New(io.Discard) }

// endlessReader is /dev/zero without the device: a producer that never reaches EOF. It
// counts what it handed out so a test can prove the read was BOUNDED rather than merely
// terminated by a cooperative fixture.
type endlessReader struct{ n int64 }

func (e *endlessReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'a'
	}
	e.n += int64(len(p))
	return len(p), nil
}

// TestReadPromptFilePreservesMultilineContent: the whole point of the flag is a long
// multi-line prompt, so interior newlines must survive untouched — only the surrounding
// whitespace a text editor adds is trimmed.
func TestReadPromptFilePreservesMultilineContent(t *testing.T) {
	body := "\n  Line one.\n\nLine three.\n  \n"
	got, err := readPromptFile(writeTemp(t, "prompt.md", body), strings.NewReader(""))
	if err != nil {
		t.Fatalf("readPromptFile() error = %v", err)
	}
	if got != "Line one.\n\nLine three." {
		t.Errorf("prompt = %q", got)
	}
}

// TestReadPromptFileDashIsStdinAndOnlyTheBareToken pins the `-` convention on BOTH
// sides: the bare token reads the supplied stdin, and `./-` stays an ordinary filename
// (otherwise a real file named "-" becomes unreadable).
func TestReadPromptFileDashIsStdinAndOnlyTheBareToken(t *testing.T) {
	got, err := readPromptFile("-", strings.NewReader("  from stdin\n"))
	if err != nil {
		t.Fatalf("readPromptFile(\"-\") error = %v", err)
	}
	if got != "from stdin" {
		t.Errorf("stdin prompt = %q", got)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "-"), []byte("from a file named dash"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = readPromptFile(filepath.Join(dir, "-"), strings.NewReader("from stdin"))
	if err != nil {
		t.Fatalf("readPromptFile(\"<dir>/-\") error = %v", err)
	}
	if got != "from a file named dash" {
		t.Errorf("a path ending in - must be read as a file, got %q", got)
	}
}

// TestReadPromptFileDoesNotCloseStdin: os.Stdin belongs to the process, not to this
// read. Closing it would break anything that reads stdin afterwards, and the reader is
// passed in precisely so the flag layer never has to reach for the global.
func TestReadPromptFileDoesNotCloseStdin(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	go func() {
		w.WriteString("ask something")
		w.Close()
	}()
	if _, err := readPromptFile("-", r); err != nil {
		t.Fatalf("readPromptFile() error = %v", err)
	}
	// A closed *os.File returns ErrClosed here; an open one returns io.EOF.
	if _, err := r.Read(make([]byte, 1)); err != io.EOF {
		t.Errorf("stdin was closed by the read: %v", err)
	}
}

// TestReadPromptFileIsBounded is the reason the read is bounded at all: a caller-supplied
// path need not be a regular file, and --timeout cannot preempt a syscall already in
// flight. Exactly the limit is ACCEPTED (a bound that rejected its own boundary would be
// off by one), one byte more is REJECTED rather than truncated, and an endless producer
// is stopped after limit+1 bytes instead of consuming memory forever.
func TestReadPromptFileIsBounded(t *testing.T) {
	atLimit := writeTemp(t, "at-limit.md", strings.Repeat("a", maxPromptFileBytes))
	got, err := readPromptFile(atLimit, strings.NewReader(""))
	if err != nil {
		t.Fatalf("a prompt of exactly the limit must be accepted: %v", err)
	}
	if len(got) != maxPromptFileBytes {
		t.Errorf("len = %d, want %d", len(got), maxPromptFileBytes)
	}

	over := writeTemp(t, "over.md", strings.Repeat("a", maxPromptFileBytes+1))
	if _, err := readPromptFile(over, strings.NewReader("")); err == nil {
		t.Fatal("an oversized prompt must be rejected, never truncated")
	} else if !strings.Contains(err.Error(), "limit") {
		t.Errorf("error should name the limit, got: %v", err)
	}

	endless := &endlessReader{}
	if _, err := readPromptFile("-", endless); err == nil {
		t.Fatal("an endless stdin producer must be rejected")
	}
	if endless.n > maxPromptFileBytes+1 {
		t.Errorf("read %d bytes from an endless producer, want at most %d", endless.n, maxPromptFileBytes+1)
	}
}

// TestPromptFileFailuresAreFatal: unlike a key file there is no other source to fall back
// TO — the prompt is the caller's actual question, so every failure is an error rather
// than a run that asks nothing.
func TestPromptFileFailuresAreFatal(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		"missing":   filepath.Join(dir, "nope.md"),
		"directory": dir,
		"blank":     writeTemp(t, "blank.md", "   \n\t\n"),
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := readPromptFile(path, strings.NewReader("")); err == nil {
				t.Fatalf("%s must be an error", name)
			} else if !strings.Contains(err.Error(), "--prompt-file") {
				t.Errorf("error should name the flag, got: %v", err)
			}
		})
	}
}

// TestProjectInstructionsFileUsesTheDaintreeMdBudget: both sources land in the same
// prompt field, so they share the 16 KiB cap — and oversized content is rejected here
// rather than skipped-with-a-warning as the implicit loader does, because a named file
// must never silently contribute nothing.
func TestProjectInstructionsFileUsesTheDaintreeMdBudget(t *testing.T) {
	at := writeTemp(t, "brief.md", strings.Repeat("b", projectinstructions.MaxBytes))
	if got, err := readProjectInstructionsFile(at); err != nil {
		t.Fatalf("exactly MaxBytes must be accepted: %v", err)
	} else if len(got) != projectinstructions.MaxBytes {
		t.Errorf("len = %d, want %d", len(got), projectinstructions.MaxBytes)
	}
	over := writeTemp(t, "big.md", strings.Repeat("b", projectinstructions.MaxBytes+1))
	if _, err := readProjectInstructionsFile(over); err == nil {
		t.Fatal("oversized instructions must be rejected")
	}
}

// TestProjectInstructionsFileFailuresAreFatal: falling through to the repo's own
// DAINTREE.md would run the job against a DIFFERENT brief than the caller named and hide
// the typo behind a successful-looking run.
func TestProjectInstructionsFileFailuresAreFatal(t *testing.T) {
	dir := t.TempDir()
	for name, path := range map[string]string{
		"missing": filepath.Join(dir, "nope.md"),
		"blank":   writeTemp(t, "blank.md", "\n \n"),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := overridesFromOptions(Options{ProjectInstructionsFile: path})
			if err == nil {
				t.Fatalf("%s must be fatal, not a fallback", name)
			}
			if !strings.Contains(err.Error(), "--project-instructions-file") {
				t.Errorf("error should name the flag, got: %v", err)
			}
		})
	}
}

// TestProjectInstructionsFileMaySymlink: the implicit loader refuses a symlink because
// the bound PROJECT is untrusted and could point DAINTREE.md at a secret. A path from
// argv carries the same trust as the environment it shadows, so the flag follows it.
func TestProjectInstructionsFileMaySymlink(t *testing.T) {
	dir := t.TempDir()
	real := writeTemp(t, "real.md", "# Synthetic brief")
	link := filepath.Join(dir, "link.md")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	got, err := readProjectInstructionsFile(link)
	if err != nil {
		t.Fatalf("an explicitly named symlink must be followed: %v", err)
	}
	if got != "# Synthetic brief" {
		t.Errorf("content = %q", got)
	}
}

// TestBuildOverridesProjectInstructionsPrecedence is the trap this feature is most
// likely to fall into: buildOverrides auto-loads DAINTREE.md AFTER the flags are mapped,
// so without the nil guard the repo's own file silently wins and the flag does nothing.
func TestBuildOverridesProjectInstructionsPrecedence(t *testing.T) {
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, projectinstructions.Filename),
		[]byte("# From the repo"), 0o600); err != nil {
		t.Fatal(err)
	}
	flagFile := writeTemp(t, "brief.md", "# From the flag")

	o, err := buildOverrides(Options{Project: project, ProjectInstructionsFile: flagFile}, discardRenderer())
	if err != nil {
		t.Fatalf("buildOverrides() error = %v", err)
	}
	if o.ProjectInstructions == nil || *o.ProjectInstructions != "# From the flag" {
		t.Errorf("the explicit file must win over DAINTREE.md, got %v", o.ProjectInstructions)
	}

	// Without the flag the auto-load must still happen — the guard must not disable it.
	o, err = buildOverrides(Options{Project: project}, discardRenderer())
	if err != nil {
		t.Fatalf("buildOverrides() error = %v", err)
	}
	if o.ProjectInstructions == nil || *o.ProjectInstructions != "# From the repo" {
		t.Errorf("DAINTREE.md must still auto-load when no flag is given, got %v", o.ProjectInstructions)
	}
}

// TestApplyAutoProjectInstructionsNeverClobbers pins the shared guard directly, because
// it is what keeps the host's per-boot descriptor from replacing an explicit brief too.
func TestApplyAutoProjectInstructionsNeverClobbers(t *testing.T) {
	explicit := "# Explicit"
	o := config.ConfigOverrides{ProjectInstructions: &explicit}
	applyAutoProjectInstructions(&o, "# Discovered")
	if *o.ProjectInstructions != "# Explicit" {
		t.Errorf("discovered content clobbered an explicit override: %q", *o.ProjectInstructions)
	}

	var empty config.ConfigOverrides
	applyAutoProjectInstructions(&empty, "# Discovered")
	if empty.ProjectInstructions == nil || *empty.ProjectInstructions != "# Discovered" {
		t.Errorf("a nil override must accept discovered content, got %v", empty.ProjectInstructions)
	}

	var none config.ConfigOverrides
	applyAutoProjectInstructions(&none, "")
	if none.ProjectInstructions != nil {
		t.Errorf("empty discovered content must leave the override nil, got %q", *none.ProjectInstructions)
	}
}
