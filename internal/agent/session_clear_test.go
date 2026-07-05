package agent

import (
	"context"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/models"
)

// TestClearResetsBackendStateToken guards a real bug: /clear must start a BRAND-NEW
// chat, so the opaque backend skill-state token cannot survive it. Left set, the first
// turn after /clear replays the pre-clear token and the backend's stateful skill
// selector treats the fresh chat as a continuation — it never re-injects the runbook,
// so a skill-shaped task (multi-agent orchestration) starts with no skill and the
// model does nothing.
func TestClearResetsBackendStateToken(t *testing.T) {
	// The fake backend stamps every meta with State="dst1.test", so turn 1 populates the
	// session's backend state token.
	rec := &recordingBackend{backendFromRouter: backendFromRouter{r: &fakeRouter{
		results: []models.ChatResult{{Content: "one"}, {Content: "two"}},
	}}}
	deps := baseDeps(&fakeRouter{}, &fakeTools{result: domain.Ok("ok", nil)})
	deps.Backend = rec
	s := NewSession(deps)

	if _, err := s.Send(context.Background(), "hello", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Clear(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Send(context.Background(), "again", SendOptions{}); err != nil {
		t.Fatal(err)
	}

	reqs := rec.requests()
	if len(reqs) < 2 {
		t.Fatalf("want at least 2 recorded requests, got %d", len(reqs))
	}
	// The last request is the FIRST turn after /clear — it must carry no state token.
	if last := reqs[len(reqs)-1]; last.State != nil {
		t.Fatalf("after /clear the request must carry NO backend state token (fresh chat), got %q", *last.State)
	}
}
