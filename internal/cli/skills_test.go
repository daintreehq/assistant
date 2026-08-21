package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
)

// capabilitiesServer serves one canned /v1/daintree/capabilities body. `--list-skills`
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
func listOpts(t *testing.T, baseURL string, asJSON bool) Options {
	t.Helper()
	dir := t.TempDir()
	return Options{BackendURL: baseURL, StateDir: dir, Project: dir, JSON: asJSON, Offline: boolp(true)}
}

func boolp(b bool) *bool { return &b }

func runList(t *testing.T, opts Options) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = runListSkills(context.Background(), opts, &out, &errOut)
	return code, out.String(), errOut.String()
}

const twoSkillCatalog = `{"skills":{"catalog_revision":"sha256:abc","manual_resolve":true,"pinned_skill_ids":true,
	"catalog":[{"id":"daintree.foundation","title":"Foundation"},{"id":"a.short","title":"Short"}]}}`

// The human listing puts the ID first because the id is what --skill takes; the title is
// only the reminder of what it is.
func TestListSkillsText(t *testing.T) {
	code, stdout, stderr := runList(t, listOpts(t, capabilitiesServer(t, twoSkillCatalog), false))
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
	if !strings.Contains(stdout, "--skill") {
		t.Fatalf("the listing must say what to do with an id:\n%s", stdout)
	}
}

// One indented JSON document, deliberately not the one-shot JSONL event stream: there is
// no run here to narrate, and the consumer wants a value it can pipe into jq.
func TestListSkillsJSON(t *testing.T) {
	code, stdout, _ := runList(t, listOpts(t, capabilitiesServer(t, twoSkillCatalog), true))
	if code != domain.OneShotExitCode.Success {
		t.Fatalf("exit = %d, want 0", code)
	}
	var doc SkillCatalogJSON
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("stdout is not one JSON document (%v):\n%s", err, stdout)
	}
	if doc.CatalogRevision != "sha256:abc" {
		t.Fatalf("catalogRevision = %q; it is what a caching caller keys the list on", doc.CatalogRevision)
	}
	if len(doc.Skills) != 2 || doc.Skills[0].ID != "a.short" || doc.Skills[1].ID != "daintree.foundation" {
		t.Fatalf("skills = %+v, want both, sorted by id", doc.Skills)
	}
	if doc.Skills[1].Title != "Foundation" {
		t.Fatalf("title dropped: %+v", doc.Skills[1])
	}
}

// An advertised EMPTY catalog is a successful answer to the question — "this backend
// loads nothing" — and must not be reported as a failure.
func TestListSkillsEmptyCatalogSucceeds(t *testing.T) {
	body := `{"skills":{"catalog_revision":"r","catalog":[],"pinned_skill_ids":true}}`
	code, stdout, _ := runList(t, listOpts(t, capabilitiesServer(t, body), false))
	if code != domain.OneShotExitCode.Success {
		t.Fatalf("exit = %d, want 0 — an empty catalog is an answer, not an error", code)
	}
	if !strings.Contains(stdout, "no skills") {
		t.Fatalf("an empty listing must say so plainly:\n%s", stdout)
	}

	code, stdout, _ = runList(t, listOpts(t, capabilitiesServer(t, body), true))
	if code != domain.OneShotExitCode.Success {
		t.Fatalf("json exit = %d, want 0", code)
	}
	var doc SkillCatalogJSON
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("stdout is not JSON: %v", err)
	}
	if len(doc.Skills) != 0 {
		t.Fatalf("skills = %+v, want empty", doc.Skills)
	}
}

// A backend that OMITS the catalog cannot answer the question, which is a different
// thing from answering "none" — and needs a different next action (upgrade the backend,
// not "there is nothing to pin").
func TestListSkillsReportsAnUnadvertisedCatalog(t *testing.T) {
	body := `{"skills":{"catalog_revision":"r","manual_resolve":true}}`
	code, _, stderr := runList(t, listOpts(t, capabilitiesServer(t, body), false))
	if code != domain.OneShotExitCode.Error {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "does not advertise a skill catalog") {
		t.Fatalf("stderr does not name the cause: %q", stderr)
	}
}

// Even a failure answers in JSON when JSON was asked for — the same rule `doctor --json`
// follows. A consumer parsing stdout must never receive prose on the one path it cannot
// handle.
func TestListSkillsFailsInJSONWhenJSONWasAsked(t *testing.T) {
	body := `{"skills":{"catalog_revision":"r"}}`
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
	if doc.Error.Code != "skill_catalog_not_advertised" {
		t.Fatalf("error code = %q, want skill_catalog_not_advertised", doc.Error.Code)
	}
	if doc.Error.Message == "" {
		t.Fatal("a machine-readable code still needs a human sentence beside it")
	}
}

// An unreachable endpoint is a different failure from an old one, and the message must
// name the endpoint — otherwise the reader has no idea which backend was asked.
func TestListSkillsReportsAnUnreachableBackend(t *testing.T) {
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
func TestListSkillsReportsCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out, errOut bytes.Buffer
	if code := runListSkills(ctx, listOpts(t, srv.URL, false), &out, &errOut); code != domain.OneShotExitCode.Cancelled {
		t.Fatalf("exit = %d, want 2 (cancelled)", code)
	}
}
