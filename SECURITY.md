# Security

## Reporting a vulnerability

Email **security@daintree.org**, or open a GitHub **security advisory** on
`daintreehq/assistant`. Please do not open a public issue for anything that discloses a
credential or lets one be obtained.

Tell us: what you did, what you expected, what happened, and the CLI version
(`daintree-assistant --version`). A `support-bundle` is ideal — it is designed to be safe
to attach. **Do not attach a debug log**; it contains your whole conversation.

## If you think a credential leaked

**Rotate it first, then tell us.** In order:

1. **Daintree MCP token** — close and reopen the Daintree window. Daintree rotates the
   per-session bearer on every re-provision, so a fresh session invalidates the old one.
2. **`DAINTREE_API_KEY`, if you set one** — the CLI does not ask for or store an upstream
   key, so most installs have none. If you exported one, revoke it at the provider and
   unset the variable.
3. **Anything else** (a PAT in a terminal, an env var an agent printed) — rotate at the
   source. The assistant only ever handled a copy.

Then report it, including where you saw it: a log file, terminal scrollback, a support
bundle, an audit export, or the cockpit. **Which surface it appeared on is the most useful
detail** — each has a different redaction path, and knowing which one failed points
straight at the gap.

## Reporting an unsafe action

Something that mutated state without asking, or asked for less confirmation than its risk
class implies, is a security bug even when nothing leaked. Include:

- the exact prompt
- what it did
- whether `AUTO_APPROVE` was on (the cockpit shows a red badge; `doctor` reports it)
- your tier (`/permissions`)
- a `support-bundle --include-audit`, which carries the tool sequence

## The model this project is defending

The CLI normally holds **no upstream credential at all** — the backend supplies its own,
and a request carries no `Authorization` header. What it does hold is the Daintree MCP
token, which authorises **system-tier** actions for its validity window, plus an optional
`DAINTREE_API_KEY` bearer on the rare install that sets one. Both are treated as material
that must never reach a durable or shareable surface.

Enforced properties, each with tests:

- **No tool edits project files.** Any tool whose name looks file-mutating is rejected at
  startup, before boot completes. Changes go through a *visible* agent terminal.
- **Credentials and the endpoint never come from a project `.env`.** A bound repository
  cannot redirect where a turn is sent, nor supply a bearer on your behalf.
- **Tool activity is scrubbed of known credential shapes** — tool call arguments and
  results, at the event source, which covers the debug log, audit rows, run events, the
  console and JSONL sinks, and the cockpit's activity rows. The debug log additionally
  scrubs every value at its write boundary. **Conversation prose is not scrubbed**: your
  messages and the model's replies are stored and displayed verbatim, so a credential the
  model repeats is not caught.
- **Endpoints are sanitized at the source** — Daintree's per-session MCP URL carries its
  bearer as a query parameter, which no shape-matcher would catch.
- **Mutating actions confirm**; git and system need a typed phrase. Unattended actors
  cannot use `AUTO_APPROVE` at all — a scoped grant is their only path.
- **Some tools can never be granted.** `daintree.call`, `grant.create` and `grant.revoke`
  are refused at the enforcement point, not merely at grant-creation time.
- **`support-bundle` refuses to write** if its own scan finds anything credential-shaped.

Known limits, stated rather than implied:

- The conversation, artifacts, and scheduled job payloads are stored **verbatim**. Each
  needs its raw value to function; they are protected by scope (0700/0600, local, never
  transmitted), not by scrubbing. See `internal/redact`.
- Redaction is best-effort against *unknown* credential formats. It catches known shapes
  and this process's own secrets by exact value.
- A model can repeat a credential it read from a terminal. No metadata-level redaction
  prevents that.

## Supported versions

Pre-release. Only the latest build is supported; there are no backported fixes.
