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
	//
	// limit bounds the page (0 = unbounded), and it lives HERE rather than in the caller
	// for two reasons. The inbox is durable and can hold a night's worth of rows, so
	// paging after the fetch still materializes all of them. And acknowledgement is
	// version-conditional on the exact rows READ — so a caller that fetched everything
	// and then acknowledged a page would either mark rows it never delivered, or have to
	// re-read them and acknowledge a NEWER version than the one it showed, silently
	// consuming an update nobody saw.
	//
	// It reports `more` rather than a count: knowing whether another page exists costs
	// one extra row, and knowing exactly how many costs a second query over the whole
	// table.
	Attention(ctx context.Context, limit int, acknowledge bool) (events []domain.QueueEvent, more bool, err error)
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

// BusyError is ErrBusy with the live run named.
//
// The bare sentinel told a caller that recovered from a lost ask response exactly what it
// already knew — something is running — and left it with no handle. Carrying the id makes
// the refusal actionable in the one case where the caller genuinely does not have it.
type BusyError struct {
	CurrentRunID string
}

func (e *BusyError) Error() string {
	if e.CurrentRunID == "" {
		return ErrBusy.Error()
	}
	return fmt.Sprintf("this session already has a turn in flight (run %s) — do not retry; "+
		"call daintree.poll with runId %s to watch it, daintree.inject to steer it, or daintree.interrupt to abandon it",
		e.CurrentRunID, e.CurrentRunID)
}

// Is makes errors.Is(err, ErrBusy) work on a BusyError, so existing callers that test the
// sentinel keep working while new ones can pull the id out with errors.As.
func (e *BusyError) Is(target error) bool { return target == ErrBusy }

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
	// closeStarted and closeErr make a session's teardown observable. A close that hangs
	// or fails is exactly the case a caller needs to SEE — the runtime may still hold
	// the project lease — and "gone from the list" is the one report that cannot
	// distinguish it from a clean close.
	closeStarted int64
	closeErr     error
	// closeDone closes when teardown finishes, so a second caller — a concurrent
	// session.close, or CloseAll at shutdown — can WAIT on the close already running
	// instead of either starting a second one over the same runtime or returning a
	// stale success while the first is still unwinding.
	closeDone chan struct{}
	// closeSettled closes after the REGISTRY has finished its bookkeeping for this
	// teardown — removed from `closing`, and reinserted as close-failed if it failed.
	// It is separate from closeDone because the two become true at different moments,
	// and a caller told "closed" before the bookkeeping lands would see the session in
	// whichever map it happened to catch. Owned by Registry.Close.
	closeSettled chan struct{}
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

// ErrEventStreamIncomplete is the outcome of a turn that returned cleanly but emitted no
// terminal event.
//
// It is deliberately a FAILURE. The runtime's contract is that a turn ends with a
// terminal event; a return with none is a broken event stream, and the state that
// produces it — a sink that was never wired — is one this package has already shipped
// once. Reporting it as an empty success is how that bug stayed invisible: the caller was
// told the run completed, so nothing looked wrong except that nothing had happened.
// errRunDeadline is the cause attached to a run's own timeout, so an expiry can be told
// apart from the server lifetime expiring around it.
var errRunDeadline = errors.New("run deadline exceeded")

var ErrEventStreamIncomplete = errors.New("RUN_EVENT_STREAM_INCOMPLETE: the turn returned without " +
	"emitting a terminal event, so this run has no trustworthy outcome. Any content below is diagnostic, " +
	"not an answer — treat the work as not done and check the session's debug log")

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
	// opening counts opens that have passed the cap check but not yet registered. The
	// cap is about RESOURCES, and the expensive part — the project lease, the database,
	// the MCP connection — all happens before registration. Counting only the map let
	// 100 concurrent opens under MaxSessions:1 every one of them build a full runtime
	// and contend for the same lease, with 99 torn down afterwards. Reserving here caps
	// the work rather than the bookkeeping.
	opening int
	// closing holds sessions whose teardown has started but not finished, so a failed or
	// slow close stays VISIBLE instead of vanishing from session.list while the runtime
	// may still hold the project lease.
	closing map[string]*Session
	closed  bool
}

