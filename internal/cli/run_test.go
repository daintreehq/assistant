package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/app"
	"github.com/daintreehq/daintree-assistant/internal/backend"
	"github.com/daintreehq/daintree-assistant/internal/cli/render"
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
	// Force the workflow-intelligence flag OFF: with it ambiently on, CheckTasks
	// would additionally require the three workflow ids and the core-only fixtures
	// below would fail for an environment reason, not a code one.
	t.Setenv("DAINTREE_WORKFLOW_INTELLIGENCE", "0")
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

// The interactive stale-schema handler AUTHORISES the reset without reading stdin: a
// stale on-disk DB in this pre-release, single-baseline product has one sensible
// recovery, so the launch is never blocked on a y/N whose answer is always "yes". It
// must still leave an honest trace (the two schema numbers) that local state was cleared.
func TestSchemaAutoReset_AuthorisesAndNotes(t *testing.T) {
	var buf bytes.Buffer
	reset, err := schemaAutoReset(render.New(&buf))(8, 10)
	if err != nil {
		t.Fatalf("schemaAutoReset must not error, got %v", err)
	}
	if !reset {
		t.Fatal("schemaAutoReset must authorise the reset (return true)")
	}
	out := buf.String()
	for _, want := range []string{strconv.Itoa(8), strconv.Itoa(10)} {
		if !strings.Contains(out, want) {
			t.Fatalf("reset notice must mention schema %s; got %q", want, out)
		}
	}
}

// A cancelled launch context is a shutdown request, not evidence that the cockpit
// is unavailable. In particular, SIGTERM cancels main's launch context and Bubble
// Tea returns an error; the CLI must exit with the cancelled code instead of falling
// through to startRepl, which intentionally detaches itself from that context.
func TestRunInteractive_CancelledCockpitDoesNotFallBack(t *testing.T) {
	t.Setenv("DAINTREE_ASSISTANT_STATE_DIR", t.TempDir())
	t.Setenv(NoDaemonEnv, "1")

	ctx, cancel := context.WithCancel(context.Background())
	runnerCalled := false
	opts := Options{
		Offline: true,
		Project: t.TempDir(),
		Cockpit: func(context.Context, *app.App) error {
			runnerCalled = true
			cancel()
			return context.Canceled
		},
	}

	if code := runInteractive(ctx, opts, true); code != domain.OneShotExitCode.Cancelled {
		t.Fatalf("cancelled cockpit exit = %d, want Cancelled(%d)", code, domain.OneShotExitCode.Cancelled)
	}
	if !runnerCalled {
		t.Fatal("cockpit seam was not called")
	}
}

// A real cockpit startup failure keeps the established classic-REPL fallback. Feed
// that REPL an immediate EOF so the unit test exercises the branch without blocking.
func TestRunInteractive_CockpitUnavailableStillFallsBack(t *testing.T) {
	t.Setenv("DAINTREE_ASSISTANT_STATE_DIR", t.TempDir())
	t.Setenv(NoDaemonEnv, "1")

	stdin, stdinWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := stdinWriter.Close(); err != nil {
		t.Fatal(err)
	}
	originalStdin := os.Stdin
	os.Stdin = stdin
	t.Cleanup(func() {
		os.Stdin = originalStdin
		_ = stdin.Close()
	})

	runnerCalled := false
	opts := Options{
		Offline: true,
		Project: t.TempDir(),
		Cockpit: func(context.Context, *app.App) error {
			runnerCalled = true
			return errors.New("cockpit unavailable in test")
		},
	}

	if code := runInteractive(context.Background(), opts, true); code != domain.OneShotExitCode.Success {
		t.Fatalf("fallback REPL exit = %d, want Success(%d)", code, domain.OneShotExitCode.Success)
	}
	if !runnerCalled {
		t.Fatal("cockpit seam was not called")
	}
}

// capsBackendURL starts a backend answering /healthz AND /v1/daintree/capabilities,
// advertising exactly the task ids given.
func capsBackendURL(t *testing.T, taskIDs []string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"ok"}`)
		case "/v1/daintree/capabilities":
			body, _ := json.Marshal(backend.Capabilities{
				Protocol: backend.ProtocolRange{Min: backend.ProtocolVersion, Max: backend.ProtocolVersion},
				Tasks:    taskIDs,
			})
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// A backend that advertises every required task id exits Success.
func TestRunDoctor_TasksPresentReturnsSuccess(t *testing.T) {
	t.Setenv("DAINTREE_BACKEND_URL", capsBackendURL(t, backend.CoreTaskIDs()))
	if code := RunDoctor(context.Background(), doctorOpts(t)); code != domain.OneShotExitCode.Success {
		t.Fatalf("doctor with a complete task inventory must exit Success(%d), got %d", domain.OneShotExitCode.Success, code)
	}
}

// Task-ID DRIFT must exit Error. This reproduces the 2026-07-07 incident exactly:
// the backend served the same NUMBER of tasks under renamed ids (a `.v1` suffix was
// dropped), so every count-based check passed while every task call 404'd at
// runtime. `doctor` is the surface that must catch this before a turn does.
func TestRunDoctor_TaskIDDriftReturnsError(t *testing.T) {
	var renamed []string
	for _, id := range backend.CoreTaskIDs() {
		renamed = append(renamed, id+".v1")
	}
	t.Setenv("DAINTREE_BACKEND_URL", capsBackendURL(t, renamed))
	if code := RunDoctor(context.Background(), doctorOpts(t)); code != domain.OneShotExitCode.Error {
		t.Fatalf("doctor must exit Error(%d) on task-id drift, got %d", domain.OneShotExitCode.Error, code)
	}
}

// A backend that answers capabilities successfully but advertises NO tasks is
// BROKEN, not merely unverifiable, and must fail. require_ready raises 503 before
// the capabilities handler runs, and a 200 always fills `tasks` from
// task_runner.task_ids() — so an empty list means the registry is empty and every
// task call will 404.
func TestRunDoctor_EmptyTaskInventoryFails(t *testing.T) {
	t.Setenv("DAINTREE_BACKEND_URL", capsBackendURL(t, nil))
	if code := RunDoctor(context.Background(), doctorOpts(t)); code != domain.OneShotExitCode.Error {
		t.Fatalf("an empty task inventory must fail doctor, got %d", code)
	}
}

// The genuine "cannot verify" case is a capabilities FETCH error (an older backend
// that 404s the endpoint). That must NOT fail doctor — it is covered by
// TestRunDoctor_BackendReachableReturnsSuccess, whose stub serves only /healthz.
