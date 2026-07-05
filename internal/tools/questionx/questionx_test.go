package questionx

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

// tool returns the single question tool for the tests.
func tool(t *testing.T) *tools.Tool {
	t.Helper()
	ts := Tools(Deps{})
	if len(ts) != 1 {
		t.Fatalf("Tools() returned %d tools, want 1", len(ts))
	}
	return ts[0]
}

// run drives the tool's handler directly (it does its own strict decode + validation).
func run(t *testing.T, args string, tctx *tools.ToolContext) tools.ToolResult {
	t.Helper()
	return tool(t).Handle(context.Background(), json.RawMessage(args), tctx)
}

// mustJSON builds a question args JSON from a question + options.
func mustJSON(t *testing.T, question string, options []string, defaultIndex *int) string {
	t.Helper()
	m := map[string]any{"question": question, "options": options}
	if defaultIndex != nil {
		m["defaultIndex"] = *defaultIndex
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// askCtx builds a ToolContext whose AskChoice records the request and replies with the
// given answer/error.
func askCtx(ans tools.AskChoiceAnswer, err error, captured *tools.AskChoiceRequest) *tools.ToolContext {
	return &tools.ToolContext{
		Actor: domain.ActorMain,
		AskChoice: func(_ context.Context, req tools.AskChoiceRequest) (tools.AskChoiceAnswer, error) {
			if captured != nil {
				*captured = req
			}
			return ans, err
		},
	}
}

func TestAsk_ToolShape(t *testing.T) {
	tl := tool(t)
	if tl.Name != "user.askMultipleChoice" {
		t.Fatalf("name = %q", tl.Name)
	}
	if tl.Risk != domain.RiskUI {
		t.Fatalf("risk = %q, want ui", tl.Risk)
	}
	if tl.Decode == nil || tl.Handle == nil {
		t.Fatal("Decode/Handle must be set")
	}
}

func TestAsk_SuccessReturnsStructuredAnswer(t *testing.T) {
	var got tools.AskChoiceRequest
	tctx := askCtx(tools.AskChoiceAnswer{Label: "B", Index: 1, Text: "Staging"}, nil, &got)

	res := run(t, mustJSON(t, "Which environment?", []string{"Local", "Staging", "Production"}, nil), tctx)
	if !res.Ok {
		t.Fatalf("expected Ok, got %+v", res.Error)
	}
	// The request handed to the surface must carry client-assigned A/B/C labels.
	if len(got.Options) != 3 {
		t.Fatalf("request options = %d, want 3", len(got.Options))
	}
	for i, want := range []string{"A", "B", "C"} {
		if got.Options[i].Label != want {
			t.Errorf("option %d label = %q, want %q", i, got.Options[i].Label, want)
		}
	}
	// The result payload the model sees must expose the choice fields.
	m, ok := res.Result.(map[string]any)
	if !ok {
		t.Fatalf("result is %T, want map", res.Result)
	}
	if m["choice"] != "B" || m["choiceText"] != "Staging" {
		t.Errorf("result choice/choiceText = %v/%v, want B/Staging", m["choice"], m["choiceText"])
	}
	if idx, _ := m["choiceIndex"].(int); idx != 1 {
		t.Errorf("result choiceIndex = %v, want 1", m["choiceIndex"])
	}
	if !strings.Contains(res.Summary, "Staging") {
		t.Errorf("summary %q should mention the chosen option", res.Summary)
	}
}

func TestAsk_DefaultIndexPassedThrough(t *testing.T) {
	var got tools.AskChoiceRequest
	tctx := askCtx(tools.AskChoiceAnswer{Label: "C", Index: 2, Text: "Three"}, nil, &got)
	def := 2
	run(t, mustJSON(t, "Pick", []string{"One", "Two", "Three"}, &def), tctx)
	if got.Default != 2 {
		t.Fatalf("request Default = %d, want 2", got.Default)
	}
}

func TestAsk_NilAskChoiceIsNotInteractive(t *testing.T) {
	// A non-interactive actor gets a nil AskChoice (buildContext leaves it unset).
	res := run(t, mustJSON(t, "Which?", []string{"A thing", "B thing"}, nil), &tools.ToolContext{Actor: domain.ActorWatcher})
	if res.Ok || res.Error == nil || res.Error.Code != codeNotInteractive {
		t.Fatalf("expected %s, got %+v", codeNotInteractive, res)
	}
	if res.Error.Recoverable {
		t.Error("QUESTION_NOT_INTERACTIVE should be unrecoverable")
	}
}

func TestAsk_CancelledMapsToQuestionCancelled(t *testing.T) {
	tctx := askCtx(tools.AskChoiceAnswer{}, context.Canceled, nil)
	res := run(t, mustJSON(t, "Which?", []string{"Yes", "No"}, nil), tctx)
	if res.Ok || res.Error == nil || res.Error.Code != codeCancelled {
		t.Fatalf("expected %s, got %+v", codeCancelled, res)
	}
}

func TestAsk_HookMissingMapsToUnavailable(t *testing.T) {
	tctx := askCtx(tools.AskChoiceAnswer{}, tools.ErrNoAskChoiceHook, nil)
	res := run(t, mustJSON(t, "Which?", []string{"Yes", "No"}, nil), tctx)
	if res.Ok || res.Error == nil || res.Error.Code != codeUnavailable {
		t.Fatalf("expected %s, got %+v", codeUnavailable, res)
	}
}

func TestAsk_InvalidArgs(t *testing.T) {
	tctx := askCtx(tools.AskChoiceAnswer{Label: "A", Index: 0, Text: "x"}, nil, nil)
	def := 5
	cases := map[string]string{
		"too few options":      mustJSON(t, "q", []string{"only one"}, nil),
		"empty question":       mustJSON(t, "   ", []string{"a", "b"}, nil),
		"empty option":         mustJSON(t, "q", []string{"a", ""}, nil),
		"duplicate option":     mustJSON(t, "q", []string{"Same", "same"}, nil),
		"default out of range": mustJSON(t, "q", []string{"a", "b"}, &def),
		"unknown field":        `{"question":"q","options":["a","b"],"bogus":1}`,
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			res := run(t, args, tctx)
			if res.Ok || res.Error == nil || res.Error.Code != codeInvalidArgs {
				t.Fatalf("%s: expected %s, got %+v", name, codeInvalidArgs, res)
			}
		})
	}
}

