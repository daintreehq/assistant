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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/domain"
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
	// StateRoot is the directory StateDir was derived from — the per-user state root,
	// or the explicitly-named directory when one was given. It equals StateDir except
	// when a project id scoped a subdirectory out of the root.
	//
	// It exists because a caller that wants to CONFINE state has to name the set of
	// directories this process can legitimately produce, not just the one it resolved
	// this time: a session naming a different projectId lands in a sibling under the
	// same root, and an allowlist holding only StateDir would be checked against a path
	// the factory then declines to use. See internal/cli/mcpserve.go.
	StateRoot string
	DBPath    string
	LogDir    string

	McpURL    string
	McpToken  string
	ProjectID string
	WindowID  string

	// BackendURL is the resolved Daintree backend endpoint. Highest wins: a CLI
	// override, then the trusted env var, then the endpoint stored by `/backend`, then
	// the deployed default.
	BackendURL string
	// EndpointPath is where `/backend` persists its choice. PER-USER (the state ROOT,
	// shared across projects) unless the state dir was explicitly overridden, in which
	// case it follows the override so tests and harnesses stay isolated from a
	// developer's real preference.
	EndpointPath string
	// EndpointLoadError is non-nil when a stored endpoint EXISTS but could not be read
	// (bad permissions, corrupt JSON, not a regular file). The resolved BackendURL falls
	// back as if nothing were stored, so the CLI still launches; this field is what lets
	// a surface say the preference was ignored rather than silently honouring the
	// default. Nil both when a preference loaded cleanly and when there is none.
	EndpointLoadError error
	// EndpointInsecureRejected is non-nil when a STORED endpoint preference (and
	// nothing else — no override, no trusted env) resolved to a plaintext,
	// non-loopback URL that AllowInsecureBackend did not authorize. The resolved
	// BackendURL falls back to the deployed default, same contract as
	// EndpointLoadError: a stored preference must not be able to brick a launch,
	// least of all the `/backend` command that is the only way to change it once
	// the app is running. An override or trusted-env value that fails the same
	// check is NOT covered here — that is a direct, actionable LoadConfig error
	// instead, since the caller just typed or exported the bad value THIS launch.
	EndpointInsecureRejected error
	// EndpointShapeRejected is non-nil when a STORED endpoint preference (and nothing
	// else) was refused by backend.NormalizeBaseURL for its SHAPE rather than for
	// being plaintext: embedded userinfo, a query string or fragment, a non-HTTP
	// scheme, no host, control characters, an absurd length. The resolved BackendURL
	// falls back to the deployed default, exactly as for EndpointInsecureRejected.
	//
	// It is a SEPARATE field rather than a second use of that one because the two say
	// different things to a user. "Plaintext to a remote host" is a security refusal
	// they may well have meant and can authorize; "malformed" is a file to repair.
	// A caller that could not tell them apart would report the second as the first.
	//
	// Its message never echoes the stored value — a rejected endpoint is exactly the
	// kind that carries `user:password@` or `?token=`, and this reaches /status and
	// the debug log. See NormalizeBaseURL's redaction rule.
	EndpointShapeRejected error
	// BackendURLPinnedByEnv reports that DAINTREE_BACKEND_URL (or --backend-url) is
	// deciding the endpoint, so a STORED choice is being overridden.
	//
	// It exists to stop a specific silent failure: someone runs `/backend local`, sees
	// it confirmed, restarts, and lands back on the deployed endpoint because a shell
	// profile exports the variable. Without this flag `/backend` cannot tell them why,
	// and the feature looks broken rather than overridden.
	BackendURLPinnedByEnv bool
	// AllowInsecureBackend authorizes a non-loopback plaintext HTTP backend
	// endpoint, which LoadConfig otherwise refuses outright — see
	// backend.ValidatePlaintextRemote. Surfaced so doctor/host:ready/the support
	// bundle can show the insecure state rather than leaving it silently in effect.
	AllowInsecureBackend bool
	// APIKey is a DEPRECATED caller-supplied bearer token, empty on virtually every
	// install.
	//
	// It is no longer described as "the key that funds a turn", because that is no
	// longer what a credential means here. Account sign-in (internal/auth) is the
	// supported path: it identifies a PERSON and their plan, where this identifies
	// nothing and simply overrides. The two cannot both be right about who is calling,
	// so this one is on its way out — see APIKeyDeprecated, which callers surface.
	//
	// It stays readable for now so an existing harness does not break in the same
	// release that introduces the replacement. Trusted env only, as before: it is
	// spendable, so a bound project's .env must be able neither to supply nor read it.
	APIKey string
	// APIKeyDeprecated reports that a deprecated caller key is in play, so `doctor`,
	// `auth status` and the support bundle can say so. It is surfaced rather than
	// warned about at load time because config loading happens on every invocation,
	// including ones with no output channel at all.
	APIKeyDeprecated bool

	Tier        domain.Tier
	AutoApprove bool
	Offline     bool

	ProjectInstructions string
	DebugLog            bool

	// WorkflowIntelligence gates the client-owned workflow execution-graph layer
	// (graph tools, dispatch observer, turn-context digests, backend workflow
	// tasks). Defaults ON; DAINTREE_WORKFLOW_INTELLIGENCE=0 turns it off for a
	// backend that does not carry the matching TurnContext contract.
	WorkflowIntelligence bool

	// Routing is the caller's endpoint-selection preference, sent to the backend on
	// every turn. The zero value means "no preference" and is what almost every
	// install runs: the server default is a no-training privacy floor ranked by
	// throughput.
	//
	// Resolved from TRUSTED env only, never a project .env — the same boundary the
	// endpoint sits behind. A bound repository cannot drop the no-training floor
	// (the backend sends that unconditionally and does not derive it from this block),
	// but it COULD pin every request to an endpoint of its choosing, or quietly stop a
	// user's zero-retention choice from taking effect. Which compliant endpoint sees
	// someone's source is not a decision a checked-in file should make.
	Routing backend.Routing
}