// SessionState is where a session is in its lifecycle, as reported to a caller.
type SessionState string

const (
	// StateOpen is a session that can take work.
	StateOpen SessionState = "open"
	// StateClosing is a session whose teardown is running. It cannot take work and its
	// lease may still be held.
	StateClosing SessionState = "closing"
	// StateCloseFailed is a session whose teardown returned an error or exceeded its
	// deadline. The lease is believed still held; the process may need restarting.
	StateCloseFailed SessionState = "close-failed"
)

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
	return &Registry{
		factory:  factory,
		lifetime: lifetime,
		sessions: map[string]*Session{},
		closing:  map[string]*Session{},
	}
}

// Open builds a runtime and registers a session for it.
func (r *Registry) Open(ctx context.Context, p OpenParams) (*Session, error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, errors.New("server is shutting down")
	}
	// RESERVE the slot, do not merely count it. The cap is about resources, and every
	// expensive thing an open does — taking the project lease, opening the database,
	// connecting MCP — happens below, before registration. Counting only the registered
	// map meant 100 concurrent opens under MaxSessions:1 all passed, all built a full
	// runtime, all contended for the same lease, and 99 were torn down after the work
	// was already done. Counting in-flight opens caps the WORK.
	//
	// A session that is closing still counts: its runtime may still hold the lease, so
	// admitting a replacement now is admitting two.
	policy, live := r.policy, len(r.sessions)+len(r.closing)+r.opening
	if policy != nil {
		if err := policy.Check(p, live); err != nil {
			r.mu.Unlock()
			return nil, err
		}
	}
	r.opening++
	r.mu.Unlock()

	// Released on EVERY exit from here on, including the panic path — a leaked
	// reservation is a cap that ratchets down until the server admits nothing.
	//
	// `released` lets the registration below give the slot back under the SAME lock hold
	// that inserts the session. Doing it in this defer alone left a window where an
	// opener was counted in BOTH sessions and opening, so a concurrent opener under a
	// cap of 2 saw 1+2-1 == 2 and refused itself even though the two of them exactly
	// filled the cap.
	released := false
	release := func() {
		if !released {
			released = true
			r.opening--
		}
	}
	defer func() {
		r.mu.Lock()
		release()
		r.mu.Unlock()
	}()

	rt, err := r.factory(ctx, r.lifetime, p)
	if err != nil {
		return nil, err
	}
	s := &Session{
		ID:           rt.SessionID(),
		runtime:      rt,
		facts:        rt.Facts(),
		runs:         map[string]*Run{},
		closeDone:    make(chan struct{}),
		closeSettled: make(chan struct{}),
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
	// The cap is re-checked here as well as reserved above. The reservation makes the
	// count correct for concurrent opens; this backstop covers the case the reservation
	// cannot — an open that was admitted while a slot was free and arrives after
	// something else registered under a policy installed in between. The runtime we just
	// built is torn down rather than leaked, exactly as the clash path above does.
	//
	// r.opening excludes THIS open (it is still counted, so subtract one) — otherwise a
	// single open under MaxSessions:1 would refuse itself.
	// Give the reservation back FIRST, so this open is counted once — as a session —
	// rather than simultaneously as a session and as an opening.
	release()
	// The cap is re-checked here as well as reserved above, as a backstop for an
	// interleaving the reservation cannot cover on its own. It uses the policy captured
	// BEFORE the factory ran, deliberately: SetPolicy is a launch-time call, so there is
	// no mid-open policy change to catch, and re-reading would mean an open could be
	// judged by two different ceilings.
	if policy != nil && policy.MaxSessions > 0 {
		if live := len(r.sessions) + len(r.closing) + r.opening; live >= policy.MaxSessions {
			r.mu.Unlock()
			_ = rt.Close()
			return nil, &PolicyError{Field: "session.open", Reason: fmt.Sprintf(
				"this server allows %d concurrent session(s) and %d are open or closing; close one first",
				policy.MaxSessions, live)}
		}
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
	out := make([]*Session, 0, len(r.sessions)+len(r.closing))
	for _, s := range r.sessions {
		out = append(out, s)
	}
	// CLOSING sessions are listed too. They count against MaxSessions — their runtime
	// may still hold the project lease, so admitting a replacement would admit two — and
	// a session that consumes capacity while being invisible is capacity nobody can
	// account for. Under MaxSessions:1 a hung close otherwise refused every new session
	// while session.list reported none, which is the least debuggable shape this can
	// take. Their State() says closing.
	for _, s := range r.closing {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// CloseResult describes what a close call actually did, so a retry after a lost response
// is a report rather than an error.
type CloseResult struct {
	// Acted is true only for the call that performed the teardown. A retry that finds
	// the session already gone reports false and succeeds.
	Acted bool
	// State is where the session ended up: closed, or close-failed.
	State string
	// Message explains a non-acting outcome.
	Message string
}

// Close closes one session: cancel any live turn, tear the runtime down, release the
// lease.
//
// It is IDEMPOTENT in the sense that matters — a retry after a lost response is not an
// error. Returning "no such session" for a close that had already succeeded made the one
// call a caller is told to always make into the one that looks like it failed, so a
// harness either ignored the error (and could not tell a real one) or retried forever.
//
// The session stays visible while it closes. Deleting it up front meant a teardown that
// hung or failed took the session out of session.list while its runtime might still hold
// the project lease — the caller could not retry, could not see it, and had no way to
// learn the lease was stuck.
func (r *Registry) Close(ctx context.Context, id string) (CloseResult, error) {
	r.mu.Lock()
	if s, closing := r.closing[id]; closing {
		r.mu.Unlock()
		// Another call owns the teardown. Wait for it under the CALLER's context rather
		// than starting a second one over the same runtime — the caller asked whether
		// this session is closed, and "someone else is closing it" is not that answer.
		return r.awaitClose(ctx, id, s, false)
	}
	s, ok := r.sessions[id]
	if !ok {
		r.mu.Unlock()
		// Never opened, or already closed and forgotten. Both are the state the caller
		// asked for, so neither is a failure.
		return CloseResult{Acted: false, State: "already-closed",
			Message: "no session with that id is open; it was closed already, or never existed"}, nil
	}
	if err := s.CloseError(); err != nil {
		// A teardown that already FAILED is terminal. Runtime.Close tears down an App —
		// store, MCP client, scheduler, lease — and running it again over a half-closed
		// one is not a retry, it is a second teardown of something that is already
		// partly gone. So say so plainly rather than pretending to have tried again.
		r.mu.Unlock()
		return CloseResult{Acted: false, State: string(StateCloseFailed), Message: err.Error()},
			fmt.Errorf("session %q already failed to close and cannot be torn down again; its project lease is "+
				"believed still held, so restart this MCP server to release it (the OS drops the flock on exit): %w", id, err)
	}
	delete(r.sessions, id)
	r.closing[id] = s
	// Ownership is claimed HERE, synchronously, before the lock is released. Claiming it
	// inside the goroutine left a window in which this call's own wait could win the
	// race, become the owner, and block inside Runtime.Close — which honours no context,
	// so the deadline that was supposed to release the caller never applied.
	owner := s.beginClose()
	r.mu.Unlock()

	// The goroutine is launched EITHER WAY, because this call moved the session into
	// `closing` either way and something has to move it back out.
	//
	// The not-owner case is a shutdown race: CloseAll can claim the teardown between the
	// lookup above and beginClose. Skipping the goroutine there left the session parked
	// in `closing` forever — invisible to Get, listed as closing, and permanently
	// consuming a slot against MaxSessions, which under a cap of one means the server
	// never admits another session again.
	//
	// Teardown itself runs on the SERVER's lifetime, not this tool call's. A close that
	// hangs must not hold the MCP request handler open: the SDK waits for in-flight
	// handlers before Run returns, so a wedged handler stopped the server's own CloseAll
	// from ever running — the process stayed alive holding every project's flock, which
	// is the exact opposite of what closing a session is for.
	go func() {
		// Bookkeeping FIRST, then the signal. A caller woken by closeSettled must find
		// the registry already consistent — told "closed" before the maps were updated,
		// it would see the session in whichever map it happened to catch.
		defer close(s.closeSettled)
		var err error
		if owner {
			err = s.finishClose()
		} else {
			// Someone else owns it. Wait, unbounded, for the outcome: this goroutine has
			// no caller to release, and the bookkeeping below must not run until the
			// teardown it describes has actually finished.
			err = s.closeWait(context.Background())
		}
		r.mu.Lock()
		delete(r.closing, id)
		if err != nil {
			// Keep it visible as close-failed: the lease is believed still held, and a
			// caller that cannot see that has no way to know the project is stuck.
			r.sessions[id] = s
		}
		r.mu.Unlock()
	}()

	return r.awaitClose(ctx, id, s, owner)
}

// closeReportBudget is how long a session.close call waits for a teardown before
// reporting that it is still running. Short relative to a hung close, long enough that
// the ordinary case — which takes milliseconds — still answers "closed".
const closeReportBudget = 10 * time.Second

// awaitClose waits for a teardown and turns it into a report. acted says whether THIS
// call is the one that started it.
func (r *Registry) awaitClose(ctx context.Context, id string, s *Session, acted bool) (CloseResult, error) {
	waitCtx, cancel := context.WithTimeout(ctx, closeReportBudget)
	defer cancel()

	err := s.closeWait(waitCtx)
	if err == nil {
		// The teardown finished; wait for the registry to reflect it, so the state this
		// call reports and the state the next session.list shows cannot disagree.
		select {
		case <-s.closeSettled:
		case <-waitCtx.Done():
		}
	}
	switch {
	case err == nil:
		if acted {
			return CloseResult{Acted: true, State: "closed"}, nil
		}
		return CloseResult{Acted: false, State: "closed",
			Message: "another call was already closing this session; it has now finished"}, nil
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		// Still running. NOT an error: the teardown continues on its own goroutine and
		// the lease will be released when it finishes. Reporting a failure here would
		// send a caller chasing a problem that may not exist.
		return CloseResult{Acted: acted, State: string(StateClosing), Message: "teardown is taking longer than " +
			closeReportBudget.String() + " and is still running; the session stays listed as closing until it " +
			"finishes, and its project lease is released then"}, nil
	default:
		return CloseResult{Acted: acted, State: string(StateCloseFailed), Message: err.Error()},
			fmt.Errorf("session %q did not close cleanly, and its project lease may still be held: %w", id, err)
	}
}

// closeAllBudget bounds server shutdown. A tool that ignores cancellation cannot be
// killed from inside the process, so the honest choice is to stop WAITING for it and let
// the OS release that project's flock on exit, rather than let one wedged session hold
// every other project's lease hostage.
const closeAllBudget = 20 * time.Second

// CloseAll tears down every session. Called on server shutdown so no process exits
// still holding a project lease.
//
// Sessions close CONCURRENTLY and under a shared deadline. Sequentially, one session
// whose turn ignored cancellation blocked every session after it in the loop — so a
// single wedged tool call kept every other project's lease held for as long as the
// process lived. Concurrency means a stuck one costs only itself; the deadline means it
// does not cost the exit either.
func (r *Registry) CloseAll() {
	r.mu.Lock()
	r.closed = true
	all := make([]*Session, 0, len(r.sessions)+len(r.closing))
	for id, s := range r.sessions {
		all = append(all, s)
		delete(r.sessions, id)
	}
	// Sessions already being torn down are waited on too, under the SAME budget. Left
	// out, a close that was in flight when the server stopped got no bound at all — and
	// its lease was exactly the one shutdown exists to release.
	for _, s := range r.closing {
		all = append(all, s)
	}
	r.mu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		for _, s := range all {
			wg.Add(1)
			go func(s *Session) {
				defer wg.Done()
				_ = s.close()
			}(s)
		}
		wg.Wait()
	}()

	select {
	case <-done:
	case <-time.After(closeAllBudget):
		// Deliberately returning with goroutines still running. They are blocked in a
		// turn that is not coming back, and the process is exiting — the OS releases
		// what they hold. Waiting longer trades a bounded exit for an unbounded one.
	}
}

// close tears the session down, or waits for the teardown already running. Safe to call
// concurrently: the first caller owns it and every other one waits.
//
// Waiting rather than returning early is the point. An early return told a second caller
// "closed" while the runtime was still unwinding — so shutdown could move on and the
// process could exit with the store closing underneath a live turn, which is the race
// this whole path exists to avoid.
func (s *Session) close() error {
	if s.beginClose() {
		return s.finishClose()
	}
	return s.closeWait(context.Background())
}

// beginClose claims ownership of the teardown, reporting whether THIS caller got it.
//
// Ownership is claimed separately from running the teardown so a caller can take it,
// hand the work to a goroutine, and then only ever WAIT. Folding the two together meant
// whoever reached the function first became the owner — including a caller that meant to
// wait with a deadline, which then blocked inside Runtime.Close, which honours no
// context at all.
func (s *Session) beginClose() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.closed = true
	s.closeStarted = domain.NowMS()
	return true
}

// closeWait blocks until the teardown finishes, and reports its outcome. It NEVER runs
// the teardown itself.
//
// ctx bounds only the wait: a caller that gives up gets an answer, and the close keeps
// going on the goroutine that owns it.
func (s *Session) closeWait(ctx context.Context) error {
	select {
	case <-s.closeDone:
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.closeErr
	case <-ctx.Done():
		return fmt.Errorf("teardown of session %q is still running: %w", s.ID, ctx.Err())
	}
}

// finishClose runs the teardown. Only the caller that won beginClose may call it.
func (s *Session) finishClose() error {
	s.mu.Lock()
	current := s.current
	s.mu.Unlock()
	defer close(s.closeDone)

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
	err := s.runtime.Close()
	s.mu.Lock()
	s.closeErr = err
	s.mu.Unlock()
	return err
}

// State reports where this session is in its lifecycle.
func (s *Session) State() SessionState {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.closeErr != nil:
		return StateCloseFailed
	case s.closed:
		return StateClosing
	default:
		return StateOpen
	}
}

