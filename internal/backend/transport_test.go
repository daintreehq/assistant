package backend

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"testing"
	"time"
)

// derefMs is a test helper: the millisecond value, or -1 when the mark was never taken.
// -1 rather than 0 so an absent mark can never be mistaken for a measured zero — which
// is the entire property these tests exist to defend.
func derefMs(v *int64) int64 {
	if v == nil {
		return -1
	}
	return *v
}

// The marks must survive onto the result of a normal streamed round — this is the half
// of the wall clock the backend's own timings cannot see, so if it does not reach the
// caller it may as well not be measured.
func TestRespondStreamRecordsTransportMarks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, timingsSSE(doneWithFullTimings))
	}))
	t.Cleanup(srv.Close)

	c := NewClient(ClientConfig{BaseURL: srv.URL})
	res, err := c.RespondStream(context.Background(), RespondRequest{}, StreamCallbacks{})
	if err != nil {
		t.Fatalf("RespondStream: %v", err)
	}
	m := res.Transport
	if m == nil {
		t.Fatal("no transport marks — the client-side half of the wait went unmeasured")
	}
	// Presence first: over loopback every one of these is legitimately 0 ms, so an
	// ordering assertion alone would pass on a recorder that never stamped anything.
	// (Hook-to-field attribution is pinned exactly in TestTransportRecorderMapsEachHook.)
	for name, got := range map[string]*int64{
		"ConnectMs": m.ConnectMs, "RequestSentMs": m.RequestSentMs, "FirstByteMs": m.FirstByteMs,
	} {
		if got == nil {
			t.Errorf("%s was never stamped", name)
		}
	}
	if m.Reused == nil {
		t.Error("Reused was never stamped")
	}
	if derefMs(m.ConnectMs) > derefMs(m.RequestSentMs) || derefMs(m.RequestSentMs) > derefMs(m.FirstByteMs) {
		t.Errorf("marks out of order: connect=%d sent=%d firstByte=%d",
			derefMs(m.ConnectMs), derefMs(m.RequestSentMs), derefMs(m.FirstByteMs))
	}
	// Plain HTTP to a test server: no handshake happened, so the field must be ABSENT
	// rather than 0 — a zero would read as an instantaneous handshake.
	if m.TLSMs != nil {
		t.Errorf("clientTlsMs = %d on a plain-HTTP request", *m.TLSMs)
	}
}

// Each hook must populate its OWN field and no other. Over loopback every duration is
// ~0 ms, so a recorder that stamped ConnectMs from the WroteRequest hook would sail
// through any timing-based assertion. Driving the hooks individually is the only way to
// pin attribution, and absence is the signal that makes it work.
func TestTransportRecorderMapsEachHook(t *testing.T) {
	cases := []struct {
		name string
		fire func(*httptrace.ClientTrace)
		want string // the single field expected to be non-nil
	}{
		{"GotConn", func(tr *httptrace.ClientTrace) { tr.GotConn(httptrace.GotConnInfo{}) }, "connect"},
		{"WroteRequest", func(tr *httptrace.ClientTrace) { tr.WroteRequest(httptrace.WroteRequestInfo{}) }, "requestSent"},
		{"GotFirstResponseByte", func(tr *httptrace.ClientTrace) { tr.GotFirstResponseByte() }, "firstByte"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newTransportRecorder()
			c.fire(r.trace())
			m := r.result()
			if m == nil {
				t.Fatalf("%s stamped nothing", c.name)
			}
			got := map[string]bool{
				"connect":     m.ConnectMs != nil,
				"requestSent": m.RequestSentMs != nil,
				"firstByte":   m.FirstByteMs != nil,
			}
			for field, present := range got {
				if want := field == c.want; present != want {
					t.Errorf("%s: %s present = %v, want %v", c.name, field, present, want)
				}
			}
		})
	}
}

// A write that ERRORED did not deliver the request; dating it would put a send that
// never landed next to one that did.
func TestTransportRecorderIgnoresAFailedWrite(t *testing.T) {
	r := newTransportRecorder()
	tr := r.trace()
	tr.GotConn(httptrace.GotConnInfo{})
	tr.WroteRequest(httptrace.WroteRequestInfo{Err: io.ErrClosedPipe})
	if m := r.result(); m == nil || m.RequestSentMs != nil {
		t.Fatalf("a failed write was recorded as a completed send: %+v", m)
	}
}