// ConfigOverrides are the explicit (CLI-supplied) overrides. All optional; nil
// pointers mean "not provided". There are deliberately no model/provider overrides:
// the CLI holds no model credentials and never talks to a provider — the backend
// owns the model choice, the prompts, and the keys (see docs/BACKEND.md).
type ConfigOverrides struct {
	ProjectPath *string
	StateDir    *string
	ProjectID   *string
	WindowID    *string
	McpURL      *string
	McpToken    *string
	// NoInheritedMcpToken and NoInheritedAPIKey drop a credential this PROCESS
	// inherited from its own environment, after resolution.
	//
	// They exist because an empty override cannot express "none": FirstString skips
	// blank values, so writing "" simply falls through to the environment. The caller
	// that needs this is the MCP server, where a session may redirect an endpoint —
	// and an inherited bearer must not follow a URL that a model chose.
	NoInheritedMcpToken  bool
	NoInheritedAPIKey    bool
	BackendURL           *string
	APIKey               *string
	Tier                 *string
	AutoApprove          *bool
	Offline              *bool
	DebugLog             *bool
	LogDir               *string
	ProjectInstructions  *string
	WorkflowIntelligence *bool
	// AllowInsecureBackend is the deliberately-named escape hatch for a non-loopback
	// plaintext HTTP backend endpoint (--allow-insecure-backend). Trusted-only, like
	// Tier/AutoApprove/Offline: a bound project's .env must not be able to downgrade
	// the endpoint's confidentiality on someone's behalf.
	AllowInsecureBackend *bool
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
// caller (e.g. the runbooks-dir override) silently read a project-controlled value.
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
	return loadConfig(overrides, true)
}

// LoadConfigForProbe resolves the same configuration as LoadConfig but does NOT create
// the state directory.
//
// It exists for the read-only probes that need only the endpoint, the optional bearer
// and the routing posture — `--list-runbooks` today. Those answer a question about the
// BACKEND, and creating a directory on the user's disk to ask it is both a side effect
// nobody requested and a way to fail for a reason that has nothing to do with the
// question: `--state-dir` pointing somewhere unwritable would abort a listing that never
// wanted a state dir at all.
//
// cfg.DBPath is still computed. Nothing on a probe path may open it.
func LoadConfigForProbe(overrides ConfigOverrides) (AppConfig, error) {
	return loadConfig(overrides, false)
}

