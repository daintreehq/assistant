package backend

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

var (
	benchmarkBytes  []byte
	benchmarkResult RespondResult
)

func benchmarkRespondRequest(messages, tools int) RespondRequest {
	input := RespondInput{ToolChoice: "auto"}
	content := json.RawMessage(`"A representative conversation message with enough text to exercise escaping and copying."`)
	for i := 0; i < messages; i++ {
		input.Messages = append(input.Messages, Message{Role: "user", Content: content})
	}
	params := json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"Absolute path to inspect"},"offset":{"type":"integer"},"limit":{"type":"integer"}},"required":["path"],"additionalProperties":false}`)
	for i := 0; i < tools; i++ {
		input.Tools = append(input.Tools, Tool{Type: "function", Function: FunctionDef{
			Name:        fmt.Sprintf("tool__operation_%03d", i),
			Description: strings.Repeat("Representative tool description. ", 6),
			Parameters:  params,
		}})
	}
	return RespondRequest{
		ProtocolVersion: 1,
		Session:         RespondSession{ID: "ses_benchmark", TurnID: "turn_benchmark", Round: 4},
		Input:           input,
		Runtime:         &RuntimeContext{PermissionTier: "operator", SchedulerActive: true},
		Selection:       &Selection{Policy: "new_instruction"},
		Generation:      &Generation{ResponseFormat: "text", Stream: true},
	}
}

func BenchmarkMarshalRespondRequest500Messages85Tools(b *testing.B) {
	req := benchmarkRespondRequest(500, 85)
	warm, err := json.Marshal(req)
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(warm)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchmarkBytes, err = json.Marshal(req)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func benchmarkContentStream(events int) string {
	var out strings.Builder
	out.Grow(events * 80)
	out.WriteString("event: meta\ndata: {}\n\n")
	for i := 0; i < events; i++ {
		out.WriteString("event: delta\ndata: {\"content\":\"abcdefghijklmnop\"}\n\n")
	}
	out.WriteString("event: done\ndata: {\"finish_reason\":\"stop\",\"usage\":{}}\n\n")
	return out.String()
}

func BenchmarkParseRespondStream1000ContentEvents(b *testing.B) {
	stream := benchmarkContentStream(1000)
	b.SetBytes(int64(len(stream)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		result, err := parseRespondStream(strings.NewReader(stream), StreamCallbacks{})
		if err != nil || len(result.Message.Content) != 16_000 {
			b.Fatalf("parseRespondStream: content=%d err=%v", len(result.Message.Content), err)
		}
		benchmarkResult = result
	}
}
