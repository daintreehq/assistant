package commands

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/daintreehq/daintree-assistant/internal/app"
	"github.com/daintreehq/daintree-assistant/internal/backend"
	"github.com/daintreehq/daintree-assistant/internal/mcp"
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

// RunDoctor runs the rich /doctor checklist. Order matters.
// It may opportunistically reconnect when the URL/token are present but the
// session is cold.
func RunDoctor(ctx context.Context, a *app.App) []DoctorCheck {
	cfg := a.Config

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

	// backend — the CLI's model gateway (owns the model credentials + prompts + skills).
	push("backend url", true, a.Backend.BaseURL(), "")
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
	caps, cerr := a.Backend.Capabilities(cctx)
	ccancel()
	if cerr == nil {
		protoOK := caps.Protocol.Min <= backend.ProtocolVersion && caps.Protocol.Max >= backend.ProtocolVersion
		push("backend protocol", protoOK,
			fmt.Sprintf("server v%d–%d, CLI v%d · %d tasks", caps.Protocol.Min, caps.Protocol.Max, backend.ProtocolVersion, len(caps.Tasks)),
			"CLI/backend protocol mismatch — update the CLI or backend")
	}
	// forbidden tools must never be exposed to the backend (skill find/load are reserved).
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
		listCtx, cancelList := context.WithTimeout(ctx, doctorProbeTimeout)
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
			callCtx, cancelCall := context.WithTimeout(ctx, doctorProbeTimeout)
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
// and any reserved names that must never be exposed (skill find/load, or the
// daintree_internal prefix). The registry holds internal dotted names.
func exposedAndForbiddenTools(a *app.App) (count int, forbidden []string) {
	list := a.Registry.List()
	count = len(list)
	for _, t := range list {
		switch t.Name {
		case "skill.find", "skill.load":
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
