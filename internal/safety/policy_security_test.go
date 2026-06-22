package safety

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// TestDecideMatrix ports the decide() cases from policy.test.ts: the full
// (risk, tier) allowed/needsConfirmation contract plus the higher-tier reason.
func TestDecideMatrix(t *testing.T) {
	// supervisor denies 'terminal' with no confirmation and a higher-tier reason.
	d := Decide(domain.RiskTerminal, domain.TierSupervisor)
	if d.Allowed || d.NeedsConfirmation {
		t.Fatalf("supervisor+terminal should be denied no-confirm, got %+v", d)
	}
	if d.Reason == "" {
		t.Fatal("denied decision must carry a reason")
	}

	// operator allows 'project' with confirmation.
	if d := Decide(domain.RiskProject, domain.TierOperator); !d.Allowed || !d.NeedsConfirmation {
		t.Fatalf("operator+project should be allowed+confirm, got %+v", d)
	}
	// operator denies 'git' (needs system).
	if d := Decide(domain.RiskGit, domain.TierOperator); d.Allowed || d.NeedsConfirmation {
		t.Fatalf("operator+git should be denied, got %+v", d)
	}
	// scoped approval suppresses confirmation for a confirm-risk class.
	if d := Decide(domain.RiskProject, domain.TierOperator, DecideOptions{HasScopedApproval: true}); !d.Allowed || d.NeedsConfirmation {
		t.Fatalf("scoped approval should suppress confirm, got %+v", d)
	}
	// read & local never need confirmation in any tier.
	for _, tier := range []domain.Tier{domain.TierSupervisor, domain.TierOperator, domain.TierSystem} {
		for _, risk := range []domain.RiskClass{domain.RiskRead, domain.RiskLocal} {
			if d := Decide(risk, tier); !d.Allowed || d.NeedsConfirmation {
				t.Fatalf("%s+%s should be allowed no-confirm, got %+v", tier, risk, d)
			}
		}
	}
}

// TestTierAllowsRiskFullMatrix mirrors the exhaustive matrix in policy.test.ts:
// each tier's exact allowed risk-class set, every other class denied.
func TestTierAllowsRiskFullMatrix(t *testing.T) {
	allowed := map[domain.Tier][]domain.RiskClass{
		domain.TierSupervisor: {domain.RiskRead, domain.RiskLocal, domain.RiskUI},
		domain.TierOperator: {
			domain.RiskRead, domain.RiskLocal, domain.RiskUI,
			domain.RiskTerminal, domain.RiskProject, domain.RiskExternal,
		},
		domain.TierSystem: {
			domain.RiskRead, domain.RiskLocal, domain.RiskUI,
			domain.RiskTerminal, domain.RiskProject, domain.RiskExternal,
			domain.RiskGit, domain.RiskSystem,
		},
	}
	all := []domain.RiskClass{
		domain.RiskRead, domain.RiskLocal, domain.RiskUI,
		domain.RiskTerminal, domain.RiskProject, domain.RiskExternal,
		domain.RiskGit, domain.RiskSystem,
	}
	for tier, list := range allowed {
		set := map[domain.RiskClass]bool{}
		for _, r := range list {
			set[r] = true
		}
		for _, risk := range all {
			want := set[risk]
			if got := TierAllowsRisk(tier, risk); got != want {
				t.Errorf("TierAllowsRisk(%s,%s)=%v want %v", tier, risk, got, want)
			}
		}
	}
}

// TestIsSensitivePathVariantMatrix ports fsToolsSecurity.test.ts isSensitivePath
// (#1): the basename-only check missed segment/suffix/case variants. Every
// credential-bearing shape must flag; ordinary source files must not.
func TestIsSensitivePathVariantMatrix(t *testing.T) {
	sensitive := []string{
		// basenames / suffixes / dirs the TS suite asserts.
		".env", "config/.env.production", "server.key", "certs/cert.pem",
		".ssh/id_ed25519", "home/.aws/credentials",
		// segment / suffix / case variants the basename-only check missed.
		"config/prod.env", // *.env suffix anywhere
		"nested/.env/x",   // .env as a directory segment
		"FOO.ENV",         // uppercase *.env
		"Server.KEY",      // uppercase .key suffix
	}
	for _, p := range sensitive {
		if !IsSensitivePath(p) {
			t.Errorf("IsSensitivePath(%q) should be true", p)
		}
	}
	ordinary := []string{"src/app.ts", "README.md", "environment.ts"}
	for _, p := range ordinary {
		if IsSensitivePath(p) {
			t.Errorf("IsSensitivePath(%q) should be false (ordinary source)", p)
		}
	}
}

