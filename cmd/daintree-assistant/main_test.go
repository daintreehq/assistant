package main

import (
	"bytes"
	"strings"
	"testing"
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
			if got.Options.Offline != tt.wantOffline {
				t.Errorf("Offline = %v, want %v", got.Options.Offline, tt.wantOffline)
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
		{name: "stdio without host", args: []string{"--stdio"}, want: "only valid with the host command"},
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
