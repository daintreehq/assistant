// Command daintree-assistant is the single static-binary entrypoint for Daintree's
// local orchestration assistant. It parses the CLI surface, then routes to exactly
// one of: environment/status commands, the persistent supervisor, the embedded
// stdio host, a one-shot prompt, or the interactive line REPL. Daintree renders the
// assistant natively over `host --stdio`; the REPL is the headless operator path, not
// a product surface. All real wiring lives in internal/cli; this file is the thin
// flag/route shim plus main's build-version seam.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/daintreehq/assistant/internal/cli"
	"github.com/daintreehq/assistant/internal/domain"
)

// version is injected at build time via -ldflags "-X main.version=…" (see Makefile).
// It defaults to "dev" for a plain `go build`/`go run` with no ldflags. It is the
// ONE value main owns end-to-end: reported by `--version`, by the host handshake,
// and by daemon descriptors. Keep the variable named exactly `version` — the
// Makefile's ldflag path (`-X main.version=$(VERSION)`) is byte-coupled to it.
var version = "dev"

func main() {
	parsed, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n\n", err)
		fmt.Fprintln(os.Stderr, "Run 'daintree-assistant --help' for usage.")
		os.Exit(2)
	}
	if parsed.Help {
		writeUsage(os.Stdout, version)
		return
	}
	if parsed.Version {
		fmt.Printf("daintree-assistant %s\n", version)
		return
	}
	opts, route := parsed.Options, parsed.Route

	// Stamp the build version into the cli layer for daemon descriptors, `status`,
	// and the host handshake.
	cli.SetVersion(version)

	// The line REPL owns Ctrl-C itself: it uses it to cancel only the current turn.
	// Capturing SIGINT in this parent context would permanently poison later turns.
	// SIGTERM is always a process shutdown; one-shot and subcommand paths
	// additionally use SIGINT as ordinary context cancellation.
	signals := []os.Signal{syscall.SIGTERM}
	// MultiTurn belongs with the one-shot paths, not the interactive ones: it has no
	// prompt argument, so HasPrompt is false, but it is headless and scripted and wants
	// Ctrl-C to be ordinary context cancellation. Omitting it would leave the flag as the
	// one scripted route a Ctrl-C could not stop.
	if route != routeDefault || opts.HasPrompt || opts.MultiTurn {
		signals = append(signals, os.Interrupt)
	}
	ctx, stop := signal.NotifyContext(context.Background(), signals...)
	defer stop()

	var code int
	switch route {
	case routeDoctor:
		code = cli.RunDoctor(ctx, opts)
	case routeHost:
		code = cli.RunHost(ctx, opts)
	case routeMCP:
		code = cli.RunMCPServe(ctx, opts)
	case routeDaemon:
		code = cli.RunDaemon(ctx, opts)
	case routeDaemonStop:
		code = cli.RunDaemonStop(ctx, opts)
	case routeStatus:
		code = cli.RunStatus(ctx, opts)
	case routeReset:
		code = cli.RunReset(ctx, opts, parsed.ResetScope, parsed.ResetOptions)
	case routeSupportBundle:
		code = cli.RunSupportBundle(ctx, opts, parsed.SupportBundle)
	case routeListSkills:
		code = cli.RunListSkills(ctx, opts)
	default:
		code = cli.Run(ctx, opts)
	}
	os.Exit(code)
}

// route is the top-level dispatch decided purely from argv. A leading command
// word wins unless `--json` or the `--` terminator explicitly makes it a prompt;
// everything else falls through to cli.Run for one-shot versus interactive.
type route int

const (
	routeDefault route = iota
	routeDoctor
	routeHost
	routeMCP
	routeDaemon
	routeDaemonStop
	routeStatus
	routeReset
	routeSupportBundle
	routeListSkills
)

