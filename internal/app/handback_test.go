package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/config"
	"github.com/daintreehq/assistant/internal/mcp"
	"github.com/daintreehq/assistant/internal/tools/asyncx"
)

// scriptedSendMCP records every terminal.sendCommand and answers from a per-call
// script, so a test can refuse the flagged send and accept the fallback.
type scriptedSendMCP struct {
	sends   []map[string]any
	results []mcp.CallResult
	errs    []error
}

func (f *scriptedSendMCP) IsConnected() bool { return true }

func (f *scriptedSendMCP) CallTool(_ context.Context, _ string, args map[string]any, _ mcp.CallOptions) (mcp.CallResult, error) {
	i := len(f.sends)
	cp := make(map[string]any, len(args))
	for k, v := range args {
		cp[k] = v
	}
	f.sends = append(f.sends, cp)
	var res mcp.CallResult
	var err error
	if i < len(f.results) {
		res = f.results[i]
	}
	if i < len(f.errs) {
		err = f.errs[i]
	}
	return res, err
}

func sendJSON(t *testing.T, args map[string]any) string {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const legacyAsyncSend = `{"command":"continue","terminalId":"terminal-a"}`

func always(v bool) func(context.Context, string) bool {
	return func(context.Context, string) bool { return v }
}

// terminal.run.async is the other road a follow-up prompt takes to an agent, so its
// sender carries the flag under the same decision — and sends the legacy call without it.
func TestAsyncSenderHandbackFollowsTheDecision(t *testing.T) {
	flagged := &scriptedSendMCP{}
	if _, err := (asyncCommandSenderAdapter{c: flagged, requestHandback: always(true)}).
		SendCommandWithReceipt(context.Background(), "terminal-a", "continue"); err != nil {
		t.Fatal(err)
	}
	if got, want := sendJSON(t, flagged.sends[0]), `{"command":"continue","handback":true,"terminalId":"terminal-a"}`; got != want {
		t.Errorf("sent %s, want %s", got, want)
	}

	for name, decide := range map[string]func(context.Context, string) bool{"declined": always(false), "unwired": nil} {
		m := &scriptedSendMCP{}
		if err := (asyncCommandSenderAdapter{c: m, requestHandback: decide}).SendCommand(context.Background(), "terminal-a", "continue"); err != nil {
			t.Fatal(err)
		}
		if len(m.sends) != 1 || sendJSON(t, m.sends[0]) != legacyAsyncSend {
			t.Errorf("%s: sends = %+v, want exactly the legacy call", name, m.sends)
		}
	}
}

func TestAsyncSenderFallsBackOnlyOnAHandbackRefusal(t *testing.T) {
	refusal := mcp.CallResult{IsError: true, Text: "handback needs an agent pane, and terminal 'terminal-a' has no agent running. Send without handback."}

	// Refused flag ⇒ nothing was sent ⇒ one legacy resend, and its receipt is returned.
	// The fallback answers with a TRACKED receipt, so a synthesized legacy one — the
	// refused call's, or none — cannot pass for it.
	tracked := mcp.CallResult{StructuredContent: map[string]any{"sent": true, "terminalId": "terminal-a", "submissionToken": "tok-fallback"}}
	m := &scriptedSendMCP{results: []mcp.CallResult{refusal, tracked}}
	receipt, err := asyncCommandSenderAdapter{c: m, requestHandback: always(true)}.
		SendCommandWithReceipt(context.Background(), "terminal-a", "continue")
	if err != nil {
		t.Fatalf("fallback send should succeed: %v", err)
	}
	if receipt.Token != "tok-fallback" || receipt.Acceptance != "tracked" {
		t.Errorf("receipt = %+v, want the fallback's tracked receipt", receipt)
	}
	if len(m.sends) != 2 || sendJSON(t, m.sends[1]) != legacyAsyncSend {
		t.Fatalf("expected flagged send then the legacy call, got %+v", m.sends)
	}

	// The fallback's OWN failure is what gets reported, and it is the last attempt even
	// if it is refused in the same words.
	twice := &scriptedSendMCP{results: []mcp.CallResult{refusal, refusal, {}}}
	_, err = asyncCommandSenderAdapter{c: twice, requestHandback: always(true)}.
		SendCommandWithReceipt(context.Background(), "terminal-a", "continue")
	var rejectedTwice asyncx.SendRejectedError
	if !errors.As(err, &rejectedTwice) || len(twice.sends) != 2 {
		t.Fatalf("refused twice: err=%v sends=%d, want SendRejectedError after exactly 2 sends", err, len(twice.sends))
	}
	lost := &scriptedSendMCP{results: []mcp.CallResult{refusal}, errs: []error{nil, errors.New("connection reset")}}
	_, err = asyncCommandSenderAdapter{c: lost, requestHandback: always(true)}.
		SendCommandWithReceipt(context.Background(), "terminal-a", "continue")
	if err == nil || errors.As(err, &rejectedTwice) || len(lost.sends) != 2 {
		t.Fatalf("fallback transport failure: err=%v sends=%d, want the ambiguous transport error", err, len(lost.sends))
	}

	// Any other rejection is reported as a rejection, once.
	other := &scriptedSendMCP{results: []mcp.CallResult{{IsError: true, Text: "Terminal not found"}}}
	_, err = asyncCommandSenderAdapter{c: other, requestHandback: always(true)}.
		SendCommandWithReceipt(context.Background(), "terminal-a", "continue")
	var rejected asyncx.SendRejectedError
	if !errors.As(err, &rejected) || len(other.sends) != 1 {
		t.Fatalf("unrelated rejection: err=%v sends=%d, want SendRejectedError and 1 send", err, len(other.sends))
	}

	// A transport error is ambiguous and must never be re-sent, whatever it says.
	ambiguous := &scriptedSendMCP{errs: []error{errors.New("reset while sending handback")}}
	_, err = asyncCommandSenderAdapter{c: ambiguous, requestHandback: always(true)}.
		SendCommandWithReceipt(context.Background(), "terminal-a", "continue")
	if err == nil || len(ambiguous.sends) != 1 {
		t.Fatalf("ambiguous failure: err=%v sends=%d, want an error and 1 send", err, len(ambiguous.sends))
	}
}

// The decision itself fails closed at every seam the App owns: switch off, no
// Session yet (the tool table is built before one exists), no MCP client.
func TestAppSendRequestsHandbackFailsClosed(t *testing.T) {
	var nilApp *App
	if nilApp.sendRequestsHandback(context.Background(), "terminal-a") || nilApp.isLiveAgentTerminal("terminal-a") {
		t.Error("nil app must be false")
	}
	// A non-nil client in both, so the gate under test — not the nil-client guard —
	// is what answers. Neither case may reach it: a zero Client is never dereferenced.
	off := &App{Config: config.AppConfig{AgentHandback: false}, MCP: &mcp.Client{}}
	if off.sendRequestsHandback(context.Background(), "terminal-a") {
		t.Error("switch off must be false")
	}
	noSession := &App{Config: config.AppConfig{AgentHandback: true}, MCP: &mcp.Client{}}
	if noSession.sendRequestsHandback(context.Background(), "terminal-a") {
		t.Error("no session (unknown terminal) must be false")
	}
}

// The #311 lesson, for the spawn's copy of the projection: dropping InputSchemaProvided
// here would make the client's accept-anything stand-in read as an advertised schema.
func TestAgentTaskToolInfosKeepTheAdvertisedBit(t *testing.T) {
	schema := map[string]any{"properties": map[string]any{"handback": map[string]any{"type": "boolean"}}}
	got := toAgentTaskToolInfos([]mcp.ToolInfo{
		{Name: "agent.launch", InputSchema: schema, InputSchemaProvided: true},
		{Name: "old.tool", InputSchema: schema, InputSchemaProvided: false},
	})
	if len(got) != 2 || got[0].Name != "agent.launch" || !got[0].InputSchemaProvided || got[1].InputSchemaProvided {
		t.Fatalf("projection lost a field: %+v", got)
	}
	if _, ok := got[0].InputSchema["properties"].(map[string]any)["handback"]; !ok {
		t.Fatal("projection lost the input schema")
	}
}

// The flag is this process's own business. Nothing the MODEL is shown may mention it:
// a described argument is one the model starts supplying, and a described marker is a
// second instruction to the agent with no code in it. Asserted on the rendered bytes —
// the whole projection, schemas and full descriptions — with the switch on (default).
func TestModelFacingToolsNeverMentionHandback(t *testing.T) {
	data := render(t, buildInventory(t, ToolInventoryOptions{}))
	for _, name := range []string{"sendCommand", "spawnForEdits", "run"} {
		if !strings.Contains(string(data), name) {
			t.Fatalf("inventory does not contain a %q tool — this guard is not reaching the real registry", name)
		}
	}
	// The one sanctioned mention is the RESULT field terminal.awaitAll documents
	// (`agentHandback`, the agent's own summary as quoted data): a field the model
	// receives has to be describable, and it is neither an argument the model could
	// start supplying nor the marker. Blank exactly that token — same length, so the
	// excerpt offsets below still line up — and keep the guard strict for the rest.
	const resultField = "agentHandback"
	if !strings.Contains(string(data), resultField) {
		t.Fatalf("inventory no longer documents %q — drop this exemption rather than keep a dead one", resultField)
	}
	scrubbed := strings.ReplaceAll(string(data), resultField, strings.Repeat("_", len(resultField)))
	if i := strings.Index(strings.ToLower(scrubbed), "handback"); i >= 0 {
		lo, hi := max(0, i-80), min(len(data), i+80)
		t.Fatalf("the model-facing tool projection mentions handback: …%s…", data[lo:hi])
	}
}
