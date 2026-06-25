package agent

import (
	"encoding/json"
	"strings"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/models"
)

// dupRefPrefix opens the back-reference a deduped tool result is collapsed to
// ("[duplicate of <toolCallID>]"). It doubles as the idempotency marker: a body that
// already starts with it is a prior sweep's reference, so it is neither a dedup source
// nor a target on a second pass.
const dupRefPrefix = "[duplicate of "

// runPreSweep performs a cheap, lossless, deterministic cleanup of the working messages
// BEFORE the auto-compact summary rung — the issue #257 pre-sweep. Two strictly
// reductive passes run over tool-role messages only; control messages (the fixed
// system/runtime/skills prefix) and prose are never touched:
//
//  1. Dedup — byte-identical tool-result bodies collapse to a "[duplicate of <id>]"
//     back-reference at every copy EXCEPT the most recent, whose verbatim body is kept.
//     Lossless: the surviving copy still carries the full content.
//  2. Stub collapse — an overflow truncation stub (Truncated && ArtifactID set) drops
//     its now-redundant inline Preview. Lossless under durable artifacts (#249): the
//     full payload stays reachable via artifact.read on the retained ArtifactID.
//
// Both passes only ever shrink a body (a rewrite that wouldn't is skipped) and are
// idempotent (a second call returns 0). The caller re-estimates tokens afterward and
// may skip the model summary entirely when the sweep alone drops back under threshold.
// Pure and lock-free over the passed slice — no I/O, no Session state — so it is
// deterministic and unit-testable; the caller invokes it under s.mu. The slice is
// mutated in place; the count of rewritten messages is returned.
func runPreSweep(msgs []models.ChatMessage) int {
	// The first ControlMessageCount messages are the fixed control prefix — never
	// candidates. With no working message after them there is nothing to sweep.
	if len(msgs) <= domain.ControlMessageCount {
		return 0
	}
	work := msgs[domain.ControlMessageCount:]
	return dedupToolResults(work) + collapseStubPreviews(work)
}

// dedupToolResults collapses byte-identical tool-result bodies to a back-reference at
// every copy but the most recent. The kept (most-recent) copy retains its verbatim
// content; earlier identical copies are rewritten to "[duplicate of <keptToolCallID>]".
// work is the post-control working slice. Strictly reductive (a reference no shorter
// than the body is skipped, so context never grows) and idempotent (an existing
// reference is neither a source nor a target). Returns the rewrite count.
func dedupToolResults(work []models.ChatMessage) int {
	// Pass 1 (newest→oldest): map each distinct tool-result body to the ToolCallID of its
	// most-recent copy — the survivor that earlier copies will reference. Skip already-
	// collapsed references and bodies without a ToolCallID (no stable anchor to point at).
	survivor := make(map[string]string)
	for i := len(work) - 1; i >= 0; i-- {
		m := work[i]
		if m.Role != "tool" || m.ToolCallID == "" || strings.HasPrefix(m.StringContent, dupRefPrefix) {
			continue
		}
		if _, seen := survivor[m.StringContent]; !seen {
			survivor[m.StringContent] = m.ToolCallID
		}
	}

	// Pass 2 (oldest→newest): rewrite every copy that is NOT the survivor to a reference.
	modified := 0
	for i := range work {
		m := &work[i]
		if m.Role != "tool" || m.ToolCallID == "" || strings.HasPrefix(m.StringContent, dupRefPrefix) {
			continue
		}
		keptID, ok := survivor[m.StringContent]
		if !ok || keptID == m.ToolCallID {
			continue // a unique body, or this message IS the survivor
		}
		ref := dupRefPrefix + keptID + "]"
		if charLen(ref) >= charLen(m.StringContent) {
			continue // a reference at least as long as the body would only grow context
		}
		m.StringContent = ref
		modified++
	}
	return modified
}

// collapseStubPreviews strips the now-redundant inline Preview from every overflow
// truncation stub whose full payload is durably archived (Truncated && ArtifactID set).
// The retained ArtifactID still reaches the full result via artifact.read (#249), so
// dropping the up-to-TruncationPreviewChars preview is lossless. Strictly reductive
// (only rewrites when the collapsed stub is smaller) and idempotent (a stub already
// missing its Preview is left alone). Returns the rewrite count.
func collapseStubPreviews(work []models.ChatMessage) int {
	modified := 0
	for i := range work {
		m := &work[i]
		if m.Role != "tool" {
			continue
		}
		// Cheap reject before the JSON decode: an overflow stub always carries this exact
		// token (json.Marshal emits no spaces), while a dedup back-reference (plain text)
		// or a normal full-payload result never does.
		if !strings.Contains(m.StringContent, `"truncated":true`) {
			continue
		}
		var stub truncationStub
		if err := json.Unmarshal([]byte(m.StringContent), &stub); err != nil {
			continue // not parseable as a stub — leave it untouched
		}
		if !stub.Result.Truncated || stub.Result.ArtifactID == "" || stub.Result.Preview == "" {
			continue // not an archived overflow stub, or already collapsed
		}
		stub.Result.Preview = ""
		stub.Result.Note = "Full result archived; call the artifact.read tool with artifactId \"" +
			stub.Result.ArtifactID + "\" to page through it."
		out, err := json.Marshal(stub)
		if err != nil {
			continue
		}
		collapsed := string(out)
		if charLen(collapsed) >= charLen(m.StringContent) {
			continue // never grow context
		}
		m.StringContent = collapsed
		modified++
	}
	return modified
}
