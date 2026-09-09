package mcpx

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/safety"
)

func TestCapabilityDiscoveryUsesReviewedReadPolicyAndPreservesSkillTokens(t *testing.T) {
	for _, action := range []string{"slashCommands.list", "agentCapabilities.search", "agentCapabilities.get"} {
		t.Run(action, func(t *testing.T) {
			payload := map[string]any{"invocation": map[string]any{"token": "$github:fix-ci", "channel": "prompt-reference"}, "sourceRevision": "fixture-revision"}
			m, deps := invokeDeps()
			m.toolList = append(m.toolList, MCPToolInfo{Name: action, Description: "Find skills and commands", InputSchemaProvided: true, InputSchema: map[string]any{"type": "object", "properties": map[string]any{"agentId": map[string]any{"type": "string"}, "worktreePath": map[string]any{"type": "string"}}, "additionalProperties": false}})
			m.result = MCPCallResult{StructuredContent: payload}
			raw, _ := json.Marshal(map[string]any{"action": action, "arguments": map[string]any{"agentId": "codex", "worktreePath": "/repo/issue-42"}})
			target, refusal, result := runInvoke(t, deps, string(raw))
			if refusal != nil || !result.Ok {
				t.Fatalf("lookup failed: refusal=%+v result=%+v", refusal, result)
			}
			if target.Risk != domain.RiskRead || safety.AlwaysConfirm(target.Risk) {
				t.Fatalf("lookup must be a read without confirmation: %+v", target)
			}
			if m.lastName != action || m.lastArgs["agentId"] != "codex" || m.lastArgs["worktreePath"] != "/repo/issue-42" {
				t.Fatalf("lookup changed target: %s %+v", m.lastName, m.lastArgs)
			}
			encoded, _ := json.Marshal(result.Result)
			var envelope map[string]any
			_ = json.Unmarshal(encoded, &envelope)
			structured, ok := envelope["structuredContent"].(map[string]any)
			if !ok {
				t.Fatalf("missing structured details: %s", encoded)
			}
			invocation := structured["invocation"].(map[string]any)
			if invocation["token"] != "$github:fix-ci" {
				t.Fatalf("skill token changed: %+v", invocation)
			}
		})
	}
}

func TestCapabilityPolicyDoesNotInventHostSupport(t *testing.T) {
	_, deps := invokeDeps()
	_, refusal, _ := runInvoke(t, deps, `{"action":"agentCapabilities.search","arguments":{"query":"skill"}}`)
	requireRefusal(t, refusal, codeActionUnavailable)
}

// Captured from Daintree's shared schemas; the host pins the same fixture in
// docs/contracts/agent-capabilities.json and checks it against its live actions.
func TestCapabilityInvocationAgainstPublishedDaintreeSchemas(t *testing.T) {
	raw, err := os.ReadFile("testdata/agent_capabilities.json")
	if err != nil {
		t.Fatal(err)
	}
	var contract []struct {
		Name        string         `json:"name"`
		InputSchema map[string]any `json:"inputSchema"`
	}
	if err := json.Unmarshal(raw, &contract); err != nil {
		t.Fatal(err)
	}
	for _, entry := range contract {
		t.Run(entry.Name, func(t *testing.T) {
			m, deps := invokeDeps()
			m.toolList = append(m.toolList, MCPToolInfo{Name: entry.Name, InputSchemaProvided: true, InputSchema: entry.InputSchema})
			args := map[string]any{"agentId": "codex", "worktreePath": "/repo/issue-42"}
			if entry.Name == "agentCapabilities.search" {
				args["query"] = "$work-issue"
				args["kinds"] = []string{"skill"}
				args["limit"] = 5
			} else {
				args["id"] = "cap_fixture"
				args["catalogRevision"] = "revision_fixture"
			}
			call, _ := json.Marshal(map[string]any{"action": entry.Name, "arguments": args})
			_, refusal, result := runInvoke(t, deps, string(call))
			if refusal != nil || !result.Ok {
				t.Fatalf("published schema rejected actual lookup: %+v %+v", refusal, result)
			}
			args["inventedFlag"] = "do not forward"
			call, _ = json.Marshal(map[string]any{"action": entry.Name, "arguments": args})
			_, refusal, _ = runInvoke(t, deps, string(call))
			requireRefusal(t, refusal, codeArgsSchemaInvalid)
		})
	}
}
