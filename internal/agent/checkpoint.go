package agent

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/models"
)

// checkpoint.go is the agent-side orchestration for the structured compaction object.
// The prompt is now SERVER-OWNED: the CLI sends only the flattened transcript to the
// backend's checkpoint task, which returns the structured CheckpointOutput. This
// file still validates that no load-bearing identifier was dropped and renders the
// note body. The auto-compact path in session.go calls BuildCheckpoint and feeds the
// rendered body to compactLocked (which prepends the [checkpoint | depth N] header).
// Best-effort throughout: a sparse checkpoint that still carries every ID is strictly
// better than the old prose-of-prose, so this never blocks compaction.

// checkpointIDPattern matches the load-bearing identifiers a checkpoint must preserve:
// the Daintree MCP's real terminal IDs (terminal-<uuid>, e.g.
// terminal-5284bfef-3d11-424c-90cb-136f24046295 — the checkpoint prompt's ID-preservation
// pass names exactly this shape, and every live session log shows it, so omitting it
// here silently dropped the single most load-bearing ID class), the legacy short term_
// handles, plus the internally-minted prefixes from domain/ids.go for timers (tmr_),
// watchers (wch_), workflow runs (wfr_), agent launches (agt_), and grant tokens (grt_).
// The short-prefix suffix class is broad ([0-9a-zA-Z]{1,32}) to cover both short handles
// and our "<prefix>+8 hex" shape; over-matching at worst preserves a non-ID token in
// preserved_ids (harmless), while under-matching would silently lose a real ID
// (unrecoverable). The terminal-<uuid> alternative requires the FULL 36-char UUID so a
// model-truncated prefix (terminal-5284bfef — matches no terminal, see the terminal-id
// resolver) is never "preserved" as if it were real. run_*/wkf_* from the old prose
// prompt are intentionally omitted: there is no such domain prefix, and run_ collides
// with provenance tokens like run_test.
var checkpointIDPattern = regexp.MustCompile(`\b(?:(?:term_|tmr_|wch_|wfr_|agt_|grt_)[0-9a-zA-Z]{1,32}|terminal-[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})\b`)

// FlattenTranscript renders a working history as "role: text" lines for the backend's
// transcript-digesting tasks (checkpoint, memory_distill). Each tool call's name +
// argument JSON is folded into the text so load-bearing IDs that live ONLY in arguments
// — e.g. terminal.read {"terminalId":"terminal-…"} — survive into the checkpoint's
// ID-preservation pass; tool results are prefixed so the model can tell them from
// prose. Shared by the auto-compact path (session.go) and the manual /compact command
// (commands package) so the two flatteners can never drift again — the manual one used
// to emit a bare "[tool call]" and silently dropped every argument-only ID.
func FlattenTranscript(msgs []models.ChatMessage) string {
	var b strings.Builder
	for _, m := range msgs {
		if m.Role == "system" {
			continue
		}
		text := m.ContentToText()
		for _, tc := range m.ToolCalls {
			if text != "" {
				text += "\n"
			}
			text += "[tool call " + tc.Function.Name + " " + tc.Function.Arguments + "]"
		}
		if m.Role == "tool" {
			if text == "" {
				text = "[tool result]"
			} else {
				text = "[tool result] " + text
			}
		}
		if text == "" {
			text = "[tool call]"
		}
		b.WriteString(m.Role)
		b.WriteString(": ")
		b.WriteString(text)
		b.WriteString("\n")
	}
	return b.String()
}

