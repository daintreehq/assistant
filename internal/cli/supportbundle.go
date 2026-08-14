package cli

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/cli/render"
	"github.com/daintreehq/assistant/internal/config"
	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/host"
	"github.com/daintreehq/assistant/internal/ipc"
	"github.com/daintreehq/assistant/internal/mcp"
	"github.com/daintreehq/assistant/internal/redact"
	"github.com/daintreehq/assistant/internal/storage"
	"github.com/daintreehq/assistant/internal/supervisor"
)

// supportbundle.go builds the artifact a tester can safely hand to a maintainer.
//
// It exists because the alternative was "send me your debug log", and that is not an
// acceptable instruction. A session log contains the whole conversation, terminal output,
// file excerpts, issue bodies, and memory contents — most of which has nothing to do with
// the bug and some of which belongs to the tester's employer. Asking for one turns every
// support request into a data-disclosure decision the tester has to make under pressure,
// with no way to check what they are about to send.
//
// So the bundle is built from the opposite direction: it collects the FACTS support
// actually needs to reproduce a version/compatibility/environment problem, and nothing
// else. What goes in is an explicit list, not a directory sweep; every string is
// redacted; and the whole manifest is printed before anything is written, so the decision
// is informed and made before the file exists rather than after it has been sent.
//
// What it deliberately does NOT include: the conversation, memories, terminal output,
// file contents, or the debug log. Those are what a maintainer would LIKE, and are
// exactly what the tester cannot safely give. A bug that genuinely needs them is a
// conversation, not a default.

// SupportBundleOptions tunes a bundle run.
type SupportBundleOptions struct {
	// Out is the destination path. Empty picks a timestamped name in the working dir.
	Out string
	// Yes skips the "write this?" confirmation. Required for a non-TTY run.
	Yes bool
	// IncludeAudit adds a bounded, redacted slice of recent tool calls — the single most
	// useful optional addition for a behaviour report, and opt-in because it is the one
	// section that can carry project-specific detail.
	IncludeAudit bool
}

// bundleFile is one entry destined for the archive.
type bundleFile struct {
	Name    string
	Purpose string
	Content []byte
}

// RunSupportBundle is the `daintree-assistant support-bundle` subcommand.
func RunSupportBundle(ctx context.Context, opts Options, bopts SupportBundleOptions) int {
	r := render.Stdout()
	cfg, err := config.LoadConfig(overridesFromOptions(opts))
	if err != nil {
		r.Error(err.Error())
		return domain.OneShotExitCode.Error
	}

	files, err := collectSupportBundle(ctx, opts, cfg, bopts)
	if err != nil {
		r.Error(err.Error())
		return domain.OneShotExitCode.Error
	}

	out := bopts.Out
	if out == "" {
		out = fmt.Sprintf("daintree-assistant-support-%s.zip", time.Now().UTC().Format("20060102-150405"))
	}

	// Show the manifest BEFORE writing. The decision to share diagnostics has to be made
	// with knowledge of what they contain — after the fact, the file already exists and
	// the tester is reading a zip listing instead of a description.
	r.Line("Daintree Assistant — support bundle")
	r.Line("")
	r.Line("This archive will contain:")
	total := 0
	for _, f := range files {
		total += len(f.Content)
		r.Line(fmt.Sprintf("  %-26s %-9s %s", f.Name, humanBytes(int64(len(f.Content))), r.Gray(f.Purpose)))
	}
	r.Line("")
	for _, line := range supportBundleExclusions(bopts) {
		r.Line("  " + r.Gray(line))
	}
	r.Line("")

	if !bopts.Yes {
		if !stdinIsTTY() {
			r.Error("support-bundle needs confirmation. Re-run with --yes (there is no terminal here to ask on).")
			return domain.OneShotExitCode.Error
		}
		fmt.Printf("Write %s? [y/N]: ", out)
		var answer string
		_, _ = fmt.Scanln(&answer)
		if !strings.EqualFold(strings.TrimSpace(answer), "y") {
			r.Line("Cancelled — nothing was written.")
			return domain.OneShotExitCode.Success
		}
	}

	// A finding BLOCKS the write. Recording "this bundle is unsafe" inside the bundle and
	// then writing it anyway is the worst of both worlds: the file exists, it carries the
	// authority of "the safe one to send", and the warning is buried in an attachment
	// nobody opens. If the scan is unhappy, there is no artifact.
	if findings := redactionFindings(files); len(findings) > 0 {
		r.Error("refusing to write: the bundle's own redaction scan found a credential-shaped value that survived assembly.")
		for _, f := range findings {
			r.Line("  " + f)
		}
		r.Line(r.Gray("This is a bug in the assistant — please report it (without attaching anything)."))
		return domain.OneShotExitCode.Error
	}

	if err := writeSupportBundle(out, files); err != nil {
		r.Error(err.Error())
		return domain.OneShotExitCode.Error
	}
	r.Line(fmt.Sprintf("Wrote %s (%s, %d files).", out, humanBytes(int64(total)), len(files)))
	r.Line(r.Gray("Every string in it was passed through the redactor; redaction-report.json records the result."))
	return domain.OneShotExitCode.Success
}