// CloseStartedAt is when teardown began, or 0. Reported alongside a closing state so a
// caller can tell "just started" from "stuck for ten minutes".
func (s *Session) CloseStartedAt() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeStarted
}

// CloseError is the last teardown failure, or nil.
func (s *Session) CloseError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeErr
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

// LiveState is a session's run-related facts, read under ONE lock hold.
//
// Reading Busy() and CurrentRunID() separately could report busy:true with no current
// run, or the reverse, because the turn can settle between the two calls — a caller
// diffing those two fields would see a state the session was never actually in.
type LiveState struct {
	Busy         bool
	CurrentRunID string
	// Recent is the retained runs, newest first, so a caller that lost an ask response
	// can recover the handle even after the run finished.
	Recent []RunSummary
}

// RunSummary is one retained run, as reported for recovery.
type RunSummary struct {
	RunID     string `json:"runId"`
	Status    string `json:"status"`
	StartedAt int64  `json:"startedAt"`
	EndedAt   int64  `json:"endedAt,omitempty"`
	// Prompt is a bounded echo of what was asked, so a caller can recognize ITS run
	// among several rather than having to poll each one to find out.
	Prompt string `json:"prompt,omitempty"`
}

// maxRecentRunSummaries bounds the recovery list. Enough to find a run a caller lost
// track of; not so many that session.list becomes a transcript.
const maxRecentRunSummaries = 5

