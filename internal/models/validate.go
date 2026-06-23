package models

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sync"

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

// logElider collapses repeated message contents in the model.request debug trace.
// The base system prompt (~22 KB) and the entire prior conversation are byte-for-byte
// identical on every model.request, so re-serializing the whole message array each
// call makes the session log grow O(turns²) — a real session hit 3.2 MB with the 22 KB
// prompt re-dumped 41×. The elider logs each distinct (role,content) ONCE in full,
// then replaces later repeats with a compact hash reference, restoring ~O(turns)
// growth while keeping full fidelity: every byte is in the log exactly once, findable
// by its hash. Session-scoped (one per Router = one process = one session log) and
// concurrency-safe (small- and large-tier calls can race). A nil *logElider is a
// pass-through, so a Router built without one never panics.
type logElider struct {
	mu   sync.Mutex
	seen map[string]struct{}
}

func newLogElider() *logElider { return &logElider{seen: map[string]struct{}{}} }

// elideMinBytes: contents at or below this size pass through inline — a hash
// reference would be longer than the content and only hurt readability for short
// user/tool messages. The wins are the system prompt and large tool results.
const elideMinBytes = 200

// elide returns a copy of the redactMessages entries with any (role,content) already
// logged this session replaced by a compact sha256 reference. The first occurrence of
// a content is logged in full, tagged with the SAME "contentSha" so a later "repeated"
// reference is greppable straight back to the full body. Wording is order-neutral:
// concurrent tiers mean the full copy may be written just after a reference, not
// strictly before it, so the hash — not "earlier" — is what links them. Entries are
// never mutated in place: annotated/elided entries are fresh maps; only small inline
// entries are passed through by reference.
func (e *logElider) elide(entries []map[string]any) []map[string]any {
	if e == nil {
		return entries
	}
	out := make([]map[string]any, len(entries))
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, entry := range entries {
		role, _ := entry["role"].(string)
		blob, err := json.Marshal(entry["content"])
		// Small or unmarshalable content stays inline and untouched.
		if err != nil || len(blob) <= elideMinBytes {
			out[i] = entry
			continue
		}
		sum := sha256.Sum256(append([]byte(role+"\x00"), blob...))
		ref := "sha256:" + hex.EncodeToString(sum[:])[:16]
		if _, ok := e.seen[ref]; ok {
			out[i] = map[string]any{
				"role":     role,
				"repeated": fmt.Sprintf("%s (%d bytes, body logged once this session)", ref, len(blob)),
			}
			continue
		}
		e.seen[ref] = struct{}{}
		// First occurrence: copy the entry into a fresh map (never mutate the caller's)
		// and tag it with the hash so a later "repeated" reference resolves to it.
		full := make(map[string]any, len(entry)+1)
		for k, v := range entry {
			full[k] = v
		}
		full["contentSha"] = ref
		out[i] = full
	}
	return out
}

// DecodeWatcherVerdict parses+validates a small-model watcher classification from
// the cleaned JSON the json path returns. Strict validation:
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
