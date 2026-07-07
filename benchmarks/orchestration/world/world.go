package world

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// World is the stateful fake Daintree a benchmark scenario runs against:
// worktrees, terminals with scripted agents, and a complete call log. It is
// the ground truth a scenario's checks grade — "did the orchestrator get the
// task done" is a predicate over this state plus the call log, never over the
// model's prose alone.
//
// All mutation happens under one mutex; agent state is computed from the wall
// clock on read (see Script), so there are no background goroutines.
type World struct {
	mu sync.Mutex

	ProjectName string
	Worktrees   []Worktree

	terminals map[string]*Terminal
	order     []string // terminal ids in creation order (stable list output)
	calls     []Call
	spawnSeq  int
	dedupe    map[string]string // agent.launch requestKey -> terminalId

	Faults Faults

	// AgentRoster is what agentSettings.get advertises. spawnForEdits validates
	// agent ids against this, so scenarios must use a listed id.
	AgentRoster []string

	// spawnScript is the scenario hook deciding what a spawned agent does —
	// agent.launch consults it to script the new terminal's behaviour.
	spawnScript func(agentID, worktreeID, prompt string) Script

	start time.Time
}

// Worktree is one row of worktree.list.
type Worktree struct {
	ID     string
	Path   string
	Branch string
	Name   string
	Status string // "ready" | "busy" | ...
}

// Terminal is one live (or closed) terminal hosting a scripted agent.
type Terminal struct {
	ID         string
	AgentID    string
	Name       string
	WorktreeID string
	SpawnedAt  time.Time
	Script     Script
	Prompt     string // the taskPrompt the orchestrator spawned it with

	inputs []InputRec
	closed bool
}

// InputRec is one piece of input the orchestrator sent to a terminal.
type InputRec struct {
	At   time.Time
	Text string
}

// Call is one MCP tools/call the world served, in arrival order.
type Call struct {
	Tool string
	Args map[string]any
	At   time.Time
}

// Faults are the injectable Daintree quirks a scenario can switch on.
type Faults struct {
	// BlankStatusTail makes terminal.getStatus's includeOutput tail return
	// whitespace padding (the Codex bottom-padded-TUI quirk) while the deep
	// terminal.getOutput still returns the real scrollback.
	BlankStatusTail bool
	// ThrottleGetOutputN makes the first N terminal.getOutput calls (per
	// terminal) fail with a rate-limit error result before succeeding.
	ThrottleGetOutputN int

	throttleCount map[string]int
}

// New builds an empty world with sane defaults.
func New() *World {
	return &World{
		ProjectName: "bench-project",
		terminals:   map[string]*Terminal{},
		dedupe:      map[string]string{},
		AgentRoster: []string{"claude", "codex"},
		start:       time.Now(),
	}
}

// AddTerminal pre-seeds a terminal that exists before the run starts (its
// script clock begins at world start). Returns the terminal for convenience.
func (w *World) AddTerminal(t Terminal) *Terminal {
	w.mu.Lock()
	defer w.mu.Unlock()
	if t.ID == "" {
		t.ID = w.nextTerminalIDLocked()
	}
	if t.SpawnedAt.IsZero() {
		t.SpawnedAt = w.start
	}
	tt := t
	w.terminals[tt.ID] = &tt
	w.order = append(w.order, tt.ID)
	return &tt
}

func (w *World) nextTerminalIDLocked() string {
	w.spawnSeq++
	// Realistic Daintree id shape: terminal-<uuid-ish>. A stable fake suffix
	// keeps logs readable while still exercising the long-id discipline.
	return fmt.Sprintf("terminal-b%07d-%04d-4bench-8000-%012d", w.spawnSeq, w.spawnSeq, w.spawnSeq)
}

// record appends to the call log. Callers hold no lock.
func (w *World) record(tool string, args map[string]any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls = append(w.calls, Call{Tool: tool, Args: args, At: time.Now()})
}

// Calls returns a copy of the call log.
func (w *World) Calls() []Call {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]Call, len(w.calls))
	copy(out, w.calls)
	return out
}

// CallCount counts served calls for one tool name.
func (w *World) CallCount(tool string) int {
	n := 0
	for _, c := range w.Calls() {
		if c.Tool == tool {
			n++
		}
	}
	return n
}

