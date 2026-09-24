package app

import (
	"context"
	"sync"
	"time"

	"github.com/daintreehq/assistant/internal/backend"
)

// scheduledCheckinsNegotiationTimeout bounds the one capability GET the scheduled
// check-in gate may issue. Short: the answer only decides whether an optional turn
// block rides later rounds, and a wedged endpoint must not hold a goroutine for long.
const scheduledCheckinsNegotiationTimeout = 5 * time.Second

// scheduledCheckinsRetryAfter is how long a FAILED negotiation for an endpoint is left
// alone before the gate asks again. A backend that is down or mid-deploy would
// otherwise be asked on every round of every turn while a check-in loop is live.
const scheduledCheckinsRetryAfter = time.Minute

// scheduledCheckinsNegotiation is the gate's private state: the answer it negotiated
// itself (pinned to the endpoint that gave it, like backendCaps) and the in-flight /
// backoff bookkeeping for the next ask.
type scheduledCheckinsNegotiation struct {
	mu          sync.Mutex
	snap        *backendCapsSnapshot
	inFlight    bool
	lastFailURL string
	lastFailAt  time.Time
}

// backendAcceptsScheduledCheckins reports whether it is SAFE to attach the
// `turn.scheduled_checkins` block for the endpoint about to be called.
//
// The same endpoint-pinned, fail-closed gate as backendAcceptsDisplayContext, for the
// same reason: TurnContext is validated with extra="forbid", so sending the block to a
// deployment that predates it 422s the WHOLE turn. It differs in one respect, and
// deliberately. Those gates only read what an explicit handshake left behind, which is
// why they stay shut on the embedded host and the supervisor daemon — exactly the two
// surfaces a check-in loop runs on (the host starts it, the daemon keeps it ticking
// after the window closes). A block that could never reach them would make the loop
// visible nowhere it matters. So this gate NEGOTIATES, lazily:
//
//   - It is consulted only when the session actually holds a scheduled message timer
//     (Session.scheduledCheckinsForTurn reads the rows first). An ordinary launch never
//     calls it, so the cost contract TestPreparePinnedRunbooksMakesNoCallWithoutPins
//     pins — no round trip nobody asked for — holds.
//   - With no answer for the live endpoint it starts ONE detached capability GET and
//     returns false. The turn is never blocked on the network; the block simply starts
//     riding from a later round, once the answer is in.
//   - The answer goes into this gate's OWN slot, never the shared backendCaps cache:
//     publishing there would quietly open the display-context, pinned-runbook and
//     compaction gates on surfaces that never negotiated them, which is a decision
//     about other features this one has no business making (826a5d0 removed a boot
//     warm-up for exactly that). It does READ the shared cache, because an explicit
//     handshake for the same endpoint is the same question already answered.
//   - A failed ask is retried no sooner than scheduledCheckinsRetryAfter per endpoint.
//     A /backend switch changes the live URL, so the stale answer stops being believed
//     and the next consult asks the new endpoint.
func (a *App) backendAcceptsScheduledCheckins() bool {
	if a.Backend == nil {
		return false
	}
	live := a.Backend.BaseURL()
	if snap := a.backendCaps.Load(); snap != nil && snap.baseURL == live {
		if snap.caps.Respond.ScheduledCheckins {
			return true
		}
	}
	n := &a.checkins
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.snap != nil && n.snap.baseURL == live {
		return n.snap.caps.Respond.ScheduledCheckins
	}
	if n.inFlight {
		return false
	}
	if n.lastFailURL == live && time.Since(n.lastFailAt) < scheduledCheckinsRetryAfter {
		return false
	}
	n.inFlight = true
	go a.negotiateScheduledCheckins()
	return false
}

// negotiateScheduledCheckins fetches the capability descriptor for the gate above.
// Detached and parented on baseCtx, so Shutdown cancels it; it touches only the backend
// client and this gate's own state, never the store, so it needs no join.
//
// The delegate is captured ONCE, as BackendCapabilities does, so the answer is filed
// under the endpoint that actually gave it even if /backend swaps the client mid-fetch.
func (a *App) negotiateScheduledCheckins() {
	n := &a.checkins
	client := a.Backend
	if sw, ok := client.(*backend.Swappable); ok {
		client = sw.Current()
	}
	asked := client.BaseURL()
	parent := a.baseCtx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, scheduledCheckinsNegotiationTimeout)
	caps, err := client.Capabilities(ctx)
	cancel()

	n.mu.Lock()
	defer n.mu.Unlock()
	n.inFlight = false
	if err != nil {
		n.lastFailURL = asked
		n.lastFailAt = time.Now()
		return
	}
	n.snap = &backendCapsSnapshot{baseURL: asked, caps: caps}
}
