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

// The picker's ROWS have to match what the typed command would have done, because the
// two are the same act reached two ways. A row whose Target is not a thing
// ResolveBackendTarget accepts is a row that silently does nothing when it is chosen.
func TestBackendChoiceQuestionOffersOnlyApplicableTargets(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()

	_, picks, _ := a.BackendChoiceQuestion()
	if len(picks) < len(BackendChoices) {
		t.Fatalf("the picker dropped menu entries: %+v", picks)
	}
	for _, p := range picks {
		if strings.TrimSpace(p.Text) == "" {
			t.Errorf("a pick has no text: %+v", p)
		}
		if _, err := ResolveBackendTarget(p.Target); err != nil {
			t.Errorf("pick %q has an unusable target %q: %v", p.Text, p.Target, err)
		}
	}
}

// The highlight must start on the endpoint that is actually answering. Starting on the
// first row instead means Enter — the fastest key on the sheet — switches away from the
// backend the user is on, in a picker they opened to find out which one that was.
func TestBackendChoiceQuestionDefaultsToTheLiveEndpoint(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()

	if _, err := a.SetBackendURL("local"); err != nil {
		t.Fatalf("SetBackendURL: %v", err)
	}
	_, picks, def := a.BackendChoiceQuestion()
	if def < 0 || def >= len(picks) {
		t.Fatalf("default index %d is out of range for %d picks", def, len(picks))
	}
	if picks[def].Target != "local" {
		t.Errorf("the highlight starts on %q, not the live endpoint", picks[def].Target)
	}
	// …and the row says so, because the highlight stops meaning anything the moment
	// someone presses an arrow.
	if !strings.Contains(picks[def].Text, "current") {
		t.Errorf("the live row is not marked: %q", picks[def].Text)
	}
}

// A custom endpoint is not in the menu but IS what is answering. Without a row of its
// own every option on the sheet is a switch AWAY from it, and there is no way to keep it.
func TestBackendChoiceQuestionKeepsACustomEndpointSelectable(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()

	if _, err := a.SetBackendURL("https://custom.example"); err != nil {
		t.Fatalf("SetBackendURL: %v", err)
	}
	_, picks, def := a.BackendChoiceQuestion()
	if picks[def].Target != "https://custom.example" {
		t.Errorf("the custom endpoint is not the highlighted row: %+v", picks[def])
	}
	if !strings.Contains(picks[def].Text, "custom.example") {
		t.Errorf("the custom endpoint is not named: %q", picks[def].Text)
	}
}

// "Forget" appears only when there is something to forget. It resolves to the same URL
// as picking the deployed backend today and would not if the default ever moved, so
// offering it unconditionally would put two rows on the sheet that do the same thing.
func TestBackendChoiceQuestionOffersForgetOnlyWhenSomethingIsRemembered(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()

	hasForget := func() bool {
		_, picks, _ := a.BackendChoiceQuestion()
		for _, p := range picks {
			if p.Target == BackendResetAlias {
				return true
			}
		}
		return false
	}
	if hasForget() {
		t.Error("a fresh session offers to forget a choice that was never made")
	}
	if _, err := a.SetBackendURL("local"); err != nil {
		t.Fatalf("SetBackendURL: %v", err)
	}
	if !hasForget() {
		t.Error("a remembered choice cannot be forgotten from the picker")
	}
}
