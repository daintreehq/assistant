package mcp

import (
	"context"
	"time"
)

// DoctorProbeResult is the outcome of the actions.getContext doctor probe.
type DoctorProbeResult struct {
	OK         bool          // pass iff the round-trip succeeded and !res.IsError
	Detail     string        // human-readable explanation (failure reason or "ok")
	RoundTrip  time.Duration // measured actions.getContext round-trip when reached
	Reachable  bool          // false when the probe could not run (not connected)
	ToolListed bool          // whether actions.getContext was advertised by listTools
}

// doctorProbeTool is the canonical doctor probe + project-name source: read-only,
// workbench tier, no confirmation — verifies end-to-end access without mutating.
const doctorProbeTool = "actions.getContext"

// Doctor runs the actions.getContext probe. It ONLY runs when connected.
//  1. listTools() (5s). If actions.getContext is not advertised → fail with a
//     "workbench tier may be unavailable" message.
//  2. else callTool("actions.getContext", {}) (5s); pass iff !res.IsError; report
//     round-trip ms. A returned error = live transport failure/timeout (not a tier
//     issue).
//
// Both calls get the 5s doctorProbeTimeout via a derived context.
func (c *Client) Doctor(ctx context.Context) DoctorProbeResult {
	// Diagnostics are Refresh-class traffic: they must queue behind in-turn tool
	// calls, but a doctor run should not be parked behind maintenance polls.
	ctx = WithPriority(ctx, PriorityRefresh)
	if !c.IsConnected() {
		return DoctorProbeResult{Reachable: false, Detail: "Daintree MCP is not connected"}
	}

	// Step 1: list tools with a 5s budget; confirm the probe tool is advertised.
	listCtx, cancelList := context.WithTimeout(ctx, doctorProbeTimeout)
	tools, err := c.ListTools(listCtx, false)
	cancelList()
	if err != nil {
		// A throw here is a live transport failure/timeout, not a tier issue.
		return DoctorProbeResult{Reachable: true, OK: false, Detail: errMsg(err)}
	}
	advertised := false
	for _, t := range tools {
		if t.Name == doctorProbeTool {
			advertised = true
			break
		}
	}
	if !advertised {
		return DoctorProbeResult{
			Reachable:  true,
			OK:         false,
			ToolListed: false,
			Detail:     doctorProbeTool + " not advertised — workbench tier may be unavailable",
		}
	}

	// Step 2: call actions.getContext with a 5s budget. Pass iff !res.IsError.
	// Retries=0 deliberately: a doctor probe must not mask flakiness.
	start := time.Now()
	res, err := c.CallTool(ctx, doctorProbeTool, map[string]any{}, CallOptions{Timeout: doctorProbeTimeout})
	rt := time.Since(start)
	if err != nil {
		return DoctorProbeResult{Reachable: true, OK: false, ToolListed: true, RoundTrip: rt, Detail: errMsg(err)}
	}
	if res.IsError {
		return DoctorProbeResult{Reachable: true, OK: false, ToolListed: true, RoundTrip: rt, Detail: "actions.getContext returned an error result"}
	}
	return DoctorProbeResult{Reachable: true, OK: true, ToolListed: true, RoundTrip: rt, Detail: "ok"}
}

// FetchProjectName calls actions.getContext and extracts the project name via the
// pure ReadProjectName helper (structuredContent first, then text-JSON fallback).
// Any failure → "" (caller keeps its provisional name). Read-only, 5s budget.
func (c *Client) FetchProjectName(ctx context.Context) string {
	if !c.IsConnected() {
		return ""
	}
	// Discovery read: Refresh class (see Doctor).
	ctx = WithPriority(ctx, PriorityRefresh)
	callCtx, cancel := context.WithTimeout(ctx, doctorProbeTimeout)
	defer cancel()
	res, err := c.CallTool(callCtx, doctorProbeTool, map[string]any{}, CallOptions{Timeout: doctorProbeTimeout})
	if err != nil {
		return ""
	}
	return ReadProjectName(res)
}
