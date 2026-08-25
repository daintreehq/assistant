package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/auth"
	"github.com/daintreehq/assistant/internal/config"
)

// authavailability_test.go pins what `auth status` actually SAYS for each shape a
// deployment can have.
//
// The rule these exist to enforce is a prohibition, and a prohibition is only worth
// anything if something checks the rendered output: a deployment with no accounts must
// read as neither signed in nor an outage. It used to read as both at once.

// statusShapes are the four answers discovery can give, and what each must produce.
var statusShapes = []struct {
	name string
	st   auth.Status
	// wantHuman / bannedHuman are substrings, checked against the whole block.
	wantHuman   []string
	bannedHuman []string
	wantExit    int
}{
	{
		name: "accounts not offered",
		st: auth.Status{StorageTier: auth.TierKeychain}.
			WithAvailability(auth.Availability{Known: true}),
		wantHuman: []string{"not offered by this backend", "no accounts", "Nothing to do"},
		// The two forbidden readings, checked as literal text rather than as state
		// constants — what a person sees is the thing the rule is about.
		bannedHuman: []string{"could not check", "Run `daintree-assistant auth login`"},
		wantExit:    0,
	},
	{
		name: "accounts supported, not required",
		st: auth.Status{State: auth.StateSignedOut, StorageTier: auth.TierKeychain}.
			WithAvailability(auth.Availability{Known: true, Configured: true}),
		wantHuman:   []string{"supported, not required", "signed out", "auth login"},
		bannedHuman: []string{"could not check"},
		wantExit:    3,
	},
	{
		name: "accounts required",
		st: auth.Status{State: auth.StateSignedOut, StorageTier: auth.TierKeychain}.
			WithAvailability(auth.Availability{Known: true, Configured: true, Required: true}),
		wantHuman:   []string{"accounts     required", "signed out", "auth login"},
		bannedHuman: []string{"could not check"},
		wantExit:    3,
	},
	{
		name: "could not ask",
		st: auth.Status{State: auth.StateUnknown, StorageTier: auth.TierKeychain}.
			WithAvailability(auth.Availability{}),
		// Spelled out rather than omitted: a missing row reads as "fine" to anyone
		// skimming, and the one thing this line must never do is let an unreachable
		// backend pass for a deployment that simply has no accounts.
		wantHuman:   []string{"could not ask this backend"},
		bannedHuman: []string{"not offered by this backend"},
		wantExit:    0,
	},
}

func TestTheFourDeploymentShapesRenderDistinctly(t *testing.T) {
	seen := map[string]string{}
	for _, tc := range statusShapes {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			renderAuthStatus(authWriter{out: &out, err: &out}, tc.st, config.AppConfig{})
			got := out.String()

			for _, want := range tc.wantHuman {
				if !strings.Contains(got, want) {
					t.Errorf("missing %q in:\n%s", want, got)
				}
			}
			for _, banned := range tc.bannedHuman {
				if strings.Contains(got, banned) {
					t.Errorf("contains %q, which is the wrong reading here:\n%s", banned, got)
				}
			}
			if prev, dup := seen[got]; dup {
				t.Errorf("renders identically to %q — the shapes are indistinguishable", prev)
			}
			seen[got] = tc.name
		})
	}
}

// The prohibition, stated as directly as it can be: for a deployment with no accounts,
// neither of the two wrong readings may appear anywhere in the rendered block or in the
// JSON a native consumer parses.
func TestAnAccountlessDeploymentIsNeitherASessionNorAnOutage(t *testing.T) {
	st := auth.Status{State: auth.StateSignedInUnverified, Authenticated: true, StorageTier: auth.TierKeychain}.
		WithAvailability(auth.Availability{Known: true})

	// Note the input: a machine that DID sign in, on a deployment that has since turned
	// accounts off. That is the case a projection derived from the credential store gets
	// wrong, because the store knows nothing about the deployment.
	if st.State != auth.StateAccountsUnavailable {
		t.Errorf("state = %q — a stale local credential outvoted the deployment", st.State)
	}
	if st.Authenticated {
		t.Error("authenticated = true for a deployment with no accounts to be authenticated to")
	}

	var out bytes.Buffer
	renderAuthStatus(authWriter{out: &out, err: &out}, st, config.AppConfig{})
	for _, banned := range []string{"could not check", "renewing", "auth login"} {
		if strings.Contains(out.String(), banned) {
			t.Errorf("rendered %q:\n%s", banned, out.String())
		}
	}
}

// The wire half. Daintree parses this line by line against a schema keyed on the event
// version, so the fields have to be present exactly when they are known and absent
// exactly when they are not.
func TestTheStatusEventCarriesTheDeploymentDimension(t *testing.T) {
	cases := []struct {
		name           string
		av             auth.Availability
		wantConfigured any
		wantRequired   any
	}{
		{"not offered", auth.Availability{Known: true}, false, false},
		{"optional", auth.Availability{Known: true, Configured: true}, true, false},
		{"required", auth.Availability{Known: true, Configured: true, Required: true}, true, true},
		{"could not ask", auth.Availability{}, nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			w := authWriter{json: true, out: &out, err: &bytes.Buffer{}}
			st := auth.Status{State: auth.StateSignedOut, StorageTier: auth.TierKeychain}.WithAvailability(tc.av)
			w.event(authEvent{Type: "auth:status", Env: st.Environment, Extra: st})

			// ONE line: Daintree reads this stream line by line, and an indented
			// document would present a bare "{" as the first thing it parsed.
			if n := strings.Count(strings.TrimRight(out.String(), "\n"), "\n"); n != 0 {
				t.Fatalf("the status event spans %d newlines:\n%s", n+1, out.String())
			}

			var ev struct {
				V    int            `json:"v"`
				Type string         `json:"type"`
				Data map[string]any `json:"data"`
			}
			if err := json.Unmarshal(out.Bytes(), &ev); err != nil {
				t.Fatalf("the status line is not valid JSON: %v\n%s", err, out.String())
			}
			if ev.V != authEventVersion || ev.Type != "auth:status" {
				t.Fatalf("envelope = v%d %q", ev.V, ev.Type)
			}

			assertJSONField(t, ev.Data, "configured", tc.wantConfigured)
			assertJSONField(t, ev.Data, "authRequired", tc.wantRequired)

			// The values that need an authoritative backend endpoint we do not have yet
			// must stay absent rather than be invented — an absent plan is honest, and a
			// guessed one is a billing claim.
			for _, invented := range []string{"planId", "entitlementSource", "usageRemaining"} {
				if _, ok := ev.Data[invented]; ok {
					t.Errorf("%s was published with no backend authority for it", invented)
				}
			}
		})
	}
}

// assertJSONField checks presence and value together. want nil means the field must be
// ABSENT — which is a different fact from present-and-false, and the whole reason these
// are pointers.
func assertJSONField(t *testing.T, data map[string]any, field string, want any) {
	t.Helper()
	got, present := data[field]
	if want == nil {
		if present {
			t.Errorf("%s = %v, want absent — a consumer would read it as a verdict", field, got)
		}
		return
	}
	if !present {
		t.Errorf("%s is absent, want %v", field, want)
		return
	}
	if got != want {
		t.Errorf("%s = %v, want %v", field, got, want)
	}
}
