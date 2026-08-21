package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestParseArgsRoutesAndPrompts(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		wantRoute   route
		wantPrompt  string
		wantJSON    bool
		wantOffline bool
	}{
		{name: "interactive", wantRoute: routeDefault},
		{name: "interspersed options", args: []string{"hello", "--offline", "from", "Daintree"}, wantRoute: routeDefault, wantPrompt: "hello from Daintree", wantOffline: true},
		{name: "doctor", args: []string{"--offline", "doctor"}, wantRoute: routeDoctor, wantOffline: true},
		{name: "host stdio compatibility", args: []string{"host", "--stdio"}, wantRoute: routeHost},
		{name: "mcp", args: []string{"mcp"}, wantRoute: routeMCP},
		{name: "mcp stdio compatibility", args: []string{"mcp", "--stdio"}, wantRoute: routeMCP},
		{name: "daemon", args: []string{"daemon"}, wantRoute: routeDaemon},
		{name: "daemon stop", args: []string{"--project", "/tmp/example", "daemon", "stop"}, wantRoute: routeDaemonStop},
		{name: "status", args: []string{"status"}, wantRoute: routeStatus},
		{name: "json reserves no command words", args: []string{"--json", "status"}, wantRoute: routeDefault, wantPrompt: "status", wantJSON: true},
		{name: "terminator forces command word prompt", args: []string{"--", "doctor"}, wantRoute: routeDefault, wantPrompt: "doctor"},
		{name: "terminator preserves hyphen tokens", args: []string{"--", "first", "--second"}, wantRoute: routeDefault, wantPrompt: "first --second"},
		{name: "trailing terminator keeps command", args: []string{"status", "--"}, wantRoute: routeStatus},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseArgs(tt.args)
			if err != nil {
				t.Fatalf("parseArgs() error = %v", err)
			}
			if got.Route != tt.wantRoute {
				t.Errorf("route = %v, want %v", got.Route, tt.wantRoute)
			}
			if got.Options.Prompt != tt.wantPrompt {
				t.Errorf("prompt = %q, want %q", got.Options.Prompt, tt.wantPrompt)
			}
			if got.Options.HasPrompt != (tt.wantPrompt != "") {
				t.Errorf("HasPrompt = %v, want %v", got.Options.HasPrompt, tt.wantPrompt != "")
			}
			if got.Options.JSON != tt.wantJSON {
				t.Errorf("JSON = %v, want %v", got.Options.JSON, tt.wantJSON)
			}
			gotOffline := got.Options.Offline != nil && *got.Options.Offline
			if gotOffline != tt.wantOffline {
				t.Errorf("Offline = %v, want %v", gotOffline, tt.wantOffline)
			}
		})
	}
}

func TestParseArgsRejectsAmbiguousOrInvalidInvocations(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "invalid explicit tier", args: []string{"--tier", "operatr", "status"}, want: "invalid --tier"},
		{name: "empty explicit tier", args: []string{"--tier=", "status"}, want: "invalid --tier"},
		{name: "status arguments", args: []string{"status", "unexpected"}, want: "status does not accept arguments"},
		{name: "doctor arguments", args: []string{"doctor", "unexpected"}, want: "doctor does not accept arguments"},
		{name: "terminator after command does not hide arguments", args: []string{"doctor", "--", "unexpected"}, want: "doctor does not accept arguments"},
		{name: "unknown daemon action", args: []string{"daemon", "restart"}, want: "unknown daemon action"},
		{name: "daemon stop arguments", args: []string{"daemon", "stop", "now"}, want: "daemon stop does not accept arguments"},
		{name: "stdio without host", args: []string{"--stdio"}, want: "only valid with the host or mcp commands"},
		{name: "stdio with an unrelated command", args: []string{"status", "--stdio"}, want: "only valid with the host or mcp commands"},
		{name: "mcp arguments", args: []string{"mcp", "unexpected"}, want: "mcp does not accept arguments"},
		{name: "json without prompt", args: []string{"--json"}, want: "--json requires a prompt"},
		{name: "unknown option", args: []string{"--clasik"}, want: "unknown option"},
		{name: "missing option value", args: []string{"--project"}, want: "requires a value"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseArgs(tt.args)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("parseArgs() error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestParseArgsHelpAndVersionArePureOutcomes(t *testing.T) {
	help, err := parseArgs([]string{"--help"})
	if err != nil || !help.Help {
		t.Fatalf("--help = %+v, %v", help, err)
	}
	ver, err := parseArgs([]string{"--version"})
	if err != nil || !ver.Version {
		t.Fatalf("--version = %+v, %v", ver, err)
	}
}

func TestUsageIsCurrentAndKeepsCompatibilityFlagsHidden(t *testing.T) {
	var out bytes.Buffer
	writeUsage(&out, "test-version")
	help := out.String()
	for _, want := range []string{
		"daintree-assistant test-version",
		"doctor",
		"backend, MCP, project, and permissions",
		"host [--stdio]",
		"mcp [--stdio]",
		"--project PATH",
		"--json",
		"Use -- before a prompt",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("usage missing %q:\n%s", want, help)
		}
	}
	for _, stale := range []string{"DeepSeek", "DEEPSEEK_API_KEY", "--inline", "-stdio host"} {
		if strings.Contains(help, stale) {
			t.Errorf("usage contains stale/hidden text %q:\n%s", stale, help)
		}
	}
}

