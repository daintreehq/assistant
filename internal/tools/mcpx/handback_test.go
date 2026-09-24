package mcpx

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// sendCatalog is an advertised catalog whose terminal.sendCommand schema does (or
// does not) carry the handback argument.
func sendCatalog(withHandback bool) []MCPToolInfo {
	props := map[string]any{
		"terminalId": map[string]any{"type": "string"},
		"command":    map[string]any{"type": "string"},
	}
	if withHandback {
		props["handback"] = map[string]any{"type": "boolean"}
	}
	return []MCPToolInfo{{
		Name:                "terminal.sendCommand",
		InputSchema:         map[string]any{"type": "object", "properties": props},
		InputSchemaProvided: true,
	}}
}

func agentOnly(id string) func(string) bool { return func(got string) bool { return got == id } }

func runSend(t *testing.T, deps Deps, terminalID, command string) {
	t.Helper()
	raw, _ := json.Marshal(sendCommandArgs{TerminalID: terminalID, Command: command})
	if res := newTerminalSendCommandTool(deps).Handle(context.Background(), raw, nil); !res.Ok {
		t.Fatalf("send failed: %+v", res.Error)
	}
}

func sentJSON(t *testing.T, m *fakeMCP) string {
	t.Helper()
	sends := m.callsTo("terminal.sendCommand")
	if len(sends) != 1 {
		t.Fatalf("terminal.sendCommand called %d times, want 1", len(sends))
	}
	b, _ := json.Marshal(sends[0])
	return string(b)
}

func TestSendCommandRequestsHandbackForAnAgentTerminal(t *testing.T) {
	m := &fakeMCP{connected: true, toolList: sendCatalog(true)}
	runSend(t, Deps{MCP: m, AgentHandback: true, IsAgentTerminal: agentOnly("terminal-a")}, "terminal-a", "  now fix the tests ✓ ")
	want := `{"command":"  now fix the tests ✓ ","handback":true,"terminalId":"terminal-a"}`
	if got := sentJSON(t, m); got != want {
		t.Errorf("sent %s, want %s", got, want)
	}
	// The send path stays as cheap as it was: a cache-first catalog read and the send,
	// never a terminal.list or getStatus to classify the target.
	if len(m.calls) != 1 {
		t.Errorf("expected exactly one MCP call, got %d: %+v", len(m.calls), m.calls)
	}
	for _, force := range m.listForces {
		if force {
			t.Error("the catalog read must be cache-first (force=false)")
		}
	}
}

// Every way the flag is withheld must produce the call this wrapper has always made.
func TestSendCommandWithoutHandbackIsByteIdenticalToTheLegacyCall(t *testing.T) {
	const legacy = `{"command":"ls","terminalId":"terminal-a"}`
	unadvertised := sendCatalog(true)
	unadvertised[0].InputSchemaProvided = false
	isAgent := agentOnly("terminal-a")
	cases := []struct {
		name string
		mcp  *fakeMCP
		deps Deps
	}{
		{"pre-feature deps", &fakeMCP{toolList: sendCatalog(true)}, Deps{}},
		{"switch off", &fakeMCP{toolList: sendCatalog(true)}, Deps{IsAgentTerminal: isAgent}},
		{"padded id is not the roster's id", &fakeMCP{toolList: sendCatalog(true)}, Deps{AgentHandback: true, IsAgentTerminal: isAgent}},
		{"shell or unknown terminal", &fakeMCP{toolList: sendCatalog(true)}, Deps{AgentHandback: true, IsAgentTerminal: agentOnly("terminal-other")}},
		{"no terminal lookup wired", &fakeMCP{toolList: sendCatalog(true)}, Deps{AgentHandback: true}},
		{"host schema lacks the argument", &fakeMCP{toolList: sendCatalog(false)}, Deps{AgentHandback: true, IsAgentTerminal: isAgent}},
		{"schema is the client's stand-in", &fakeMCP{toolList: unadvertised}, Deps{AgentHandback: true, IsAgentTerminal: isAgent}},
		{"tool missing from the catalog", &fakeMCP{}, Deps{AgentHandback: true, IsAgentTerminal: isAgent}},
		{"catalog read fails", &fakeMCP{listErr: errors.New("list failed")}, Deps{AgentHandback: true, IsAgentTerminal: isAgent}},
	}
	for _, tc := range cases {
		tc.mcp.connected = true
		tc.deps.MCP = tc.mcp
		id, want := "terminal-a", legacy
		if tc.name == "padded id is not the roster's id" {
			// Looked up exactly as it is sent: the padded id goes out untouched, and
			// without the flag.
			id, want = " terminal-a ", `{"command":"ls","terminalId":" terminal-a "}`
		}
		runSend(t, tc.deps, id, "ls")
		if got := sentJSON(t, tc.mcp); got != want {
			t.Errorf("%s: sent %s, want %s", tc.name, got, want)
		}
	}
	// A shell send never even reads the catalog: the terminal check comes first.
	if n := cases[3].mcp.listCount; n != 0 {
		t.Errorf("a non-agent send read the catalog %d times, want 0", n)
	}
}

