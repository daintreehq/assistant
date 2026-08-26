package app

import (
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/config"
)

func TestResolveBackendTargetAcceptsAliasNumberAndURL(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"official", backend.DefaultBaseURL},
		{"OFFICIAL", backend.DefaultBaseURL}, // people type what they see, in any case
		{"1", backend.DefaultBaseURL},        // …and the number they just read off the list
		{"local", backend.LocalBaseURL},
		{"2", backend.LocalBaseURL},
		{"https://staging.example", "https://staging.example"},
		{"https://staging.example/", "https://staging.example"}, // no double slash on join
	} {
		got, err := ResolveBackendTarget(tc.in)
		if err != nil {
			t.Errorf("ResolveBackendTarget(%q) error = %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ResolveBackendTarget(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A schemeless host is the mistake worth catching at the door: "127.0.0.1:8473" parses
// as a URL with an EMPTY host, so accepting it would fail much later as an unhelpful
// transport error against an endpoint that was never real.
func TestResolveBackendTargetRejectsWhatCannotBeAnEndpoint(t *testing.T) {
	for _, bad := range []string{"", "   ", "127.0.0.1:8473", "localhost", "3", "prod"} {
		if got, err := ResolveBackendTarget(bad); err == nil {
			t.Errorf("ResolveBackendTarget(%q) = %q, want an error", bad, got)
		}
	}
	// The error has to be actionable: name the aliases that WOULD have worked.
	_, err := ResolveBackendTarget("prod")
	if err == nil || !strings.Contains(err.Error(), "official") {
		t.Errorf("the error should name the valid aliases, got %v", err)
	}
}

// The switch must reach the live client, not just the config — that is the whole point
// of App.Backend being a Swappable, and a version that updated only cfg would look
// correct in /backend while every turn still went to the old endpoint.
func TestSetBackendURLSwapsTheLiveClient(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()

	before := a.Backend.BaseURL()
	target, err := a.SetBackendURL("https://switched.example")
	if err != nil {
		t.Fatalf("SetBackendURL: %v", err)
	}
	if target != "https://switched.example" {
		t.Fatalf("returned target = %q", target)
	}
	if got := a.Backend.BaseURL(); got != "https://switched.example" {
		t.Errorf("live client still points at %q (was %q)", got, before)
	}
	if got := a.SnapshotConfig().BackendURL; got != "https://switched.example" {
		t.Errorf("config still reports %q", got)
	}
	// The ledger outlives the client on purpose: a switch must not zero a running total.
	if a.CostLedger == nil {
		t.Error("the cost ledger was dropped by the swap")
	}
}

// "Which am I on?" is half the reason to run this, so the live endpoint must be marked
// even when it is a custom one that appears nowhere in the menu.
func TestDescribeBackendChoicesMarksACustomEndpoint(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()

	if _, err := a.SetBackendURL("https://custom.example"); err != nil {
		t.Fatalf("SetBackendURL: %v", err)
	}
	out := a.DescribeBackendChoices()
	if !strings.Contains(out, "custom.example") {
		t.Errorf("a custom endpoint must be named, got:\n%s", out)
	}
	if !strings.Contains(out, "→") {
		t.Errorf("something must be marked as live, got:\n%s", out)
	}
	// The switch is durable, and the listing has to say so — plus how to undo it.
	if !strings.Contains(out, "Remembered") {
		t.Errorf("it must say the choice persists, got:\n%s", out)
	}
	if !strings.Contains(out, BackendResetAlias) {
		t.Errorf("it must name the way back to the default, got:\n%s", out)
	}
}

// The point of the whole feature: a switch survives the process. Nothing else in this
// package can prove that, since App holds the choice in memory either way.
func TestSetBackendURLPersistsAcrossSessions(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()

	if _, err := a.SetBackendURL("local"); err != nil {
		t.Fatalf("SetBackendURL: %v", err)
	}
	path := a.SnapshotConfig().EndpointPath
	if got, _ := config.LoadBackendURL(path); got != backend.LocalBaseURL {
		t.Fatalf("stored endpoint = %q, want the local backend — the choice did not survive", got)
	}

	// …and `default` clears it rather than freezing today's default into a file, which
	// would keep pinning the old address if the deployed endpoint ever moved.
	if _, err := a.ResetBackendURL(); err != nil {
		t.Fatalf("ResetBackendURL: %v", err)
	}
	if got, _ := config.LoadBackendURL(path); got != "" {
		t.Errorf("reset left %q stored; it should leave nothing", got)
	}
}

// Validation matrix. The old sign-in flow normalised endpoints and was deleted with it;
// this is the one door a custom endpoint comes through now, and every rejection below is
// something that fails silently or dangerously if it is allowed past.
func TestResolveBackendTargetRejectsDangerousURLs(t *testing.T) {
	for name, in := range map[string]string{
		// Go's http.Client turns URL userinfo into a Basic Authorization header when no
		// other one is set — silently authenticating every request, in a CLI whose whole
		// contract is that it sends no credential.
		"userinfo":         "https://user:pass@backend.example",
		"userinfo no pass": "https://user@backend.example",
		// The API path is JOINED onto the base, so these never reach the API at all.
		"query":    "https://backend.example?token=x",
		"fragment": "https://backend.example#frag",
		// Every turn, its tool arguments and its tool results would cross this in the
		// clear, and an on-path attacker could rewrite the stream to inject tool calls.
		"remote plaintext":  "http://backend.example",
		"no host":           "https://",
		"control character": "https://backend.example\x1b[2J",
	} {
		if got, err := ResolveBackendTarget(in); err == nil {
			t.Errorf("%s: ResolveBackendTarget(%q) = %q, want an error", name, in, got)
		}
	}
}

// …while the things that MUST keep working, do. Loopback over plaintext is the local
// development loop: there is no network to intercept.
func TestResolveBackendTargetAcceptsWhatItShould(t *testing.T) {
	for _, in := range []string{
		"https://backend.example",
		"https://backend.example:8443",
		"https://backend.example/proxy-prefix",
		"http://127.0.0.1:8473",
		"http://localhost:8473",
	} {
		if _, err := ResolveBackendTarget(in); err != nil {
			t.Errorf("ResolveBackendTarget(%q) rejected a valid endpoint: %v", in, err)
		}
	}
}

// Choosing the endpoint you are ALREADY on is how someone pins it — `/backend official`
// on a fresh install, or `/backend local` while the environment happens to supply local.
// Returning early skipped the write while still reporting "Remembered for future
// sessions", which is a lie the user cannot see.
func TestSetBackendURLPersistsEvenWhenAlreadyLive(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()

	live := a.SnapshotConfig().BackendURL
	if _, err := a.SetBackendURL(live); err != nil {
		t.Fatalf("SetBackendURL: %v", err)
	}
	stored, err := config.LoadBackendURL(a.SnapshotConfig().EndpointPath)
	if err != nil {
		t.Fatalf("LoadBackendURL: %v", err)
	}
	if stored != live {
		t.Errorf("selecting the live endpoint must still persist it, stored %q want %q", stored, live)
	}
}

// --- the source matrix ---
//
// THE test for this whole seam. An endpoint reaches this process from five places:
// `--backend-url`, the trusted DAINTREE_BACKEND_URL, the preference `/backend` wrote to
// disk, the compiled-in default, and `/backend <url>` typed mid-session. For a long time
// only the last of them was validated properly — startup applied the plaintext rule and
// nothing else — so `https://user:pass@host` (a Basic Authorization header on every
// request) and `https://host?token=x` (a base the API path is joined onto, so nothing
// ever reaches the API) were refused in-session and dialed for a whole session if they
// arrived at launch. Same URL, two answers, decided by which door it came through.
//
// So the assertion is not "each source validates". It is that all five reach the SAME
// decision and the SAME canonical string for the same input, and it is written as one
// table precisely so a future change to one door fails here rather than in production.
//
// Aliases and menu numbers are deliberately absent: `official`, `2` and `default` mean
// something to `/backend` and nothing at startup, and that difference is intended (see
// ResolveBackendTarget).
func TestEveryEndpointSourceReachesTheSameDecision(t *testing.T) {
	for name, tc := range map[string]struct {
		in string
		// want is the canonical form every source must land on, or "" when every
		// source must refuse the input.
		want string
	}{
		"plain https":            {"https://backend.example", "https://backend.example"},
		"trailing slash":         {"https://backend.example/", "https://backend.example"},
		"path prefix":            {"https://backend.example/prefix/", "https://backend.example/prefix"},
		"loopback plaintext":     {"http://127.0.0.1:8473", "http://127.0.0.1:8473"},
		"ipv6 loopback":          {"http://[::1]:8473", "http://[::1]:8473"},
		"the compiled default":   {backend.DefaultBaseURL, backend.DefaultBaseURL},
		"the compiled local url": {backend.LocalBaseURL, backend.LocalBaseURL},
		"userinfo":               {"https://user:supersecret@backend.example", ""},
		"query token":            {"https://backend.example?token=supersecret", ""},
		"fragment":               {"https://backend.example#frag", ""},
		"remote plaintext":       {"http://backend.example", ""},
		"unsupported scheme":     {"ftp://backend.example", ""},
		"no host":                {"https://", ""},
		"schemeless":             {"127.0.0.1:8473", ""},
		"control character":      {"https://backend.example\x1b[2J", ""},
	} {
		t.Run(name, func(t *testing.T) {
			// `/backend <url>`, the door that was always strict.
			got, err := ResolveBackendTarget(tc.in)
			if tc.want == "" {
				if err == nil {
					t.Errorf("/backend accepted %q → %q", tc.in, got)
				}
			} else {
				if err != nil {
					t.Errorf("/backend rejected %q: %v", tc.in, err)
				} else if got != tc.want {
					t.Errorf("/backend canonicalized %q to %q, want %q", tc.in, got, tc.want)
				}
			}

			// `--backend-url` and DAINTREE_BACKEND_URL are the same decision made by
			// whatever launched the process, so they must FAIL the launch rather than
			// fall back — the caller just typed or exported the bad value.
			t.Run("flag", func(t *testing.T) {
				cfg, err := loadEndpointConfig(t, endpointSource{flag: tc.in})
				assertExplicitSource(t, cfg, err, tc.want)
			})
			t.Run("trusted env", func(t *testing.T) {
				cfg, err := loadEndpointConfig(t, endpointSource{env: tc.in})
				assertExplicitSource(t, cfg, err, tc.want)
			})

			// The stored preference must never brick a launch, least of all the
			// `/backend` command that is the only way to repair it. Same DECISION —
			// the URL is never dialed — different HANDLING.
			if strings.TrimSpace(tc.in) == "" {
				return // nothing to store; SaveBackendURL refuses an empty value
			}
			t.Run("stored preference", func(t *testing.T) {
				cfg, err := loadEndpointConfig(t, endpointSource{stored: tc.in})
				if err != nil {
					t.Fatalf("a stored preference must never fail the launch: %v", err)
				}
				if tc.want == "" {
					if cfg.BackendURL != backend.DefaultBaseURL {
						t.Errorf("a refused stored preference was still resolved: %q", cfg.BackendURL)
					}
					if cfg.EndpointInsecureRejected == nil && cfg.EndpointShapeRejected == nil {
						t.Error("a refused stored preference must carry a diagnostic, or `/backend` cannot explain the fallback")
					}
					return
				}
				if cfg.BackendURL != tc.want {
					t.Errorf("stored preference resolved to %q, want %q", cfg.BackendURL, tc.want)
				}
				if cfg.EndpointInsecureRejected != nil || cfg.EndpointShapeRejected != nil {
					t.Errorf("an accepted stored preference carried a rejection diagnostic: %v / %v",
						cfg.EndpointInsecureRejected, cfg.EndpointShapeRejected)
				}
			})
		})
	}
}

// endpointSource names which of the three startup doors a URL comes through. Exactly one
// is set per case.
type endpointSource struct {
	flag   string
	env    string
	stored string
}

// loadEndpointConfig resolves a config with the endpoint supplied through one door and
// the other two silenced — an exported DAINTREE_BACKEND_URL on the developer's machine
// would otherwise decide half the matrix.
func loadEndpointConfig(t *testing.T, src endpointSource) (config.AppConfig, error) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("DAINTREE_BACKEND_URL", src.env)
	t.Setenv("DAINTREE_ALLOW_INSECURE_BACKEND", "")
	if src.stored != "" {
		if err := config.SaveBackendURL(config.EndpointPath(dir), src.stored); err != nil {
			t.Fatalf("SaveBackendURL: %v", err)
		}
	}
	overrides := config.ConfigOverrides{StateDir: &dir}
	if src.flag != "" {
		overrides.BackendURL = &src.flag
	}
	return config.LoadConfig(overrides)
}

// assertExplicitSource holds the contract for the two doors a HUMAN or a harness chose
// this launch: accept and canonicalize, or fail the launch outright. Never fall back —
// a silent fallback would run the session against an endpoint nobody named.
func assertExplicitSource(t *testing.T, cfg config.AppConfig, err error, want string) {
	t.Helper()
	if want == "" {
		if err == nil {
			t.Fatalf("an explicitly-supplied bad endpoint must fail the launch, got %q", cfg.BackendURL)
		}
		if strings.Contains(err.Error(), "supersecret") {
			t.Errorf("the startup refusal echoed the secret back: %v", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.BackendURL != want {
		t.Errorf("BackendURL = %q, want %q", cfg.BackendURL, want)
	}
}

// The escape hatch is startup's and must never become `/backend`'s. Startup honours
// --allow-insecure-backend because the person launching the process is the person
// authorizing it; a running session talked into switching to a plaintext remote endpoint
// from the inside is the failure the refusal exists to prevent, so the ambient
// authorization must not reach in here.
func TestResolveBackendTargetHasNoInsecureEscapeHatch(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("DAINTREE_ALLOW_INSECURE_BACKEND", "1")
	t.Setenv("DAINTREE_BACKEND_URL", "http://backend.example")

	if got, err := ResolveBackendTarget("http://backend.example"); err == nil {
		t.Fatalf("/backend accepted a plaintext remote endpoint = %q", got)
	}
	// …while startup, with that same variable set, does authorize it. Both halves in one
	// test on purpose: the difference between them is the thing being pinned, and split
	// across two tests it reads as an inconsistency somebody would eventually "fix".
	cfg, err := config.LoadConfig(config.ConfigOverrides{StateDir: &dir})
	if err != nil {
		t.Fatalf("DAINTREE_ALLOW_INSECURE_BACKEND must still authorize a plaintext remote endpoint at startup: %v", err)
	}
	if cfg.BackendURL != "http://backend.example" {
		t.Errorf("authorized endpoint = %q", cfg.BackendURL)
	}
}

// `/backend`'s own error prose is the other thing that reaches a terminal and a debug
// log, and the alias branch quotes what was typed. A mistyped alias is worth quoting
// back; `ftp://user:pass@host` lands in the same branch and is not.
func TestResolveBackendTargetErrorsNeverEchoASecret(t *testing.T) {
	for _, in := range []string{
		"ftp://user:supersecret@backend.example",
		"https://user:supersecret@backend.example",
		"https://backend.example?token=supersecret",
		"https://user:supersecret@[::1",
	} {
		_, err := ResolveBackendTarget(in)
		if err == nil {
			t.Errorf("ResolveBackendTarget(%q) was accepted", in)
			continue
		}
		if strings.Contains(err.Error(), "supersecret") {
			t.Errorf("ResolveBackendTarget(%q) echoed the secret back: %v", in, err)
		}
	}
	// A bare token pasted into the wrong prompt reaches the same branch a mistyped alias
	// does, and no punctuation rule can tell the two apart — so nothing typed is quoted
	// back there at all. The useful half survives: the error still names the aliases that
	// WOULD have worked.
	if _, err := ResolveBackendTarget("sk-test-verysecret0123"); err == nil {
		t.Fatal("a bare token is not an endpoint")
	} else if strings.Contains(err.Error(), "verysecret") {
		t.Errorf("the alias-branch error echoed what was typed: %v", err)
	}
	if _, err := ResolveBackendTarget("prod"); err == nil || !strings.Contains(err.Error(), "official") {
		t.Errorf("the error should still name the valid aliases: %v", err)
	}
}

// A stored preference that is refused at startup falls back instead of bricking the
// launch — which is only half a contract. The other half is that the user can find out,
// and `/backend` is where they look: it is both the listing that says which endpoint is
// answering and the command that repairs the file. Rendering the refused value as a plain
// "Remembered: …" is how someone reads that they chose an endpoint their session is
// demonstrably not using.
func TestDescribeBackendChoicesExplainsARefusedPreference(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("DAINTREE_BACKEND_URL", "")
	t.Setenv("DAINTREE_ALLOW_INSECURE_BACKEND", "")
	if err := config.SaveBackendURL(config.EndpointPath(dir), "https://user:supersecret@stored.example"); err != nil {
		t.Fatal(err)
	}
	a, err := Create(CreateOptions{Overrides: config.ConfigOverrides{
		Offline: boolPtr(true), StateDir: &dir, ProjectPath: &dir, Tier: strPtr("operator"),
		WorkflowIntelligence: boolPtr(false),
	}})
	if err != nil {
		t.Fatalf("a refused stored preference must never fail the launch: %v", err)
	}
	defer a.Shutdown()

	listing := a.DescribeBackendChoices()
	if !strings.Contains(listing, "refused at startup") {
		t.Errorf("the listing does not explain why the remembered choice is not in use:\n%s", listing)
	}
	if strings.Contains(listing, "supersecret") {
		t.Errorf("the listing echoed the refused endpoint's credential:\n%s", listing)
	}
	if !strings.Contains(listing, BackendResetAlias) {
		t.Errorf("the listing does not name the way out:\n%s", listing)
	}

	// …and once the file is repaired, the warning goes. It was recorded at startup about
	// a preference that no longer exists, and a warning naming a remedy the user has
	// already applied is worse than the silence it replaced.
	if _, err := a.SetBackendURL("https://repaired.example"); err != nil {
		t.Fatalf("SetBackendURL: %v", err)
	}
	if repaired := a.DescribeBackendChoices(); strings.Contains(repaired, "refused at startup") {
		t.Errorf("the stale rejection survived the repair:\n%s", repaired)
	}
}
