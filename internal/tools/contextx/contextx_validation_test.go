package contextx

import (
	"encoding/json"
	"testing"
)

// Finding 1: terminal.read / terminal.summarize advertise a required terminalId
// and bounded maxLines/tailBytes that strict decoding didn't enforce — a missing
// terminalId would read nothing and a negative maxLines was forwarded straight to
// the MCP read. Validate() must reject these as INVALID_ARGS.
func TestReadRejectsRequiredAndBoundsGaps(t *testing.T) {
	tool := newReadTool(Deps{})
	for name, bad := range map[string]string{
		"missing terminalId": `{"maxLines":10}`,
		"empty terminalId":   `{"terminalId":""}`,
		"blank terminalId":   `{"terminalId":"   "}`,
		"zero maxLines":      `{"terminalId":"t","maxLines":0}`,
		"negative maxLines":  `{"terminalId":"t","maxLines":-1}`,
		"huge maxLines":      `{"terminalId":"t","maxLines":1001}`,
		"zero tailBytes":     `{"terminalId":"t","tailBytes":0}`,
		"huge tailBytes":     `{"terminalId":"t","tailBytes":100001}`,
	} {
		if _, err := tool.Decode(json.RawMessage(bad)); err == nil {
			t.Errorf("read %s should be rejected: %s", name, bad)
		}
	}
	if _, err := tool.Decode(json.RawMessage(`{"terminalId":"t","maxLines":200,"tailBytes":5000}`)); err != nil {
		t.Errorf("valid read args should decode: %v", err)
	}
}

func TestSummarizeRejectsRequiredAndBoundsGaps(t *testing.T) {
	tool := newSummarizeTool(Deps{})
	for name, bad := range map[string]string{
		"missing terminalId": `{"purpose":"x"}`,
		"empty terminalId":   `{"terminalId":""}`,
		"negative tailBytes": `{"terminalId":"t","tailBytes":-1}`,
		"huge tailBytes":     `{"terminalId":"t","tailBytes":100001}`,
	} {
		if _, err := tool.Decode(json.RawMessage(bad)); err == nil {
			t.Errorf("summarize %s should be rejected: %s", name, bad)
		}
	}
	if _, err := tool.Decode(json.RawMessage(`{"terminalId":"t","tailBytes":5000}`)); err != nil {
		t.Errorf("valid summarize args should decode: %v", err)
	}
}
