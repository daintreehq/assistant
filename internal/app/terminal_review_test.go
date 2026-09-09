package app

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/daintreehq/assistant/internal/backend"
)

type reviewTaskRunner struct {
	request backend.TaskRequest
	result  backend.TaskResult
}

func (r *reviewTaskRunner) RunTask(_ context.Context, req backend.TaskRequest) (backend.TaskResult, error) {
	r.request = req
	return r.result, nil
}

func TestExtractionAdapterForwardsOutputBudget(t *testing.T) {
	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			r := &reviewTaskRunner{result: backend.TaskResult{Output: json.RawMessage(`{"text":"owner commits","result":{"owner":true}}`)}}
			a := extractionRouterAdapter{tasks: r}
			var err error
			if format == "text" {
				_, _, err = a.ExtractText(context.Background(), "ownership", []string{"t1", "t2"}, "tail", 2000)
			} else {
				_, err = a.ExtractJSON(context.Background(), "ownership", []string{"t1", "t2"}, "tail", map[string]any{"type": "object"}, 2000)
			}
			if err != nil {
				t.Fatal(err)
			}
			if r.request.Input["max_output_tokens"] != float64(2000) {
				t.Fatalf("requested budget lost at task boundary: %#v", r.request.Input)
			}
		})
	}
}

func TestTerminalAdaptersPreserveTaskTruncation(t *testing.T) {
	for _, finish := range []string{"stop", "length"} {
		t.Run(finish, func(t *testing.T) {
			r := &reviewTaskRunner{result: backend.TaskResult{FinishReason: finish, Output: json.RawMessage(`{"text":"What I'll ship is"}`)}}
			_, truncated, err := (contextRouterAdapter{tasks: r}).Summarize(context.Background(), "draft", "tail")
			if err != nil || truncated != (finish == "length") {
				t.Fatalf("summary: truncated=%v err=%v", truncated, err)
			}
			_, truncated, err = (extractionRouterAdapter{tasks: r}).ExtractText(context.Background(), "draft", []string{"t1"}, "tail", 1024)
			if err != nil || truncated != (finish == "length") {
				t.Fatalf("extract: truncated=%v err=%v", truncated, err)
			}
		})
	}
}

func TestJSONAdapterRejectsParseableButTruncatedResult(t *testing.T) {
	r := &reviewTaskRunner{result: backend.TaskResult{FinishReason: "length", Output: json.RawMessage(`{"result":[]}`)}}
	_, err := (extractionRouterAdapter{tasks: r}).ExtractJSON(context.Background(), "all drafts", []string{"t1"}, "tail", nil, 2000)
	if err == nil {
		t.Fatal("truncated JSON was accepted as a complete review")
	}
}
