package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daintreehq/assistant/internal/domain"
)

// NO policy must stay fully permissive: the same registry backs the trusted embedding
// paths, where the operator IS the caller, and a ceiling that switched itself on would
// break them — --auto-approve most visibly.
//
// This is the distinction the design turns on: "no policy" and "a policy whose fields are
// all zero" are different things, because installing a policy is opting into deny-by-
// default.
func TestNoPolicyPermitsEverything(t *testing.T) {
	built := false
	reg := NewUnconfinedRegistry(context.Background(), func(_, _ context.Context, _ OpenParams) (Runtime, error) {
		built = true
		return newFakeRuntime("ses_test"), nil
	})
	// Deliberately the most privileged request there is.
	_, err := reg.Open(context.Background(), OpenParams{
		Project: t.TempDir(), Tier: string(domain.TierSystem),
		StateDir: t.TempDir(), LogDir: t.TempDir(), Approvals: ApprovalAuto,
	})
	if err != nil {
		t.Fatalf("an unconfined registry refused a system/auto session: %v", err)
	}
	if !built {
		t.Error("the runtime was never built")
	}
}

// An INSTALLED policy denies by default, even one whose fields are all zero.
func TestInstalledPolicyDeniesAutoApproveByDefault(t *testing.T) {
	reg := NewUnconfinedRegistry(context.Background(), func(_, _ context.Context, _ OpenParams) (Runtime, error) {
		return newFakeRuntime("ses_test"), nil
	})
	reg.SetPolicy(ServerPolicy{})
	if _, err := reg.Open(context.Background(), OpenParams{Approvals: ApprovalAuto}); err == nil {
		t.Fatal("installing a policy must switch auto-approve off unless it is explicitly allowed")
	}
}

// A session may NARROW the policy and can never widen it. This is the rule that stops a
// content-level instruction becoming a new process authority boundary.
func TestPolicyRefusesATierAboveTheCeiling(t *testing.T) {
	p := ServerPolicy{MaxTier: domain.TierOperator}

	if err := p.Check(OpenParams{Tier: string(domain.TierSupervisor)}, 0); err != nil {
		t.Errorf("narrowing to supervisor was refused: %v", err)
	}
	if err := p.Check(OpenParams{Tier: string(domain.TierOperator)}, 0); err != nil {
		t.Errorf("asking for exactly the ceiling was refused: %v", err)
	}

	err := p.Check(OpenParams{Tier: string(domain.TierSystem)}, 0)
	var pe *PolicyError
	if !errors.As(err, &pe) {
		t.Fatalf("asking above the ceiling returned %v, want a PolicyError", err)
	}
	// Refused, not silently downgraded: a caller that believes it holds system tier and
	// was quietly given operator reads every later refusal as a bug.
	if !strings.Contains(pe.Error(), "system") || !strings.Contains(pe.Error(), "operator") {
		t.Errorf("the refusal must name both tiers, got %q", pe.Error())
	}
}

// An unknown tier must not sort as "lowest" and slip under every ceiling.
func TestPolicyRefusesAnUnknownTier(t *testing.T) {
	p := ServerPolicy{MaxTier: domain.TierSupervisor}
	if err := p.Check(OpenParams{Tier: "root"}, 0); err == nil {
		t.Fatal("an unknown tier passed the ceiling")
	}
	unknownCeiling := ServerPolicy{MaxTier: domain.Tier("godmode")}
	if err := unknownCeiling.Check(OpenParams{Tier: string(domain.TierSupervisor)}, 0); err == nil {
		t.Fatal("a policy naming an unknown ceiling must refuse everything, not permit it")
	}
}

// auto-approve is the setting that turns a read-mostly session into one that can push and
// run commands with nothing watching, so it is off unless the operator allowed it.
func TestPolicyGatesAutoApprove(t *testing.T) {
	strict := ServerPolicy{MaxTier: domain.TierSystem}
	if err := strict.Check(OpenParams{Approvals: ApprovalAuto}, 0); err == nil {
		t.Fatal("approvals:auto was permitted under a policy that did not allow it")
	}
	// Decline (and an omitted mode, which resolves to it here) is always available: it
	// grants nothing.
	for _, mode := range []ApprovalMode{ApprovalDecline, ""} {
		if err := strict.Check(OpenParams{Approvals: mode}, 0); err != nil {
			t.Errorf("mode %q was refused: %v", mode, err)
		}
	}
	permissive := ServerPolicy{AllowAutoApprove: true}
	if err := permissive.Check(OpenParams{Approvals: ApprovalAuto}, 0); err != nil {
		t.Errorf("an explicitly allowed auto was refused: %v", err)
	}
}

