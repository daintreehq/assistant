// Command daintree-assistant is the single static-binary entrypoint for Daintree's
// local orchestration assistant. It parses the CLI surface, then routes to exactly
// one of: the `doctor` environment check, the `host --stdio` embedded transport, a
// one-shot prompt run, or the interactive path (Bubble Tea cockpit on a TTY, classic
// REPL otherwise). All the real wiring lives in internal/cli; this file is the thin
// flag/route shim plus the two seams main owns: the cockpit runner (ui.Run) and the
// build version stamped into the masthead.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/daintreehq/daintree-assistant/internal/cli"
	"github.com/daintreehq/daintree-assistant/internal/ui"
)

// version is injected at build time via -ldflags "-X main.version=…" (see Makefile).
// It defaults to "dev" for a plain `go build`/`go run` with no ldflags. It is the
// ONE value main owns end-to-end: stamped into the cockpit masthead and printed by
// --version. Keep the variable named exactly `version` — the Makefile's ldflag path
// (`-X main.version=$(VERSION)`) is byte-coupled to it.
var version = "dev"

func main() {
	opts, route := parseArgs(os.Args[1:])

	// Stamp the build version into the cockpit masthead before any UI is constructed
	// (UIVersion is a package var read at model-init time, so set it up front).
	ui.UIVersion = version

	// Cancel the run context on SIGINT/SIGTERM for the cooked-mode paths (one-shot,
	// classic REPL, host). The cockpit runs the terminal in raw mode, so Ctrl-C never
	// reaches us as a signal there — Bubble Tea reads it as a key — and this is inert.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
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
	default:
		code = cli.Run(ctx, opts)
	}
	os.Exit(code)
}

// route is the top-level dispatch decided purely from argv: a leading `doctor` or
// `host` subcommand wins; everything else falls through to cli.Run, which itself
// splits one-shot (a prompt arg) from interactive.
type route int

const (
	routeDefault route = iota
	routeDoctor
	routeHost
)

// parseArgs parses the CLI surface into cli.Options + a route: the flags, the
// `doctor` subcommand, and `host --stdio`. Unlike Go's stock flag parsing, flags
// and the positional prompt may be interspersed (Daintree/users rely on forms
// like `--json "prompt"`); the loop below re-parses after each positional so a
// trailing flag is still seen.
func parseArgs(args []string) (cli.Options, route) {
	fs := flag.NewFlagSet("daintree-assistant", flag.ContinueOnError)

	var (
		mcpURL   = fs.String("mcp-url", "", "Daintree MCP base URL (else $DAINTREE_MCP_URL)")
		mcpToken = fs.String("mcp-token", "", "Daintree MCP bearer token (else $DAINTREE_MCP_TOKEN)")
		project  = fs.String("project", "", "project path (DAINTREE.md is read from its root; default cwd)")
		tier     = fs.String("tier", "", "permission tier: supervisor | operator | system")
		offline  = fs.Bool("offline", false, "no network calls (degraded local mode)")
		classic  = fs.Bool("classic", false, "force the classic line REPL (no cockpit)")
		inline   = fs.Bool("inline", false, "DEPRECATED NO-OP (the cockpit is always inline)")
		jsonOut  = fs.Bool("json", false, "one-shot JSONL event stream to stdout (requires a prompt)")
		stdio    = fs.Bool("stdio", false, "embedded host over stdio NDJSON (use with the `host` subcommand)")
		showVer  = fs.Bool("version", false, "print the version and exit")
	)

	fs.Usage = func() {
		w := fs.Output()
		fmt.Fprintf(w, "daintree-assistant %s — Daintree's local orchestration assistant (DeepSeek-powered).\n\n", version)
		fmt.Fprintln(w, "Usage:")
		fmt.Fprintln(w, "  daintree-assistant [flags] [prompt]   interactive cockpit, or one-shot when a prompt is given")
		fmt.Fprintln(w, "  daintree-assistant doctor             environment check (MCP / DeepSeek key / project / tier)")
		fmt.Fprintln(w, "  daintree-assistant host --stdio       embedded host: stdio NDJSON transport")
		fmt.Fprintln(w, "\nFlags:")
		fs.PrintDefaults()
		fmt.Fprintln(w, "\nWithout DEEPSEEK_API_KEY, read-only slash commands still work: /tools, /skills, /doctor.")
		fmt.Fprintln(w, "A project .env may set DEEPSEEK_API_KEY and DAINTREE_{LARGE,MEDIUM,SMALL}_MODEL;")
		fmt.Fprintln(w, "the Daintree MCP URL/token come only from the real environment, never a project .env.")
	}

	// Interspersed parse: collect every non-flag token as a positional while still
	// letting flags appear before, between, and after them.
	var positionals []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			// flag already wrote the error + usage to stderr; -h/--help returns ErrHelp.
			if err == flag.ErrHelp {
				os.Exit(0)
			}
			os.Exit(2)
		}
		rest = fs.Args()
		if len(rest) == 0 {
			break
		}
		positionals = append(positionals, rest[0])
		rest = rest[1:]
	}

	if *showVer {
		fmt.Printf("daintree-assistant %s\n", version)
		os.Exit(0)
	}

	opts := cli.Options{
		McpURL:   *mcpURL,
		McpToken: *mcpToken,
		Project:  *project,
		Tier:     *tier,
		Offline:  *offline,
		Classic:  *classic,
		JSON:     *jsonOut,
		Inline:   *inline, // accepted and ignored (deprecated)
	}

	// A leading `doctor`/`host` positional is a subcommand; anything else is the
	// one-shot prompt. `host` implies the stdio transport (the only one), so the
	// bare `--stdio` flag is just an accepted alias and need not be set.
	if len(positionals) > 0 {
		switch positionals[0] {
		case "doctor":
			return opts, routeDoctor
		case "host":
			_ = *stdio // accepted for `host --stdio`; host.Run always uses stdio
			return opts, routeHost
		}
		// Join remaining tokens so an unquoted multi-word prompt still works; a single
		// quoted arg passes through unchanged.
		opts.Prompt = strings.Join(positionals, " ")
		opts.HasPrompt = true
	}

	return opts, routeDefault
}
