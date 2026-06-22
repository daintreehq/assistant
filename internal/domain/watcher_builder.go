package domain

import "encoding/json"

// SupervisorWatcherSpec is the caller-varying input to a supervisor watcher. It
// captures exactly the fields that differ between the spawn-for-edits attach, the
// workflow.startWorkOnIssue attach, and the superviseTerminal adopt path — every
// other field of a supervisor WatcherRecord is fixed and assembled by the builder.
type SupervisorWatcherSpec struct {
	TerminalID string // the terminal to supervise (the sole watcher target)
	WorktreeID string // scopes post-completion git verification (omitted ⇒ active worktree)
	Title      string // human label, already including any "watch " prefix
	Goal       string // what the supervisor watches for
	CadenceMs  int    // check cadence (storage floors it to the scheduler tick)
	SpawnMode  string // "edit" | "explore" — the agent's intent, recorded in options
	// AcceptanceCriteria, when set, is persisted so the supervisor gates completion
	// on evidence (the criteria are met) rather than git cleanliness alone.
	AcceptanceCriteria string
	// ExtraOptions are merged into optionsJson last (e.g. {"adoptMode": true} for an
	// adopted terminal) — they win over the builder-set keys on a collision.
	ExtraOptions map[string]any
}

// BuildSupervisorWatcherRecord assembles the canonical supervisor WatcherRecord
// shared by every attach path: kind "terminal", isSupervisor true, the small model
// tier, a single-target JSON array, and an options blob carrying spawnMode plus the
// optional verificationScope / acceptanceCriteria. It is the single source of truth
// for "what a supervisor watcher looks like" so the three call sites can never drift.
// Pure (stdlib only) — the id/createdAt/nextCheckAt it stamps are kept by storage's
// InsertWatcher (which only fills these when zero), so a builder-stamped record and a
// bare one insert identically.
func BuildSupervisorWatcherRecord(spec SupervisorWatcherSpec) WatcherRecord {
	opts := map[string]any{"spawnMode": spec.SpawnMode}
	if spec.WorktreeID != "" {
		opts["verificationScope"] = map[string]any{"worktreeId": spec.WorktreeID}
	}
	if spec.AcceptanceCriteria != "" {
		opts["acceptanceCriteria"] = spec.AcceptanceCriteria
	}
	for k, v := range spec.ExtraOptions {
		opts[k] = v
	}
	optsJSON, _ := json.Marshal(opts)
	optsStr := string(optsJSON)
	targetsJSON, _ := json.Marshal([]string{spec.TerminalID})

	isSup := true
	now := NowMS()
	return WatcherRecord{
		ID:           NewID(PrefixWatcher),
		Kind:         "terminal",
		Title:        spec.Title,
		Goal:         spec.Goal,
		TargetsJson:  string(targetsJSON),
		CadenceMs:    spec.CadenceMs,
		IsSupervisor: &isSup,
		ModelTier:    ModelSmall,
		OptionsJson:  &optsStr,
		Status:       "active",
		NextCheckAt:  now,
		CreatedAt:    now,
	}
}
