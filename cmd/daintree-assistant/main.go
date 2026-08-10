// Command daintree-assistant is the single static-binary entrypoint for Daintree's
// local orchestration assistant. It parses the CLI surface, then routes to exactly
// one of: environment/status commands, the persistent supervisor, the embedded
// stdio host, a one-shot prompt, or the interactive path (Bubble Tea cockpit on a
// TTY, classic REPL otherwise). All real wiring lives in internal/cli; this file is
// the thin flag/route shim plus main's cockpit and build-version seams.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/daintreehq/assistant/internal/cli"
	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/ui"
)

// version is injected at build time via -ldflags "-X main.version=…" (see Makefile).
// It defaults to "dev" for a plain `go build`/`go run` with no ldflags. It is the
// ONE value main owns end-to-end: stamped into the cockpit masthead and printed by
// --version. Keep the variable named exactly `version` — the Makefile's ldflag path
// (`-X main.version=$(VERSION)`) is byte-coupled to it.
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

	// Stamp the build version into the cockpit masthead before any UI is constructed
	// (UIVersion is a package var read at model-init time, so set it up front), and
	// into the cli layer for daemon descriptors/status.
	ui.UIVersion = version
	cli.SetVersion(version)

	// Interactive mode owns Ctrl-C itself: the cockpit receives it as a raw key and
	// the classic REPL uses it to cancel only the current turn. Capturing SIGINT in
	// this parent context would permanently poison later classic turns. SIGTERM is
	// always a process shutdown; one-shot and subcommand paths additionally use
	// SIGINT as ordinary context cancellation.
	signals := []os.Signal{syscall.SIGTERM}
	if route != routeDefault || opts.HasPrompt {
		signals = append(signals, os.Interrupt)
	}
	ctx, stop := signal.NotifyContext(context.Background(), signals...)
	defer stop()

	// The cockpit runner is main's seam: internal/cli only knows the CockpitRunner
	// type, internal/ui is the sole Bubble Tea importer, and main is where the two
	// meet (so the UI-boundary invariant holds — cli never imports ui directly).
	opts.Cockpit = ui.Run

	var code int
	switch route {
	case routeDoctor:
		code = cli.RunDoctor(ctx, opts)
	case routeHost:
		code = cli.RunHost(ctx, opts)
	case routeDaemon:
		code = cli.RunDaemon(ctx, opts)
	case routeDaemonStop:
		code = cli.RunDaemonStop(ctx, opts)
	case routeStatus:
		code = cli.RunStatus(ctx, opts)
	case routeLogin:
		code = cli.RunLoginCommand(ctx, opts)
	case routeLogout:
		code = cli.RunLogoutCommand(ctx, opts)
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
	routeDaemon
	routeDaemonStop
	routeStatus
	routeLogin
	routeLogout
)

// parsedArgs is the pure result of command-line parsing. main is the only place
// that prints or exits, which keeps the argument contract table-testable.
type parsedArgs struct {
	Options cli.Options
	Route   route
	Help    bool
	Version bool
}

