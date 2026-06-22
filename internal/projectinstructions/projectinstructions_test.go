package projectinstructions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFile writes the DAINTREE.md file into dir.
func writeFile(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, Filename), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoad_AbsentFileReturnsNothing(t *testing.T) {
	res := Load(t.TempDir())
	if res.Content != "" || res.Warning != "" {
		t.Errorf("absent file: content=%q warning=%q, want both empty", res.Content, res.Warning)
	}
}

func TestLoad_PresentTrimsContent(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "\n  # Norms\nUse make check.\n  \n")
	res := Load(dir)
	if res.Content != "# Norms\nUse make check." {
		t.Errorf("content = %q", res.Content)
	}
	if res.Warning != "" {
		t.Errorf("unexpected warning %q", res.Warning)
	}
}

func TestLoad_EmptyFileIsNoInstructions(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "")
	res := Load(dir)
	if res.Content != "" || res.Warning != "" {
		t.Errorf("empty file: content=%q warning=%q", res.Content, res.Warning)
	}
}

func TestLoad_WhitespaceOnlyIsNoInstructions(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "   \n\t\n  ")
	res := Load(dir)
	if res.Content != "" || res.Warning != "" {
		t.Errorf("whitespace file: content=%q warning=%q", res.Content, res.Warning)
	}
}

func TestLoad_DirectoryNotFileSkipped(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, Filename), 0o700); err != nil {
		t.Fatal(err)
	}
	res := Load(dir)
	if res.Content != "" || res.Warning != "" {
		t.Errorf("directory: content=%q warning=%q, want silent skip", res.Content, res.Warning)
	}
}

func TestLoad_ExactlyAtCapLoads(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, strings.Repeat("a", MaxBytes))
	res := Load(dir)
	if len(res.Content) != MaxBytes {
		t.Errorf("content length = %d, want %d", len(res.Content), MaxBytes)
	}
	if res.Warning != "" {
		t.Errorf("at-cap file should not warn: %q", res.Warning)
	}
}

func TestLoad_OverCapSkipsWithWarning(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, strings.Repeat("a", MaxBytes+1))
	res := Load(dir)
	if res.Content != "" {
		t.Errorf("over-cap file should yield no content, got %d bytes", len(res.Content))
	}
	if !strings.Contains(res.Warning, Filename) || !strings.Contains(res.Warning, "limit") {
		t.Errorf("warning = %q, want it to mention %q and 'limit'", res.Warning, Filename)
	}
}

func TestLoad_CapsOnUTF8ByteLengthNotChars(t *testing.T) {
	dir := t.TempDir()
	// Each "é" is 2 UTF-8 bytes; half-the-cap-plus-one runes overruns the byte cap
	// although the rune count is well under it.
	writeFile(t, dir, strings.Repeat("é", MaxBytes/2+1))
	res := Load(dir)
	if res.Content != "" {
		t.Errorf("byte-overrun file should yield no content")
	}
	if !strings.Contains(res.Warning, "limit") {
		t.Errorf("warning = %q, want it to mention 'limit'", res.Warning)
	}
}

func TestLoad_ResolvesAgainstGivenPathNotCwd(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "scoped to this dir")
	other := t.TempDir() // sibling dir with no instruction file
	if got := Load(other).Content; got != "" {
		t.Errorf("sibling dir content = %q, want empty", got)
	}
	if got := Load(dir).Content; got != "scoped to this dir" {
		t.Errorf("scoped content = %q", got)
	}
}

// TestLoad_RejectsSymlink locks the exfiltration fix: a symlinked DAINTREE.md is NOT
// followed (a malicious repo could point it at a secret file to read into the prompt).
func TestLoad_RejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(secret, []byte("TOPSECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(dir, Filename)); err != nil {
		t.Skipf("symlink unsupported on this platform: %v", err)
	}
	res := Load(dir)
	if strings.Contains(res.Content, "TOPSECRET") {
		t.Fatal("a symlinked DAINTREE.md was followed — a linked secret leaked into the prompt")
	}
	if res.Warning == "" {
		t.Error("expected a warning that the symlink was skipped")
	}
}
