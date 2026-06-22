// Package config resolves the assistant's runtime configuration with the
// trusted-env security boundary. Spec: docs/port/domain-config.md §1.
//
// Resolution order (highest priority first):
//  1. explicit overrides (CLI flags)
//  2. real process environment (incl. what Daintree injects on launch)
//  3. project-root .env (<projectPath>/.env)
//  4. the assistant's own package .env (gap-filling fallback)
//  5. built-in DEFAULTS
//
// godotenv.Load never overrides an already-set var, so real env (2) always beats
// either .env file; the project .env is loaded before the own .env, and since it
// doesn't override, the project .env wins over the own .env.
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

// DEFAULTS holds the built-in default values (domain-config.md §1.3). These are
// the CODE defaults (glm-5p2 / deepseek-v4-flash), NOT the CLAUDE.md prose.
var DEFAULTS = struct {
	FireworksBaseURL string
	LargeModel       string
	MediumModel      string
	SmallModel       string
	DefaultMcpURL    string
}{
	FireworksBaseURL: "https://api.fireworks.ai/inference/v1",
	LargeModel:       "accounts/fireworks/models/glm-5p2",
	MediumModel:      "accounts/fireworks/models/glm-5p2",
	SmallModel:       "accounts/fireworks/models/deepseek-v4-flash",
	// Declared but NOT applied as a default for mcpUrl; mcpUrl is left empty when
	// unset → degraded local mode.
	DefaultMcpURL: "http://127.0.0.1:45454/mcp",
}

// stateRootSubpath is the per-user state root under the home dir.
var stateRootSubpath = filepath.Join(".daintree", "assistant-cli")

// logDirSubpath is the GLOBAL (not per-project) default log dir under home.
var logDirSubpath = filepath.Join(".daintree", "logs")

// AppConfig is the fully resolved configuration. Optional string fields are ""
// when unset (the TS optionals).
type AppConfig struct {
	ProjectPath string
	StateDir    string
	DBPath      string
	LogDir      string

	FireworksAPIKey  string
	FireworksBaseURL string
	LargeModel       string
	MediumModel      string
	SmallModel       string

	McpURL    string
	McpToken  string
	ProjectID string
	WindowID  string

	Tier        domain.Tier
	AutoApprove bool
	Offline     bool

	ProjectInstructions string
	DebugLog            bool
}

// ConfigOverrides are the explicit (CLI-supplied) overrides. All optional; nil
// pointers mean "not provided". Note: NO MediumModel, FireworksBaseURL override
// (per the spec's ConfigOverrides field list).
type ConfigOverrides struct {
	ProjectPath         *string
	StateDir            *string
	ProjectID           *string
	WindowID            *string
	McpURL              *string
	McpToken            *string
	FireworksAPIKey     *string
	LargeModel          *string
	SmallModel          *string
	Tier                *string
	AutoApprove         *bool
	Offline             *bool
	DebugLog            *bool
	LogDir              *string
	ProjectInstructions *string
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
// depend on this exact algorithm. Spec: domain-config.md §1.7.
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

// env is the resolution context: a trusted snapshot (taken before any .env load)
// and the merged environment (after .env loads).
type env struct {
	trusted map[string]string // os.Environ() snapshot, pre-.env
	// merged is read live via os.Getenv after godotenv.Load mutated the process
	// env (godotenv writes only previously-unset keys).
}

func (e *env) trustedGet(key string) string { return e.trusted[key] }
func (e *env) mergedGet(key string) string  { return os.Getenv(key) }

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

	// 2. Load .env files with no-override semantics (godotenv.Load never
	//    overrides an already-set var). Project .env first, then own .env.
	//    Errors are ignored: a missing .env is the normal case.
	_ = godotenv.Load(filepath.Join(projectPath, ".env"))
	if ownEnv := ownDotenvPath(); ownEnv != "" {
		_ = godotenv.Load(ownEnv)
	}

	cfg := AppConfig{ProjectPath: projectPath}

	// --- merged-env-OK settings ---
	cfg.FireworksAPIKey = FirstString(deref(overrides.FireworksAPIKey), e.mergedGet("FIREWORKS_API_KEY"))
	cfg.FireworksBaseURL = FirstString(e.mergedGet("FIREWORKS_BASE_URL"), DEFAULTS.FireworksBaseURL)
	cfg.LargeModel = FirstString(deref(overrides.LargeModel), e.mergedGet("DAINTREE_LARGE_MODEL"), DEFAULTS.LargeModel)
	cfg.MediumModel = FirstString(e.mergedGet("DAINTREE_MEDIUM_MODEL"), DEFAULTS.MediumModel)
	cfg.SmallModel = FirstString(deref(overrides.SmallModel), e.mergedGet("DAINTREE_SMALL_MODEL"), DEFAULTS.SmallModel)
	cfg.McpURL = FirstString(deref(overrides.McpURL), e.mergedGet("DAINTREE_MCP_URL")) // no default → degraded local mode
	cfg.McpToken = FirstString(deref(overrides.McpToken), e.mergedGet("DAINTREE_MCP_TOKEN"))
	cfg.ProjectID = FirstString(deref(overrides.ProjectID), e.mergedGet("DAINTREE_PROJECT_ID"))
	cfg.WindowID = FirstString(deref(overrides.WindowID), e.mergedGet("DAINTREE_WINDOW_ID"))
	cfg.DebugLog = resolveBool(overrides.DebugLog, e.mergedGet("DAINTREE_ASSISTANT_DEBUG_LOG"))

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

// resolveTier implements the fail-closed tier resolution (domain-config.md §1.5).
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

// DescribeConfig returns a secret-redacted view for /status (domain-config.md
// §1.7). fireworksApiKey and mcpToken are redacted; projectInstructions shown as
// a byte count; mcpUrl notes degraded mode when unset.
func DescribeConfig(cfg AppConfig) map[string]string {
	out := map[string]string{
		"projectPath":      cfg.ProjectPath,
		"stateDir":         cfg.StateDir,
		"dbPath":           cfg.DBPath,
		"logDir":           cfg.LogDir,
		"fireworksApiKey":  redactSecret(cfg.FireworksAPIKey),
		"fireworksBaseUrl": cfg.FireworksBaseURL,
		"largeModel":       cfg.LargeModel,
		"mediumModel":      cfg.MediumModel,
		"smallModel":       cfg.SmallModel,
		"mcpToken":         redactSecret(cfg.McpToken),
		"projectId":        cfg.ProjectID,
		"windowId":         placeholderUnset(cfg.WindowID),
		"tier":             string(cfg.Tier),
		"autoApprove":      strconv.FormatBool(cfg.AutoApprove),
		"offline":          strconv.FormatBool(cfg.Offline),
		"debugLog":         strconv.FormatBool(cfg.DebugLog),
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
// "(unset)" placeholder when it is empty — matches the TS describeConfig contract.
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
