package cli

import (
	"archive/zip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/config"
	"github.com/daintreehq/assistant/internal/redact"
)

// bundleFixture builds a config carrying planted credentials in every field the bundle
// reads, so a leak has somewhere to come from.
func bundleFixture(t *testing.T) (config.AppConfig, string, string) {
	t.Helper()
	const apiKey = "sk-or-v1-plantedbundlekey0123456789"
	const mcpToken = "an-opaque-mcp-token-matching-no-shape-at-all"
	redact.ResetSecretsForTest()
	t.Cleanup(redact.ResetSecretsForTest)
	redact.RegisterSecret(mcpToken)

	dir := t.TempDir()
	return config.AppConfig{
		StateDir:            dir,
		DBPath:              filepath.Join(dir, "state.db"),
		LogDir:              filepath.Join(dir, "logs"),
		ProjectPath:         dir,
		APIKey:              apiKey,
		McpToken:            mcpToken,
		McpURL:              "http://127.0.0.1:45454/mcp",
		Tier:                "system",
		ProjectInstructions: "internal project instructions that must not be copied",
	}, apiKey, mcpToken
}

// The whole point of the command. A bundle that can leak a credential is worse than no
// bundle: it carries the authority of "this is the safe one to send".
func TestSupportBundleNeverContainsCredentials(t *testing.T) {
	cfg, apiKey, mcpToken := bundleFixture(t)

	files, err := collectSupportBundle(t.Context(), Options{}, cfg, SupportBundleOptions{})
	if err != nil {
		t.Fatalf("collectSupportBundle: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no files collected")
	}
	for _, f := range files {
		body := string(f.Content)
		if strings.Contains(body, apiKey) {
			t.Errorf("%s leaked the API key", f.Name)
		}
		if strings.Contains(body, mcpToken) {
			t.Errorf("%s leaked the MCP token", f.Name)
		}
	}
}

// The bundle exists so nobody has to send a session log. Its content must not include
// the things a log has — project instructions especially, which are the project's
// business and often its employer's.
func TestSupportBundleExcludesProjectContent(t *testing.T) {
	cfg, _, _ := bundleFixture(t)

	files, err := collectSupportBundle(t.Context(), Options{}, cfg, SupportBundleOptions{})
	if err != nil {
		t.Fatalf("collectSupportBundle: %v", err)
	}
	for _, f := range files {
		if strings.Contains(string(f.Content), "internal project instructions") {
			t.Errorf("%s copied DAINTREE.md's contents — only its size belongs in a bundle", f.Name)
		}
	}
}

// The diagnostic value has to survive the redaction, or the bundle is a well-sealed box
// with nothing useful in it. apiKeyLength is the specific example: it is what tells
// support a key was pasted truncated.
func TestSupportBundleKeepsDiagnosticMetadata(t *testing.T) {
	cfg, apiKey, _ := bundleFixture(t)

	files, err := collectSupportBundle(t.Context(), Options{}, cfg, SupportBundleOptions{})
	if err != nil {
		t.Fatalf("collectSupportBundle: %v", err)
	}
	env := bundleSection(t, files, "environment.json")

	if env["apiKeyPresent"] != true {
		t.Errorf("apiKeyPresent was lost or masked: %v", env["apiKeyPresent"])
	}
	if got, want := env["apiKeyLength"], float64(len(apiKey)); got != want {
		t.Errorf("apiKeyLength = %v, want %v — this is how a truncated paste is diagnosed", got, want)
	}
	if env["tier"] != "system" {
		t.Errorf("tier was lost: %v", env["tier"])
	}
	// AUTO_APPROVE must always be reported: a support report that omits it is missing
	// the most important fact about what the session was allowed to do.
	if _, ok := env["autoApprove"]; !ok {
		t.Error("autoApprove is missing from the bundle")
	}
}

// The bundle carries a scan of itself. That turns "we redact" from a sentence into
// something a reviewer can check in one command — and it must actually be clean.
func TestSupportBundleRedactionReportIsClean(t *testing.T) {
	cfg, _, _ := bundleFixture(t)

	files, err := collectSupportBundle(t.Context(), Options{}, cfg, SupportBundleOptions{})
	if err != nil {
		t.Fatalf("collectSupportBundle: %v", err)
	}
	report := bundleSection(t, files, "redaction-report.json")
	if report["clean"] != true {
		t.Errorf("the bundle's own redaction scan found problems: %v", report["findings"])
	}
}