// refusingMCP rejects any send that carries the flag, the way Daintree does for a pane
// with no agent running, and accepts the same send without it.
type refusingMCP struct {
	*fakeMCP
	transportErr error
	// fallbackErr fails the UNFLAGGED resend, to pin which failure is reported.
	fallbackErr error
}

func (r *refusingMCP) CallTool(ctx context.Context, name string, args map[string]any) (MCPCallResult, error) {
	res, err := r.fakeMCP.CallTool(ctx, name, args)
	if _, flagged := args["handback"]; flagged {
		if r.transportErr != nil {
			return MCPCallResult{}, r.transportErr
		}
		return MCPCallResult{IsError: true, Text: "handback needs an agent pane, and terminal 'terminal-a' has no agent running. Send without handback."}, nil
	}
	if r.fallbackErr != nil {
		return MCPCallResult{}, r.fallbackErr
	}
	return res, err
}

type countingObserver struct{ marks int }

func (c *countingObserver) MarkCommandSent(string, int64) { c.marks++ }

// The fallback is the last attempt, its own failure is the one reported, and the
// settle evidence is invalidated once for what is still one logical send.
func TestSendCommandFallbackFailureIsReportedAndFinal(t *testing.T) {
	m := &refusingMCP{
		fakeMCP:     &fakeMCP{connected: true, toolList: sendCatalog(true)},
		fallbackErr: errors.New("connection reset"),
	}
	obs := &countingObserver{}
	raw, _ := json.Marshal(sendCommandArgs{TerminalID: "terminal-a", Command: "ls"})
	res := newTerminalSendCommandTool(Deps{MCP: m, Observer: obs, AgentHandback: true, IsAgentTerminal: agentOnly("terminal-a")}).
		Handle(context.Background(), raw, nil)
	if res.Ok || res.Error == nil || !strings.Contains(res.Error.Message, "connection reset") {
		t.Fatalf("expected the fallback's transport failure, got %+v", res)
	}
	if n := len(m.callsTo("terminal.sendCommand")); n != 2 {
		t.Fatalf("expected exactly 2 sends, got %d", n)
	}
	if obs.marks != 1 {
		t.Errorf("MarkCommandSent called %d times, want 1", obs.marks)
	}
}

// A roster a few seconds stale can call a just-exited agent live. Daintree refuses the
// flag before sending anything, and the model has no argument to drop — so the wrapper
// drops it and the command still goes out, once.
func TestSendCommandFallsBackWhenTheHostRefusesTheFlag(t *testing.T) {
	m := &refusingMCP{fakeMCP: &fakeMCP{connected: true, toolList: sendCatalog(true)}}
	runSend(t, Deps{MCP: m, AgentHandback: true, IsAgentTerminal: agentOnly("terminal-a")}, "terminal-a", "ls")
	sends := m.callsTo("terminal.sendCommand")
	if len(sends) != 2 {
		t.Fatalf("expected the flagged send then the fallback, got %d sends", len(sends))
	}
	if sends[0]["handback"] != true {
		t.Errorf("first send should carry the flag: %+v", sends[0])
	}
	if b, _ := json.Marshal(sends[1]); string(b) != `{"command":"ls","terminalId":"terminal-a"}` {
		t.Errorf("fallback must be the legacy call, got %s", b)
	}
}

