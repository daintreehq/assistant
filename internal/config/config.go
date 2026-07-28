// Package config resolves the assistant's runtime configuration with the
// trusted-env security boundary.
//
// Resolution order (highest priority first):
//  1. explicit overrides (CLI flags)
//  2. real process environment (incl. what Daintree injects on launch)
//  3. project-root .env (<projectPath>/.env) — UNTRUSTED (arbitrary bound repo)
//  4. the assistant's own package .env (gap-filling fallback)
//  5. built-in DEFAULTS
//
// .env files are READ INTO MAPS (godotenv.Read), never loaded into os.Environ, so the
// trusted snapshot stays pristine and no os.Getenv caller can be steered by a project .env.
// THREE trust tiers govern which sources a setting may come from:
//   - trusted-only (trustedGet): tier/autoApprove/offline/stateDir/logDir — real env only;
//     a bound project must never escalate the assistant.
//   - trusted-or-own (trustedOrOwnGet): the endpoint / secret-pairing vars (the MCP
//     URL + token) PLUS the Daintree-injected identity (project id, window id) and
//     the debug-logging toggle — real env or the assistant's own .env, NEVER the project
//     .env, so a malicious repo can't redirect where a trusted key/token is sent
//     (exfiltration), spoof identity (cross-project state access), or silently enable tracing.
//   - merged (mergedGet): everything else — real env > project .env > own .env.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/joho/godotenv"
)

// DEFAULTS holds the built-in default values (the CODE defaults, not the
// CLAUDE.md prose).
//
// There are no model or provider defaults here any more. Model choice, prompt
// assembly, and the upstream credentials all belong to the Daintree backend
// (docs/BACKEND.md); the CLI is a local runtime that never talks to a provider.
var DEFAULTS = struct {
	DefaultMcpURL string
}{
	// Declared but NOT applied as a default for mcpUrl; mcpUrl is left empty when
	// unset → degraded local mode.
	DefaultMcpURL: "http://127.0.0.1:45454/mcp",
}

// stateRootSubpath is the per-user state root under the home dir.
var stateRootSubpath = filepath.Join(".daintree", "assistant-cli")

// logDirSubpath is the GLOBAL (not per-project) default log dir under home.
var logDirSubpath = filepath.Join(".daintree", "logs")

// AppConfig is the fully resolved configuration. Optional string fields are ""
// when unset.
type AppConfig struct {
	ProjectPath string
	StateDir    string
	DBPath      string
	LogDir      string

	McpURL    string
	McpToken  string
	ProjectID string
	WindowID  string

	Tier        domain.Tier
	AutoApprove bool
	Offline     bool

	ProjectInstructions string
	DebugLog            bool

	// WorkflowIntelligence gates the client-owned workflow execution-graph layer
	// (graph tools, dispatch observer, turn-context digests, backend workflow
	// tasks). Rollout flag: the backend must carry the matching TurnContext
	// contract before this is on, so it defaults off.
	WorkflowIntelligence bool
}

// ConfigOverrides are the explicit (CLI-supplied) overrides. All optional; nil
// pointers mean "not provided". There are deliberately no model/provider overrides:
// the CLI holds no model credentials and never talks to a provider — the backend
// owns the model choice, the prompts, and the keys (see docs/BACKEND.md).
type ConfigOverrides struct {
	ProjectPath          *string
	StateDir             *string
	ProjectID            *string
	WindowID             *string
	McpURL               *string
	McpToken             *string
	Tier                 *string
	AutoApprove          *bool
	Offline              *bool
	DebugLog             *bool
	LogDir               *string
	ProjectInstructions  *string
	WorkflowIntelligence *bool
}

// FirstString returns the first argument that is non-empty after TrimSpace,
// returned trimmed; or "" if none qualify. Port of firstString.
func FirstString(vals ...string) string {
	for _, v := range vals {
		if t := strings.TrimSpace(v); t != "" {
			return t
		}
	}
	return ""
}

var (
	slugNonAllowed = regexp.MustCompile(`[^a-z0-9_-]`)
	slugCollapse   = regexp.MustCompile(`-+`)
)