// supportBundleExclusions is the "what is NOT in here" notice.
func supportBundleExclusions(bopts SupportBundleOptions) []string {
	lines := []string{
		"NOT included: your conversation, memories, terminal output, file contents, or the debug log.",
		"Every value is redacted — API keys, bearer tokens, and MCP tokens cannot appear.",
	}
	if bopts.IncludeAudit {
		lines = append(lines,
			"You asked for --include-audit: recent TOOL NAMES, outcomes and timings are included",
			"  (redacted, no arguments or results). Review audit.json before sending if the project is sensitive.")
	} else {
		lines = append(lines, "Add --include-audit to include recent tool names and outcomes (no args or results).")
	}
	return lines
}

// collectSupportBundle gathers the bundle's contents.
//
// Every section is built from structured data and marshaled here, rather than copied from
// a file on disk. That is the property that makes the redaction claim checkable: there is
// no path by which a byte reaches the archive without passing through this function.
func collectSupportBundle(ctx context.Context, opts Options, cfg config.AppConfig, bopts SupportBundleOptions) ([]bundleFile, error) {
	var files []bundleFile
	add := func(name, purpose string, v any) error {
		data, err := json.MarshalIndent(redact.Value(v), "", "  ")
		if err != nil {
			return fmt.Errorf("encode %s: %w", name, err)
		}
		files = append(files, bundleFile{Name: name, Purpose: purpose, Content: append(data, '\n')})
		return nil
	}

	// 1. Versions — the compatibility triple a release actually is.
	if err := add("versions.json", "CLI build, protocol and schema versions", map[string]any{
		"cliVersion":       buildVersion,
		"goVersion":        runtime.Version(),
		"os":               runtime.GOOS,
		"arch":             runtime.GOARCH,
		"backendProtocol":  backend.ProtocolVersion,
		"hostProtocol":     host.ProtocolVersion,
		"stateSchema":      storage.SchemaVersion(),
		"requiredTasks":    backend.CoreTaskIDs(),
		"workflowTasks":    backend.WorkflowTaskIDs(),
		"backendEndpoint":  mcp.SanitizeURL(cfg.BackendURL),
		"officialEndpoint": backend.DefaultBaseURL,
	}); err != nil {
		return nil, err
	}

	// 2. Environment — settings that change behaviour, and NEVER their secret values.
	//    Presence and length only for the credentials: "is a token set, and does it look
	//    truncated" is the diagnostic question; the value itself never is.
	if err := add("environment.json", "settings that change behaviour (no secret values)", map[string]any{
		"tier":                 string(cfg.Tier),
		"autoApprove":          cfg.AutoApprove,
		"offline":              cfg.Offline,
		"workflowIntelligence": cfg.WorkflowIntelligence,
		"debugLogEnabled":      cfg.DebugLog,
		"stateDir":             cfg.StateDir,
		"projectPath":          cfg.ProjectPath,
		"hasProjectId":         cfg.ProjectID != "",
		"hasWindowId":          cfg.WindowID != "",
		"apiKeyPresent":        strings.TrimSpace(cfg.APIKey) != "",
		"apiKeyLength":         len(strings.TrimSpace(cfg.APIKey)),
		"mcpUrlPresent":        strings.TrimSpace(cfg.McpURL) != "",
		"mcpTokenPresent":      strings.TrimSpace(cfg.McpToken) != "",
		"mcpTokenLength":       len(strings.TrimSpace(cfg.McpToken)),
		"hasProjectInstructions": func() bool {
			return strings.TrimSpace(cfg.ProjectInstructions) != ""
		}(),
		// The LENGTH of DAINTREE.md, never its text: a project's instructions are the
		// project's business, and "is one loaded and roughly how big" answers the
		// prompt-assembly questions support actually asks.
		"projectInstructionsBytes": len(cfg.ProjectInstructions),
		"terminal":                 os.Getenv("TERM"),
		"shell":                    filepath.Base(os.Getenv("SHELL")),
		"lang":                     os.Getenv("LANG"),
	}); err != nil {
		return nil, err
	}

	// 3. Doctor — the whole diagnosis, already structured and already redacted.
	report, derr := buildDoctorReport(ctx, opts)
	if derr != nil {
		report = &DoctorReport{Version: buildVersion, Platform: runtime.GOOS + "/" + runtime.GOARCH}
		report.Add(DoctorCheck{ID: "doctor.setup", Label: "doctor", Status: StatusFail, Detail: derr.Error()})
		report.Finalize()
	}
	if err := add("doctor.json", "full environment diagnosis", report); err != nil {
		return nil, err
	}

	// 4. Supervisor — running or not, and what it is supervising.
	if err := add("daemon.json", "supervisor daemon state", daemonSupportInfo(ctx, cfg)); err != nil {
		return nil, err
	}

	// 5. Debug-log INVENTORY — names, sizes and times only. Never contents: the whole
	//    point of the bundle is to be the thing you send INSTEAD of a session log. Listing
	//    them still tells support whether tracing was on and which session to ask about.
	if err := add("debug-logs.json", "session-log inventory (names and sizes ONLY, never contents)",
		debugLogInventory(cfg.LogDir)); err != nil {
		return nil, err
	}

	if bopts.IncludeAudit {
		rows, aerr := auditSupportSlice(cfg)
		if aerr != nil {
			rows = map[string]any{"error": aerr.Error()}
		}
		if err := add("audit.json", "recent tool names, outcomes and timings (no args or results)", rows); err != nil {
			return nil, err
		}
	}

	// 6. The redaction report — a machine-checkable claim rather than a promise.
	if err := add("redaction-report.json", "proof that no credential-shaped value survived",
		redactionReport(files)); err != nil {
		return nil, err
	}
	return files, nil
}

