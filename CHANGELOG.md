# Changelog

Tester-facing changes. Pre-release: no version numbers yet, no backward-compatibility
guarantees, and the SQLite schema is a single clean baseline rather than a migration chain.

## Unreleased — internal beta preparation

### Added — Daintree account sign-in

- **`daintree-assistant auth login | status | logout | disconnect`.** A browser
  Authorization Code + PKCE flow; the refresh token goes to the OS credential store
  (macOS Keychain, Linux Secret Service) and nowhere else, and the access token stays in
  process memory. One login covers every project sharing a state root.
- **Whether you need one is the BACKEND's answer, not the binary's.** A deployment
  publishes `configured` and `required`; the one this build points at today publishes
  neither, so nothing changes for anyone — `auth status` says
  `accounts  not offered by this backend`.
- **`auth status --refresh` asks the backend who you are and what your plan permits.**
  It makes one request to `/v1/daintree/account` — two if an expired token has to be
  renewed and the request replayed, none if the deployment has no accounts — and fills in
  whichever of the email, plan, entitlement source and check time the backend reports. Before this it renewed the Supabase token and then
  printed plan fields nothing populated, so a fresh process could never learn that a
  checkout had succeeded. Plain `auth status` still makes no account request: it stays
  fast and offline-capable, which is what you want when the network is the problem.
  Nothing about the plan is written to disk — a plan on disk is a plan that can be wrong.
- **`auth login` names the plan, and never calls a billing problem a login failure.**
  Signing in successfully with no plan is a success: it says so, keeps the credential,
  exits 0, and shows where to choose one. A LAPSED plan gets the billing portal instead —
  a second checkout is how people pay twice. A billing outage says the plan could not be
  checked and leaves the session alone.
- **`/login`, `/logout` and `/account`, inside a running session.** Sign-in was always the
  engine's own business — the PKCE exchange, the browser, the loopback listener and the
  keychain all live here — but there was no way to REACH it from a session, so an
  embedding host had to shell out to the subcommands from its own settings screen. These
  are the same manager and the same account read as the `auth` commands, so the two
  surfaces cannot disagree about the STATE of a credential — they word it differently,
  because a card in a panel and a terminal status block are not the same shape.
- **`/account` asks the backend rather than reciting what this process remembers.** It
  used to format an in-process snapshot, which in a session that had never made an account
  request is almost nothing: a perfectly good keychain sign-in showed no plan and a state
  line saying the backend had not confirmed anything, while `auth status --refresh`
  against the same credential named the plan. It also means a plan bought on the website
  becomes active on the next `/account` that successfully reaches the backend — no second
  sign-in to pick up a purchase you have already paid for. A read that fails says so and
  leaves the last known state alone; it never reports a billing outage as "no plan".
- **A sign-in that cannot open a browser now says what to run.** "Re-run with `--no-open`"
  means nothing inside a panel that has no flags and no prompt; the remedy is now the whole
  command, and it is attached to the sign-in rather than to one launcher, so it appears
  however the browser failed.
- **A turn that stops at the account door now says so.** Account verdicts used to arrive
  as `Model error: backend: http 401/402/403/429/503 …`, which describes a billing or
  sign-in problem as a model problem. Twelve of the thirteen account codes now open with
  `Account problem:` and name the one thing to do, keeping "could not check" firmly apart
  from "not subscribed". The thirteenth, a per-account request-rate limit, keeps the
  ordinary rate-limit reply because it clears on its own.
- **Corrected who owns the upstream credential across every user-facing surface.** Messages
  said the provider had rejected "your API key", offered a top-up link for an account you
  do not hold, and pointed at `/login` at a time when no such command existed. The backend funds
  every model call from its own credential; `DAINTREE_API_KEY` and `--api-key-file` say
  who is CALLING and are never spent upstream.
- **The backend's account verdicts now change local state.** An eligible protected 2xx
  marks the session active — with two exceptions, both deliberate: the account endpoint's
  own 200 confirms nothing (it answers 200 for a caller with no plan) and a success never
  overwrites a known missing or inactive plan; an expired credential is refreshed and the request replayed once
  (never after anything visible has streamed), a revoked session is deleted locally, and
  plan or dependency failures leave it alone. Unattended supervision stops when a session
  it was using ends.
- `DAINTREE_API_KEY` is unchanged and still not a way to pay: it says who is calling.

### Fixed — account correctness

- **A late revocation could delete the credential a sign-in had just stored**, while the
  sign-in reported success. Two windows, both closed: one inside a single process (the new
  identity is now published before the credential lock is released) and a wider one across
  processes, where a revocation raised elsewhere could not see a login that had happened
  here and stayed wrong until its next token read. The safe error is now the other way
  round — a revocation may be deferred, never a good credential deleted.
- **A cancelled sign-in could resurrect a session that ended while it was open.** Signing
  out during a five-minute browser round trip, then cancelling the sign-in, restored the
  old "signed in" state with no credential behind it. The guard compares this process's
  own identity, so it covers a sign-out or a revocation in the SAME session; one in
  another process is still only noticed on the next token read.
- **A session kept reporting itself able to spend after another process signed out.** The
  token was already gone; only the state disagreed — and that state is what the background
  supervisor consults before an unattended turn. Deliberately narrow: a sign-in still
  running, a memory-only credential and a refused one all keep saying so, because each is
  a diagnosis "signed out" would hide.
- **A sign-in no longer claims the current backend when `/backend` moved under it.**
  Switching endpoints while a browser sign-in was open left the credential stored for the
  endpoint it started against and the card announcing "Signed in" above the new endpoint's
  signed-out state. It now says which happened.
- **A sign-in that cannot record itself no longer half-succeeds.** If the shared revision
  could not be bumped, the credential stayed written while the command reported failure —
  so a fresh process would load a session this one said it could not create. It is now
  rolled back, and "login failed" is true.
- **The CLI rejected a valid backend response.** It required a `checked_at` timestamp on
  every reply, including the identity-only one the backend sends when entitlement lookup
  is not configured — turning a correct answer into "could not verify your plan". The
  contract now matches the server on both halves: that reply carries no entitlement fields
  at all, and a completed lookup must carry all of them.

### Changed — no API key to paste

The entry below is kept as written when it landed. Sign-in returned in the form above,
which is an ACCOUNT, not the provider key this describes removing.

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
- **`/backend` replaces `/login`'s endpoint picker.** In the cockpit, bare `/backend`
  opens a selection sheet — ↑/↓, letter keys, Enter — marking the live endpoint. With a
  target (`local`, `official`, a number, or a URL) it switches straight away. Either path
  **remembers the choice across restarts**, in a 0600 `endpoint.json` at the per-user
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
