package redact

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRepositoryContainsNoCredentials turns this package's own scanner on the working
// tree.
//
// The redactor's job is to stop a credential reaching a durable surface. A credential
// COMMITTED to the repository is that failure at its worst: it is published, it is in the
// history forever, and every test that reads the file uses it. Catching it here — with
// patterns maintained alongside the runtime's, so the two cannot drift into disagreeing
// about what a token looks like — needs no service, licence, or network, and runs on every
// contributor's machine rather than only in CI.
func TestRepositoryContainsNoCredentials(t *testing.T) {
	root := repoRoot(t)

	// Only files whose PURPOSE is to enumerate credential shapes.
	//
	// Deliberately NOT a blanket "_test.go" skip. That was the first version, and it was a
	// hole big enough to drive a real key through: gitleaks' path allowlist covers the
	// same test families, so a genuine credential pasted into any _test.go would have been
	// invisible to BOTH scanners. Test fixtures elsewhere are handled by the marker rule
	// below instead, which lets a fixture through only if it SAYS it is one.
	skipSuffixes := []string{
		filepath.Join("internal", "redact", "redact.go"),      // the patterns themselves
		filepath.Join("internal", "redact", "redact_test.go"), // proves the patterns fire
		filepath.Join("internal", "redact", "repo_scan_test.go"),
		".gitleaks.toml", // the allowlist names the shapes
		".env.example",   // shows the shape of a key to paste
	}

	// Only genuinely non-source trees. `docs` and `benchmarks` were skipped at first and
	// should not have been: both are committed, so a credential in either is published.
	skipDirs := []string{".git", "bin", "worktrees", ".tmp", "node_modules"}

	// A developer's own .env is gitignored and is theirs, not the repository's — it is
	// SUPPOSED to hold a real key. Scanning it would fail this test on every machine that
	// has one, which is the fastest way to get a security test deleted.
	skipNames := map[string]bool{".env": true, ".env.local": true}

	var findings []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return nil
		}
		if d.IsDir() {
			for _, skip := range skipDirs {
				if d.Name() == skip {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if skipNames[rel] {
			return nil
		}
		for _, suffix := range skipSuffixes {
			if strings.HasSuffix(rel, suffix) {
				return nil
			}
		}
		// Text only. A binary would produce noise, not findings — and the extensions
		// listed include the ones a leaked credential actually arrives in.
		switch filepath.Ext(rel) {
		case ".go", ".md", ".yml", ".yaml", ".json", ".toml", ".sh", ".env", ".txt",
			".conf", ".cfg", ".ini", ".pem", ".key", ".py", ".sql", ".ts", ".js", "":
		default:
			return nil
		}
		data, rerr2 := os.ReadFile(path)
		if rerr2 != nil || len(data) > 4<<20 {
			return nil
		}
		// FindLiteralSecrets, not String. String deliberately matches prose that
		// DESCRIBES a credential shape — keyed field names, `NAME=value`, an
		// Authorization header — and this repository is full of such prose because it
		// implements the redactor. A scanner that fires on its own documentation gets
		// switched off, and then it protects nothing. This asks the narrower question:
		// is there a literal, issuer-prefixed token here?
		var real []string
		for _, hit := range FindLiteralSecretsRaw(string(data)) {
			if isObviousFixture(hit) {
				continue
			}
			real = append(real, previewSecret(hit))
		}
		if len(real) > 0 {
			findings = append(findings, rel+" → "+strings.Join(real, ", "))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	if len(findings) > 0 {
		t.Errorf("credential-shaped strings found in the working tree:\n  %s\n\n"+
			"If one of these is a real credential: ROTATE IT FIRST, then remove it — it is in the\n"+
			"git history too, so deleting the line is not enough. If it is a deliberate test\n"+
			"fixture, use an obviously-invented value; do not widen the skip list, which is what\n"+
			"turns a scanner into decoration.", strings.Join(findings, "\n  "))
	}
}

// fixtureMarkers are the words that make a credential-shaped string obviously invented.
//
// This is the rule that replaced a blanket "skip every _test.go", and it is strictly
// better in both directions. A skip list lets a REAL key hide in any test file — and
// gitleaks allowlists the same paths, so nothing would have caught it. Requiring a marker
// means a real key, which never contains the word "fake", still trips the scan; and it
// pushes fixtures toward saying what they are, which a reader benefits from anyway.
var fixtureMarkers = []string{
	"test", "fake", "planted", "example", "dummy", "sample", "placeholder",
	"notreal", "invalid", "bogus", "xxx", "abcdef", "0123456789",
}

// isObviousFixture reports whether a credential-shaped string announces itself as one.
func isObviousFixture(secret string) bool {
	lower := strings.ToLower(secret)
	for _, marker := range fixtureMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// repoRoot walks up from the test's working directory to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 10; i++ {
		if _, serr := os.Stat(filepath.Join(dir, "go.mod")); serr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not locate the module root")
	return ""
}

// A scanner nobody has seen fire is a scanner nobody should trust. Prove it catches a
// real token shape, and that it reports it REDACTED — a scanner that prints its finding
// puts the secret in the CI log, which is the same disclosure by a different route.
func TestFindLiteralSecretsCatchesRealTokenShapes(t *testing.T) {
	for name, body := range map[string]string{
		"openrouter": `key = "sk-or-v1-abcdefghijklmnopqrstuvwxyz0123456789"`,
		"github pat": `token: ghp_abcdefghijklmnopqrstuvwxyz0123456789`,
		"aws":        `AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE`,
		// A realistic body: the pattern requires actual key material, because a document
		// that merely MENTIONS a PEM header is not a leak.
		"pem": "-----BEGIN RSA PRIVATE KEY-----\n" +
			"MIIEpAIBAAKCAQEAxyzQWERTYUIOPasdfghjklZXCVBNM1234567890abcdefghij\n" +
			"-----END RSA PRIVATE KEY-----",
	} {
		t.Run(name, func(t *testing.T) {
			hits := FindLiteralSecrets(body)
			if len(hits) == 0 {
				t.Fatalf("the scan missed a real token shape: %s", body)
			}
			for _, h := range hits {
				if strings.Contains(body, h) {
					t.Errorf("the finding printed the secret verbatim: %q", h)
				}
			}
		})
	}
}

// And that it stays quiet on the prose this repository is full of. A scanner that fires
// on its own documentation gets switched off, and then it protects nothing.
func TestFindLiteralSecretsIgnoresProseAboutSecrets(t *testing.T) {
	for _, body := range []string{
		"the risk-class-and-confirmation matrix",        // contains "sk-class-and-confirmation"
		"send an Authorization: Bearer header",          // describes the shape
		"export API_KEY=<your key here>",                // a placeholder
		`{"api_key": "…"}`,                              // documentation
		"a sk- prefixed key, like OpenAI or OpenRouter", // names the prefix
		"see internal/redact for the pattern list",      // ordinary prose
		// A PEM header on its own is a MENTION, not a key.
		"files starting -----BEGIN PRIVATE KEY----- are refused",
		"DAINTREE_MCP_TOKEN not set", // an error message
	} {
		if hits := FindLiteralSecrets(body); len(hits) > 0 {
			t.Errorf("the scan fired on prose %q: %v", body, hits)
		}
	}
}
