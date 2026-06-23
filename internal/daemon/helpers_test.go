package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// errModelMustNotRun is returned by a guard model whose Classify must never be
// reached; the engine treats a Classify error as the unknown fallback, but the
// surrounding tests assert classification stays no_change (the model guard held)
// without the model contributing a verdict.
var errModelMustNotRun = errors.New("model must not be consulted")

// statusErrMCP is a fake whose terminal.getStatus returns IsError (a transient
// transport failure). Absence under a failed status call must never be read as a
// closed terminal.
type statusErrMCP struct{}

func (statusErrMCP) Connected() bool                               { return true }
func (statusErrMCP) SupportsSubscribe() bool                       { return false }
func (statusErrMCP) Subscribe(_ context.Context, _ string) error   { return nil }
func (statusErrMCP) Unsubscribe(_ context.Context, _ string) error { return nil }
func (statusErrMCP) CallRead(_ context.Context, name string, _ map[string]any) (MCPResult, error) {
	if name == "terminal.getStatus" {
		return MCPResult{Text: "boom", IsError: true}, nil
	}
	return MCPResult{StructuredContent: map[string]any{"content": ""}}, nil
}

// termCfg is one terminal's scripted state for the programmable MCP fake.
type termCfg struct {
	agentID       string // emitted as agentId in getStatus (keys the subscribable resource)
	agentState    string
	waitingReason string
	recentOutput  *string // nil ⇒ omitted from getStatus (forces getOutput fallback)
	tail          string  // returned by terminal.getOutput
	exitCode      *int
}

// progMCP is a programmable MCP fake used across the
// watcher multi-terminal / rate-limit / verification suites: a batched
// terminal.getStatus keyed by includeOutput, a per-terminal terminal.getOutput, a
// configurable terminal.list inventory, and a git pulse. It records every call so
// tests can assert batching and fallback behavior.
type progMCP struct {
	mu          sync.Mutex
	connected   bool
	perTerminal map[string]termCfg
	// list, when non-nil, is returned for terminal.list (each entry already a
	// map[string]any so a test can shape id/agentState/exitCode freely). When
	// listResult is set it overrides list (for error/unparseable cases).
	list       []map[string]any
	listResult *MCPResult
	pulse      *MCPResult // git.getProjectPulse override (default clean)
	calls      []mcpCall

	// resource-subscription seam
	supportsSub  bool
	subErr       error
	subscribed   []string
	unsubscribed []string
}

type mcpCall struct {
	name string
	args map[string]any
}

func newProgMCP(perTerminal map[string]termCfg) *progMCP {
	return &progMCP{connected: true, perTerminal: perTerminal}
}

func (m *progMCP) callsFor(name string) []mcpCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []mcpCall
	for _, c := range m.calls {
		if c.name == name {
			out = append(out, c)
		}
	}
	return out
}

func (m *progMCP) Connected() bool { return m.connected }

func (m *progMCP) SupportsSubscribe() bool { return m.supportsSub }

func (m *progMCP) Subscribe(_ context.Context, uri string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.subErr != nil {
		return m.subErr
	}
	m.subscribed = append(m.subscribed, uri)
	return nil
}

func (m *progMCP) Unsubscribe(_ context.Context, uri string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.unsubscribed = append(m.unsubscribed, uri)
	return nil
}

func (m *progMCP) subscribeCalls() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.subscribed...)
}

func (m *progMCP) unsubscribeCalls() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.unsubscribed...)
}

func (m *progMCP) CallRead(_ context.Context, name string, args map[string]any) (MCPResult, error) {
	m.mu.Lock()
	m.calls = append(m.calls, mcpCall{name: name, args: args})
	m.mu.Unlock()

	switch name {
	case "terminal.getStatus":
		ids := stringIDs(args["terminalIds"])
		_, wantOutput := args["includeOutput"]
		var terminals []map[string]any
		for _, id := range ids {
			cfg, ok := m.perTerminal[id]
			if !ok {
				continue // omitted ⇒ absent from getStatus
			}
			e := map[string]any{"terminalId": id}
			if cfg.agentID != "" {
				e["agentId"] = cfg.agentID
			}
			if cfg.agentState != "" {
				e["agentState"] = cfg.agentState
			}
			if cfg.waitingReason != "" {
				e["waitingReason"] = cfg.waitingReason
			}
			if wantOutput && cfg.recentOutput != nil {
				e["recentOutput"] = *cfg.recentOutput
			}
			if cfg.exitCode != nil {
				e["exitCode"] = float64(*cfg.exitCode)
			}
			terminals = append(terminals, e)
		}
		body, _ := json.Marshal(map[string]any{"terminals": terminals})
		return MCPResult{Text: string(body)}, nil

	case "terminal.getOutput":
		id, _ := args["terminalId"].(string)
		cfg := m.perTerminal[id]
		return MCPResult{StructuredContent: map[string]any{"terminalId": id, "content": cfg.tail}}, nil

	case "terminal.list":
		if m.listResult != nil {
			return *m.listResult, nil
		}
		body, _ := json.Marshal(map[string]any{"terminals": m.list})
		return MCPResult{Text: string(body)}, nil

	case "git.getProjectPulse":
		if m.pulse != nil {
			return *m.pulse, nil
		}
		return MCPResult{StructuredContent: map[string]any{"isDirty": false, "changedFiles": float64(0)}}, nil
	}
	return MCPResult{IsError: true}, nil
}

