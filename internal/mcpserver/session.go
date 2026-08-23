package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

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
	// impossible to build a runtime that silently records nothing. runID travels for
	// the same reason: an approval raised mid-turn has to name the run it is blocking,
	// or every run in the session reports itself blocked by it.
	Send(ctx context.Context, prompt, runID string, sink agent.EventSink) (string, error)
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
	// AcknowledgeAttention marks the named inbox rows delivered, reporting how many
	// matched and which ids did not. Separating it from the read is what makes delivery
	// at-least-once: a response lost in transit costs a duplicate, not the item.
	AcknowledgeAttention(ctx context.Context, ids []string) (acked int, unknown []string, err error)
	// Approvals brokers this runtime's tool confirmations. Never nil.
	Approvals() *Approvals
	// Close tears the runtime down and releases the project lease.
	Close() error
}

// RuntimeFacts is the immutable description of what a session is bound to.
type RuntimeFacts struct {
	Project     string `json:"project"`
	Tier        string `json:"tier"`
	BackendURL  string `json:"backendUrl"`
	LogPath     string `json:"logPath"`
	AutoApprove bool   `json:"autoApprove"`
	// ApprovalMode is how this session answers a mutating tool: decline (skip it and
	// carry on), ask (park it for the caller to decide), or auto (never ask).
	ApprovalMode string `json:"approvalMode"`
	MCPConnected bool   `json:"mcpConnected"`
	MCPTransport string `json:"mcpTransport,omitempty"`
	// PinnedSkills are the ids this session ASKS for on every turn — what was requested,
	// not what the backend ended up loading. Preflight can only prove an id exists in the
	// catalog; whether it is executable under this profile, and whether it survives the
	// active-skill cap, is decided per turn and reported through the pin warnings.
	// Reported at all because a caller that inherited a server-level default did not
	// choose these and would otherwise have no way to see them.
	PinnedSkills []string `json:"pinnedSkills,omitempty"`
	// PinPreflightWarning is the non-fatal pin advisory. NOT part of the facts wire
	// shape — describe() folds it into SessionOutput.Warnings, which is the field a
	// caller already reads for "conditions that will silently ruin a run".
	PinPreflightWarning string `json:"-"`
}

