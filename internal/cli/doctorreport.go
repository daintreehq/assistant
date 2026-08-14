package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// doctorreport.go is doctor's STRUCTURED core: a list of typed checks that both the
// human banner and `doctor --json` render from.
//
// The old doctor printed prose straight to stdout, which made it unusable as the release
// gate it needs to be. Support cannot ask a tester to interpret a paragraph; an installer
// cannot branch on one; and "backend unreachable" and "your key has no credit" both
// arrived as an indistinguishable red line. Typed checks give each condition an id, a
// status, and a next action — so `doctor --json | jq '.checks[] | select(.status=="fail")'`
// is a support workflow, and the human output is one rendering of the same facts rather
// than a separate implementation that can disagree with it.

// DoctorStatus is one check's verdict.
type DoctorStatus string

const (
	// StatusOK — verified working.
	StatusOK DoctorStatus = "ok"
	// StatusWarn — degraded or unverifiable, but not release-blocking. A warning must
	// never be used for "we could not check": that is StatusUnknown, because reporting
	// an unknown as a problem sends people hunting for one they may not have.
	StatusWarn DoctorStatus = "warn"
	// StatusFail — broken, and the reason the exit code is non-zero.
	StatusFail DoctorStatus = "fail"
	// StatusUnknown — the check could not run. Never affects the exit code.
	StatusUnknown DoctorStatus = "unknown"
	// StatusSkip — not applicable in this environment.
	StatusSkip DoctorStatus = "skip"
)

// DoctorCheck is one diagnosed condition.
type DoctorCheck struct {
	// ID is the stable machine name (`backend.reachable`, `binary.duplicates`). Stable
	// across releases so a script or a support runbook can key off it; the Label and
	// Detail are free to be reworded.
	ID     string       `json:"id"`
	Label  string       `json:"label"`
	Status DoctorStatus `json:"status"`
	// Detail is the human-facing finding.
	Detail string `json:"detail,omitempty"`
	// Hint is the ONE next action. Every failing check must carry one — an error a
	// tester cannot act on is a support ticket by construction.
	Hint string `json:"hint,omitempty"`
	// Data carries structured extras for the JSON form (versions, paths, counts) that
	// would clutter the human line.
	Data map[string]any `json:"data,omitempty"`
}

// DoctorReport is the whole diagnosis.
type DoctorReport struct {
	Version  string         `json:"version"`
	Platform string         `json:"platform"`
	Checks   []DoctorCheck  `json:"checks"`
	Summary  DoctorSummary  `json:"summary"`
	Extra    map[string]any `json:"extra,omitempty"`
}

// DoctorSummary is the at-a-glance count, so a caller can branch without walking checks.
type DoctorSummary struct {
	OK      int  `json:"ok"`
	Warn    int  `json:"warn"`
	Fail    int  `json:"fail"`
	Unknown int  `json:"unknown"`
	Healthy bool `json:"healthy"`
}

// Add appends a check.
func (r *DoctorReport) Add(c DoctorCheck) { r.Checks = append(r.Checks, c) }

// Finalize computes the summary. Healthy is false iff something FAILED — a warning or an
// unknown must not gate a release, or the gate becomes noise people learn to ignore.
func (r *DoctorReport) Finalize() {
	r.Summary = DoctorSummary{}
	for _, c := range r.Checks {
		switch c.Status {
		case StatusOK:
			r.Summary.OK++
		case StatusWarn:
			r.Summary.Warn++
		case StatusFail:
			r.Summary.Fail++
		case StatusUnknown:
			r.Summary.Unknown++
		}
	}
	r.Summary.Healthy = r.Summary.Fail == 0
}

// WriteJSON emits the report as a single indented JSON document.
func (r *DoctorReport) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// statusGlyph is the human-output marker. Text, not colour: doctor is read over screen
// shares, in pasted terminal output, and by people with colour disabled, and a diagnosis
// that only distinguishes states by hue is unreadable in all three.
func statusGlyph(s DoctorStatus) string {
	switch s {
	case StatusOK:
		return "ok  "
	case StatusWarn:
		return "WARN"
	case StatusFail:
		return "FAIL"
	case StatusUnknown:
		return "?   "
	default:
		return "-   "
	}
}

// --- Environment checks ------------------------------------------------------------
//
// These are the ones that have nothing to do with the backend or Daintree: which binary
// is actually going to run, whether this platform can do what the assistant promises,
// and whether the state directory is usable and private.

