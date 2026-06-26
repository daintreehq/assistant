package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// run_test.go locks the first-run/CLI-discoverability contract: the `doctor`
// subcommand exits non-zero when the CLI's model gateway (the Daintree backend) is
// UNREACHABLE — so scripts and CI can gate on it — and exits Success when the backend
// is healthy even with MCP disconnected (a valid degraded local mode). Each test points
// the state dir at a temp dir and the backend at a controlled URL via
// DAINTREE_BACKEND_URL, so no real network or ~/.daintree state is touched. The env
// override means these tests must NOT run in parallel.

// doctorOpts builds Options that resolve to an isolated App. The backend URL is
// controlled by the caller via t.Setenv before invoking.
func doctorOpts(t *testing.T) Options {
	t.Helper()
	t.Setenv("DAINTREE_ASSISTANT_STATE_DIR", t.TempDir())
	return Options{Offline: true, Project: t.TempDir()}
}

// deadBackendURL returns a URL guaranteed to refuse connections: an httptest server
// started then immediately closed, freeing its port.
func deadBackendURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	return url
}

// healthyBackendURL starts a minimal backend answering /healthz ok (cleaned up via t).
func healthyBackendURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"ok"}`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// With the backend UNREACHABLE, `doctor` must exit Error so scripts/CI can gate on it.
func TestRunDoctor_BackendUnreachableReturnsError(t *testing.T) {
	t.Setenv("DAINTREE_BACKEND_URL", deadBackendURL(t))
	opts := doctorOpts(t)
	if code := RunDoctor(context.Background(), opts); code != domain.OneShotExitCode.Error {
		t.Fatalf("doctor with an unreachable backend must exit Error(%d), got %d", domain.OneShotExitCode.Error, code)
	}
}

// With the backend reachable, `doctor` exits Success even though MCP is not connected —
// a disconnected MCP is a valid degraded local mode, not a doctor failure.
func TestRunDoctor_BackendReachableReturnsSuccess(t *testing.T) {
	t.Setenv("DAINTREE_BACKEND_URL", healthyBackendURL(t))
	opts := doctorOpts(t)
	if code := RunDoctor(context.Background(), opts); code != domain.OneShotExitCode.Success {
		t.Fatalf("doctor with a healthy backend (MCP disconnected) must exit Success(%d), got %d", domain.OneShotExitCode.Success, code)
	}
}