// parsedArgs is the pure result of command-line parsing. main is the only place
// that prints or exits, which keeps the argument contract table-testable.
type parsedArgs struct {
	Options cli.Options
	Route   route
	Help    bool
	Version bool
	// Reset carries the `reset <scope>` subcommand's arguments. Only meaningful when
	// Route is routeReset.
	ResetScope   cli.ResetScope
	ResetOptions cli.ResetOptions
	// SupportBundle carries the `support-bundle` subcommand's arguments.
	SupportBundle cli.SupportBundleOptions
}

// parseArgs parses the CLI surface while preserving two useful properties that
// Go's stock FlagSet does not combine: options may be interspersed with a prompt,
// and `--` permanently ends option/subcommand parsing. The latter matters for
// prompts such as `-- "status"` and `-- "--summarize this"`.
// parseArgs is a thin wrapper so the route-scoped flag checks have ONE enforcement
// point. parseArgsInto returns from a dozen places; adding a guard to each of them is how
// a route eventually gets missed.
func parseArgs(args []string) (parsedArgs, error) {
	parsed, fs, err := parseArgsInto(args)
	if err != nil {
		return parsedArgs{}, err
	}
	// --help and --version answer without running anything, so they have no route to
	// scope a flag against. Refusing help because of a misplaced flag would withhold the
	// one output that explains where the flag belongs.
	if !parsed.Help && !parsed.Version {
		if err := checkRouteScopedFlags(fs, parsed.Route); err != nil {
			return parsedArgs{}, err
		}
	}
	return parsed, nil
}