// CheckPlatform reports what this build can actually do here.
//
// Background supervision rests on flock leases and setsid detachment, neither of which
// has a Windows port. The `!unix` builds fail loudly rather than run without exclusion,
// so on Windows timers, watchers, and async work simply stop when the cockpit exits.
// A tester needs to know that BEFORE being told "I'll let you know when it's done".
func CheckPlatform() DoctorCheck {
	c := DoctorCheck{
		ID:    "platform.supervision",
		Label: "platform",
		Data:  map[string]any{"os": runtime.GOOS, "arch": runtime.GOARCH},
	}
	switch runtime.GOOS {
	case "darwin", "linux":
		c.Status = StatusOK
		c.Detail = fmt.Sprintf("%s/%s — background supervision supported", runtime.GOOS, runtime.GOARCH)
	default:
		c.Status = StatusWarn
		c.Detail = fmt.Sprintf("%s/%s — background supervision is NOT supported", runtime.GOOS, runtime.GOARCH)
		c.Hint = "Timers, watchers and async work stop when the assistant closes. Use macOS or Linux for unattended supervision."
	}
	return c
}

// CheckBinaryOnPath finds every daintree-assistant on PATH and reports which one wins.
//
// Daintree launches the CLI by NAME, so an older copy earlier on PATH silently shadows a
// newer one — and the symptom is not "wrong version", it is a feature that mysteriously
// does not exist, or a bug that was fixed weeks ago. Nothing else in the system can
// notice this, because from Daintree's side the resolution is working perfectly.
func CheckBinaryOnPath(selfVersion string) DoctorCheck {
	c := DoctorCheck{ID: "binary.duplicates", Label: "binary on PATH"}

	found := lookPathAll("daintree-assistant")
	c.Data = map[string]any{"paths": found, "count": len(found)}
	// Bound how many we EXECUTE. Every probe runs a binary this code knows nothing about
	// beyond its filename; a pathological PATH with dozens of entries should not turn a
	// diagnostic into a minute of running other people's code. The first few are the ones
	// that can actually shadow, which is the whole question.
	if len(found) > maxVersionProbes {
		c.Data["probeLimited"] = maxVersionProbes
	}

	switch {
	case len(found) == 0:
		// Running from ./bin or `go run` — normal for a contributor, worth noting for a
		// tester, since Daintree will not find it this way.
		c.Status = StatusWarn
		c.Detail = "not on PATH"
		c.Hint = "Daintree resolves the CLI by name. Install it somewhere on PATH (`make install`) or Daintree will report it missing."
	case len(found) == 1:
		c.Status = StatusOK
		c.Detail = found[0] + versionSuffix(found[0], selfVersion)
	default:
		c.Status = StatusWarn
		c.Detail = fmt.Sprintf("%d copies on PATH; Daintree will run %s", len(found), found[0])
		c.Hint = "Remove the copies you are not using — an older one earlier on PATH shadows this build and its fixes."
		probe := found
		if len(probe) > maxVersionProbes {
			probe = probe[:maxVersionProbes]
		}
		versions := make([]string, 0, len(probe))
		for _, p := range probe {
			versions = append(versions, p+versionSuffix(p, selfVersion))
		}
		if len(found) > len(probe) {
			versions = append(versions, fmt.Sprintf("(+%d more, not probed)", len(found)-len(probe)))
		}
		c.Data["resolved"] = found[0]
		c.Data["versions"] = versions
	}
	return c
}

// versionProbeTimeout bounds each `--version` execution.
//
// This runs an executable found on the user's PATH — which is to say, a binary this code
// knows nothing about beyond its name. It may hang, wait on stdin, or be something else
// entirely that happens to be called daintree-assistant. A diagnostic that can hang
// forever is worse than no diagnostic, because it hangs at exactly the moment someone is
// already stuck.
const versionProbeTimeout = 3 * time.Second

// maxVersionProbes bounds how many PATH candidates are executed.
const maxVersionProbes = 5

