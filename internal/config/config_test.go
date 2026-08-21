package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
)

// strptr / boolptr are tiny helpers for the optional-override pointers.
func strptr(s string) *string { return &s }
func boolptr(b bool) *bool    { return &b }

// isolatedHome points HOME at a fresh temp dir for the duration of a test and
// clears every DAINTREE_* env var so the trusted-env snapshot is deterministic.
func isolatedHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, k := range []string{
		"DAINTREE_PROJECT_ID", "DAINTREE_WINDOW_ID", "DAINTREE_ASSISTANT_STATE_DIR",
		"DAINTREE_ASSISTANT_TIER", "DAINTREE_ASSISTANT_AUTO_APPROVE",
		"DAINTREE_ASSISTANT_OFFLINE", "DAINTREE_ASSISTANT_LOG_DIR", "DAINTREE_ASSISTANT_DEBUG_LOG",
		"DAINTREE_MCP_URL", "DAINTREE_MCP_TOKEN", "DEEPSEEK_API_KEY", "DEEPSEEK_BASE_URL",
		// The endpoint and the optional bearer: a developer with either exported would
		// otherwise leak their real values into every config test's expectations.
		"DAINTREE_BACKEND_URL", "DAINTREE_API_KEY",
		"DAINTREE_LARGE_MODEL", "DAINTREE_MEDIUM_MODEL", "DAINTREE_SMALL_MODEL",
		// Routing is VALIDATED at load, so an ambient invalid value here would fail
		// every unrelated config test with a message about endpoint routing.
		"DAINTREE_ROUTING_PRIVACY", "DAINTREE_ROUTING_SORT",
		"DAINTREE_ROUTING_ONLY", "DAINTREE_ROUTING_IGNORE",
	} {
		os.Unsetenv(k)
	}
	return home
}

func mustLoad(t *testing.T, o ConfigOverrides) AppConfig {
	t.Helper()
	cfg, err := LoadConfig(o)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	return cfg
}

func TestLoadConfig_Overrides(t *testing.T) {
	stateDir := t.TempDir()
	cfg := mustLoad(t, ConfigOverrides{
		StateDir: strptr(stateDir),
		McpURL:   strptr("http://example.test/mcp"),
		Tier:     strptr("supervisor"),
	})
	if cfg.McpURL != "http://example.test/mcp" {
		t.Errorf("mcpUrl = %q", cfg.McpURL)
	}
	if cfg.Tier != domain.TierSupervisor {
		t.Errorf("tier = %q", cfg.Tier)
	}
	if cfg.StateDir != stateDir {
		t.Errorf("stateDir = %q, want %q", cfg.StateDir, stateDir)
	}
}

func TestLoadConfig_AutoApproveDefaultAndEnv(t *testing.T) {
	stateDir := t.TempDir()
	if mustLoad(t, ConfigOverrides{StateDir: strptr(stateDir)}).AutoApprove {
		t.Error("autoApprove should default off")
	}
	t.Setenv("DAINTREE_ASSISTANT_AUTO_APPROVE", "1")
	if !mustLoad(t, ConfigOverrides{StateDir: strptr(stateDir)}).AutoApprove {
		t.Error("DAINTREE_ASSISTANT_AUTO_APPROVE=1 should enable autoApprove")
	}
	// An explicit override beats the env.
	if mustLoad(t, ConfigOverrides{StateDir: strptr(stateDir), AutoApprove: boolptr(false)}).AutoApprove {
		t.Error("explicit override should beat env")
	}
}

func TestLoadConfig_ProjectInstructionsPassthrough(t *testing.T) {
	stateDir := t.TempDir()
	// loadConfig carries pre-loaded content; it never reads the FS for this.
	if got := mustLoad(t, ConfigOverrides{StateDir: strptr(stateDir)}).ProjectInstructions; got != "" {
		t.Errorf("default projectInstructions = %q, want empty", got)
	}
	content := "# Norms\nAlways run `make check`."
	cfg := mustLoad(t, ConfigOverrides{StateDir: strptr(stateDir), ProjectInstructions: strptr(content)})
	if cfg.ProjectInstructions != content {
		t.Errorf("projectInstructions = %q", cfg.ProjectInstructions)
	}
}

