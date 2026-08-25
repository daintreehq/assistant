package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// availability_test.go covers the question that is NOT "is this machine signed in":
// what does this DEPLOYMENT offer? The two are answered by different things and were
// previously collapsed, which is how a backend working exactly as designed came to be
// reported as a broken one.

// rawManifestServer serves a literal body, for shapes that are not a marshalled
// Manifest. discovery_test.go's manifestServer covers everything that is; this exists
// for the unconfigured response, whose whole point is that it is NOT a full manifest.
func rawManifestServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != DiscoveryPath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The shape a deployment with no identity provider actually returns — four fields, no
// issuer, no client id. It fails validation by design, and its flags are exactly what a
// caller needs in order to say the deployment is fine.
const unconfiguredBody = `{"version":1,"environment":"staging","configured":false,"required":false}`

// The case the whole file exists for. Reading availability only when the manifest
// VALIDATES makes the unconfigured deployment unanswerable, because the unconfigured
// deployment is precisely the one that does not validate.
func TestAvailabilitySurvivesAManifestThatCannotValidate(t *testing.T) {
	d := NewDiscoverer(rawManifestServer(t, unconfiguredBody).URL, nil)

	if _, err := d.Manifest(context.Background()); CodeOf(err) != CodeAccountsUnavailable {
		t.Fatalf("Manifest() code = %q, want %q", CodeOf(err), CodeAccountsUnavailable)
	}

	av := d.Availability(context.Background())
	if !av.Known {
		t.Fatal("Known = false — the backend answered, so this is a verdict, not a gap")
	}
	if av.Configured {
		t.Error("Configured = true for a deployment that said otherwise")
	}
	if av.Offered() {
		t.Error("Offered() = true — there is nothing here to sign in to")
	}
	if av.Required {
		t.Error("Required = true for a body that said false")
	}
	if av.Environment != "staging" {
		t.Errorf("Environment = %q, want staging — it is present even on a manifest naming no issuer", av.Environment)
	}
}

// A body that DECODES but fails the shape checks must not publish account flags. It
// described a manifest this build will not use, and its environment string reaches the
// terminal — so an unsupported version carrying `environment: "\x1b[2J…"` would both
// answer a question it was never trusted to answer and rewrite the screen.
func TestARejectedManifestPublishesNoAvailability(t *testing.T) {
	// A JSON-ESCAPED escape, so the body is valid JSON that decodes to a real control
	// character. A raw ESC byte would fail to decode and never reach the code under test.
	hostile := `{"version":99,"environment":"\u001b[2Jwiped","configured":false,"required":false}`
	d := NewDiscoverer(rawManifestServer(t, hostile).URL, nil)

	av := d.Availability(context.Background())
	if av.Known {
		t.Error("Known = true for a manifest that failed validation")
	}
	if av.Environment != "" {
		t.Errorf("Environment = %q — a rejected manifest's text reached a rendered field", av.Environment)
	}
}

// The third state, and the one a bare bool would destroy. A backend we could not reach
// has told us NOTHING, and rendering that as "this deployment has no accounts" tells
// someone their sign-in is unnecessary during an outage.
func TestAnUnreachableBackendReportsUnknownNotUnconfigured(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	av := NewDiscoverer(srv.URL, nil).Availability(context.Background())
	if av.Known {
		t.Error("Known = true for a backend that never answered")
	}
	if av.Offered() {
		t.Error("Offered() = true with nothing known")
	}
}

