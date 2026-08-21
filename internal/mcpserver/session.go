package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/daintreehq/assistant/internal/agent"
	"github.com/daintreehq/assistant/internal/app"
	"github.com/daintreehq/assistant/internal/domain"
)

// Runtime is the seam to a live assistant. The registry depends on this interface, not
// on *app.App, so the server's session lifecycle can be exercised against a fake
// without a database, a backend or a project lease — the same trick internal/host uses
// for its own App seam.
type Runtime interface {
	// SessionID is the assistant's conversation id, and names its debug-log file.
	SessionID() string
	// Facts describes the binding this runtime resolved: project, tier, endpoint,
	// whether MCP came up. Read once at open.
	Facts() RuntimeFacts
	// Send runs ONE turn to completion, emitting through sink. It is single-flight in
	// the underlying session, which is why the registry serializes runs per session.
	//
	// The sink is per-TURN, not per-runtime, because each turn records into its own
	// Run. Passing it here rather than wiring it once at construction is what makes it
	// impossible to build a runtime that silently records nothing.
	Send(ctx context.Context, prompt string, sink agent.EventSink) (string, error)
	// InjectPrompt folds a message into the RUNNING turn at its next tool boundary.
	InjectPrompt(text string)
	// DiscardPendingInjections drops messages that were buffered but never folded in.
	// Without it an injection that missed its window survives to be folded into an
	// unrelated LATER turn, arriving after that turn's own prompt.
	DiscardPendingInjections()
	// Attention reads the project's attention inbox — completions from async work,
	// watcher findings, timer fires — that accumulated outside a turn. It returns only
	// events not yet delivered to this caller; acknowledge marks the batch delivered so
	// the next call reports only what is NEW.
	Attention(ctx context.Context, acknowledge bool) ([]domain.QueueEvent, error)
	// Close tears the runtime down and releases the project lease.
	Close() error
}

// RuntimeFacts is the immutable description of what a session is bound to.
type RuntimeFacts struct {
	Project      string `json:"project"`
	Tier         string `json:"tier"`
	BackendURL   string `json:"backendUrl"`
	LogPath      string `json:"logPath"`
	AutoApprove  bool   `json:"autoApprove"`
	MCPConnected bool   `json:"mcpConnected"`
	MCPTransport string `json:"mcpTransport,omitempty"`
}

// OpenParams is what session.open resolves into a runtime. Every field is optional and
// falls back to the process environment — the server itself holds no binding, so that a
// client which cannot restart this process can still repoint it.
type OpenParams struct {
	Project     string
	BackendURL  string
	APIKeyFile  string
	Tier        string
	McpURL      string
	McpToken    string
	StateDir    string
	LogDir      string
	AutoApprove *bool
	DebugLog    *bool
}

// RuntimeFactory builds a runtime for one session. cli supplies the real one.
//
// It takes TWO contexts and the distinction is load-bearing. bootstrap bounds the work
// that must finish before session.open answers — acquiring the project lease, opening
// the store, connecting MCP — and is the tool call's own context, so a client that gives
// up stops us waiting. lifetime is the SERVER's context and is what anything long-lived
// must hang off: the scheduler, the async coordinator, background ticks. Using bootstrap
// for those would kill them the instant open returned its response, because the SDK
// cancels a request context once the reply is sent.
type RuntimeFactory func(bootstrap, lifetime context.Context, p OpenParams) (Runtime, error)

// Every error a tool can return names what to do INSTEAD. The caller is a language
// model: an error it cannot act on becomes a retry loop, which is one of the failure
// modes this repo has repeatedly had to engineer out.
var (
	// ErrNoSession is returned for an unknown or already-closed session id.
	ErrNoSession = errors.New("no such session — it was never opened or has been closed; " +
		"call daintree.session.list to see open sessions, or daintree.session.open to start one")
	// ErrNoRun is returned for an unknown run id.
	ErrNoRun = errors.New("no such run in this session — check the runId returned by daintree.ask; " +
		"note that a session keeps only its most recent runs")
	// ErrBusy is returned when a session already has a turn in flight. The assistant's
	// Send is single-flight, and a second concurrent turn would corrupt the
	// conversation — inject is the way to steer a running turn.
	ErrBusy = errors.New("this session already has a turn in flight — do not retry; " +
		"use daintree.poll to watch it, daintree.inject to steer it, or daintree.interrupt to abandon it")
)

