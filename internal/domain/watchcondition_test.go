package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

func ptrStr(v string) *string           { return &v }
func ptrI64(v int64) *int64             { return &v }
func ptrState(v AgentState) *AgentState { return &v }

// WatchCondition is a discriminated union with EXACTLY-ONE-KEY semantics, and the
// guards below exist because a degenerate condition produces FALSE SUPERVISION: the
// human believes a terminal is being watched when the watcher can never fire (or
// fires instantly on everything). The schema advertises the rule via
// minProperties/maxProperties, but the schema is advisory — this validator is the
// enforcement point, so every guard is pinned.
func TestWatchConditionValidate(t *testing.T) {
	cases := []struct {
		name    string
		cond    WatchCondition
		wantErr string // "" = must validate; otherwise a substring of the error
	}{
		// --- valid leaves ---
		{"stateIs completed", WatchCondition{StateIs: ptrState(AgentCompleted)}, ""},
		{"runtimeStatusIs running", WatchCondition{RuntimeStatusIs: ptrStr("running")}, ""},
		{"runtimeStatusIs exited", WatchCondition{RuntimeStatusIs: ptrStr("exited")}, ""},
		{"contains", WatchCondition{Contains: ptrStr("BUILD OK")}, ""},
		{"regex", WatchCondition{Regex: ptrStr(`^done \d+$`)}, ""},
		{"noOutputForMs positive", WatchCondition{NoOutputForMs: ptrI64(1)}, ""},
		{"modelJudge", WatchCondition{ModelJudge: ptrStr("has the agent finished?")}, ""},

		// --- leaf guards ---
		{"stateIs rejects an unknown state", WatchCondition{StateIs: ptrState("bogus")}, "invalid stateIs"},
		{"stateIs rejects empty", WatchCondition{StateIs: ptrState("")}, "invalid stateIs"},
		{"runtimeStatusIs rejects other values", WatchCondition{RuntimeStatusIs: ptrStr("idle")}, "running|exited"},
		{
			// A blank contains matches everything and would fire on any output.
			"contains rejects empty", WatchCondition{Contains: ptrStr("")}, "contains must be non-empty",
		},
		{"contains rejects whitespace-only", WatchCondition{Contains: ptrStr("   \t\n ")}, "contains must be non-empty"},
		{"regex rejects empty", WatchCondition{Regex: ptrStr("")}, "regex must be non-empty"},
		{
			// An invalid regex would silently never match — worse than an error.
			"regex rejects one that does not compile", WatchCondition{Regex: ptrStr("a(b")}, "does not compile",
		},
		{
			// A non-positive timeout fires immediately.
			"noOutputForMs rejects zero", WatchCondition{NoOutputForMs: ptrI64(0)}, "positive integer",
		},
		{"noOutputForMs rejects negative", WatchCondition{NoOutputForMs: ptrI64(-1)}, "positive integer"},
		{"modelJudge rejects whitespace-only", WatchCondition{ModelJudge: ptrStr("  ")}, "modelJudge must be non-empty"},

		// --- the exactly-one-key rule ---
		{"no key present is rejected", WatchCondition{}, "no variant key present"},
		{
			"two leaves are rejected",
			WatchCondition{StateIs: ptrState(AgentCompleted), Contains: ptrStr("done")},
			"exactly one variant key",
		},
		{
			"a leaf plus a combinator is rejected",
			WatchCondition{Contains: ptrStr("done"), All: []WatchCondition{{Contains: ptrStr("x")}}},
			"exactly one variant key",
		},
		{
			"two combinators are rejected",
			WatchCondition{
				All: []WatchCondition{{Contains: ptrStr("x")}},
				Any: []WatchCondition{{Contains: ptrStr("y")}},
			},
			"exactly one variant key",
		},

		// --- combinators ---
		{"all with one member", WatchCondition{All: []WatchCondition{{Contains: ptrStr("x")}}}, ""},
		{
			"all with several members",
			WatchCondition{All: []WatchCondition{
				{StateIs: ptrState(AgentWaiting)},
				{ModelJudge: ptrStr("is it really done?")},
			}},
			"",
		},
		{"any with one member", WatchCondition{Any: []WatchCondition{{Regex: ptrStr("ok")}}}, ""},
		{"not wrapping a valid leaf", WatchCondition{Not: &WatchCondition{Contains: ptrStr("error")}}, ""},
		{
			// An empty combinator can never be satisfied (all) or can never fire (any).
			"all rejects an empty member list", WatchCondition{All: []WatchCondition{}}, "all must have at least one member",
		},
		{"any rejects an empty member list", WatchCondition{Any: []WatchCondition{}}, "any must have at least one member"},

		// --- recursion: a degenerate member must fail the whole tree ---
		{
			"all rejects an invalid member",
			WatchCondition{All: []WatchCondition{{Contains: ptrStr("ok")}, {NoOutputForMs: ptrI64(0)}}},
			"positive integer",
		},
		{
			"any rejects an empty member object",
			WatchCondition{Any: []WatchCondition{{}}},
			"no variant key present",
		},
		{
			"not rejects an empty inner object",
			WatchCondition{Not: &WatchCondition{}},
			"no variant key present",
		},
		{
			"nesting is validated all the way down",
			WatchCondition{All: []WatchCondition{
				{Any: []WatchCondition{
					{Not: &WatchCondition{Regex: ptrStr("a(b")}},
				}},
			}},
			"does not compile",
		},
		{
			"a deep valid tree validates",
			WatchCondition{All: []WatchCondition{
				{StateIs: ptrState(AgentWaiting)},
				{Not: &WatchCondition{Any: []WatchCondition{
					{Contains: ptrStr("panic")},
					{Regex: ptrStr(`(?i)fatal`)},
				}}},
			}},
			"",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cond := tc.cond
			err := cond.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want an error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() = %q, want it to contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// UnmarshalJSON must run the SAME guards as Validate, so a degenerate condition can
// never enter the system through the decode path (which is how every model-authored
// condition arrives). A watcher built from JSON that skipped validation is exactly
// the false-supervision failure the guards exist to prevent.
func TestWatchConditionUnmarshalJSONValidates(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{"valid leaf decodes", `{"stateIs":"completed"}`, ""},
		{"valid nested tree decodes", `{"all":[{"stateIs":"waiting"},{"modelJudge":"done?"}]}`, ""},
		{"empty object is rejected", `{}`, "no variant key present"},
		{"two keys are rejected", `{"stateIs":"completed","contains":"done"}`, "exactly one variant key"},
		{"blank contains is rejected", `{"contains":"  "}`, "contains must be non-empty"},
		{"zero noOutputForMs is rejected", `{"noOutputForMs":0}`, "positive integer"},
		{"bad regex is rejected", `{"regex":"a(b"}`, "does not compile"},
		{"unknown stateIs is rejected", `{"stateIs":"finished"}`, "invalid stateIs"},
		{"empty all is rejected", `{"all":[]}`, "all must have at least one member"},
		{"a degenerate NESTED member is rejected", `{"any":[{"contains":"ok"},{}]}`, "no variant key present"},
		{"a degenerate member under not is rejected", `{"not":{"regex":""}}`, "regex must be non-empty"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cond WatchCondition
			err := json.Unmarshal([]byte(tc.raw), &cond)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Unmarshal(%s) = %v, want nil", tc.raw, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Unmarshal(%s) = nil, want an error containing %q", tc.raw, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Unmarshal(%s) = %q, want it to contain %q", tc.raw, err.Error(), tc.wantErr)
			}
		})
	}
}