// One http.Client.Do can span more than one round trip — net/http transparently
// re-dials when a pooled connection turns out to be dead. Keeping the FIRST GotConn
// would then report `Reused=true` with a near-zero connect for a request that actually
// paid a full dial: exactly inverting the reading, on the field the log tells people to
// check before concluding anything got slower.
func TestTransportRecorderKeepsTheConnectionThatCarriedTheRequest(t *testing.T) {
	r := newTransportRecorder()
	tr := r.trace()
	tr.GotConn(httptrace.GotConnInfo{Reused: true})  // stale pooled connection…
	tr.GotConn(httptrace.GotConnInfo{Reused: false}) // …dead, so net/http re-dialed
	m := r.result()
	if m == nil || m.Reused == nil {
		t.Fatal("no connection recorded")
	}
	if *m.Reused {
		t.Error("reported the discarded pooled connection, not the one that carried the request")
	}
}

// The second round of a turn reuses the pooled connection. That flag is what stops a
// warm round and a cold one from being averaged into a number describing neither — and
// it is the likely explanation for a session's first turn looking far worse.
func TestTransportMarksReportConnectionReuse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, timingsSSE(doneWithoutTimings))
	}))
	t.Cleanup(srv.Close)

	c := NewClient(ClientConfig{BaseURL: srv.URL})
	first, err := c.RespondStream(context.Background(), RespondRequest{}, StreamCallbacks{})
	if err != nil {
		t.Fatalf("first round: %v", err)
	}
	if first.Transport == nil || first.Transport.Reused == nil || *first.Transport.Reused {
		t.Fatalf("first round reported a reused connection: %+v", first.Transport)
	}
	second, err := c.RespondStream(context.Background(), RespondRequest{}, StreamCallbacks{})
	if err != nil {
		t.Fatalf("second round: %v", err)
	}
	if second.Transport == nil || second.Transport.Reused == nil || !*second.Transport.Reused {
		t.Fatalf("second round did not reuse the pooled connection: %+v", second.Transport)
	}
}

// A server that sits on the request before answering must show up as time-to-first-byte,
// not as connect or upload time. This is the mark that carries the backend's own
// accept/queue/route cost — the part that happens BEFORE the clock its preparation_ms
// reports even starts.
func TestTransportMarksAttributeServerDelayToFirstByte(t *testing.T) {
	const delay = 120 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(delay)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, timingsSSE(doneWithoutTimings))
	}))
	t.Cleanup(srv.Close)

	res, err := NewClient(ClientConfig{BaseURL: srv.URL}).
		RespondStream(context.Background(), RespondRequest{}, StreamCallbacks{})
	if err != nil {
		t.Fatalf("RespondStream: %v", err)
	}
	m := res.Transport
	if m == nil || m.RequestSentMs == nil || m.FirstByteMs == nil {
		t.Fatalf("marks incomplete: %+v", m)
	}
	// A lower bound only: a loaded box or the race detector can stretch the measured
	// gap, never shrink it below the sleep, so this cannot flake in CI. Deliberately NOT
	// paired with an upper bound on RequestSentMs — that direction IS stall-sensitive,
	// and this assertion already fails if the mark is stamped at the wrong hook (a
	// RequestSentMs taken at first-byte time collapses the gap to ~0).
	if gap := *m.FirstByteMs - *m.RequestSentMs; gap < delay.Milliseconds()/2 {
		t.Errorf("first byte only %dms after the request was sent, want ≳%dms — "+
			"the server's think time was attributed to the wrong phase", gap, delay.Milliseconds())
	}
}

// A 502/503 unambiguously reached the wire — it HAS a first byte. Returning it with no
// marks would leave the log implying the opposite, and this is precisely the failure
// where "did we even get there, and how long did it take" is the question.
func TestTransportMarksSurviveAnHTTPErrorResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest) // 400: terminal, so no retries muddy the result
		_, _ = io.WriteString(w, `{"error":{"code":"bad_request","message":"nope"}}`)
	}))
	t.Cleanup(srv.Close)

	res, err := NewClient(ClientConfig{BaseURL: srv.URL}).
		RespondStream(context.Background(), RespondRequest{}, StreamCallbacks{})
	if err == nil {
		t.Fatal("expected the 400 to surface as an error")
	}
	if res.Transport == nil {
		t.Fatal("a 400 response reached the wire but reported no transport marks")
	}
	if res.Transport.FirstByteMs == nil {
		t.Error("a response with a status line has a first byte; it went unrecorded")
	}
}

// A refused socket produces no marks at all. Zeroes there would read as an
// instantaneous connection to a host that was never reached.
func TestTransportMarksAbsentWhenNothingReachedTheWire(t *testing.T) {
	r := newTransportRecorder()
	if got := r.result(); got != nil {
		t.Fatalf("marks = %+v, want nil before anything happened", got)
	}
}