// The separator check is the whole point of confining a path: a bare prefix match would
// let /srv/appliance pass as being inside /srv/app.
func TestPolicyConfinesPathsOnSeparatorBoundaries(t *testing.T) {
	root := t.TempDir()
	sibling := root + "-evil"
	p := ServerPolicy{AllowedProjectRoots: []string{root}}

	if err := p.Check(OpenParams{Project: root}, 0); err != nil {
		t.Errorf("the root itself was refused: %v", err)
	}
	if err := p.Check(OpenParams{Project: filepath.Join(root, "sub", "dir")}, 0); err != nil {
		t.Errorf("a path beneath the root was refused: %v", err)
	}
	if err := p.Check(OpenParams{Project: sibling}, 0); err == nil {
		t.Errorf("%q passed as being inside %q — the prefix match is not separator-aware", sibling, root)
	}
	// Traversal must not escape: the path is cleaned before it is compared.
	escape := filepath.Join(root, "..", "elsewhere")
	if err := p.Check(OpenParams{Project: escape}, 0); err == nil {
		t.Errorf("%q escaped the allowed root via traversal", escape)
	}
	// An unset path is the caller declining to choose, which the policy has nothing to
	// say about.
	if err := p.Check(OpenParams{}, 0); err != nil {
		t.Errorf("an unset project path was refused: %v", err)
	}
}

// State and log roots are separate from project roots because the honest answers differ:
// a caller may read a project it may not scribble beside.
func TestPolicyConfinesStateAndLogRootsIndependently(t *testing.T) {
	project := t.TempDir()
	writable := t.TempDir()
	p := ServerPolicy{AllowedProjectRoots: []string{project}, AllowedStateRoots: []string{writable}, AllowedLogRoots: []string{writable}}

	if err := p.Check(OpenParams{Project: project, StateDir: project}, 0); err == nil {
		t.Error("a state dir inside the project root passed a policy that only allows a separate writable root")
	}
	if err := p.Check(OpenParams{Project: project, StateDir: writable, LogDir: writable}, 0); err != nil {
		t.Errorf("the permitted combination was refused: %v", err)
	}
}

// Each session holds a project lease and starts real runtime machinery, so the count is
// a resource bound, not a preference.
func TestPolicyCapsConcurrentSessions(t *testing.T) {
	p := ServerPolicy{MaxSessions: 2}
	if err := p.Check(OpenParams{}, 1); err != nil {
		t.Errorf("opening the second session was refused: %v", err)
	}
	if err := p.Check(OpenParams{}, 2); err == nil {
		t.Fatal("the cap did not hold")
	}
}

// A refusal must happen BEFORE the runtime is built: otherwise the refused open has
// already taken the project lease, and the refusal is the only thing that was not a side
// effect.
func TestPolicyRefusalNeverBuildsARuntime(t *testing.T) {
	built := false
	reg := NewUnconfinedRegistry(context.Background(), func(_, _ context.Context, _ OpenParams) (Runtime, error) {
		built = true
		return newFakeRuntime("ses_test"), nil
	})
	reg.SetPolicy(ServerPolicy{MaxTier: domain.TierSupervisor})

	if _, err := reg.Open(context.Background(), OpenParams{Tier: string(domain.TierSystem)}); err == nil {
		t.Fatal("the registry opened a session the policy forbids")
	}
	if built {
		t.Error("the runtime was constructed for a refused open; the lease would already be held")
	}
}