// Session is one live assistant conversation held open across MCP tool calls.
type Session struct {
	ID      string
	runtime Runtime
	facts   RuntimeFacts

	mu      sync.Mutex
	current *Run
	runs    map[string]*Run
	// order keeps run ids oldest-first so pruning drops the oldest completed run.
	order  []string
	closed bool
	// turns tracks the turn GOROUTINES, which is not the same as the runs being
	// settled. The recorder settles a run on assistant:end — while Send is still
	// unwinding, persisting the conversation and closing out its round. Waiting on the
	// run would therefore let close() tear the App down underneath a live Send.
	turns sync.WaitGroup
}

// maxRunsPerSession bounds the per-session run history. A long-lived session driven by
// an agent accumulates runs indefinitely otherwise, and every one retains its whole
// event list. Completed runs are dropped oldest-first; a live run is never pruned.
const maxRunsPerSession = 32

// Registry owns every open session for this server process.
type Registry struct {
	factory RuntimeFactory
	// lifetime is the server's context, handed to every factory call and to every turn.
	// Nothing that must outlive a single tool call may use anything else.
	lifetime context.Context

	mu       sync.Mutex
	sessions map[string]*Session
	closed   bool
}

// NewRegistry builds an empty registry over a runtime factory. lifetime is the server's
// context; a nil one falls back to Background so tests need not thread one through.
func NewRegistry(lifetime context.Context, factory RuntimeFactory) *Registry {
	if lifetime == nil {
		lifetime = context.Background()
	}
	return &Registry{factory: factory, lifetime: lifetime, sessions: map[string]*Session{}}
}

// Open builds a runtime and registers a session for it.
func (r *Registry) Open(ctx context.Context, p OpenParams) (*Session, error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, errors.New("server is shutting down")
	}
	r.mu.Unlock()

	rt, err := r.factory(ctx, r.lifetime, p)
	if err != nil {
		return nil, err
	}
	s := &Session{
		ID:      rt.SessionID(),
		runtime: rt,
		facts:   rt.Facts(),
		runs:    map[string]*Run{},
	}

	r.mu.Lock()
	// Losing the shutdown race must not strand a live runtime holding the project
	// lease: close what we just built rather than registering it into a dead registry.
	if r.closed {
		r.mu.Unlock()
		_ = rt.Close()
		return nil, errors.New("server is shutting down")
	}
	// A duplicate id would make the EXISTING runtime unreachable while it still held
	// the project lease — the map overwrite loses the only reference, so neither Close
	// nor CloseAll would ever release it. Ids are random, so this is rare; a leaked
	// exclusive lease is permanent, so it is worth a check.
	if _, clash := r.sessions[s.ID]; clash {
		r.mu.Unlock()
		_ = rt.Close()
		return nil, fmt.Errorf("session id %q is already open", s.ID)
	}
	r.sessions[s.ID] = s
	r.mu.Unlock()
	return s, nil
}

// Get resolves a session id.
func (r *Registry) Get(id string) (*Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[id]
	if !ok {
		return nil, fmt.Errorf("%w (id %q)", ErrNoSession, id)
	}
	return s, nil
}

// List returns every open session, for a client that lost track of its ids.
func (r *Registry) List() []*Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Session, 0, len(r.sessions))
	for _, s := range r.sessions {
		out = append(out, s)
	}
	return out
}

// Close closes one session: cancel any live turn, tear the runtime down, release the
// lease. Idempotent.
func (r *Registry) Close(id string) error {
	r.mu.Lock()
	s, ok := r.sessions[id]
	delete(r.sessions, id)
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: %q", ErrNoSession, id)
	}
	return s.close()
}

// CloseAll tears down every session. Called on server shutdown so no process exits
// still holding a project lease.
func (r *Registry) CloseAll() {
	r.mu.Lock()
	r.closed = true
	all := make([]*Session, 0, len(r.sessions))
	for id, s := range r.sessions {
		all = append(all, s)
		delete(r.sessions, id)
	}
	r.mu.Unlock()
	for _, s := range all {
		_ = s.close()
	}
}

func (s *Session) close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	current := s.current
	s.mu.Unlock()

	// Cancel the live turn FIRST, then wait for its GOROUTINE — not merely for the run
	// to settle. Tearing the runtime down under a live Send would close the store and
	// the MCP client out from under it.
	if current != nil {
		current.Cancel()
	}
	s.turns.Wait()
	return s.runtime.Close()
}

