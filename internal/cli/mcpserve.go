package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/daintreehq/assistant/internal/app"
	"github.com/daintreehq/assistant/internal/cli/render"
	"github.com/daintreehq/assistant/internal/config"
	"github.com/daintreehq/assistant/internal/debuglog"
	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/mcp"
	"github.com/daintreehq/assistant/internal/mcpserver"
	"github.com/daintreehq/assistant/internal/tools"
)

// RunMCPServe is the `mcp --stdio` entry: it serves the assistant itself as an MCP
// server so another agent can drive it as a sub-agent.
//
// The factory below is the interesting part. Unlike every other entry point, it resolves
// configuration PER SESSION rather than once at startup: an MCP client launches this
// process a single time and holds the pipe for its whole session, with no way to restart
// it when the operator wants a different project or backend. Making the binding a
// session argument turns "restart the server" into "close and open a session".
func RunMCPServe(ctx context.Context, opts Options) int {
	// Resolve the process-level defaults ONCE. The auto-approve default in particular
	// cannot be read off opts: a nil Options.AutoApprove means "the environment
	// decides", so inspecting the pointer would silently report false for a process
	// launched with DAINTREE_ASSISTANT_AUTO_APPROVE=1 and then suppress it.
	var defaultAutoApprove bool
	// policy is the process-level authority ceiling. On THIS surface the caller is a
	// model whose arguments can be steered by repository text or tool output, so the
	// ceiling is not optional — an unconfined registry here would let a prompt
	// injection choose its own tier and approval mode. It is derived from what the
	// OPERATOR launched this process with, which makes the rule precise: a session may
	// narrow what the operator already chose, and can never widen it.
	policy := mcpserver.ServerPolicy{
		// Endpoints and credentials are PINNED to what the operator launched this
		// process with. They are the two arguments that decide where a session's data
		// goes and whose credential pays for it, and neither is a thing a model reading
		// a repository is entitled to choose. A harness that genuinely needs to repoint
		// launches a second server against the other endpoint — which is a decision
		// made at the shell, by a human, exactly once.
		AllowBackendOverride:         false,
		AllowMCPOverride:             false,
		AllowCredentialOverride:      false,
		RequireTLSForRemoteEndpoints: true,
	}
	// FATAL, not best-effort. The ceiling is DERIVED from this config: without it the
	// policy keeps empty root allowlists and no MaxTier, which is precisely the
	// unconfined server the policy exists to prevent — and a session argument can then
	// repair the very config error that produced it (name a writable stateDir, an
	// arbitrary project, tier "system") and run under no ceiling at all. A
	// model-facing process cannot safely synthesize its own ceiling from a failure.
	cfg, err := loadConfigFromOptions(opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mcp server: cannot resolve the launch configuration this server's "+
			"authority ceiling is derived from:", err)
		return domain.OneShotExitCode.Error
	}
	defaultAutoApprove = cfg.AutoApprove
	policy.DefaultAutoApprove = cfg.AutoApprove
	// Auto-approve is permitted for a session only if the operator already turned
	// it on process-wide. Otherwise a session cannot grant itself unattended
	// mutation.
	policy.AllowAutoApprove = cfg.AutoApprove
	// Delegation is a launch decision, not a session one. See Options.AllowDelegatedApprovals.
	policy.AllowDelegatedApprovals = opts.AllowDelegatedApprovals
	// QUESTIONS are permitted by default, unlike approvals, and the asymmetry is the
	// point. An approval releases an action the assistant wants to take; a question picks
	// among options the assistant itself proposed, and answering one authorises nothing
	// that declining would have prevented — the turn proceeds with a choice instead of a
	// cancelled call. Gating it would only mean the surface built to test the product
	// still could not reach the branches the product reaches.
	policy.AllowDelegatedQuestions = true
	policy.MaxTier = domain.Tier(cfg.Tier)
	policy.DefaultTier = domain.Tier(cfg.Tier)
	policy.DefaultProject = cfg.ProjectPath
	policy.DefaultStateDir = cfg.StateDir
	policy.DefaultLogDir = cfg.LogDir
	// CONFINE the filesystem too, and to the directories this process was launched
	// against rather than to nothing.
	//
	// An empty allowlist means unconfined, which is the wrong default here for the
	// same reason an unpinned endpoint is: a prompt injection in one repository
	// should not be able to open a system-tier session on another one, or on the
	// user's home directory. The operator picks the project by launching the server
	// in it (or with --project); a session may still name a path INSIDE that
	// project, which is what a monorepo harness actually needs.
	policy.AllowedProjectRoots = confineRoots(cfg.ProjectPath)
	// The state ROOT, not the resolved state DIR. A session may legitimately name a
	// different projectId, which config scopes into a SIBLING directory under the
	// same root — so an allowlist holding only this launch's resolved dir would be
	// checked against a path the factory then declines to use, and the confinement
	// would be a string comparison rather than a boundary. The root is the honest
	// set of directories this process can produce. An explicitly-named state dir has
	// no scoping, so root and dir are the same path there.
	policy.AllowedStateRoots = confineRoots(cfg.StateRoot)
	policy.AllowedLogRoots = confineRoots(cfg.LogDir)

	// PIN THE RESOLVED ENDPOINT ONTO EVERY SESSION.
	//
	// Refusing an explicit `backendUrl` is not enough on its own, because the endpoint
	// has a second, indirect source: an explicit state directory makes config read
	// `endpoint.json` from THAT directory rather than the per-user root (it is how a
	// harness keeps its own `/backend` choice off the developer's). A session that names
	// only a stateDir — perfectly legal, confined, no endpoint argument in sight —
	// would therefore pick up whatever endpoint that directory's file names, and the
	// inherited API key would follow it, since nothing looked like a redirect.
	//
	// Resolving the endpoint once here and handing it to every session closes that: an
	// explicit override outranks the stored file, so the launch endpoint is the endpoint
	// whatever a session does with its state directory.
	opts.BackendURL = cfg.BackendURL

	// One debug log for the PROCESS, not one per session. debuglog keeps a single
	// package-global active path, so a per-session start would silently redirect every
	// earlier session's writes into the newest session's file and leave the paths those
	// sessions reported pointing at a log that stopped growing. Sessions stay
	// distinguishable inside the one file by their sessionId fields.
	var logOnce sync.Once
	var processLogPath string
	startProcessLog := func(cfg config.AppConfig) string {
		logOnce.Do(func() {
			processLogPath = debuglog.StartDebugLog(
				debuglog.Config{DebugLog: cfg.DebugLog, LogDir: cfg.LogDir},
				map[string]any{"sessionId": "mcp-server", "project": cfg.ProjectPath})
		})
		// Prefer the live value: a log started by an earlier session is the one in use.
		if path := debuglog.CurrentDebugLogPath(); path != "" {
			return path
		}
		return processLogPath
	}

	// Serving on stdout means every diagnostic must go to stderr. A single stray byte on
	// stdout corrupts the JSON-RPC framing and the client drops the connection.
	factory := func(bootstrap, lifetime context.Context, p mcpserver.OpenParams) (mcpserver.Runtime, error) {
		sessionOpts := sessionOptions(opts, p)
		// The mode decides both the confirm hook's behaviour and, for "auto", the
		// runtime's own auto-approve (which makes dispatch skip the hook entirely —
		// they are two different layers and both have to agree).
		mode, auto := resolveApprovalMode(p.Approvals, defaultAutoApprove)
		sessionOpts.AutoApprove = &auto
		if p.DebugLog != nil {
			sessionOpts.DebugLog = p.DebugLog
		}
		if tier := sessionOpts.Tier; tier != "" && !domain.Tier(tier).IsValid() {
			return nil, fmt.Errorf("invalid tier %q (choose supervisor, operator, or system)", tier)
		}

		overrides, err := buildOverrides(sessionOpts, render.New(os.Stderr))
		if err != nil {
			return nil, err
		}
		// Take the project's owner lease for the session's whole life, spawning the
		// supervisor daemon like an interactive launch: an MCP-driven session is a
		// long-lived owner, not a probe, and the work it starts deserves to be adopted
		// when the session closes. A short wait so a busy project fails fast with a
		// clear message rather than hanging a tool call.
		own, err := acquireOwnership(bootstrap, overrides, true, 15*time.Second, nil)
		if err != nil {
			return nil, err
		}
		a, err := app.Create(app.CreateOptions{Overrides: overrides, PinnedSkillIDs: sessionOpts.PinnedSkillIDs})
		if err != nil {
			own.Release()
			return nil, err
		}
		// Negotiate the pins before the session is handed back, so a bad id fails the
		// session.open tool call — where the caller is looking — instead of silently
		// producing turns that never load the runbook they named. Uses the BOOTSTRAP
		// context: this is work the open must finish, and a client that gave up should
		// stop us waiting on it.
		//
		// Ordered ahead of AdoptAsCurrentSession: adoption writes the project's durable
		// current-session pointer and shutdown does not restore the previous value, so a
		// session.open that FAILS its preflight would otherwise leave the supervisor
		// resuming a conversation that never ran a turn.
		pinNotice, perr := a.PreparePinnedSkills(bootstrap)
		if perr != nil {
			_ = a.Shutdown()
			own.Release()
			return nil, perr
		}
		a.AdoptAsCurrentSession()

		logPath := startProcessLog(a.Config)

		// Confirmations go through the session's broker, which is what makes "delegate"
		// more than a slogan: it parks the dispatch, surfaces the call with its risk,
		// consequence and redacted args, and fails closed on a timer so a forgotten
		// approval can never pin the turn forever. What it is NOT is a human decision —
		// see approvals.go.
		//
		// The event sink is NOT set here. It is per-TURN — each turn records into its own
		// Run — and appRuntime.Send installs it. Wiring one here would be wrong twice
		// over: it would mix turns together, and it would look like the recording is
		// handled when it is not.
		approvals := mcpserver.NewApprovals(mode, p.ApprovalTimeout)
		// Questions are INDEPENDENT of approvals. Deriving one from the other defeated the
		// case they were added for: a harness that wants planning questions while keeping
		// mutations declined could not have them without also granting approval authority
		// it did not want. There is no auto-answer either — bypassing a confirmation is a
		// decision an operator can make, but answering "which of these did you mean?" on
		// someone's behalf is not.
		questionMode := p.Questions
		if questionMode == "" {
			questionMode = mcpserver.QuestionDecline
		}
		questions := mcpserver.NewQuestions(questionMode, p.QuestionTimeout)
		// runtime is captured by the hook so an approval can name the run it blocks.
		// Assigned below, after the facts are built; the hook only ever reads it on a
		// dispatch, which cannot happen before the runtime exists.
		var runtime interface{ CurrentRunID() string }
		a.SetHooks(app.AppHooks{
			// AskChoice is wired HERE, which is what closes the parity gap: without it
			// the runtime has no question surface, user.askMultipleChoice reports
			// QUESTION_UNAVAILABLE, and a turn that needed a planning decision took a
			// different path on this surface than it takes in the product — so an
			// end-to-end run could not reach the branch it was written to test.
			AskChoice: func(cctx context.Context, req tools.AskChoiceRequest) (tools.AskChoiceAnswer, error) {
				runID := ""
				if runtime != nil {
					runID = runtime.CurrentRunID()
				}
				return questions.Ask(cctx, req, runID)
			},
			Confirm: func(cctx context.Context, req tools.ConfirmRequest) (bool, error) {
				runID := ""
				if runtime != nil {
					runID = runtime.CurrentRunID()
				}
				return approvals.Confirm(cctx, mcpserver.ApprovalRequest{
					Tool:        req.ToolName,
					Risk:        req.Risk,
					Consequence: req.Consequence,
					Summary:     req.Summary,
					RawArgs:     string(req.Args),
					RunID:       runID,
					// Forwarded, not enforced: this server delegates the decision to a
					// caller with its own approval UX, but dropping the verdict made a
					// system-risk action and an ordinary project mutation arrive as the
					// same boolean with different prose.
					NeedsTypedConfirm: req.NeedsTypedConfirm,
				}), nil
			},
		})

		st := a.ConnectMcp(bootstrap)
		// LIFETIME, not bootstrap: the SDK cancels a request context the moment its
		// response is sent, so a scheduler started on the open call's context would die
		// before the session had run a single turn — and the async coordinator would keep
		// ACCEPTING work (its started flag survives its parent's cancellation) with no
		// loop left to settle it.
		//
		// The attention callback is deliberately NIL. A non-nil one enables the
		// scheduler's notifier, which marks every attention-or-higher event delivered
		// after invoking it — so a no-op callback would silently consume exactly the
		// async completions daintree.attention exists to hand back. With nil, the
		// notifier stands down and the rows stay unnotified until a caller reads them.
		a.StartScheduler(lifetime, nil)

		facts := mcpserver.RuntimeFacts{
			Project:      a.Config.ProjectPath,
			Tier:         string(a.Tier()),
			BackendURL:   mcp.SanitizeURL(a.Config.BackendURL),
			LogPath:      logPath,
			AutoApprove:  a.Config.AutoApprove,
			ApprovalMode: string(mode),
			MCPConnected: st.Connected,
			MCPTransport: st.Transport,
			// Reported so a caller that inherited a server-level --skill can see what this
			// session REQUESTS on every turn (not what the backend honours — the pin
			// warnings own that); the advisory rides SessionOutput.Warnings via describe.
			PinnedSkills:        a.PinnedSkillIDs(),
			PinPreflightWarning: pinNotice,
		}
		rt := mcpserver.NewAppRuntime(a, facts, approvals, questions, own.Release)
		if withRun, ok := rt.(interface{ CurrentRunID() string }); ok {
			runtime = withRun
		}
		return rt, nil
	}

	err = mcpserver.ServeModelFacing(ctx, mcpserver.Options{
		Version:     buildVersion,
		Factory:     factory,
		Diagnostics: os.Stderr,
		Policy:      policy,
	})
	if err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, "mcp server:", err)
		return domain.OneShotExitCode.Error
	}
	return domain.OneShotExitCode.Success
}

