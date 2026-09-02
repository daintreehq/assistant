package host

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// presentation.go carries the activity tree's HUMAN VERBS to an embedded host.
//
// The cockpit turned every tool call into "<verb> <target>" — "Read src/main.go",
// "Delegating the auth refactor" — and its own comment called that the brand signature
// of the activity tree. A protocol-only consumer could not reproduce it: the verb table
// keys off internal tool names, and the target is pulled out of the RAW argument JSON,
// which never leaves this process. A host left showing `fs.read` beside a redacted
// argument blob is strictly less legible than the terminal it replaced.
//
// So the derivation stays here, next to the args, and the verb and target travel as
// DATA — the same split masthead.go uses. The host decides how to draw them; the engine
// stays the single source of what they say.
//
// The target is REDACTED on the way out (bridge.go), because it is drawn from raw
// arguments: a command line or a memory body can carry a credential, and the whole
// reason raw args do not cross this boundary is that they cannot be trusted not to.

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
// HasPresentation reports whether the table knows a human verb for a tool.
//
// Exported for ONE reason: the completeness contract in internal/app, which is the
// only package where the real registry is assembled and can therefore ask "does every
// tool we actually register have a verb?". A tool that does not renders in a host's
// activity tree as a raw dotted identifier — a deliberate fallback, and therefore a
// gap that fails nothing and looks like the feature working.
func HasPresentation(name string) bool {
	verb, _ := presentToolVerb(name)
	return verb != ""
}

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
	case "tool.schema":
		return "Read tool schema", []string{"name"}
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
	case "agentSessionHistory.list":
		return "Listed past sessions", nil
	case "browser.getConsoleMessages":
		return "Read console", []string{"terminalId"}
	case "errors.recent":
		// "Diagnostics", not "errors": the tool reads Daintree's own diagnostics log,
		// and a row saying "Read errors" reads as though the run had failed.
		return "Read diagnostics", nil
	case "notifications.recent":
		return "Read notifications", nil
	case "project.detectRunners":
		return "Listed commands", nil
	case "project.runCheck":
		// Matches terminal.sendCommand's "Ran": from the reader's side these are the
		// same event, and the difference is which surface it happened on.
		return "Ran", []string{"runnerId", "cwd:rel"}
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
	case "runbook.step.advance":
		return "Advanced step", []string{"runbookId"}
	case "runbook.run.get":
		return "Checked runbook progress", []string{"runbookId"}
	case "worktree.createWithRecipe":
		return "Created worktree", []string{"recipeId"}
	case "worktree.list":
		return "Listed worktrees", nil
	case "worktree.getCurrent":
		return "Read worktree", nil
	case "worktree.resource.status":
		// "Load", not "resource status": the row has to say what the reader learns,
		// and what this answers is how hard the machine is working.
		return "Read load", []string{"worktreeId"}
	case "git.getProjectPulse":
		// The tool's own words are "branch, uncommitted/staged changes and recent
		// commits" — which is the git state, so that is what the row says.
		return "Read git state", []string{"worktreeId"}
	case "forge.getIssue":
		return "Read issue", []string{"issueNumber"}
	case "forge.listIssues":
		return "Listed issues", nil
	case "forge.listPRs":
		return "Listed PRs", nil
	case "forge.getPR":
		return "Read PR", []string{"prNumber"}
	case "forge.getPRs":
		// ":ids" joins the array. The bare key would fall through to strArg, which
		// handles only strings and numbers, so a list argument resolves to "" and the
		// row shows no target at all — the same silent gap the terminalIds entries
		// above use this mode to avoid.
		return "Read PRs", []string{"prNumbers:ids"}
	case "forge.getChecks":
		return "Read CI", []string{"prNumber"}
	case "forge.listIssueComments":
		return "Read comments", []string{"issueNumber"}
	case "watcher.watchPR":
		return "Watching PR", []string{"prNumber"}
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
		return "Called", []string{"name"}
	case "daintree.invoke":
		// Same shape as daintree.call — one untyped action, named by its argument —
		// so it reads the same way rather than inventing a second word for it.
		return "Called", []string{"action"}
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
	if v := resolveTargetKeys(obj, keys); v != "" {
		return v
	}
	// The mcpwrap opaque-args wrappers (forgeRead: git.getProjectPulse, forge.getIssue,
	// forge.list*, worktree.list/getCurrent) take NO top-level target at all — their
	// whole payload is one `arguments` object forwarded verbatim, so the id the row
	// wants to name is one level down. Unwrapping here rather than per-tool keeps the
	// table describing what the row SHOWS while the envelope stays a transport detail;
	// a tool that has the key at the top level never reaches this line.
	if nested, ok := obj["arguments"].(map[string]any); ok {
		return resolveTargetKeys(nested, keys)
	}
	return ""
}

// resolveTargetKeys returns the first key in keys that resolves against obj, "" if none
// does. A "key:mode" entry resolves specially: ":lit" is a literal string (e.g.
// "workspace context"), ":rel" relativizes a path, ":ids" joins an array.
func resolveTargetKeys(obj map[string]any, keys []string) string {
	for _, k := range keys {
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

// truncateCells bounds a target to w characters with an ellipsis.
//
// The cockpit measured in terminal CELLS because it was laying out a fixed-width grid.
// Here the host lays out the row, so this is only a sanity bound on what crosses the
// wire — runes are the right unit, and a CJK target that measures wider than it counts
// costs nothing now that nothing is being aligned against it.
func truncateCells(s string, w int) string {
	if w <= 0 {
		return ""
	}
	rs := []rune(s)
	if len(rs) <= w {
		return s
	}
	return string(rs[:w]) + "…"
}