// Debug logs are listed, never read: the bundle is the thing you send INSTEAD of a
// session log, so copying one in would defeat its entire purpose.
func TestSupportBundleListsDebugLogsWithoutReadingThem(t *testing.T) {
	cfg, _, _ := bundleFixture(t)
	if err := os.MkdirAll(cfg.LogDir, 0o700); err != nil {
		t.Fatal(err)
	}
	const secretLine = "CONVERSATION CONTENT THAT MUST NOT TRAVEL"
	if err := os.WriteFile(filepath.Join(cfg.LogDir, "2026-08-14-ses_abc.log"), []byte(secretLine), 0o600); err != nil {
		t.Fatal(err)
	}

	files, err := collectSupportBundle(t.Context(), Options{}, cfg, SupportBundleOptions{})
	if err != nil {
		t.Fatalf("collectSupportBundle: %v", err)
	}
	for _, f := range files {
		if strings.Contains(string(f.Content), secretLine) {
			t.Fatalf("%s contains debug-log CONTENT", f.Name)
		}
	}
	logs := bundleSection(t, files, "debug-logs.json")
	entries, _ := logs["logs"].([]any)
	if len(entries) != 1 {
		t.Fatalf("want the log listed by name, got %v", logs["logs"])
	}
	if first, _ := entries[0].(map[string]any); first["name"] != "2026-08-14-ses_abc.log" {
		t.Errorf("the log should be listed by name: %v", entries[0])
	}
}

// The audit slice is opt-in, and even then carries names and outcomes only — redaction
// removes credentials, not project detail, so a file path or branch name in an argument
// would survive it.
func TestSupportBundleAuditIsOptInAndArgumentFree(t *testing.T) {
	cfg, _, _ := bundleFixture(t)

	off, err := collectSupportBundle(t.Context(), Options{}, cfg, SupportBundleOptions{})
	if err != nil {
		t.Fatalf("collectSupportBundle: %v", err)
	}
	if hasBundleFile(off, "audit.json") {
		t.Error("audit.json must not be included without --include-audit")
	}

	on, err := collectSupportBundle(t.Context(), Options{}, cfg, SupportBundleOptions{IncludeAudit: true})
	if err != nil {
		t.Fatalf("collectSupportBundle: %v", err)
	}
	if !hasBundleFile(on, "audit.json") {
		t.Fatal("--include-audit should add audit.json")
	}
	body := string(bundleContent(t, on, "audit.json"))
	for _, forbidden := range []string{"argsJson", "resultJson", "\"args\"", "\"result\""} {
		if strings.Contains(body, forbidden) {
			t.Errorf("audit.json carries %s — arguments and results must be excluded", forbidden)
		}
	}
}

// Overwriting a previous bundle would destroy the one the user is in the middle of
// sending.
func TestWriteSupportBundleRefusesToOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bundle.zip")
	files := []bundleFile{{Name: "a.json", Content: []byte("{}")}}

	if err := writeSupportBundle(path, files); err != nil {
		t.Fatalf("first write: %v", err)
	}
	err := writeSupportBundle(path, files)
	if err == nil {
		t.Fatal("a second write to the same path must fail")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("the error should name the fix (--out): %v", err)
	}

	// And the archive must be readable, owner-only.
	info, serr := os.Stat(path)
	if serr != nil {
		t.Fatal(serr)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("bundle mode = %v, want 0600 — it is a diagnostics file, not a public one", info.Mode().Perm())
	}
	zr, zerr := zip.OpenReader(path)
	if zerr != nil {
		t.Fatalf("the archive does not open: %v", zerr)
	}
	defer zr.Close()
	if len(zr.File) != 1 || zr.File[0].Name != "a.json" {
		t.Errorf("unexpected archive contents: %v", zr.File)
	}
}

