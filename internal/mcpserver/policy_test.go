package mcpserver

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

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
	reg := NewRegistry(context.Background(), func(_, _ context.Context, _ OpenParams) (Runtime, error) {
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
	reg := NewRegistry(context.Background(), func(_, _ context.Context, _ OpenParams) (Runtime, error) {
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
	for _, mode := range []ApprovalMode{ApprovalAsk, ApprovalDecline, ""} {
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
	reg := NewRegistry(context.Background(), func(_, _ context.Context, _ OpenParams) (Runtime, error) {
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