func TestAsk_TooManyOptionsRejected(t *testing.T) {
	tctx := askCtx(tools.AskChoiceAnswer{Label: "A", Index: 0, Text: "x"}, nil, nil)
	opts := make([]string, 27)
	for i := range opts {
		opts[i] = string(rune('a'+i)) + "-option"
	}
	res := run(t, mustJSON(t, "q", opts, nil), tctx)
	if res.Ok || res.Error == nil || res.Error.Code != codeInvalidArgs {
		t.Fatalf("27 options should be rejected, got %+v", res)
	}
}

func TestAsk_SanitizesControlChars(t *testing.T) {
	var got tools.AskChoiceRequest
	tctx := askCtx(tools.AskChoiceAnswer{Label: "A", Index: 0, Text: "clean"}, nil, &got)
	// An option carrying a REAL ANSI escape + a newline must be flattened to one safe line.
	res := run(t, mustJSON(t, "q", []string{"\x1b[31mred\x1b[0m line\ntwo", "plain"}, nil), tctx)
	if !res.Ok {
		t.Fatalf("expected Ok, got %+v", res.Error)
	}
	text := got.Options[0].Text
	if strings.ContainsRune(text, '\x1b') || strings.ContainsRune(text, '\n') {
		t.Fatalf("option text not sanitized: %q", text)
	}
	if !strings.Contains(text, "red") || !strings.Contains(text, "two") {
		t.Errorf("sanitize dropped the visible text: %q", text)
	}
}

func TestAsk_RejectsDuplicateAfterSanitize(t *testing.T) {
	tctx := askCtx(tools.AskChoiceAnswer{Label: "A", Index: 0, Text: "x"}, nil, nil)
	// "Yes" and an ANSI-wrapped "Yes" render identically — reject as duplicate options.
	res := run(t, mustJSON(t, "q", []string{"Yes", "\x1b[31mYes\x1b[0m"}, nil), tctx)
	if res.Ok || res.Error == nil || res.Error.Code != codeInvalidArgs {
		t.Fatalf("ANSI-disguised duplicate should be rejected, got %+v", res)
	}
}

func TestAsk_RejectsBlankAfterSanitize(t *testing.T) {
	tctx := askCtx(tools.AskChoiceAnswer{Label: "A", Index: 0, Text: "x"}, nil, nil)
	// A pure-formatting option ("\x1b[0m") sanitizes to blank — reject it.
	res := run(t, mustJSON(t, "q", []string{"\x1b[0m", "plain"}, nil), tctx)
	if res.Ok || res.Error == nil || res.Error.Code != codeInvalidArgs {
		t.Fatalf("blank-after-sanitize option should be rejected, got %+v", res)
	}
}

func TestAsk_LabelForAlphabet(t *testing.T) {
	cases := map[int]string{0: "A", 1: "B", 25: "Z"}
	for idx, want := range cases {
		if got := labelFor(idx); got != want {
			t.Errorf("labelFor(%d) = %q, want %q", idx, got, want)
		}
	}
}
