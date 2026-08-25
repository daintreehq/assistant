package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/cli/render"
	"github.com/daintreehq/assistant/internal/domain"
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
	return Options{Offline: boolPtr(true), Project: t.TempDir()}
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

// healthyBackendURL starts a minimal backend answering /health ok (cleaned up via t).
func healthyBackendURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
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

// The interactive route is now the line REPL unconditionally — there is no second front end
// to fall back FROM. Feed the REPL an immediate EOF so the unit test exercises
// the route without blocking.
func TestRunInteractive_RunsTheLineRepl(t *testing.T) {
	t.Setenv("DAINTREE_ASSISTANT_STATE_DIR", t.TempDir())
	t.Setenv("DAINTREE_API_KEY", "test-key")
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

	opts := Options{
		Offline: boolPtr(true),
		Project: t.TempDir(),
	}

	// ttyOK=true is the branch that used to select the attached session; it must now reach the
	// REPL like any other, so a TTY launch cannot resurrect a front end that is gone.
	if code := runInteractive(context.Background(), opts, true); code != domain.OneShotExitCode.Success {
		t.Fatalf("interactive exit = %d, want Success(%d)", code, domain.OneShotExitCode.Success)
	}
}

// capsBackendURL starts a backend answering /health AND /v1/daintree/capabilities,
// advertising exactly the task ids given.
func capsBackendURL(t *testing.T, taskIDs []string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
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
// TestRunDoctor_BackendReachableReturnsSuccess, whose stub serves only /health.

// authRejectingBackendURL answers /health but 401s every authenticated route — which is
// exactly what a backend predating this build does when the CLI sends no key at all.
func authRejectingBackendURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"ok"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"code":"invalid_api_key","message":"Invalid or missing API key."}}`)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// Having no key is the healthy state now, so doctor must not carry a "signed in" row —
// a red line on every install would train people to ignore the whole report.
func TestDoctorHasNoSignedInCheck(t *testing.T) {
	t.Setenv("DAINTREE_BACKEND_URL", healthyBackendURL(t))
	report, err := buildDoctorReport(context.Background(), doctorOpts(t))
	if err != nil {
		t.Fatalf("buildDoctorReport: %v", err)
	}
	for _, c := range report.Checks {
		if strings.Contains(strings.ToLower(c.Label), "signed in") || c.ID == "auth.signedIn" {
			t.Fatalf("doctor still reports a sign-in row: %+v", c)
		}
		// The bearer row is reported ONLY when one is actually being sent. doctorOpts
		// sets no key, so its presence here would mean the row fires on every install.
		if c.ID == "auth.bearer" {
			t.Fatalf("the bearer row must stay hidden when no key is set: %+v", c)
		}
	}
}

// The failure this replaced the sign-in gate with. A backend that refuses the request at
// its own door cannot serve a single turn, so reporting it as "could not check" would
// have doctor conclude "no blocking problems" for an install that does not work at all.
func TestDoctorFailsWhenTheBackendRejectsTheRequestOutright(t *testing.T) {
	t.Setenv("DAINTREE_BACKEND_URL", authRejectingBackendURL(t))
	report, err := buildDoctorReport(context.Background(), doctorOpts(t))
	if err != nil {
		t.Fatalf("buildDoctorReport: %v", err)
	}
	var found *DoctorCheck
	for i := range report.Checks {
		if report.Checks[i].ID == "auth.credentialUsable" {
			found = &report.Checks[i]
		}
	}
	if found == nil {
		t.Fatal("no upstream-credential row; a reachable backend must always be probed")
	}
	if found.Status != StatusFail {
		t.Fatalf("a 401 at the backend's own door must FAIL, got %s (%s)", found.Status, found.Detail)
	}
	// The hint has to name the right culprit. With no key of our own, re-checking a
	// credential the user never supplied is not an action they can take.
	if !strings.Contains(found.Hint, "DAINTREE_BACKEND_URL") {
		t.Errorf("the hint must point at the endpoint, not at a key the user never set: %q", found.Hint)
	}
	if code := RunDoctor(context.Background(), doctorOpts(t)); code != domain.OneShotExitCode.Error {
		t.Fatalf("doctor must exit Error(%d) against a backend that refuses it, got %d", domain.OneShotExitCode.Error, code)
	}
}

// The same row, told whose credential it just reported on — which is the DEPLOYMENT's,
// always.
//
// The row reports on the credential the backend would spend, and it spends its own on
// every install. A caller-supplied bearer (DAINTREE_API_KEY / --api-key-file) identifies
// the ACCOUNT and is never sent upstream, so describing the verified credential as
// "yours" whenever one was set was a straightforward misattribution: it sent someone to
// replace a value that could not have caused the rejection, and disagreed with what a
// turn says about the identical condition.
func TestCredentialOwnershipAlwaysNamesTheDeployment(t *testing.T) {
	for name, suffix := range map[string]string{
		"no caller key":  credentialOwnerSuffix(""),
		"caller key set": credentialOwnerSuffix("k"),
	} {
		if !strings.Contains(suffix, "backend's own") {
			t.Errorf("%s: row = %q, want the backend's credential named", name, suffix)
		}
		if strings.Contains(suffix, "yours") {
			t.Errorf("%s: row = %q claims a credential the caller does not hold", name, suffix)
		}
	}
	for name, hint := range map[string]string{
		"no caller key":  credentialFixHint(""),
		"caller key set": credentialFixHint("k"),
	} {
		if !strings.Contains(hint, "not yours") {
			t.Errorf("%s: fix = %q, want the deployment named as the owner", name, hint)
		}
	}
	// A bearer that IS set is still named — as present and ruled out, never as the fault.
	if h := credentialFixHint(""); strings.Contains(h, "caller-supplied") {
		t.Errorf("with no caller key the fix must not mention one: %q", h)
	}
	if h := credentialFixHint("k"); !strings.Contains(h, "caller-supplied") {
		t.Errorf("with a caller key the fix must say it is in play: %q", h)
	}
}