// ProjectIDToDir maps a raw project id to a per-project directory name:
// slug + "-" + sha256(rawId)[:8 hex]. Slug: lowercase, [^a-z0-9_-]→"-", collapse
// repeated "-", strip leading/trailing "-", truncate to 40, strip trailing "-"
// again. If the slug is empty, just the 8-hex hash. WIRE-COMPATIBLE: path names
// depend on this exact algorithm.
func ProjectIDToDir(rawID string) string {
	sum := sha256.Sum256([]byte(rawID))
	hash8 := hex.EncodeToString(sum[:])[:8]

	slug := strings.ToLower(rawID)
	slug = slugNonAllowed.ReplaceAllString(slug, "-")
	slug = slugCollapse.ReplaceAllString(slug, "-")
	slug = strings.Trim(slug, "-")
	if len(slug) > 40 {
		slug = slug[:40]
	}
	slug = strings.TrimRight(slug, "-")

	if slug == "" {
		return hash8
	}
	return slug + "-" + hash8
}

// env is the resolution context. All THREE sources are read into MAPS (godotenv.Read,
// never godotenv.Load) so loading a project .env can NEVER mutate the real process env —
// which would otherwise (a) pollute a later trusted snapshot and (b) let any os.Getenv
// caller (e.g. the skills-dir override) silently read a project-controlled value.
type env struct {
	trusted    map[string]string // os.Environ() snapshot — real, injected-by-Daintree env
	projectEnv map[string]string // <projectPath>/.env (UNTRUSTED — arbitrary bound repo)
	ownEnv     map[string]string // the assistant's own package .env (trusted-adjacent)
}

// trustedGet reads ONLY the real-env snapshot (never any .env): tier/autoApprove/offline/
// stateDir/logDir — a bound project must never be able to escalate the assistant.
func (e *env) trustedGet(key string) string { return e.trusted[key] }

// mergedGet is the normal precedence (real env > project .env > own .env), skipping blanks
// so an empty real-env var correctly falls through to a .env value.
//
// NOTE: this tier currently has NO members. Every setting is either trusted-only or
// trusted-or-own, because the last merged settings were the model/provider vars
// (DEEPSEEK_API_KEY, DAINTREE_{LARGE,MEDIUM,SMALL}_MODEL) and those went away with
// the direct provider client — the backend owns model choice and credentials now.
// It is kept as the DEFAULT tier: a new setting that is neither an escalation vector
// nor a secret-pairing endpoint belongs here, and a project .env may legitimately
// supply it. Its precedence is pinned by TestMergedGetPrecedence.
func (e *env) mergedGet(key string) string {
	return FirstString(e.trusted[key], e.projectEnv[key], e.ownEnv[key])
}

// trustedOrOwnGet EXCLUDES the untrusted project .env (real env > own .env). It governs the
// endpoint / secret-pairing vars (the MCP URL + token) so a malicious repo's
// .env can NEVER redirect where a TRUSTED API key / token is sent — a confused-deputy
// exfiltration. A custom endpoint must be set in the real env or the assistant's own .env.
func (e *env) trustedOrOwnGet(key string) string {
	return FirstString(e.trusted[key], e.ownEnv[key])
}