// Facts describes what this session is bound to.
func (s *Session) Facts() RuntimeFacts { return s.facts }

// Busy reports whether a turn is in flight.
func (s *Session) Busy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current != nil
}

// Run resolves a run id within this session.
func (s *Session) Run(id string) (*Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	run, ok := s.runs[id]
	if !ok {
		return nil, fmt.Errorf("%w (id %q)", ErrNoRun, id)
	}
	return run, nil
}

// Runs returns this session's runs, oldest first.
func (s *Session) Runs() []*Run {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Run, 0, len(s.order))
	for _, id := range s.order {
		if run, ok := s.runs[id]; ok {
			out = append(out, run)
		}
	}
	return out
}

// Ask starts a turn and returns its Run immediately. The caller decides whether to wait
// on Run.Done() (block mode) or return the handle for later polling (async mode) —
// which is the whole point: an orchestration turn runs for minutes and no MCP client
// will hold a request open that long.
//
// parent is the SERVER's lifetime context, not the tool call's: a tool call's context
// dies when its response is sent, which for an async ask is immediately. Binding the
// turn to it would cancel every run the instant it was accepted.
func (s *Session) Ask(parent context.Context, prompt string) (*Run, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, fmt.Errorf("%w (id %q)", ErrNoSession, s.ID)
	}
	if s.current != nil {
		s.mu.Unlock()
		return nil, ErrBusy
	}
	ctx, cancel := context.WithCancel(parent)
	run := NewRun(domain.NewID("mrun_"), s.ID, prompt, cancel)
	s.current = run
	s.runs[run.ID] = run
	s.order = append(s.order, run.ID)
	s.pruneLocked()
	s.turns.Add(1)
	s.mu.Unlock()

	// Drop anything a previous turn buffered but never folded in. InjectPrompt only
	// BUFFERS — a turn that was interrupted past its final fold check leaves the message
	// sitting there, and without this it would surface inside THIS turn, after this
	// prompt, as an instruction nobody issued for this work.
	s.runtime.DiscardPendingInjections()

	go func() {
		defer s.turns.Done()
		defer cancel()
		defer func() {
			// A panic in the turn must not take down the MCP server: it would drop the
			// pipe and every other session with it. Record it and settle the run.
			if p := recover(); p != nil {
				run.append(Event{Type: "error", Text: fmt.Sprintf("assistant panicked: %v", p)})
				run.settle(RunFailed, "", fmt.Sprintf("assistant panicked: %v", p))
			}
			s.mu.Lock()
			if s.current == run {
				s.current = nil
			}
			s.mu.Unlock()
		}()
		reply, err := s.runtime.Send(ctx, prompt, NewRecorder(run))
		// settle is first-wins, so in the normal case the recorder has already
		// classified this from the terminal event. These are the backstops, and their
		// ORDER is the point: a cancelled context is a cancellation however the runtime
		// chose to report it. Testing err first would relabel every interrupt as a
		// failure, because a Send that honours cancellation returns context.Canceled.
		switch {
		case ctx.Err() != nil:
			run.settle(RunCancelled, reply, "")
		case err != nil:
			run.append(Event{Type: "error", Text: err.Error()})
			run.settle(RunFailed, reply, err.Error())
		default:
			// Normally the recorder already settled this on assistant:end. This is the
			// backstop for a turn that returns without a terminal event.
			run.settle(RunSucceeded, reply, "")
		}
	}()
	return run, nil
}

// pruneLocked drops completed runs beyond the cap, oldest first. Callers hold s.mu.
func (s *Session) pruneLocked() {
	for len(s.order) > maxRunsPerSession {
		oldest := s.order[0]
		run, ok := s.runs[oldest]
		// Never prune a live run — its handle is outstanding with the caller.
		if ok && run.Status() == RunRunning {
			return
		}
		s.order = s.order[1:]
		delete(s.runs, oldest)
	}
}

// Inject folds a message into the running turn. Returns false when nothing is running,
// so the caller can be told to use ask instead of silently losing the message.
func (s *Session) Inject(text string) bool {
	s.mu.Lock()
	current := s.current
	s.mu.Unlock()
	if current == nil {
		return false
	}
	s.runtime.InjectPrompt(text)
	return true
}

