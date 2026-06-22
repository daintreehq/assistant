package daemon

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// PrWatcherOptions is the last-seen PR state persisted in WatcherRecord.optionsJson
// between ticks.
type PrWatcherOptions struct {
	Cwd           string `json:"cwd,omitempty"`
	PrNumber      int    `json:"prNumber"`
	LastState     string `json:"lastState,omitempty"`
	LastIsDraft   *bool  `json:"lastIsDraft,omitempty"`
	LastUpdatedAt string `json:"lastUpdatedAt,omitempty"`
}

// PrTransition is the transition published in a tick, if any.
type PrTransition string

const (
	PrTransitionNone        PrTransition = ""
	PrTransitionStateChange PrTransition = "state_change"
	PrTransitionDraftReady  PrTransition = "draft_ready"
	PrTransitionActivity    PrTransition = "activity"
)

// PrCheckResult records what a single PR check did (returned for testability).
type PrCheckResult struct {
	Status     string
	Transition PrTransition
	Published  bool
	State      string
}

// prFields are the normalized PR fields this engine reads across forge dialects.
type prFields struct {
	state     string // "open" | "closed" | "merged" | ""
	hasState  bool
	isDraft   *bool
	updatedAt string
	title     string
}

// RunPrWatcherCheck runs one PR-state watcher check: poll forge.getPR, diff vs the
// last-seen state in optionsJson, publish any transition, persist the new baseline.
// Transient failures reschedule without publishing. Corrupt options disable the
// watcher.
func RunPrWatcherCheck(ctx *CheckContext, rec domain.WatcherRecord) PrCheckResult {
	now := domain.NowMS()

	// --- parse persisted options ---------------------------------------------
	var options PrWatcherOptions
	corrupt := true
	if rec.OptionsJson != nil && strings.TrimSpace(*rec.OptionsJson) != "" {
		// Require a numeric prNumber: decode into a generic map first so a missing
		// field is distinguishable from a zero value.
		var probe map[string]any
		if json.Unmarshal([]byte(*rec.OptionsJson), &probe) == nil {
			if _, ok := probe["prNumber"].(float64); ok {
				if json.Unmarshal([]byte(*rec.OptionsJson), &options) == nil {
					corrupt = false
				}
			}
		}
	}
	if corrupt {
		_ = ctx.Store.UpdateWatcher(rec.ID, map[string]any{"status": "error", "lastCheckedAt": now})
		_, _ = ctx.Store.RevokeGrantsByActor(rec.ID, now)
		_ = ctx.Queue.Publish(domain.QueuePublishArgs{
			Source:        domain.SourcePRWatcher,
			Severity:      domain.SeverityError,
			Title:         rec.Title + ": watcher disabled",
			Summary:       fmt.Sprintf("Corrupt PR watcher state for %s.", rec.ID),
			EpistemicKind: domain.EpistemicUnverified,
		})
		return PrCheckResult{Status: "error", Published: false}
	}

	// Timeout wins before any read.
	if rec.StopAfterMs != nil && now-rec.CreatedAt >= *rec.StopAfterMs {
		_ = ctx.Store.UpdateWatcher(rec.ID, map[string]any{"status": "timeout", "lastCheckedAt": now})
		_, _ = ctx.Store.RevokeGrantsByActor(rec.ID, now)
		_ = ctx.Queue.Publish(domain.QueuePublishArgs{
			Source:        domain.SourcePRWatcher,
			Severity:      domain.SeverityInfo,
			Title:         rec.Title + ": watch ended",
			Summary:       fmt.Sprintf("Stopped watching PR #%d after the configured timeout; no terminal transition observed.", options.PrNumber),
			EpistemicKind: domain.EpistemicObserved,
			DedupeKey:     fmt.Sprintf("pr_watcher:%s:timeout", rec.ID),
		})
		return PrCheckResult{Status: "timeout", Published: true}
	}

	reschedule := func() {
		_ = ctx.Store.UpdateWatcher(rec.ID, map[string]any{
			"lastCheckedAt": now,
			"nextCheckAt":   now + PRWatcherCadenceMS,
			"status":        "active",
		})
	}

	// --- transient guards: never stop on a hiccup ----------------------------
	if !ctx.MCP.Connected() {
		reschedule()
		return PrCheckResult{Status: "active", Published: false}
	}
	cwd := options.Cwd
	if cwd == "" {
		cwd = ctx.ProjectPath
	}
	res, err := ctx.MCP.CallRead(ctx.Ctx, "forge.getPR", map[string]any{
		"cwd":      cwd,
		"prNumber": options.PrNumber,
	})
	if err != nil {
		reschedule()
		return PrCheckResult{Status: "active", Published: false}
	}
	if res.IsError {
		reschedule()
		return PrCheckResult{Status: "active", Published: false}
	}
	fields, ok := extractPrFields(res)
	if !ok {
		reschedule()
		return PrCheckResult{Status: "active", Published: false}
	}

	// --- diff against the last-seen baseline ---------------------------------
	firstObservation := options.LastState == ""
	stateChanged := fields.hasState && fields.state != options.LastState
	becameTerminal := (fields.state == "merged" || fields.state == "closed") && (firstObservation || stateChanged)
	draftReady := options.LastIsDraft != nil && *options.LastIsDraft && fields.isDraft != nil && !*fields.isDraft
	activity := !becameTerminal && !draftReady && !stateChanged &&
		advanced(options.LastUpdatedAt, fields.updatedAt)

	prLabel := fmt.Sprintf("PR #%d", options.PrNumber)
	titleSuffix := ""
	if fields.title != "" {
		titleSuffix = " — " + fields.title
	}
	result := PrCheckResult{Status: "active", Published: false, State: fields.state}

	switch {
	case becameTerminal:
		verb := "closed"
		if fields.state == "merged" {
			verb = "merged"
		}
		_ = ctx.Queue.Publish(domain.QueuePublishArgs{
			Source:        domain.SourcePRWatcher,
			Severity:      domain.SeverityAttention,
			Title:         fmt.Sprintf("%s: %s %s", rec.Title, prLabel, verb),
			Summary:       fmt.Sprintf("%s is %s%s.", prLabel, verb, titleSuffix),
			Evidence:      []string{prLabel, "state: " + fields.state},
			EpistemicKind: domain.EpistemicObserved,
			DedupeKey:     fmt.Sprintf("pr_watcher:%s:state_change", rec.ID),
		})
		result = PrCheckResult{Status: "condition_met", Transition: PrTransitionStateChange, Published: true, State: fields.state}
	case draftReady:
		_ = ctx.Queue.Publish(domain.QueuePublishArgs{
			Source:        domain.SourcePRWatcher,
			Severity:      domain.SeverityAttention,
			Title:         fmt.Sprintf("%s: %s ready for review", rec.Title, prLabel),
			Summary:       fmt.Sprintf("%s moved out of draft and is ready for review%s.", prLabel, titleSuffix),
			Evidence:      []string{prLabel, "draft: false"},
			EpistemicKind: domain.EpistemicObserved,
			DedupeKey:     fmt.Sprintf("pr_watcher:%s:draft_ready", rec.ID),
		})
		result = PrCheckResult{Status: "active", Transition: PrTransitionDraftReady, Published: true, State: fields.state}
	case activity:
		updatedEv := "updatedAt advanced"
		if fields.updatedAt != "" {
			updatedEv = "updatedAt: " + fields.updatedAt
		}
		_ = ctx.Queue.Publish(domain.QueuePublishArgs{
			Source:        domain.SourcePRWatcher,
			Severity:      domain.SeverityInfo,
			Title:         fmt.Sprintf("%s: %s updated", rec.Title, prLabel),
			Summary:       fmt.Sprintf("%s has new activity (comment, push, or review)%s.", prLabel, titleSuffix),
			Evidence:      []string{prLabel, updatedEv},
			EpistemicKind: domain.EpistemicObserved,
			DedupeKey:     fmt.Sprintf("pr_watcher:%s:activity", rec.ID),
		})
		result = PrCheckResult{Status: "active", Transition: PrTransitionActivity, Published: true, State: fields.state}
	}

	// --- persist the new baseline + schedule ---------------------------------
	next := options
	if fields.hasState {
		next.LastState = fields.state
	}
	if fields.isDraft != nil {
		next.LastIsDraft = fields.isDraft
	}
	if fields.updatedAt != "" {
		next.LastUpdatedAt = fields.updatedAt
	}
	nextJSON, _ := json.Marshal(next)
	lastClassification := "no_change"
	if result.Transition != PrTransitionNone {
		lastClassification = string(result.Transition)
	}
	_ = ctx.Store.UpdateWatcher(rec.ID, map[string]any{
		"lastClassification": lastClassification,
		"lastEpistemicKind":  string(domain.EpistemicObserved),
		"lastCheckedAt":      now,
		"nextCheckAt":        now + PRWatcherCadenceMS,
		"optionsJson":        string(nextJSON),
		"status":             result.Status,
	})
	if result.Status == "condition_met" {
		_, _ = ctx.Store.RevokeGrantsByActor(rec.ID, now)
	}
	return result
}