func stringIDs(v any) []string {
	arr, ok := v.([]any)
	if ok {
		out := make([]string, 0, len(arr))
		for _, x := range arr {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	if ss, ok := v.([]string); ok {
		return ss
	}
	return nil
}

// progModel routes Classify vs Judge independently: judgeFn decides each yes/no
// from (question, tail).
type progModel struct {
	verdict  domain.WatcherVerdict
	classErr error
	judgeFn  func(question, tail string) domain.ModelJudgeAnswer
}

func (m *progModel) Classify(_ context.Context, _ ClassifyInput) (domain.WatcherVerdict, error) {
	if m.classErr != nil {
		return domain.WatcherVerdict{}, m.classErr
	}
	return m.verdict, nil
}

func (m *progModel) Judge(_ context.Context, in JudgeInput) (domain.ModelJudgeAnswer, error) {
	if m.judgeFn != nil {
		return m.judgeFn(in.Question, in.Tail), nil
	}
	return domain.ModelJudgeAnswer{}, nil
}

// tierRecorder records the ModelTier passed to Classify/Judge so a test can assert
// the watcher pins the small tier regardless of the stored rec.ModelTier. Judges
// run in parallel goroutines, so access is mutex-guarded.
type tierRecorder struct {
	mu      sync.Mutex
	cTier   domain.ModelTier
	jTier   domain.ModelTier
	cCalled bool
	jCalled bool
	verdict domain.WatcherVerdict
	judgeFn func(question, tail string) domain.ModelJudgeAnswer
}

func (m *tierRecorder) Classify(_ context.Context, in ClassifyInput) (domain.WatcherVerdict, error) {
	m.mu.Lock()
	m.cTier, m.cCalled = in.Tier, true
	m.mu.Unlock()
	return m.verdict, nil
}

func (m *tierRecorder) Judge(_ context.Context, in JudgeInput) (domain.ModelJudgeAnswer, error) {
	m.mu.Lock()
	m.jTier, m.jCalled = in.Tier, true
	m.mu.Unlock()
	if m.judgeFn != nil {
		return m.judgeFn(in.Question, in.Tail), nil
	}
	return domain.ModelJudgeAnswer{}, nil
}

func (m *tierRecorder) classifyTier() (domain.ModelTier, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cTier, m.cCalled
}

func (m *tierRecorder) judgeTier() (domain.ModelTier, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.jTier, m.jCalled
}

func strptr(s string) *string { return &s }

// watcherWith builds a terminal watcher record with options/alert/stop conditions.
func watcherWith(id string, targets []string, fns ...func(*domain.WatcherRecord)) domain.WatcherRecord {
	tj, _ := json.Marshal(targets)
	rec := domain.WatcherRecord{
		ID: id, Kind: "terminal", Title: "Sup", Goal: "do the task",
		TargetsJson: string(tj), CadenceMs: 10_000, ModelTier: domain.ModelSmall,
		Status: "active", NextCheckAt: 0, CreatedAt: domain.NowMS(),
	}
	for _, fn := range fns {
		fn(&rec)
	}
	return rec
}

func withOptions(opts watcherOptions) func(*domain.WatcherRecord) {
	return func(r *domain.WatcherRecord) {
		b, _ := json.Marshal(opts)
		r.OptionsJson = ptrStr(string(b))
	}
}

func withAlert(raw string) func(*domain.WatcherRecord) {
	return func(r *domain.WatcherRecord) { r.AlertWhenJson = ptrStr(raw) }
}

func withStop(raw string) func(*domain.WatcherRecord) {
	return func(r *domain.WatcherRecord) { r.StopWhenJson = ptrStr(raw) }
}

func withSupervisor() func(*domain.WatcherRecord) {
	return func(r *domain.WatcherRecord) { b := true; r.IsSupervisor = &b }
}

// aged moves a record's createdAt past the spawn grace.
func aged(rec domain.WatcherRecord) domain.WatcherRecord {
	rec.CreatedAt = domain.NowMS() - WatcherSpawnGraceMS - 1000
	return rec
}

// terminalTargets pulls the per-terminal event terminalIds from the queue.
func publishedTerminals(q *fakeQueue) []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []string
	for _, p := range q.published {
		if p.Target != nil {
			out = append(out, p.Target.TerminalID)
		}
	}
	return out
}
