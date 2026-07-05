package ui

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/daintreehq/daintree-assistant/internal/ui/theme"
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
	case "terminal.extract.async":
		return "Extracting", []string{"terminalIds:ids"}
	case "terminal.summarize":
		return "Summarized", []string{"terminalId"}
	case "queue.publish":
		return "Raised", []string{"title"}
	case "queue.digest":
		return "Read inbox", nil
	case "queue.resolve":
		return "Resolved", []string{"id"}
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
	case "workflow.startWorkOnIssue":
		return "Started work", []string{"issueNumber", "title"}
	case "workflow.prepBranchForReview":
		return "Prepping branch", []string{"branch", "worktreeId"}
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
	default:
		return "", nil
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
	case "info":
		return th.Info().Render(s)
	default:
		return th.Body().Render(s)
	}
}

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
	// BEFORE the detail instead.
	label := presentTool(a.Name)

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
		// Budget the detail so a long one truncates BEFORE colliding with the timing:
		// labelCols = max(label+1, LABEL_WIDTH);
		// detailRoom = max(8, width - PREFIX_COLS - labelCols - DURATION_COLS).
		labelLen := cellWidth(label)
		labelCols := labelLen + 1
		if labelCols < labelWidth {
			labelCols = labelWidth
		}
		detailRoom := width - prefixCols - labelCols - durationCols
		if detailRoom < 8 {
			detailRoom = 8
		}
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
