# Changelog

Tester-facing changes. Pre-release: no version numbers yet, no backward-compatibility
guarantees, and the SQLite schema is a single clean baseline rather than a migration chain.

## Unreleased — internal beta preparation

### Changed — no sign-in, no API key

- **The CLI no longer asks for, verifies, or stores a credential.** The backend holds its
  own upstream key and serves a request that carries no `Authorization` header, so
  Daintree pays for the model calls and a fresh install works as soon as the binary is on
  your `PATH`. Removed: `login`, `logout`, the `/auth` and `/login` cockpit commands, the
  sign-in sheet, the startup sign-in gate on every entry point, the `reset credentials`
  scope, and `internal/credentials` (and with it `credentials.json` — the CLI now writes
  no credential file at all). **A `credentials.json` left over from an earlier build is
  ignored, not deleted; remove it yourself.**
- **This is a stage, not the destination.** Daintree account authentication is being built
  next. Three seams are kept live for it: `DAINTREE_API_KEY` and `--api-key-file` (both
  unset on a normal install) still ride as a bearer the backend prefers over its own key,
  `App.Backend` stays a `backend.Swappable` so re-authentication can swap a delegate rather
  than re-wire every consumer, and `/v1/daintree/auth/verify` keeps answering — now for
  whichever key the request would spend.
- **`doctor` reports an `upstream credential` row instead of `signed in` + `key valid`.**
  Having no key is the healthy state now, so it is no longer a failed check. The row names
  whose credential it just probed, and routes a rejection to whoever can fix it — the
  backend's own is a backend-side problem, not yours. A `bearer token` row appears only
  when `DAINTREE_API_KEY` is actually set.
- **`/backend` replaces `/login`'s endpoint picker.** With no argument it lists the
  endpoints and marks the live one — since the sign-in went away, nothing else named it
  on demand. With a target (`local`, `official`, a number, or a URL) it switches in place
  and **remembers the choice across restarts**, in a 0600 `endpoint.json` at the per-user
  state root holding only `{backend_url}`. `/backend default` forgets it.
  `DAINTREE_BACKEND_URL` and `--backend-url` still outrank it, so a harness is never
  redirected — and `/backend` says when that is happening, rather than looking broken.
- **The cockpit masthead names a non-default backend.** The deployed default stays
  silent; a local or custom endpoint gets a `backend` row in the permanent scrollback
  record, so a pasted transcript says which backend answered.
- `reset` now takes `project-state` or `all-data`; both are about this project's state.

### Security

- **Closed an ungrantable-tool bypass.** A grant scoped to a *risk class* authorised
  `daintree.call` — the raw, unbounded MCP escape hatch — so a watcher, timer, or
  unattended wake turn could reach any Daintree MCP method with no human present.
  `grant.create` refused those tools by NAME, but authorisation is "toolName OR riskClass";
  the check now runs at the enforcement point and in storage.
- **Tool arguments and results are scrubbed of known credential shapes** before they reach
  the debug log, audit rows, run events, the console/JSONL/host sinks, or the cockpit's
  activity rows (which seal into your terminal's native scrollback). Done at the event
  source and at the debug log's write boundary, so individual call sites cannot opt out.
  Conversation prose is deliberately not scrubbed — see `docs/beta/PRIVACY_AND_DATA.md`.
- **Endpoints are sanitized at the source.** Daintree's per-session MCP URL carries its
  bearer as `?session=<token>`; it now never reaches a report or a bundle.
- **`AUTO_APPROVE` is conspicuous.** A badge on the cockpit's live status line and its
  masthead, a warning line in the classic REPL banner, a stderr/JSONL warning on one-shot
  and host runs — which inherit the bypass too, being the same `main` actor — plus a line
  in `doctor` and every support bundle.

### Added

- **`support-bundle`** — a redacted diagnostics archive to attach to an issue, so nobody is
  asked for a debug log. Shows its manifest before writing, and refuses to write if its own
  scan finds anything credential-shaped.
- **`doctor --json`**, and a structured `doctor` with a stable id per check, one next
  action each, and a non-zero exit only on real failures. New checks: duplicate binaries on
  PATH (Daintree resolves by name, so an older copy silently shadows this build), platform
  supervision support, state-dir writability and privacy, schema version, owner lease,
  docs MCP, `AUTO_APPROVE`.
- **`reset project-state | all-data`** — a safe replacement for `rm -rf`-ing the state
  directory. Stops the daemon, takes the owner lease, shows what dies and what survives,
  backs up first (unless `--no-backup`), and refuses to run against anything that does not
  look like an assistant state directory.
- **Generated capability reference** — `docs/generated/TOOLS.md`, `COMMANDS.md`, and
  `COMPATIBILITY.md`, projected from the live registry and diffed in CI.
- **Internal beta documentation** under `docs/beta/`.
- **Prebuilt release archives** for macOS (arm64/amd64) and Linux (amd64/arm64), with
  `SHA256SUMS` and an SPDX SBOM, so a tester needs no Go toolchain. Not yet code-signed
  or notarized — that needs an Apple certificate, and a pipeline that claimed to notarize
  while skipping it would be worse than one that says it does not.
- **CI on macOS and Linux**, with the PTY render harness on macOS and the race detector on
  Linux; plus `govulncheck`, a `gitleaks` scan, and a scan of the working tree using this
  project's own redactor, so the scanner and the runtime cannot disagree about what a
  credential looks like.

### Changed

- A remote backend that does not serve `/v1/daintree/auth/verify` is a `doctor` failure;
  loopback stays lenient, since a local backend is routinely mid-change.
- `make db-reset` delegates to `reset project-state` instead of `rm -rf`-ing the state
  directory from the shell.
- Debug-log values are capped at 64 KiB, keeping the head *and* the tail (build output puts
  the failure at the end).

### Fixed

- Provider documentation now matches reality: OpenRouter is the only upstream transport.
- The platform matrix is honest: Windows is unsupported, and not merely for background
  work — the ownership lease every stateful mode takes is built on `flock`, which has no
  Windows port, so those modes cannot start there at all.
- `docs/TOOLS.md` and the README no longer carry hand-maintained tool inventories that had
  drifted apart (67 listed, 83 listed, 86 registered — including four that no longer
  existed).
- The debug log no longer claims values are untruncated, and the dead `mcp.credentials`
  replay instructions are gone with the code that produced them.
