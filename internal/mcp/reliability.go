package mcp

// Read-only retry/timeout primitives. These live inside internal/mcp (rather than
// a shared internal/reliability) because the MCP client is their only consumer in
// this phase; if a second subsystem needs them, promote the file to its own
// package then.

import (
	"context"
	"errors"
	"math/rand"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// readRetryPolicy mirrors MCP_READ_RETRY_POLICY. These knobs are for READ-ONLY
// callers only — a mutating tool must never auto-retry (double-apply risk), which
// is enforced by CallOptions.Retries defaulting to 0.
type retryPolicy struct {
	MaxRetries  int           // additional attempts after the first
	BaseDelayMs time.Duration // base backoff
	MaxDelayMs  time.Duration // backoff ceiling
}

var mcpReadRetryPolicy = retryPolicy{
	MaxRetries:  2,
	BaseDelayMs: 250 * time.Millisecond,
	MaxDelayMs:  2000 * time.Millisecond,
}

// mcpReadTimeout is the per-attempt read timeout callers pass as CallOptions.Timeout
// / ListTools timeout for read-only probes (20s in TS).
const mcpReadTimeout = 20000 * time.Millisecond

// doctorProbeTimeout is the caller-side 5s budget on both listTools() and the
// actions.getContext call in the doctor probe.
const doctorProbeTimeout = 5000 * time.Millisecond

// jitterRand is a dedicated non-crypto PRNG. Full jitter intentionally uses a
// cheap PRNG (matches TS Math.random); seeded once so retries don't all collide.
// *rand.Rand is NOT safe for concurrent use, and CallTool retries can run on
// several goroutines at once (UI hook, daemon ticks, doctor) — so every read is
// serialized through jitterMu. (math/rand's top-level funcs are concurrency-safe
// but globally seeded; a private guarded source keeps the dedicated seed.)
var (
	jitterMu   sync.Mutex
	jitterRand = rand.New(rand.NewSource(time.Now().UnixNano()))
)

// randInt63n returns a uniform random int64 in [0, n) under the jitter lock.
func randInt63n(n int64) int64 {
	jitterMu.Lock()
	defer jitterMu.Unlock()
	return jitterRand.Int63n(n)
}

// fullJitterDelay returns a uniform random delay in
// [0, min(maxMs, baseMs * 2^max(0,attempt))]. attempt is 0-based. Full jitter
// (not equal/decorrelated) avoids retry thundering herds. The ceiling is
// inclusive — Intn(ceiling+1).
func fullJitterDelay(attempt int, base, max time.Duration) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	// baseMs * 2^attempt, clamped to maxMs. Compute in ms (int64) to mirror TS.
	baseMs := int64(base / time.Millisecond)
	maxMs := int64(max / time.Millisecond)
	// Shift can overflow for absurd attempt counts; clamp the exponent so the
	// product saturates at maxMs rather than wrapping negative.
	scaled := baseMs
	for i := 0; i < attempt && scaled < maxMs; i++ {
		scaled *= 2
	}
	ceiling := scaled
	if ceiling > maxMs {
		ceiling = maxMs
	}
	if ceiling < 0 {
		ceiling = 0
	}
	// Inclusive ceiling: random integer in [0, ceiling].
	n := randInt63n(ceiling + 1)
	return time.Duration(n) * time.Millisecond
}

// connErrorRe matches transport-level connection failures by message
// (case-insensitive). A JSON-RPC application error (a tool that genuinely failed)
// is NOT matched here — only the transport.
var connErrorRe = regexp.MustCompile(`(?i)fetch failed|ECONNRESET|ETIMEDOUT|ECONNREFUSED|socket hang up|network error|timed out`)

// isRetriableMcpError reports whether err is a transient transport hiccup worth a
// read-only retry. Retriable iff:
//   - a JSON-RPC error with code RequestTimeout (-32001) or ConnectionClosed (-32000); or
//   - the SDK's connection-closed sentinel / a context deadline; or
//   - the message matches a known connection-failure pattern.
//
// IMPORTANT: SESSION_BINDING_GONE / BINDING_STALE are TERMINAL binding errors and
// are deliberately NOT matched — re-issuing a call against a dead Daintree window
// would be wrong. context.Canceled (caller abort) is also NOT retriable.
func isRetriableMcpError(err error) bool {
	if err == nil {
		return false
	}
	// A user-initiated cancel is never a transport problem.
	if errors.Is(err, context.Canceled) {
		return false
	}
	// TERMINAL-FIRST: a binding-gone marker is terminal regardless of how it is
	// wrapped — including a jsonrpc.Error carrying -32001/-32000 whose Message/Data
	// says BINDING_STALE / SESSION_BINDING_GONE. Daintree's binding contract says
	// re-issuing against a dead window is wrong, so this check MUST precede the
	// retriable-code/regex classification below (which would otherwise retry those
	// codes). We inspect the error string, plus the wire Message and Data fields
	// explicitly, since errors.As may render only the code into Error().
	var wire *jsonrpc.Error
	if errors.As(err, &wire) {
		if isBindingTerminal(wire.Message) || isBindingTerminal(string(wire.Data)) {
			return false
		}
	}
	if isBindingTerminal(err.Error()) {
		return false
	}
	// JSON-RPC wire error codes: -32001 RequestTimeout, -32000 ConnectionClosed.
	if wire != nil {
		if wire.Code == -32001 || wire.Code == -32000 {
			return true
		}
		// Any other JSON-RPC code is an application/protocol error → not retriable.
		return false
	}
	// SDK sentinels: a closed connection or a deadline are transport-level.
	if errors.Is(err, sdkmcp.ErrConnectionClosed) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	return connErrorRe.MatchString(err.Error())
}

