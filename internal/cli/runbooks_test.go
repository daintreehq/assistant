package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/storage"
)

// capabilitiesServer serves one canned /v1/daintree/capabilities body. `--list-runbooks`
// must be exactly one GET against the configured endpoint — no lease, no database, no
// MCP — so a bare httptest server is the whole world these tests need.
func capabilitiesServer(t *testing.T, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/capabilities") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// listOpts isolates the run from the developer's real environment: a listing must never
// depend on, or write to, the state dir of a live assistant.
//
// The state dir names a path INSIDE a temp dir that does not exist yet, on purpose.
// Handing it an already-created directory would hide the very thing this route promises:
// a listing asks the backend a question and must not make a directory to do it.
func listOpts(t *testing.T, baseURL string, asJSON bool) Options {
	t.Helper()
	dir := t.TempDir()
	return Options{
		BackendURL: baseURL,
		StateDir:   filepath.Join(dir, "never-created"),
		Project:    dir,
		JSON:       asJSON,
		Offline:    boolp(true),
	}
}

func boolp(b bool) *bool { return &b }

func runList(t *testing.T, opts Options) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = runListRunbooks(context.Background(), opts, &out, &errOut)
	return code, out.String(), errOut.String()
}

const twoRunbookCatalog = `{"runbooks":{"catalog_revision":"sha256:abc","manual_resolve":true,"pinned_runbook_ids":true,
	"catalog":[{"id":"daintree.foundation","title":"Foundation"},{"id":"a.short","title":"Short"}]}}`

// The human listing puts the ID first because the id is what --runbook takes; the title is
// only the reminder of what it is.
func TestListRunbooksText(t *testing.T) {
	code, stdout, stderr := runList(t, listOpts(t, capabilitiesServer(t, twoRunbookCatalog), false))
	if code != domain.OneShotExitCode.Success {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "ID") || !strings.Contains(stdout, "TITLE") {
		t.Fatalf("listing has no header:\n%s", stdout)
	}
	// Sorted defensively rather than trusting the server's documented order — a human
	// scans this and a script may diff it.
	iShort := strings.Index(stdout, "a.short")
	iFound := strings.Index(stdout, "daintree.foundation")
	if iShort < 0 || iFound < 0 || iShort > iFound {
		t.Fatalf("listing is not sorted by id:\n%s", stdout)
	}
	if !strings.Contains(stdout, "--runbook") {
		t.Fatalf("the listing must say what to do with an id:\n%s", stdout)
	}
}

// One indented JSON document, deliberately not the one-shot JSONL event stream: there is
// no run here to narrate, and the consumer wants a value it can pipe into jq.
func TestListRunbooksJSON(t *testing.T) {
	code, stdout, _ := runList(t, listOpts(t, capabilitiesServer(t, twoRunbookCatalog), true))
	if code != domain.OneShotExitCode.Success {
		t.Fatalf("exit = %d, want 0", code)
	}
	var doc RunbookCatalogJSON
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("stdout is not one JSON document (%v):\n%s", err, stdout)
	}
	if doc.CatalogRevision != "sha256:abc" {
		t.Fatalf("catalogRevision = %q; it is what a caching caller keys the list on", doc.CatalogRevision)
	}
	if len(doc.Runbooks) != 2 || doc.Runbooks[0].ID != "a.short" || doc.Runbooks[1].ID != "daintree.foundation" {
		t.Fatalf("runbooks = %+v, want both, sorted by id", doc.Runbooks)
	}
	if doc.Runbooks[1].Title != "Foundation" {
		t.Fatalf("title dropped: %+v", doc.Runbooks[1])
	}
}

// An advertised EMPTY catalog is a successful answer to the question — "this backend
// loads nothing" — and must not be reported as a failure.
func TestListRunbooksEmptyCatalogSucceeds(t *testing.T) {
	body := `{"runbooks":{"catalog_revision":"r","catalog":[],"pinned_runbook_ids":true}}`
	code, stdout, _ := runList(t, listOpts(t, capabilitiesServer(t, body), false))
	if code != domain.OneShotExitCode.Success {
		t.Fatalf("exit = %d, want 0 — an empty catalog is an answer, not an error", code)
	}
	if !strings.Contains(stdout, "no runbooks") {
		t.Fatalf("an empty listing must say so plainly:\n%s", stdout)
	}

	code, stdout, _ = runList(t, listOpts(t, capabilitiesServer(t, body), true))
	if code != domain.OneShotExitCode.Success {
		t.Fatalf("json exit = %d, want 0", code)
	}
	var doc RunbookCatalogJSON
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("stdout is not JSON: %v", err)
	}
	if len(doc.Runbooks) != 0 {
		t.Fatalf("runbooks = %+v, want empty", doc.Runbooks)
	}
	// An EMPTY ARRAY, never null. len() accepts both, which is exactly how a nil slice
	// sneaks back in — and `jq '.runbooks[]'` fails on null while it happily yields nothing
	// on []. Assert the raw bytes.
	if !strings.Contains(stdout, `"runbooks": []`) {
		t.Fatalf("an advertised empty catalog must serialize as [], not null:\n%s", stdout)
	}
}