// resolveApprovalMode decides the session's approval mode and the Config.AutoApprove
// that must accompany it.
//
// The two are separate layers and both have to agree: Config.AutoApprove makes
// tools.Dispatch skip the confirm hook ENTIRELY, while the mode governs what the hook
// does when it IS consulted. Auto-approve is therefore set if and only if the mode is
// "auto" — anything else would either ask nobody while claiming to ask, or park a call
// that dispatch already waved through.
//
// defaultAutoApprove comes from the RESOLVED process config, not from an Options
// pointer: a nil Options.AutoApprove means "the environment decides", so reading the
// pointer would report false for a process launched with
// DAINTREE_ASSISTANT_AUTO_APPROVE=1 and then write an explicit false that suppressed it.
func resolveApprovalMode(requested mcpserver.ApprovalMode, defaultAutoApprove bool) (mcpserver.ApprovalMode, bool) {
	mode := requested
	if mode == "" {
		mode = mcpserver.ApprovalDecline
		if defaultAutoApprove {
			mode = mcpserver.ApprovalAuto
		}
	}
	return mode, mode == mcpserver.ApprovalAuto
}

// sessionOptions overlays one session's arguments onto the process-level options.
//
// Start from what this process was launched with (the .mcp.json env and any flags), then
// let the session's arguments win. That is the defaults-not-bindings rule: the launch
// config SEEDS a session, it does not constrain one — an omitted session field inherits
// the launch value rather than blanking it.
//
// It is a standalone function rather than inline in the factory so the precedence can be
// asserted without standing up a project lease, a store and a backend.
func sessionOptions(base Options, p mcpserver.OpenParams) Options {
	o := base
	// The one-shot prompt state is CLEARED, not overlaid: a server launched with a
	// prompt must not replay it into a session, and on the stdio transport a
	// --prompt-file of "-" would be read off the JSON-RPC stream that is carrying the
	// protocol itself.
	// MultiTurn is cleared for the same reason and then some: it reads prompts from
	// stdin line by line, which on this transport is the JSON-RPC stream carrying the
	// protocol itself.
	o.Prompt, o.HasPrompt, o.PromptFile, o.MultiTurn = "", false, "", false
	applyIfSet(&o.Project, p.Project)
	applyIfSet(&o.BackendURL, p.BackendURL)
	applyIfSet(&o.APIKeyFile, p.APIKeyFile)
	applyIfSet(&o.Tier, p.Tier)
	applyIfSet(&o.McpURL, p.McpURL)
	applyIfSet(&o.McpTokenFile, p.McpTokenFile)
	// AN INHERITED CREDENTIAL MUST NEVER FOLLOW A SESSION-CHOSEN URL.
	//
	// Both endpoints are session arguments by design — an MCP client cannot restart this
	// process, so repointing has to be possible. But on this surface the caller is a
	// MODEL whose context can be steered by repository text or tool output, and the
	// process may hold credentials it inherited from ITS launch. Combining the two gives
	// a clean exfiltration primitive: name `mcpUrl: http://attacker/`, say nothing about
	// the token, and the server posts its own Daintree bearer — which authorises
	// system-tier actions — straight to that host. `backendUrl` plus an inherited
	// DAINTREE_API_KEY is the same trick against a spendable key.
	//
	// So redirecting an endpoint forfeits the inherited secret for it. A session that
	// genuinely needs a different endpoint supplies its own credential file for that
	// endpoint; one that supplies neither simply runs degraded, which is a visible,
	// recoverable state rather than a silent leak.
	// Clearing the field is NOT enough — config's FirstString skips a blank value and
	// falls straight through to the environment, so an assignment of "" here would have
	// left the inherited token in place and the leak wide open. The suppression has to
	// be an explicit signal that survives resolution.
	o.NoInheritedMcpToken = strings.TrimSpace(p.McpURL) != "" && strings.TrimSpace(p.McpTokenFile) == ""
	o.NoInheritedAPIKey = strings.TrimSpace(p.BackendURL) != "" && strings.TrimSpace(p.APIKeyFile) == ""
	applyIfSet(&o.StateDir, p.StateDir)
	applyIfSet(&o.LogDir, p.LogDir)
	applyIfSet(&o.ProjectID, p.ProjectID)
	applyIfSet(&o.WindowID, p.WindowID)
	applySliceIfSet(&o.PinnedSkillIDs, p.Skills)
	return o
}