// versionSuffix renders " (v…)" for a binary, asking it directly.
//
// Asking rather than assuming: the whole point is to detect a DIFFERENT build, so reading
// our own version and printing it beside someone else's path would report exactly the
// agreement we are trying to disprove. A copy that will not answer is reported as such —
// a binary that cannot run is itself the finding.
//
// The output is UNTRUSTED: it comes from a program we did not write. It is bounded in
// time, bounded in length, and stripped of control characters, so a hostile or broken
// binary cannot hang doctor, flood the report, or inject escape sequences into a terminal
// that is about to render it.
func versionSuffix(path, selfVersion string) string {
	ctx, cancel := context.WithTimeout(context.Background(), versionProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	// Never inherit stdin: a binary that waits for input would otherwise block until the
	// timeout on every doctor run.
	cmd.Stdin = nil
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return " (did not respond)"
		}
		return " (could not run)"
	}
	v := sanitizeProbeOutput(string(out))
	v = strings.TrimPrefix(v, "daintree-assistant ")
	if v == "" {
		return ""
	}
	if v == selfVersion {
		return " (" + v + ", this build)"
	}
	// "reports": the string came from another binary, which is free to say anything. The
	// useful signal is that it DIFFERS, not that the number is true.
	return " (reports " + v + ")"
}

// sanitizeProbeOutput bounds and cleans output from an untrusted binary.
//
// Control characters are stripped because this string is printed to a terminal and
// embedded in a support bundle: an ANSI escape sequence from a hostile binary could
// rewrite the surrounding diagnosis, which is precisely the output a user is relying on
// to decide what is wrong.
func sanitizeProbeOutput(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	if len(s) > 64 {
		s = s[:64] + "…"
	}
	return strings.TrimSpace(s)
}

// lookPathAll returns every executable named `name` on PATH, in resolution order.
// Deduplicated by resolved path, so a directory listed twice does not read as a conflict.
func lookPathAll(name string) []string {
	var out []string
	seen := map[string]bool{}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		// An empty entry and "." both mean "the current directory" — a PATH containing
		// either would have doctor execute whatever ./daintree-assistant happens to be in
		// the directory the user is standing in. That is not a copy Daintree would resolve
		// in practice, and probing it is a needless risk.
		if dir == "" || dir == "." {
			continue
		}
		full := filepath.Join(dir, name)
		info, err := os.Stat(full)
		if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			continue
		}
		resolved := full
		if r, rerr := filepath.EvalSymlinks(full); rerr == nil {
			resolved = r
		}
		if seen[resolved] {
			continue
		}
		seen[resolved] = true
		out = append(out, full)
	}
	return out
}

// CheckStateDir reports whether the state directory is usable and private.
//
// Two distinct failures with the same symptom ("nothing persists"): the directory is not
// writable, or it is writable by other users. The second matters because the directory
// holds the conversation, the audit trail, automation grants, and — at the per-user root
// — the spendable API key.
func CheckStateDir(stateDir string) DoctorCheck {
	c := DoctorCheck{ID: "state.dir", Label: "state dir", Data: map[string]any{"path": stateDir}}

	info, err := os.Stat(stateDir)
	if err != nil {
		c.Status = StatusFail
		c.Detail = "cannot stat " + stateDir + ": " + err.Error()
		c.Hint = "Check the path exists and is readable, or set DAINTREE_ASSISTANT_STATE_DIR to somewhere writable."
		return c
	}
	if !info.IsDir() {
		c.Status = StatusFail
		c.Detail = stateDir + " is not a directory"
		c.Hint = "Move the file aside, or point DAINTREE_ASSISTANT_STATE_DIR elsewhere."
		return c
	}

	// Prove writability by writing. Mode bits lie under ACLs, read-only mounts, and
	// container filesystems, and a "looks writable but is not" state dir fails later, in
	// the middle of a turn, as a confusing SQLite error.
	//
	// CreateTemp, not a fixed filename: a fixed path would TRUNCATE whatever is already
	// there, and in a world-writable state dir (exactly the case the privacy check below
	// exists to catch) another user could pre-place a symlink and redirect the write to
	// any file this process can reach. CreateTemp fails on an existing entry instead. The
	// cleanup is deferred at the moment of creation so a later failure cannot leak it.
	probe, werr := os.CreateTemp(stateDir, ".doctor-write-probe-*")
	if werr != nil {
		c.Status = StatusFail
		c.Detail = "not writable: " + werr.Error()
		c.Hint = "Fix the permissions on " + stateDir + ", or set DAINTREE_ASSISTANT_STATE_DIR to a writable path."
		return c
	}
	probeName := probe.Name()
	defer func() { _ = os.Remove(probeName) }()
	if _, werr := probe.WriteString("ok"); werr != nil {
		_ = probe.Close()
		c.Status = StatusFail
		c.Detail = "not writable: " + werr.Error()
		c.Hint = "Check for a full disk or a read-only mount at " + stateDir + "."
		return c
	}
	if werr := probe.Close(); werr != nil {
		c.Status = StatusFail
		c.Detail = "not writable: " + werr.Error()
		c.Hint = "Check for a full disk or a read-only mount at " + stateDir + "."
		return c
	}

	perm := info.Mode().Perm()
	c.Data["mode"] = fmt.Sprintf("%04o", perm)
	if perm&0o077 != 0 {
		c.Status = StatusWarn
		c.Detail = fmt.Sprintf("%s is mode %04o — readable by other users", stateDir, perm)
		c.Hint = fmt.Sprintf("It holds your conversation, audit trail and grants. Run: chmod 700 %s", stateDir)
		return c
	}
	c.Status = StatusOK
	c.Detail = fmt.Sprintf("%s (%04o)", stateDir, perm)
	return c
}