// A backend that OMITS the catalog cannot answer the question, which is a different
// thing from answering "none" — and needs a different next action (upgrade the backend,
// not "there is nothing to pin").
func TestListRunbooksReportsAnUnadvertisedCatalog(t *testing.T) {
	body := `{"runbooks":{"catalog_revision":"r","manual_resolve":true}}`
	code, _, stderr := runList(t, listOpts(t, capabilitiesServer(t, body), false))
	if code != domain.OneShotExitCode.Error {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "does not advertise a runbook catalog") {
		t.Fatalf("stderr does not name the cause: %q", stderr)
	}
}

// Even a failure answers in JSON when JSON was asked for — the same rule `doctor --json`
// follows. A consumer parsing stdout must never receive prose on the one path it cannot
// handle.
func TestListRunbooksFailsInJSONWhenJSONWasAsked(t *testing.T) {
	body := `{"runbooks":{"catalog_revision":"r"}}`
	code, stdout, _ := runList(t, listOpts(t, capabilitiesServer(t, body), true))
	if code != domain.OneShotExitCode.Error {
		t.Fatalf("exit = %d, want 1", code)
	}
	var doc struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("a --json failure wrote prose to stdout (%v): %s", err, stdout)
	}
	if doc.Error.Code != "runbook_catalog_not_advertised" {
		t.Fatalf("error code = %q, want runbook_catalog_not_advertised", doc.Error.Code)
	}
	if doc.Error.Message == "" {
		t.Fatal("a machine-readable code still needs a human sentence beside it")
	}
}

// An unreachable endpoint is a different failure from an old one, and the message must
// name the endpoint — otherwise the reader has no idea which backend was asked.
func TestListRunbooksReportsAnUnreachableBackend(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	code, stdout, _ := runList(t, listOpts(t, srv.URL, true))
	if code != domain.OneShotExitCode.Error {
		t.Fatalf("exit = %d, want 1", code)
	}
	var doc struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("stdout is not JSON: %v — %s", err, stdout)
	}
	if doc.Error.Code != "capabilities_unavailable" {
		t.Fatalf("error code = %q, want capabilities_unavailable", doc.Error.Code)
	}
	if !strings.Contains(doc.Error.Message, srv.URL) {
		t.Fatalf("the message must name the endpoint it asked: %q", doc.Error.Message)
	}
}

// A cancelled listing is a cancellation (exit 2), not a failure (exit 1) — the one-shot
// exit contract every scripted caller already keys on.
func TestListRunbooksReportsCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out, errOut bytes.Buffer
	if code := runListRunbooks(ctx, listOpts(t, srv.URL, false), &out, &errOut); code != domain.OneShotExitCode.Cancelled {
		t.Fatalf("exit = %d, want 2 (cancelled)", code)
	}
}

// A listing is a question about the BACKEND. Creating a state directory to ask it is a
// side effect nobody requested, and it turns an unwritable --state-dir into a failure of
// something that never wanted a state dir — which is the difference between a listing
// that works everywhere and one that works only where a session could have run.
func TestListRunbooksTouchesNoState(t *testing.T) {
	opts := listOpts(t, capabilitiesServer(t, twoRunbookCatalog), false)
	code, _, stderr := runList(t, opts)
	if code != domain.OneShotExitCode.Success {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if _, err := os.Stat(opts.StateDir); !os.IsNotExist(err) {
		t.Fatalf("--list-runbooks created %s (stat err = %v); it must touch no state", opts.StateDir, err)
	}
}

// The one-document contract has to hold on EVERY path or a parser cannot rely on it, and
// an interrupted run that wrote nothing is the case a parser handles worst.
func TestListRunbooksCancellationStillAnswersInJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out, errOut bytes.Buffer
	code := runListRunbooks(ctx, listOpts(t, srv.URL, true), &out, &errOut)
	if code != domain.OneShotExitCode.Cancelled {
		t.Fatalf("exit = %d, want 2 (cancelled)", code)
	}
	var doc struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(out.String()), &doc); err != nil {
		t.Fatalf("a cancelled --json run wrote no parseable document (%v): %q", err, out.String())
	}
	if doc.Error.Code != "cancelled" {
		t.Fatalf("error code = %q, want cancelled", doc.Error.Code)
	}
}