// applyIfSet overwrites dst only when the session supplied a value, so an unset session
// argument falls back to the process-level default rather than blanking it.
//
// "Supplied" means non-blank, not merely non-empty. config's FirstString trims every
// value it resolves, so a raw " " stored here would count as SET at this layer and as
// UNSET at that one — the launch flag would be discarded and the environment (or a bare
// state root) would answer instead. For an id that scopes the state directory, that
// silently opens the wrong project's database.
//
// It stores the ORIGINAL value, not the trimmed one. Blankness is a question about
// whether an argument was given; trimming is a change to what it SAYS, and several of
// these fields are paths, where a trailing space is a legal part of a filename. Reading
// "/keys/account" because the caller named "/keys/account " would bill a different
// credential — the exact class of silent substitution these flags exist to prevent.
func applyIfSet(dst *string, v string) {
	if strings.TrimSpace(v) != "" {
		*dst = v
	}
}

// applySliceIfSet is applyIfSet for a list argument, and it tests NIL rather than
// length on purpose. For a string, "" is the only way to say "unset", so the two
// coincide; for a slice they part company, and the difference is a real instruction: a
// caller that sends `"skills": []` is explicitly clearing a server-level --skill for
// this session, which length-testing would silently reverse into "inherit them".
//
// The copy is defensive — the session's decoded argument must not stay aliased into the
// process-level options that seed every later session.
func applySliceIfSet(dst *[]string, v []string) {
	if v != nil {
		*dst = append([]string(nil), v...)
	}
}

// confineRoots turns one resolved process-level directory into an allowlist.
//
// A blank one yields NIL, not a one-element list holding "" — an empty string would
// resolve to the process working directory and quietly confine every session there,
// which is a different (and unannounced) policy from the one the operator chose. Nil
// means the same thing the policy has always meant by an empty allowlist: this
// dimension is unconfined, because the process itself never bound it.
func confineRoots(dir string) []string {
	if strings.TrimSpace(dir) == "" {
		return nil
	}
	return []string{dir}
}