// A condition that round-trips through JSON must survive re-validation, and the
// omitempty tags must not drop a load-bearing key. This is the guard against a
// persisted watcher (conditions are stored as JSON in SQLite) decoding back into a
// DIFFERENT condition than the one that was validated on create.
func TestWatchConditionRoundTrip(t *testing.T) {
	original := WatchCondition{All: []WatchCondition{
		{StateIs: ptrState(AgentWaiting)},
		{Not: &WatchCondition{Any: []WatchCondition{
			{Contains: ptrStr("panic")},
			{NoOutputForMs: ptrI64(30_000)},
		}}},
	}}
	if err := original.Validate(); err != nil {
		t.Fatalf("fixture is invalid: %v", err)
	}

	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded WatchCondition
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("Unmarshal(%s): %v", encoded, err)
	}

	reEncoded, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("re-Marshal: %v", err)
	}
	if string(encoded) != string(reEncoded) {
		t.Fatalf("round trip changed the condition:\n  before %s\n  after  %s", encoded, reEncoded)
	}
	if !decoded.IsComposite() {
		t.Fatal("decoded tree lost its combinator (IsComposite = false)")
	}
}

// IsComposite drives whether a caller descends the tree; a wrong answer either skips
// evaluation of a combinator or pointlessly recurses a leaf. It must also be
// nil-safe — callers hold *WatchCondition fields that are legitimately absent.
func TestWatchConditionIsComposite(t *testing.T) {
	var nilCond *WatchCondition
	if nilCond.IsComposite() {
		t.Fatal("nil condition reported composite")
	}

	cases := []struct {
		name string
		cond WatchCondition
		want bool
	}{
		{"leaf is not composite", WatchCondition{Contains: ptrStr("x")}, false},
		{"zero value is not composite", WatchCondition{}, false},
		{"all is composite", WatchCondition{All: []WatchCondition{{Contains: ptrStr("x")}}}, true},
		{"any is composite", WatchCondition{Any: []WatchCondition{{Contains: ptrStr("x")}}}, true},
		{"not is composite", WatchCondition{Not: &WatchCondition{Contains: ptrStr("x")}}, true},
		{"empty all slice is not composite (len 0)", WatchCondition{All: []WatchCondition{}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cond := tc.cond
			if got := cond.IsComposite(); got != tc.want {
				t.Fatalf("IsComposite() = %v, want %v", got, tc.want)
			}
		})
	}
}
