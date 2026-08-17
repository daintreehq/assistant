package agent

import (
	"context"
	"sort"
	"testing"

	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/models"
)

func ptr(v int) *int    { return &v }
func ms(v int64) *int64 { return &v }

// marks builds a fully-populated TransportMarks; dns/tls may be nil for the pooled case.
func marks(connect int64, reused bool, sent, firstByte int64, dns, tlsMs *int64) *backend.TransportMarks {
	return &backend.TransportMarks{
		ConnectMs: ms(connect), Reused: &reused, DNSMs: dns, TLSMs: tlsMs,
		RequestSentMs: ms(sent), FirstByteMs: ms(firstByte),
	}
}

// keysOf returns the sorted field names of a trace event's payload.
func keysOf(fields map[string]any) []string {
	out := make([]string, 0, len(fields))
	for k := range fields {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// A phase that did not happen is ABSENT from the backend's JSON, and it must stay
// absent in the log. Zero-filling here would turn "the selector never ran" into "the
// selector answered instantly" — the exact confusion the pointer contract exists to
// prevent, now baked into a file someone reads six weeks later.
func TestAddBackendTimingsOmitsAbsentPhases(t *testing.T) {
	fields := map[string]any{}
	addBackendTimings(fields, &backend.TurnTimings{
		PreparationMs: ptr(12),
		FirstOutputMs: ptr(420),
		GenerationMs:  ptr(900),
		TotalMs:       ptr(1320),
	}, 0, 0)

	want := []string{"serverFirstOutputMs", "serverGenerationMs", "serverPreparationMs", "serverTotalMs"}
	got := keysOf(fields)
	if len(got) != len(want) {
		t.Fatalf("keys = %v, want exactly %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("keys = %v, want %v", got, want)
		}
	}
	if fields["serverTotalMs"] != 1320 {
		t.Errorf("serverTotalMs = %v, want 1320", fields["serverTotalMs"])
	}
}

// Every measured phase reaches the log — this is the whole point of the change, so pin
// the key names the way a grep across a session will spell them.
func TestAddBackendTimingsWritesEveryMeasuredPhase(t *testing.T) {
	fields := map[string]any{}
	addBackendTimings(fields, &backend.TurnTimings{
		SelectionMs:    ptr(863),
		DocsMs:         ptr(210),
		PreparationMs:  ptr(1104),
		UpstreamOpenMs: ptr(1290),
		ThinkingMs:     ptr(42),
		FirstOutputMs:  ptr(1655),
		GenerationMs:   ptr(4120),
		TotalMs:        ptr(5775),
	}, 0, 0)

	for key, want := range map[string]int{
		"serverSelectionMs":    863,
		"serverDocsMs":         210,
		"serverPreparationMs":  1104,
		"serverUpstreamOpenMs": 1290,
		"serverThinkingMs":     42,
		"serverFirstOutputMs":  1655,
		"serverGenerationMs":   4120,
		"serverTotalMs":        5775,
	} {
		if fields[key] != want {
			t.Errorf("%s = %v, want %d", key, fields[key], want)
		}
	}
	// Every value renders INLINE (debuglog only inlines scalars), which is what keeps a
	// whole session's latency profile greppable instead of buried in per-round blocks.
	for k, v := range fields {
		if _, ok := v.(int); !ok {
			t.Errorf("%s is %T, want an int so it renders inline", k, v)
		}
	}
}

// The derived client-side overhead is the one number neither side can see alone: the
// round trip plus our own stream handling.
func TestAddBackendTimingsDerivesClientOverhead(t *testing.T) {
	fields := map[string]any{}
	addBackendTimings(fields, &backend.TurnTimings{TotalMs: ptr(1000)}, 1240, 0)
	if fields["clientOverheadMs"] != int64(240) {
		t.Fatalf("clientOverheadMs = %v, want 240", fields["clientOverheadMs"])
	}
}

// A RETRIED round's durationMs spans every attempt and its backoff sleeps, while the
// backend's total covers the winning attempt only. Subtracting them there would report
// an abandoned attempt as transport overhead — a wrong answer to exactly the question
// the number exists to answer, so it is withheld instead.
func TestAddBackendTimingsSuppressesOverheadAfterARetry(t *testing.T) {
	fields := map[string]any{}
	addBackendTimings(fields, &backend.TurnTimings{TotalMs: ptr(1000)}, 30000, 2)
	if _, ok := fields["clientOverheadMs"]; ok {
		t.Fatalf("clientOverheadMs = %v — a retried round's gap is not transport", fields["clientOverheadMs"])
	}
	if fields["serverTotalMs"] != 1000 {
		t.Error("the server's own timings must still be logged for a retried round")
	}
}

// The two clocks start at different instants (ours before the request is even
// serialized), so a fast round can land a millisecond "negative". Noise, not a finding.
func TestAddBackendTimingsSkipsNegativeOverhead(t *testing.T) {
	fields := map[string]any{}
	addBackendTimings(fields, &backend.TurnTimings{TotalMs: ptr(1000)}, 998, 0)
	if _, ok := fields["clientOverheadMs"]; ok {
		t.Fatalf("clientOverheadMs = %v, want it withheld", fields["clientOverheadMs"])
	}
}

// A backend that reports no timings (the deployed one, until this ships) must add
// nothing at all — not an empty block, not a row of zeroes.
func TestAddBackendTimingsNilAndEmptyAddNothing(t *testing.T) {
	for name, tm := range map[string]*backend.TurnTimings{
		"nil":   nil,
		"empty": {},
	} {
		fields := map[string]any{}
		addBackendTimings(fields, tm, 500, 0)
		if len(fields) != 0 {
			t.Errorf("%s block wrote %v, want nothing", name, fields)
		}
	}
}

// The client-side marks are the other half of the picture: the backend's clock starts
// when the request lands, so without these the difference between a client-measured
// round and the server's total is one unattributable number.
func TestAddTransportMarksWritesClientSideLatency(t *testing.T) {
	fields := map[string]any{}
	addTransportMarks(fields, marks(140, false, 210, 930, ms(31), ms(88)))
	for key, want := range map[string]any{
		"clientConnectMs":     int64(140),
		"clientConnReused":    false,
		"clientDnsMs":         int64(31),
		"clientTlsMs":         int64(88),
		"clientRequestSentMs": int64(210),
		"clientFirstByteMs":   int64(930),
	} {
		if fields[key] != want {
			t.Errorf("%s = %v, want %v", key, fields[key], want)
		}
	}
}

// On a pooled connection no lookup and no handshake happened. Reporting them as 0 would
// read as an instantaneous DNS resolution rather than as none at all — the same
// absent-is-not-zero rule the server's own phases follow.
func TestAddTransportMarksOmitsDNSAndTLSOnAReusedConnection(t *testing.T) {
	fields := map[string]any{}
	addTransportMarks(fields, marks(0, true, 4, 610, nil, nil))
	if _, ok := fields["clientDnsMs"]; ok {
		t.Error("clientDnsMs written for a connection that was never dialed")
	}
	if _, ok := fields["clientTlsMs"]; ok {
		t.Error("clientTlsMs written for a handshake that never happened")
	}
	// Reuse itself is stated whenever there WAS a connection: it is what makes a
	// near-zero connect readable rather than suspicious.
	if fields["clientConnReused"] != true {
		t.Error("connection reuse went unrecorded")
	}
	// And a genuine measured zero still gets logged — absent and zero are different
	// facts in BOTH directions.
	if fields["clientConnectMs"] != int64(0) {
		t.Errorf("clientConnectMs = %v, want a measured 0", fields["clientConnectMs"])
	}
}

// A PARTIAL set is the shape of a failed attempt: a request that connected and uploaded
// and then died before any response byte. Each missing mark must stay missing —
// `clientFirstByteMs=0` on a request that never got a response would read as the
// fastest round ever recorded.
func TestAddTransportMarksOmitsUnstampedFields(t *testing.T) {
	fields := map[string]any{}
	addTransportMarks(fields, &backend.TransportMarks{ConnectMs: ms(180), DNSMs: ms(40)})

	if fields["clientConnectMs"] != int64(180) || fields["clientDnsMs"] != int64(40) {
		t.Fatalf("measured marks lost: %v", fields)
	}
	for _, key := range []string{"clientRequestSentMs", "clientFirstByteMs", "clientConnReused"} {
		if _, ok := fields[key]; ok {
			t.Errorf("%s = %v written for a stage that never ran", key, fields[key])
		}
	}
}

// A backend fake, or an attempt that never reached the wire, writes nothing.
func TestAddTransportMarksNilAddsNothing(t *testing.T) {
	fields := map[string]any{}
	addTransportMarks(fields, nil)
	if len(fields) != 0 {
		t.Errorf("wrote %v for an unmeasured attempt, want nothing", fields)
	}
}

// timingsBackend stamps a fixed timings block onto every round and optionally fires the
// retry callback first, so the session-level trace can be driven end to end.
type timingsBackend struct {
	backendFromRouter
	timings   *backend.TurnTimings
	transport *backend.TransportMarks
	retries   int
}

func (b timingsBackend) RespondStream(ctx context.Context, req backend.RespondRequest, cb backend.StreamCallbacks) (backend.RespondResult, error) {
	for i := 0; i < b.retries && cb.OnRetry != nil; i++ {
		cb.OnRetry(backend.RetryInfo{Attempt: i + 1, MaxAttempts: 9, Op: "respond"})
	}
	res, err := b.backendFromRouter.RespondStream(ctx, req, cb)
	res.Timings = b.timings
	res.Transport = b.transport
	return res, err
}

// End to end: what the backend reports on `done` reaches the debug-log event, next to
// the client-observed durationMs it is meant to be read against.
func TestTraceBackendDoneCarriesServerTimings(t *testing.T) {
	cap := &traceCapture{}
	deps := baseDeps(&fakeRouter{results: []models.ChatResult{{Content: "hi"}}}, &fakeTools{})
	deps.Backend = timingsBackend{
		backendFromRouter: backendFromRouter{r: deps.Backend.(backendFromRouter).r},
		timings:           &backend.TurnTimings{SelectionMs: ptr(863), PreparationMs: ptr(1104), TotalMs: ptr(5775)},
		transport:         marks(140, false, 210, 930, nil, nil),
	}
	deps.Trace = cap.record

	if _, err := NewSession(deps).Send(context.Background(), "go", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	ev, ok := cap.first("backend.respond.done")
	if !ok {
		t.Fatal("missing backend.respond.done")
	}
	if ev.fields["serverSelectionMs"] != 863 || ev.fields["serverTotalMs"] != 5775 {
		t.Fatalf("server timings did not reach the trace: %v", ev.fields)
	}
	// Both halves land on the SAME line. That is the whole point — the server's phases
	// and our transport marks are only meaningful read against each other.
	if ev.fields["clientRequestSentMs"] != int64(210) || ev.fields["clientFirstByteMs"] != int64(930) {
		t.Fatalf("client transport marks did not reach the trace: %v", ev.fields)
	}
	if _, ok := ev.fields["durationMs"]; !ok {
		t.Error("durationMs is gone — the client-observed number is what the server's is read against")
	}
	if _, ok := ev.fields["retries"]; ok {
		t.Error("retries logged on a round that never retried")
	}
}

// A retried round says so. Without the tally, a minute of wall clock spent across nine
// attempts is indistinguishable from one very slow call.
func TestTraceBackendDoneCountsRetries(t *testing.T) {
	cap := &traceCapture{}
	deps := baseDeps(&fakeRouter{results: []models.ChatResult{{Content: "hi"}}}, &fakeTools{})
	deps.Backend = timingsBackend{
		backendFromRouter: backendFromRouter{r: deps.Backend.(backendFromRouter).r},
		retries:           2,
	}
	deps.Trace = cap.record
	deps.Events = NoopEventSink{}

	if _, err := NewSession(deps).Send(context.Background(), "go", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	ev, ok := cap.first("backend.respond.done")
	if !ok {
		t.Fatal("missing backend.respond.done")
	}
	if ev.fields["retries"] != 2 {
		t.Fatalf("retries = %v, want 2", ev.fields["retries"])
	}
}

// The retry tally is per ROUND, not per turn: a round that retried must not colour the
// next round's numbers, or the overhead gate stays shut for the rest of the turn.
func TestTraceBackendDoneRetryCountResetsPerRound(t *testing.T) {
	cap := &traceCapture{}
	r := &fakeRouter{results: []models.ChatResult{
		{ToolCalls: []models.ToolCallRequest{toolCall("call_1", "fs__read", `{"path":"x"}`)}},
		{Content: "done"},
	}}
	deps := baseDeps(r, &fakeTools{result: domain.Ok("read it", nil)})
	base := deps.Backend.(backendFromRouter)
	// Retry on the FIRST round only.
	first := true
	deps.Backend = retryOnceBackend{backendFromRouter: base, first: &first}
	deps.Trace = cap.record

	if _, err := NewSession(deps).Send(context.Background(), "go", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	dones := cap.only("backend.respond.done")
	if len(dones) != 2 {
		t.Fatalf("done count = %d, want 2", len(dones))
	}
	if dones[0].fields["retries"] != 1 {
		t.Errorf("round 0 retries = %v, want 1", dones[0].fields["retries"])
	}
	if _, ok := dones[1].fields["retries"]; ok {
		t.Errorf("round 1 inherited round 0's retry: %v", dones[1].fields["retries"])
	}
}

// failingBackend returns a stream failure that still carries the marks of the attempt
// that died — the shape of a mid-stream upstream failure, where the connection was
// fine and the trouble came later.
type failingBackend struct {
	backendFromRouter
	transport *backend.TransportMarks
}

func (b failingBackend) RespondStream(_ context.Context, _ backend.RespondRequest, _ backend.StreamCallbacks) (backend.RespondResult, error) {
	return backend.RespondResult{Transport: b.transport},
		&backend.Error{Code: "stream_error", Message: "upstream died", Stream: true}
}

// A failure is where the client-side marks matter MOST: a round that died 30s in reads
// completely differently once you can see the connection was established in 200ms.
func TestTraceBackendErrorCarriesTransportMarks(t *testing.T) {
	cap := &traceCapture{}
	deps := baseDeps(&fakeRouter{results: []models.ChatResult{{Content: "unused"}}}, &fakeTools{})
	deps.Backend = failingBackend{
		backendFromRouter: backendFromRouter{r: deps.Backend.(backendFromRouter).r},
		transport:         marks(200, false, 240, 2000, nil, nil),
	}
	deps.Trace = cap.record

	if _, err := NewSession(deps).Send(context.Background(), "go", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	ev, ok := cap.first("backend.respond.error")
	if !ok {
		t.Fatal("missing backend.respond.error")
	}
	if ev.fields["clientConnectMs"] != int64(200) || ev.fields["clientFirstByteMs"] != int64(2000) {
		t.Fatalf("the dead attempt's transport marks were dropped: %v", ev.fields)
	}
}

// A failure that never reached the wire carries no marks — which is itself the answer,
// and must not be dressed up as an instantaneous connection.
func TestTraceBackendErrorWithoutMarksLogsNone(t *testing.T) {
	cap := &traceCapture{}
	deps := baseDeps(&fakeRouter{results: []models.ChatResult{{Content: "unused"}}}, &fakeTools{})
	deps.Backend = failingBackend{backendFromRouter: backendFromRouter{r: deps.Backend.(backendFromRouter).r}}
	deps.Trace = cap.record

	if _, err := NewSession(deps).Send(context.Background(), "go", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	ev, ok := cap.first("backend.respond.error")
	if !ok {
		t.Fatal("missing backend.respond.error")
	}
	for _, key := range []string{"clientConnectMs", "clientConnReused", "clientFirstByteMs"} {
		if _, ok := ev.fields[key]; ok {
			t.Errorf("%s written for an attempt that never reached the wire", key)
		}
	}
}

// retryOnceBackend fires OnRetry on the first round only.
type retryOnceBackend struct {
	backendFromRouter
	first *bool
}

func (b retryOnceBackend) RespondStream(ctx context.Context, req backend.RespondRequest, cb backend.StreamCallbacks) (backend.RespondResult, error) {
	if *b.first && cb.OnRetry != nil {
		*b.first = false
		cb.OnRetry(backend.RetryInfo{Attempt: 1, MaxAttempts: 9, Op: "respond"})
	}
	return b.backendFromRouter.RespondStream(ctx, req, cb)
}