// candidateObjects collects every plausible PR object: structuredContent, a parsed
// JSON text body, plus one-level unwraps under common wrapper keys.
func candidateObjects(res MCPResult) []map[string]any {
	var out []map[string]any
	wrapperKeys := []string{"pr", "pullRequest", "mergeRequest", "result", "data"}
	consider := func(v any) {
		obj, ok := v.(map[string]any)
		if !ok {
			return
		}
		out = append(out, obj)
		for _, k := range wrapperKeys {
			if nested, ok := obj[k].(map[string]any); ok {
				out = append(out, nested)
			}
		}
	}
	if res.StructuredContent != nil {
		consider(res.StructuredContent)
	}
	if strings.TrimSpace(res.Text) != "" {
		var parsed any
		if json.Unmarshal([]byte(res.Text), &parsed) == nil {
			consider(parsed)
		}
	}
	return out
}

// extractPrFields normalizes a PR object's lifecycle fields across forge dialects.
// Returns (fields, false) when no candidate looks like a PR (so a partial payload
// never fabricates a transition).
func extractPrFields(res MCPResult) (prFields, bool) {
	for _, obj := range candidateObjects(res) {
		rawState := strings.ToLower(asString(obj["state"]))
		merged := obj["merged"] == true ||
			asString(obj["merged_at"]) != "" ||
			asString(obj["mergedAt"]) != "" ||
			rawState == "merged"
		// Neither a state nor a merged signal → not a PR payload.
		if rawState == "" && !merged {
			continue
		}
		var state string
		switch {
		case merged:
			state = "merged"
		case rawState == "closed":
			state = "closed"
		case rawState == "open" || rawState == "opened":
			state = "open"
		default:
			// Recognized-but-unknown state (likely an envelope field) — fall through
			// to the next candidate (the unwrapped PR).
			continue
		}
		isDraft := firstBool(obj["isDraft"], obj["draft"], obj["work_in_progress"], obj["workInProgress"])
		updatedAt := asString(obj["updatedAt"])
		if updatedAt == "" {
			updatedAt = asString(obj["updated_at"])
		}
		return prFields{
			state:     state,
			hasState:  true,
			isDraft:   isDraft,
			updatedAt: updatedAt,
			title:     asString(obj["title"]),
		}, true
	}
	return prFields{}, false
}

// advanced reports whether next is a strictly later valid timestamp than prev.
func advanced(prev, next string) bool {
	if prev == "" || next == "" {
		return false
	}
	p, err1 := time.Parse(time.RFC3339, prev)
	n, err2 := time.Parse(time.RFC3339, next)
	if err1 != nil || err2 != nil {
		// Fall back to a permissive parse for non-RFC3339 ISO strings.
		p2, ok1 := parseLooseTime(prev)
		n2, ok2 := parseLooseTime(next)
		if !ok1 || !ok2 {
			return false
		}
		return n2.After(p2)
	}
	return n.After(p)
}

func parseLooseTime(s string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05Z", "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func firstBool(vals ...any) *bool {
	for _, v := range vals {
		if b, ok := v.(bool); ok {
			return &b
		}
	}
	return nil
}