func parseArgsInto(args []string) (parsedArgs, *flag.FlagSet, error) {
	fs := flag.NewFlagSet("daintree-assistant", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var (
		mcpURL   = fs.String("mcp-url", "", "")
		mcpToken = fs.String("mcp-token", "", "")
		project  = fs.String("project", "", "")
		tier     = fs.String("tier", "", "")
		offline  = fs.Bool("offline", false, "")
		classic  = fs.Bool("classic", false, "")
		inline   = fs.Bool("inline", false, "") // deprecated, accepted but hidden
		jsonOut  = fs.Bool("json", false, "")
		stdio    = fs.Bool("stdio", false, "") // host compatibility spelling
		showVer  = fs.Bool("version", false, "")
		// Harness knobs. Each shadows a trusted env var and wins over it; the point is
		// that a scripted caller (a test harness, another agent) can say all of this in
		// argv instead of having to rewrite the process environment.
		backendURL = fs.String("backend-url", "", "")
		apiKeyFile = fs.String("api-key-file", "", "")
		// promptFile carries the one-shot prompt out of argv entirely. A skill-test prompt
		// is long and multi-line and wants to live in a file next to the runbook it
		// exercises, rather than being shell-quoted — and a prompt beginning with a dash
		// no longer needs `--` first. "-" reads stdin.
		promptFile = fs.String("prompt-file", "", "")
		// multiTurn is the plural of the whole one-shot idea: one process, one session,
		// one prompt per stdin line, one JSONL transcript. It is a separate flag rather
		// than a plural reading of --prompt-file because that flag's stdin spelling
		// already means "all of stdin is ONE prompt", newlines included, and quietly
		// re-cutting it at line boundaries would change what an existing harness asks.
		multiTurn = fs.Bool("multi-turn", false, "")
		stateDir  = fs.String("state-dir", "", "")
		logDir    = fs.String("log-dir", "", "")
		// The project identity pair. --project-id is the load-bearing one: it scopes the
		// state directory into a per-project subdirectory, which is how a harness gets
		// isolation without hand-rolling paths.
		projectID = fs.String("project-id", "", "")
		windowID  = fs.String("window-id", "", "")
		// A PATH whose CONTENT becomes the DAINTREE.md override, so a skill can be tested
		// against a synthetic project brief without writing one into the repo under test.
		projectInstructionsFile = fs.String("project-instructions-file", "", "")
		autoApprove             = fs.Bool("auto-approve", false, "")
		// `mcp --stdio` only. Lets a session choose approvals:"delegate", where the
		// CALLING AGENT settles each confirmation. Plain bool, not the *bool tri-state:
		// it has no env counterpart, because the whole point is that a human at a shell
		// decides whether the agent on the other end of the pipe is one they trust to
		// approve mutations — an environment variable is exactly the wrong place for
		// that, since it is inherited rather than chosen.
		mcpAllowDelegatedApprovals = fs.Bool("allow-delegated-approvals", false, "")
		debugLog                   = fs.Bool("debug-log", false, "")
		timeout                    = fs.Duration("timeout", 0, "")
		// Plain bool, not the *bool tri-state: that machinery exists only for flags that
		// shadow a trusted env var and must be able to beat it. This one has no env
		// counterpart — it is a per-invocation decision about what this run commits to.
		runScheduler = fs.Bool("run-scheduler", false, "")
		// `reset` flags. Parsed always (a FlagSet cannot be conditional here) but only
		// consulted on the reset route, like every other subcommand-specific option.
		yes      = fs.Bool("yes", false, "")
		noBackup = fs.Bool("no-backup", false, "")
		// `support-bundle` flags.
		bundleOut   = fs.String("out", "", "")
		bundleAudit = fs.Bool("include-audit", false, "")
		// Skill controls. `--skill` is the repo's only repeatable flag, so it is the only
		// one registered through fs.Var rather than fs.String — one pin per occurrence,
		// deliberately NOT comma-splitting, because a skill id is an opaque backend key
		// and inventing a separator inside it would make a legal id unnameable.
		skills     skillIDFlags
		listSkills = fs.Bool("list-skills", false, "")
	)
	fs.Var(&skills, "skill", "")

	flagArgs, positionals, help, forcePrompt, err := splitInterspersedArgs(fs, args)
	if err != nil {
		return parsedArgs{}, nil, err
	}
	if err := fs.Parse(flagArgs); err != nil {
		return parsedArgs{}, nil, err
	}
	if help {
		return parsedArgs{Help: true}, fs, nil
	}
	if *showVer {
		return parsedArgs{Version: true}, fs, nil
	}

	tierValue := strings.TrimSpace(*tier)
	if flagWasSet(fs, "tier") && !domain.Tier(tierValue).IsValid() {
		return parsedArgs{}, nil, fmt.Errorf("invalid --tier %q (choose supervisor, operator, or system)", *tier)
	}

	if *timeout < 0 {
		return parsedArgs{}, nil, fmt.Errorf("invalid --timeout %s (must not be negative)", *timeout)
	}
	// --run-scheduler REQUIRES a bound. The flag holds the run open until the async work
	// it started settles, and settling is not guaranteed: an invocation whose terminals
	// stay unreadable does not advance toward expiry, so an unbounded flagged run can
	// wait forever. Demanding the duration explicitly is better than inventing a default,
	// which would silently truncate a legitimately long job at a number nobody chose.
	if *runScheduler && *timeout <= 0 {
		return parsedArgs{}, nil, fmt.Errorf("--run-scheduler requires a positive --timeout (e.g. --timeout 10m)")
	}
	// --multi-turn is validated HERE, before the route is chosen, and that placement is
	// the point. Every check below this line sits after a subcommand's early return, so
	// putting it there let `--multi-turn status` and `--json --multi-turn doctor` through
	// unexamined — the flag silently doing nothing on a route that never runs a turn,
	// which is exactly the "looks like it worked" failure --skill's own route check
	// exists to prevent. --run-scheduler validates its bound here for the same reason.
	//
	// It insists on --json, which is not mere validation. Without it this flag would be a
	// second, worse spelling of something that already exists — the line REPL on piped
	// stdin is multi-turn today — and the whole point of the flag is the half that route
	// CANNOT do: emit the conversation as one JSONL transcript.
	//
	// And it is a THIRD prompt source, so it obeys the same rule as the other two:
	// naming more than one is a mistake, never a precedence question. A command word is
	// a positional too, so this is also what refuses `--multi-turn doctor`.
	if *multiTurn {
		if !*jsonOut {
			return parsedArgs{}, nil, fmt.Errorf("--multi-turn requires --json; for human-rendered multi-turn output pipe stdin to --classic")
		}
		if *promptFile != "" || len(positionals) > 0 {
			return parsedArgs{}, nil, fmt.Errorf("--multi-turn reads its prompts from stdin, one per line; it cannot be combined with a prompt argument, a command, or --prompt-file")
		}
		if *stdio {
			return parsedArgs{}, nil, stdioRequiresHostError()
		}
		// --list-skills names its route with a FLAG rather than a positional, so the
		// check above cannot see it: it is a read-and-print that never runs a turn, and
		// pairing it with --multi-turn silently discards the conversation.
		if *listSkills {
			return parsedArgs{}, nil, fmt.Errorf("--multi-turn and --list-skills do not go together: --list-skills prints the catalog and exits without running a turn")
		}
	}
	// An explicitly EMPTY value is a mistake, never a request to fall back. A harness
	// that expands an unset shell variable produces `--api-key-file=` or `--state-dir=`,
	// and silently deferring to the environment there is precisely the failure these
	// flags exist to prevent: the run proceeds against a different key, or writes to the
	// developer's real state dir. Fail at the argument boundary instead.
	// "-" is a non-empty value, so --prompt-file's stdin spelling passes this check
	// untouched; only a literally empty `--prompt-file=` is rejected.
	//
	// The advice differs by flag because the FALLBACK differs, and pointing someone at an
	// environment variable that does not exist is worse than saying nothing: --prompt-file
	// has no other source at all, and --project-instructions-file falls back to the
	// project's own DAINTREE.md rather than to the environment.
	emptyValueFallback := map[string]string{
		"prompt-file":               "there is no other prompt source",
		"project-instructions-file": "omit the flag to use the project's own DAINTREE.md",
		"project":                   "omit the flag to use the current directory",
	}
	for _, name := range []string{"backend-url", "api-key-file", "prompt-file", "state-dir", "log-dir",
		"mcp-url", "mcp-token", "project", "project-id", "window-id", "project-instructions-file"} {
		f := fs.Lookup(name)
		if f == nil || !flagWasSet(fs, name) {
			continue
		}
		if strings.TrimSpace(f.Value.String()) == "" {
			advice, ok := emptyValueFallback[name]
			if !ok {
				advice = "omit the flag to fall back to the environment"
			}
			return parsedArgs{}, nil, fmt.Errorf("--%s was given an empty value; %s", name, advice)
		}
	}
	// A boolean override is carried as a POINTER so an explicit --auto-approve=false can
	// beat DAINTREE_ASSISTANT_AUTO_APPROVE=1. Passing the plain value would collapse
	// "not passed" and "passed false" into the same nil, and the env would keep winning
	// against someone who explicitly turned the flag off — worst on the flag whose whole
	// job is to decide whether mutating tools run unattended.
	boolFlag := func(name string, v *bool) *bool {
		if !flagWasSet(fs, name) {
			return nil
		}
		return v
	}

	opts := cli.Options{
		McpURL:                  *mcpURL,
		McpToken:                *mcpToken,
		Project:                 *project,
		Tier:                    tierValue,
		Offline:                 boolFlag("offline", offline),
		Classic:                 *classic,
		JSON:                    *jsonOut,
		Inline:                  *inline, // accepted and ignored (deprecated)
		BackendURL:              *backendURL,
		APIKeyFile:              *apiKeyFile,
		PromptFile:              *promptFile,
		MultiTurn:               *multiTurn,
		StateDir:                *stateDir,
		LogDir:                  *logDir,
		ProjectID:               *projectID,
		WindowID:                *windowID,
		ProjectInstructionsFile: *projectInstructionsFile,
		AutoApprove:             boolFlag("auto-approve", autoApprove),
		AllowDelegatedApprovals: *mcpAllowDelegatedApprovals,
		DebugLog:                boolFlag("debug-log", debugLog),
		Timeout:                 *timeout,
		RunScheduler:            *runScheduler,

		PinnedSkillIDs: skills,
	}

	parsed := parsedArgs{Options: opts, Route: routeDefault}
	// --list-skills is a read-and-print, and it is carved out FIRST — ahead of the
	// subcommand switch and, crucially, ahead of the "--json requires a prompt" rule
	// below, for the same reason `doctor --json` is: a listing a script cannot parse is
	// not one. It takes no prompt and no subcommand, and pairing it with --skill is a
	// contradiction (you cannot pin an id in the same breath as asking what the ids are),
	// so both are refused rather than silently ignored.
	if *listSkills {
		if len(positionals) > 0 {
			return parsedArgs{}, nil, fmt.Errorf("--list-skills does not take a prompt or a command: %s", strings.Join(positionals, " "))
		}
		if len(skills) > 0 {
			return parsedArgs{}, nil, errors.New("--list-skills and --skill do not go together: list the catalog first, then pin an id from it")
		}
		if *stdio {
			return parsedArgs{}, nil, stdioRequiresHostError()
		}
		parsed.Route = routeListSkills
		return parsed, fs, nil
	}
	// `doctor --json` is a real thing: doctor is the release gate, and a gate that can
	// only be read by a human is not one. It is carved out BEFORE the --json rule below
	// because that rule exists to keep a PROMPT named "doctor" working — and `-- doctor`
	// still forces the prompt for anyone who genuinely wants to ask about doctors.
	if len(positionals) == 1 && positionals[0] == "doctor" && !forcePrompt {
		parsed.Route = routeDoctor
		if *stdio {
			return parsedArgs{}, nil, stdioRequiresHostError()
		}
		if len(skills) > 0 {
			return parsedArgs{}, nil, errors.New("--skill has no effect on \"doctor\", which never runs a turn")
		}
		return parsed, fs, nil
	}
	// --json is otherwise unambiguously a one-shot request, so a prompt that happens to
	// be named "status" remains a prompt. `--` provides the same escape for the
	// human-output path.
	if len(positionals) > 0 && !forcePrompt && !*jsonOut {
		switch positionals[0] {
		case "doctor":
			if err := rejectCommandArgs("doctor", positionals[1:]); err != nil {
				return parsedArgs{}, nil, err
			}
			parsed.Route = routeDoctor
		case "host":
			if err := rejectCommandArgs("host", positionals[1:]); err != nil {
				return parsedArgs{}, nil, err
			}
			parsed.Route = routeHost
		case "mcp":
			if err := rejectCommandArgs("mcp", positionals[1:]); err != nil {
				return parsedArgs{}, nil, err
			}
			parsed.Route = routeMCP
		case "daemon":
			switch {
			case len(positionals) == 1:
				parsed.Route = routeDaemon
			case positionals[1] != "stop":
				return parsedArgs{}, nil, fmt.Errorf("unknown daemon action %q (only 'stop' is supported)", positionals[1])
			case len(positionals) > 2:
				return parsedArgs{}, nil, fmt.Errorf("daemon stop does not accept arguments: %s", strings.Join(positionals[2:], " "))
			default:
				parsed.Route = routeDaemonStop
			}
		case "status":
			if err := rejectCommandArgs("status", positionals[1:]); err != nil {
				return parsedArgs{}, nil, err
			}
			parsed.Route = routeStatus
		case "reset":
			// The scope is REQUIRED and has no default. A bare `reset` that silently
			// picked one would be the most dangerous possible convenience: the scopes
			// differ by how much of this project's state they destroy.
			if len(positionals) < 2 {
				return parsedArgs{}, nil, fmt.Errorf("reset needs a scope:\n%s", cli.ResetUsage())
			}
			if len(positionals) > 2 {
				return parsedArgs{}, nil, fmt.Errorf("reset accepts one scope, got: %s", strings.Join(positionals[1:], " "))
			}
			scope, ok := cli.ParseResetScope(positionals[1])
			if !ok {
				return parsedArgs{}, nil, fmt.Errorf("unknown reset scope %q:\n%s", positionals[1], cli.ResetUsage())
			}
			parsed.Route = routeReset
			parsed.ResetScope = scope
			parsed.ResetOptions = cli.ResetOptions{Yes: *yes, NoBackup: *noBackup}
		case "support-bundle":
			if err := rejectCommandArgs("support-bundle", positionals[1:]); err != nil {
				return parsedArgs{}, nil, err
			}
			parsed.Route = routeSupportBundle
			parsed.SupportBundle = cli.SupportBundleOptions{Out: *bundleOut, Yes: *yes, IncludeAudit: *bundleAudit}
		}
		if parsed.Route != routeDefault {
			if *stdio && parsed.Route != routeHost && parsed.Route != routeMCP {
				return parsedArgs{}, nil, stdioRequiresHostError()
			}
			// A pin is meaningless on a route that never runs a turn, and --timeout's
			// documented "silently ignored elsewhere" is the WRONG precedent to follow
			// here: this whole flag exists because a --skill that does nothing looks
			// exactly like one that worked. Say so at the argument boundary instead.
			if len(skills) > 0 && !routeRunsTurns(parsed.Route) {
				return parsedArgs{}, nil, fmt.Errorf("--skill has no effect on %q, which never runs a turn", positionals[0])
			}
			return parsed, fs, nil
		}
	}

	if *stdio {
		return parsedArgs{}, nil, stdioRequiresHostError()
	}
	// Two prompt sources at once is a MISTAKE, not a precedence question. Picking one
	// silently would run a prompt the caller can see they also passed the other way,
	// which is the worst possible outcome for a harness whose whole job is reproducing
	// an exact question.
	if *promptFile != "" && len(positionals) > 0 {
		return parsedArgs{}, nil, fmt.Errorf("--prompt-file and a prompt argument cannot be combined; pass the prompt one way")
	}
	if len(positionals) > 0 {
		// Join remaining tokens so an unquoted multi-word prompt still works; a single
		// quoted arg passes through unchanged.
		parsed.Options.Prompt = strings.Join(positionals, " ")
		parsed.Options.HasPrompt = true
	}
	// HasPrompt WITHOUT Prompt: parseArgs is deliberately I/O-free, so the text arrives
	// later (RunOneShot, inside the --timeout bound). Saying "there is a prompt" here is
	// what routes the run to one-shot and satisfies the --json check below — both of
	// which are decisions about whether a prompt exists, not about what it says.
	if *promptFile != "" {
		parsed.Options.HasPrompt = true
	}
	if *jsonOut && !parsed.Options.HasPrompt && !*multiTurn {
		return parsedArgs{}, nil, fmt.Errorf("--json requires a prompt (or --multi-turn to read one prompt per line from stdin)")
	}

	return parsed, fs, nil
}

// checkRouteScopedFlags refuses a flag that only means something on one route.
//
// --timeout's "silently ignored elsewhere" is the wrong precedent for this one, for the
// same reason --skill does not follow it: an operator types --allow-delegated-approvals
// to make a deliberate decision about whether the agent on the other end of the pipe may
// approve mutations, and a flag that quietly does nothing looks exactly like one that
// worked. Say so at the argument boundary.
func checkRouteScopedFlags(fs *flag.FlagSet, route route) error {
	if flagWasSet(fs, "allow-delegated-approvals") && route != routeMCP {
		return errors.New("--allow-delegated-approvals only applies to \"mcp --stdio\", " +
			"where the caller is another agent; it has no meaning on any other route")
	}
	return nil
}

// splitInterspersedArgs separates registered options from positional tokens
// without losing their order. Once `--` appears every remaining token is a
// positional, including strings beginning with '-' and reserved command names.
func splitInterspersedArgs(fs *flag.FlagSet, args []string) (flagArgs, positionals []string, help, forcePrompt bool, err error) {
	literal := false
	for i := 0; i < len(args); i++ {
		token := args[i]
		if literal {
			positionals = append(positionals, token)
			continue
		}
		if token == "--" {
			literal = true
			// Only a leading terminator opts out of subcommand routing. A trailing
			// terminator affects subsequent tokens but must not retroactively turn
			// `status --` into a prompt.
			forcePrompt = len(positionals) == 0
			continue
		}
		if token == "-h" || token == "-help" || token == "--help" {
			help = true
			continue
		}
		if token == "-" || !strings.HasPrefix(token, "-") {
			positionals = append(positionals, token)
			continue
		}

		name, inlineValue := optionName(token)
		f := fs.Lookup(name)
		if f == nil {
			return nil, nil, false, false, fmt.Errorf("unknown option %q", token)
		}
		flagArgs = append(flagArgs, token)
		if inlineValue || isBoolFlag(f) {
			continue
		}
		if i+1 >= len(args) {
			return nil, nil, false, false, fmt.Errorf("option %q requires a value", token)
		}
		i++
		flagArgs = append(flagArgs, args[i])
	}
	return flagArgs, positionals, help, forcePrompt, nil
}

func optionName(token string) (name string, inlineValue bool) {
	name = strings.TrimPrefix(token, "--")
	if name == token {
		name = strings.TrimPrefix(token, "-")
	}
	if i := strings.IndexByte(name, '='); i >= 0 {
		return name[:i], true
	}
	return name, false
}

// skillIDFlags accumulates a repeatable --skill. Go's stock FlagSet has no repeatable
// string, and this is the first flag in the binary that needs one.
//
// It rejects an empty occurrence for the same reason the empty-value guard below rejects
// `--state-dir=`: a harness expanding an unset shell variable produces `--skill=`, and
// quietly ignoring it would run unpinned — the one outcome this flag exists to prevent.
// Exact repeats are collapsed (first occurrence wins) rather than rejected, because
// naming the same runbook twice is a harmless script artifact, not a mistake worth
// failing a launch over.
//
// Case is preserved and commas are not split: the id is the backend's own key.
type skillIDFlags []string

func (v *skillIDFlags) String() string { return strings.Join(*v, ",") }

func (v *skillIDFlags) Set(raw string) error {
	id := strings.TrimSpace(raw)
	if id == "" {
		return errors.New("--skill was given an empty value; omit the flag to let the backend's selector choose")
	}
	for _, have := range *v {
		if have == id {
			return nil
		}
	}
	*v = append(*v, id)
	return nil
}

func isBoolFlag(f *flag.Flag) bool {
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}

func flagWasSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) { set = set || f.Name == name })
	return set
}