// LoadConfig resolves the configuration. It snapshots os.Environ() into the
// trusted set BEFORE loading any .env, then loads the project .env and the
// assistant's own .env with no-override semantics. Tier/autoApprove/offline/
// stateDir/logDir are read ONLY from the trusted snapshot or an explicit
// override — a bound project's .env must never escalate the assistant.
func LoadConfig(overrides ConfigOverrides) (AppConfig, error) {
	// 1. Snapshot the trusted environment BEFORE any .env mutates os.Environ.
	//    Rationale: the bound project is arbitrary/untrusted code; a repo-local
	//    .env must not be able to silently escalate the assistant.
	e := &env{trusted: snapshotEnv()}

	projectPath := derefOr(overrides.ProjectPath, "")
	if projectPath == "" {
		if cwd, err := os.Getwd(); err == nil {
			projectPath = cwd
		}
	}
	absProject, err := filepath.Abs(projectPath)
	if err != nil {
		return AppConfig{}, fmt.Errorf("resolve project path: %w", err)
	}
	projectPath = absProject

	// 2. READ the .env files into maps (godotenv.Read — NOT Load). Read never touches
	//    os.Environ, so the trusted snapshot above stays pristine and project-controlled
	//    values can't leak to os.Getenv callers. A missing .env yields an error → nil map
	//    (indexing a nil map is "", so resolution just falls through). Precedence is
	//    applied per-key by mergedGet / trustedOrOwnGet, not by load order.
	if m, err := godotenv.Read(filepath.Join(projectPath, ".env")); err == nil {
		e.projectEnv = m
	}
	if ownEnv := ownDotenvPath(); ownEnv != "" {
		if m, err := godotenv.Read(ownEnv); err == nil {
			e.ownEnv = m
		}
	}

	cfg := AppConfig{ProjectPath: projectPath}

	// --- merged-env-OK settings ---
	// DEEPSEEK_BASE_URL is trustedOrOwn (NOT mergedGet): the API key is a trusted secret,
	// so a project .env must never redirect where it is sent (exfiltration).
	// The MCP endpoint + token are the Daintree connection credentials (injected by Daintree
	// into the real env). They are trustedOrOwn so a project .env can't redirect the link or
	// inject a token: no default → degraded local mode when genuinely unset.
	cfg.McpURL = FirstString(deref(overrides.McpURL), e.trustedOrOwnGet("DAINTREE_MCP_URL"))
	cfg.McpToken = FirstString(deref(overrides.McpToken), e.trustedOrOwnGet("DAINTREE_MCP_TOKEN"))
	// ProjectID + WindowID are the Daintree-injected IDENTITY (they scope StateDir
	// and the UI binding) and DebugLog gates full-fidelity tracing — all trustedOrOwn
	// (real env or the assistant's OWN .env), NEVER the project .env. A bound repo
	// must not be able to spoof identity (cross-project state access) or silently
	// enable tracing by planting a .env.
	cfg.ProjectID = FirstString(deref(overrides.ProjectID), e.trustedOrOwnGet("DAINTREE_PROJECT_ID"))
	cfg.WindowID = FirstString(deref(overrides.WindowID), e.trustedOrOwnGet("DAINTREE_WINDOW_ID"))
	cfg.DebugLog = resolveBool(overrides.DebugLog, e.trustedOrOwnGet("DAINTREE_ASSISTANT_DEBUG_LOG"))
	// The workflow-intelligence rollout flag is trustedOrOwn like DebugLog: a
	// bound project's .env must not be able to flip a feature that changes what
	// the backend is sent (workflow_state would 422 on a backend without the
	// matching contract).
	cfg.WorkflowIntelligence = resolveBool(overrides.WorkflowIntelligence, e.trustedOrOwnGet("DAINTREE_WORKFLOW_INTELLIGENCE"))

	// --- trusted-only settings (NEVER from a loaded .env) ---
	cfg.Tier = resolveTier(overrides.Tier, e.trustedGet("DAINTREE_ASSISTANT_TIER"))
	cfg.AutoApprove = resolveBool(overrides.AutoApprove, e.trustedGet("DAINTREE_ASSISTANT_AUTO_APPROVE"))
	cfg.Offline = resolveBool(overrides.Offline, e.trustedGet("DAINTREE_ASSISTANT_OFFLINE"))

	// stateDir (trusted/override → per-project subdir → flat root).
	home, _ := os.UserHomeDir()
	stateRoot := filepath.Join(home, stateRootSubpath)
	cfg.StateDir = FirstString(deref(overrides.StateDir), e.trustedGet("DAINTREE_ASSISTANT_STATE_DIR"))
	if cfg.StateDir == "" {
		if cfg.ProjectID != "" {
			cfg.StateDir = filepath.Join(stateRoot, ProjectIDToDir(cfg.ProjectID))
		} else {
			cfg.StateDir = stateRoot
		}
	}
	// 0700: the state dir holds conversations, the audit trail, automation grants, and
	// memories — owner-only, never world/group readable (mirrors the debug-log dir perms).
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return AppConfig{}, fmt.Errorf("create state dir: %w", err)
	}
	cfg.DBPath = filepath.Join(cfg.StateDir, "state.db")

	// logDir (trusted/override → ~/.daintree/logs); always absolute. GLOBAL.
	logDir := FirstString(deref(overrides.LogDir), e.trustedGet("DAINTREE_ASSISTANT_LOG_DIR"))
	if logDir == "" {
		logDir = filepath.Join(home, logDirSubpath)
	}
	if abs, err := filepath.Abs(logDir); err == nil {
		logDir = abs
	}
	cfg.LogDir = logDir

	// projectInstructions: override only; loadConfig never reads the FS for this.
	cfg.ProjectInstructions = deref(overrides.ProjectInstructions)

	return cfg, nil
}

