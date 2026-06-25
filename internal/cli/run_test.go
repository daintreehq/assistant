package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// run_test.go locks the first-run/CLI-discoverability contract (issue #211): the
// `doctor` subcommand exits non-zero when the model key is missing (so scripts and
// CI can gate on it), and one-shot fails fast on a missing key before a dead model
// round-trip. Each test points the state dir at a temp dir and runs offline so no
// network or real ~/.daintree state is touched. The DEEPSEEK_API_KEY env override
// means these tests must NOT run in parallel.

// doctorOpts builds Options that resolve to an isolated, offline App. The key is
// controlled by the caller via t.Setenv before invoking.
func doctorOpts(t *testing.T) Options {
	t.Helper()
	t.Setenv("DAINTREE_ASSISTANT_STATE_DIR", t.TempDir())
	return Options{Offline: true, Project: t.TempDir()}
}

// With no model key, `doctor` must exit Error so scripts/CI can gate on it — the
// pre-fix behavior was to always exit Success even with a MISSING key.
func TestRunDoctor_MissingKeyReturnsError(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "")
	opts := doctorOpts(t)
	if code := RunDoctor(context.Background(), opts); code != domain.OneShotExitCode.Error {
		t.Fatalf("doctor with no key must exit Error(%d), got %d", domain.OneShotExitCode.Error, code)
	}
}

// With a model key present, `doctor` exits Success even though the MCP is not
// connected — a disconnected MCP is a valid degraded local mode, not a failure.
func TestRunDoctor_WithKeyReturnsSuccess(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "fake-key-for-test")
	opts := doctorOpts(t)
	if code := RunDoctor(context.Background(), opts); code != domain.OneShotExitCode.Success {
		t.Fatalf("doctor with a key (MCP disconnected) must exit Success(%d), got %d", domain.OneShotExitCode.Success, code)
	}
}

// One-shot must fail fast with Error when the key is absent — surfacing the missing
// key up front rather than only after a dead model round-trip.
func TestRunOneShot_MissingKeyReturnsEarlyError(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "")
	opts := doctorOpts(t)
	opts.Prompt = "hello"
	opts.HasPrompt = true
	if code := RunOneShot(context.Background(), opts); code != domain.OneShotExitCode.Error {
		t.Fatalf("one-shot with no key must exit Error(%d) before a model call, got %d", domain.OneShotExitCode.Error, code)
	}
}

// The missing-key preflight must short-circuit BEFORE any model request — proving
// it is genuinely early (no dead round-trip), not just error-shaped. The model
// client is pointed at a counting server that a removed preflight would hit.
func TestRunOneShot_MissingKeyMakesNoModelRequest(t *testing.T) {
	var modelHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&modelHits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	t.Setenv("DEEPSEEK_API_KEY", "")
	t.Setenv("DEEPSEEK_BASE_URL", srv.URL) // a real model call would land here
	t.Setenv("DAINTREE_ASSISTANT_STATE_DIR", t.TempDir())
	// NOT offline: a removed preflight would build a live client and dial srv.
	opts := Options{Project: t.TempDir(), Prompt: "hello", HasPrompt: true}

	if code := RunOneShot(context.Background(), opts); code != domain.OneShotExitCode.Error {
		t.Fatalf("one-shot with no key must exit Error(%d), got %d", domain.OneShotExitCode.Error, code)
	}
	if n := atomic.LoadInt32(&modelHits); n != 0 {
		t.Fatalf("preflight must short-circuit before any model request; got %d", n)
	}
}