// parseArgs parses the CLI surface while preserving two useful properties that
// Go's stock FlagSet does not combine: options may be interspersed with a prompt,
// and `--` permanently ends option/subcommand parsing. The latter matters for
// prompts such as `-- "status"` and `-- "--summarize this"`.
func parseArgs(args []string) (parsedArgs, error) {
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
	)

	flagArgs, positionals, help, forcePrompt, err := splitInterspersedArgs(fs, args)
	if err != nil {
		return parsedArgs{}, err
	}
	if err := fs.Parse(flagArgs); err != nil {
		return parsedArgs{}, err
	}
	if help {
		return parsedArgs{Help: true}, nil
	}
	if *showVer {
		return parsedArgs{Version: true}, nil
	}

	tierValue := strings.TrimSpace(*tier)
	if flagWasSet(fs, "tier") && !domain.Tier(tierValue).IsValid() {
		return parsedArgs{}, fmt.Errorf("invalid --tier %q (choose supervisor, operator, or system)", *tier)
	}

	opts := cli.Options{
		McpURL:   *mcpURL,
		McpToken: *mcpToken,
		Project:  *project,
		Tier:     tierValue,
		Offline:  *offline,
		Classic:  *classic,
		JSON:     *jsonOut,
		Inline:   *inline, // accepted and ignored (deprecated)
	}

	parsed := parsedArgs{Options: opts, Route: routeDefault}
	// --json is unambiguously a one-shot request, so a prompt that happens to be
	// named "status" or "doctor" remains a prompt. `--` provides the same escape
	// for the human-output path.
	if len(positionals) > 0 && !forcePrompt && !*jsonOut {
		switch positionals[0] {
		case "doctor":
			if err := rejectCommandArgs("doctor", positionals[1:]); err != nil {
				return parsedArgs{}, err
			}
			parsed.Route = routeDoctor
		case "host":
			if err := rejectCommandArgs("host", positionals[1:]); err != nil {
				return parsedArgs{}, err
			}
			parsed.Route = routeHost
		case "daemon":
			switch {
			case len(positionals) == 1:
				parsed.Route = routeDaemon
			case positionals[1] != "stop":
				return parsedArgs{}, fmt.Errorf("unknown daemon action %q (only 'stop' is supported)", positionals[1])
			case len(positionals) > 2:
				return parsedArgs{}, fmt.Errorf("daemon stop does not accept arguments: %s", strings.Join(positionals[2:], " "))
			default:
				parsed.Route = routeDaemonStop
			}
		case "status":
			if err := rejectCommandArgs("status", positionals[1:]); err != nil {
				return parsedArgs{}, err
			}
			parsed.Route = routeStatus
		case "login":
			if err := rejectCommandArgs("login", positionals[1:]); err != nil {
				return parsedArgs{}, err
			}
			parsed.Route = routeLogin
		case "logout":
			if err := rejectCommandArgs("logout", positionals[1:]); err != nil {
				return parsedArgs{}, err
			}
			parsed.Route = routeLogout
		}
		if parsed.Route != routeDefault {
			if *stdio && parsed.Route != routeHost {
				return parsedArgs{}, stdioRequiresHostError()
			}
			return parsed, nil
		}
	}

	if *stdio {
		return parsedArgs{}, stdioRequiresHostError()
	}
	if len(positionals) > 0 {
		// Join remaining tokens so an unquoted multi-word prompt still works; a single
		// quoted arg passes through unchanged.
		parsed.Options.Prompt = strings.Join(positionals, " ")
		parsed.Options.HasPrompt = true
	}
	if *jsonOut && !parsed.Options.HasPrompt {
		return parsedArgs{}, fmt.Errorf("--json requires a prompt")
	}

	return parsed, nil
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

func isBoolFlag(f *flag.Flag) bool {
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}

func flagWasSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) { set = set || f.Name == name })
	return set
}

func rejectCommandArgs(command string, args []string) error {
	if len(args) == 0 {
		return nil
	}
	return fmt.Errorf("%s does not accept arguments: %s", command, strings.Join(args, " "))
}

func stdioRequiresHostError() error {
	return fmt.Errorf("--stdio is only valid with the host command")
}

// writeUsage owns the human help layout instead of flag.PrintDefaults: requested
// help goes to stdout, long options use their documented `--` spelling, and
// compatibility-only flags stay accepted without cluttering the public surface.
func writeUsage(w io.Writer, buildVersion string) {
	fmt.Fprintf(w, "daintree-assistant %s — Daintree's local operations officer.\n\n", buildVersion)
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  daintree-assistant [options]                 interactive cockpit")
	fmt.Fprintln(w, "  daintree-assistant [options] <prompt...>     run one prompt and exit")
	fmt.Fprintln(w, "  daintree-assistant [options] <command>")
	fmt.Fprintln(w, "\nCommands:")
	fmt.Fprintln(w, "  login               choose a backend endpoint and store an API key")
	fmt.Fprintln(w, "  logout              forget the stored endpoint and API key")
	fmt.Fprintln(w, "  doctor              check backend, MCP, project, and permissions")
	fmt.Fprintln(w, "  status              show supervisor health and live work")
	fmt.Fprintln(w, "  daemon              run the project supervisor in the foreground")
	fmt.Fprintln(w, "  daemon stop         stop the project supervisor")
	fmt.Fprintln(w, "  host [--stdio]      serve embedded-host NDJSON over stdio")
	fmt.Fprintln(w, "\nOptions:")
	fmt.Fprintln(w, "  --project PATH      project root (default: current directory)")
	fmt.Fprintln(w, "  --tier TIER         supervisor, operator, or system")
	fmt.Fprintln(w, "  --offline           run without the Daintree MCP connection")
	fmt.Fprintln(w, "  --classic           use the line-oriented REPL instead of the cockpit")
	fmt.Fprintln(w, "  --json              emit JSONL for a one-shot prompt")
	fmt.Fprintln(w, "  --mcp-url URL       Daintree MCP URL (env: DAINTREE_MCP_URL)")
	fmt.Fprintln(w, "  --mcp-token TOKEN   Daintree MCP token (env: DAINTREE_MCP_TOKEN)")
	fmt.Fprintln(w, "  --version           print the version and exit")
	fmt.Fprintln(w, "  -h, --help          show this help")
	fmt.Fprintln(w, "  --                  end option parsing; before the first word, force a prompt")
	fmt.Fprintln(w, "\nUse -- before a prompt that begins with an option or command name.")
	fmt.Fprintln(w, "Without Daintree MCP credentials, the assistant runs in degraded local mode.")
}
