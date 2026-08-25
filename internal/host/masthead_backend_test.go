package host

import (
	"testing"

	"github.com/daintreehq/assistant/internal/backend"
)

// The masthead names the endpoint every time, including the deployed one.
//
// It used to announce only a deviation, on the reasoning that the deployed backend is
// what every install talks to. That reasoning inverted when the endpoint became the
// session's own: the deployed backend is now what an unconfigured install ARRIVES at,
// and it is the one that sends the conversation off the machine. Announcing only the
// exception made the two cases identical on screen — and the silent one was the one
// that left the box.
func TestMastheadNamesEveryBackendIncludingTheDeployedOne(t *testing.T) {
	deployed := mastheadBackend(backend.DefaultBaseURL)
	if deployed == "" {
		t.Fatal("the deployed backend rendered as an empty masthead field — a session shipping the conversation off-box would look identical to one talking to localhost")
	}

	local := mastheadBackend(backend.LocalBaseURL)
	if local == "" {
		t.Fatal("the local backend rendered as an empty masthead field")
	}

	// The point of naming both is that a reader can TELL them apart. Equal strings
	// would satisfy "non-empty" while answering none of the question.
	if deployed == local {
		t.Fatalf("the deployed and local backends render identically as %q", deployed)
	}
}

// An empty endpoint is not "no backend" — the client resolves it to the deployed
// default downstream (backend.NewClient). The masthead has to report where turns will
// actually go, not the absence it was handed.
func TestMastheadResolvesAnEmptyEndpointTheWayTheClientDoes(t *testing.T) {
	if got, want := mastheadBackend(""), mastheadBackend(backend.DefaultBaseURL); got != want {
		t.Fatalf("empty endpoint rendered as %q, want the deployed default %q", got, want)
	}
	if got := mastheadBackend("   "); got != mastheadBackend(backend.DefaultBaseURL) {
		t.Fatalf("whitespace-only endpoint rendered as %q, want the deployed default", got)
	}
}
