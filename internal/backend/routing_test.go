package backend

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// The closed set is validated on BOTH sides on purpose. The backend would reject these
// too — but only at request time, which means a mistyped env var surfaces as a 400 in
// the middle of a turn, after the user has already typed a message and waited.
func TestRoutingValidateRejectsWhatTheBackendWould(t *testing.T) {
	cases := []struct {
		name string
		r    Routing
		want string // substring the error must name
	}{
		{"bad privacy", Routing{Privacy: "none"}, "no_training"},
		{"bad sort", Routing{Sort: "cheapest"}, "throughput"},
		{"uppercase slug", Routing{Only: []string{"DeepInfra"}}, "not a valid endpoint slug"},
		{"slug with a space", Routing{Only: []string{"deep infra"}}, "not a valid endpoint slug"},
		// Anchored at BOTH ends: an unanchored match accepts a trailing newline, and
		// that slug would be forwarded upstream newline and all.
		{"slug with a trailing newline", Routing{Ignore: []string{"deepinfra\n"}}, "not a valid endpoint slug"},
		{"empty slug", Routing{Only: []string{""}}, "not a valid endpoint slug"},
		{"over the list cap", Routing{Only: make([]string, MaxEndpointList+1)}, "over the limit"},
		// The backend requires a slug to START with an alphanumeric. Reading its docs
		// rather than its source once cost this exact case: ".deepinfra" passed here and
		// 400'd there, which is the failure local validation exists to prevent.
		{"leading separator", Routing{Only: []string{".deepinfra"}}, "not a valid endpoint slug"},
		{"leading dash", Routing{Ignore: []string{"-deepinfra"}}, "not a valid endpoint slug"},
		// Duplicates consume the list cap for nothing and produce a distinct request body
		// for a semantically identical policy.
		{"duplicate slug", Routing{Only: []string{"deepinfra", "deepinfra"}}, "appears more than once"},
		// An endpoint both required and forbidden guarantees an empty pool — better a
		// startup error naming the contradiction than a routing dead end mid-turn.
		{"only and ignore overlap", Routing{Only: []string{"deepinfra"}, Ignore: []string{"deepinfra"}}, "can never route"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.r.Validate()
			if err == nil {
				t.Fatalf("%+v was accepted", tc.r)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %q — the message must say what IS valid", err, tc.want)
			}
		})
	}
}

func TestRoutingValidateAcceptsTheClosedSet(t *testing.T) {
	valid := []Routing{
		{},
		{Privacy: PrivacyNoTraining},
		{Privacy: PrivacyZDR, Sort: SortPrice},
		{Sort: SortLatency},
		{Only: []string{"deepinfra", "together-ai", "fireworks.ai", "some_endpoint"}},
		{Ignore: []string{"slow-one"}},
	}
	for _, r := range valid {
		if err := r.Validate(); err != nil {
			t.Errorf("%+v was rejected: %v", r, err)
		}
	}
}

// A zero Routing must serialize to NOTHING. Sending an empty object is a different
// statement on the wire from sending no block, and the backend's default only applies to
// the latter.
func TestZeroRoutingIsOmittedFromTheRequest(t *testing.T) {
	blob, err := json.Marshal(RespondRequest{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(blob), "routing") {
		t.Errorf("an unset routing preference reached the wire: %s", blob)
	}

	r := Routing{Privacy: PrivacyZDR}
	blob, err = json.Marshal(RespondRequest{Routing: &r})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(blob), `"routing":{"privacy":"zdr"}`) {
		t.Errorf("a set preference did not serialize as expected: %s", blob)
	}
	// The unset halves stay out — `omitempty` on each field, so a privacy-only
	// preference does not also assert a ranking the user never chose.
	if strings.Contains(string(blob), "sort") || strings.Contains(string(blob), "only") {
		t.Errorf("unset routing fields leaked onto the wire: %s", blob)
	}
}

// IsDefault decides whether the posture is announced in the masthead. An explicit
// setting that happens to match the default is not news; a deviation always is.
func TestRoutingIsDefault(t *testing.T) {
	for _, r := range []Routing{
		{},
		{Privacy: PrivacyNoTraining},
		{Sort: SortThroughput},
		{Privacy: PrivacyNoTraining, Sort: SortThroughput},
	} {
		if !r.IsDefault() {
			t.Errorf("%+v should read as the default posture", r)
		}
	}
	for _, r := range []Routing{
		{Privacy: PrivacyZDR},
		{Sort: SortPrice},
		{Sort: SortLatency},
		{Only: []string{"deepinfra"}},
		{Ignore: []string{"deepinfra"}},
	} {
		if r.IsDefault() {
			t.Errorf("%+v is a deviation and must be announced", r)
		}
	}
}

