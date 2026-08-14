package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/models"
)

var (
	benchmarkBackendTools    []backend.Tool
	benchmarkBackendMessages []backend.Message
)

func benchmarkChatTools(count int) []models.ChatTool {
	params := json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"offset":{"type":"integer"},"limit":{"type":"integer"}},"required":["path"],"additionalProperties":false}`)
	tools := make([]models.ChatTool, count)
	for i := range tools {
		tools[i] = models.ChatTool{Type: "function", Function: models.ChatToolFunc{
			Name:        fmt.Sprintf("tool__operation_%03d", i),
			Description: strings.Repeat("Read a bounded resource and return structured data. ", 6),
			Parameters:  params,
		}}
	}
	return tools
}

func BenchmarkToBackendTools85(b *testing.B) {
	tools := benchmarkChatTools(85)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out, err := ToBackendTools(tools)
		if err != nil || len(out) != len(tools) {
			b.Fatalf("toBackendTools: tools=%d err=%v", len(out), err)
		}
		benchmarkBackendTools = out
	}
}

func benchmarkConversationMessages(count int) []models.ChatMessage {
	msgs := make([]models.ChatMessage, 0, count)
	args := `{"path":"/a/representative/path","offset":128,"limit":256}`
	for i := 0; i < count; i++ {
		switch i % 3 {
		case 0:
			msgs = append(msgs, models.TextMessage("user", "Inspect the requested file and summarize the relevant section."))
		case 1:
			msgs = append(msgs, models.ChatMessage{
				Role: "assistant", ContentNull: true,
				ToolCalls: []models.ToolCallRequest{{
					ID: "call_benchmark", Type: "function",
					Function: models.ToolCallFunction{Name: "fs__read", Arguments: args},
				}},
			})
		case 2:
			msgs = append(msgs, models.ChatMessage{Role: "tool", StringContent: strings.Repeat("result data ", 16), ToolCallID: "call_benchmark"})
		}
	}
	return msgs
}

func BenchmarkToBackendMessages300WithToolHistory(b *testing.B) {
	msgs := benchmarkConversationMessages(300)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out, err := toBackendMessages(msgs)
		if err != nil || len(out) != len(msgs) {
			b.Fatalf("toBackendMessages: messages=%d err=%v", len(out), err)
		}
		benchmarkBackendMessages = out
	}
}

func BenchmarkToBackendToolsCached85(b *testing.B) {
	tools := benchmarkChatTools(85)
	s := &Session{toolProj: toolProjCache{valid: true, unconstrained: true, tools: tools}}
	if _, err := s.toBackendToolsCached(tools); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out, err := s.toBackendToolsCached(tools)
		if err != nil || len(out) != len(tools) {
			b.Fatalf("toBackendToolsCached: tools=%d err=%v", len(out), err)
		}
		benchmarkBackendTools = out
	}
}