// promptEchoBytes bounds the prompt echo in a run summary.
const promptEchoBytes = 160

// Live reports this session's run state as a single consistent snapshot.
//
// It exists for RESPONSE LOSS. If ask starts a turn and its response never arrives, the
// caller is left knowing only that the session is busy, and retrying ask says the same
// unhelpful thing again. CurrentRunID recovers the handle while the turn is live —
// Recent recovers it afterwards, which is the case a fast run lands in and the one a
// caller cannot otherwise get out of, since a retried ask on an idle session is accepted
// and simply does the work twice.
func (s *Session) Live() LiveState {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := LiveState{Busy: s.current != nil}
	if s.current != nil {
		out.CurrentRunID = s.current.ID
	}
	// Newest first: the run a caller is looking for is almost always the last one.
	for i := len(s.order) - 1; i >= 0 && len(out.Recent) < maxRecentRunSummaries; i-- {
		run, ok := s.runs[s.order[i]]
		if !ok {
			continue
		}
		_, _, status, _, _, _, startedAt, endedAt := run.Snapshot(0, 1)
		out.Recent = append(out.Recent, RunSummary{
			RunID:     run.ID,
			Status:    string(status),
			StartedAt: startedAt,
			EndedAt:   endedAt,
			Prompt:    truncateRunes(run.Prompt, promptEchoBytes),
		})
	}
	return out
}

