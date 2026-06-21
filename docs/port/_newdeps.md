# Port — new deps & wire-contract changes

Records additions/changes Phase-C waves make that the next wave must know about.
No `go get` / `go.mod` edits are made here; this is a ledger of decisions.

## host wave (`internal/host`)

- **PROTOCOL_VERSION bumped 1 → 2.** The TS host spoke over an Electron
  utility-process MessagePort (structured-clone objects); the Go port speaks
  **stdio NDJSON** (one JSON object per line; stdin = commands, first line =
  descriptor; stdout = events; stderr = diagnostics). The framing change is
  breaking for any consumer that parsed the old port messages, so the version
  moves in lockstep. Daintree's `ASSISTANT_HOST_PROTOCOL_VERSION` must be raised
  to `2` to match (`host.ProtocolVersion` in `internal/host/wire.go`). All
  event/command type names, field names, and vocabulary string values are
  preserved verbatim — only the version + transport framing changed.

- **No new module deps.** Uses stdlib (`encoding/json`, `bufio`, `bytes`,
  `context`, `time`, `sync`, `unicode/utf16`) + the existing `internal/agent`,
  `internal/config`, `internal/debuglog`, `internal/domain`,
  `internal/projectinstructions`. `github.com/google/uuid` is reached only via
  `domain.NewID`.

- **redactArgs UTF-16 parity choice:** the TS `.length > 80` cap + the
  `"<string: N chars>"` count are JS String.length (UTF-16 code units). The Go
  port uses `utf16.Encode` length (NOT rune count) so the cap + the reported N
  agree with Daintree's MCP-audit redaction for non-BMP input. Event/redaction
  JSON is marshaled with `SetEscapeHTML(false)` so `<`/`>`/`&` in
  markers/messages match TS `JSON.stringify` byte-for-byte.

- **App seam:** the host depends on the `host.App` interface + `host.AppFactory`
  (in `internal/host/app.go`), NOT the concrete `internal/app.App` — the cockpit/
  cli wave fills the factory (`hostAppFactory` in `cmd/daintree-assistant/main.go`
  currently returns `host.ErrAppFactoryUnset`, so `host --stdio` builds green and
  boot-fails cleanly until wired). The concrete App's surface differs slightly
  (`ConnectMcp` returns `mcp.Status`, `Shutdown()` takes no ctx, `StartScheduler`
  takes a ctx) — wire it with a thin adapter to `host.App`.