// The session cap must hold under CONCURRENT opens, and it must cap the WORK rather than
// the bookkeeping.
//
// Building a runtime is slow — project lease, database, MCP connect — so it cannot happen
// under the registry lock, which means the count an open reads before building is stale
// by the time it registers. Enforcing only on that early read admitted two sessions under
// a cap of one. Re-checking at insert time fixed the count but not the cost: every
// concurrent open still built a full runtime and contended for the same lease, and all
// but one were torn down after the expensive part was already done.
//
// So the cap is RESERVED, and this asserts both halves: one session opens, and only one
// factory ever ran.
func TestPolicySessionCapReservesCapacityBeforeBuildingARuntime(t *testing.T) {
	const cap = 1
	const attempts = 8

	var factoryMu sync.Mutex
	factoryCalls := 0
	entered := make(chan struct{}, attempts)
	release := make(chan struct{})
	reg := NewUnconfinedRegistry(context.Background(), func(_, _ context.Context, _ OpenParams) (Runtime, error) {
		factoryMu.Lock()
		factoryCalls++
		factoryMu.Unlock()
		entered <- struct{}{}
		// Hold the factory open so every other attempt is racing a build in flight,
		// which is the interleaving that defeats a check-then-act cap.
		<-release
		return newFakeRuntime(domain.NewID("ses_")), nil
	})
	reg.SetPolicy(ServerPolicy{MaxSessions: cap})

	var wg sync.WaitGroup
	var mu sync.Mutex
	opened := 0
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := reg.Open(context.Background(), OpenParams{}); err == nil {
				mu.Lock()
				opened++
				mu.Unlock()
			}
		}()
	}
	// Wait for the one build that should be admitted, then let it finish. The others
	// must have been refused without ever reaching the factory.
	<-entered
	// Give any wrongly-admitted attempt a moment to reach the factory, so this fails on
	// the bug rather than on scheduling luck.
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if opened != cap {
		t.Fatalf("%d sessions opened under a cap of %d; the cap is check-then-act", opened, cap)
	}
	if got := len(reg.List()); got != cap {
		t.Fatalf("registry holds %d sessions, want %d", got, cap)
	}
	factoryMu.Lock()
	defer factoryMu.Unlock()
	if factoryCalls != cap {
		t.Fatalf("%d runtimes were built under a cap of %d — the cap bounds the registry but not the "+
			"lease/database/MCP work each open pays for", factoryCalls, cap)
	}
}

// A reservation that is not released is a cap that ratchets down until the server admits
// nothing. Every exit from Open must give it back — including a factory that fails.
func TestAFailedOpenReleasesItsReservation(t *testing.T) {
	fail := true
	reg := NewUnconfinedRegistry(context.Background(), func(_, _ context.Context, _ OpenParams) (Runtime, error) {
		if fail {
			return nil, errors.New("lease contended")
		}
		return newFakeRuntime("ses_ok"), nil
	})
	reg.SetPolicy(ServerPolicy{MaxSessions: 1})

	for i := 0; i < 5; i++ {
		if _, err := reg.Open(context.Background(), OpenParams{}); err == nil {
			t.Fatal("the failing factory reported success")
		}
	}
	fail = false
	if _, err := reg.Open(context.Background(), OpenParams{}); err != nil {
		t.Fatalf("five failed opens exhausted the cap: %v", err)
	}
}

// A long poll must never sleep through a turn that is STOPPED. The revision counter
// alone cannot catch an approval that parked BETWEEN two polls: the handler then
// captures an already-advanced revision, sinceSeq sits at the event tail, and nothing
// further is ever signalled — so the caller waits out its whole budget, possibly past
// the approval's own timeout, on a run that is blocked rather than slow.
func TestPollDoesNotWaitWhenTheRunIsAlreadyParkedOnAnApproval(t *testing.T) {
	run := NewRun("mrun_p", "ses_p", "prompt", func() {})
	approvals := NewApprovals(ApprovalDelegate, 0)

	if hasPendingApproval(run, approvals) {
		t.Fatal("a fresh run reported a pending approval")
	}
	if hasPendingApproval(run, nil) {
		t.Fatal("a nil broker must report nothing pending, not panic")
	}

	parked := make(chan struct{})
	go func() {
		close(parked)
		approvals.Confirm(context.Background(), ApprovalRequest{Tool: "git.push", RunID: run.ID})
	}()
	<-parked

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !hasPendingApproval(run, approvals) {
		time.Sleep(5 * time.Millisecond)
	}
	if !hasPendingApproval(run, approvals) {
		t.Fatal("the parked approval was never visible to the poll pre-check")
	}

	// An approval parked against a DIFFERENT run must not short-circuit this one's wait
	// — that would turn every poll in the session into a busy loop.
	other := NewRun("mrun_other", "ses_p", "prompt", func() {})
	if hasPendingApproval(other, approvals) {
		t.Error("another run's approval was reported as blocking this one")
	}

	approvals.RejectRun(run.ID)
}