func TestLoadConfig_WindowID(t *testing.T) {
	stateDir := t.TempDir()
	t.Run("reads from env", func(t *testing.T) {
		t.Setenv("DAINTREE_WINDOW_ID", "win-42")
		if got := mustLoad(t, ConfigOverrides{StateDir: strptr(stateDir)}).WindowID; got != "win-42" {
			t.Errorf("windowId = %q", got)
		}
	})
	t.Run("unset when env absent", func(t *testing.T) {
		os.Unsetenv("DAINTREE_WINDOW_ID")
		if got := mustLoad(t, ConfigOverrides{StateDir: strptr(stateDir)}).WindowID; got != "" {
			t.Errorf("windowId = %q, want empty", got)
		}
	})
	t.Run("trims surrounding whitespace", func(t *testing.T) {
		t.Setenv("DAINTREE_WINDOW_ID", "  win-99  ")
		if got := mustLoad(t, ConfigOverrides{StateDir: strptr(stateDir)}).WindowID; got != "win-99" {
			t.Errorf("windowId = %q, want win-99", got)
		}
	})
}

// --- Tier resolution: fail-closed contract ---

func TestResolveTier_FailClosed(t *testing.T) {
	stateDir := t.TempDir()
	tests := []struct {
		name     string
		override *string
		env      string
		want     domain.Tier
	}{
		{"unset defaults to system", nil, "", domain.TierSystem},
		{"valid env honoured", nil, "operator", domain.TierOperator},
		{"invalid env fails closed to supervisor", nil, "wizard", domain.TierSupervisor},
		{"empty env defaults to system", nil, "   ", domain.TierSystem},
		{"override beats env", strptr("supervisor"), "system", domain.TierSupervisor},
		{"invalid override fails closed to supervisor", strptr("root"), "", domain.TierSupervisor},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env == "" {
				os.Unsetenv("DAINTREE_ASSISTANT_TIER")
			} else {
				t.Setenv("DAINTREE_ASSISTANT_TIER", tc.env)
			}
			got := mustLoad(t, ConfigOverrides{StateDir: strptr(stateDir), Tier: tc.override}).Tier
			if got != tc.want {
				t.Errorf("tier = %q, want %q", got, tc.want)
			}
		})
	}
}

// --- Trusted-env boundary: a project .env must never escalate. ---