// Spawned returns the terminals created by agent.launch during the run (i.e.
// not pre-seeded), in creation order.
func (w *World) Spawned() []*Terminal {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []*Terminal
	for _, id := range w.order {
		t := w.terminals[id]
		if !t.SpawnedAt.Equal(w.start) && t.Prompt != "" {
			out = append(out, t)
		}
	}
	return out
}

// Terminal returns a terminal by id (nil when unknown).
func (w *World) Terminal(id string) *Terminal {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.terminals[id]
}

// Inputs returns the inputs a terminal received.
func (w *World) Inputs(id string) []InputRec {
	w.mu.Lock()
	defer w.mu.Unlock()
	t := w.terminals[id]
	if t == nil {
		return nil
	}
	out := make([]InputRec, len(t.inputs))
	copy(out, t.inputs)
	return out
}

// AllInputs returns every input any terminal received, joined for matching.
func (w *World) AllInputs() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var b strings.Builder
	for _, id := range w.order {
		for _, in := range w.terminals[id].inputs {
			b.WriteString(in.Text)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// snapshotLocked computes a terminal's live snapshot now.
func (w *World) snapshotLocked(t *Terminal, now time.Time) Snapshot {
	elapsed := now.Sub(t.SpawnedAt)
	hasInput := len(t.inputs) > 0
	var firstInput time.Duration
	if hasInput {
		firstInput = t.inputs[0].At.Sub(t.SpawnedAt)
	}
	return t.Script.At(elapsed, firstInput, hasInput)
}

// launch creates a terminal for agent.launch. The script is chosen by the
// scenario's SpawnScript hook (falls back to instant-finish).
func (w *World) launch(agentID, name, worktreeID, prompt, requestKey string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if requestKey != "" {
		if id, ok := w.dedupe[requestKey]; ok {
			return id
		}
	}
	id := w.nextTerminalIDLocked()
	script := w.spawnScript
	if script == nil {
		script = func(agentID, worktreeID, prompt string) Script {
			return Script{Phases: []Phase{
				{After: 0, State: "working", Append: "starting...\n"},
				{After: 5 * time.Second, State: "waiting", WaitingReason: "prompt", Append: "done.\n"},
			}}
		}
	}
	t := &Terminal{
		ID:         id,
		AgentID:    agentID,
		Name:       name,
		WorktreeID: worktreeID,
		SpawnedAt:  time.Now(),
		Script:     script(agentID, worktreeID, prompt),
		Prompt:     prompt,
	}
	w.terminals[id] = t
	w.order = append(w.order, id)
	if requestKey != "" {
		w.dedupe[requestKey] = id
	}
	return id
}

// SetSpawnScript installs the scenario's script factory for agent.launch.
func (w *World) SetSpawnScript(f func(agentID, worktreeID, prompt string) Script) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.spawnScript = f
}

// sendInput records input to a terminal (and triggers OnInput rebasing).
func (w *World) sendInput(id, text string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	t := w.terminals[id]
	if t == nil || t.closed {
		return false
	}
	t.inputs = append(t.inputs, InputRec{At: time.Now(), Text: text})
	return true
}

// close marks a terminal closed; closed terminals leave terminal.list and
// getStatus reports them per-entry as unknown.
func (w *World) close(id string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	t := w.terminals[id]
	if t == nil || t.closed {
		return false
	}
	t.closed = true
	return true
}

// IsClosed reports whether a terminal has been closed.
func (w *World) IsClosed(id string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	t := w.terminals[id]
	return t != nil && t.closed
}

// rename retitles a terminal.
func (w *World) rename(id, name string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	t := w.terminals[id]
	if t == nil || t.closed {
		return false
	}
	t.Name = name
	return true
}

// throttleTick returns true when this getOutput call should be rejected with a
// rate-limit result under Faults.ThrottleGetOutputN.
func (w *World) throttleTick(terminalID string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.Faults.ThrottleGetOutputN <= 0 {
		return false
	}
	if w.Faults.throttleCount == nil {
		w.Faults.throttleCount = map[string]int{}
	}
	if w.Faults.throttleCount[terminalID] >= w.Faults.ThrottleGetOutputN {
		return false
	}
	w.Faults.throttleCount[terminalID]++
	return true
}
