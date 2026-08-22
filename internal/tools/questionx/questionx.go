// Package questionx holds user.askMultipleChoice — the one tool that lets the model
// ask the human a single, finite, multiple-choice question mid-turn and BLOCK on the
// answer. It is the only structured way the assistant collects a decision from the
// user: the model supplies a question + 2–26 plain-text options; the CLI assigns
// A/B/C… labels, renders a selection sheet in place of the composer, waits for the
// pick, and returns the chosen option as a normal tool result so the model continues
// the SAME turn with the answer in hand. The tool never mutates anything — it drives a
// UI interaction (RiskUI) and reaches the user purely through ToolContext.AskChoice, so
// the family needs no external providers.
package questionx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/tools"
)

// Model-facing recovery codes (exact strings).
const (
	codeInvalidArgs    = "INVALID_ARGS"
	codeNotInteractive = "QUESTION_NOT_INTERACTIVE"
	codeUnavailable    = "QUESTION_UNAVAILABLE"
	codeCancelled      = "QUESTION_CANCELLED"
)

const (
	minOptions       = 2
	maxOptions       = 26 // A..Z — the label alphabet
	maxQuestionRunes = 500
	maxOptionRunes   = 240
)

// Deps is the (empty) dependency set — the tool talks to the user only through the
// ToolContext, so there is nothing to inject. Kept for wiring symmetry with the other
// families and so a future dependency doesn't change the call site.
type Deps struct{}

// Tools returns the question tool family (a single tool).
func Tools(_ Deps) []*tools.Tool {
	return []*tools.Tool{newAskMultipleChoiceTool()}
}