// The privacy copy is the one thing a client must NOT invent. "does not collect or train
// on" and "does not retain" are different promises, and only the first holds under the
// default mode — so the served description wins, and the fallback is held to the same
// standard.
func TestPrivacyDescriptionPrefersTheServedWording(t *testing.T) {
	caps := &Capabilities{}
	caps.Routing.PrivacyMode = PrivacyNoTraining
	caps.Routing.PrivacyDescription = "SERVED WORDING"

	if got := PrivacyDescription(PrivacyNoTraining, caps); got != "SERVED WORDING" {
		t.Errorf("the served description was ignored: %q", got)
	}
	// A mode the server is not describing must fall back rather than mislabel the
	// server's sentence as covering it.
	if got := PrivacyDescription(PrivacyZDR, caps); got == "SERVED WORDING" {
		t.Error("the served no_training wording was reused for zdr")
	}

	// The fallbacks: never "store"/"retain" for no_training, always for zdr.
	noTraining := PrivacyDescription(PrivacyNoTraining, nil)
	if strings.Contains(noTraining, "retention") && !strings.Contains(noTraining, "not enforced") {
		t.Errorf("the no-training copy implies retention is covered: %q", noTraining)
	}
	if !strings.Contains(noTraining, "collect or train") {
		t.Errorf("the no-training copy does not state what it actually guarantees: %q", noTraining)
	}
	if !strings.Contains(PrivacyDescription(PrivacyZDR, nil), "zero data retention") {
		t.Errorf("the zdr copy does not mention retention: %q", PrivacyDescription(PrivacyZDR, nil))
	}
}

func TestParseEndpointList(t *testing.T) {
	got := ParseEndpointList(" deepinfra, together-ai ,, ")
	if len(got) != 2 || got[0] != "deepinfra" || got[1] != "together-ai" {
		t.Errorf("ParseEndpointList = %#v, want [deepinfra together-ai]", got)
	}
	if ParseEndpointList("") != nil {
		t.Error("an empty value should produce no endpoints, not an empty-string entry")
	}
}

// The policy must ride BOTH endpoints. A task sends the caller's content upstream exactly
// as a turn does — terminal tails, transcripts, memories — so a privacy choice honoured
// only on `/respond` would be kept precisely where the user can see it and dropped
// everywhere else. That is the failure this test exists to prevent.
func TestRoutingReachesBothRespondAndTasks(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]*Routing{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/respond"):
			var req RespondRequest
			_ = json.Unmarshal(body, &req)
			seen["respond"] = req.Routing
		case strings.HasSuffix(r.URL.Path, "/tasks"):
			var req TaskRequest
			_ = json.Unmarshal(body, &req)
			seen["task"] = req.Routing
		}
		mu.Unlock()

		if strings.HasSuffix(r.URL.Path, "/respond") {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: meta\ndata: {}\n\n"+
				"event: delta\ndata: {\"content\":\"hi\"}\n\n"+
				"event: done\ndata: {\"finish_reason\":\"stop\"}\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"t","object":"daintree.task.result","task":"checkpoint",`+
			`"model":"m","output":{},"finish_reason":"stop","usage":{},"prompt_version":"v"}`)
	}))
	t.Cleanup(srv.Close)

	want := Routing{Privacy: PrivacyZDR, Sort: SortPrice, Only: []string{"deepinfra"}}
	c := NewClient(ClientConfig{
		BaseURL:           srv.URL,
		RoutingPreference: func() Routing { return want },
	})

	if _, err := c.RespondStream(context.Background(), RespondRequest{}, StreamCallbacks{}); err != nil {
		t.Fatalf("RespondStream: %v", err)
	}
	if _, err := c.RunTask(context.Background(), TaskRequest{Task: "checkpoint"}); err != nil {
		t.Fatalf("RunTask: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, endpoint := range []string{"respond", "task"} {
		got := seen[endpoint]
		if got == nil {
			t.Errorf("%s received NO routing block — its content goes upstream under the server default", endpoint)
			continue
		}
		if got.Privacy != PrivacyZDR || got.Sort != SortPrice || len(got.Only) != 1 || got.Only[0] != "deepinfra" {
			t.Errorf("%s received %+v, want %+v", endpoint, *got, want)
		}
	}
}

// No preference configured ⇒ no block on either endpoint, so the server default applies.
// Sending an empty object would be a different statement on the wire.
func TestNoRoutingPreferenceSendsNoBlock(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(body))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"t","object":"daintree.task.result","task":"checkpoint",`+
			`"model":"m","output":{},"finish_reason":"stop","usage":{},"prompt_version":"v"}`)
	}))
	t.Cleanup(srv.Close)

	// Both an unset hook and a hook returning the zero value must omit the block.
	for _, cfg := range []ClientConfig{
		{BaseURL: srv.URL},
		{BaseURL: srv.URL, RoutingPreference: func() Routing { return Routing{} }},
	} {
		if _, err := NewClient(cfg).RunTask(context.Background(), TaskRequest{Task: "checkpoint"}); err != nil {
			t.Fatalf("RunTask: %v", err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for i, body := range bodies {
		if strings.Contains(body, "routing") {
			t.Errorf("request %d carried a routing block with no preference set: %s", i, body)
		}
	}
}
