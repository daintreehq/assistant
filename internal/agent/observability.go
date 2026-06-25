package agent

import "fmt"

// observability.go holds the cheap, in-process drift counters (issue #251): the
// compaction-depth tag and the per-tool failure tally. Both are additive signals —
// they never gate a turn, never touch the DB, and (like every side-channel here)
// must never break a tool call. The counters themselves live as s.mu-guarded fields
// on Session; the helpers below are the only writers/readers, so the locking
// discipline stays in one place.

// compactionNotePrefix builds the persisted compaction-note header, tagging it with the
// running compaction depth so a checkpoint-of-checkpoint chain is visible (each pass
// re-flattens the prior note; without the tag, that degradation is invisible). The
// framing is "[checkpoint | depth N]" (issue #256 replaced the old prose "compacted
// summary" wording with a structured state object — the auto-compact body is now JSON;
// the manual /compact path still rides a prose body under this same header). The
// rehydration boundary keys off the SYSTEM compaction marker, not this user note, so
// changing this text is safe (see rehydrate.go).
func compactionNotePrefix(depth int) string {
	return fmt.Sprintf("[checkpoint | depth %d]\n", depth)
}

// recordToolFailure increments the session-cumulative failure tally for one tool
// (keyed by its internal dotted name, e.g. "terminal.read") and returns the new
// count. This is DISTINCT from runToolBatch's per-turn circuit-breaker map: that one
// resets every turn and keys on canonicalized args+errCode to catch a tight retry
// loop; this one accumulates across rounds for the whole session as a coarse "which
// tools keep failing" signal. Takes s.mu itself — runToolBatch holds no lock at the
// call site (it dispatches and pushes results via methods that lock internally), so
// the brief critical section here is safe. Lazy-inits the map so the zero-value
// Session needs no constructor wiring.
func (s *Session) recordToolFailure(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.toolFailures == nil {
		s.toolFailures = make(map[string]int)
	}
	s.toolFailures[name]++
	return s.toolFailures[name]
}

// ToolFailureCounts returns a COPY of the session-cumulative per-tool failure tally
// (the caller may freely read/mutate it without racing the turn goroutine). Empty
// when nothing has failed.
func (s *Session) ToolFailureCounts() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int, len(s.toolFailures))
	for name, n := range s.toolFailures {
		out[name] = n
	}
	return out
}

// CompactionDepth returns how many times this session has compacted (auto or manual
// /compact). Resets to 0 on /clear (the compaction chain is destroyed). Session-
// scoped only — it does NOT survive a process restart; durable depth tracking is the
// Wave-3 structured-checkpoint work's concern.
func (s *Session) CompactionDepth() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.compactionDepth
}