// TestParseArgsCarriesHarnessFlags pins the flags a scripted caller needs so it never
// has to rewrite the process environment to say something argv says perfectly well.
// They must survive alongside a prompt, since a one-shot run is where they matter.
func TestParseArgsCarriesHarnessFlags(t *testing.T) {
	got, err := parseArgs([]string{
		"--backend-url", "http://127.0.0.1:8473",
		"--api-key-file", "/tmp/key.txt",
		"--state-dir", "/tmp/state",
		"--log-dir", "/tmp/logs",
		"--project-id", "proj_fake_test",
		"--window-id", "win_fake_test",
		"--project-instructions-file", "/tmp/brief.md",
		"--auto-approve",
		"--debug-log",
		"--timeout", "90s",
		"--json", "which worktrees are ready?",
	})
	if err != nil {
		t.Fatalf("parseArgs() error = %v", err)
	}
	o := got.Options
	if got.Route != routeDefault || !o.HasPrompt || o.Prompt != "which worktrees are ready?" {
		t.Fatalf("route/prompt = %v/%q", got.Route, o.Prompt)
	}
	if o.BackendURL != "http://127.0.0.1:8473" || o.APIKeyFile != "/tmp/key.txt" {
		t.Errorf("backend/key = %q/%q", o.BackendURL, o.APIKeyFile)
	}
	if o.StateDir != "/tmp/state" || o.LogDir != "/tmp/logs" {
		t.Errorf("state/log dir = %q/%q", o.StateDir, o.LogDir)
	}
	if o.AutoApprove == nil || !*o.AutoApprove || o.DebugLog == nil || !*o.DebugLog {
		t.Errorf("autoApprove/debugLog = %v/%v", o.AutoApprove, o.DebugLog)
	}
	if o.Timeout != 90*time.Second {
		t.Errorf("timeout = %v, want 90s", o.Timeout)
	}
	if o.ProjectID != "proj_fake_test" || o.WindowID != "win_fake_test" {
		t.Errorf("project/window id = %q/%q", o.ProjectID, o.WindowID)
	}
	// A nonexistent path: parseArgs must capture it WITHOUT reading it, or the function
	// stops being the pure, table-testable thing every case here relies on.
	if o.ProjectInstructionsFile != "/tmp/brief.md" {
		t.Errorf("projectInstructionsFile = %q", o.ProjectInstructionsFile)
	}
	// Absent unless asked for: a plain one-shot must never be routed into the stdin loop.
	if o.MultiTurn {
		t.Error("MultiTurn = true without --multi-turn")
	}
}

// TestParseArgsCarriesMultiTurn: the flag reaches one-shot as a bare intent — there is
// no path and no prompt to capture, because the prompts arrive on stdin later, inside
// the --timeout bound like every other read.
func TestParseArgsCarriesMultiTurn(t *testing.T) {
	got, err := parseArgs([]string{"--json", "--multi-turn", "--timeout", "5m"})
	if err != nil {
		t.Fatalf("parseArgs() error = %v", err)
	}
	o := got.Options
	if got.Route != routeDefault {
		t.Fatalf("route = %v, want routeDefault", got.Route)
	}
	if !o.MultiTurn || !o.JSON {
		t.Fatalf("MultiTurn/JSON = %v/%v, want true/true", o.MultiTurn, o.JSON)
	}
	// It satisfies --json's "a run must have a prompt" rule WITHOUT setting HasPrompt:
	// there is no single prompt, which is the whole point of the flag.
	if o.HasPrompt || o.Prompt != "" {
		t.Errorf("HasPrompt/Prompt = %v/%q, want false/\"\"", o.HasPrompt, o.Prompt)
	}
}

