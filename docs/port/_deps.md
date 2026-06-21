# Go dependency pins (Phase A)

Module: `github.com/daintreehq/daintree-assistant`

## Pinned direct dependencies

| Module | Version | Notes |
|---|---|---|
| `charm.land/bubbletea/v2` | v2.0.7 | **Import-path substitution** — see below |
| `charm.land/bubbles/v2` | v2.1.0 | substitution |
| `charm.land/lipgloss/v2` | v2.0.4 | substitution |
| `charm.land/glamour/v2` | v2.0.1 | substitution; **forces `go 1.25.8`** — see below |
| `github.com/charmbracelet/x/ansi` | v0.11.7 | NOT migrated — still on the `github.com/charmbracelet/x` path |
| `modernc.org/sqlite` | v1.53.0 | pure-Go SQLite (no cgo) |
| `github.com/modelcontextprotocol/go-sdk` | v1.6.1 | official MCP Go SDK; importable package is `.../go-sdk/mcp` |
| `github.com/google/uuid` | v1.6.0 | ID generation |
| `github.com/joho/godotenv` | v1.5.1 | `.env` loading (no-override `Load` semantics) |

`go.sum` is real (`go mod verify` → "all modules verified").

## Substitutions / TODOs an integrator must know

- **TODO(deps): Charm v2 vanity path.** The Charm v2 modules (`bubbletea`, `bubbles`,
  `lipgloss`, `glamour`) have migrated their declared module path from
  `github.com/charmbracelet/<x>/v2` to **`charm.land/<x>/v2`**. `go get github.com/charmbracelet/bubbletea/v2`
  FAILS with "module declares its path as charm.land/...". Import from `charm.land/...`.
  Note the split: `x/ansi` did **not** migrate and is still
  `github.com/charmbracelet/x/ansi`.
- **TODO(deps): go directive is `go 1.25.8`, not `go 1.25`.** `charm.land/glamour/v2`
  (both v2.0.0 and v2.0.1, the only published charm.land glamour v2 tags) declares
  `go 1.25.8` in its own `go.mod`, so our module's `go` directive must be ≥ 1.25.8 or
  `go build` fails with "requires go >= 1.25.8". The task asked for `go 1.25`; this was
  raised to `1.25.8` to satisfy glamour. Any toolchain ≥ 1.25.8 builds it natively (no
  `toolchain` directive needed); an older 1.25.x will auto-download 1.25.8 unless
  `GOTOOLCHAIN=local`. If a `go 1.25` floor is ever required, drop glamour or vendor a
  fork.
- **Anchor file**: `internal/deps/deps.go` blank-imports the UI/storage/MCP leaf packages
  purely so `go mod tidy` keeps them pinned before any subsystem imports them for real.
  Phase C should delete each blank import once the corresponding subsystem imports the
  package directly. The file has no runtime effect.

## MCP SDK import path

The module is `github.com/modelcontextprotocol/go-sdk`; the client/types live under the
`.../go-sdk/mcp` package (that is the importable path the anchor uses).