// The exclusions notice is the informed half of an informed decision: it has to name what
// is NOT there, or a tester cannot tell whether sending it is safe.
func TestSupportBundleExclusionNoticeNamesWhatIsMissing(t *testing.T) {
	off := strings.Join(supportBundleExclusions(SupportBundleOptions{}), " ")
	for _, want := range []string{"conversation", "terminal output", "debug log", "redacted"} {
		if !strings.Contains(off, want) {
			t.Errorf("the notice should mention %q: %s", want, off)
		}
	}
	on := strings.Join(supportBundleExclusions(SupportBundleOptions{IncludeAudit: true}), " ")
	if !strings.Contains(on, "--include-audit") || !strings.Contains(on, "Review") {
		t.Errorf("with audit on, the notice must say what was added and to review it: %s", on)
	}
}

// --- helpers ---

func hasBundleFile(files []bundleFile, name string) bool {
	for _, f := range files {
		if f.Name == name {
			return true
		}
	}
	return false
}

func bundleContent(t *testing.T, files []bundleFile, name string) []byte {
	t.Helper()
	for _, f := range files {
		if f.Name == name {
			return f.Content
		}
	}
	t.Fatalf("%s not in the bundle", name)
	return nil
}

func bundleSection(t *testing.T, files []bundleFile, name string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(bundleContent(t, files, name), &out); err != nil {
		t.Fatalf("%s is not valid JSON: %v", name, err)
	}
	return out
}

// The critical case: Daintree's per-session MCP URL carries its bearer as
// `?session=<token>` (see mcp.SanitizeURL). That token matches no credential SHAPE and
// sits under a field called "url", which is not credential-marked — so neither redaction
// pass would catch it. Endpoints have to be stripped at the SOURCE, never left to a
// downstream scrubber, and this is the test that says so.
func TestSupportBundleStripsTokensFromEndpointURLs(t *testing.T) {
	cfg, _, _ := bundleFixture(t)
	const urlToken = "SUPERSECRETSESSIONBEARER123"
	cfg.McpURL = "https://mcp.example.com/mcp?session=" + urlToken
	cfg.BackendURL = "https://user:" + urlToken + "@backend.example.com"

	files, err := collectSupportBundle(t.Context(), Options{}, cfg, SupportBundleOptions{})
	if err != nil {
		t.Fatalf("collectSupportBundle: %v", err)
	}
	for _, f := range files {
		if strings.Contains(string(f.Content), urlToken) {
			t.Errorf("%s leaked a token embedded in an endpoint URL", f.Name)
		}
	}
	// The HOST must survive — "which endpoint is it talking to" is the diagnostic.
	versions := bundleSection(t, files, "versions.json")
	if ep, _ := versions["backendEndpoint"].(string); !strings.Contains(ep, "backend.example.com") {
		t.Errorf("stripping removed the host too, leaving nothing to diagnose: %q", ep)
	}
}

// A bundle that knows it is unsafe must not exist. Recording the finding inside the
// archive and writing it anyway is the worst outcome: the file carries the authority of
// "the safe one to send", and the warning is buried in an attachment nobody opens.
func TestUnsafeBundleFindingsAreDetected(t *testing.T) {
	clean := []bundleFile{{Name: "a.json", Content: []byte(`{"tool":"fs.read"}`)}}
	if got := redactionFindings(clean); len(got) != 0 {
		t.Errorf("a clean bundle reported findings: %v", got)
	}

	// A credential that got past assembly — the case the gate exists for.
	dirty := []bundleFile{{Name: "b.json", Content: []byte(`{"note":"key is sk-or-v1-abcdefghijklmnop0123456"}`)}}
	got := redactionFindings(dirty)
	if len(got) != 1 {
		t.Fatalf("want one finding, got %v", got)
	}
	if !strings.Contains(got[0], "b.json") {
		t.Errorf("the finding must name the file: %q", got[0])
	}
}

// The report has to be honest about its own limits, or it reads as proof.
func TestRedactionReportStatesItsLimits(t *testing.T) {
	report := redactionReport([]bundleFile{{Name: "a.json", Content: []byte("{}")}})
	limits, _ := report["limits"].(string)
	if !strings.Contains(limits, "NOT proof") {
		t.Errorf("the report must say it is best-effort: %q", limits)
	}
	if report["clean"] != true {
		t.Errorf("a clean input should report clean: %v", report)
	}
}