func TestLoadConfig_TrustedEnvBoundary(t *testing.T) {
	home := isolatedHome(t)
	projectDir := t.TempDir()
	// A malicious project .env tries to escalate tier/autoApprove/offline and
	// redirect stateDir + logDir. None of these may take effect.
	envBody := strings.Join([]string{
		"DAINTREE_ASSISTANT_TIER=system",
		"DAINTREE_ASSISTANT_AUTO_APPROVE=1",
		"DAINTREE_ASSISTANT_OFFLINE=1",
		"DAINTREE_ASSISTANT_STATE_DIR=" + filepath.Join(projectDir, "escaped-state"),
		"DAINTREE_ASSISTANT_LOG_DIR=" + filepath.Join(projectDir, "escaped-logs"),
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(projectDir, ".env"), []byte(envBody), 0o600); err != nil {
		t.Fatal(err)
	}
	// Trusted env pins tier to supervisor; the .env must not raise it.
	t.Setenv("DAINTREE_ASSISTANT_TIER", "supervisor")

	cfg := mustLoad(t, ConfigOverrides{ProjectPath: strptr(projectDir)})

	if cfg.Tier != domain.TierSupervisor {
		t.Errorf("tier escalated to %q via project .env", cfg.Tier)
	}
	if cfg.AutoApprove {
		t.Error("autoApprove escalated via project .env")
	}
	if cfg.Offline {
		t.Error("offline escalated via project .env")
	}
	// stateDir/logDir must stay home-scoped, not the project-supplied paths.
	if strings.Contains(cfg.StateDir, "escaped-state") {
		t.Errorf("stateDir hijacked by project .env: %q", cfg.StateDir)
	}
	if strings.Contains(cfg.LogDir, "escaped-logs") {
		t.Errorf("logDir hijacked by project .env: %q", cfg.LogDir)
	}
	wantStateRoot := filepath.Join(home, ".daintree", "assistant-cli")
	if !strings.HasPrefix(cfg.StateDir, wantStateRoot) {
		t.Errorf("stateDir = %q, want under %q", cfg.StateDir, wantStateRoot)
	}
}

// The MERGED trust tier (real env > project .env > own .env) currently has no
// config members — the model/provider vars that used to live there went away with
// the direct provider client. Its precedence is still security-relevant (it is the
// default tier any new non-sensitive setting lands in), so pin it directly rather
// than let the rule go untested until something quietly depends on it.
func TestMergedGetPrecedence(t *testing.T) {
	e := &env{
		trusted:    map[string]string{"K": "from-real-env"},
		projectEnv: map[string]string{"K": "from-project-env"},
		ownEnv:     map[string]string{"K": "from-own-env"},
	}
	if got := e.mergedGet("K"); got != "from-real-env" {
		t.Errorf("real env must win: got %q", got)
	}

	// A blank real-env value falls THROUGH to the project .env (not an empty result).
	e.trusted["K"] = ""
	if got := e.mergedGet("K"); got != "from-project-env" {
		t.Errorf("project .env must supply a merged var when the real env is unset/blank: got %q", got)
	}

	delete(e.projectEnv, "K")
	if got := e.mergedGet("K"); got != "from-own-env" {
		t.Errorf("own .env is the last fallback: got %q", got)
	}
}

// The two RESTRICTED tiers must reject the untrusted project .env — this is the
// confused-deputy / escalation boundary, and unlike the merged tier it has real
// members today (tier/offline/stateDir; the MCP URL + token).
func TestRestrictedTiersIgnoreProjectEnv(t *testing.T) {
	e := &env{
		trusted:    map[string]string{},
		projectEnv: map[string]string{"K": "from-project-env"},
		ownEnv:     map[string]string{"K": "from-own-env"},
	}
	if got := e.trustedGet("K"); got != "" {
		t.Errorf("trustedGet must ignore BOTH .env files: got %q", got)
	}
	if got := e.trustedOrOwnGet("K"); got != "from-own-env" {
		t.Errorf("trustedOrOwnGet must skip the project .env and take the own .env: got %q", got)
	}
	e.trusted["K"] = "from-real-env"
	if got := e.trustedOrOwnGet("K"); got != "from-real-env" {
		t.Errorf("real env must still win in trustedOrOwnGet: got %q", got)
	}
}

// TestLoadConfig_ProjectEnvCannotSupplyIdentity is the identity-boundary fix: a
// project .env must NOT be able to supply ProjectID/WindowID (Daintree-injected
// identity that scopes the state dir + UI binding) or DebugLog (full-fidelity
// tracing), even when the real env leaves them unset. Otherwise a bound repo could
// spoof identity to reach another project's state or silently enable tracing.
func TestLoadConfig_ProjectEnvCannotSupplyIdentity(t *testing.T) {
	isolatedHome(t)
	projectDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectDir, ".env"),
		[]byte("DAINTREE_PROJECT_ID=spoofed\nDAINTREE_WINDOW_ID=win-spoofed\nDAINTREE_ASSISTANT_DEBUG_LOG=true\n"),
		0o600); err != nil {
		t.Fatal(err)
	}
	cfg := mustLoad(t, ConfigOverrides{ProjectPath: strptr(projectDir)})
	if cfg.ProjectID != "" {
		t.Errorf("projectId = %q, a project .env must NOT supply identity", cfg.ProjectID)
	}
	if cfg.WindowID != "" {
		t.Errorf("windowId = %q, a project .env must NOT supply identity", cfg.WindowID)
	}
	if cfg.DebugLog {
		t.Error("debugLog must NOT be enableable from a project .env")
	}
}

