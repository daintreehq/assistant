package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The exit-code contract: non-zero iff something FAILED. A gate that also fires on
// warnings or on "could not check" is a gate people learn to ignore, and then it protects
// nothing at all.
func TestDoctorHealthyIsFailuresOnly(t *testing.T) {
	cases := []struct {
		name    string
		status  DoctorStatus
		healthy bool
	}{
		{"ok", StatusOK, true},
		{"warn", StatusWarn, true},
		{"unknown", StatusUnknown, true},
		{"skip", StatusSkip, true},
		{"fail", StatusFail, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &DoctorReport{}
			r.Add(DoctorCheck{ID: "x", Status: tc.status})
			r.Finalize()
			if r.Summary.Healthy != tc.healthy {
				t.Errorf("status %s → healthy=%v, want %v", tc.status, r.Summary.Healthy, tc.healthy)
			}
		})
	}
}

// Every failing check must carry a next action. An error a tester cannot act on is a
// support ticket by construction — which is exactly what doctor exists to prevent.
func TestDoctorFailuresCarryAHint(t *testing.T) {
	dir := t.TempDir()
	notADir := filepath.Join(dir, "afile")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []DoctorCheck{
		CheckStateDir(filepath.Join(dir, "does-not-exist")),
		CheckStateDir(notADir),
		CheckAutoApprove(true, "system"),
	} {
		if c.Status != StatusFail && c.Status != StatusWarn {
			continue
		}
		if strings.TrimSpace(c.Hint) == "" {
			t.Errorf("%s is %s with no hint — the tester has nothing to do next", c.ID, c.Status)
		}
	}
}

func TestCheckStateDirDetectsWritabilityAndPrivacy(t *testing.T) {
	// Owner-only: the happy path.
	private := t.TempDir()
	if err := os.Chmod(private, 0o700); err != nil {
		t.Fatal(err)
	}
	if c := CheckStateDir(private); c.Status != StatusOK {
		t.Errorf("a 0700 dir should pass, got %s: %s", c.Status, c.Detail)
	}

	// World-readable: a warning, because the dir holds the conversation, the audit
	// trail, grants, and (at the user root) the spendable API key.
	open := t.TempDir()
	if err := os.Chmod(open, 0o755); err != nil {
		t.Fatal(err)
	}
	c := CheckStateDir(open)
	if c.Status != StatusWarn {
		t.Errorf("a 0755 state dir should warn, got %s", c.Status)
	}
	if !strings.Contains(c.Hint, "chmod 700") {
		t.Errorf("the hint should give the exact command: %q", c.Hint)
	}

	// Missing entirely: a failure, not a warning — nothing will persist.
	if c := CheckStateDir(filepath.Join(t.TempDir(), "nope")); c.Status != StatusFail {
		t.Errorf("a missing state dir should fail, got %s", c.Status)
	}
}

// Mode bits lie under ACLs, read-only mounts and container filesystems, so writability is
// proven by writing. A "looks writable but is not" state dir otherwise fails much later,
// mid-turn, as a confusing SQLite error.
func TestCheckStateDirProvesWritabilityByWriting(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("root ignores mode bits; this check is about the non-root case")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil { // r-x: readable, NOT writable
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	c := CheckStateDir(dir)
	if c.Status != StatusFail {
		t.Errorf("an unwritable dir must FAIL, got %s: %s", c.Status, c.Detail)
	}
	if !strings.Contains(c.Hint, "DAINTREE_ASSISTANT_STATE_DIR") {
		t.Errorf("the hint should offer the override: %q", c.Hint)
	}
	// And the probe must not leave litter behind.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), "doctor-write-probe") {
			t.Error("the write probe was left behind")
		}
	}
}

// AUTO_APPROVE is invisible from every other surface and changes what the assistant may
// do without asking. A diagnostic that omits it is missing the most important fact.
func TestCheckAutoApproveIsConspicuousWhenOn(t *testing.T) {
	off := CheckAutoApprove(false, "operator")
	if off.Status != StatusOK || !strings.Contains(off.Detail, "off") {
		t.Errorf("off should read as ok: %+v", off)
	}
	on := CheckAutoApprove(true, "system")
	if on.Status != StatusWarn {
		t.Errorf("on must warn, got %s", on.Status)
	}
	if !strings.Contains(on.Detail, "WITHOUT asking") || !strings.Contains(on.Detail, "system") {
		t.Errorf("the detail must say what it means AND at which tier: %q", on.Detail)
	}
}

// The JSON form is the machine contract: a stable id per check, and a summary a caller
// can branch on without walking the list.
func TestDoctorReportJSONShape(t *testing.T) {
	r := &DoctorReport{Version: "1.2.3", Platform: "darwin/arm64"}
	r.Add(DoctorCheck{ID: "auth.credentialUsable", Label: "upstream credential", Status: StatusOK})
	r.Add(DoctorCheck{ID: "backend.reachable", Label: "backend", Status: StatusFail, Hint: "check the network"})
	r.Finalize()

	var buf strings.Builder
	if err := r.WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal([]byte(buf.String()), &back); err != nil {
		t.Fatalf("doctor --json emitted invalid JSON: %v", err)
	}
	if back["version"] != "1.2.3" || back["platform"] != "darwin/arm64" {
		t.Errorf("version/platform missing: %v", back)
	}
	summary, _ := back["summary"].(map[string]any)
	if summary["healthy"] != false || summary["fail"] != float64(1) {
		t.Errorf("summary should report the failure: %v", summary)
	}
	checks, _ := back["checks"].([]any)
	if len(checks) != 2 {
		t.Fatalf("want 2 checks, got %d", len(checks))
	}
	first, _ := checks[0].(map[string]any)
	if first["id"] != "auth.credentialUsable" {
		t.Errorf("check ids must be stable and present: %v", first)
	}
}

// Human output must be readable with colour stripped — doctor is read over screen shares
// and in pasted terminal output, where hue is not available.
func TestDoctorHumanOutputIsReadableWithoutColour(t *testing.T) {
	r := &DoctorReport{Version: "dev", Platform: "linux/amd64"}
	r.Add(DoctorCheck{ID: "a.ok", Label: "fine", Status: StatusOK, Detail: "all good"})
	r.Add(DoctorCheck{ID: "b.bad", Label: "broken", Status: StatusFail, Detail: "it broke", Hint: "do this"})
	r.Finalize()

	var buf strings.Builder
	renderDoctorHuman(&buf, r)
	out := buf.String()
	for _, want := range []string{"FAIL", "ok", "it broke", "do this", "Failing checks: b.bad"} {
		if !strings.Contains(out, want) {
			t.Errorf("human output is missing %q:\n%s", want, out)
		}
	}
}

// A hint under a GREEN line trains people to stop reading the lines that matter.
func TestDoctorHumanOutputHidesHintsOnPassingChecks(t *testing.T) {
	r := &DoctorReport{}
	r.Add(DoctorCheck{ID: "a.ok", Label: "fine", Status: StatusOK, Detail: "good", Hint: "SHOULD NOT APPEAR"})
	r.Finalize()

	var buf strings.Builder
	renderDoctorHuman(&buf, r)
	if strings.Contains(buf.String(), "SHOULD NOT APPEAR") {
		t.Errorf("a passing check printed its hint:\n%s", buf.String())
	}
}
