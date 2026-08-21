package ui

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/daintreehq/assistant/internal/ui/theme"
)

// render_activity.go renders the activity tree — a turn's delegated work as a
// branch tree, the brand signature. Each activity is one
// branch carrying a state glyph + a human verb + target + duration.

// presentTool maps an internal tool name to a human verb. An unknown tool falls
// back to its RAW internal name — never raw fn()/JSON syntax, and NOT title-cased.
func presentTool(name string) string {
	if l, _ := presentToolVerb(name); l != "" {
		return l
	}
	return name
}

// presentToolVerb returns the verb label for a known tool plus the args KEY whose
// value is the target/detail (or one of several, tried in order). A known tool with
// no target key returns ("Verb", nil). An unknown tool returns ("", nil).
func presentToolVerb(name string) (label string, keys []string) {
	switch name {
	case "fs.read":
		return "Read", []string{"path:rel"}
	case "fs.list":
		return "Listed", []string{"path:rel", ".:lit"}
	case "fs.search":
		return "Searched", []string{"query"}
	case "tool.search":
		return "Searched tools", []string{"query"}
	case "context.snapshot":
		return "Snapshotted", []string{"workspace context:lit"}
	case "context.summarize":
		return "Summarized", []string{"terminalId"}
	case "agentTask.spawnForEdits":
		return "Delegated", []string{"title", "goal"}
	case "watcher.terminal.create":
		return "Watching", []string{"goal", "title", "terminalIds:ids"}
	case "watcher.list":
		return "Listed watchers", nil
	case "watcher.cancel":
		return "Stopped watcher", []string{"id"}
	case "timer.schedule":
		return "Scheduled", []string{"title"}
	case "timer.list":
		return "Listed timers", nil
	case "timer.cancel":
		return "Cancelled timer", []string{"id"}
	case "terminal.focus":
		return "Focused", []string{"terminalId"}
	case "terminal.read":
		return "Read", []string{"terminalId"}
	case "terminal.extract":
		return "Extracted", []string{"terminalIds:ids"}
	case "terminal.extract.json":
		return "Extracted", []string{"terminalIds:ids"}
	case "terminal.summarize":
		return "Summarized", []string{"terminalId"}
	case "terminal.awaitAll":
		return "Waited", []string{"terminalIds:ids"}
	case "terminal.sendCommand":
		return "Ran", []string{"command"}
	case "terminal.rename":
		return "Renamed", []string{"name", "terminalId"}
	case "terminal.close":
		// "Ended" avoids stuttering against the result summary "Closed N terminal(s): …".
		return "Ended", []string{"terminalId", "terminalIds:ids"}
	case "terminal.moveToWorktree":
		// "Relocated" rather than "Moved": the result summary already opens with
		// "Moved N terminal(s) into …", and the verb column must not stutter against it.
		return "Relocated", []string{"worktreeId", "terminalId", "terminalIds:ids"}
	case "terminal.arm":
		return "Armed", []string{"terminalId"}
	case "terminal.disarm":
		return "Disarmed", []string{"terminalId"}
	case "terminal.disarmAll":
		return "Disarmed all", nil
	case "terminal.run.async":
		return "Running", []string{"command", "prompt"}
	case "terminal.await.async":
		return "Awaiting", []string{"terminalIds:ids"}
	case "async.list":
		return "Listed async", nil
	case "async.cancel":
		// "Dropped" avoids echoing the summary "Stopped monitoring async operation …".
		return "Dropped async", []string{"asyncId"}
	// The scratch store's calls all read as one grouped "Scratch" column; the
	// specific create/set/get/… verb rides the result summary in the detail slot, so
	// a category label groups the run without stuttering against that summary.
	case "scratch.create", "scratch.set", "scratch.get", "scratch.delete", "scratch.drop":
		return "Scratch", nil
	case "queue.publish":
		return "Raised", []string{"title"}
	case "queue.digest":
		return "Read inbox", nil
	case "queue.resolve":
		return "Resolved", []string{"id"}
	case "memory.recall":
		return "Recalled", []string{"query"}
	case "memory.list":
		return "Listed memories", nil
	case "memory.save":
		// "Remembered" pairs with recall's "Recalled" and avoids stuttering against the
		// summary "Saved memory mem_…"; the active-row target previews the saved content.
		return "Remembered", []string{"content", "category"}
	case "memory.forget":
		return "Forgot memory", []string{"id"}
	case "memory.pin":
		return "Pinned memory", []string{"id"}
	case "memory.unpin":
		return "Unpinned memory", []string{"id"}
	case "artifact.read":
		return "Read artifact", []string{"artifactId", "id"}
	// A delegated sub-agent run. The target is the BRIEF, not a tool name or an id,
	// because that is the only thing that tells the user what was handed off — and
	// while the run is live the row's detail is replaced by the sub-agent's own
	// progress beats ("round 3/10 · fs.search"), so this label has to read
	// naturally in front of both.
	case "subagent.run":
		return "Sub-agent", []string{"task"}
	case "copyTree.generate":
		return "Generated tree", []string{"worktreeId"}
	case "copyTree.generateAndCopyFile":
		return "Copied tree", []string{"worktreeId"}
	case "copyTree.injectToTerminal":
		return "Injected tree", []string{"terminalId"}
	case "recipe.list":
		return "Listed recipes", nil
	case "recipe.run":
		return "Ran recipe", []string{"recipeId"}
	case "skill.step.advance":
		return "Advanced step", []string{"skillId"}
	case "skill.run.get":
		return "Checked skill progress", []string{"skillId"}
	case "worktree.createWithRecipe":
		return "Created worktree", []string{"recipeId"}
	case "forge.getIssue":
		return "Read issue", []string{"issueNumber"}
	case "forge.listIssues":
		return "Listed issues", nil
	case "forge.listPRs":
		return "Listed PRs", nil
	case "forge.getPR":
		return "Read PR", []string{"prNumber", "number"}
	case "watcher.watchPR":
		return "Watching PR", []string{"prNumber", "number"}
	case "workflow.startWorkOnIssue":
		return "Started work", []string{"issueNumber", "title"}
	case "workflow.prepBranchForReview":
		return "Prepping branch", []string{"branch", "worktreeId"}
	// Workflow-graph tools (flag-gated). Labels are chosen NOT to echo each tool's
	// self-describing result summary, and the target keys use the real arg names
	// (workflowId / nodeId), not the graphId/id the schemas never had.
	case "workflow.plan":
		return "Mapped", []string{"goal"}
	case "workflow.getGraph":
		return "Inspected", []string{"id"}
	case "workflow.next":
		return "Computed next", []string{"id"}
	case "workflow.attachResource":
		return "Attached", []string{"nodeId", "workflowId"}
	case "workflow.recordEvidence":
		return "Logged evidence", []string{"nodeId", "workflowId"}
	case "workflow.reconcile":
		return "Synced", []string{"workflowId"}
	case "workflow.cancel":
		return "Stopped workflow", []string{"nodeId", "workflowId"}
	case "workflow.create":
		return "Logged workflow", []string{"issueTitle", "branch"}
	case "workflow.get":
		return "Read workflow log", []string{"id"}
	case "workflow.list":
		return "Listed workflows", nil
	case "workflow.update":
		return "Revised workflow", []string{"id"}
	case "grant.create":
		return "Granted automation", nil
	case "grant.list":
		return "Listed grants", nil
	case "grant.revoke":
		return "Revoked grant", []string{"id"}
	case "daintree.status":
		return "Checked status", nil
	case "daintree.listTools":
		return "Listed tools", nil
	case "daintree.call":
		return "Called", []string{"toolName", "name"}
	case "agentTask.status":
		return "Checked spawn", []string{"launchId", "id"}
	case "agentTask.list":
		return "Listed spawns", nil
	case "agentTask.superviseTerminal":
		return "Supervising", []string{"terminalId"}
	case "audit.export":
		// Noun-phrase label avoids stuttering against "Exported N audit row(s) as …".
		return "Audit export", nil
	case "user.askMultipleChoice":
		return "Asked", []string{"question"}
	default:
		return "", nil
	}
}