func loadConfig(overrides ConfigOverrides, ensureStateDir bool) (AppConfig, error) {
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
	// Nothing here is mergedGet. Every endpoint below is trustedOrOwn because a bearer
	// token travels to it: a project .env must never be able to redirect where a trusted
	// secret is sent (exfiltration).
	// The MCP endpoint + token are the Daintree connection credentials (injected by Daintree
	// into the real env). They are trustedOrOwn so a project .env can't redirect the link or
	// inject a token: no default → degraded local mode when genuinely unset.
	cfg.McpURL = FirstString(deref(overrides.McpURL), e.trustedOrOwnGet("DAINTREE_MCP_URL"))
	cfg.McpToken = FirstString(deref(overrides.McpToken), e.trustedOrOwnGet("DAINTREE_MCP_TOKEN"))
	// Applied AFTER resolution: the point is to discard what the environment supplied,
	// which an empty override cannot do.
	if overrides.NoInheritedMcpToken && deref(overrides.McpToken) == "" {
		cfg.McpToken = ""
	}
	// ProjectID + WindowID are the Daintree-injected IDENTITY (they scope StateDir
	// and the UI binding) and DebugLog gates full-fidelity tracing — all trustedOrOwn
	// (real env or the assistant's OWN .env), NEVER the project .env. A bound repo
	// must not be able to spoof identity (cross-project state access) or silently
	// enable tracing by planting a .env.
	cfg.ProjectID = FirstString(deref(overrides.ProjectID), e.trustedOrOwnGet("DAINTREE_PROJECT_ID"))
	cfg.WindowID = FirstString(deref(overrides.WindowID), e.trustedOrOwnGet("DAINTREE_WINDOW_ID"))
	cfg.DebugLog = resolveBool(overrides.DebugLog, e.trustedOrOwnGet("DAINTREE_ASSISTANT_DEBUG_LOG"))
	// The workflow-intelligence flag is trustedOrOwn like DebugLog: a bound project's
	// .env must not be able to flip a feature that changes what the backend is sent.
	//
	// ON unless something turns it off. It was a rollout flag defaulting off while the
	// backend caught up, and the backend has caught up — it advertises `workflow_plan`,
	// `workflow_reconcile` and `workflow_resume_digest`, and its runbook catalog ships
	// "Execute a durable workflow graph", which the selector loads at high confidence
	// for any large request. A client with the flag off registers none of the graph
	// tools that runbook is written against, so the model is instructed to build a
	// durable workflow and handed nothing to build it with: it announces the plan,
	// finds no tool, and announces it again. The rollout finished on the server and was
	// never flipped on the client, and this is what that looked like from the outside.
	cfg.WorkflowIntelligence = resolveBoolDefault(
		overrides.WorkflowIntelligence,
		e.trustedOrOwnGet("DAINTREE_WORKFLOW_INTELLIGENCE"),
		true,
	)

	// Endpoint routing. Validated HERE so a typo is a startup error naming the valid
	// choices, rather than a 400 that lands mid-turn after the user has typed a message.
	cfg.Routing = backend.Routing{
		Privacy: strings.TrimSpace(e.trustedGet("DAINTREE_ROUTING_PRIVACY")),
		Sort:    strings.TrimSpace(e.trustedGet("DAINTREE_ROUTING_SORT")),
		Only:    backend.ParseEndpointList(e.trustedGet("DAINTREE_ROUTING_ONLY")),
		Ignore:  backend.ParseEndpointList(e.trustedGet("DAINTREE_ROUTING_IGNORE")),
	}
	if err := cfg.Routing.Validate(); err != nil {
		return AppConfig{}, fmt.Errorf("invalid endpoint routing: %w", err)
	}

	// --- trusted-only settings (NEVER from a loaded .env) ---
	cfg.Tier = resolveTier(overrides.Tier, e.trustedGet("DAINTREE_ASSISTANT_TIER"))
	cfg.AutoApprove = resolveBool(overrides.AutoApprove, e.trustedGet("DAINTREE_ASSISTANT_AUTO_APPROVE"))
	cfg.Offline = resolveBool(overrides.Offline, e.trustedGet("DAINTREE_ASSISTANT_OFFLINE"))

	// stateDir (trusted/override → per-project subdir → flat root).
	home, homeErr := os.UserHomeDir()
	explicitStateDir := FirstString(deref(overrides.StateDir), e.trustedGet("DAINTREE_ASSISTANT_STATE_DIR"))
	// A missing home is FATAL unless the state dir was named explicitly. Ignoring the
	// error yields a RELATIVE stateRoot (".daintree/assistant-cli"), which resolves
	// against the working directory — i.e. inside the bound project. The state dir now
	// holds the API key, so that silently writes a spendable secret into the user's
	// repository, where 0600 does not save it from a stray `git add`. Fail loudly.
	if (homeErr != nil || strings.TrimSpace(home) == "") && explicitStateDir == "" {
		return AppConfig{}, fmt.Errorf("cannot resolve a home directory for the state dir (set DAINTREE_ASSISTANT_STATE_DIR): %w", homeErr)
	}
	stateRoot := filepath.Join(home, stateRootSubpath)
	cfg.StateDir = explicitStateDir
	cfg.StateRoot = explicitStateDir
	if cfg.StateDir == "" {
		cfg.StateRoot = stateRoot
		if cfg.ProjectID != "" {
			cfg.StateDir = filepath.Join(stateRoot, ProjectIDToDir(cfg.ProjectID))
		} else {
			cfg.StateDir = stateRoot
		}
	}
	// Always ABSOLUTE, like logDir below. A relative state dir resolves against the
	// working directory, and the two processes that share a project do not share one:
	// spawnDaemon sets the child's cwd to the PROJECT path while handing it this same
	// string. `--state-dir .state` launched from /tmp/harness would then give the
	// foreground /tmp/harness/.state and the daemon <project>/.state — different flocks,
	// different databases, and a credentials.json created inside the user's repository.
	if abs, err := filepath.Abs(cfg.StateDir); err == nil {
		cfg.StateDir = abs
	}
	if abs, err := filepath.Abs(cfg.StateRoot); err == nil {
		cfg.StateRoot = abs
	}
	// 0700: the state dir holds conversations, the audit trail, automation grants, and
	// memories — owner-only, never world/group readable (mirrors the debug-log dir perms).
	// Skipped for a read-only probe, which resolves the endpoint and nothing else.
	if ensureStateDir {
		if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
			return AppConfig{}, fmt.Errorf("create state dir: %w", err)
		}
	}
	cfg.DBPath = filepath.Join(cfg.StateDir, "state.db")

	// --- backend endpoint + optional bearer ---
	// Both are trustedGet, never merged. A bound project's .env must be able neither
	// to redirect where a turn is sent (the URL) nor to supply a spendable credential
	// on the user's behalf (the key).
	//
	// The endpoint is PER-USER, like the choice it records: one `/backend local` serves
	// every project rather than having to be repeated in each. An EXPLICIT state-dir
	// override is the exception — tests, benchmarks and harnesses all point the state
	// dir somewhere disposable, and they must neither read nor clobber the developer's
	// real preference.
	endpointDir := stateRoot
	if explicitStateDir != "" {
		endpointDir = cfg.StateDir
	}
	cfg.EndpointPath = EndpointPath(endpointDir)
	// Env ABOVE the stored choice, deliberately. A harness, a CI job and the e2e fake
	// backend all set DAINTREE_BACKEND_URL and must not be silently redirected by
	// whatever a developer last chose interactively. The cost is that an exported
	// variable overrides a stored one without saying so, which is what
	// BackendURLPinnedByEnv exists to let `/backend` explain.
	envURL := e.trustedGet("DAINTREE_BACKEND_URL")
	cfg.BackendURLPinnedByEnv = strings.TrimSpace(envURL) != "" || strings.TrimSpace(deref(overrides.BackendURL)) != ""
	// A stored preference that cannot be READ is not the same as no preference, and the
	// difference matters: silently falling back would send the next conversation to the
	// deployed backend for someone who deliberately chose a local one. Never fatal — a
	// preference must not brick a launch, least of all the `/backend` command that
	// rewrites it — so the error is carried for the caller to surface instead.
	stored, storedErr := LoadBackendURL(cfg.EndpointPath)
	cfg.EndpointLoadError = storedErr
	cfg.BackendURL = FirstString(
		deref(overrides.BackendURL),
		envURL,
		stored,
		backend.DefaultBaseURL,
	)
	// Trusted-only, like Tier/AutoApprove/Offline above: a bound project's .env must
	// not be able to authorize plaintext for an endpoint it does not control.
	cfg.AllowInsecureBackend = resolveBool(overrides.AllowInsecureBackend, e.trustedGet("DAINTREE_ALLOW_INSECURE_BACKEND"))
	// ONE validator, for all four startup sources at once — and it is the SAME one the
	// interactive `/backend <url>` runs (app.ResolveBackendTarget). Startup used to apply
	// only the plaintext rule, so `https://user:pass@host` (which Go turns into a Basic
	// Authorization header on every request), `https://host?token=x` (which the API path
	// is joined onto, so nothing ever reaches the API) and `ftp://host` were all accepted
	// at launch while being flatly refused mid-session. Same input, two answers, decided
	// by which door it came through.
	//
	// It also settles the SPELLING. NormalizeBaseURL returns a canonical form, so
	// DAINTREE_BACKEND_URL=https://x/ and `/backend https://x` are now one value rather
	// than two that compare unequal at every string check and render differently in every
	// diagnostic.
	canonical, endpointErr := backend.NormalizeBaseURL(cfg.BackendURL, cfg.AllowInsecureBackend)
	if endpointErr == nil {
		cfg.BackendURL = canonical
	} else {
		// An override or trusted env chose this URL for THIS launch: a direct,
		// actionable failure — the caller just typed or exported the bad value.
		// Matches FirstString's own TrimSpace, so "the value that won" and "the value
		// we blame" can never be different strings.
		explicit := strings.TrimSpace(deref(overrides.BackendURL)) != "" || strings.TrimSpace(envURL) != ""
		if explicit {
			return AppConfig{}, fmt.Errorf("backend endpoint: %w", endpointErr)
		}
		// Nothing this launch chose it — it can only be the STORED preference (the
		// compiled-in default is validated by this same function in the test suite
		// and cannot fail it). Same contract as EndpointLoadError just above: a
		// stored preference must not be able to brick a launch, least of all the one
		// command (`/backend default`) that could otherwise fix it — fall back
		// instead, and surface the rejection for a caller to report.
		//
		// The two diagnostics stay SEPARATE because a surface says different things
		// about them: "your remembered endpoint is plaintext to a remote host" is a
		// security refusal a user may have meant and can authorize, while "your
		// remembered endpoint is malformed" is a repair job. Collapsing them into
		// EndpointInsecureRejected would report the second as the first.
		var plaintext *backend.PlaintextRemoteError
		if errors.As(endpointErr, &plaintext) {
			cfg.EndpointInsecureRejected = endpointErr
		} else {
			cfg.EndpointShapeRejected = endpointErr
		}
		cfg.BackendURL = backend.DefaultBaseURL
	}
	cfg.APIKey = FirstString(
		deref(overrides.APIKey),
		e.trustedGet("DAINTREE_API_KEY"),
	)
	// See NoInheritedMcpToken: applied after resolution, because an empty override
	// falls through to the environment rather than clearing it.
	if overrides.NoInheritedAPIKey && deref(overrides.APIKey) == "" {
		cfg.APIKey = ""
	}
	// Shape-check it HERE, where the value is resolved, because this is the only place
	// a human error is still legible. Nobody is prompted for this key any more, so a
	// bad one arrives via the environment — shell-mangled, smart-quoted, wrapped — and
	// would otherwise die inside net/http as "invalid header field value" on every
	// single turn, naming neither the variable nor the cause.
	if cfg.APIKey != "" {
		if err := backend.ValidateKeyShape(cfg.APIKey); err != nil {
			return AppConfig{}, fmt.Errorf("DAINTREE_API_KEY: %w", err)
		}
		cfg.APIKeyDeprecated = true
	}

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