// ReadCallOptions is the canonical CallOptions for a READ-ONLY in-turn MCP call:
// a bounded per-attempt timeout plus the read-only retry budget. It is the in-turn
// analogue of the daemon's daemonReadCallOptions — both reads hit the same throttled
// terminal.getStatus / terminal.getOutput, and a Retries>0 budget is what lets
// CallTool absorb a transient transport blip OR a getOutput throttle (see
// throttleRetryAfter) below the model, so neither surfaces as a failed read that
// costs a whole LLM round to re-decide. CallTool force-disables retry for mutating
// tools, so this is safe to hand to a generic read adapter.
func ReadCallOptions() CallOptions {
	return CallOptions{Timeout: mcpReadTimeout, Retries: mcpReadRetryPolicy.MaxRetries}
}

// throttleCodeMarker is Daintree's machine code for a per-tool read throttle. The
// server returns it as a tool RESULT (IsError=true), NOT a transport error — e.g.
//
//	{"code":"MCP_RATE_LIMITED","message":"Rate limit exceeded for 'terminal.getOutput'. Retry after 1s.","retriable":true,"details":{"retryAfter":1}}
//
// so CallTool's err==nil break would hand it straight to the model as a failed read,
// which then burns a whole LLM round improvising (observed thrash: "let me use
// terminal.summarize instead", "let me read each terminal individually"). We match
// the structural CODE, not the human phrase, so a terminal whose own scrollback
// mentions "rate limit" can't be mistaken for a throttle — and IsError already gates
// this to error results, never a successful read.
const throttleCodeMarker = "MCP_RATE_LIMITED"

// Throttle backoff knobs (vars, mirroring mcpReadRetryPolicy, so a test can shrink
// them). maxThrottleRetryAfter caps an honoured retryAfter so a pathological server
// value can't park a read for minutes; throttleBaseDelay is the fallback when the
// result carries no parseable retryAfter (a reworded throttle still earns one polite
// wait).
var (
	maxThrottleRetryAfter = 8 * time.Second
	throttleBaseDelay     = 500 * time.Millisecond
)

// retryAfterRe pulls the integer SECONDS out of the server's `"retryAfter":1`
// (details.retryAfter). Tolerant of surrounding JSON whitespace.
var retryAfterRe = regexp.MustCompile(`"retryAfter"\s*:\s*(\d+)`)

// throttleRetryAfter reports whether a SUCCESSFUL-but-IsError result text is a
// Daintree read throttle, and how long to wait before retrying. The delay is the
// server's details.retryAfter (seconds) clamped to (0, maxThrottleRetryAfter], or
// throttleBaseDelay when no hint is present. Returns (0, false) for any non-throttle
// result so the caller breaks and surfaces it normally.
func throttleRetryAfter(text string) (time.Duration, bool) {
	if !strings.Contains(text, throttleCodeMarker) {
		return 0, false
	}
	delay := throttleBaseDelay
	if m := retryAfterRe.FindStringSubmatch(text); m != nil {
		// \d+ guarantees Atoi succeeds; the d>0 guard also catches an int64 overflow
		// from an absurd value (which wraps negative) → fall through to the cap.
		if secs, err := strconv.Atoi(m[1]); err == nil && secs > 0 {
			if d := time.Duration(secs) * time.Second; d > 0 && d <= maxThrottleRetryAfter {
				delay = d
			} else {
				delay = maxThrottleRetryAfter
			}
		}
	}
	return delay, true
}

// abortableSleep sleeps for d or returns early with ctx.Err() the moment ctx is
// done (cancel or deadline). No listener leak.
func abortableSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// isAborted reports whether the context error is a caller cancellation (NOT a
// deadline). "aborted" in the spec maps to context.Canceled — a deadline is a
// timeout, which the RETRY logic treats differently (a client-side per-attempt
// timeout is exactly the transient blip a retry exists to absorb).
func isAborted(ctx context.Context) bool {
	return errors.Is(ctx.Err(), context.Canceled)
}

