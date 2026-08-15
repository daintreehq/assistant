package ui

import (
	"strings"
	"testing"
)

func TestHealthyMCPConnectionUsesCompactFixedHeightStatus(t *testing.T) {
	m := harnessModel()
	m.theme = darkTheme()
	m.composer.SetTheme(m.theme)
	w := m.contentW()
	beforeRows := lineCount(m.bottomBand(w))
	connectingComposer := m.composerView(w)
	connectingPlain := stripAnsi(connectingComposer)
	if !strings.Contains(connectingPlain, "MCP") || strings.Contains(connectingPlain, "Connecting") {
		t.Fatalf("connecting composer must show only compact MCP status: %q", connectingPlain)
	}
	if initial := stripAnsi(m.mcpDegradedView(w)); initial != "" {
		t.Fatalf("unresolved MCP state must not show a verbose warning, got %q", initial)
	}

	m, _ = step(t, m, MCPConnectedMsg{Transport: "http", ToolCount: 3})
	connectedComposer := m.composerView(w)
	connectedPlain := stripAnsi(connectedComposer)
	if !strings.Contains(connectedPlain, "MCP") || strings.Contains(connectedPlain, "Connected") {
		t.Fatalf("connected composer must show only compact MCP status: %q", connectedPlain)
	}
	if connectingComposer == connectedComposer {
		t.Fatal("connecting and connected MCP status must use distinct colors")
	}
	if connected := stripAnsi(m.mcpDegradedView(w)); connected != "" {
		t.Fatalf("healthy MCP state must not show a verbose warning, got %q", connected)
	}
	if len(m.transcript) != 0 {
		t.Fatal("healthy MCP completion must not append a scrollback note")
	}
	if got := lineCount(m.bottomBand(w)); got != beforeRows {
		t.Fatalf("healthy MCP completion changed bottom-band height from %d to %d", beforeRows, got)
	}
	if m.degraded || !m.mcpResolved {
		t.Fatal("connected state did not record healthy resolved MCP")
	}
}

func TestMCPConnectionRowDegradedAlsoKeepsGeometry(t *testing.T) {
	m := harnessModel()
	w := m.contentW()
	beforeRows := lineCount(m.bottomBand(w))

	m, _ = step(t, m, MCPDegradedMsg{Reason: "timeout"})
	line := stripAnsi(m.mcpDegradedView(w))
	// The recovery wording itself is pinned by mcpwarning_test.go; this test only cares
	// that the warning renders and that it expands the band.
	if !strings.Contains(line, "Daintree MCP unavailable") ||
		!strings.Contains(line, "/reconnect") {
		t.Fatalf("degraded MCP warning = %q", line)
	}
	if len(m.transcript) != 0 {
		t.Fatal("MCP degradation must update live chrome, not append a scrollback note")
	}
	if got := lineCount(m.bottomBand(w)); got <= beforeRows {
		t.Fatalf("degraded MCP warning did not expand the bottom band: before=%d after=%d", beforeRows, got)
	}
	if !m.degraded || !m.mcpResolved {
		t.Fatal("degraded state did not resolve the MCP boot gate")
	}
}