// resolveBoolDefault is resolveBool for a setting that is ON unless something says
// otherwise: the override wins, then the env var, then the default.
//
// It exists because resolveBool cannot express "unset means on" — an unset variable and
// an explicit "0" are the same empty-ish input to it, so a default-on setting could
// never be turned off. Accepted OFF spellings are "0", "false", "no" and "off", case
// insensitive; anything else non-empty is ON, because a typo in a feature switch should
// leave the feature working rather than silently disable it.
func resolveBoolDefault(override *bool, envValue string, def bool) bool {
	if override != nil {
		return *override
	}
	switch strings.ToLower(strings.TrimSpace(envValue)) {
	case "":
		return def
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
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
// apiKey and mcpToken are redacted; projectInstructions shown as
// a byte count; mcpUrl notes degraded mode when unset.
func DescribeConfig(cfg AppConfig) map[string]string {
	out := map[string]string{
		"projectPath":          cfg.ProjectPath,
		"stateDir":             cfg.StateDir,
		"dbPath":               cfg.DBPath,
		"logDir":               cfg.LogDir,
		"backendUrl":           cfg.BackendURL,
		"apiKey":               redactSecret(cfg.APIKey),
		"mcpToken":             redactSecret(cfg.McpToken),
		"projectId":            cfg.ProjectID,
		"windowId":             placeholderUnset(cfg.WindowID),
		"tier":                 string(cfg.Tier),
		"autoApprove":          strconv.FormatBool(cfg.AutoApprove),
		"offline":              strconv.FormatBool(cfg.Offline),
		"debugLog":             strconv.FormatBool(cfg.DebugLog),
		"workflowIntelligence": strconv.FormatBool(cfg.WorkflowIntelligence),
		"allowInsecureBackend": strconv.FormatBool(cfg.AllowInsecureBackend),
	}
	if cfg.EndpointInsecureRejected != nil {
		out["endpointInsecureRejected"] = cfg.EndpointInsecureRejected.Error()
	}
	// Safe to render verbatim: NormalizeBaseURL's errors are structural by
	// construction and never quote the value that failed.
	if cfg.EndpointShapeRejected != nil {
		out["endpointShapeRejected"] = cfg.EndpointShapeRejected.Error()
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