// resolveTier implements the fail-closed tier resolution.
// Default when unset is system; an explicitly-set INVALID value falls back to the
// least-privileged supervisor, never silently to system.
func resolveTier(override *string, trusted string) domain.Tier {
	raw := FirstString(deref(override), trusted)
	if raw == "" {
		return domain.TierSystem
	}
	t := domain.Tier(raw)
	if t.IsValid() {
		return t
	}
	return domain.TierSupervisor
}

// resolveBool returns the override when set, else (envValue == "1").
func resolveBool(override *bool, envValue string) bool {
	if override != nil {
		return *override
	}
	return strings.TrimSpace(envValue) == "1"
}

// snapshotEnv copies the current process environment into a map.
func snapshotEnv() map[string]string {
	pairs := os.Environ()
	m := make(map[string]string, len(pairs))
	for _, kv := range pairs {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			m[kv[:i]] = kv[i+1:]
		}
	}
	return m
}

// ownDotenvPath returns the assistant's own .env path (next to the executable),
// or "" if it cannot be determined. Lowest-precedence fallback.
func ownDotenvPath() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Join(filepath.Dir(exe), ".env")
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func derefOr(p *string, fallback string) string {
	if p == nil {
		return fallback
	}
	return *p
}

// DescribeConfig returns a secret-redacted view for /status.
// deepseekApiKey and mcpToken are redacted; projectInstructions shown as
// a byte count; mcpUrl notes degraded mode when unset.
func DescribeConfig(cfg AppConfig) map[string]string {
	out := map[string]string{
		"projectPath":          cfg.ProjectPath,
		"stateDir":             cfg.StateDir,
		"dbPath":               cfg.DBPath,
		"logDir":               cfg.LogDir,
		"mcpToken":             redactSecret(cfg.McpToken),
		"projectId":            cfg.ProjectID,
		"windowId":             placeholderUnset(cfg.WindowID),
		"tier":                 string(cfg.Tier),
		"autoApprove":          strconv.FormatBool(cfg.AutoApprove),
		"offline":              strconv.FormatBool(cfg.Offline),
		"debugLog":             strconv.FormatBool(cfg.DebugLog),
		"workflowIntelligence": strconv.FormatBool(cfg.WorkflowIntelligence),
	}
	if cfg.McpURL == "" {
		out["mcpUrl"] = "(unset → degraded local mode)"
	} else {
		out["mcpUrl"] = cfg.McpURL
	}
	if cfg.ProjectInstructions == "" {
		out["projectInstructions"] = "(none)"
	} else {
		out["projectInstructions"] = fmt.Sprintf("%d bytes", len([]byte(cfg.ProjectInstructions)))
	}
	return out
}

// placeholderUnset surfaces a Daintree-injected (non-secret) value, or the
// "(unset)" placeholder when it is empty.
func placeholderUnset(s string) string {
	if s == "" {
		return "(unset)"
	}
	return s
}

// redactSecret renders a secret as "<first4>…<last2> (<len>)" or "(unset)".
func redactSecret(s string) string {
	if s == "" {
		return "(unset)"
	}
	r := []rune(s)
	if len(r) <= 6 {
		// Avoid revealing the whole short secret; still show the length.
		return fmt.Sprintf("…(%d)", len(r))
	}
	return fmt.Sprintf("%s…%s (%d)", string(r[:4]), string(r[len(r)-2:]), len(r))
}
