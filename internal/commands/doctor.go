package commands

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/daintreehq/assistant/internal/app"
	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/mcp"
)

// DoctorCheck is one row of the rich /doctor checklist.
type DoctorCheck struct {
	Label  string
	OK     bool
	Detail string
	Fix    string
}

// doctorProbeTool is the workbench-tier, read-only, no-confirm tool the live probe
// calls. EXTERNAL CONTRACT — keep verbatim.
const doctorProbeTool = "actions.getContext"

// doctorProbeTimeout bounds both listTools and the probe call.
const doctorProbeTimeout = 5 * time.Second

// boundedProbeCtx bounds a LIVE-client MCP probe with a CANCEL-based timer rather
// than context.WithTimeout — the mcp-bestEffort-reads rule (see
// internal/app/toolterminal.go). /doctor runs against the attached session's live
// a.MCP, and the client's own per-attempt budget (20s) is longer than this 5s
// one, so a slow-but-alive server would otherwise surface the CALLER's
// DeadlineExceeded to the client's degrade path and close the session — making
// the diagnostic cause the very outage it then reports ("connection may be
// stale; run /reconnect"). Expiring as a plain context.Canceled keeps the probe
// a read-only observation of connection health.
func boundedProbeCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	pctx, cancel := context.WithCancel(ctx)
	timer := time.AfterFunc(doctorProbeTimeout, cancel)
	return pctx, func() {
		timer.Stop()
		cancel()
	}
}