// BuildCheckpoint runs the backend's checkpoint task over the soon-to-be-discarded
// transcript, then runs the ID-preservation validation pass. Exported because BOTH
// compaction entry points must go through it — the auto path (maybeAutoCompact) and
// the manual /compact command; calling backend.RunCheckpoint directly would skip
// validateCheckpoint and silently lose any ID the model dropped. Callers handle the
// backend-down case (err != nil) themselves — here a non-nil error simply yields a
// zero-value checkpoint that still gets its IDs mined from the transcript.
// transcript is the flattened, tool-call-synthesized history (FlattenTranscript).
func BuildCheckpoint(ctx context.Context, runner backend.TaskRunner, transcript string) (backend.CheckpointOutput, error) {
	cp, err := backend.RunCheckpoint(ctx, runner, backend.CheckpointInput{Transcript: transcript})
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
func validateCheckpoint(cp *backend.CheckpointOutput, transcript string) {
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

// AppendTranscriptBreadcrumb appends the compaction escape hatch to a checkpoint note
// body: the full pre-compaction transcript is preserved as a durable artifact, and this
// line tells the post-compaction model exactly how to page it back. Compaction is lossy
// by design — the breadcrumb makes the loss recoverable on demand (the pattern Claude
// Code uses with its on-disk transcript pointer) instead of permanent. A missing id
// (nothing archived / no artifact store) returns the summary unchanged.
func AppendTranscriptBreadcrumb(summary, artifactID string) string {
	if artifactID == "" {
		return summary
	}
	return summary + "\n\nFull pre-compaction transcript preserved as artifact " + artifactID +
		` — if you need exact details this checkpoint rounded off (verbatim tool output, error text, the user's earlier wording), page it back with artifact.read {"artifactId":"` + artifactID + `"}.`
}

// ArchiveCompactionTranscript stores the flattened pre-compaction transcript as a
// durable session artifact and returns its id, so the checkpoint note can carry a
// breadcrumb back to the exact history the compaction discarded (readable via
// artifact.read, which pages). Best-effort: "" when there is nothing to store or no
// artifact store is wired — the caller appends no breadcrumb and compaction proceeds
// exactly as before. Takes no session lock (the artifact store has its own).
func (s *Session) ArchiveCompactionTranscript(transcript string) string {
	if s.artifacts == nil || trimSpace(transcript) == "" {
		return ""
	}
	return s.artifacts.set(transcript)
}

// CompactWithTranscript is the manual /compact entry: it rejects an OCCUPIED session
// BEFORE archiving, then archives the transcript, appends the breadcrumb to the
// summary, and compacts. Ordering matters — archiving first and letting Compact
// reject afterwards stranded a multi-megabyte orphaned artifact (durable row + a
// hot-cache slot) on every /compact attempted while a turn was in flight. The
// pre-check leaves a tiny window in which a turn starts before Compact re-checks;
// that rare race just recreates the bounded, GC'd orphan — never corruption.
//
// "Occupied" has to mean exactly what Compact means by it, or the pre-check stops
// pre-checking. It checked only inFlight while Compact also refuses an outstanding
// endpoint reservation, so a /compact during an open `/backend` picker archived the
// transcript and was then refused — recreating the orphan on the one path written to
// prevent it.
func (s *Session) CompactWithTranscript(summary, transcript string) error {
	s.mu.Lock()
	busy := s.inFlight || s.endpointHeld != 0
	s.mu.Unlock()
	if busy {
		return ErrTurnInProgress
	}
	summary = AppendTranscriptBreadcrumb(summary, s.ArchiveCompactionTranscript(transcript))
	return s.Compact(summary)
}

// renderCheckpoint produces the note BODY — pretty-printed JSON of the checkpoint. The
// [checkpoint | depth N] header is added by compactLocked via compactionNotePrefix, so
// the body is header-free here. Indented JSON is the most model-legible form on
// rehydration and is deterministic given the struct's field order; the few extra tokens
// are negligible against the discarded transcript. An empty checkpoint marshals to "{}"
// (never ""), so the caller always has a non-empty note to compact with.
func renderCheckpoint(cp backend.CheckpointOutput) string {
	blob, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		// MarshalIndent of this plain struct can't realistically fail; guard so a render
		// fault degrades to an empty object rather than an empty (compaction-blocking) note.
		return "{}"
	}
	return string(blob)
}
