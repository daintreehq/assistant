// Package deps is a build-time anchor that blank-imports the third-party stack
// the Phase-C subsystems (UI, storage, MCP) will use, so `go mod tidy` keeps
// these modules pinned in go.mod/go.sum before any subsystem code imports them.
//
// Delete an import here once the corresponding subsystem imports the package for
// real. This file has no runtime effect.
package deps

import (
	// UI stack — Bubble Tea cockpit.
	_ "charm.land/bubbles/v2/textarea"
	_ "charm.land/bubbletea/v2"
	_ "charm.land/glamour/v2"
	_ "charm.land/lipgloss/v2"
	_ "github.com/charmbracelet/x/ansi"

	// Storage — pure-Go SQLite driver.
	_ "modernc.org/sqlite"

	// MCP — official Go SDK for the Daintree MCP client.
	_ "github.com/modelcontextprotocol/go-sdk/mcp"
)
