// Package terminalobs is the session-scoped, cross-call terminal observation
// memory: which agent terminals this process has SEEN working, and when the
// assistant last injected input into each. It exists to kill a systematic wall-
// clock tax in the in-turn waits (terminal.awaitAll / terminal.extract wait:{}):
// their working→waiting settle gate latched "seen working" PER CALL, so a
// RE-await of an agent that finished between two waits started from zero
// evidence and could not settle before the full FinishSettleGraceMS (20s) —
// even though the previous await in the same session had watched that same
// agent work for a minute (ses_49ca848d: a 20.1s re-await of an agent that had
// been idle for 16s). Persisting the latch across calls lets a re-await settle
// on its first poll.
//
// The latch must NOT outlive the evidence: input we inject (terminal.sendCommand,
// terminal.run.async, copyTree.injectToTerminal) may start a NEW task, and prior
// work proves nothing about the new prompt — a wait seeded from stale evidence
// would settle "finished" on an agent that has not yet picked the new task up
// (the exact pre-start false positive the grace exists to prevent). So the send
// wrappers record MarkCommandSent ON ATTEMPT (before the send — an ambiguous
// transport failure may still have delivered the input, so the mark cannot wait
// for a confirmed success), and the latch only counts when the working
// observation is STRICTLY after the last send. A same-millisecond tie fails
// safe: the wait falls back to collecting fresh evidence / the spawn grace.
//
// Contract limits, by design: only ASSISTANT-injected immediate input is
// tracked. Deferred injections (terminal.arm — fires later, when the agent
// settles) and a human typing into the terminal directly are invisible here;
// the waits' judge/grace gates remain the safety net for those, exactly as
// before this memory existed. Deliberately in-memory and per-process: the grace
// fallback still covers a fresh process, and persisting "seen working" across
// owners would import the staleness problems the per-call latch was avoiding.
package terminalobs

import "sync"

// Memory is the threadsafe observation registry. The zero value is usable; all
// methods are nil-receiver-safe so unwired consumers (tests, stripped tool sets)
// degrade to the old per-call behaviour.
type Memory struct {
	mu   sync.Mutex
	byID map[string]record
}

type record struct {
	worked        bool  // any working observation at all (robust to a zero clock)
	commanded     bool  // any input injection at all (robust to a zero clock)
	lastWorkingAt int64 // last live "working" state / advancing-tail observation
	lastCommandAt int64 // last (attempted) input injection into the terminal
}

// NewMemory returns an empty observation memory.
func NewMemory() *Memory { return &Memory{} }

// MarkWorking records that the terminal's agent was observed working (a live
// "working" agentState, or a genuinely advancing tail) at the given epoch-ms.
func (m *Memory) MarkWorking(terminalID string, at int64) {
	if m == nil || terminalID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.byID == nil {
		m.byID = make(map[string]record)
	}
	r := m.byID[terminalID]
	r.worked = true
	if at > r.lastWorkingAt {
		r.lastWorkingAt = at
	}
	m.byID[terminalID] = r
}

// MarkCommandSent records that the assistant (attempted to) inject input into
// the terminal at the given epoch-ms, invalidating any earlier working evidence
// for settle purposes (the injected input may start a new task).
func (m *Memory) MarkCommandSent(terminalID string, at int64) {
	if m == nil || terminalID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.byID == nil {
		m.byID = make(map[string]record)
	}
	r := m.byID[terminalID]
	r.commanded = true
	if at > r.lastCommandAt {
		r.lastCommandAt = at
	}
	m.byID[terminalID] = r
}

// SeenWorkingSinceLastCommand reports whether the terminal has been observed
// working STRICTLY after the assistant's last input injection — the cross-call
// form of the waits' seenWorking settle gate. A same-millisecond tie fails
// safe (false): a delayed poll mark and a send can land in the same clock tick
// in either real order, and a wrong "true" would settle a wait on evidence from
// the task BEFORE the send. False merely routes the wait to the slow
// fresh-evidence/grace path.
func (m *Memory) SeenWorkingSinceLastCommand(terminalID string) bool {
	if m == nil || terminalID == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.byID[terminalID]
	if !ok || !r.worked {
		return false
	}
	return !r.commanded || r.lastWorkingAt > r.lastCommandAt
}