// TestParseArgsRejectsMultiTurnMisuse: --multi-turn is a third prompt source, so it
// obeys the same "name one source" rule as the other two — and it insists on --json,
// without which it would just be a worse spelling of the classic REPL on piped stdin.
func TestParseArgsRejectsMultiTurnMisuse(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{"without --json", []string{"--multi-turn"}, "--multi-turn requires --json"},
		{"with a prompt argument", []string{"--json", "--multi-turn", "hello"}, "cannot be combined"},
		{"with --prompt-file", []string{"--json", "--multi-turn", "--prompt-file", "/tmp/p.txt"}, "cannot be combined"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := parseArgs(tt.args); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("parseArgs(%v) error = %v, want substring %q", tt.args, err, tt.want)
			}
		})
	}
}

// TestParseArgsCarriesPromptFile: the flag reaches one-shot as a PATH plus HasPrompt,
// never as text. parseArgs does no I/O — the paths below do not exist and must still
// parse — so "there is a prompt" is decided here and "what it says" much later, inside
// the --timeout bound.
func TestParseArgsCarriesPromptFile(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"a path", []string{"--prompt-file", "/tmp/does-not-exist.md"}, "/tmp/does-not-exist.md"},
		// "-" is stdin, and it must survive the explicitly-empty check that rejects
		// `--prompt-file=`.
		{"stdin", []string{"--prompt-file", "-"}, "-"},
		// The whole reason the flag exists for a harness: --json rejects a run with no
		// prompt, and a file-supplied prompt has to satisfy that check.
		{"with --json", []string{"--json", "--prompt-file", "/tmp/prompt.md"}, "/tmp/prompt.md"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseArgs(tc.args)
			if err != nil {
				t.Fatalf("parseArgs() error = %v", err)
			}
			if got.Route != routeDefault {
				t.Errorf("route = %v, want the one-shot route", got.Route)
			}
			if got.Options.PromptFile != tc.want {
				t.Errorf("PromptFile = %q, want %q", got.Options.PromptFile, tc.want)
			}
			if !got.Options.HasPrompt {
				t.Error("HasPrompt = false; a prompt file must route the run to one-shot")
			}
			if got.Options.Prompt != "" {
				t.Errorf("Prompt = %q, want empty — parseArgs must not read the file", got.Options.Prompt)
			}
		})
	}
}

// TestParseArgsRejectsTwoPromptSources: two prompts is a MISTAKE, not a precedence
// question. Silently picking one would run a prompt the caller can see they also passed
// the other way — the worst outcome for a harness whose job is reproducing an exact
// question. The `--` case matters because that is how a prompt beginning with a dash is
// passed today.
func TestParseArgsRejectsTwoPromptSources(t *testing.T) {
	for _, args := range [][]string{
		{"--prompt-file", "/tmp/prompt.md", "which worktrees are ready?"},
		{"--prompt-file", "/tmp/prompt.md", "--", "--summarize this"},
		{"--json", "--prompt-file", "/tmp/prompt.md", "hello"},
	} {
		_, err := parseArgs(args)
		if err == nil {
			t.Fatalf("%v must be rejected", args)
		}
		if !strings.Contains(err.Error(), "--prompt-file") {
			t.Errorf("error should name the flag, got: %v", err)
		}
	}
}

// TestParseArgsPromptFileDoesNotHijackCommandRoutes: a command word is chosen before the
// prompt branch, so --prompt-file is simply ignored there — the same convention
// --timeout already follows. It matters most for the stdio routes: "-" must never be
// read off a stream that is carrying the protocol itself.
func TestParseArgsPromptFileDoesNotHijackCommandRoutes(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want route
	}{
		{[]string{"--prompt-file", "-", "doctor"}, routeDoctor},
		{[]string{"--prompt-file", "-", "host", "--stdio"}, routeHost},
		{[]string{"--prompt-file", "-", "mcp", "--stdio"}, routeMCP},
	} {
		got, err := parseArgs(tc.args)
		if err != nil {
			t.Fatalf("parseArgs(%v) error = %v", tc.args, err)
		}
		if got.Route != tc.want {
			t.Errorf("parseArgs(%v) route = %v, want %v", tc.args, got.Route, tc.want)
		}
		if got.Options.HasPrompt {
			t.Errorf("parseArgs(%v) set HasPrompt on a command route", tc.args)
		}
	}
}