// A transport failure is ambiguous — the command may have been delivered — so it is
// never repeated, flag or no flag, even when the error text names the argument.
func TestSendCommandNeverRetriesAnAmbiguousFailure(t *testing.T) {
	m := &refusingMCP{
		fakeMCP:      &fakeMCP{connected: true, toolList: sendCatalog(true)},
		transportErr: errors.New("connection reset while sending handback"),
	}
	raw, _ := json.Marshal(sendCommandArgs{TerminalID: "terminal-a", Command: "ls"})
	res := newTerminalSendCommandTool(Deps{MCP: m, AgentHandback: true, IsAgentTerminal: agentOnly("terminal-a")}).
		Handle(context.Background(), raw, nil)
	if res.Ok {
		t.Fatal("a transport failure must surface as a failure")
	}
	if n := len(m.callsTo("terminal.sendCommand")); n != 1 {
		t.Fatalf("an ambiguous failure was re-sent: %d sends", n)
	}
}

// An unrelated refusal is reported as-is, not retried.
func TestSendCommandDoesNotRetryAnUnrelatedRefusal(t *testing.T) {
	m := &fakeMCP{connected: true, toolList: sendCatalog(true), failOn: map[string]bool{"terminal-a": true}}
	raw, _ := json.Marshal(sendCommandArgs{TerminalID: "terminal-a", Command: "ls"})
	res := newTerminalSendCommandTool(Deps{MCP: m, AgentHandback: true, IsAgentTerminal: agentOnly("terminal-a")}).
		Handle(context.Background(), raw, nil)
	if res.Ok {
		t.Fatal("expected the host's refusal")
	}
	if n := len(m.callsTo("terminal.sendCommand")); n != 1 {
		t.Fatalf("an unrelated refusal was retried: %d sends", n)
	}
}

// verbatim is the opt-out for an agent's own built-in slash command. Daintree appends
// the handback instruction to the submitted text, so a flagged "/status" would reach
// the agent as a multi-line prompt and be answered as a question instead of run. The
// flag itself is the wrapper's: it never reaches Daintree.
func TestSendCommandVerbatimSendsTheTextUnchanged(t *testing.T) {
	m := &fakeMCP{connected: true, toolList: sendCatalog(true)}
	raw := []byte(`{"terminalId":"terminal-a","command":"/status","verbatim":true}`)
	deps := Deps{MCP: m, AgentHandback: true, IsAgentTerminal: agentOnly("terminal-a")}
	if res := newTerminalSendCommandTool(deps).Handle(context.Background(), raw, nil); !res.Ok {
		t.Fatalf("send failed: %+v", res.Error)
	}
	if got, want := sentJSON(t, m), `{"command":"/status","terminalId":"terminal-a"}`; got != want {
		t.Errorf("sent %s, want %s", got, want)
	}
}

// Without verbatim a slash command is an ordinary send: a custom command that starts a
// model turn still asks for the handback its completion detection relies on.
func TestSendCommandSlashCommandWithoutVerbatimKeepsTheHandback(t *testing.T) {
	m := &fakeMCP{connected: true, toolList: sendCatalog(true)}
	runSend(t, Deps{MCP: m, AgentHandback: true, IsAgentTerminal: agentOnly("terminal-a")}, "terminal-a", "/work 123")
	if got, want := sentJSON(t, m), `{"command":"/work 123","handback":true,"terminalId":"terminal-a"}`; got != want {
		t.Errorf("sent %s, want %s", got, want)
	}
}
