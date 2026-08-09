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
