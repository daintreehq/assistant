package safety

import (
	"path/filepath"
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
)

func TestTierGatingMatrix(t *testing.T) {
	cases := []struct {
		tier  domain.Tier
		risk  domain.RiskClass
		allow bool
	}{
		{domain.TierSupervisor, domain.RiskRead, true},
		{domain.TierSupervisor, domain.RiskLocal, true},
		{domain.TierSupervisor, domain.RiskUI, true},
		{domain.TierSupervisor, domain.RiskTerminal, false},
		{domain.TierSupervisor, domain.RiskGit, false},
		{domain.TierOperator, domain.RiskTerminal, true},
		{domain.TierOperator, domain.RiskProject, true},
		{domain.TierOperator, domain.RiskExternal, true},
		{domain.TierOperator, domain.RiskGit, false},    // operator lacks git
		{domain.TierOperator, domain.RiskSystem, false}, // operator lacks system
		{domain.TierSystem, domain.RiskGit, true},
		{domain.TierSystem, domain.RiskSystem, true},
	}
	for _, c := range cases {
		if got := TierAllowsRisk(c.tier, c.risk); got != c.allow {
			t.Errorf("TierAllowsRisk(%s,%s)=%v want %v", c.tier, c.risk, got, c.allow)
		}
	}
}

func TestAlwaysConfirmSet(t *testing.T) {
	confirm := []domain.RiskClass{domain.RiskTerminal, domain.RiskProject, domain.RiskGit, domain.RiskExternal, domain.RiskSystem}
	noConfirm := []domain.RiskClass{domain.RiskRead, domain.RiskLocal, domain.RiskUI}
	for _, r := range confirm {
		if !AlwaysConfirm(r) {
			t.Errorf("AlwaysConfirm(%s) should be true", r)
		}
	}
	for _, r := range noConfirm {
		if AlwaysConfirm(r) {
			t.Errorf("AlwaysConfirm(%s) should be false", r)
		}
	}
}

func TestNeedsTypedConfirm(t *testing.T) {
	// Only git/system (the irreversible subset of always-confirm) demand a typed phrase.
	typed := []domain.RiskClass{domain.RiskGit, domain.RiskSystem}
	single := []domain.RiskClass{
		domain.RiskRead, domain.RiskLocal, domain.RiskUI,
		domain.RiskTerminal, domain.RiskProject, domain.RiskExternal,
	}
	for _, r := range typed {
		if !NeedsTypedConfirm(r) {
			t.Errorf("NeedsTypedConfirm(%s) should be true", r)
		}
	}
	for _, r := range single {
		if NeedsTypedConfirm(r) {
			t.Errorf("NeedsTypedConfirm(%s) should be false", r)
		}
	}
	// An unknown/bogus risk class is never typed-confirm (zero value of the set).
	if NeedsTypedConfirm(domain.RiskClass("bogus")) {
		t.Error("NeedsTypedConfirm(bogus) should be false")
	}
	// Every typed-confirm class is also an always-confirm class (it is a strict subset).
	for _, r := range typed {
		if !AlwaysConfirm(r) {
			t.Errorf("typed-confirm class %s must also be always-confirm", r)
		}
	}
}

func TestDecide(t *testing.T) {
	// Tier-denied: read-only tier cannot do git.
	d := Decide(domain.RiskGit, domain.TierSupervisor)
	if d.Allowed {
		t.Fatal("supervisor should not be allowed git")
	}
	if d.Reason == "" {
		t.Fatal("denied decision must carry a reason")
	}

	// Allowed but needs confirmation.
	d = Decide(domain.RiskGit, domain.TierSystem)
	if !d.Allowed || !d.NeedsConfirmation {
		t.Fatalf("system+git should be allowed and need confirmation, got %+v", d)
	}

	// Allowed, no confirmation (read).
	d = Decide(domain.RiskRead, domain.TierSupervisor)
	if !d.Allowed || d.NeedsConfirmation {
		t.Fatalf("read should be allowed, no confirm, got %+v", d)
	}

	// Pre-resolved scoped approval suppresses confirmation.
	d = Decide(domain.RiskGit, domain.TierSystem, DecideOptions{HasScopedApproval: true})
	if !d.Allowed || d.NeedsConfirmation {
		t.Fatalf("scoped approval should suppress confirmation, got %+v", d)
	}
}

func TestAssertNoFileEditTools(t *testing.T) {
	// Clean set passes.
	if err := AssertNoFileEditTools([]string{"fs.read", "fs.list", "daintree.call"}); err != nil {
		t.Fatalf("clean set should pass: %v", err)
	}
	// Each forbidden fragment is caught (case-insensitive, substring).
	forbidden := []string{
		"fs.write", "fs.edit", "file.write", "file.edit",
		"write_file", "WriteFile", "apply_patch", "APPLYPATCH",
		"edit_file", "editfile", "patch.apply",
		"some.write_file.thing", // substring anywhere
	}
	for _, name := range forbidden {
		if !IsForbiddenToolName(name) {
			t.Errorf("IsForbiddenToolName(%q) should be true", name)
		}
		if err := AssertNoFileEditTools([]string{name}); err == nil {
			t.Errorf("AssertNoFileEditTools should reject %q", name)
		} else if _, ok := err.(*FileEditAttemptError); !ok {
			t.Errorf("expected *FileEditAttemptError for %q, got %T", name, err)
		}
	}
}

func TestSecretGuards(t *testing.T) {
	sensitive := []string{
		".env", "config/.env", "nested/.env.local", "app/prod.env",
		".npmrc", "home/.aws/credentials", "deploy/service-account.json",
		"keys/server.pem", "id_rsa", "secrets/cert.key",
		"project/.ssh/known_hosts", "x/.gnupg/y",
	}
	for _, p := range sensitive {
		if !IsSensitivePath(p) {
			t.Errorf("IsSensitivePath(%q) should be true", p)
		}
	}
	safe := []string{"src/main.go", "README.md", "config/app.yaml", "environment.txt"}
	for _, p := range safe {
		if IsSensitivePath(p) {
			t.Errorf("IsSensitivePath(%q) should be false", p)
		}
	}
}

func TestResolveInsideProject(t *testing.T) {
	root := t.TempDir()

	// In-bounds path resolves.
	got, err := ResolveInsideProject(root, "src/main.go")
	if err != nil {
		t.Fatalf("in-bounds path should resolve: %v", err)
	}
	if want := filepath.Join(root, "src/main.go"); got != want {
		t.Errorf("got %q want %q", got, want)
	}

	// Traversal escapes (lexical pass catches not-yet-existing paths).
	for _, esc := range []string{"../escape", "../../etc/passwd", "a/../../b"} {
		if _, err := ResolveInsideProject(root, esc); err == nil {
			t.Errorf("ResolveInsideProject should reject escape %q", esc)
		} else if _, ok := err.(*FileEditAttemptError); !ok {
			t.Errorf("expected *FileEditAttemptError for %q, got %T", esc, err)
		}
	}

	// The root itself is allowed.
	if _, err := ResolveInsideProject(root, "."); err != nil {
		t.Errorf("root '.' should be allowed: %v", err)
	}
}
