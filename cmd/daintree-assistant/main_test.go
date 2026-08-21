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
	for _, name := range []string{"api-key-file", "state-dir", "backend-url", "log-dir", "project", "mcp-url", "mcp-token"} {
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