// truncateRunes shortens a string on a RUNE boundary, so a multi-byte character is never
// cut in half into invalid UTF-8 that a JSON encoder then has to replace.
func truncateRunes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// CurrentRunID is the id of the turn in flight, or "" when the session is idle.
func (s *Session) CurrentRunID() string { return s.Live().CurrentRunID }

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
func (s *Session) Ask(parent context.Context, prompt string, deadline time.Duration) (*Run, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, fmt.Errorf("%w (id %q)", ErrNoSession, s.ID)
	}
	if s.current != nil {
		id := s.current.ID
		s.mu.Unlock()
		return nil, &BusyError{CurrentRunID: id}
	}
	// A run with no deadline lives until it completes, the caller interrupts, or the
	// server stops — so a wedged one holds the session (and its project lease) for as
	// long as the process runs, and the caller's only recovery is to notice and
	// interrupt. A bound makes the stuck case self-clearing.
	//
	// It is still COOPERATIVE: cancelling a context only stops code that watches one.
	// A tool that ignores cancellation is bounded by CloseAll's deadline at shutdown,
	// not by this.
	var (
		ctx    context.Context
		cancel context.CancelFunc
	)
	if deadline > 0 {
		// WithTimeoutCause, so an expiry can be attributed. The SERVER lifetime can also
		// carry a deadline, and ctx.Err() reads DeadlineExceeded for either — labelling a
		// server shutdown as "this run exceeded its limit" would send a caller tuning a
		// timeout that had nothing to do with it.
		ctx, cancel = context.WithTimeoutCause(parent, deadline, errRunDeadline)
	} else {
		ctx, cancel = context.WithCancel(parent)
	}
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
				// A deadline that expired is a different fact from a caller that
				// interrupted, and a caller cannot tell them apart from "cancelled"
				// alone — one means "you stopped it", the other "it ran too long".
				if errors.Is(context.Cause(ctx), errRunDeadline) {
					errMsg = fmt.Sprintf("RUN_DEADLINE_EXCEEDED: this run exceeded its %s limit and was cancelled. "+
						"Nothing was rolled back — poll the events to see how far it got, and any background work it "+
						"started stays live and reports through daintree.attention", deadline)
					run.append(Event{Type: "error", Text: errMsg})
				}
				if content == "" {
					// The turn was stopped mid-sentence. Whatever it had streamed is
					// the only account of what it was doing, and dropping it was the
					// gap the recorder's buffer existed to close.
					content = rec.FinalizePartial()
				}
			case err != nil:
				run.append(Event{Type: "error", Text: err.Error()})
				status, content, errMsg = RunFailed, reply, err.Error()
				if content == "" {
					content = rec.FinalizePartial()
				}
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
					// NO TERMINAL EVENT, AND NO ERROR. This is not an empty success —
					// it is exactly the shape a broken event sink produces, which this
					// package has already shipped once: a run that recorded nothing
					// reported itself as a clean completion and the caller believed it.
					//
					// The reply is kept as diagnostic content, because it may be the
					// only thing that says what happened. What the caller must not be
					// told is that the run finished cleanly.
					partial := rec.FinalizePartial()
					content = reply
					if content == "" {
						content = partial
					}
					status, errMsg = RunFailed, ErrEventStreamIncomplete.Error()
					run.append(Event{Type: "error", Text: errMsg})
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
	// A session obtained from Get can start closing before the call reaches here. Acting
	// then reports acted:true for a message folded into a turn that is already being
	// cancelled — and, worse, touches a runtime whose store may be closing underneath.
	if s.closed {
		return fmt.Errorf("%w (id %q is closing)", ErrNoSession, s.ID)
	}
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
//
// Both inbox calls check `closed` first. They reach into the runtime's store, and a
// session handed out by Get can begin closing before the call arrives — reading a store
// that teardown is closing is a use-after-free wearing a database's clothes.
func (s *Session) Attention(ctx context.Context, limit int, acknowledge bool) ([]domain.QueueEvent, bool, error) {
	if err := s.aliveForRuntimeCall(); err != nil {
		return nil, false, err
	}
	return s.runtime.Attention(ctx, limit, acknowledge)
}

