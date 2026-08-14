# Changelog

Tester-facing changes. Pre-release: no version numbers yet, no backward-compatibility
guarantees, and the SQLite schema is a single clean baseline rather than a migration chain.

## Unreleased — internal beta preparation

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
- The stored sign-in being world-readable is now a `doctor` **failure**, not a note.

### Added

- **`support-bundle`** — a redacted diagnostics archive to attach to an issue, so nobody is
  asked for a debug log. Shows its manifest before writing, and refuses to write if its own
  scan finds anything credential-shaped.
- **`doctor --json`**, and a structured `doctor` with a stable id per check, one next
  action each, and a non-zero exit only on real failures. New checks: duplicate binaries on
  PATH (Daintree resolves by name, so an older copy silently shadows this build), platform
  supervision support, state-dir writability and privacy, credentials file mode, schema
  version, owner lease, docs MCP, `AUTO_APPROVE`.
- **`reset project-state | credentials | all-data`** — a safe replacement for
  `rm -rf`-ing the state directory. Stops the daemon, takes the owner lease, shows what
  dies and what survives, backs up first (unless `--no-backup`, and never the sign-in),
  and refuses to run against anything that does not look like an assistant state
  directory.
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

- Sign-in is **strict for remote endpoints**: a backend that does not serve
  `/v1/daintree/auth/verify` now fails sign-in rather than warning through and persisting
  an unverified, spendable key. Loopback keeps the lenient path for local development.
- `make db-reset` delegates to `reset project-state`, and **keeps your sign-in** — it used
  to delete it along with everything else.
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
