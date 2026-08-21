package ui

import (
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/domain"
)

// Since the sign-in went away, the masthead is the ONLY place a non-default backend is
// stated — and it is the permanent record, so a pasted transcript says which backend
// answered. The deployed default stays silent: a line that is identical on every launch
// is a line that stops being read.
func TestMastheadNamesOnlyANonDefaultBackend(t *testing.T) {
	th := darkTheme()

	if got := mastheadBackend(backend.DefaultBaseURL); got != "" {
		t.Errorf("the deployed default must be silent, got %q", got)
	}
	if got := mastheadBackend(""); got != "" {
		t.Errorf("an unset endpoint must be silent, got %q", got)
	}
	// The local backend is NAMED, not just addressed — "local" is what an operator calls
	// it and reads at a glance where a host:port does not.
	local := mastheadBackend(backend.LocalBaseURL)
	if !strings.Contains(local, "local") || !strings.Contains(local, backend.LocalBaseURL) {
		t.Errorf("local should be named AND addressed, got %q", local)
	}
	// A credential in the endpoint must never reach the host's native scrollback, which
	// the cockpit never clears. Neither DAINTREE_BACKEND_URL nor --backend-url is
	// normalized, so this is the only thing between userinfo and a permanent record.
	if got := mastheadBackend("https://user:supersecret@backend.example"); strings.Contains(got, "supersecret") {
		t.Errorf("userinfo leaked into the masthead: %q", got)
	}

	out := renderMasthead(th, mastheadParams{Version: "1.2.3", Tier: domain.TierOperator, Backend: local}, 100)
	if !strings.Contains(out, "backend ") || !strings.Contains(out, backend.LocalBaseURL) {
		t.Errorf("the rendered masthead should carry the backend row, got:\n%s", out)
	}
	plain := renderMasthead(th, mastheadParams{Version: "1.2.3", Tier: domain.TierOperator}, 100)
	if strings.Contains(plain, "backend ") {
		t.Errorf("no backend row belongs on the default endpoint, got:\n%s", plain)
	}
}
