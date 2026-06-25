package agent

import (
	"context"
	"encoding/json"
	"regexp"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/models"
	"github.com/daintreehq/daintree-assistant/internal/models/prompts"
)

// checkpoint.go is the agent-side orchestration for the structured compaction object
// (issue #256). The prompt text, struct, and lenient parser live in internal/models/
// prompts (mirroring distill.go); this file builds the checkpoint via the small model,
// validates that no load-bearing identifier was dropped, and renders the note body. The
// auto-compact path in session.go swaps its prose-summary Chat call for buildCheckpoint
// and feeds the rendered body to compactLocked (which prepends the [checkpoint | depth N]
// header). Best-effort throughout: a sparse checkpoint that still carries every ID is
// strictly better than the old prose-of-prose, so this never blocks compaction.

// checkpointIDPattern matches the load-bearing identifiers a checkpoint must preserve:
// terminal IDs (term_, assigned by the Daintree MCP — short alphanumeric, e.g. term_1
// or term_a1b2), plus the internally-minted prefixes from domain/ids.go for timers
// (tmr_), watchers (wch_), workflow runs (wfr_), agent launches (agt_), and grant tokens
// (grt_). The suffix class is broad ([0-9a-zA-Z]{1,32}) to cover both the MCP's short
// handles and our "<prefix>+8 hex" shape; over-matching at worst preserves a non-ID token
// in preserved_ids (harmless), while under-matching would silently lose a real ID
// (unrecoverable). run_*/wkf_* from the old prose prompt are intentionally omitted: there
// is no such domain prefix, and run_ collides with provenance tokens like run_test.
var checkpointIDPattern = regexp.MustCompile(`\b(?:term_|tmr_|wch_|wfr_|agt_|grt_)[0-9a-zA-Z]{1,32}\b`)

// buildCheckpoint asks the small model for a structured checkpoint of the soon-to-be-
// discarded history, parses it leniently, then runs the ID-preservation validation pass.
// The caller (maybeAutoCompact) handles the model-down case (err != nil) separately —
// here a non-nil error simply yields a zero-value checkpoint that still gets its IDs
// mined from the transcript. summaryMsgs is the flattened, tool-call-synthesized history
// the model reads; transcript is the same content as one string for the regex scan.
func buildCheckpoint(ctx context.Context, router Router, summaryMsgs []models.ChatMessage, transcript string) (prompts.CompactionCheckpoint, error) {
	var cp prompts.CompactionCheckpoint
	result, err := router.Chat(ctx, domain.ModelSmall, models.ChatOptions{Messages: summaryMsgs})
	if err == nil {
		cp = prompts.ParseCheckpoint(result.Content)
	}
	validateCheckpoint(&cp, transcript)
	return cp, err
}

// validateCheckpoint enforces the issue's core guarantee: every terminal/watcher/
// workflow/agent/timer/grant ID present in the discarded transcript must survive in the
// checkpoint. It scans the transcript for those IDs and, for each one the model didn't
// carry anywhere into the object, appends it to PreservedIDs. The "is it present" check
// is over the full serialized checkpoint (not just PreservedIDs) so an ID the model
// correctly placed in active_terminals/approvals_grants/etc. is not duplicated. Mutates
// cp in place; de-dupes PreservedIDs at the end.
func validateCheckpoint(cp *prompts.CompactionCheckpoint, transcript string) {
	ids := checkpointIDPattern.FindAllString(transcript, -1)
	if len(ids) == 0 {
		return
	}
	// Build the set of IDs the model already placed ANYWHERE in the object by re-scanning
	// the serialized checkpoint with the SAME regex — NOT strings.Contains. A substring
	// check would falsely judge a short ID "present" because it is a prefix of a longer one
	// (term_1 is a substring of term_10), silently dropping the exact ID this pass exists to
	// preserve. The regex's \b…\b boundaries tokenize term_1 and term_10 as distinct, so set
	// membership is collision-safe.
	present := make(map[string]struct{})
	if blob, err := json.Marshal(cp); err == nil {
		for _, id := range checkpointIDPattern.FindAllString(string(blob), -1) {
			present[id] = struct{}{}
		}
	}
	seen := make(map[string]struct{}, len(cp.PreservedIDs)+len(ids))
	for _, existing := range cp.PreservedIDs {
		seen[existing] = struct{}{}
	}
	for _, id := range ids {
		if _, ok := present[id]; ok {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		cp.PreservedIDs = append(cp.PreservedIDs, id)
	}
}

// renderCheckpoint produces the note BODY — pretty-printed JSON of the checkpoint. The
// [checkpoint | depth N] header is added by compactLocked via compactionNotePrefix, so
// the body is header-free here. Indented JSON is the most model-legible form on
// rehydration and is deterministic given the struct's field order; the few extra tokens
// are negligible against the discarded transcript. An empty checkpoint marshals to "{}"
// (never ""), so the caller always has a non-empty note to compact with.
func renderCheckpoint(cp prompts.CompactionCheckpoint) string {
	blob, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		// MarshalIndent of this plain struct can't realistically fail; guard so a render
		// fault degrades to an empty object rather than an empty (compaction-blocking) note.
		return "{}"
	}
	return string(blob)
}