// CheckCredentialsFile reports the stored sign-in's file permissions.
//
// Separate from "are you signed in": the key can be perfectly valid and world-readable at
// the same time, and only one of those is visible from a turn. Reported as a WARNING with
// the exact chmod rather than repaired silently — a file this process did not create,
// with permissions it did not choose, is not its to quietly change.
func CheckCredentialsFile(path string) DoctorCheck {
	c := DoctorCheck{ID: "credentials.perms", Label: "sign-in file", Data: map[string]any{"path": path}}

	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		c.Status = StatusSkip
		c.Detail = "no stored sign-in"
		return c
	}
	if err != nil {
		c.Status = StatusUnknown
		c.Detail = err.Error()
		return c
	}
	perm := info.Mode().Perm()
	c.Data["mode"] = fmt.Sprintf("%04o", perm)
	if perm&0o077 != 0 {
		// FAIL, not warn. This is not a hygiene note: any other account on the machine can
		// read a credential that spends the user's money, and the file is 0600 by
		// construction when this CLI writes it — so a wider mode means something else set
		// it, and the user needs to know now rather than at the bottom of a warning list.
		c.Status = StatusFail
		c.Detail = fmt.Sprintf("mode %04o — other users on this machine can read your API key", perm)
		c.Hint = "Run: chmod 600 " + path
		return c
	}
	c.Status = StatusOK
	c.Detail = fmt.Sprintf("%04o (owner only)", perm)
	return c
}

// CheckAutoApprove reports the confirmation bypass.
//
// It belongs in a diagnostic because it is invisible from anywhere else and changes what
// the assistant is allowed to do without asking. A support report that does not say
// AUTO_APPROVE was on is missing the most important fact about the session.
func CheckAutoApprove(on bool, tier string) DoctorCheck {
	c := DoctorCheck{ID: "safety.autoApprove", Label: "auto-approve", Data: map[string]any{"enabled": on, "tier": tier}}
	if !on {
		c.Status = StatusOK
		c.Detail = "off (mutating actions ask first)"
		return c
	}
	c.Status = StatusWarn
	c.Detail = "ON — mutating actions run WITHOUT asking, up to the '" + tier + "' tier"
	c.Hint = "Unset DAINTREE_ASSISTANT_AUTO_APPROVE unless this is an automated harness."
	return c
}

// renderDoctorHuman writes the report as the human banner.
func renderDoctorHuman(w io.Writer, r *DoctorReport) {
	fmt.Fprintf(w, "Daintree Assistant %s — doctor  (%s)\n\n", r.Version, r.Platform)
	width := 0
	for _, c := range r.Checks {
		if len(c.Label) > width {
			width = len(c.Label)
		}
	}
	for _, c := range r.Checks {
		fmt.Fprintf(w, "  %s  %-*s  %s\n", statusGlyph(c.Status), width, c.Label, c.Detail)
		// The hint is printed only when it is actionable NOW. Repeating advice under a
		// green line trains people to stop reading the lines that matter.
		if c.Hint != "" && (c.Status == StatusFail || c.Status == StatusWarn) {
			fmt.Fprintf(w, "  %s  %-*s  → %s\n", "    ", width, "", c.Hint)
		}
	}
	fmt.Fprintln(w)
	if r.Summary.Healthy {
		fmt.Fprintf(w, "  %d ok, %d warning(s). No blocking problems.\n", r.Summary.OK, r.Summary.Warn)
		return
	}
	fmt.Fprintf(w, "  %d FAILING, %d warning(s), %d ok.\n", r.Summary.Fail, r.Summary.Warn, r.Summary.OK)
	var ids []string
	for _, c := range r.Checks {
		if c.Status == StatusFail {
			ids = append(ids, c.ID)
		}
	}
	sort.Strings(ids)
	fmt.Fprintf(w, "  Failing checks: %s\n", strings.Join(ids, ", "))
}