// AcknowledgeAttention consumes the named inbox rows.
func (s *Session) AcknowledgeAttention(ctx context.Context, ids []string) (int, []string, error) {
	if err := s.aliveForRuntimeCall(); err != nil {
		return 0, nil, err
	}
	return s.runtime.AcknowledgeAttention(ctx, ids)
}

// aliveForRuntimeCall refuses an operation on a session whose teardown has begun.
//
// It is a check, not a gate: it narrows the window rather than closing it, since close
// can still start between this returning and the runtime call landing. Closing it
// properly needs a per-session operation refcount that teardown drains, which is a
// larger change than this one. What it does remove is the common case — a caller holding
// a session id across a close — and it makes the refusal say what happened instead of
// failing somewhere inside the store.
func (s *Session) aliveForRuntimeCall() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("%w (id %q is closing)", ErrNoSession, s.ID)
	}
	return nil
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

func (a *appRuntime) Attention(ctx context.Context, limit int, acknowledge bool) ([]domain.QueueEvent, bool, error) {
	a.attentionMu.Lock()
	defer a.attentionMu.Unlock()
	// NotifiedIsNull: only what has not already been handed to this caller. Without the
	// filter a polling agent re-reads the same completions forever and cannot tell new
	// work from old — the inbox keeps an event until it is RESOLVED, which is a
	// different lifecycle from "seen".
	//
	// The limit is pushed into the QUERY, and asks for one row more than the page so
	// "is there another page" is answered without a second count over the whole table.
	opts := domain.QueueDigestOptions{NotifiedIsNull: true}
	if limit > 0 {
		plusOne := limit + 1
		opts.MaxItems = &plusOne
	}
	events, err := a.app.Queue.Digest(ctx, opts)
	if err != nil {
		return nil, false, err
	}
	more := false
	if limit > 0 && len(events) > limit {
		more = true
		events = events[:limit]
	}
	// Acknowledging is version-conditional inside the store, so an event a publisher
	// updated after this read stays un-notified and re-surfaces — an update can never
	// be stamped away undelivered.
	//
	// It marks THESE rows, the exact snapshots being returned. That is why the page has
	// to be applied before this point rather than by the caller afterwards: marking a
	// fetch the caller then truncated would stamp rows nobody received, and re-reading
	// to mark a page would acknowledge a newer version than the one that was shown.
	if acknowledge && len(events) > 0 {
		if err := a.app.Queue.MarkNotified(ctx, events); err != nil {
			return events, more, err
		}
	}
	return events, more, nil
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