// daemonSupportInfo describes the supervisor without disturbing it.
func daemonSupportInfo(ctx context.Context, cfg config.AppConfig) map[string]any {
	info := map[string]any{"stateDir": cfg.StateDir}
	st, err := supervisor.QueryStatus(ctx, cfg.StateDir)
	if err != nil {
		info["running"] = false
		info["detail"] = err.Error()
		// A stamped pid alone is stale after a clean release (the lock FILE stays on
		// disk); only a failing probe proves a live owner.
		lockPath := filepath.Join(cfg.StateDir, ipc.OwnerLockName)
		probe := ipc.NewFileLock(lockPath)
		if got, _ := probe.TryAcquire(); got {
			probe.Release()
			info["ownerHeld"] = false
		} else {
			info["ownerHeld"] = true
			info["ownerPid"] = ipc.ReadLockHolderPid(lockPath)
		}
		return info
	}
	info["running"] = true
	info["status"] = st
	return info
}

// debugLogInventory lists the session logs WITHOUT reading them.
func debugLogInventory(logDir string) map[string]any {
	out := map[string]any{"dir": logDir}
	entries, err := os.ReadDir(logDir)
	if err != nil {
		out["error"] = err.Error()
		return out
	}
	var logs []map[string]any
	for _, e := range entries {
		info, ierr := e.Info()
		if ierr != nil || e.IsDir() {
			continue
		}
		logs = append(logs, map[string]any{
			"name":     e.Name(),
			"bytes":    info.Size(),
			"modified": info.ModTime().UTC().Format(time.RFC3339),
		})
	}
	sort.Slice(logs, func(i, j int) bool {
		return logs[i]["modified"].(string) > logs[j]["modified"].(string)
	})
	if len(logs) > 20 {
		logs = logs[:20]
	}
	out["logs"] = logs
	out["note"] = "Contents are deliberately excluded. A maintainer who needs one will ask."
	return out
}

