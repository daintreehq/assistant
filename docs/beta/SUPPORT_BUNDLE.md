# Support bundle

```bash
daintree-assistant support-bundle
daintree-assistant support-bundle --include-audit --out ~/Desktop/bundle.zip
```

A small archive designed to be safe to attach to an issue. It removes known credential
shapes and strips tokens out of endpoints — but it still contains local paths, your shell
and locale, and session-log filenames, so glance at the manifest before you send it.

---

## Why this exists

The alternative was "send me your debug log", and that was never an acceptable
instruction. A session log contains your entire conversation, terminal output, file
excerpts, issue bodies, and memories — most of it irrelevant to the bug, and some of it
your employer's. Asking for one turns every support request into a data-disclosure
decision you have to make under pressure, with no way to check what you are about to send.

So the bundle is built from the other direction: it collects the facts needed to diagnose
a version, compatibility, or environment problem, and **nothing else**.

---

## What is in it

| File | Contents |
| --- | --- |
| `versions.json` | CLI build version, Go version, OS/arch, backend protocol version, host NDJSON version, SQLite schema version, the task ids this build sends, the sanitized backend endpoint |
| `environment.json` | Tier, `AUTO_APPROVE`, offline, feature flags, state and project paths, TERM/SHELL/LANG, and credential **presence and length** — never values |
| `doctor.json` | The full diagnosis: every check with its id, status, detail, and structured data |
| `daemon.json` | Whether the supervisor is running, what it is supervising, and its last error |
| `debug-logs.json` | An **inventory** of your session logs — filenames, sizes, timestamps. Never contents |
| `audit.json` | Only with `--include-audit`: up to 200 recent tool **names**, actors, outcomes and durations |
| `redaction-report.json` | The bundle's scan of itself |

## What is deliberately NOT in it

- Your conversation.
- Your memories.
- Terminal output.
- File contents, including `DAINTREE.md` (only its size).
- Tool arguments and results — even in `audit.json`, and even though they are already
  redacted in the database. Redaction removes *credentials*, not *project detail*: a file
  path, a branch name, or an issue title survives it, and none of those is needed to
  answer "which tool failed, how often, and how long did it take".
- Your session logs. Listing them tells us whether tracing was on and which session to ask
  about; that is enough to start.

---

## How the redaction claim is checked

Every section is built from structured data and walked before it is serialized — there is
no path by which a byte reaches the archive without passing through one function. Then the
assembled bytes are scanned again, and the result is written into the bundle as
`redaction-report.json`. (That report is assembled last, so it scans every other file but
not itself — it contains only constants.)

**If that scan finds anything, no bundle is written.** Recording "this is unsafe" inside a
file and shipping it anyway would be the worst outcome: the archive carries the authority
of "the safe one to send" while the warning sits in an attachment nobody opens. You get an
error instead, and that error is itself a bug worth reporting (without attaching anything).

The report states its own limits, because it is best-effort and not proof: it cannot see a
credential that matches no known shape and is not one of this process's own registered
secrets — an opaque query token, a base64-wrapped value, a secret split across fields.
That is precisely why endpoints are stripped at the **source** rather than left to the
scan: Daintree's per-session MCP URL carries its bearer as `?session=<token>`, which
matches no shape and lives under a field called `url`.

---

## Before you send it

The manifest is printed **before** the file exists, with a size for every section and an
explicit list of what is excluded. Read it. If anything looks wrong, say no — nothing is
written.

You can always inspect afterwards:

```bash
unzip -l bundle.zip
unzip -p bundle.zip environment.json
```

The archive is written 0600, and refuses to overwrite an existing file — pass `--out` for
a different name rather than silently destroying the bundle you were about to send.

---

## If you think something leaked

Rotate the credential first. Then see [`../../SECURITY.md`](../../SECURITY.md). Rotating
costs a minute; not rotating can cost a great deal more.