// The Daintree bearer must not be reachable as a tool ARGUMENT, and the server's own
// instructions must not teach a model to try. Both halves matter: the schema is what a
// client validates against, and the instructions are what the model actually reads —
// prose telling it to "pass mcpToken" produces invalid calls AND pressures a maintainer
// to put the field back.
func TestTheInlineMcpBearerIsGoneFromEveryModelFacingSurface(t *testing.T) {
	// The argument struct is what the SDK projects into the tool schema.
	typ := reflect.TypeOf(OpenInput{})
	for i := 0; i < typ.NumField(); i++ {
		tag := typ.Field(i).Tag.Get("json")
		name := strings.Split(tag, ",")[0]
		if name == "mcpToken" {
			t.Fatalf("OpenInput still exposes an inline %q field; the bearer must be named by FILE", name)
		}
	}
	// A path field is the supported replacement, so this is not passing by deletion.
	if _, ok := typ.FieldByName("McpTokenFile"); !ok {
		t.Fatal("OpenInput lost McpTokenFile; there must still be a way to name the token")
	}

	// `mcpTokenFile` legitimately contains the substring `mcpToken`, so match the
	// standalone word rather than a naive Contains.
	inlineMention := regexp.MustCompile(`\bmcpToken\b`)
	if inlineMention.MatchString(instructions) {
		t.Errorf("the server instructions still tell a model to pass mcpToken:\n%s", instructions)
	}
	if !strings.Contains(instructions, "mcpTokenFile") {
		t.Error("the instructions should name mcpTokenFile so a model knows the supported route")
	}
}

// --- Endpoint authority (P0-1, P0-2) ---

// The two endpoint arguments decide where a session's conversation goes and which tool
// server it believes. Under a policy they are pinned, and an override is a refusal
// rather than a silent redirect.
func TestPolicyPinsEndpointsByDefault(t *testing.T) {
	p := ServerPolicy{}
	for _, tc := range []struct {
		field string
		in    OpenParams
	}{
		{"backendUrl", OpenParams{BackendURL: "http://attacker.example/"}},
		{"mcpUrl", OpenParams{McpURL: "http://attacker.example/mcp"}},
	} {
		err := p.Check(tc.in, 0)
		var pe *PolicyError
		if !errors.As(err, &pe) {
			t.Fatalf("%s override was not refused by an installed policy: %v", tc.field, err)
		}
		if pe.Field != tc.field {
			t.Errorf("refusal named %q, want %q", pe.Field, tc.field)
		}
	}
	// Omitting them is always fine — the caller chose nothing, so there is nothing to
	// judge.
	if err := p.Check(OpenParams{}, 0); err != nil {
		t.Fatalf("omitted endpoints were refused: %v", err)
	}
}

func TestPolicyEnforcesEndpointOriginAllowlist(t *testing.T) {
	p := ServerPolicy{
		AllowBackendOverride:  true,
		AllowedBackendOrigins: []string{"https://assistant.daintree.org"},
	}
	if err := p.Check(OpenParams{BackendURL: "https://assistant.daintree.org/v1/x"}, 0); err != nil {
		t.Fatalf("an allowlisted origin was refused: %v", err)
	}
	if err := p.Check(OpenParams{BackendURL: "https://evil.example/v1/x"}, 0); err == nil {
		t.Fatal("an origin outside the allowlist was permitted")
	}
	// The switch and the list are separate decisions: listing origins must not by
	// itself turn overrides on.
	off := ServerPolicy{AllowedBackendOrigins: []string{"https://assistant.daintree.org"}}
	if err := off.Check(OpenParams{BackendURL: "https://assistant.daintree.org"}, 0); err == nil {
		t.Fatal("an origin allowlist alone enabled overrides")
	}
}

// Userinfo is a credential travelling through the one channel that exists so that
// credentials do not. It is refused, never stripped — stripping would leave the caller
// believing it had authenticated.
func TestPolicyRefusesEndpointUserinfoAndOddSchemes(t *testing.T) {
	p := ServerPolicy{AllowBackendOverride: true}
	for _, raw := range []string{
		"https://user:pass@backend.example/",
		"file:///etc/passwd",
		"gopher://backend.example/",
		"https://",
	} {
		if err := p.Check(OpenParams{BackendURL: raw}, 0); err == nil {
			t.Errorf("%q was accepted as an endpoint", raw)
		}
	}
}

// Plaintext is fine on loopback (that IS the local dev backend) and refused off it.
func TestPolicyRequiresTLSOffLoopback(t *testing.T) {
	p := ServerPolicy{AllowBackendOverride: true, RequireTLSForRemoteEndpoints: true}
	if err := p.Check(OpenParams{BackendURL: "http://127.0.0.1:8473"}, 0); err != nil {
		t.Fatalf("loopback http was refused: %v", err)
	}
	if err := p.Check(OpenParams{BackendURL: "http://backend.example"}, 0); err == nil {
		t.Fatal("remote plaintext was permitted under RequireTLSForRemoteEndpoints")
	}
}