// TestParseArgsRejectsNegativeTimeout: a negative duration would make the run context
// expire before the first token, which reads as an instant mystery cancellation. Fail
// at the argument boundary instead.
func TestParseArgsRejectsNegativeTimeout(t *testing.T) {
	if _, err := parseArgs([]string{"--timeout", "-5s", "hello"}); err == nil {
		t.Fatal("negative --timeout must be rejected")
	}
}

// TestUsageDocumentsHarnessFlags: a flag nobody can discover is a flag nobody uses.
func TestUsageDocumentsHarnessFlags(t *testing.T) {
	var out bytes.Buffer
	writeUsage(&out, "test-version")
	help := out.String()
	for _, want := range []string{
		"--backend-url URL", "--api-key-file PATH", "--state-dir PATH",
		"--log-dir PATH", "--auto-approve", "--debug-log", "--timeout DURATION",
		"--prompt-file PATH", "--multi-turn", "--project-id ID", "--window-id ID",
		"--project-instructions-file PATH",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("usage missing %q:\n%s", want, help)
		}
	}
}

// TestParseArgsExplicitFalseBeatsEnv: --auto-approve=false must reach config as a
// non-nil false, or DAINTREE_ASSISTANT_AUTO_APPROVE=1 keeps winning against someone who
// explicitly turned it off — on the one flag that decides whether mutating tools run
// unattended. An ABSENT flag must stay nil so the env still decides.
func TestParseArgsExplicitFalseBeatsEnv(t *testing.T) {
	got, err := parseArgs([]string{"--auto-approve=false", "--debug-log=false", "--offline=false", "hello"})
	if err != nil {
		t.Fatalf("parseArgs() error = %v", err)
	}
	for name, p := range map[string]*bool{
		"AutoApprove": got.Options.AutoApprove,
		"DebugLog":    got.Options.DebugLog,
		"Offline":     got.Options.Offline,
	} {
		if p == nil {
			t.Errorf("%s = nil, want a non-nil false (explicit off must beat the env)", name)
		} else if *p {
			t.Errorf("%s = true, want false", name)
		}
	}

	absent, err := parseArgs([]string{"hello"})
	if err != nil {
		t.Fatalf("parseArgs() error = %v", err)
	}
	if absent.Options.AutoApprove != nil || absent.Options.DebugLog != nil || absent.Options.Offline != nil {
		t.Errorf("an absent flag must stay nil so the env decides: %v %v %v",
			absent.Options.AutoApprove, absent.Options.DebugLog, absent.Options.Offline)
	}
}

// TestParseArgsRejectsExplicitlyEmptyValues: a harness that expands an unset shell
// variable produces `--api-key-file=`. Treating that as "flag absent" is the exact
// wrong-key / wrong-state-dir fallback these flags exist to prevent, so it must fail at
// the argument boundary.
func TestParseArgsRejectsExplicitlyEmptyValues(t *testing.T) {
	for _, name := range []string{"api-key-file", "prompt-file", "state-dir", "backend-url", "log-dir",
		"project", "project-id", "window-id", "project-instructions-file", "mcp-url", "mcp-token"} {
		t.Run(name, func(t *testing.T) {
			_, err := parseArgs([]string{"--" + name + "=", "hello"})
			if err == nil {
				t.Fatalf("--%s= must be rejected, not treated as absent", name)
			}
			if !strings.Contains(err.Error(), "--"+name) {
				t.Errorf("error should name the flag, got: %v", err)
			}
		})
	}
}