// Having ONCE answered, then going unreachable, keeps the last answer rather than
// flapping to unknown — which is the deliberate choice, and is worth pinning because it
// is the one that could be mistaken for the bug this type exists to prevent. Known stays
// true, so a caller that cares can tell this apart from a live reading; what it must not
// do is silently become "unknown" and lose the only answer we have.
func TestAKnownAnswerSurvivesALaterOutage(t *testing.T) {
	var fail atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		if r.URL.Path != DiscoveryPath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(unconfiguredBody))
	}))
	defer srv.Close()

	d := NewDiscoverer(srv.URL, nil)
	// Age the recorded answer past its TTL so the outage below is actually reached
	// rather than served from the fresh-answer short circuit.
	base := time.Now()
	d.now = func() time.Time { return base }
	if av := d.Availability(context.Background()); !av.Known || av.Configured {
		t.Fatalf("setup: Known=%v Configured=%v", av.Known, av.Configured)
	}

	fail.Store(true)
	d.now = func() time.Time { return base.Add(2 * manifestCacheTTL) }

	av := d.Availability(context.Background())
	if !av.Known {
		t.Error("the last known answer was discarded on an outage")
	}
	if av.Configured {
		t.Error("the answer changed while the backend was unreachable")
	}
}

// A configured deployment reports both flags, and `required` is carried through
// untouched — it is the difference between "you may sign in" and "you must".
func TestAConfiguredDeploymentReportsWhetherAuthIsRequired(t *testing.T) {
	for _, required := range []bool{false, true} {
		m := validManifest()
		yes := true
		m.Configured, m.Required = &yes, required
		srv := manifestServer(t, m, "", nil)

		av := NewDiscoverer(srv.URL, nil).Availability(context.Background())
		srv.Close()
		if !av.Known || !av.Configured {
			t.Fatalf("required=%v: Known=%v Configured=%v", required, av.Known, av.Configured)
		}
		if av.Required != required {
			t.Errorf("required=%v: Required = %v", required, av.Required)
		}
		if !av.Offered() {
			t.Errorf("required=%v: Offered() = false for a configured deployment", required)
		}
	}
}

// An older backend omits `configured` entirely. Reading absence as false would silently
// report every deployment predating the flag as having no accounts.
func TestAnAbsentConfiguredFlagCountsAsConfigured(t *testing.T) {
	m := validManifest()
	if m.Configured != nil {
		t.Fatal("the fixture sets Configured; this test needs it absent")
	}
	srv := manifestServer(t, m, "", nil)
	defer srv.Close()

	av := NewDiscoverer(srv.URL, nil).Availability(context.Background())
	if !av.Known || !av.Configured {
		t.Errorf("Known=%v Configured=%v, want both true", av.Known, av.Configured)
	}
}

// A `/backend` switch invalidates the cache. The availability has to go with it, or
// `auth status` reports backend A's "no accounts here" about backend B.
func TestInvalidateForgetsTheAvailabilityToo(t *testing.T) {
	d := NewDiscoverer(rawManifestServer(t, unconfiguredBody).URL, nil)
	if !d.Availability(context.Background()).Known {
		t.Fatal("nothing was recorded to forget")
	}

	d.Invalidate()

	d.mu.Lock()
	stale := d.availability
	d.mu.Unlock()
	if stale.Known {
		t.Error("the previous endpoint's answer survived an Invalidate")
	}
}

// --- Status projection -------------------------------------------------------------

// The pair of fields a native consumer branches on, and the rule that makes them worth
// having: `configured:false, required:false` must never render as signed in, and must
// never render as an outage. It used to be both at once.
func TestStatusDistinguishesTheThreeDeploymentShapes(t *testing.T) {
	cases := []struct {
		name           string
		av             Availability
		wantConfigured *bool
		wantRequired   *bool
	}{
		{"not configured", Availability{Known: true}, availBoolPtr(false), availBoolPtr(false)},
		{"configured, optional", Availability{Known: true, Configured: true}, availBoolPtr(true), availBoolPtr(false)},
		{"required", Availability{Known: true, Configured: true, Required: true}, availBoolPtr(true), availBoolPtr(true)},
		// Not knowing is its own answer and must stay absent rather than default to a
		// shape we did not observe.
		{"could not ask", Availability{}, nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := Status{}.WithAvailability(tc.av)
			assertBoolPtr(t, "configured", s.Configured, tc.wantConfigured)
			assertBoolPtr(t, "authRequired", s.AuthRequired, tc.wantRequired)
		})
	}
}