// --- Credential authority (P0-3) ---

func TestPolicyPinsCredentialFilesByDefault(t *testing.T) {
	p := ServerPolicy{}
	for _, tc := range []struct {
		field string
		in    OpenParams
	}{
		{"apiKeyFile", OpenParams{APIKeyFile: "/home/someone/.keys/other-account"}},
		{"mcpTokenFile", OpenParams{McpTokenFile: "/proc/self/environ"}},
	} {
		err := p.Check(tc.in, 0)
		var pe *PolicyError
		if !errors.As(err, &pe) || pe.Field != tc.field {
			t.Errorf("%s was not refused by an installed policy: %v", tc.field, err)
		}
	}
}

// The allowlist holds EXACT files, and a symlink pointing at one of them does not count
// as being one of them in either direction — both sides resolve before comparison.
func TestPolicyAllowsOnlyExactCredentialFiles(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "backend.key")
	if err := os.WriteFile(real, []byte("fake-test-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(dir, "other.key")
	if err := os.WriteFile(other, []byte("fake-test-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.key")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	p := ServerPolicy{AllowCredentialOverride: true, AllowedAPIKeyFiles: []string{real}}
	if err := p.Check(OpenParams{APIKeyFile: real}, 0); err != nil {
		t.Fatalf("the allowlisted file was refused: %v", err)
	}
	// A symlink TO the allowlisted file resolves to it, so it is the same credential.
	if err := p.Check(OpenParams{APIKeyFile: link}, 0); err != nil {
		t.Fatalf("a symlink to the allowlisted file was refused: %v", err)
	}
	if err := p.Check(OpenParams{APIKeyFile: other}, 0); err == nil {
		t.Fatal("a file outside the allowlist was permitted")
	}
}

// --- Symlink-safe root confinement (P0-4) ---

// The finding this closes: a lexical prefix check makes "allowed root" a set of STRINGS.
// /srv/projects/link is textually inside /srv/projects even when link points at /etc.
func TestPolicyRootConfinementSurvivesASymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	p := ServerPolicy{AllowedProjectRoots: []string{root}}

	if err := p.Check(OpenParams{Project: filepath.Join(root, "inside")}, 0); err != nil {
		t.Fatalf("a genuinely-inside path was refused: %v", err)
	}
	if err := p.Check(OpenParams{Project: link}, 0); err == nil {
		t.Fatal("a symlink out of the allowed root was accepted as being inside it")
	}
	if err := p.Check(OpenParams{Project: filepath.Join(link, "deeper")}, 0); err == nil {
		t.Fatal("a path THROUGH an escaping symlink was accepted")
	}
}

// A state or log root is routinely created by the open that names it, so confinement has
// to work on a path that does not exist yet — while still resolving the ancestors it
// hangs off.
func TestPolicyConfinesNotYetExistingPathsThroughTheirAncestors(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	p := ServerPolicy{AllowedStateRoots: []string{root}}

	if err := p.Check(OpenParams{StateDir: filepath.Join(root, "a", "b", "c")}, 0); err != nil {
		t.Fatalf("a not-yet-created path inside the root was refused: %v", err)
	}
	if err := p.Check(OpenParams{StateDir: filepath.Join(link, "a", "b")}, 0); err == nil {
		t.Fatal("a not-yet-created path behind an escaping symlink was accepted")
	}
}

// A symlinked ROOT is the mirror case: the allowlist entry itself resolves, so naming
// the resolved directory is naming the same place.
func TestPolicyResolvesTheAllowlistedRootToo(t *testing.T) {
	real := t.TempDir()
	parent := t.TempDir()
	link := filepath.Join(parent, "root-link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	p := ServerPolicy{AllowedProjectRoots: []string{link}}
	if err := p.Check(OpenParams{Project: filepath.Join(real, "project")}, 0); err != nil {
		t.Fatalf("the resolved form of a symlinked root was refused: %v", err)
	}
}

// --- The unsafe configuration must take more code than the safe one (P0-7) ---

func TestServeModelFacingRefusesTheUnconfinedMarker(t *testing.T) {
	err := ServeModelFacing(context.Background(), Options{
		Factory:    func(_, _ context.Context, _ OpenParams) (Runtime, error) { return newFakeRuntime("ses"), nil },
		Unconfined: &TrustedUnconfined{},
	})
	if err == nil || !strings.Contains(err.Error(), "unconfined") {
		t.Fatalf("a model-facing server accepted the unconfined marker: %v", err)
	}
}

// --- Path spelling: the policy and the code that opens the path must agree ---

// The escape this closes: config trims every value it resolves and the session layer
// deliberately does not, so "/repo/state " (a symlink into the allowed root) and
// "/repo/state" (an attacker-controlled directory outside it) are the same argument to
// one layer and different paths to the other.
func TestPolicyRefusesPathsPaddedWithWhitespace(t *testing.T) {
	root := t.TempDir()
	p := ServerPolicy{
		AllowedProjectRoots:     []string{root},
		AllowedStateRoots:       []string{root},
		AllowedLogRoots:         []string{root},
		AllowCredentialOverride: true,
	}.Canonicalize()

	for _, tc := range []struct {
		field string
		in    OpenParams
	}{
		{"project", OpenParams{Project: filepath.Join(root, "p") + " "}},
		{"stateDir", OpenParams{StateDir: " " + filepath.Join(root, "s")}},
		{"logDir", OpenParams{LogDir: filepath.Join(root, "l") + "\t"}},
		{"apiKeyFile", OpenParams{APIKeyFile: filepath.Join(root, "k") + " "}},
	} {
		err := p.Check(tc.in, 0)
		var pe *PolicyError
		if !errors.As(err, &pe) || pe.Field != tc.field {
			t.Errorf("a whitespace-padded %s was accepted: %v", tc.field, err)
		}
	}
}

// filepath.Abs cleans "a/link/../b" to "a/b" LEXICALLY, before any symlink is followed,
// while the kernel follows `link` first and lands somewhere else. Every check built on
// the cleaned path is answering a question about a path that will never be opened.
func TestPolicyRefusesDotDotComponents(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	p := ServerPolicy{AllowedProjectRoots: []string{root}}.Canonicalize()

	// Built by concatenation, not filepath.Join — Join CLEANS, which would eliminate the
	// ".." before the policy ever saw it and quietly turn this into a different test.
	//
	// Lexically this cleans to <root>/target and passes; the kernel resolves `link`
	// first and lands at <outside>/../target, which is not inside the root at all.
	escape := root + string(filepath.Separator) + "link" + string(filepath.Separator) +
		".." + string(filepath.Separator) + "target"
	err := p.Check(OpenParams{Project: escape}, 0)
	var pe *PolicyError
	if !errors.As(err, &pe) || pe.Field != "project" {
		t.Fatalf("a path traversing a symlink via .. was accepted: %v", err)
	}
}

// --- The ceiling must not move after launch (F4) ---

// A root the operator named as a symlink was re-followed on EVERY check, so retargeting
// that link while the server ran widened the ceiling. Canonicalize pins it once.
func TestCanonicalizePinsASymlinkedRootAtInstallTime(t *testing.T) {
	real := t.TempDir()
	elsewhere := t.TempDir()
	parent := t.TempDir()
	link := filepath.Join(parent, "root-link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	pinned := ServerPolicy{AllowedProjectRoots: []string{link}}.Canonicalize()

	// Retarget the link, exactly as an attacker with write access to its directory would.
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, link); err != nil {
		t.Fatal(err)
	}

	if err := pinned.Check(OpenParams{Project: filepath.Join(real, "p")}, 0); err != nil {
		t.Errorf("the pinned root stopped admitting its original target: %v", err)
	}
	if err := pinned.Check(OpenParams{Project: filepath.Join(elsewhere, "p")}, 0); err == nil {
		t.Error("retargeting the symlink moved the ceiling")
	}
}

// A root that cannot be resolved must stay in the allowlist in its lexical form. Dropping
// it would empty the list, and an empty list means UNCONFINED — the one outcome a
// resolution failure must never produce.
func TestCanonicalizeKeepsAnUnresolvableRootRatherThanEmptyingTheList(t *testing.T) {
	pinned := ServerPolicy{AllowedProjectRoots: []string{"/definitely/not/here/at/all"}}.Canonicalize()
	if len(pinned.AllowedProjectRoots) != 1 {
		t.Fatalf("an unresolvable root was dropped: %#v", pinned.AllowedProjectRoots)
	}
	if err := pinned.Check(OpenParams{Project: "/tmp"}, 0); err == nil {
		t.Error("dropping an unresolvable root left the policy unconfined")
	}
}

// --- resolvePath's error handling (F6) ---

// A component that ENOENTs but is present to Lstat is a DANGLING SYMLINK, not a missing
// name — and its target decides where the eventual mkdir lands.
func TestResolvePathRefusesADanglingSymlink(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "dangling")
	if err := os.Symlink(filepath.Join(dir, "nothing-here"), link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := resolvePath(filepath.Join(link, "state")); err == nil {
		t.Error("a path through a dangling symlink resolved as though the name were merely missing")
	}
}

func TestResolvePathResolvesANotYetCreatedTail(t *testing.T) {
	dir := t.TempDir()
	got, err := resolvePath(filepath.Join(dir, "a", "b", "c"))
	if err != nil {
		t.Fatalf("a not-yet-created path failed to resolve: %v", err)
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(real, "a", "b", "c"); got != want {
		t.Errorf("resolvePath = %q, want %q", got, want)
	}
}

// --- Loopback detection (F7) ---

// "127." as a string prefix is not loopback: 127.attacker.example is an ordinary remote
// hostname that happens to start with those four characters.
func TestRemoteHostnameStartingWith127IsNotLoopback(t *testing.T) {
	p := ServerPolicy{AllowBackendOverride: true, RequireTLSForRemoteEndpoints: true}
	if err := p.Check(OpenParams{BackendURL: "http://127.attacker.example/"}, 0); err == nil {
		t.Fatal("a remote hostname beginning \"127.\" was treated as loopback")
	}
	for _, ok := range []string{"http://127.0.0.1:8473", "http://localhost:8473", "http://[::1]:8473"} {
		if err := p.Check(OpenParams{BackendURL: ok}, 0); err != nil {
			t.Errorf("%s was refused as non-loopback: %v", ok, err)
		}
	}
	// Not loopback, and deliberately not treated as such.
	for _, bad := range []string{"http://0.0.0.0:8473", "http://[::]:8473"} {
		if err := p.Check(OpenParams{BackendURL: bad}, 0); err == nil {
			t.Errorf("%s was treated as loopback", bad)
		}
	}
}

// --- The unsafe configuration must take more code than the safe one (F8) ---

// An installed-but-empty policy confines nothing. "Has a policy" and "is confined" are
// different claims, and the model-facing constructor must check the second one.
func TestServeModelFacingRefusesAPolicyThatConfinesNothing(t *testing.T) {
	err := ServeModelFacing(context.Background(), Options{
		Factory: func(_, _ context.Context, _ OpenParams) (Runtime, error) { return newFakeRuntime("ses"), nil },
	})
	if err == nil || !strings.Contains(err.Error(), "tier ceiling") {
		t.Fatalf("a model-facing server accepted an empty policy: %v", err)
	}
}

// Delegation is gated SEPARATELY from auto-approve, because it is a separate question.
// Under delegate the caller agent settles each confirmation — which is useful for a
// harness and wrong for an unattended loop over a repository that can steer that agent —
// and only the operator knows which they launched.
func TestPolicyGatesDelegatedApprovalsIndependentlyOfAutoApprove(t *testing.T) {
	strict := ServerPolicy{MaxTier: domain.TierSystem}
	err := strict.Check(OpenParams{Approvals: ApprovalDelegate}, 0)
	var pe *PolicyError
	if !errors.As(err, &pe) {
		t.Fatalf("delegate was permitted under a policy that did not allow it: %v", err)
	}
	if !strings.Contains(pe.Reason, "calling agent") {
		t.Errorf("the refusal does not say who would have been deciding: %q", pe.Reason)
	}

	// Allowing auto-approve must NOT imply allowing delegation, nor the reverse: they
	// are different grants and a policy that conflated them would surprise in both
	// directions.
	// The one direction that DOES follow: auto is strictly the broader grant (it runs
	// every tier-permitted call with nothing consulted), so an operator who allowed it
	// cannot coherently be refusing the mode that reviews each call first. Refusing
	// would push a caller toward the mode that reviews none.
	autoOnly := ServerPolicy{AllowAutoApprove: true}
	if err := autoOnly.Check(OpenParams{Approvals: ApprovalDelegate}, 0); err != nil {
		t.Errorf("a server that allows auto refused the narrower delegate: %v", err)
	}
	delegateOnly := ServerPolicy{AllowDelegatedApprovals: true}
	if err := delegateOnly.Check(OpenParams{Approvals: ApprovalAuto}, 0); err == nil {
		t.Error("AllowDelegatedApprovals silently enabled auto-approve")
	}
	if err := delegateOnly.Check(OpenParams{Approvals: ApprovalDelegate}, 0); err != nil {
		t.Errorf("an explicitly allowed delegate was refused: %v", err)
	}
}

// The mode name is the thing this phase is fixing: "ask" implied a human. Every pending
// approval now states whose decision it actually is, in the payload, so a caller does not
// have to infer authority from a word.
func TestPendingApprovalNamesItsDecisionAuthority(t *testing.T) {
	if got := ApprovalDelegate.DecisionAuthority(); got != "caller-agent" {
		t.Errorf("delegate authority = %q, want caller-agent", got)
	}
	for _, m := range []ApprovalMode{ApprovalDecline, ApprovalAuto} {
		if got := m.DecisionAuthority(); got != "none" {
			t.Errorf("%q authority = %q, want none", m, got)
		}
	}
	if ApprovalDelegate.DecisionAuthority() == "human" {
		t.Error("delegation must never describe itself as human authorization")
	}

	a := NewApprovals(ApprovalDelegate, 2*time.Second)
	done := make(chan bool, 1)
	go func() {
		done <- a.Confirm(context.Background(), ApprovalRequest{
			Tool: "git.push", Risk: domain.RiskGit, NeedsTypedConfirm: true, RunID: "mrun_1",
		})
	}()
	var pending []PendingApproval
	for i := 0; i < 200 && len(pending) == 0; i++ {
		pending = a.Pending()
		if len(pending) == 0 {
			time.Sleep(5 * time.Millisecond)
		}
	}
	if len(pending) != 1 {
		t.Fatalf("expected one parked approval, got %d", len(pending))
	}
	if pending[0].DecisionAuthority != "caller-agent" {
		t.Errorf("pending approval authority = %q, want caller-agent", pending[0].DecisionAuthority)
	}
	a.Resolve(pending[0].ID, DecisionRejected)
	<-done
}

// needsTypedConfirm lost omitempty: a caller distinguishing "no extra friction needed"
// from "the peer is too old to tell me" cannot do it from an absent field, and the
// approval whose friction requirement silently vanished is the one an automated caller
// waves through.
func TestNeedsTypedConfirmIsAlwaysPresentOnTheWire(t *testing.T) {
	body, err := json.Marshal(PendingApproval{ID: "apr_1", Tool: "fs.read", NeedsTypedConfirm: false})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"needsTypedConfirm":false`) {
		t.Errorf("a false needsTypedConfirm was omitted from the wire shape: %s", body)
	}
	if !strings.Contains(string(body), `"decisionAuthority"`) {
		t.Errorf("decisionAuthority was omitted from the wire shape: %s", body)
	}
}

// A half-done rename is worse than no rename: the mode is refused under its old name
// while the error, the schemas and the guidance still recommend it, which reads to a
// model as a server bug and becomes a retry loop. This pins the whole model-facing
// surface at once.
func TestTheApprovalModeVocabularyIsDelegateEverywhereOnTheWire(t *testing.T) {
	surfaces := map[string]string{
		"server instructions": instructions,
	}
	// Every tool schema and description the SDK will generate, taken from the struct
	// tags rather than re-derived, so a tag that kept the old word is caught here.
	for name, tag := range map[string]string{
		"OpenInput.Approvals":         fieldTag(t, OpenInput{}, "Approvals"),
		"OpenInput.ApprovalTimeoutMs": fieldTag(t, OpenInput{}, "ApprovalTimeoutMs"),
		"ApprovalsOutput.Mode":        fieldTag(t, ApprovalsOutput{}, "Mode"),
	} {
		surfaces[name] = tag
	}

	for where, text := range surfaces {
		lower := strings.ToLower(text)
		// The bare word "ask" appears legitimately in "daintree.ask" and in prose like
		// "never ask", so match the shapes that would actually mislead a caller into
		// sending approvals:"ask".
		for _, bad := range []string{`approvals is ask`, `approvals:"ask"`, `"ask"`, `is ask.`} {
			if strings.Contains(lower, strings.ToLower(bad)) {
				t.Errorf("%s still advertises the old mode name via %q: %s", where, bad, text)
			}
		}
	}

	// And the refusal a caller gets for the old name must name the new one.
	if !strings.Contains(instructions, "delegate") {
		t.Error("the server instructions never mention the delegate mode")
	}
}

// fieldTag returns a struct field's jsonschema description.
func fieldTag(t *testing.T, v any, field string) string {
	t.Helper()
	f, ok := reflect.TypeOf(v).FieldByName(field)
	if !ok {
		t.Fatalf("no field %q on %T", field, v)
	}
	return f.Tag.Get("jsonschema")
}