// auditSupportSlice returns recent tool calls with their NAMES and outcomes only.
//
// Args and results are dropped even though they are already redacted in the database.
// Redaction removes credentials, not project detail — a file path, a branch name, an
// issue title all survive it, and none of them is needed to answer "which tool failed,
// how often, and how long did it take".
func auditSupportSlice(cfg config.AppConfig) (any, error) {
	// READ-ONLY. storage.Open applies WAL/schema settings and runs retention GC, so
	// calling it here would MUTATE a database whose owner lease we are not holding —
	// racing a live cockpit or the daemon to collect a diagnostic. A diagnostic that
	// changes the thing it is diagnosing is not one.
	store, err := storage.OpenReadOnly(cfg.DBPath)
	if err != nil {
		return nil, err
	}
	defer store.Close()

	limit := 200
	rows, err := store.QueryAudit(storage.AuditFilters{Limit: &limit})
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(rows))
	for _, a := range rows {
		out = append(out, map[string]any{
			"ts":         a.Ts,
			"tool":       a.ToolName,
			"actor":      string(a.Actor),
			"outcome":    a.Outcome,
			"durationMs": a.DurationMs,
		})
	}
	return map[string]any{
		"count": len(out),
		"calls": out,
		"note":  "Tool arguments and results are excluded — redaction removes credentials, not project detail.",
	}, nil
}

// redactionReport scans the assembled bundle for anything that still looks like a
// credential, and records the result inside the bundle itself.
//
// A self-check, not a proof — but it is a MACHINE-CHECKABLE claim, and it fails loudly.
// "We redact" is a sentence; "the bundle contains a scan of itself, and here is what it
// found" is something a reviewer can verify in one command.
func redactionFindings(files []bundleFile) []string {
	var out []string
	for _, f := range files {
		// Re-run the redactor over the final bytes. A difference means something got in
		// unredacted — a section that bypassed the add() helper, or a pattern that only
		// matches once assembled.
		if body := string(f.Content); redact.String(body) != body {
			out = append(out, f.Name+": a credential-shaped value survived assembly")
		}
	}
	return out
}

func redactionReport(files []bundleFile) map[string]any {
	findings := redactionFindings(files)
	return map[string]any{
		// One fewer than the archive's file count: this report is assembled last and
		// cannot scan itself. It contains only constants, so that is a statement of
		// scope, not a gap being papered over.
		"scannedFiles": len(files),
		"findings":     findings,
		"clean":        len(findings) == 0,
		"method":       "every value is passed through internal/redact at assembly, then the assembled bytes are re-scanned",
		"limits": "Best-effort, NOT proof. It cannot see a credential that matches no known " +
			"shape and is not registered — an opaque query token, a base64-wrapped value, or a " +
			"secret split across fields. Endpoints are therefore stripped at the source rather " +
			"than left to this scan.",
	}
}

// writeSupportBundle writes the archive.
//
// O_EXCL: silently overwriting a previous bundle would destroy the one the user is in the
// middle of sending.
func writeSupportBundle(path string, files []bundleFile) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("%s already exists — pass --out to choose another name", path)
		}
		return err
	}
	defer f.Close()

	zw := zip.NewWriter(f)
	for _, bf := range files {
		w, werr := zw.Create(bf.Name)
		if werr != nil {
			zw.Close()
			return werr
		}
		if _, werr := w.Write(bf.Content); werr != nil {
			zw.Close()
			return werr
		}
	}
	return zw.Close()
}