// callerDone reports whether the CALLER's context is finished for ANY reason —
// cancellation or deadline. This is the correct gate for the DEGRADE decision,
// which isAborted is not.
//
// Every network attempt runs on a per-attempt CHILD context
// (`context.WithTimeout(ctx, timeout)` in CallTool), so the two cases are
// distinguishable at the degrade site:
//
//   - the client's OWN per-attempt deadline fired → the child is done but the
//     caller's ctx is still clean → the transport really is wedged → DEGRADE;
//   - the CALLER's budget ran out → ctx.Err() is non-nil → the transport told us
//     nothing about its health → do NOT degrade.
//
// Conflating them lets any caller-side timeout tear down the process-wide MCP
// session (markDegraded nulls c.low, clears connected, and Closes the transport),
// which is how a bounded /doctor probe or an over-budget scheduler job could take
// the whole cockpit offline until the next reconnect. Callers are still expected
// to bound best-effort reads with time.AfterFunc(cancel) rather than
// context.WithTimeout (see internal/app/toolterminal.go) — this is the
// belt-and-braces guard for the ones that don't.
func callerDone(ctx context.Context) bool {
	return ctx.Err() != nil
}

// isSessionDead reports whether err is DEFINITIVE evidence that the transport is
// gone — the session was closed underneath us — as opposed to an ambiguous timeout.
//
// This is the escape hatch on callerDone. Context state is not error provenance: a
// caller's budget expiring at the same moment the transport dies is a coincidence,
// and suppressing the degrade there would leave `connected` true over a session that
// can never answer again. That is the dangerous direction, because the degrade IS
// the health signal — the supervisor's monitorMcp only reads Status().Connected and
// never probes independently, so nothing else would ever trigger a reconnect.
//
// Deliberately NARROW. context.DeadlineExceeded is excluded even though
// isRetriableMcpError treats it as transport-level: a deadline is exactly the
// ambiguous case (slow-but-alive server vs. wedged transport) that callerDone exists
// to stop over-reading. Only an explicit closed/evicted-session sentinel or the
// JSON-RPC ConnectionClosed code (-32000) counts. The broad connErrorRe patterns and
// an HTTP/2 GOAWAY are deliberately NOT here — Streamable HTTP recovers those on a
// fresh underlying connection.
//
// KNOWN GAP (not covered here, needs Daintree-side semantics to fix safely):
// Daintree reports a dead window binding as a SUCCESSFUL JSON-RPC response carrying
// CallToolResult{isError:true, code:SESSION_BINDING_GONE|BINDING_STALE} — see
// buildToolError in daintree/electron/services/mcp-server/sessionServer.ts. That
// lands in CallTool's `err == nil` branch, so it never reaches any degrade gate:
// `connected` stays true, monitorMcp never reconnects, and watcher reads re-arm
// against a dead window indefinitely. isBindingTerminal already recognises the
// marker (it makes such results non-retriable) — the missing half is deciding
// whether the binding is SESSION-scoped (degrade + reconnect is right) or
// TERMINAL-scoped (degrading the shared transport would be a gross over-reaction on
// one stale terminal). Resolve that before wiring it into markDegraded.
func isSessionDead(err error) bool {
	if err == nil {
		return false
	}
	// ErrConnectionClosed: the transport is gone. ErrSessionMissing: Streamable
	// HTTP answered 404 for our Mcp-Session-Id — the server evicted the session, so
	// every future call on this client fails identically until we re-handshake.
	if errors.Is(err, sdkmcp.ErrConnectionClosed) || errors.Is(err, sdkmcp.ErrSessionMissing) {
		return true
	}
	var wire *jsonrpc.Error
	if errors.As(err, &wire) && wire != nil && wire.Code == -32000 {
		return true
	}
	return false
}

// errMsg renders any error as a string (TS errMsg shim). nil → "". The text is
// sanitized: a transport error (*url.Error and the SDK errors wrapping it)
// embeds the FULL request URL — query string included, which for an MCP endpoint
// can carry credentials — so every URL in the message is stripped to
// scheme://host/path before the string can reach lastError/Status()/debug logs
// (see sanitizeErrText in errors.go).
func errMsg(err error) string {
	if err == nil {
		return ""
	}
	return sanitizeErrText(err.Error())
}

// isBindingTerminal reports whether a tool-result text carries one of Daintree's
// terminal binding markers (SESSION_BINDING_GONE / BINDING_STALE). Exposed as a
// helper so callers (and tests) can confirm these are classified terminal, never
// retriable. The client itself does not parse them, but the predicate documents
// the contract in one place.
func isBindingTerminal(text string) bool {
	if text == "" {
		return false
	}
	up := strings.ToUpper(text)
	return strings.Contains(up, "SESSION_BINDING_GONE") || strings.Contains(up, "BINDING_STALE")
}