// TestNoRawAPIKeyFlag pins the security decision behind --api-key-file: argv is
// world-readable through `ps`, so a key must never be accepted on the command line. If
// someone later registers --api-key this fails, which is the point.
func TestNoRawAPIKeyFlag(t *testing.T) {
	_, err := parseArgs([]string{"--api-key", "sk-or-v1-fake-test-secret", "hello"})
	if err == nil {
		t.Fatal("--api-key must not be a registered flag")
	}
	if strings.Contains(err.Error(), "sk-or-v1-fake-test-secret") {
		t.Errorf("the rejection must not echo the value back: %v", err)
	}

	var out bytes.Buffer
	writeUsage(&out, "test-version")
	for _, line := range strings.Split(out.String(), "\n") {
		trimmed := strings.TrimSpace(line)
		// Exact-prefix, not substring: --api-key-file is legitimate and contains it.
		if strings.HasPrefix(trimmed, "--api-key ") || trimmed == "--api-key" {
			t.Errorf("usage advertises a raw --api-key flag: %q", line)
		}
	}
}

// TestParseArgsCarriesRunScheduler: the opt-in reaches Options as a plain bool, and
// the flag's absence leaves it false. It is deliberately NOT a *bool tri-state — there
// is no env var for an explicit false to have to beat.
func TestParseArgsCarriesRunScheduler(t *testing.T) {
	got, err := parseArgs([]string{"--run-scheduler", "--timeout", "10m", "--json", "spawn the agents"})
	if err != nil {
		t.Fatalf("parseArgs() error = %v", err)
	}
	if !got.Options.RunScheduler {
		t.Error("RunScheduler = false, want true")
	}
	if got.Options.Timeout != 10*time.Minute {
		t.Errorf("timeout = %v, want 10m", got.Options.Timeout)
	}

	off, err := parseArgs([]string{"--timeout", "10m", "hello"})
	if err != nil {
		t.Fatalf("parseArgs() error = %v", err)
	}
	if off.Options.RunScheduler {
		t.Error("RunScheduler = true without the flag, want false (the default must stay off)")
	}

	// An explicit =false is still just false; the point is that it parses rather than
	// being read as a positional prompt.
	explicit, err := parseArgs([]string{"--run-scheduler=false", "hello"})
	if err != nil {
		t.Fatalf("parseArgs(--run-scheduler=false) error = %v", err)
	}
	if explicit.Options.RunScheduler {
		t.Error("RunScheduler = true for --run-scheduler=false")
	}
}

// TestParseArgsRunSchedulerRequiresTimeout: the flag holds the run open until its async
// work settles, and settling is not guaranteed — an invocation whose terminals stay
// unreadable never advances toward expiry. Without a bound that is a script that hangs
// forever, so the missing duration is an argument-boundary error rather than a default
// nobody chose.
func TestParseArgsRunSchedulerRequiresTimeout(t *testing.T) {
	for _, args := range [][]string{
		{"--run-scheduler", "hello"},
		{"--run-scheduler", "--timeout", "0", "hello"},
		{"--run-scheduler", "--timeout", "0s", "--json", "hello"},
	} {
		if _, err := parseArgs(args); err == nil {
			t.Errorf("parseArgs(%v) = nil error, want a rejection", args)
		}
	}
	// A positive timeout is the only accepted shape.
	if _, err := parseArgs([]string{"--run-scheduler", "--timeout", "1s", "hello"}); err != nil {
		t.Errorf("parseArgs with a positive --timeout errored: %v", err)
	}
	// --timeout alone must keep working exactly as before and must NOT imply the flag.
	got, err := parseArgs([]string{"--timeout", "1s", "hello"})
	if err != nil {
		t.Fatalf("parseArgs(--timeout only) error = %v", err)
	}
	if got.Options.RunScheduler {
		t.Error("--timeout implied --run-scheduler; it must not")
	}
}

// TestUsageDocumentsRunScheduler: an opt-in nobody can discover is an opt-in nobody
// uses — and this one's --timeout requirement has to be discoverable too.
func TestUsageDocumentsRunScheduler(t *testing.T) {
	var out bytes.Buffer
	writeUsage(&out, "test-version")
	help := out.String()
	if !strings.Contains(help, "--run-scheduler") {
		t.Errorf("usage missing --run-scheduler:\n%s", help)
	}
	if !strings.Contains(help, "requires --timeout") {
		t.Errorf("usage does not mention the --timeout requirement:\n%s", help)
	}
}