// TestResolveInsideProjectSymlinkEscape ports noFileEditGuard.test.ts: a
// repo-local symlink that resolves OUTSIDE the project is blocked even though it
// is lexically inside; a genuine in-project file still resolves.
func TestResolveInsideProjectSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation may require privilege on Windows")
	}
	proj := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("top secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(proj, "escape")); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}

	// Lexically inside the project, but the symlink resolves outside it.
	if _, err := ResolveInsideProject(proj, "escape/secret.txt"); err == nil {
		t.Error("symlink escaping the project must be blocked")
	} else if _, ok := err.(*FileEditAttemptError); !ok {
		t.Errorf("expected *FileEditAttemptError, got %T", err)
	}

	// A real file genuinely inside the project still resolves fine.
	if err := os.WriteFile(filepath.Join(proj, "ok.txt"), []byte("fine"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveInsideProject(proj, "ok.txt")
	if err != nil {
		t.Fatalf("in-project file should resolve: %v", err)
	}
	if want := filepath.Join(proj, "ok.txt"); got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

// TestIsForbiddenToolNameCaseAndPatch ports noFileEditGuard.test.ts: the guard is
// case-insensitive and catches apply_patch / edit_file; read-only names are not
// flagged, and AssertNoFileEditTools names every offender.
func TestIsForbiddenToolNameCaseAndPatch(t *testing.T) {
	for _, name := range []string{"fs.write", "apply_patch", "edit_file", "FS.WRITE", "APPLY_PATCH", "Edit_File"} {
		if !IsForbiddenToolName(name) {
			t.Errorf("IsForbiddenToolName(%q) should be true (case-insensitive)", name)
		}
	}
	for _, name := range []string{"fs.read", "fs.list", "queue.publish", ""} {
		if IsForbiddenToolName(name) {
			t.Errorf("IsForbiddenToolName(%q) should be false", name)
		}
	}
	// The error names BOTH offenders, not just the first.
	err := AssertNoFileEditTools([]string{"fs.read", "apply_patch", "fs.write"})
	if err == nil {
		t.Fatal("expected rejection")
	}
	msg := err.Error()
	for _, want := range []string{"apply_patch", "fs.write"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error should name offender %q, got %q", want, msg)
		}
	}
}

// Finding 7: the hardened forbidden-fragment list must catch common mutating verbs
// and the OpenAI wire form (fs__write) so a write tool can't be wired in under a
// near-miss name, and AssertSafe at startup rejects it.
func TestForbiddenFragmentsHardening(t *testing.T) {
	// Each synthetic mutating name (incl. case/wire-form variants) must be forbidden.
	forbidden := []string{
		"fs__write",          // OpenAI wire form of fs.write
		"workspace.saveFile", // saveFile verb
		"project.deleteFile", // deleteFile verb
		"git.removeFile",     // removeFile verb
		"editor.renameFile",  // renameFile verb
		"file.delete", "file.remove", "file.rename", "file.save",
		"workspace.patch", "patchFile",
		"FS__WRITE", "Project.DeleteFile", // case-insensitive
	}
	for _, name := range forbidden {
		if !IsForbiddenToolName(name) {
			t.Errorf("IsForbiddenToolName(%q) should be true (hardened fragment)", name)
		}
	}
	// AssertNoFileEditTools rejects a synthetic forbidden name at startup.
	if err := AssertNoFileEditTools([]string{"fs.read", "workspace.saveFile"}); err == nil {
		t.Fatal("AssertNoFileEditTools must reject a synthetic forbidden name")
	}
	// Benign read-only names are still allowed (no over-matching).
	for _, name := range []string{"fs.read", "fs.list", "fs.search", "queue.publish", "artifact.read", "audit.export"} {
		if IsForbiddenToolName(name) {
			t.Errorf("IsForbiddenToolName(%q) should be false (benign read-only tool)", name)
		}
	}
}
