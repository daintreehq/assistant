package auth

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// CachedLinks is the projection an account-failure message renders through, so its whole
// value is that reading it costs nothing. A version that fetched would put a network round
// trip inside the composition of an error string — on the one code path where the network
// has just been demonstrated to be a problem.
//
// This proves it issues no NEW request. It does not exercise an in-flight fetch, which
// CachedLinks also never joins; that arm is held by the lock, not by a branch.
func TestCachedLinksNeverFetch(t *testing.T) {
	var hits int32
	srv := manifestServer(t, validManifest(), "", &hits)
	defer srv.Close()

	d := NewDiscoverer(srv.URL, nil)
	if got := d.CachedLinks(); got != (StatusLinks{}) {
		t.Fatalf("CachedLinks = %+v before any discovery ran, want the zero value", got)
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Fatalf("server saw %d requests, want 0 — CachedLinks fetched", got)
	}

	if _, err := d.Manifest(context.Background()); err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	before := atomic.LoadInt32(&hits)
	for i := 0; i < 5; i++ {
		if got := d.CachedLinks(); got == (StatusLinks{}) {
			t.Fatalf("call %d returned nothing after a successful discovery", i)
		}
	}
	if got := atomic.LoadInt32(&hits); got != before {
		t.Fatalf("server saw %d requests, want %d — CachedLinks fetched", got, before)
	}
}

// The serve-TTL governs whether a cached copy may still answer as AUTHORITATIVE — the
// issuer and client id a login is about to be conducted against. A link is not that
// claim, and manifestCacheTTL is a minute, far shorter than a session: binding the two
// would mean the link almost never appeared on exactly the configured happy path it
// exists for. The companion property, that Manifest DOES apply the TTL, is pinned by
// TestTheCacheExpires.
func TestCachedLinksOutliveTheServeTTL(t *testing.T) {
	srv := manifestServer(t, validManifest(), "", nil)
	defer srv.Close()

	d := NewDiscoverer(srv.URL, nil)
	base := time.Now()
	d.now = func() time.Time { return base }
	if _, err := d.Manifest(context.Background()); err != nil {
		t.Fatalf("Manifest: %v", err)
	}

	d.now = func() time.Time { return base.Add(10 * manifestCacheTTL) }
	got := d.CachedLinks()
	if got.Subscribe != "https://staging.daintree.org/subscribe" {
		t.Fatalf("Subscribe = %q after the serve window — the validated links were dropped once they stopped being servable", got.Subscribe)
	}
}

// The accessor answers with two browser destinations and nothing else. Widening it to the
// manifest would hand a caller a stale ISSUER, token endpoint and client id under an
// exemption that was only ever argued for links — so this pins the shape, not just the
// values.
func TestCachedLinksExposeNoIssuerMaterial(t *testing.T) {
	srv := manifestServer(t, validManifest(), "", nil)
	defer srv.Close()

	d := NewDiscoverer(srv.URL, nil)
	if _, err := d.Manifest(context.Background()); err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	got := d.CachedLinks()
	want := StatusLinks{
		Account:   "https://staging.daintree.org/account",
		Subscribe: "https://staging.daintree.org/subscribe",
	}
	if got != want {
		t.Fatalf("CachedLinks = %+v, want %+v", got, want)
	}
}

// Endpoint safety comes first from baseURL being immutable per Discoverer: a `/backend`
// switch builds a new Manager and with it a new Discoverer, so one endpoint's cache can
// never answer for another. Nothing on the switch path calls Invalidate, which is why
// this is the property worth pinning.
func TestACachedLinkCannotCrossEndpoints(t *testing.T) {
	staging := manifestServer(t, validManifest(), "", nil)
	defer staging.Close()

	warm := NewDiscoverer(staging.URL, nil)
	if _, err := warm.Manifest(context.Background()); err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if warm.CachedLinks() == (StatusLinks{}) {
		t.Fatal("nothing cached after a successful discovery")
	}

	// What a switch actually produces: a second Discoverer, which shares nothing.
	fresh := NewDiscoverer(staging.URL, nil)
	if got := fresh.CachedLinks(); got != (StatusLinks{}) {
		t.Fatalf("a newly built Discoverer answered %+v — caches are being shared across endpoints", got)
	}
}

// Invalidate remains correct as the second line: where it IS called, it drops the links
// with the manifest rather than leaving a link behind describing a deployment we have left.
func TestInvalidateDropsTheCachedLinks(t *testing.T) {
	srv := manifestServer(t, validManifest(), "", nil)
	defer srv.Close()

	d := NewDiscoverer(srv.URL, nil)
	if _, err := d.Manifest(context.Background()); err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if d.CachedLinks() == (StatusLinks{}) {
		t.Fatal("nothing cached after a successful discovery")
	}

	d.Invalidate()
	if got := d.CachedLinks(); got != (StatusLinks{}) {
		t.Fatalf("CachedLinks = %+v after Invalidate", got)
	}
}

// A caller with no manifest gets the zero value, not a guess. The one accepted
// degradation when discovery is unavailable is "no link" — never a URL assembled from the
// backend hostname.
func TestCachedLinksAreEmptyBeforeDiscoverySucceeds(t *testing.T) {
	m, err := NewManager(Options{StateRoot: t.TempDir(), BackendURL: "https://assistant.daintree.org"})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if got := m.CachedLinks(); got != (StatusLinks{}) {
		t.Fatalf("CachedLinks = %+v with no discovery, want the zero value", got)
	}
}

// The links must be the manifest's own validated, origin-pinned values, arriving through
// the same Status.WithManifest projection every other surface renders — not a pair of
// strings this path assembles for itself.
func TestManagerCachedLinksComeFromTheValidatedManifest(t *testing.T) {
	srv := manifestServer(t, validManifest(), "", nil)
	defer srv.Close()

	m, err := NewManager(Options{StateRoot: filepath.Join(t.TempDir(), "root"), BackendURL: srv.URL})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if _, err := m.Manifest(context.Background()); err != nil {
		t.Fatalf("Manifest: %v", err)
	}

	got := m.CachedLinks()
	want := StatusLinks{
		Account:   "https://staging.daintree.org/account",
		Subscribe: "https://staging.daintree.org/subscribe",
	}
	if got != want {
		t.Fatalf("CachedLinks = %+v, want %+v", got, want)
	}
	// The same values a full Status carries, so the two link paths cannot drift.
	man := validManifest()
	if projected := (Status{}).WithManifest(&man).Links; projected != got {
		t.Fatalf("CachedLinks = %+v but Status.WithManifest gives %+v", got, projected)
	}
}

// Configured is a *bool because an absent flag and a false one are different answers.
// Cloning it shallowly lets a caller flip the CACHED manifest's "this deployment has
// accounts" answer after validation has already passed on it — and a later 304 reuses
// that object without revalidating.
func TestCloneDoesNotShareTheConfiguredFlag(t *testing.T) {
	m := validManifest()
	configured := true
	m.Configured = &configured

	c := m.clone()
	if c.Configured == nil {
		t.Fatal("clone dropped Configured")
	}
	*c.Configured = false
	if !*m.Configured {
		t.Fatal("mutating the clone's Configured rewrote the original — the pointer is shared")
	}
}