// OpenParams is what session.open resolves into a runtime. Every field is optional and
// falls back to the process environment — the server itself holds no binding, so that a
// client which cannot restart this process can still repoint it.
type OpenParams struct {
	Project    string
	BackendURL string
	APIKeyFile string
	Tier       string
	McpURL     string
	// McpTokenFile is a PATH. The bearer itself never crosses this boundary as a value
	// — see OpenInput.McpTokenFile for why.
	McpTokenFile string
	StateDir     string
	LogDir       string
	ProjectID    string
	WindowID     string
	DebugLog     *bool
	// Approvals selects the confirmation mode. Empty INHERITS the process default —
	// auto when --auto-approve/DAINTREE_ASSISTANT_AUTO_APPROVE is set, else decline —
	// which is why the policy judges the RESOLVED mode rather than this field.
	Approvals ApprovalMode
	// ApprovalTimeout bounds a parked approval; zero uses DefaultApprovalTimeout.
	ApprovalTimeout time.Duration
	// Skills pins backend runbooks for every turn of this session. nil inherits the
	// process-level --skill defaults; a non-nil empty slice explicitly clears them.
	Skills []string
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
	// ErrNoActiveRun is returned when a steering or cancelling call arrives with no
	// turn in flight. It is not a failure of the session — it means the caller's model
	// of what is running has gone stale.
	ErrNoActiveRun = errors.New("no turn is running in this session — daintree.ask starts one; " +
		"call daintree.poll on the last runId if you expected it to still be going")
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

// RunMismatchError is returned when a run-correlated call names a run that is no longer
// the live one. It carries BOTH ids because the only useful recovery is to look at what
// is actually running: over a slow pipe an inject or interrupt written for one turn can
// easily arrive after that turn ended and another began, and applying it anyway would
// steer or cancel work the caller never meant to touch.
type RunMismatchError struct {
	Want    string
	Current string
}

func (e *RunMismatchError) Error() string {
	return fmt.Sprintf(
		"run %q is no longer the active turn (the session is running %q) — poll %s for its outcome, "+
			"and re-issue this against the live run if you still mean it",
		e.Want, e.Current, e.Want)
}

// Registry owns every open session for this server process.
type Registry struct {
	factory RuntimeFactory
	// lifetime is the server's context, handed to every factory call and to every turn.
	// Nothing that must outlive a single tool call may use anything else.
	lifetime context.Context

	// policy is the process-level ceiling, or NIL when none was installed.
	//
	// The nil-ness is load-bearing. Installing a policy is opting into a ceiling, and
	// within one the defaults deny (auto-approve most of all). But the same registry
	// backs the trusted embedding paths, where the operator IS the caller and a ceiling
	// that switched itself on would break them — so "no policy" and "a policy whose
	// fields are all zero" must not mean the same thing. See policy.go.
	policy *ServerPolicy

	mu       sync.Mutex
	sessions map[string]*Session
	closed   bool
}

// SetPolicy installs the process policy. Call it once, at launch, before any session is
// opened; there is deliberately no tool that reaches it — a ceiling a session argument
// could raise is not a ceiling.
//
// The policy is CANONICALIZED on the way in: its roots are resolved once, here, rather
// than on every check, so a symlink retargeted while the server runs cannot move the
// ceiling afterwards.
func (r *Registry) SetPolicy(p ServerPolicy) {
	pinned := p.Canonicalize()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.policy = &pinned
}

// NewRegistry builds a registry over a runtime factory, under a process policy. lifetime
// is the server's context; a nil one falls back to Background so tests need not thread
// one through.
//
// The policy is a required ARGUMENT rather than an optional second step: forgetting
// SetPolicy used to produce a fully unconfined registry, which made the dangerous
// configuration the one you got by omission. A registry that genuinely wants no ceiling
// says so with NewUnconfinedRegistry.
func NewRegistry(lifetime context.Context, factory RuntimeFactory, policy ServerPolicy) *Registry {
	r := NewUnconfinedRegistry(lifetime, factory)
	r.SetPolicy(policy)
	return r
}

// NewUnconfinedRegistry builds a registry with NO process ceiling. Only ever right for a
// trusted embedding path where the operator IS the caller — and for tests that are
// exercising something other than the policy. On a model-facing surface this is the one
// configuration that must not happen, which is why it has its own name.
func NewUnconfinedRegistry(lifetime context.Context, factory RuntimeFactory) *Registry {
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
	// Checked BEFORE the factory runs. A policy violation must not have already
	// acquired a project lease, opened a database, or connected MCP by the time it is
	// refused — the refusal would then be the only thing that was not a side effect.
	//
	// This early check is an OPTIMISATION, not the enforcement point: it is read
	// outside the lock the registration takes, so the session count it sees can be
	// stale. The authoritative re-check happens below, under the lock that inserts.
	policy, open := r.policy, len(r.sessions)
	r.mu.Unlock()
	if policy != nil {
		if err := policy.Check(p, open); err != nil {
			return nil, err
		}
	}

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
	// A run parked on an approval produces no further events of its own, so without
	// this a long poll would sit through its entire budget while the turn was STOPPED
	// rather than merely slow. Installed once, here, so it cannot race the per-ask
	// rebinding of the elicitation hook.
	rt.Approvals().SetOnChange(func(runID string) {
		if runID == "" {
			return
		}
		s.mu.Lock()
		run := s.runs[runID]
		s.mu.Unlock()
		if run != nil {
			run.Touch()
		}
	})

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
	// THE session cap is enforced here, not by the early check above. Building the
	// runtime is slow (lease, database, MCP connect) and must not hold this lock, so
	// two concurrent opens can both pass a count read before either had registered —
	// admitting two sessions under a cap of one. Re-checking under the insert lock is
	// what actually makes the cap hold; the runtime we just built is torn down rather
	// than leaked, exactly as the clash path above does.
	if policy != nil && policy.MaxSessions > 0 && len(r.sessions) >= policy.MaxSessions {
		r.mu.Unlock()
		_ = rt.Close()
		return nil, &PolicyError{Field: "session.open", Reason: fmt.Sprintf(
			"this server allows %d concurrent session(s) and %d are open; close one first",
			policy.MaxSessions, len(r.sessions))}
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
// List returns the open sessions in a STABLE order — by session id, since ids are
// generated per open and a caller diffing two listings must not see phantom churn from
// Go's randomized map iteration.
func (r *Registry) List() []*Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Session, 0, len(r.sessions))
	for _, s := range r.sessions {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
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
	// Reject before waiting, not after: turns.Wait() would otherwise block on a
	// dispatch parked on an approval nobody is left to answer.
	s.runtime.Approvals().RejectAll()
	s.turns.Wait()
	return s.runtime.Close()
}

// Facts describes what this session is bound to.
func (s *Session) Facts() RuntimeFacts { return s.facts }

// Approvals is this session's confirmation broker.
func (s *Session) Approvals() *Approvals { return s.runtime.Approvals() }

// ApprovalTimeout is how long this session parks an unanswered approval.
func (s *Session) ApprovalTimeout() time.Duration { return s.runtime.Approvals().Timeout() }

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
	// Drop anything a previous turn buffered but never folded in, BEFORE the new run
	// becomes visible. InjectPrompt only BUFFERS — a turn interrupted past its final
	// fold check leaves the message sitting there, and without this it would surface
	// inside THIS turn, after this prompt, as an instruction nobody issued for this
	// work. The ORDER matters as much as the discard: done after publishing s.current,
	// a concurrent inject could observe the new run, be told it succeeded, and then be
	// thrown away as if it had belonged to the previous turn. Inject holds s.mu across
	// its own buffer write for the same reason, so the two cannot interleave.
	s.runtime.DiscardPendingInjections()
	s.current = run
	s.runs[run.ID] = run
	s.order = append(s.order, run.ID)
	s.pruneLocked()
	s.turns.Add(1)
	s.mu.Unlock()

	rec := NewRecorder(run)
	go func() {
		defer s.turns.Done()
		defer cancel()

		// FINALIZATION IS OWNED HERE, not by the sink. The recorder can see that the
		// agent emitted a terminal-looking event, but only this goroutine knows whether
		// Send returned cleanly, whether cancellation won, whether a later error
		// arrived, and whether the post-response bookkeeping is done. Settling from the
		// sink left a window where poll reported `success` while Busy() was still true,
		// so the caller's next ask raced ErrBusy against a run it had just been told
		// had finished — and the recorded duration excluded the unwind.
		var (
			status  RunStatus
			content string
			errMsg  string
		)
		func() {
			defer func() {
				// A panic in the turn must not take down the MCP server: it would drop
				// the pipe and every other session with it. Record it as the outcome.
				if p := recover(); p != nil {
					msg := fmt.Sprintf("assistant panicked: %v", p)
					run.append(Event{Type: "error", Text: msg})
					status, content, errMsg = RunFailed, "", msg
				}
			}()
			reply, err := s.runtime.Send(ctx, prompt, run.ID, rec)
			// The ORDER of these cases is the point: a cancelled context is a
			// cancellation however the runtime chose to report it. Testing err first
			// would relabel every interrupt as a failure, because a Send that honours
			// cancellation returns context.Canceled.
			switch {
			case ctx.Err() != nil:
				status, content, errMsg = RunCancelled, reply, ""
			case err != nil:
				run.append(Event{Type: "error", Text: err.Error()})
				status, content, errMsg = RunFailed, reply, err.Error()
			default:
				// Prefer what the STREAM said over the bare return: a turn that failed
				// reports its failure as an `error` event and still returns a sentinel
				// reply with no error, so the return value alone would read as success.
				if st, c, e, ok := rec.Candidate(); ok {
					status, content, errMsg = st, c, e
					if content == "" {
						content = reply
					}
				} else {
					// Backstop for a turn that returns without any terminal event.
					status, content, errMsg = RunSucceeded, reply, ""
				}
			}
		}()

		// Clear `current` BEFORE settling. A caller woken by Done() must find the
		// session idle: the whole reason finalization moved here is that "this run
		// finished" and "this session can take another ask" must become true in that
		// order, never the reverse.
		s.mu.Lock()
		if s.current == run {
			s.current = nil
		}
		s.mu.Unlock()

		run.settle(status, content, errMsg)
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

// Inject folds a message into the running turn. expectRunID, when non-empty, is the run
// the CALLER meant: a steering message written for one turn must never land in whatever
// turn happens to be current by the time the request arrives, which over a slow pipe is
// a different turn entirely. A mismatch returns ErrRunMismatch naming the live run
// rather than silently steering the wrong work.
//
// The buffer write happens UNDER s.mu, so it cannot interleave with the discard Ask
// performs while installing a new run — otherwise this could report success for a
// message the very next turn throws away.
func (s *Session) Inject(expectRunID, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.current
	if current == nil {
		return ErrNoActiveRun
	}
	if expectRunID != "" && expectRunID != current.ID {
		return &RunMismatchError{Want: expectRunID, Current: current.ID}
	}
	s.runtime.InjectPrompt(text)
	return nil
}

// Interrupt cancels the running turn. expectRunID, when non-empty, must name it — a
// cancel aimed at a turn that has already finished must not take down its successor.
func (s *Session) Interrupt(expectRunID string) error {
	s.mu.Lock()
	current := s.current
	s.mu.Unlock()
	if current == nil {
		return ErrNoActiveRun
	}
	if expectRunID != "" && expectRunID != current.ID {
		return &RunMismatchError{Want: expectRunID, Current: current.ID}
	}
	current.Cancel()
	// Unpark this RUN's confirmations. Cancelling the context resolves them anyway, but
	// doing it here means the pending list is empty the moment interrupt returns rather
	// than whenever each dispatch goroutine notices. Scoped to the run, not the session:
	// the captured run can finish and a new one start before this lands, and cancelling
	// the new turn's approvals would abort work nobody asked to stop.
	s.runtime.Approvals().RejectRun(current.ID)
	// Discard here as well as at the next Ask: an interrupt is the likeliest way for an
	// injection to miss its fold window, and leaving it buffered means the next turn
	// silently inherits an instruction meant for the abandoned one.
	//
	// But ONLY if the run we just cancelled is still the current one. The lock was
	// released above so the cancel could not deadlock, and in that window the captured
	// run can finish and a successor start — which a caller may already have injected
	// into and been told succeeded. An unscoped discard would then silently delete a
	// message belonging to a turn this interrupt was never aimed at. RejectRun above is
	// run-scoped for exactly this reason; the discard has to be too.
	s.mu.Lock()
	stillCurrent := s.current == current
	s.mu.Unlock()
	if stillCurrent {
		s.runtime.DiscardPendingInjections()
	}
	return nil
}

// Attention reads the project's attention inbox.
func (s *Session) Attention(ctx context.Context, acknowledge bool) ([]domain.QueueEvent, error) {
	return s.runtime.Attention(ctx, acknowledge)
}

// AcknowledgeAttention consumes the named inbox rows.
func (s *Session) AcknowledgeAttention(ctx context.Context, ids []string) (int, []string, error) {
	return s.runtime.AcknowledgeAttention(ctx, ids)
}

// appRuntime adapts the concrete *app.App onto the Runtime seam. It lives here rather
// than in cli so the adaptation is testable alongside the registry, but it is
// constructed by cli, which owns the lease and the App wiring.
type appRuntime struct {
	app       *app.App
	facts     RuntimeFacts
	release   func()
	approvals *Approvals
	// runMu guards currentRunID, which the confirm hook reads from the DISPATCH
	// goroutine while Send sets it from the turn goroutine.
	runMu        sync.Mutex
	currentRunID string
	// attentionMu serializes the read-and-acknowledge pair. MCP handlers run
	// concurrently, so two attention calls could otherwise both Digest the same fresh
	// rows before either marked them — and the conditional mark protects an UPDATED row
	// from being lost, not a duplicate already handed to a second caller.
	attentionMu sync.Mutex
}

// NewAppRuntime wraps a constructed App. release hands the project lease back.
func NewAppRuntime(a *app.App, facts RuntimeFacts, approvals *Approvals, release func()) Runtime {
	if approvals == nil {
		approvals = NewApprovals(ApprovalDecline, 0)
	}
	return &appRuntime{app: a, facts: facts, approvals: approvals, release: release}
}

func (a *appRuntime) Approvals() *Approvals     { return a.approvals }
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

func (a *appRuntime) Send(ctx context.Context, prompt, runID string, sink agent.EventSink) (string, error) {
	a.runMu.Lock()
	a.currentRunID = runID
	a.runMu.Unlock()
	defer func() {
		a.runMu.Lock()
		if a.currentRunID == runID {
			a.currentRunID = ""
		}
		a.runMu.Unlock()
	}()
	return sendWithSink(ctx, a.app, a.app.Session, prompt, sink)
}

// CurrentRunID is the run a confirmation raised right now belongs to ("" between turns).
func (a *appRuntime) CurrentRunID() string {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	return a.currentRunID
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

// AcknowledgeAttention marks exactly the named rows delivered.
//
// It re-digests rather than acknowledging by bare id because MarkNotified is
// VERSION-conditional: it compares the row's count and coalesce key against the event it
// was handed, so a row a publisher updated between the caller's read and its ack stays
// un-notified and re-surfaces. Acking by id alone would stamp away an update nobody had
// seen — which is the same at-most-once loss that splitting read from ack exists to
// prevent, just moved one call later.
func (a *appRuntime) AcknowledgeAttention(ctx context.Context, ids []string) (int, []string, error) {
	a.attentionMu.Lock()
	defer a.attentionMu.Unlock()
	wanted := make(map[string]bool, len(ids))
	for _, id := range ids {
		wanted[id] = true
	}
	events, err := a.app.Queue.Digest(ctx, domain.QueueDigestOptions{NotifiedIsNull: true})
	if err != nil {
		return 0, nil, err
	}
	matched := make([]domain.QueueEvent, 0, len(ids))
	for _, e := range events {
		if wanted[e.ID] {
			matched = append(matched, e)
			delete(wanted, e.ID)
		}
	}
	// Whatever is left never matched an un-notified row: already acknowledged, moved on
	// by a publisher, or never real. Reported, not errored — a retry after an ambiguous
	// transport failure lands here by design and must stay idempotent.
	unknown := make([]string, 0, len(wanted))
	for _, id := range ids {
		if wanted[id] {
			unknown = append(unknown, id)
			delete(wanted, id)
		}
	}
	if len(matched) == 0 {
		return 0, unknown, nil
	}
	if err := a.app.Queue.MarkNotified(ctx, matched); err != nil {
		return 0, unknown, err
	}
	return len(matched), unknown, nil
}

func (a *appRuntime) Close() error {
	err := a.app.Shutdown()
	if a.release != nil {
		a.release()
	}
	return err
}
