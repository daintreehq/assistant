package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/models"
)

// varyingRouter requests the same tool with DIFFERENT args each round, then stops.
// No (tool,args,err) signature repeats, so the breaker must NOT fire.
type varyingRouter struct{ round int }

func (r *varyingRouter) Stream(ctx context.Context, tier domain.ModelTier, opts models.ChatOptions, onToken func(string)) (models.ChatResult, error) {
	r.round++
	if r.round >= 4 {
		return models.ChatResult{Content: "done"}, nil
	}
	args := `{"attempt":` + itoa(r.round) + `}`
	return models.ChatResult{ToolCalls: []models.ToolCallRequest{toolCall("call_"+itoa(r.round), "watcher__terminal__create", args)}}, nil
}
func (r *varyingRouter) Chat(ctx context.Context, tier domain.ModelTier, opts models.ChatOptions) (models.ChatResult, error) {
	return models.ChatResult{Content: "S"}, nil
}
func (r *varyingRouter) ModelFor(domain.ModelTier) string { return "minimax-m3" }
func (r *varyingRouter) FlushMeter() []models.TierUsage   { return nil }

func TestBreakerDoesNotTripWhenArgsVary(t *testing.T) {
	// Each call fails (the tool always fails) but with distinct args, so the
	// signature never repeats — genuine progress, the breaker stays silent and the
	// turn ends naturally when the model stops requesting tools.
	tools := &fakeTools{result: domain.Fail("BOOM", "it broke")}
	r := &varyingRouter{}
	s := NewSession(baseDeps(r, tools))
	reply, err := s.Send(context.Background(), "attach a watcher", SendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if reply != "done" {
		t.Fatalf("reply = %q want done (breaker must not trip on varying args)", reply)
	}
	if r.round != 4 {
		t.Fatalf("stream rounds = %d want 4", r.round)
	}
}

func TestBreakerWarnsAtSecondIdenticalFailure(t *testing.T) {
	// Two identical failures (warn threshold) inject a one-time system nudge into
	// history telling the model to change arguments — without aborting the turn yet.
	failing := domain.Fail("INVALID_ARGS", "bad shape")
	tools := &fakeTools{result: failing}
	rounds := make([]models.ChatResult, 6)
	for i := range rounds {
		rounds[i] = models.ChatResult{ToolCalls: []models.ToolCallRequest{toolCall("c", "fs__read", `{"path":"x"}`)}}
	}
	r := &fakeRouter{results: rounds}
	s := NewSession(baseDeps(r, tools))
	if _, err := s.Send(context.Background(), "go", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	var nudged bool
	for _, m := range s.Messages() {
		if m.Role == "user" && strings.Contains(m.StringContent, "the same arguments") &&
			strings.Contains(m.StringContent, "INVALID_ARGS") {
			nudged = true
		}
	}
	if !nudged {
		t.Fatal("expected a one-time system nudge after the second identical failure")
	}
}

// TestBreakerTripsWhenOnlyKeyOrderVaries proves the circuit breaker groups
// semantically-identical calls: the model re-emits the SAME failing call with
// reordered JSON keys each round, which must share one signature and trip the
// breaker, not slip past it as "varying args".
func TestBreakerTripsWhenOnlyKeyOrderVaries(t *testing.T) {
	failing := domain.Fail("INVALID_ARGS", "bad shape")
	tools := &fakeTools{result: failing}
	// Same {path, mode} object, keys in a different order each round.
	argVariants := []string{
		`{"path":"x","mode":"r"}`,
		`{"mode":"r","path":"x"}`,
		`{"path":"x",  "mode":"r"}`,
		`{"mode":"r","path":"x"}`,
		`{"path":"x","mode":"r"}`,
		`{"mode":"r","path":"x"}`,
	}
	rounds := make([]models.ChatResult, len(argVariants))
	for i, a := range argVariants {
		rounds[i] = models.ChatResult{ToolCalls: []models.ToolCallRequest{toolCall("c", "fs__read", a)}}
	}
	r := &fakeRouter{results: rounds}
	s := NewSession(baseDeps(r, tools))
	reply, err := s.Send(context.Background(), "go", SendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "fs.read") || !strings.Contains(reply, "identical arguments") {
		t.Fatalf("breaker must abort the turn once the key-reordered repeats hit the limit; reply = %q", reply)
	}
}

func TestCanonicalJSON_NormalizesKeyOrderAndWhitespace(t *testing.T) {
	a := canonicalJSON(`{"path":"x","mode":"r"}`)
	b := canonicalJSON(`{"mode":"r","path":"x"}`)
	c := canonicalJSON(`{ "path" : "x" ,  "mode":"r" }`)
	if a != b || a != c {
		t.Fatalf("key order / whitespace must canonicalize to one form: %q %q %q", a, b, c)
	}
	// Non-JSON and empty pass through unchanged.
	if got := canonicalJSON("not json"); got != "not json" {
		t.Fatalf("non-JSON must pass through: %q", got)
	}
	if got := canonicalJSON(""); got != "" {
		t.Fatalf("empty must stay empty: %q", got)
	}
	// Distinct content must NOT collide.
	if canonicalJSON(`{"path":"x"}`) == canonicalJSON(`{"path":"y"}`) {
		t.Fatal("distinct args must not canonicalize to the same form")
	}
}
