package models

import (
	"encoding/json"
	"fmt"
	"math"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// redactMessages masks base64 image-data URIs in messages before they go to the
// debug log. A captured screenshot is hundreds of KB of base64 — logging it
// verbatim would bloat the trace and leak the raw bytes. Text/string content
// passes through. The size-marker math is the load-bearing detail.
func redactMessages(messages []ChatMessage) []map[string]any {
	out := make([]map[string]any, 0, len(messages))
	for _, m := range messages {
		entry := map[string]any{"role": m.Role}
		if m.Parts == nil {
			entry["content"] = m.StringContent
		} else {
			parts := make([]map[string]any, 0, len(m.Parts))
			for _, p := range m.Parts {
				if p.Type != "image_url" {
					parts = append(parts, map[string]any{"type": "text", "text": p.Text})
					continue
				}
				marker := "<redacted image url>"
				if len(p.ImageURL) >= 5 && p.ImageURL[:5] == "data:" {
					kb := int(math.Ceil(float64(len(p.ImageURL)) * 3 / 4 / 1024))
					marker = fmt.Sprintf("<redacted base64 ~%dkb>", kb)
				}
				parts = append(parts, map[string]any{
					"type": "image_url", "image_url": map[string]any{"url": marker},
				})
			}
			entry["content"] = parts
		}
		out = append(out, entry)
	}
	return out
}

// DecodeWatcherVerdict parses+validates a small-model watcher classification from
// the cleaned JSON the json path returns. Mirrors the Zod WatcherVerdict.strict():
//   - confidence clamped to [0,1] is REJECTED out of range (range check), evidence
//     defaults to [], recommendedAction defaults to "none".
//
// Returns an error when the JSON is malformed or a required field is invalid.
func DecodeWatcherVerdict(jsonStr string) (domain.WatcherVerdict, error) {
	var raw struct {
		Classification    string   `json:"classification"`
		Confidence        *float64 `json:"confidence"`
		Summary           *string  `json:"summary"`
		Evidence          []string `json:"evidence"`
		RecommendedAction string   `json:"recommendedAction"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &raw); err != nil {
		return domain.WatcherVerdict{}, fmt.Errorf("watcher verdict: %w", err)
	}
	cls := domain.WatcherClassification(raw.Classification)
	if !cls.IsValid() {
		return domain.WatcherVerdict{}, fmt.Errorf("watcher verdict: invalid classification %q", raw.Classification)
	}
	if raw.Confidence == nil || *raw.Confidence < 0 || *raw.Confidence > 1 {
		return domain.WatcherVerdict{}, fmt.Errorf("watcher verdict: confidence out of [0,1]")
	}
	if raw.Summary == nil {
		return domain.WatcherVerdict{}, fmt.Errorf("watcher verdict: missing summary")
	}
	action := domain.RecommendedActionVerb(raw.RecommendedAction)
	if raw.RecommendedAction == "" {
		action = domain.ActionNone
	} else if !isValidActionVerb(action) {
		return domain.WatcherVerdict{}, fmt.Errorf("watcher verdict: invalid recommendedAction %q", raw.RecommendedAction)
	}
	evidence := raw.Evidence
	if evidence == nil {
		evidence = []string{}
	}
	return domain.WatcherVerdict{
		Classification:    cls,
		Confidence:        *raw.Confidence,
		Summary:           *raw.Summary,
		Evidence:          evidence,
		RecommendedAction: action,
	}, nil
}

// DecodeModelJudgeAnswer parses+validates a small-model yes/no judgement. Mirrors
// ModelJudgeAnswer.strict(): reason required, confidence in [0,1], matched bool.
func DecodeModelJudgeAnswer(jsonStr string) (domain.ModelJudgeAnswer, error) {
	var raw struct {
		Reason     *string  `json:"reason"`
		Confidence *float64 `json:"confidence"`
		Matched    *bool    `json:"matched"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &raw); err != nil {
		return domain.ModelJudgeAnswer{}, fmt.Errorf("judge answer: %w", err)
	}
	if raw.Reason == nil {
		return domain.ModelJudgeAnswer{}, fmt.Errorf("judge answer: missing reason")
	}
	if raw.Confidence == nil || *raw.Confidence < 0 || *raw.Confidence > 1 {
		return domain.ModelJudgeAnswer{}, fmt.Errorf("judge answer: confidence out of [0,1]")
	}
	if raw.Matched == nil {
		return domain.ModelJudgeAnswer{}, fmt.Errorf("judge answer: missing matched")
	}
	return domain.ModelJudgeAnswer{
		Reason: *raw.Reason, Confidence: *raw.Confidence, Matched: *raw.Matched,
	}, nil
}

func isValidActionVerb(a domain.RecommendedActionVerb) bool {
	switch a {
	case domain.ActionNone, domain.ActionFocusTerminal, domain.ActionAskUser,
		domain.ActionSendInput, domain.ActionSpawnHelper, domain.ActionOpenReview:
		return true
	}
	return false
}
