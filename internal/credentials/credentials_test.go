package credentials

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadMissingFileIsNotAnError(t *testing.T) {
	// "Not signed in yet" is the ordinary first-run state, not a failure — every entry
	// point branches on it, so it must never surface as an error.
	got, ok, err := Load(Path(t.TempDir()))
	if err != nil {
		t.Fatalf("missing credentials returned an error: %v", err)
	}
	if ok {
		t.Fatalf("missing credentials reported as present: %+v", got)
	}
}

func TestLoadMalformedFileIsAnError(t *testing.T) {
	// A corrupt file must NOT masquerade as "signed out": that would send the user
	// through a login whose save then fails against the same broken file, with the real
	// cause never named.
	path := Path(t.TempDir())
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(path); err == nil {
		t.Fatal("malformed credentials must return an error")
	}
}

func TestLoadIncompleteFileReadsAsSignedOut(t *testing.T) {
	// Well-formed JSON missing a half is unusable, so it reads as signed out rather
	// than handing a caller a Credentials it would send as an empty bearer token.
	path := Path(t.TempDir())
	if err := os.WriteFile(path, []byte(`{"backend_url":"https://example.test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, ok, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatal("credentials without a key must not report as signed in")
	}
}

func TestSaveRoundTripsAndIsOwnerOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested")
	path := Path(dir)
	want := Credentials{BaseURL: "https://assistant.daintree.org", APIKey: "sk-or-v1-abcdef0123456789"}
	if err := Save(path, want); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, ok, err := Load(path)
	if err != nil || !ok {
		t.Fatalf("load after save: ok=%v err=%v", ok, err)
	}
	if got != want {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}

	// The file funds model calls; group/world readability would leak a spendable
	// secret to anything else running as another user on the machine.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("credentials file mode = %o, want 600", perm)
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Fatalf("credentials dir mode = %o, want 700", perm)
	}
}

func TestSaveLeavesNoTempFileBehind(t *testing.T) {
	// Save writes via a temp file + rename. A leaked temp file would be a second copy
	// of the key sitting in the state dir, unreferenced and unnoticed.
	dir := t.TempDir()
	if err := Save(Path(dir), Credentials{BaseURL: "http://127.0.0.1:8473", APIKey: "sk-test-0123456789"}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("save left a temp file behind: %s", e.Name())
		}
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly the credentials file, got %d entries", len(entries))
	}
}

func TestSaveRejectsIncomplete(t *testing.T) {
	if err := Save(Path(t.TempDir()), Credentials{BaseURL: "https://example.test"}); err == nil {
		t.Fatal("saving credentials without a key must fail")
	}
}

func TestDeleteIsIdempotent(t *testing.T) {
	path := Path(t.TempDir())
	if err := Save(path, Credentials{BaseURL: "https://example.test", APIKey: "sk-test-0123456789"}); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		if err := Delete(path); err != nil {
			t.Fatalf("delete #%d: %v", i+1, err)
		}
	}
}

func TestRedactNeverLeaksTheWholeKey(t *testing.T) {
	const key = "sk-or-v1-0123456789abcdef0123456789abcdef"
	got := Redact(key)
	if strings.Contains(got, key) {
		t.Fatalf("Redact returned the raw key: %q", got)
	}
	if !strings.HasPrefix(got, "sk-or-v1") {
		t.Fatalf("Redact should keep a recognisable prefix, got %q", got)
	}

	// A short key has no safe middle to elide, so it collapses entirely rather than
	// exposing most of itself.
	if got := Redact("sk-short"); got != "****" {
		t.Fatalf("Redact(short) = %q, want ****", got)
	}
	if got := Redact(""); got != "(none)" {
		t.Fatalf("Redact(empty) = %q, want (none)", got)
	}
}

func TestNormalizeBaseURL(t *testing.T) {
	cases := []struct {
		in, want string
		wantErr  bool
	}{
		{in: "https://assistant.daintree.org", want: "https://assistant.daintree.org"},
		// A bare host is what people actually type; assume https rather than reject it.
		{in: "assistant.daintree.org", want: "https://assistant.daintree.org"},
		// A trailing slash would produce "//v1/..." on every request path join.
		{in: "http://127.0.0.1:8473/", want: "http://127.0.0.1:8473"},
		{in: "http://127.0.0.1:8473/?x=1", want: "http://127.0.0.1:8473"},
		{in: "ftp://example.test", wantErr: true},
		{in: "", wantErr: true},
	}
	for _, c := range cases {
		got, err := NormalizeBaseURL(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("NormalizeBaseURL(%q) = %q, want an error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("NormalizeBaseURL(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("NormalizeBaseURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The shape check exists so a mis-paste fails at the point of entry with a readable
// reason instead of coming back later as an opaque 401.
func TestValidateKeyShape(t *testing.T) {
	if err := ValidateKeyShape("sk-or-v1-abcdef"); err != nil {
		t.Fatalf("a normal key must pass: %v", err)
	}
	if err := ValidateKeyShape("sk-or-v1 abcdef"); err == nil {
		t.Fatal("a key containing a space must be rejected")
	}
	if err := ValidateKeyShape("sk-or-v1-“smart”"); err == nil {
		t.Fatal("a key with non-ASCII characters must be rejected")
	}
	if err := ValidateKeyShape("   "); err == nil {
		t.Fatal("a blank key must be rejected")
	}
}

// SECURITY: every request carries the key as a bearer token, so plain HTTP to a remote
// host would put a spendable secret on the wire in cleartext.
func TestNormalizeBaseURLRejectsRemoteHTTP(t *testing.T) {
	for _, raw := range []string{"http://backend.example.com", "http://10.0.0.5:8473"} {
		if got, err := NormalizeBaseURL(raw); err == nil {
			t.Errorf("NormalizeBaseURL(%q) = %q, want a refusal — it would send the key unencrypted", raw, got)
		}
	}
	// Loopback is the exception: local development has no network to eavesdrop on.
	for _, raw := range []string{"http://127.0.0.1:8473", "http://localhost:8473", "http://[::1]:8473"} {
		if _, err := NormalizeBaseURL(raw); err != nil {
			t.Errorf("NormalizeBaseURL(%q) must be allowed for local development: %v", raw, err)
		}
	}
}

// Credentials in the URL are a second, unmanaged copy of a secret that leaks anywhere
// the endpoint is displayed (/auth, doctor, logs).
func TestNormalizeBaseURLRejectsUserinfo(t *testing.T) {
	if got, err := NormalizeBaseURL("https://user:pass@backend.test"); err == nil {
		t.Fatalf("NormalizeBaseURL = %q, want a refusal for embedded credentials", got)
	}
}

// MkdirAll's mode applies only to directories it CREATES, so a pre-existing
// ~/.daintree/assistant-cli — from an older build, an installer, a restored backup, or a
// permissive umask — keeps whatever mode it had. The file is 0600 regardless, but the
// directory mode is defence in depth around something that carries spend authority.
func TestSaveTightensAPreExistingLooseDirectory(t *testing.T) {
	credDir := filepath.Join(t.TempDir(), StateDirName)
	if err := os.MkdirAll(credDir, 0o755); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Defeat any umask so the precondition is genuinely loose.
	if err := os.Chmod(credDir, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	path := filepath.Join(credDir, FileName)
	if err := Save(path, Credentials{BaseURL: "https://example.test", APIKey: "sk-or-v1-fake-test-key"}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	info, err := os.Stat(credDir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("credentials dir is %04o — group/world bits survived the write", perm)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat file: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("credentials file is %04o, want 0600", perm)
	}
}

// Tightening clears group/world bits and PRESERVES the owner's. Seeded at 0577 rather
// than 0500: a mode with no group/world bits returns before the chmod runs, so it would
// pass even with the hardening deleted — the assertion has to reach the masking itself.
func TestSaveTighteningPreservesOwnerBits(t *testing.T) {
	credDir := filepath.Join(t.TempDir(), StateDirName)
	if err := os.MkdirAll(credDir, 0o700); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.Chmod(credDir, 0o577); err != nil { // owner r-x, group/world rwx
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(credDir, 0o700) })

	// Owner has no write bit, so the save itself fails. What matters is the mode left
	// behind: group/world cleared, the owner's own bits untouched (NOT widened to 0700).
	_ = Save(filepath.Join(credDir, FileName),
		Credentials{BaseURL: "https://example.test", APIKey: "sk-or-v1-fake-test-key"})

	info, err := os.Stat(credDir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o500 {
		t.Errorf("credentials dir is %04o, want 0500 (group/world cleared, owner bits unchanged)", perm)
	}
}

// A directory OTHER users can write to is not a permissions nuisance, it is a different
// hazard: they can unlink and replace credentials.json, and with DAINTREE_API_KEY
// supplying the victim's key an attacker who controls only the stored backend_url
// receives every request that key funds. Saving into it and reporting success would be
// the wrong answer, so the save is refused.
func TestSaveRefusesAWorldWritableDirectory(t *testing.T) {
	// Deliberately NOT named StateDirName: the refusal is unconditional, while the
	// silent chmod is restricted to a directory we created. A user-chosen state dir must
	// still be checked.
	credDir := filepath.Join(t.TempDir(), "custom-state")
	if err := os.MkdirAll(credDir, 0o700); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.Chmod(credDir, 0o777); err != nil {
		t.Skipf("cannot set 0777 here: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(credDir, 0o700) })

	path := filepath.Join(credDir, FileName)
	err := Save(path, Credentials{BaseURL: "https://example.test", APIKey: "sk-or-v1-fake-test-key"})
	if err == nil {
		t.Fatal("Save accepted a world-writable credentials directory")
	}
	if !strings.Contains(err.Error(), "writable by other users") {
		t.Errorf("error %q does not name the hazard", err)
	}
	if !strings.Contains(err.Error(), "chmod go-w") {
		t.Errorf("error %q does not tell the user how to fix it", err)
	}
	// And nothing was written — refusing must not leave a key in an unsafe place.
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Error("the credentials file was written despite the refusal")
	}
}

// The state directory is user-configurable, so filepath.Dir(credentialsPath) can be
// anything the user named — including $HOME. Signing in must not silently chmod a
// directory we did not create.
func TestSaveDoesNotChmodADirectoryItDoesNotOwn(t *testing.T) {
	credDir := filepath.Join(t.TempDir(), "my-own-directory")
	if err := os.MkdirAll(credDir, 0o755); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.Chmod(credDir, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	// 0755 is traversable but not writable by others, so the save proceeds — the FILE is
	// 0600, which is what actually protects the key.
	if err := Save(filepath.Join(credDir, FileName),
		Credentials{BaseURL: "https://example.test", APIKey: "sk-or-v1-fake-test-key"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(credDir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o755 {
		t.Errorf("a user-chosen directory was silently chmod'ed from 0755 to %04o", perm)
	}
}

// isHardenableDir must refuse the paths where a silent chmod would be a surprising,
// system-wide side effect of signing in.
func TestIsHardenableDirRefusesSurprisingTargets(t *testing.T) {
	if home, err := os.UserHomeDir(); err == nil {
		if isHardenableDir(home) {
			t.Error("the home directory is hardenable — signing in would chmod it")
		}
	}
	for _, dir := range []string{"/", ".", "", "/tmp", "/Users"} {
		if isHardenableDir(dir) {
			t.Errorf("%q is hardenable", dir)
		}
	}
	if !isHardenableDir(filepath.Join(t.TempDir(), StateDirName)) {
		t.Errorf("our own %q directory is not hardenable", StateDirName)
	}
}