// TestParseArgsRunSchedulerTimeoutRuleIsRouteIndependent pins a deliberate choice: the
// --timeout requirement is checked before the route is picked, so `daemon
// --run-scheduler` is REJECTED rather than silently ignored. Only RunOneShot reads the
// flag, so the alternative would be accepting an explicit request and doing nothing with
// it — silence is the worse answer for a flag someone typed on purpose.
func TestParseArgsRunSchedulerTimeoutRuleIsRouteIndependent(t *testing.T) {
	routes := map[string]route{"doctor": routeDoctor, "daemon": routeDaemon, "host": routeHost}
	for word := range routes {
		_, err := parseArgs([]string{"--run-scheduler", word})
		if err == nil {
			t.Errorf("parseArgs(--run-scheduler %s) = nil error, want the --timeout rejection", word)
			continue
		}
		// The SPECIFIC error, not merely any error: a route that failed for an unrelated
		// reason would otherwise pass this test while proving nothing about the rule.
		if !strings.Contains(err.Error(), "--timeout") {
			t.Errorf("parseArgs(--run-scheduler %s) error = %q, want it to name --timeout", word, err)
		}
	}
	// With a bound it parses AND still reaches the right route — the flag must not
	// disturb route selection, and the route words must not be read as prompts.
	for word, want := range routes {
		got, err := parseArgs([]string{"--run-scheduler", "--timeout", "1m", word})
		if err != nil {
			t.Errorf("parseArgs(--run-scheduler --timeout 1m %s) errored: %v", word, err)
			continue
		}
		if got.Route != want {
			t.Errorf("parseArgs(... %s).Route = %v, want %v", word, got.Route, want)
		}
		if got.Options.HasPrompt {
			t.Errorf("parseArgs(... %s) took the route word as a prompt (%q)", word, got.Options.Prompt)
		}
	}
}

// --skill is the binary's only repeatable flag, so it is the only one whose accumulation
// semantics are worth pinning. The failures below are all silent-underrun shapes: a pin
// that vanished is indistinguishable from one that worked, which is the exact ambiguity
// this flag was added to remove.
func TestParseArgsCollectsRepeatedSkills(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{name: "none", args: []string{"hi"}},
		{name: "one", args: []string{"--skill", "a.one", "hi"}, want: []string{"a.one"}},
		{name: "repeated, in order", args: []string{"--skill", "b.two", "--skill", "a.one", "hi"}, want: []string{"b.two", "a.one"}},
		{name: "inline value spelling", args: []string{"--skill=a.one", "hi"}, want: []string{"a.one"}},
		{name: "single-dash spelling", args: []string{"-skill", "a.one", "hi"}, want: []string{"a.one"}},
		{name: "exact repeat collapses", args: []string{"--skill", "a.one", "--skill", "a.one", "hi"}, want: []string{"a.one"}},
		{name: "surrounding space trimmed", args: []string{"--skill", "  a.one  ", "hi"}, want: []string{"a.one"}},
		// A comma is a legal character in an opaque backend id, so inventing it as a
		// separator would make such an id unnameable. One pin per occurrence.
		{name: "commas are not a separator", args: []string{"--skill", "a,b", "hi"}, want: []string{"a,b"}},
		{name: "interactive launch may pin", args: []string{"--skill", "a.one"}, want: []string{"a.one"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseArgs(tt.args)
			if err != nil {
				t.Fatalf("parseArgs() error = %v", err)
			}
			pins := got.Options.PinnedSkillIDs
			if len(pins) != len(tt.want) {
				t.Fatalf("pins = %v, want %v", pins, tt.want)
			}
			for i := range pins {
				if pins[i] != tt.want[i] {
					t.Fatalf("pins = %v, want %v", pins, tt.want)
				}
			}
		})
	}
}