// RunDoctor runs the rich /doctor checklist. Order matters.
// It may opportunistically reconnect when the URL/token are present but the
// session is cold.
func RunDoctor(ctx context.Context, a *app.App) []DoctorCheck {
	// Snapshot under the App's config lock: /doctor runs off the UI loop as a slash
	// command, and SignIn (/login) mutates the sign-in fields of this same struct from
	// another goroutine. A direct copy here races that write.
	cfg := a.SnapshotConfig()

	// 1. Opportunistic reconnect when configured-but-cold.
	if !a.MCP.IsConnected() && cfg.McpURL != "" && cfg.McpToken != "" {
		func() {
			defer func() { _ = recover() }()
			a.ReconnectMcp(ctx)
		}()
	}

	st := a.MCP.Status()
	var checks []DoctorCheck
	push := func(label string, ok bool, detail, fix string) {
		if ok {
			fix = ""
		}
		checks = append(checks, DoctorCheck{Label: label, OK: ok, Detail: detail, Fix: fix})
	}

	presentOrMissing := func(v string) string {
		if v != "" {
			return "present"
		}
		return "MISSING"
	}

	// backend — the CLI's model gateway (owns the model credentials + prompts + runbooks).
	//
	// Every backend probe below runs one-shot. A turn PATIENTLY retries a transient
	// failure (up to ~a minute, to ride out a restart), but a diagnostic must answer
	// "up right now?" immediately — otherwise each row silently spends its whole
	// probe budget replaying a refused socket to reach the same verdict.
	ctx = backend.WithoutRetry(ctx)
	push("backend url", true, a.Backend.BaseURL(), "")
	// There is deliberately no "signed in" row. The backend holds its own upstream
	// credential and serves a request that carries no Authorization header at all, so
	// having no key is the normal working state — reporting it as a failed check would
	// put a permanent red line on every healthy install.
	//
	// A key is worth a row only when one is actually being sent, because then it
	// CHANGES which account funds the turn, and an inherited or stale DAINTREE_API_KEY
	// is otherwise invisible. The value itself is never printed: naming the source is
	// what makes it actionable.
	if cfg.APIKey != "" {
		push("bearer token", true, "sent from DAINTREE_API_KEY — this key funds the turn, not the backend's", "")
	}
	bctx, bcancel := context.WithTimeout(ctx, doctorProbeTimeout)
	herr := a.Backend.Health(bctx)
	bcancel()
	push("backend health", herr == nil, errDetail(herr, "ok"),
		"start the Daintree backend (../assistant-backend)")
	rctx, rcancel := context.WithTimeout(ctx, doctorProbeTimeout)
	rerr := a.Backend.Ready(rctx)
	rcancel()
	push("backend ready", rerr == nil, errDetail(rerr, "ready"),
		"backend is up but not ready (config/secrets/catalog/provider)")
	cctx, ccancel := context.WithTimeout(ctx, doctorProbeTimeout)
	// Through the App so a successful probe REFRESHES the cached descriptor: if the boot
	// handshake missed (a slow endpoint, a blip), running /doctor is the natural moment
	// for capability-gated behaviour to come back, rather than waiting for a relaunch.
	caps, cerr := a.BackendCapabilities(cctx)
	ccancel()
	if cerr == nil {
		protoOK := caps.Protocol.Min <= backend.ProtocolVersion && caps.Protocol.Max >= backend.ProtocolVersion
		push("backend protocol", protoOK,
			fmt.Sprintf("server v%d–%d, CLI v%d · %d tasks", caps.Protocol.Min, caps.Protocol.Max, backend.ProtocolVersion, len(caps.Tasks)),
			"CLI/backend protocol mismatch — update the CLI or backend")

		// Task-ID drift. The row above deliberately reports only a COUNT, and a count
		// is provably blind to the failure that actually happened: on 2026-07-07 the
		// backend renamed every task id (dropping a `.v1` suffix) and the count stayed
		// identical, so the CLI 404'd mid-turn instead of failing loudly here. Diff the
		// ids the CLI will actually send against the ids the server advertises.
		av := backend.CheckTasks(caps, cfg.WorkflowIntelligence)
		switch {
		case !av.Reported:
			push("backend tasks", false, "backend advertised NO tasks — every task call will fail",
				"the backend's task registry is empty; restart/repair the backend")
		case av.OK():
			push("backend tasks", true, fmt.Sprintf("%d/%d required present", av.Required, av.Required), "")
		default:
			push("backend tasks", false,
				fmt.Sprintf("%d of %d required task(s) MISSING: %s", len(av.Missing), av.Required, joinNames(av.Missing)),
				"CLI/backend task-id drift — these calls will fail at runtime; update the CLI or backend")
		}

		// Routing. GATING when the user asked for a non-default policy and the backend
		// does not accept one: the request still goes upstream, just under a weaker
		// filter than they chose, and nothing else in the product would ever say so.
		// That is the failure mode a privacy setting must not have.
		if r := cfg.Routing; !r.IsDefault() {
			if caps.Routing.ClientSelectable != nil {
				push("routing", true, "backend accepts a routing preference — see /routing", "")
			} else {
				push("routing", false,
					"you have configured a non-default routing policy, but this backend does not accept one — the server default applies instead",
					"Update the backend, or unset DAINTREE_ROUTING_* so the policy in force is the one you expect")
			}
		}

		// Cost reporting. Not a failure either way — an older backend simply cannot tell
		// you what a turn cost — but a tester whose /cost panel is empty deserves to
		// learn WHY here rather than assume the feature is broken.
		if caps.Respond.CostReporting != nil {
			push("cost reporting", true,
				"supported ("+caps.Respond.CostReporting.Currency+", on the "+
					caps.Respond.CostReporting.StreamEvent+" event) — see /cost", "")
		} else {
			push("cost reporting", true,
				"not advertised by this backend — /cost will have nothing to total", "")
		}
	}
	// forbidden tools must never be exposed to the backend (runbook find/load are reserved).
	exposed, forbidden := exposedAndForbiddenTools(a)
	push("backend tools", len(forbidden) == 0, fmt.Sprintf("%d exposed", exposed),
		"a reserved tool is exposed: "+joinNames(forbidden))

	if cfg.Offline {
		// Offline is an explicit operating mode, not four independent MCP failures.
		push("mcp mode", true, "offline (connection checks skipped)", "")
	} else {
		// mcp url / token.
		push("mcp url", cfg.McpURL != "", orUnset(cfg.McpURL), "set DAINTREE_MCP_URL to Daintree's MCP endpoint")
		push("mcp token", cfg.McpToken != "", presentOrMissing(cfg.McpToken), "set DAINTREE_MCP_TOKEN")

		// mcp connection.
		connDetail := "ok (" + st.Transport + ")"
		if !st.Connected {
			connDetail = st.Error
			if connDetail == "" {
				connDetail = "not connected"
			}
		}
		push("mcp connection", st.Connected, connDetail, "start Daintree, then run /reconnect")

		// A tool-count check is meaningful only after connection succeeds; otherwise it
		// merely repeats the connection failure with a misleading remediation.
		if st.Connected {
			toolCount := 0
			if st.ToolCount != nil {
				toolCount = *st.ToolCount
			}
			push("mcp tools", toolCount > 0, fmt.Sprintf("%d tools", toolCount),
				"connected but no tools listed; run /reconnect")
		}
	}

	// Live probe — only when connected.
	if st.Connected {
		probeAdvertised := false
		listCtx, cancelList := boundedProbeCtx(ctx)
		toolList, listErr := a.MCP.ListTools(listCtx, false)
		cancelList()
		if listErr == nil {
			for _, t := range toolList {
				if t.Name == doctorProbeTool {
					probeAdvertised = true
					break
				}
			}
		}
		if !probeAdvertised {
			push("mcp probe", false, doctorProbeTool+" not advertised — workbench tier may be unavailable",
				"verify the MCP token grants at least workbench tier")
		} else {
			callCtx, cancelCall := boundedProbeCtx(ctx)
			start := time.Now()
			res, callErr := a.MCP.CallTool(callCtx, doctorProbeTool, map[string]any{}, mcp.CallOptions{})
			cancelCall()
			ms := time.Since(start).Milliseconds()
			switch {
			case callErr != nil:
				push("mcp probe", false, "probe failed: "+callErr.Error(),
					"connection may be stale; run /reconnect")
			case res.IsError:
				push("mcp probe", false, res.Text, "check Daintree tier/permissions; run /reconnect")
			default:
				push("mcp probe", true, fmt.Sprintf("%s ok (%dms)", doctorProbeTool, ms),
					"check Daintree tier/permissions; run /reconnect")
			}
		}
	}

	// state writable.
	probePath := filepath.Join(cfg.StateDir, ".doctor-probe")
	writeErr := os.WriteFile(probePath, []byte("ok"), 0o600)
	if writeErr == nil {
		_ = os.Remove(probePath)
	}
	push("state writable", writeErr == nil, cfg.StateDir,
		"ensure the state dir is writable or set DAINTREE_ASSISTANT_STATE_DIR")

	// project path.
	info, statErr := os.Stat(cfg.ProjectPath)
	projectOK := statErr == nil && info.IsDir()
	push("project path", projectOK, cfg.ProjectPath, "pass --project <dir> or run from the project root")

	// mcp drift — only when connected and present.
	if st.Connected && len(st.DriftToolNames) > 0 {
		push("mcp drift", true, fmt.Sprintf("%d documented tool(s) not advertised at this tier/plugin config: %s",
			len(st.DriftToolNames), joinNames(st.DriftToolNames)), "")
	}

	// tier.
	push("tier", true, string(cfg.Tier), "")

	// tools loaded.
	n := len(a.Registry.List())
	push("tools loaded", n > 0, fmt.Sprintf("%d", n), "")

	// session cost — informational, never a failure. It belongs in a diagnostic because
	// "why is this expensive?" is a real support question, and because the cache ratio
	// on the same line is the first place a prompt-assembly regression shows up. A
	// support bundle that answered every connectivity question and could not say what
	// the session had spent would be missing the one number the tester paid for.
	if cs := a.CostLedger.Snapshot(); cs.Calls > 0 {
		var detail string
		switch {
		// Nothing reported at ALL — an older backend, most likely. Say that rather than
		// "≥ $0.0000 over 12 requests", which reads as a malfunction and hides the
		// actual explanation. Same branch /cost takes, for the same reason.
		case cs.Unreported == cs.Calls:
			detail = fmt.Sprintf("not reported by this backend (%d billed request(s))", cs.Calls)
		default:
			detail = formatUSD(cs.Observed, cs.LowerBound) +
				fmt.Sprintf(" over %d billed request(s)", cs.Calls)
			if ratio, ok := cs.CacheHitRatio(); ok {
				detail += fmt.Sprintf(", %.1f%% prompt-cache hit on the main call", ratio*100)
			}
			if cs.LowerBound {
				detail += " (lower bound — see /cost)"
			}
		}
		push("session cost", true, detail, "")
	}

	return checks
}