// A caller-owned deadline is a "you stopped us", exactly like a SIGINT — not a failure of
// the listing. Reporting it as an error (1) would send a script hunting a backend problem
// that never happened.
func TestListRunbooksParentDeadlineIsCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()
	var out, errOut bytes.Buffer
	if code := runListRunbooks(ctx, listOpts(t, srv.URL, false), &out, &errOut); code != domain.OneShotExitCode.Cancelled {
		t.Fatalf("exit = %d, want 2 — a caller's expired deadline is a cancellation", code)
	}
	// SILENT in human mode. A red "✗ context canceled" tells someone who just pressed
	// Ctrl-C what they already know, and calls a listing that was STOPPED a listing that
	// FAILED. The exit code carries the whole message.
	if strings.TrimSpace(errOut.String()) != "" {
		t.Fatalf("a cancelled human run must print nothing, got %q", errOut.String())
	}
	if strings.TrimSpace(out.String()) != "" {
		t.Fatalf("stdout must stay empty on a cancelled human run, got %q", out.String())
	}
}

// THE regression test for the adoption-order bug: a launch whose pin preflight FAILS must
// not touch the project's durable current-session pointer.
//
// Adoption is not undone by shutdown, so running it before the preflight let a mistyped
// `--runbook` — a launch that never ran a single turn — permanently displace the real
// conversation. The supervisor's detached wake turns resume whatever that pointer names,
// so the user's actual session would simply stop being continued.
//
// This drives runInteractive twice against the same state dir, because the bug is an
// ORDERING inside that function and nothing below it can observe the difference.
func TestFailedPinPreflightDoesNotDisplaceTheCurrentSession(t *testing.T) {
	stateDir := t.TempDir()
	project := t.TempDir()
	t.Setenv("DAINTREE_ASSISTANT_STATE_DIR", stateDir)
	t.Setenv(NoDaemonEnv, "1")
	t.Setenv("DAINTREE_BACKEND_URL", capabilitiesServer(t, twoRunbookCatalog))

	// Both launches run the line REPL, so give each one a closed stdin: it reads EOF
	// and returns immediately, which is enough to reach (or fail before) adoption.
	closeStdin(t)

	// Both launches run the line REPL, so give each one a closed stdin: it reads EOF
	// and returns immediately, which is enough to reach (or fail before) adoption.
	closeStdin(t)

	// A normal launch, no pins: it adopts and becomes the project's current session.
	good := Options{
		Offline: boolPtr(true),
		Project: project,
	}
	if code := runInteractive(context.Background(), good, true); code != domain.OneShotExitCode.Success {
		t.Fatalf("baseline launch exit = %d, want 0", code)
	}
	adopted := currentSessionPointer(t, stateDir)
	if adopted == "" {
		t.Fatal("the baseline launch never adopted a current session")
	}

	// Now a launch with a mistyped pin. It must fail BEFORE adopting, leaving the
	// pointer on the baseline session.
	bad := Options{
		Offline:          boolPtr(true),
		Project:          project,
		PinnedRunbookIDs: []string{"daintree.foundatoin"},
	}
	if code := runInteractive(context.Background(), bad, true); code != domain.OneShotExitCode.Error {
		t.Fatalf("a mistyped --runbook launch exit = %d, want 1", code)
	}
	if got := currentSessionPointer(t, stateDir); got != adopted {
		t.Fatalf("a failed launch displaced the current session: %q, want %q — the supervisor "+
			"would now resume a conversation that never ran a turn", got, adopted)
	}
}

// closeStdin points os.Stdin at an already-closed pipe for the test's duration, so
// the line REPL reads EOF and returns instead of blocking.
func closeStdin(t *testing.T) {
	t.Helper()
	stdin, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	original := os.Stdin
	os.Stdin = stdin
	t.Cleanup(func() {
		os.Stdin = original
		_ = stdin.Close()
	})
}

// currentSessionPointer reads the durable pointer the supervisor resumes from.
func currentSessionPointer(t *testing.T, stateDir string) string {
	t.Helper()
	store, err := storage.Open(filepath.Join(stateDir, "state.db"), nil)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	v, err := store.GetRuntimeState(storage.RuntimeKeyCurrentSession)
	if err != nil {
		t.Fatalf("read current session: %v", err)
	}
	return v
}