// presentToolActiveVerb returns the in-progress form of a tool's verb ("Waiting",
// not "Waited"), used while the row has NOT settled (queued, running, or
// approval-pending) — the past tense on a non-settled row reads as already
// finished. Only the tools that visibly block for many seconds need an entry;
// everything else settles fast enough that the settled label never reads wrong,
// so "" means "keep it".
func presentToolActiveVerb(name string) string {
	switch name {
	case "terminal.awaitAll":
		return "Waiting"
	case "terminal.extract", "terminal.extract.json":
		return "Extracting"
	case "terminal.summarize", "context.summarize":
		return "Summarizing"
	case "agentTask.spawnForEdits":
		return "Delegating"
	default:
		return ""
	}
}

// presentToolTarget derives the verb's target/object from the raw args JSON (the
// `detail` half of a tool presentation, truncated to 48 cells). Returns "" when the
// tool is unknown or has no resolvable target.
func presentToolTarget(name, args string) string {
	_, keys := presentToolVerb(name)
	if len(keys) == 0 {
		return ""
	}
	var obj map[string]any
	if args != "" {
		_ = json.Unmarshal([]byte(args), &obj)
	}
	for _, k := range keys {
		// A "key:mode" entry resolves specially: ":lit" is a literal string (e.g.
		// "workspace context"), ":rel" relativizes a path, ":ids" joins an array.
		key, mode, _ := strings.Cut(k, ":")
		switch mode {
		case "lit":
			return truncateCells(key, 48)
		case "rel":
			if v := strArg(obj, key); v != "" {
				return truncateCells(relativizePath(v), 48)
			}
		case "ids":
			if v := idsArg(obj, key); v != "" {
				return truncateCells(v, 48)
			}
		default:
			if v := strArg(obj, key); v != "" {
				return truncateCells(v, 48)
			}
		}
	}
	return ""
}

