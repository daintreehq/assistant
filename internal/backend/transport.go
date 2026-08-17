package backend

import (
	"crypto/tls"
	"net/http/httptrace"
	"sync"
	"time"
)

// This file measures the half of a turn's wall clock the SERVER cannot see: getting our
// request to it, and getting its first byte back.
//
// The backend's own `timings` block starts counting when the request arrives and stops
// when the response completes. Subtracting it from a client-measured round leaves a
// single opaque remainder — and on the first real turn measured against the deployed
// backend that remainder was 934 ms of a 7.9 s round, invisible and unattributable. It
// is also CONSTANT across the round (the gap at first token and the gap at completion
// matched to 2 ms), which is the signature of a fixed cost paid before the server's
// clock starts, not of a slow stream. These marks say which fixed cost.
//
// net/http/httptrace is the only way to see it: connection reuse, DNS, and the TLS
// handshake all happen inside http.Client.Do, and a naive time.Since around Do lumps
// them together with the upload and the server's own queueing.

// TransportMarks is one HTTP attempt's client-side latency in milliseconds. Every field
// is measured on OUR side of the wire.
//
// ConnectMs, RequestSentMs and FirstByteMs are elapsed from the START of the call, so
// they nest and the reader subtracts. DNSMs and TLSMs are the opposite — each is that
// STAGE's own duration, because that is what the httptrace hooks bracket and inventing
// a common origin for them would be presenting arithmetic as measurement.
//
// Every field is a pointer, and for the same reason the backend's phase timings are: a
// stage that did not happen must be ABSENT, never 0. That is not hypothetical here —
// a failed attempt legitimately produces a partial set (a DNS failure resolves nothing
// and connects to nothing), and a `FirstByteMs: 0` on a request that never got a
// response would read as the fastest turn ever recorded.
type TransportMarks struct {
	// ConnectMs is call start → a usable connection. On a REUSED connection this is the
	// time to check one out of the pool: near zero, and the reason a session's first
	// turn can look far worse than every turn after it. nil when no connection was
	// established at all.
	ConnectMs *int64
	// Reused reports whether the connection came from the pool, and is nil when there
	// was no connection to have an opinion about. It is the caveat that makes every
	// other field readable: a cold dial and a pooled checkout measure different things,
	// and comparing them without it invents a regression that never happened.
	Reused *bool
	// DNSMs is the name lookup's own duration, nil when none happened (a pooled
	// connection, an IP literal, or a hit inside the resolver's cache).
	DNSMs *int64
	// TLSMs is the handshake's own duration, nil when none happened. On a cold
	// connection to a remote endpoint this and DNSMs are usually most of ConnectMs.
	TLSMs *int64
	// RequestSentMs is call start → the transport reporting our request written. That is
	// NOT "when they had it", and not even reliably "when the kernel had all of it": the
	// hook fires before the transport's final buffer flush, and a warm round measured
	// 0 ms here while shipping ~200 KB of prompt, because the body fit in the socket
	// buffer. Treat it as a lower bound on our own send time.
	RequestSentMs *int64
	// FirstByteMs is call start → the first byte of the response, and the only mark that
	// is round-trip complete. Subtracting the server's own preparation_ms from
	// (FirstByteMs - RequestSentMs) leaves the real network cost — upload transmission
	// included, which is where a large uncached prompt actually shows up.
	FirstByteMs *int64
}

// transportRecorder collects httptrace marks for one attempt. The callbacks fire on
// whichever goroutine the transport happens to be using — the connection pool's dialer,
// the reader — so the mutex is not defensive style, it is required.
//
// Every mark takes the LAST occurrence, not the first. One http.Client.Do can span more
// than one round trip: net/http transparently re-dials when a pooled connection turns
// out to be dead, and a redirect adds hops. Keeping the FIRST GotConn there would report
// `Reused=true` with a near-zero connect for a request that actually paid a full dial
// and handshake — precisely inverting the reading, and on the one field the log tells
// people to check before concluding anything got slower. The last occurrence is the one
// that carried the request that was answered.
type transportRecorder struct {
	start time.Time

	mu          sync.Mutex
	dnsStart    time.Time
	tlsStart    time.Time
	connectMs   *int64
	reused      *bool
	dnsMs       *int64
	tlsMs       *int64
	requestSent *int64
	firstByteMs *int64
}

// newTransportRecorder starts the clock for one HTTP attempt.
func newTransportRecorder() *transportRecorder {
	return &transportRecorder{start: time.Now()}
}

// sinceStart is elapsed milliseconds from the attempt's origin. The caller holds r.mu;
// this takes no lock of its own, so there is no ordering to get wrong.
func (r *transportRecorder) sinceStart() int64 {
	return time.Since(r.start).Milliseconds()
}

// trace returns the httptrace hooks to attach to the request context.
//
// Only the hooks whose numbers we report are implemented. httptrace fires several more
// (Got100Continue, WroteHeaderField, …) and a recorder that captured everything would
// be a per-request allocation storm on a hot path for values nobody reads.
func (r *transportRecorder) trace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		DNSStart: func(httptrace.DNSStartInfo) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.dnsStart = time.Now()
		},
		DNSDone: func(httptrace.DNSDoneInfo) {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.dnsStart.IsZero() {
				return
			}
			ms := time.Since(r.dnsStart).Milliseconds()
			r.dnsMs = &ms
		},
		TLSHandshakeStart: func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.tlsStart = time.Now()
		},
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			// A FAILED handshake is not a handshake duration; reporting it would put the
			// cost of a connection we never used next to the one we did.
			if r.tlsStart.IsZero() || err != nil {
				return
			}
			ms := time.Since(r.tlsStart).Milliseconds()
			r.tlsMs = &ms
		},
		GotConn: func(info httptrace.GotConnInfo) {
			r.mu.Lock()
			defer r.mu.Unlock()
			ms, reused := r.sinceStart(), info.Reused
			r.connectMs, r.reused = &ms, &reused
		},
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			r.mu.Lock()
			defer r.mu.Unlock()
			// A write that errored did not deliver the request. Recording it would date
			// a send that never landed, and net/http will re-send on a fresh connection.
			if info.Err != nil {
				return
			}
			ms := r.sinceStart()
			r.requestSent = &ms
		},
		GotFirstResponseByte: func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			ms := r.sinceStart()
			r.firstByteMs = &ms
		},
	}
}

// result returns the collected marks, or nil when NOTHING was measured — a context
// cancelled before the dial, a request that failed to build.
//
// A PARTIAL set is returned, deliberately. An attempt that connected and uploaded and
// then died before any response byte is a real and interesting failure, and every field
// being a pointer is what lets that be reported as "no first byte" rather than as an
// instantaneous one.
func (r *transportRecorder) result() *TransportMarks {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := TransportMarks{
		ConnectMs:     r.connectMs,
		Reused:        r.reused,
		DNSMs:         r.dnsMs,
		TLSMs:         r.tlsMs,
		RequestSentMs: r.requestSent,
		FirstByteMs:   r.firstByteMs,
	}
	if m.ConnectMs == nil && m.Reused == nil && m.DNSMs == nil && m.TLSMs == nil &&
		m.RequestSentMs == nil && m.FirstByteMs == nil {
		return nil
	}
	return &m
}