// routeRunsTurns reports whether a route ever opens a backend turn, which is the only
// thing a pinned skill can affect. `host` and `mcp` both serve sessions that do, so a
// process-level --skill is a legitimate default for them (the MCP client can still
// override it per session.open); doctor/status/daemon/reset/support-bundle never do.
func routeRunsTurns(r route) bool {
	switch r {
	case routeDefault, routeHost, routeMCP:
		return true
	default:
		return false
	}
}

func rejectCommandArgs(command string, args []string) error {
	if len(args) == 0 {
		return nil
	}
	return fmt.Errorf("%s does not accept arguments: %s", command, strings.Join(args, " "))
}

func stdioRequiresHostError() error {
	return fmt.Errorf("--stdio is only valid with the host or mcp commands")
}

// writeUsage owns the human help layout instead of flag.PrintDefaults: requested
// help goes to stdout, long options use their documented `--` spelling, and
// compatibility-only flags stay accepted without cluttering the public surface.
func writeUsage(w io.Writer, buildVersion string) {
	fmt.Fprintf(w, "daintree-assistant %s — Daintree's local operations officer.\n\n", buildVersion)
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  daintree-assistant [options]                 interactive line REPL")
	fmt.Fprintln(w, "  daintree-assistant [options] <prompt...>     run one prompt and exit")
	fmt.Fprintln(w, "  daintree-assistant [options] <command>")
	fmt.Fprintln(w, "\nCommands:")
	fmt.Fprintln(w, "  doctor              check backend, MCP, project, and permissions")
	fmt.Fprintln(w, "  status              show supervisor health and live work")
	fmt.Fprintln(w, "  daemon              run the project supervisor in the foreground")
	fmt.Fprintln(w, "  daemon stop         stop the project supervisor")
	fmt.Fprintln(w, "  host [--stdio]      serve embedded-host NDJSON over stdio")
	fmt.Fprintln(w, "  mcp [--stdio]       serve the assistant AS an MCP server, for another agent to drive")
	fmt.Fprintln(w, "  support-bundle      write a redacted diagnostics archive to send to a maintainer")
	fmt.Fprint(w, cli.ResetUsage())
	fmt.Fprintln(w, "\nOptions:")
	fmt.Fprintln(w, "  --project PATH      project root (default: current directory)")
	fmt.Fprintln(w, "  --tier TIER         supervisor, operator, or system")
	fmt.Fprintln(w, "  --offline           run without the Daintree MCP connection")
	fmt.Fprintln(w, "  --classic           deprecated no-op (the line REPL is the only interactive mode)")
	fmt.Fprintln(w, "  --json              emit JSONL for a one-shot prompt")
	fmt.Fprintln(w, "  --mcp-url URL       Daintree MCP URL (env: DAINTREE_MCP_URL)")
	fmt.Fprintln(w, "  --mcp-token TOKEN   Daintree MCP token (env: DAINTREE_MCP_TOKEN)")
	fmt.Fprintln(w, "  --backend-url URL   assistant backend (env: DAINTREE_BACKEND_URL)")
	fmt.Fprintln(w, "  --api-key-file PATH read the API key from a file (env: DAINTREE_API_KEY)")
	fmt.Fprintln(w, "  --prompt-file PATH  read the one-shot prompt from a file ('-' for stdin)")
	fmt.Fprintln(w, "  --multi-turn        run one prompt per stdin line as a conversation in one")
	fmt.Fprintln(w, "                      session, all of it one JSONL transcript (requires --json)")
	fmt.Fprintln(w, "  --state-dir PATH    state root (env: DAINTREE_ASSISTANT_STATE_DIR)")
	fmt.Fprintln(w, "  --log-dir PATH      debug-log directory (env: DAINTREE_ASSISTANT_LOG_DIR)")
	fmt.Fprintln(w, "  --project-id ID     Daintree project id (env: DAINTREE_PROJECT_ID)")
	fmt.Fprintln(w, "  --window-id ID      Daintree window id (env: DAINTREE_WINDOW_ID)")
	fmt.Fprintln(w, "  --project-instructions-file PATH")
	fmt.Fprintln(w, "                      read DAINTREE.md content from a file instead of the project")
	fmt.Fprintln(w, "  --auto-approve      run mutating tools without confirmation")
	fmt.Fprintln(w, "  --allow-delegated-approvals")
	fmt.Fprintln(w, "                      mcp --stdio: let a session use approvals:\"delegate\",")
	fmt.Fprintln(w, "                      where the CALLING AGENT answers each confirmation")
	fmt.Fprintln(w, "  --debug-log         write the session trace to the log directory")
	fmt.Fprintln(w, "  --timeout DURATION  cancel a one-shot run after this long (e.g. 10m; 0 = no limit)")
	fmt.Fprintln(w, "  --run-scheduler     run the scheduler during a one-shot and await its async work")
	fmt.Fprintln(w, "                      before exiting (requires --timeout)")
	fmt.Fprintln(w, "  --skill ID          load this backend runbook on every turn (repeatable)")
	fmt.Fprintln(w, "  --list-skills       print the runbooks this backend can load, and exit")
	fmt.Fprintln(w, "  --yes               skip the reset confirmation (required without a TTY)")
	fmt.Fprintln(w, "  --no-backup         skip the reset's timestamped backup")
	fmt.Fprintln(w, "  --out PATH          support-bundle destination")
	fmt.Fprintln(w, "  --include-audit     add recent tool names + outcomes to the support bundle")
	fmt.Fprintln(w, "  --version           print the version and exit")
	fmt.Fprintln(w, "  -h, --help          show this help")
	fmt.Fprintln(w, "  --                  end option parsing; before the first word, force a prompt")
	fmt.Fprintln(w, "\nUse -- before a prompt that begins with an option or command name.")
	fmt.Fprintln(w, "Without Daintree MCP credentials, the assistant runs in degraded local mode.")
}