// strArg returns a non-empty string/number arg, or "".
func strArg(obj map[string]any, key string) string {
	v, ok := obj[key]
	if !ok {
		return ""
	}
	switch t := v.(type) {
	case string:
		if strings.TrimSpace(t) != "" {
			return t
		}
	case float64:
		// JSON numbers decode to float64; render integers without a trailing ".0".
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'g', -1, 64)
	}
	return ""
}

// idsArg joins an array arg "a, b" or falls back to a scalar.
func idsArg(obj map[string]any, key string) string {
	v, ok := obj[key]
	if !ok {
		return ""
	}
	if arr, ok := v.([]any); ok {
		parts := make([]string, 0, len(arr))
		for _, e := range arr {
			parts = append(parts, fmt.Sprint(e))
		}
		return strings.Join(parts, ", ")
	}
	return strArg(obj, key)
}

// relativizePath trims the cwd prefix from an absolute path.
func relativizePath(p string) string {
	cwd, err := os.Getwd()
	if err == nil && cwd != "" && strings.HasPrefix(p, cwd) {
		rel := strings.TrimLeft(p[len(cwd):], "/\\")
		if rel != "" {
			return rel
		}
	}
	return p
}

// activityGlyph returns the state glyph (animated spinner frame for active rows)
// and the lipgloss style for that row's tone.
func activityGlyph(th theme.Theme, a Activity, spinnerFrame int) (string, string) {
	g := th.Glyphs
	switch a.State {
	case ActQueued:
		return g.Queued, "muted"
	case ActActive:
		// Animated spinner (not a static glyph) for the active row. The live
		// ThinkingDot renders as a PLAIN <text> (terminal default fg, no
		// tone) — the motion alone reads as "working"; only the STATIC state glyphs
		// (done ✓ green, failed × red, …) carry a tone color.
		frames := g.Spinner
		if len(frames) == 0 {
			return g.Active, "plain"
		}
		return frames[spinnerFrame%len(frames)], "plain"
	case ActDone:
		// The "success" tone is the accent green — but NOT bold (the bold-accent
		// style is reserved for the ◆ DAINTREE marker).
		return g.Done, "success"
	case ActFailed:
		return g.Failed, "danger"
	case ActWaiting:
		// Plain waiting (e.g. a watcher) → the clock glyph, warning tone (yellow);
		// the ◇/blocked diamond is reserved for an explicit approval-pending state.
		return g.Waiting, "warning"
	case ActCancelled:
		// User-aborted: a muted bullet (inert, not the active spinner or the red ×
		// failure). The "cancelled" / "not run" note carries the meaning.
		return g.Bullet, "muted"
	case ActAsyncPending:
		// Accepted async work: the CALL settled, the WORK runs on — a steady yellow
		// dot (warning tone), deliberately not the green ✓ (would read "finished")
		// and not the spinner (nothing in THIS turn is still executing).
		return g.Async, "warning"
	default:
		return g.Bullet, "muted"
	}
}