// Every rejection here exists because the alternative is a launch that looks pinned and
// is not — or, for --list-skills, a route that quietly swallowed something the caller
// meant.
func TestParseArgsRejectsMeaninglessSkillCombinations(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		// A harness expanding an unset shell variable produces exactly this, and running
		// unpinned would be the worst possible interpretation of it.
		{name: "empty value", args: []string{"--skill=", "hi"}, want: "empty value"},
		{name: "blank value", args: []string{"--skill", "   ", "hi"}, want: "empty value"},
		// --timeout's "silently ignored elsewhere" is the wrong precedent for this flag.
		{name: "pin on doctor", args: []string{"--skill", "a.one", "doctor"}, want: "never runs a turn"},
		{name: "pin on status", args: []string{"--skill", "a.one", "status"}, want: "never runs a turn"},
		{name: "pin on daemon", args: []string{"--skill", "a.one", "daemon"}, want: "never runs a turn"},
		{name: "pin on daemon stop", args: []string{"--skill", "a.one", "daemon", "stop"}, want: "never runs a turn"},
		{name: "pin on reset", args: []string{"--skill", "a.one", "reset", "project-state"}, want: "never runs a turn"},
		{name: "pin on support-bundle", args: []string{"--skill", "a.one", "support-bundle"}, want: "never runs a turn"},
		// A trailing --skill with nothing after it is caught by splitInterspersedArgs,
		// before the value type ever sees it.
		{name: "trailing skill with no value", args: []string{"hi", "--skill"}, want: "requires a value"},
		{name: "list with a prompt", args: []string{"--list-skills", "hello"}, want: "does not take a prompt"},
		{name: "list with a command", args: []string{"--list-skills", "doctor"}, want: "does not take a prompt"},
		{name: "list plus pin", args: []string{"--list-skills", "--skill", "a.one"}, want: "do not go together"},
		{name: "list with stdio", args: []string{"--list-skills", "--stdio"}, want: "--stdio is only valid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseArgs(tt.args)
			if err == nil {
				t.Fatalf("parseArgs(%v) succeeded; want an error naming %q", tt.args, tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %q, want it to mention %q", err, tt.want)
			}
		})
	}
}

// A pin IS meaningful on the two routes that serve turns; rejecting it there would break
// the parity the issue asks for between argv and daintree.session.open.
func TestParseArgsAllowsSkillsOnTurnServingRoutes(t *testing.T) {
	for _, args := range [][]string{
		{"--skill", "a.one", "mcp"},
		{"--skill", "a.one", "mcp", "--stdio"},
		{"--skill", "a.one", "host", "--stdio"},
	} {
		got, err := parseArgs(args)
		if err != nil {
			t.Fatalf("parseArgs(%v) error = %v", args, err)
		}
		if len(got.Options.PinnedSkillIDs) != 1 {
			t.Fatalf("parseArgs(%v) dropped the pin: %v", args, got.Options.PinnedSkillIDs)
		}
	}
}

// --list-skills is carved out AHEAD of the "--json requires a prompt" rule for the same
// reason `doctor --json` is: a listing a script cannot parse is not one, and the whole
// point of the catalog is to feed --skill from a script.
func TestParseArgsRoutesListSkills(t *testing.T) {
	for _, args := range [][]string{{"--list-skills"}, {"--list-skills", "--json"}, {"--json", "--list-skills"}} {
		got, err := parseArgs(args)
		if err != nil {
			t.Fatalf("parseArgs(%v) error = %v", args, err)
		}
		if got.Route != routeListSkills {
			t.Fatalf("parseArgs(%v) route = %v, want routeListSkills", args, got.Route)
		}
	}
	// The general rule it is carved out of must still hold for everyone else.
	if _, err := parseArgs([]string{"--json"}); err == nil {
		t.Fatal("a bare --json must still require a prompt")
	}

	// A TRAILING terminator does not retroactively turn the flag into a prompt, matching
	// how `status --` keeps its command.
	if got, err := parseArgs([]string{"--list-skills", "--"}); err != nil || got.Route != routeListSkills {
		t.Fatalf(`parseArgs("--list-skills", "--") = (%v, %v), want the list route`, got.Route, err)
	}
	// A LEADING terminator does: `-- --list-skills` is someone asking the assistant about
	// the flag, which is exactly what `--` is for. It never reaches the flag set, so the
	// carve-out's indifference to forcePrompt is correct rather than a gap.
	got, err := parseArgs([]string{"--", "--list-skills"})
	if err != nil {
		t.Fatalf(`parseArgs("--", "--list-skills") error = %v`, err)
	}
	if got.Route != routeDefault || got.Options.Prompt != "--list-skills" {
		t.Fatalf("a leading terminator must make it a prompt, got route %v prompt %q", got.Route, got.Options.Prompt)
	}
}

func TestUsageDocumentsSkillFlags(t *testing.T) {
	var buf bytes.Buffer
	writeUsage(&buf, "test")
	for _, want := range []string{"--skill ID", "--list-skills", "repeatable"} {
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("usage does not mention %q:\n%s", want, buf.String())
		}
	}
}