// The JSON contract is versioned and additive: the fields must be OMITTED when unknown,
// not serialised as false. A consumer reading `"configured": false` on an outage would
// tell the user their deployment has no accounts.
func TestUnknownAvailabilityIsOmittedFromTheWire(t *testing.T) {
	unknown, err := json.Marshal(Status{}.WithAvailability(Availability{}))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(unknown, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["configured"]; ok {
		t.Error("configured is present with nothing known — a consumer would read it as a verdict")
	}
	if _, ok := fields["authRequired"]; ok {
		t.Error("authRequired is present with nothing known")
	}

	// And the inverse: a KNOWN false has to survive the wire, or the two become
	// indistinguishable again from the other direction.
	known, err := json.Marshal(Status{}.WithAvailability(Availability{Known: true}))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(known, &fields); err != nil {
		t.Fatal(err)
	}
	if v, ok := fields["configured"]; !ok || v != false {
		t.Errorf("configured = %v (present=%v), want a literal false", v, ok)
	}
}

// --- the two states that used to lie ------------------------------------------------

// The exact rendering the doc forbids. A deployment with no accounts must be neither
// "signed in" nor an outage, and it used to be reported as both simultaneously:
// StateTemporarilyUnavailable, whose SignedIn() is true.
func TestAccountsUnavailableIsNeitherASessionNorAnOutage(t *testing.T) {
	s := StateAccountsUnavailable

	if s.SignedIn() {
		t.Error("SignedIn() = true — there is no credential and no way to obtain one")
	}
	if s == StateTemporarilyUnavailable {
		t.Error("collapsed into the outage state")
	}
	if s.NeedsLogin() {
		t.Error("NeedsLogin() = true — it would send someone to a browser flow no endpoint answers")
	}
	if !s.Terminal() {
		t.Error("Terminal() = false — nothing about this changes without a deployment change")
	}
	if s.NeedsPlan() {
		t.Error("NeedsPlan() = true")
	}
	// CanSpend is false because only a CONFIRMED ACTIVE session may spend on an
	// account. That is not the same question as whether the daemon may work: anonymous
	// requests are served here, and the supervisor's own gate (authorizedToSpendLocked)
	// deliberately permits every state it does not name, this one included.
	if s.CanSpend() {
		t.Error("CanSpend() = true — it means a confirmed account session, which this is not")
	}
}

// A settled refusal is not a blip. This is what StateTemporarilyUnavailable claimed
// about a rejected OAuth client, while reporting the session as signed in — so an
// unattended daemon kept scheduling work the backend had already refused.
func TestAccessRefusedIsSettledAndNotASession(t *testing.T) {
	s := StateAccessRefused

	// SignedIn is TRUE here on purpose: the credential exists and is deliberately kept.
	// Reporting otherwise would contradict the human line, and would make Hydrate erase
	// the refusal the moment it found the stored credential.
	if !s.SignedIn() {
		t.Error("SignedIn() = false — the credential exists and is retained")
	}
	if s == StateTemporarilyUnavailable {
		t.Error("collapsed into the outage state — this one is settled, not a blip")
	}
	if !s.Terminal() {
		t.Error("Terminal() = false — no retry, refresh or re-login changes this answer")
	}
	if s.NeedsLogin() {
		t.Error("NeedsLogin() = true — a fresh credential is refused identically")
	}
	if s.CanSpend() {
		t.Error("CanSpend() = true")
	}
}

func availBoolPtr(b bool) *bool { return &b }

func assertBoolPtr(t *testing.T, field string, got, want *bool) {
	t.Helper()
	switch {
	case want == nil && got != nil:
		t.Errorf("%s = %v, want absent", field, *got)
	case want != nil && got == nil:
		t.Errorf("%s absent, want %v", field, *want)
	case want != nil && got != nil && *got != *want:
		t.Errorf("%s = %v, want %v", field, *got, *want)
	}
}