// Interrupt cancels the running turn. Returns false when nothing is running.
func (s *Session) Interrupt() bool {
	s.mu.Lock()
	current := s.current
	s.mu.Unlock()
	if current == nil {
		return false
	}
	current.Cancel()
	// Discard here as well as at the next Ask: an interrupt is the likeliest way for an
	// injection to miss its fold window, and leaving it buffered means the next turn
	// silently inherits an instruction meant for the abandoned one.
	s.runtime.DiscardPendingInjections()
	return true
}

// Attention reads the project's attention inbox.
func (s *Session) Attention(ctx context.Context, acknowledge bool) ([]domain.QueueEvent, error) {
	return s.runtime.Attention(ctx, acknowledge)
}

// appRuntime adapts the concrete *app.App onto the Runtime seam. It lives here rather
// than in cli so the adaptation is testable alongside the registry, but it is
// constructed by cli, which owns the lease and the App wiring.
type appRuntime struct {
	app     *app.App
	facts   RuntimeFacts
	release func()
	// attentionMu serializes the read-and-acknowledge pair. MCP handlers run
	// concurrently, so two attention calls could otherwise both Digest the same fresh
	// rows before either marked them — and the conditional mark protects an UPDATED row
	// from being lost, not a duplicate already handed to a second caller.
	attentionMu sync.Mutex
}

// NewAppRuntime wraps a constructed App. release hands the project lease back.
func NewAppRuntime(a *app.App, facts RuntimeFacts, release func()) Runtime {
	return &appRuntime{app: a, facts: facts, release: release}
}

func (a *appRuntime) SessionID() string         { return a.app.SessionID }
func (a *appRuntime) Facts() RuntimeFacts       { return a.facts }
func (a *appRuntime) InjectPrompt(t string)     { a.app.Session.InjectPrompt(t) }
func (a *appRuntime) DiscardPendingInjections() { a.app.Session.DiscardPendingInjections() }

// hookInstaller and turnSender are the two capabilities appRuntime.Send needs from the
// App. They exist as interfaces for ONE reason: the bug this code shipped with was
// forgetting to install the sink at all, and a test that drove a fake Runtime could
// never catch it — the fake receives the sink as an argument, so it works whether or not
// the real adapter installs anything. Naming the seam makes the wiring itself testable.
type hookInstaller interface{ SetHooks(app.AppHooks) }

type turnSender interface {
	Send(ctx context.Context, prompt string, opts agent.SendOptions) (string, error)
}

// sendWithSink installs THIS turn's sink and runs the turn.
//
// Installing per turn rather than once at construction is the whole reason the sink is a
// Send parameter: each turn records into its own Run, and a runtime wired with a single
// sink at build time would either mix turns together or — as this code did before —
// record nothing at all, leaving every poll empty and every failed turn reported as a
// success (agent.Session.Send returns turn FAILURES as sentinel replies with a nil
// error, so without the sink's Error event there is nothing to classify).
//
// Re-installing per turn is safe because a session is single-flight: Session.Ask admits
// one turn at a time, so no other turn can be streaming into the old sink.
func sendWithSink(ctx context.Context, hooks hookInstaller, sender turnSender, prompt string, sink agent.EventSink) (string, error) {
	hooks.SetHooks(app.AppHooks{AgentEvents: sink})
	return sender.Send(ctx, prompt, agent.SendOptions{})
}

func (a *appRuntime) Send(ctx context.Context, prompt string, sink agent.EventSink) (string, error) {
	return sendWithSink(ctx, a.app, a.app.Session, prompt, sink)
}

func (a *appRuntime) Attention(ctx context.Context, acknowledge bool) ([]domain.QueueEvent, error) {
	a.attentionMu.Lock()
	defer a.attentionMu.Unlock()
	// NotifiedIsNull: only what has not already been handed to this caller. Without the
	// filter a polling agent re-reads the same completions forever and cannot tell new
	// work from old — the inbox keeps an event until it is RESOLVED, which is a
	// different lifecycle from "seen".
	events, err := a.app.Queue.Digest(ctx, domain.QueueDigestOptions{NotifiedIsNull: true})
	if err != nil {
		return nil, err
	}
	// Acknowledging is version-conditional inside the store, so an event a publisher
	// updated after this read stays un-notified and re-surfaces — an update can never
	// be stamped away undelivered.
	if acknowledge && len(events) > 0 {
		if err := a.app.Queue.MarkNotified(ctx, events); err != nil {
			return events, err
		}
	}
	return events, nil
}

func (a *appRuntime) Close() error {
	err := a.app.Shutdown()
	if a.release != nil {
		a.release()
	}
	return err
}