// askSchema is the model-facing JSON Schema. It shows the LITERAL shape (no dotted
// prose, no baked-in labels) so the model can't get the arguments subtly wrong.
var askSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["question", "options"],
  "properties": {
    "question": {
      "type": "string",
      "description": "The concise, human-facing question. Do NOT include option letters (A/B/C) or restate the options inside it."
    },
    "options": {
      "type": "array",
      "minItems": 2,
      "maxItems": 26,
      "items": {
        "type": "string",
        "description": "One selectable option as plain text. Do NOT prefix it with a letter — the client assigns A/B/C… labels."
      }
    },
    "defaultIndex": {
      "type": "integer",
      "minimum": 0,
      "description": "Optional 0-based index of the option to highlight first. Defaults to 0."
    }
  }
}`)

const askDescription = "Ask the human ONE multiple-choice question and wait for the answer, then continue the same turn using it. " +
	"Give a concise `question` and 2–26 plain-text `options` (no A/B/C prefixes — the client assigns letters). " +
	"Use it whenever you need a finite decision before proceeding, INSTEAD of asking in prose. Call it ALONE — never in a batch with other tools — and re-plan once the answer arrives. " +
	"Interactive sessions only: a watcher, timer or non-interactive run cannot ask."

func newAskMultipleChoiceTool() *tools.Tool {
	return &tools.Tool{
		Name:        "user.askMultipleChoice",
		Description: askDescription,
		Risk:        domain.RiskUI,
		Schema:      askSchema,
		Decode:      tools.StrictDecoder(func() any { return &askArgs{} }),
		Handle:      handleAsk,
	}
}

// askArgs is the decoded argument shape. Validate enforces the bounds strict JSON
// decoding can't express (option counts, non-empty, length caps, no duplicates, default
// in range).
type askArgs struct {
	Question     string   `json:"question"`
	Options      []string `json:"options"`
	DefaultIndex *int     `json:"defaultIndex,omitempty"`
}

// Validate runs after strict decode (via tools.StrictDecoder) so the model gets a
// precise INVALID_ARGS on a malformed question rather than a rendered sheet with a bad
// shape.
func (a *askArgs) Validate() error {
	if strings.TrimSpace(a.Question) == "" {
		return errors.New("question is required")
	}
	if n := len([]rune(a.Question)); n > maxQuestionRunes {
		return fmt.Errorf("question must be at most %d characters (got %d)", maxQuestionRunes, n)
	}
	if len(a.Options) < minOptions {
		return fmt.Errorf("provide at least %d options", minOptions)
	}
	if len(a.Options) > maxOptions {
		return fmt.Errorf("provide at most %d options (A–Z)", maxOptions)
	}
	seen := make(map[string]bool, len(a.Options))
	for i, opt := range a.Options {
		t := strings.TrimSpace(opt)
		if t == "" {
			return fmt.Errorf("option %d is empty", i+1)
		}
		if n := len([]rune(t)); n > maxOptionRunes {
			return fmt.Errorf("option %d must be at most %d characters (got %d)", i+1, maxOptionRunes, n)
		}
		key := strings.ToLower(t)
		if seen[key] {
			return fmt.Errorf("option %d duplicates an earlier option", i+1)
		}
		seen[key] = true
	}
	if a.DefaultIndex != nil && (*a.DefaultIndex < 0 || *a.DefaultIndex >= len(a.Options)) {
		return fmt.Errorf("defaultIndex %d is out of range (0–%d)", *a.DefaultIndex, len(a.Options)-1)
	}
	return nil
}

func handleAsk(ctx context.Context, raw json.RawMessage, tctx *tools.ToolContext) tools.ToolResult {
	var a askArgs
	if err := tools.DecodeStrict(raw, &a); err != nil {
		return tools.Fail(codeInvalidArgs, "Invalid arguments for user.askMultipleChoice: "+err.Error())
	}
	// Defense in depth: StrictDecoder already ran Validate, but re-check so a direct
	// caller (loop bug, test) can't reach the UI with an out-of-bounds request.
	if err := a.Validate(); err != nil {
		return tools.Fail(codeInvalidArgs, "Invalid arguments for user.askMultipleChoice: "+err.Error())
	}

	// A nil AskChoice means a non-interactive actor (watcher/timer/workflow) — there is
	// no human to prompt. Unrecoverable: retrying can't make one appear; the model must
	// decide without asking.
	if tctx == nil || tctx.AskChoice == nil {
		return tools.Fail(codeNotInteractive,
			"A multiple-choice question can only be asked during an interactive user turn — a watcher, timer, or automated actor cannot prompt the user. Decide without asking.",
			tools.Unrecoverable())
	}

	// Build the labelled request. Labels (A, B, C…) are client-owned so the model never
	// spells them; the option text is sanitized to a single safe line before it renders.
	// Validate on the SANITIZED text too: stripping ANSI/control runes can turn a
	// distinct-looking option blank ("\x1b[0m") or collapse two options to the same
	// visible text ("Yes" vs "\x1b[31mYes\x1b[0m"), which raw-text validation misses.
	opts := make([]tools.ChoiceOption, len(a.Options))
	seen := make(map[string]bool, len(a.Options))
	for i, text := range a.Options {
		clean := sanitize(text)
		if clean == "" {
			return tools.Fail(codeInvalidArgs, fmt.Sprintf(
				"Invalid arguments for user.askMultipleChoice: option %d is empty once formatting is removed", i+1))
		}
		if key := strings.ToLower(clean); seen[key] {
			return tools.Fail(codeInvalidArgs, fmt.Sprintf(
				"Invalid arguments for user.askMultipleChoice: option %d duplicates another option", i+1))
		} else {
			seen[key] = true
		}
		opts[i] = tools.ChoiceOption{Label: labelFor(i), Text: clean}
	}
	def := 0
	if a.DefaultIndex != nil {
		def = *a.DefaultIndex
	}
	req := tools.AskChoiceRequest{
		ToolCallID: tctx.ToolCallID,
		Question:   sanitize(a.Question),
		Options:    opts,
		Default:    def,
	}

	// Progress cue so the live footer reads "waiting for your choice" while blocked
	// (ReportProgress is an optional field — nil in tests / non-attached session sinks).
	if tctx.ReportProgress != nil {
		tctx.ReportProgress(tools.ToolProgress{Phase: tools.ProgressAwaitingQuestion, Message: "waiting for your choice"})
	}

	ans, err := tctx.AskChoice(ctx, req)
	if err != nil {
		// A cancelled turn (Esc / Ctrl+C / shutdown) unblocks the tool with a cancel.
		if errors.Is(err, context.Canceled) || ctx.Err() != nil {
			return tools.Fail(codeCancelled, "The user cancelled before answering the question.", tools.Unrecoverable())
		}
		// Any other error means the surface couldn't ask (e.g. ErrNoAskChoiceHook in a
		// one-shot / host run that offered the tool but has no interactive sheet).
		return tools.Fail(codeUnavailable, "Could not ask the user: "+err.Error(), tools.Unrecoverable())
	}

	// Success — hand the model a clear, structured answer to branch on. The summary is
	// the human-readable line; the result carries the machine-friendly fields.
	summary := fmt.Sprintf("User chose %s — %s", ans.Label, ans.Text)
	result := map[string]any{
		"question":    req.Question,
		"choice":      ans.Label,
		"choiceIndex": ans.Index,
		"choiceText":  ans.Text,
		"options":     opts,
	}
	return tools.Ok(summary, result)
}

// labelFor maps a 0-based option index to its display label: 0→A, 1→B, … 25→Z. The
// caller has already bounded the option count to maxOptions, so this never overflows Z.
func labelFor(i int) string {
	return string(rune('A' + i))
}

// ansiRe matches a CSI escape sequence (ESC [ … final-byte) so a stray colour code in
// an option can't leak into the sheet's cell math.
var ansiRe = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]")

// sanitize collapses an option/question to a single safe display line: it strips ANSI
// escape sequences, turns newlines/tabs into spaces, drops the remaining control runes,
// and trims the edges. The bounds check in Validate ran on the RAW text, so a sanitized
// value is never longer than the cap.
func sanitize(s string) string {
	s = ansiRe.ReplaceAllString(s, "")
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t' || r == '\r':
			b.WriteByte(' ')
		case unicode.IsControl(r):
			// drop every control rune (C0, C1, and DEL) — not just the C0 range, so a
			// bare ESC left after ANSI stripping or a C1 control can't reach the sheet
		default:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}