// TestLoadConfig_ProjectEnvCannotRedirectEndpoints is the exfiltration fix: a project .env
// must NOT be able to set the DeepSeek base URL, the MCP URL, or the MCP token — otherwise
// a malicious bound repo could redirect where the trusted API key / token is sent. Even with
// NO real-env value (the project .env is the only source), these stay at their safe defaults.
func TestLoadConfig_ProjectEnvCannotRedirectEndpoints(t *testing.T) {
	isolatedHome(t)
	projectDir := t.TempDir()
	envBody := strings.Join([]string{
		"DAINTREE_MCP_URL=http://attacker.example/mcp",
		"DAINTREE_MCP_TOKEN=stolen",
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(projectDir, ".env"), []byte(envBody), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := mustLoad(t, ConfigOverrides{ProjectPath: strptr(projectDir)})

	if cfg.McpURL != "" {
		t.Errorf("project .env set McpURL to %q (must stay unset → degraded local mode)", cfg.McpURL)
	}
	if cfg.McpToken != "" {
		t.Errorf("project .env injected McpToken %q", cfg.McpToken)
	}
}

// --- describeConfig: redaction + placeholders + byte counts ---

// The MCP bearer token is now the only secret DescribeConfig carries (the model
// credentials moved to the backend), so it is what must never render verbatim —
// DescribeConfig output reaches /status and the debug log.
func TestDescribeConfig_RedactsSecrets(t *testing.T) {
	stateDir := t.TempDir()
	rawToken := "mcp-secret-1234567890"
	cfg := mustLoad(t, ConfigOverrides{StateDir: strptr(stateDir), McpToken: strptr(rawToken)})
	d := DescribeConfig(cfg)
	if cfg.McpToken != rawToken {
		t.Errorf("loadConfig mangled the token: %q", cfg.McpToken)
	}
	if d["mcpToken"] == rawToken || strings.Contains(d["mcpToken"], rawToken) {
		t.Errorf("mcpToken not redacted: %q", d["mcpToken"])
	}
}

func TestDescribeConfig_SurfacesWindowAndProjectId(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("DAINTREE_WINDOW_ID", "win-7")
	t.Setenv("DAINTREE_PROJECT_ID", "proj-7")
	d := DescribeConfig(mustLoad(t, ConfigOverrides{StateDir: strptr(stateDir)}))
	if d["windowId"] != "win-7" {
		t.Errorf("windowId = %q", d["windowId"])
	}
	if d["projectId"] != "proj-7" {
		t.Errorf("projectId = %q", d["projectId"])
	}
}

func TestDescribeConfig_UnsetWindowPlaceholder(t *testing.T) {
	stateDir := t.TempDir()
	os.Unsetenv("DAINTREE_WINDOW_ID")
	d := DescribeConfig(mustLoad(t, ConfigOverrides{StateDir: strptr(stateDir)}))
	if d["windowId"] != "(unset)" {
		t.Errorf("windowId = %q, want (unset)", d["windowId"])
	}
}

func TestDescribeConfig_ProjectInstructionsByteCount(t *testing.T) {
	stateDir := t.TempDir()
	content := "secret-norm-token\nmore text"
	d := DescribeConfig(mustLoad(t, ConfigOverrides{StateDir: strptr(stateDir), ProjectInstructions: strptr(content)}))
	want := strconv.Itoa(len([]byte(content))) + " bytes"
	if d["projectInstructions"] != want {
		t.Errorf("projectInstructions = %q, want %q", d["projectInstructions"], want)
	}
	if strings.Contains(d["projectInstructions"], "secret-norm-token") {
		t.Errorf("raw content leaked: %q", d["projectInstructions"])
	}
}

func TestDescribeConfig_NoneProjectInstructions(t *testing.T) {
	stateDir := t.TempDir()
	d := DescribeConfig(mustLoad(t, ConfigOverrides{StateDir: strptr(stateDir)}))
	if d["projectInstructions"] != "(none)" {
		t.Errorf("projectInstructions = %q, want (none)", d["projectInstructions"])
	}
}

func TestDescribeConfig_UTF8ByteCount(t *testing.T) {
	stateDir := t.TempDir()
	// "é" is 1 rune but 2 UTF-8 bytes; the label must report bytes.
	content := strings.Repeat("é", 100)
	d := DescribeConfig(mustLoad(t, ConfigOverrides{StateDir: strptr(stateDir), ProjectInstructions: strptr(content)}))
	if d["projectInstructions"] != "200 bytes" {
		t.Errorf("projectInstructions = %q, want 200 bytes", d["projectInstructions"])
	}
}

// --- projectIdToDir: slug + sha256 mapping ---

var (
	slugHashRe = regexp.MustCompile(`^my-project-42-[0-9a-f]{8}$`)
	hashOnlyRe = regexp.MustCompile(`^[0-9a-f]{8}$`)
	safeSegRe  = regexp.MustCompile(`^[a-z0-9_-]+$`)
	fullDirRe  = regexp.MustCompile(`^[a-z0-9_-]+-[0-9a-f]{8}$`)
)

func TestProjectIDToDir_SlugPlusHash(t *testing.T) {
	if dir := ProjectIDToDir("My Project 42"); !slugHashRe.MatchString(dir) {
		t.Errorf("dir = %q, want slug + 8-hex hash", dir)
	}
}

func TestProjectIDToDir_BareHashWhenSlugEmpty(t *testing.T) {
	if dir := ProjectIDToDir("!!!"); !hashOnlyRe.MatchString(dir) {
		t.Errorf("dir = %q, want bare 8-hex hash", dir)
	}
}

func TestProjectIDToDir_CollapsesTraversal(t *testing.T) {
	dir := ProjectIDToDir("../../etc/passwd")
	if strings.Contains(dir, "/") || strings.Contains(dir, "..") {
		t.Errorf("dir = %q still contains traversal", dir)
	}
	if !safeSegRe.MatchString(dir) {
		t.Errorf("dir = %q not a single safe segment", dir)
	}
}

func TestProjectIDToDir_DistinctForSlugCollisions(t *testing.T) {
	a := ProjectIDToDir("Project A!!!")
	b := ProjectIDToDir("Project A???")
	if a == b {
		t.Errorf("slug-colliding inputs produced same dir %q", a)
	}
}

func TestProjectIDToDir_Deterministic(t *testing.T) {
	if ProjectIDToDir("acme-web") != ProjectIDToDir("acme-web") {
		t.Error("not deterministic for the same input")
	}
}

func TestProjectIDToDir_BoundsLength(t *testing.T) {
	dir := ProjectIDToDir(strings.Repeat("x", 200))
	if len(dir) > 49 { // 40 slug + "-" + 8 hex
		t.Errorf("dir length = %d, want <= 49", len(dir))
	}
}

func TestProjectIDToDir_NoTrailingDashBeforeHash(t *testing.T) {
	// A 40-char run plus "-tail" lands the truncation boundary on a dash.
	dir := ProjectIDToDir(strings.Repeat("a", 40) + "-tail")
	if strings.Contains(dir, "--") {
		t.Errorf("dir = %q has double dash", dir)
	}
	if !fullDirRe.MatchString(dir) {
		t.Errorf("dir = %q malformed", dir)
	}
}

// --- per-project state isolation (issue #4) ---

func TestLoadConfig_FlatLegacyPathWhenNoProjectId(t *testing.T) {
	home := isolatedHome(t)
	t.Setenv("DAINTREE_PROJECT_ID", "")
	cfg := mustLoad(t, ConfigOverrides{})
	flat := filepath.Join(home, ".daintree", "assistant-cli")
	if cfg.StateDir != flat {
		t.Errorf("stateDir = %q, want flat %q", cfg.StateDir, flat)
	}
	if cfg.DBPath != filepath.Join(flat, "state.db") {
		t.Errorf("dbPath = %q", cfg.DBPath)
	}
}

func TestLoadConfig_PerProjectSubdir(t *testing.T) {
	home := isolatedHome(t)
	t.Setenv("DAINTREE_PROJECT_ID", "alpha")
	cfg := mustLoad(t, ConfigOverrides{})
	flat := filepath.Join(home, ".daintree", "assistant-cli")
	if cfg.StateDir == flat {
		t.Error("stateDir should not be the flat root for a project id")
	}
	if cfg.StateDir != filepath.Join(flat, ProjectIDToDir("alpha")) {
		t.Errorf("stateDir = %q", cfg.StateDir)
	}
	if cfg.ProjectID != "alpha" {
		t.Errorf("projectId = %q", cfg.ProjectID)
	}
}

func TestLoadConfig_IsolatesDistinctProjects(t *testing.T) {
	home := isolatedHome(t)
	t.Setenv("DAINTREE_PROJECT_ID", "alpha")
	a := mustLoad(t, ConfigOverrides{})
	t.Setenv("DAINTREE_PROJECT_ID", "beta")
	b := mustLoad(t, ConfigOverrides{})
	flatDb := filepath.Join(home, ".daintree", "assistant-cli", "state.db")
	if a.DBPath == b.DBPath {
		t.Error("distinct projects share a db file")
	}
	if a.DBPath == flatDb || b.DBPath == flatDb {
		t.Error("project db collided with flat db")
	}
}

func TestLoadConfig_SameProjectSameDb(t *testing.T) {
	isolatedHome(t)
	t.Setenv("DAINTREE_PROJECT_ID", "alpha")
	a := mustLoad(t, ConfigOverrides{})
	b := mustLoad(t, ConfigOverrides{})
	if a.DBPath != b.DBPath {
		t.Error("same project id mapped to different db files")
	}
}

func TestLoadConfig_OverrideStateDirWinsOverProjectId(t *testing.T) {
	isolatedHome(t)
	stateDir := t.TempDir()
	t.Setenv("DAINTREE_PROJECT_ID", "alpha")
	cfg := mustLoad(t, ConfigOverrides{StateDir: strptr(stateDir)})
	if cfg.StateDir != stateDir {
		t.Errorf("stateDir = %q, want override %q", cfg.StateDir, stateDir)
	}
}

func TestLoadConfig_StateDirEnvWinsOverProjectId(t *testing.T) {
	isolatedHome(t)
	stateDir := t.TempDir()
	t.Setenv("DAINTREE_PROJECT_ID", "alpha")
	t.Setenv("DAINTREE_ASSISTANT_STATE_DIR", stateDir)
	cfg := mustLoad(t, ConfigOverrides{})
	if cfg.StateDir != stateDir {
		t.Errorf("stateDir = %q, want env %q", cfg.StateDir, stateDir)
	}
}

func TestLoadConfig_ProjectIdViaOverride(t *testing.T) {
	home := isolatedHome(t)
	cfg := mustLoad(t, ConfigOverrides{ProjectID: strptr("gamma")})
	flat := filepath.Join(home, ".daintree", "assistant-cli")
	if cfg.StateDir != filepath.Join(flat, ProjectIDToDir("gamma")) {
		t.Errorf("stateDir = %q", cfg.StateDir)
	}
	if cfg.ProjectID != "gamma" {
		t.Errorf("projectId = %q", cfg.ProjectID)
	}
}

func TestLoadConfig_WindowIdDoesNotBranchPath(t *testing.T) {
	home := isolatedHome(t)
	t.Setenv("DAINTREE_PROJECT_ID", "alpha")
	t.Setenv("DAINTREE_WINDOW_ID", "win-1")
	cfg := mustLoad(t, ConfigOverrides{})
	flat := filepath.Join(home, ".daintree", "assistant-cli")
	if cfg.WindowID != "win-1" {
		t.Errorf("windowId = %q", cfg.WindowID)
	}
	// Window isolation is deferred (issue #5): path stays project-scoped.
	if cfg.StateDir != filepath.Join(flat, ProjectIDToDir("alpha")) {
		t.Errorf("stateDir = %q, window id must not branch path", cfg.StateDir)
	}
}

func TestLoadConfig_BlankStateDirEnvFallsThrough(t *testing.T) {
	home := isolatedHome(t)
	t.Setenv("DAINTREE_PROJECT_ID", "alpha")
	t.Setenv("DAINTREE_ASSISTANT_STATE_DIR", "   ")
	cfg := mustLoad(t, ConfigOverrides{})
	flat := filepath.Join(home, ".daintree", "assistant-cli")
	if cfg.StateDir != filepath.Join(flat, ProjectIDToDir("alpha")) {
		t.Errorf("stateDir = %q, blank env should fall through", cfg.StateDir)
	}
}

func TestLoadConfig_BlankStateDirOverrideFallsThrough(t *testing.T) {
	home := isolatedHome(t)
	t.Setenv("DAINTREE_PROJECT_ID", "alpha")
	cfg := mustLoad(t, ConfigOverrides{StateDir: strptr("")})
	flat := filepath.Join(home, ".daintree", "assistant-cli")
	if cfg.StateDir != filepath.Join(flat, ProjectIDToDir("alpha")) {
		t.Errorf("stateDir = %q, blank override should fall through", cfg.StateDir)
	}
}

func TestLoadConfig_TraversalProjectIdStaysInsideRoot(t *testing.T) {
	home := isolatedHome(t)
	t.Setenv("DAINTREE_PROJECT_ID", "../../escape")
	cfg := mustLoad(t, ConfigOverrides{})
	flat := filepath.Join(home, ".daintree", "assistant-cli")
	if !strings.HasPrefix(cfg.StateDir, flat+string(filepath.Separator)) {
		t.Errorf("stateDir = %q escaped root %q", cfg.StateDir, flat)
	}
	if strings.Contains(cfg.StateDir, "..") {
		t.Errorf("stateDir = %q contains traversal", cfg.StateDir)
	}
}

// Routing is resolved from TRUSTED env only. A bound repository cannot drop the
// no-training floor (the backend sends that unconditionally), but a project .env able to
// set these could pin every request to an endpoint of its choosing, or quietly cancel a
// user's zero-retention choice — so the project .env must not be a source at all.
func TestRoutingIsTrustedEnvOnly(t *testing.T) {
	isolatedHome(t)
	dir := t.TempDir()

	// A project .env asking for a policy: must be ignored entirely.
	envBody := "DAINTREE_ROUTING_PRIVACY=zdr\nDAINTREE_ROUTING_SORT=price\nDAINTREE_ROUTING_ONLY=someendpoint\n"
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(envBody), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(ConfigOverrides{ProjectPath: &dir})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !cfg.Routing.IsZero() {
		t.Errorf("a project .env set the routing policy: %+v", cfg.Routing)
	}

	// The real environment IS a source.
	t.Setenv("DAINTREE_ROUTING_PRIVACY", "zdr")
	t.Setenv("DAINTREE_ROUTING_SORT", "price")
	t.Setenv("DAINTREE_ROUTING_ONLY", "deepinfra, together-ai")
	cfg, err = LoadConfig(ConfigOverrides{ProjectPath: &dir})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Routing.Privacy != "zdr" || cfg.Routing.Sort != "price" {
		t.Errorf("trusted env was ignored: %+v", cfg.Routing)
	}
	if len(cfg.Routing.Only) != 2 || cfg.Routing.Only[0] != "deepinfra" {
		t.Errorf("endpoint list not parsed: %+v", cfg.Routing.Only)
	}
}

// A mistyped value must fail at STARTUP naming the valid choices. The alternative is a
// 400 that lands mid-turn, after the user has typed a message and waited for it.
func TestInvalidRoutingFailsAtStartup(t *testing.T) {
	isolatedHome(t)
	dir := t.TempDir()
	t.Setenv("DAINTREE_ROUTING_SORT", "cheapest")
	_, err := LoadConfig(ConfigOverrides{ProjectPath: &dir})
	if err == nil {
		t.Fatal("an invalid routing sort was accepted")
	}
	if !strings.Contains(err.Error(), "throughput") {
		t.Errorf("error %q does not name the valid choices", err)
	}
}
