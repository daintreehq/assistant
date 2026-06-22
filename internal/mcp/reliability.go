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
// timeout, which the retry/degrade logic treats differently.
func isAborted(ctx context.Context) bool {
	return errors.Is(ctx.Err(), context.Canceled)
}

// errMsg renders any error as a string (TS errMsg shim). nil → "".
func errMsg(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
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
