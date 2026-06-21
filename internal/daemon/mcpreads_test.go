package daemon

import (
	"context"
	"testing"
)

// Ports readStatuses exit-metadata coercion + text-body fallback (watcherEngine.test.ts
// §readStatuses #22 / #108) and readOutput's failed-read vs silent distinction.

// rawMCP returns a fixed MCPResult per tool name, recording calls.
type rawMCP struct{ byName map[string]MCPResult }

func (m rawMCP) Connected() bool { return true }
func (m rawMCP) CallRead(_ context.Context, name string, _ map[string]any) (MCPResult, error) {
	if r, ok := m.byName[name]; ok {
		return r, nil
	}
	return MCPResult{IsError: true}, nil
}

func readCtx(m MCP) *CheckContext {
	return ctxFor(newFakeStore(), newFakeQueue(), m, &progModel{})
}

func TestReadStatuses_PreservesNumericExitMetadata(t *testing.T) {
	body := `{"terminals":[
		{"terminalId":"t1","agentState":"exited","exitCode":0,"spawnedAt":1700000000000,"lastTransitionAt":1700000001000},
		{"terminalId":"t2","agentState":"exited","exitCode":1}
	]}`
	batch := readStatuses(readCtx(rawMCP{byName: map[string]MCPResult{"terminal.getStatus": {Text: body}}}), []string{"t1", "t2"}, false)
	if !batch.Ok {
		t.Fatal("ok should be true")
	}
	t1 := batch.ByID["t1"]
	if t1.ExitCode == nil || *t1.ExitCode != 0 {
		t.Errorf("exitCode 0 must be preserved, got %v", t1.ExitCode)
	}
	if t1.SpawnedAt == nil || *t1.SpawnedAt != 1700000000000 {
		t.Errorf("spawnedAt must be preserved, got %v", t1.SpawnedAt)
	}
	if t1.LastTransitionAt == nil || *t1.LastTransitionAt != 1700000001000 {
		t.Errorf("lastTransitionAt must be preserved, got %v", t1.LastTransitionAt)
	}
	if t2 := batch.ByID["t2"]; t2.ExitCode == nil || *t2.ExitCode != 1 {
		t.Errorf("exitCode 1 must be preserved, got %v", t2.ExitCode)
	}
}

func TestReadStatuses_CoercesBadExitMetadataToNil(t *testing.T) {
	// null/string/NaN/Infinity/fractional → undefined; string timestamps → undefined.
	body := `{"terminals":[
		{"terminalId":"n","agentState":"exited","exitCode":null},
		{"terminalId":"s","agentState":"exited","exitCode":"1"},
		{"terminalId":"frac","agentState":"exited","exitCode":1.5},
		{"terminalId":"tsStr","agentState":"exited","spawnedAt":"2024-01-01"},
		{"terminalId":"tsStr2","agentState":"exited","lastTransitionAt":"2026-06-17T10:00:00Z"}
	]}`
	batch := readStatuses(readCtx(rawMCP{byName: map[string]MCPResult{"terminal.getStatus": {Text: body}}}),
		[]string{"n", "s", "frac", "tsStr", "tsStr2"}, false)
	for _, id := range []string{"n", "s", "frac"} {
		if batch.ByID[id].ExitCode != nil {
			t.Errorf("%s exitCode should coerce to nil, got %v", id, *batch.ByID[id].ExitCode)
		}
	}
	if batch.ByID["tsStr"].SpawnedAt != nil {
		t.Error("string spawnedAt should coerce to nil")
	}
	if batch.ByID["tsStr2"].LastTransitionAt != nil {
		t.Error("string lastTransitionAt should coerce to nil")
	}
}

func TestReadStatuses_AbsentMetadataLeftNil(t *testing.T) {
	body := `{"terminals":[{"terminalId":"t1","agentState":"working"}]}`
	batch := readStatuses(readCtx(rawMCP{byName: map[string]MCPResult{"terminal.getStatus": {Text: body}}}), []string{"t1"}, false)
	e := batch.ByID["t1"]
	if e.ExitCode != nil || e.SpawnedAt != nil || e.LastTransitionAt != nil {
		t.Errorf("absent metadata must stay nil, got %+v", e)
	}
}

func TestReadStatuses_TextBodyFallback(t *testing.T) {
	body := `{"terminals":[{"terminalId":"t1","agentState":"waiting","exitCode":0},{"terminalId":"t2","agentState":"working"}]}`
	batch := readStatuses(readCtx(rawMCP{byName: map[string]MCPResult{"terminal.getStatus": {Text: body}}}), []string{"t1", "t2"}, false)
	if !batch.Ok {
		t.Fatal("ok should be true")
	}
	if batch.ByID["t1"].AgentState != "waiting" {
		t.Errorf("t1 agentState should parse from text body, got %q", batch.ByID["t1"].AgentState)
	}
	if batch.ByID["t2"].AgentState != "working" {
		t.Errorf("t2 agentState should parse from text body, got %q", batch.ByID["t2"].AgentState)
	}
}

func TestReadStatuses_OkWithEmptyByIDWhenNoTerminals(t *testing.T) {
	batch := readStatuses(readCtx(rawMCP{byName: map[string]MCPResult{"terminal.getStatus": {Text: ""}}}), []string{"t1"}, false)
	if !batch.Ok {
		t.Error("ok reflects call success, not byId population")
	}
	if len(batch.ByID) != 0 {
		t.Errorf("byId should be empty, got %d", len(batch.ByID))
	}
}

func TestReadOutput_TextBodyAndSilentDistinction(t *testing.T) {
	t.Run("raw text body returned verbatim", func(t *testing.T) {
		res := readOutput(readCtx(rawMCP{byName: map[string]MCPResult{"terminal.getOutput": {Text: "build finished\nall green"}}}), "t1")
		if !res.Ok || res.Value != "build finished\nall green" {
			t.Errorf("raw text body must be returned, got ok=%v value=%q", res.Ok, res.Value)
		}
	})
	t.Run("silent terminal is ok with empty value", func(t *testing.T) {
		res := readOutput(readCtx(rawMCP{byName: map[string]MCPResult{"terminal.getOutput": {Text: ""}}}), "t1")
		if !res.Ok || res.Value != "" {
			t.Errorf("an empty text body is real silence (ok, ''), got ok=%v value=%q", res.Ok, res.Value)
		}
	})
	t.Run("errored read is not ok", func(t *testing.T) {
		res := readOutput(readCtx(rawMCP{byName: map[string]MCPResult{"terminal.getOutput": {IsError: true}}}), "t1")
		if res.Ok {
			t.Error("an errored read must be ok=false (distinct from silence)")
		}
	})
}