// styleFor maps a tone name to a lipgloss render of s.
func styleFor(th theme.Theme, tone, s string) string {
	switch tone {
	case "accent":
		return th.Accent().Render(s)
	case "success":
		// The "success" tone = accent green, NON-bold. Distinct from the bold
		// "accent" used by the DAINTREE marker.
		return th.Body().Foreground(th.Color.Accent).Render(s)
	case "danger":
		return th.Danger().Render(s)
	case "blocked":
		return th.Blocked().Render(s)
	case "warning":
		return th.Warning().Render(s)
	case "muted":
		return th.Muted().Render(s)
	case "active", "info":
		// "active" (a working agent) and "info" both read as the informational cyan —
		// the same pairing toneGlyphFor already makes. Without the "active" arm the
		// badge system's default tone fell through to plain body text (an un-toned
		// WORKING badge in the ops deck and the footer strip).
		return th.Info().Render(s)
	default:
		return th.Body().Render(s)
	}
}

// fanOutTargetCells caps the identity half of a fan-out row. Deliberately much
// tighter than presentToolTarget's own 48: this prefix shares the detail budget
// with a live progress beat or a result summary, and the point is for the row to
// stay IDENTIFIABLE, not for the brief to be readable in full.
const fanOutTargetCells = 30

// briefLeadIns are the imperative openings a delegation brief almost always
// starts with. They are stripped for DISPLAY ONLY. Longest-first, so "find every"
// is tried before "find".
var briefLeadIns = []string{
	"in this repository,", "in this repo,", "in this codebase,",
	"find out which", "find out what", "find out where", "find out how",
	"work out which", "work out what",
	"figure out which", "figure out what",
	"find every", "find all", "find the", "find",
	"locate every", "locate all", "locate the", "locate",
	"identify every", "identify all", "identify the", "identify",
	"determine which", "determine what", "determine the", "determine",
	"list every", "list all", "list the", "list",
	"count how many", "count the", "count",
	"search for the", "search for", "search",
	"tell me which", "tell me what",
	"report which", "report what",
	"which", "what", "where", "how many",
}

// briefGist trims a leading imperative from a sub-agent brief so the DISTINCTIVE
// words lead the row.
//
// It exists because the fan-out prefix is only ~30 cells, and briefs are written
// by a model that opens nearly all of them the same way. Raw, three concurrent
// rows read "Find the GitHub issue describ…", "Find every Go file that reg…",
// "Find what schemaUserVersion…" — the shared boilerplate consumes the budget and
// the part that identifies each row is what gets cut. Stripping the lead-in turns
// them into "GitHub issue describing the…", "Go file that registers a to…",
// "schemaUserVersion is set to".
//
// Display only, and conservative: it strips at most ONE lead-in, only from the
// very start, and only when a substantial remainder survives — a brief that is
// entirely a lead-in keeps its original text rather than rendering as an empty
// row. The full brief is never lost; it is in the tool args, the transcript, and
// the expanded view.
func briefGist(brief string) string {
	trimmed := strings.TrimSpace(brief)
	lower := strings.ToLower(trimmed)
	// ToLower can change BYTE length for a few runes (Turkish dotted capital I is
	// the classic case), which would make len(lead) the wrong cut point in the
	// ORIGINAL string. The lead-ins are all ASCII, so when the lengths diverge the
	// prefix cannot be one of them in any meaningful sense — bail rather than slice.
	if len(lower) != len(trimmed) {
		return trimmed
	}
	for _, lead := range briefLeadIns {
		if !strings.HasPrefix(lower, lead) {
			continue
		}
		// The match must end on a WORD BOUNDARY. Without this, "find" matched
		// "Finding the relevant issue" and rendered it as "ing the relevant issue"
		// — a corrupted label, which is worse than the boilerplate it was trying to
		// remove. "what" did the same to "Whatever causes the failure".
		rest := trimmed[len(lead):]
		if rest != "" {
			next, _ := utf8.DecodeRuneInString(rest)
			if next != utf8.RuneError && !isBriefBoundary(next) {
				continue
			}
		}
		rest = strings.TrimSpace(rest)
		// Drop a bare article left behind ("find all of the X" leaves "of the X").
		for _, filler := range []string{"of the ", "of ", "the ", "a ", "an "} {
			if strings.HasPrefix(strings.ToLower(rest), filler) {
				rest = strings.TrimSpace(rest[len(filler):])
				break
			}
		}
		// Only accept the trim if what remains still says something. A very short
		// remainder means the lead-in WAS the brief.
		if len([]rune(rest)) >= 8 {
			return rest
		}
		return trimmed
	}
	return trimmed
}