// FormatDoctor renders the checklist; remediation is shown only for failed checks.
func FormatDoctor(checks []DoctorCheck) string {
	var b []byte
	for i, c := range checks {
		if i > 0 {
			b = append(b, '\n')
		}
		mark := "✗"
		if c.OK {
			mark = "✓"
		}
		line := mark + " " + padRight(c.Label, 16) + ": " + c.Detail
		if !c.OK && c.Fix != "" {
			line += "  → " + c.Fix
		}
		b = append(b, line...)
	}
	return string(b)
}

func orUnset(s string) string {
	if s == "" {
		return "(unset)"
	}
	return s
}

// errDetail renders ok when err is nil, else the error message.
func errDetail(err error, ok string) string {
	if err == nil {
		return ok
	}
	return err.Error()
}

// exposedAndForbiddenTools returns the count of tools the CLI would offer the backend
// and any reserved names that must never be exposed (runbook find/load, or the
// daintree_internal prefix). The registry holds internal dotted names.
func exposedAndForbiddenTools(a *app.App) (count int, forbidden []string) {
	list := a.Registry.List()
	count = len(list)
	for _, t := range list {
		switch t.Name {
		case "runbook.find", "runbook.load":
			forbidden = append(forbidden, t.Name)
		}
		if strings.HasPrefix(t.Name, "daintree_internal.") {
			forbidden = append(forbidden, t.Name)
		}
	}
	return count, forbidden
}

func joinNames(names []string) string {
	out := ""
	for i, n := range names {
		if i > 0 {
			out += ", "
		}
		out += n
	}
	return out
}