// isBriefBoundary reports whether r can legitimately follow a lead-in — i.e. the
// lead-in was a whole word rather than the start of a longer one.
func isBriefBoundary(r rune) bool {
	return unicode.IsSpace(r) || unicode.IsPunct(r)
}

// fanOutMinIdentityCells is the smallest identity prefix worth showing. Below
// this the prefix is noise rather than a label, and the room is better spent on
// the outcome.
const fanOutMinIdentityCells = 12

// fanOutDetail composes "<identity> · <volatile>" for a fan-out row, but ONLY
// when both halves fit the row's detail budget.
//
// The ordering rule is a safety one, and it was a real regression before this
// function existed. The identity prefix is what makes three concurrent sub-agent
// rows tellable apart, but the volatile half is what says how the run ENDED — and
// at a narrow width the prefix pushed "PARTIAL — stopped early" off the row,
// leaving a partial finding rendered exactly like a complete one. So the volatile
// half is never sacrificed for the identity: if there is not room for a useful
// prefix beside it, the prefix is dropped entirely.
func fanOutDetail(identity, volatile string, room int) string {
	const sep = " · "
	avail := room - cellWidth(volatile) - cellWidth(sep)
	if avail < fanOutMinIdentityCells {
		return volatile
	}
	if avail > fanOutTargetCells {
		avail = fanOutTargetCells
	}
	return truncateCells(identity, avail) + sep + volatile
}

// identityBearingTarget reports whether a tool's target should survive alongside
// its progress/summary, because several calls of it run concurrently under one
// verb and the target is the only thing telling them apart.
//
// Only subagent.run today. agentTask.spawnForEdits also fans out, but its rows
// are already distinguished by the terminal ids in their summaries, and widening
// this would change long-settled rendering for the codebase's busiest tool.
func identityBearingTarget(name string) bool { return name == "subagent.run" }

// renderActivityRow renders one branch row: "<branch> <glyph> <verb> <detail> <dur>".
// last marks the final branch (└─ vs ├─). expanded reveals raw args. now drives
// the live elapsed on an active row.
func renderActivityRow(th theme.Theme, a Activity, last, expanded bool, spinnerFrame int, now int64, width int) string {
	g := th.Glyphs
	branch := g.BranchMid
	if last {
		branch = g.BranchLast
	}
	glyph, tone := activityGlyph(th, a, spinnerFrame)

	// The verb label is rendered RAW (not padded) — the alignment padding goes
	// BEFORE the detail instead. A row that hasn't settled (queued / running /
	// approval-pending) prefers the verb's in-progress form when one exists; the
	// past tense is the settled form.
	label := presentTool(a.Name)
	switch a.State {
	case ActQueued, ActActive, ActWaiting:
		if live := presentToolActiveVerb(a.Name); live != "" {
			label = live
		}
	}

	// The detail budget is computed HERE, before the detail is composed, because
	// the fan-out prefix below has to fit inside it — see fanOutDetail. The final
	// truncate at render time uses this same number.
	labelCols := cellWidth(label) + 1
	if labelCols < labelWidth {
		labelCols = labelWidth
	}
	detailRoom := width - prefixCols - labelCols - durationCols
	if detailRoom < 8 {
		detailRoom = 8
	}

	// Default detail (`a.detail ?? (done ? a.summary)`): the row's
	// own Detail when set (the controller stores the target there, and the result
	// summary on done), otherwise the args-derived target. Either way it is the
	// `a.detail` slot the failure-outcome below appends to.
	detail := a.Detail
	if detail == "" {
		detail = presentToolTarget(a.Name, a.Args)
	}
	// While active, the live in-tool substep overrides ("launching terminal").
	if a.State == ActActive && a.ProgressMsg != "" {
		detail = a.ProgressMsg
	}
	// Fan-out rows KEEP their target, because for these tools the target IS the
	// row's identity. The model is told to delegate several questions at once, so
	// three sub-agents run side by side under the same verb — and with the default
	// rules above, all three would read "round 3/10 · fs.search" while live and
	// "Reported back · 3 rounds…" when settled, i.e. three identical rows for three
	// different questions. Prefixing the (short) brief makes each row say which
	// delegation it is, and puts the volatile half where truncation eats it first.
	if identityBearingTarget(a.Name) {
		if target := presentToolTarget(a.Name, a.Args); target != "" && detail != "" && detail != target {
			detail = fanOutDetail(briefGist(target), detail, detailRoom)
		}
	}
	// On FAILURE, surface the failure summary even when a target detail exists — the
	// outcome must never be hidden behind the original "Reading foo.go" target. The
	// separator is " · " (space-bullet-space).
	if a.State == ActFailed && a.Outcome != "" {
		if detail != "" {
			detail = detail + " · " + a.Outcome
		} else {
			detail = a.Outcome
		}
	}
	// On CANCEL, append the terminal note ("cancelled" / "not run") after the resolved
	// target so an aborted row reads truthfully instead of as a still-pending one.
	if a.State == ActCancelled {
		note := a.Outcome
		if note == "" {
			note = "not run"
		}
		if detail != "" {
			detail = detail + " · " + note
		} else {
			detail = note
		}
	}

	// Elapsed: present only when the row has ended (done/failed) or is active; a
	// queued/waiting row shows NO duration (`elapsed` is undefined
	// → the timing cell renders nothing). Done/failed use ended−started even when
	// started is 0; active uses max(0, now−started).
	showDur := false
	var elapsed int64
	switch {
	case a.EndedAt > 0:
		elapsed = a.EndedAt - a.StartedAt
		showDur = true
	case a.State == ActActive:
		elapsed = now - a.StartedAt
		if elapsed < 0 {
			elapsed = 0
		}
		showDur = true
	}
	right := ""
	if showDur {
		right = formatDuration(elapsed)
	}

	var b strings.Builder
	b.WriteString(th.Muted().Render(branch))
	b.WriteByte(' ')
	b.WriteString(styleFor(th, tone, glyph))
	b.WriteByte(' ')
	b.WriteString(th.Body().Render(label))

	if detail != "" {
		// labelCols / detailRoom were computed above, before the detail was composed
		// (the fan-out prefix needs the budget to decide whether it fits).
		labelLen := cellWidth(label)
		// Pad short labels so details line up in a column; long labels get the single
		// separating space (max(1, LABEL_WIDTH - label.length)).
		pad := labelWidth - labelLen
		if pad < 1 {
			pad = 1
		}
		b.WriteString(strings.Repeat(" ", pad))
		b.WriteString(th.Dim().Render(truncateCells(detail, detailRoom)))
	}

	if right != "" {
		// Right-align the duration into a flush-right gutter (TS used space-between).
		// cellWidth(b) counts styled spans ANSI-aware.
		used := cellWidth(b.String())
		target := width - cellWidth(right)
		if p := target - used; p > 0 {
			b.WriteString(strings.Repeat(" ", p))
		} else {
			b.WriteByte(' ')
		}
		b.WriteString(th.Dim().Render(right))
	}
	// Truncate the assembled row line to width BEFORE appending any expanded-args
	// line, so the multi-line truncate never clips the row's flush-right duration.
	row := truncateCells(b.String(), width)
	if expanded {
		// Expanded view (^X): raw args indented 3 cells, dim.
		row += "\n" + indentLines(th.Dim().Render(a.Name+" args: "+compactArgs(a.Args, max(20, width-12))), 3)
		// `result:` line uses the run's summary (Go: Detail on done, Outcome on fail).
		summary := a.Detail
		if summary == "" {
			summary = a.Outcome
		}
		if summary != "" {
			row += "\n" + indentLines(th.Dim().Render("result: "+truncateCells(summary, width-12)), 3)
		}
	}
	return row
}

// labelWidth / prefixCols / durationCols are the column budgets the activity tree
// aligns to.
const (
	labelWidth   = 11
	prefixCols   = 5
	durationCols = 8
)
