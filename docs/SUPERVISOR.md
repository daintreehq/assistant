# Persistent supervisor — the detachable per-project daemon

This is the architecture reference for the **persistent supervisor runtime**: the
process split that lets the assistant keep supervising Daintree work after the
interactive cockpit closes, then hand everything back on the next launch. It is the
companion to [`ARCHITECTURE.md`](ARCHITECTURE.md) (which describes the in-process
engines) and [`DAINTREE_HOST.md`](DAINTREE_HOST.md) (how Daintree launches the CLI —
several design decisions below exist because of that document; read its §7 lifecycle
matrix first if you touch credential handling).

> **Platform support: Unix only (macOS, Linux).** The whole model rests on two POSIX
> primitives with no Windows equivalent in this codebase: `flock` for the ownership lease
> (`internal/ipc/lock_unix.go`) and `Setsid` detachment for the daemon
> (`internal/supervisor/spawn_unix.go`). The `!unix` builds exist and compile, but they
> return `errFlockUnsupported` / "the supervisor daemon is not supported on this platform"
> rather than pretend — a lock that does not exclude is worse than no lock, because the
> single-owner invariant is what keeps two processes from writing the same `state.db`.
>
> Consequence for a Windows tester: timers, watchers, and async operations run only while
> the cockpit is open and **stop when it exits**. Nothing is corrupted and nothing is
> abandoned — the rows persist and are adopted on the next launch — but there is no
> background supervision, so "I'll tell you when it's done" cannot be honoured after the
> panel closes. Windows is not a supported platform until the ownership model is ported
> (owner lock, process lifetime, a named-pipe control transport, detached spawn).

## TL;DR

- **Exactly one process at a time owns a project's `state.db`** — an open assistant
  (cockpit/REPL/one-shot/host) or the supervisor daemon — serialized by an flock
  **owner lease** (`<stateDir>/owner.lock`). flock is the primitive because the
  kernel releases it when the holder dies: a crashed cockpit hands supervision back
  with zero cleanup code.
- The daemon (`daintree-assistant daemon`) is a **persistent contender** for that
  lease. While an assistant is attached (an open connection on the control socket)
  it stands down; the moment the assistant exits or crashes it re-acquires, builds a
  **headless App**, **adopts** the persisted watchers/async futures/timers/inbox, and
  runs the scheduler + async coordinator + **autonomous wake turns**.
- Supervision state is **project-scoped, not session-scoped**: `storage.Open` no
  longer cancels watchers, abandons async futures, or wipes the inbox. The explicit
  owner-boot reconciliation is `Store.BeginOwnership` (spawn-saga reset + adopted-work
  summary); `/clear` remains the only wholesale teardown.
- Daemon wake turns continue the project's **current conversation** (the
  `runtime_state` session pointer + the persisted backend state token) and dispatch
  tools as **`domain.ActorWake`**: reads run freely under the tier gate; mutating
  tools consume a scoped **wake grant** (`grant.create` with `actorType:"wake"`,
  `actorId:"wake"`) or become a **blocked pending-approval inbox item** for the next
  attach. There is deliberately no confirm hook in the daemon — nobody is present to
  answer one.

## Process & lease choreography

```
cockpit start ──► ensure daemon (spawn detached if absent, Setsid)
              ──► ReqAttach over <sockets>/dXXXX.sock   (carries fresh MCP creds)
              │     daemon: cancel supervision span → drain wake turn → close App/
              │     store → release owner.lock → stand down while the conn lives
              ──► flock(owner.lock)  → app.Create() → run exactly as before
cockpit exit  ──► App.Shutdown → release owner.lock → conn closes
              │     daemon: ConnClosed → re-contend → flock → new supervision span
cockpit CRASH ──► kernel releases flock + conn drops → same path, no cleanup code
```

- **Daemon singleton:** `daemon.lock` (flock) — a second `daemon` invocation exits 0.
- **Control socket:** NDJSON request/response (`internal/ipc`), request types
  `status` / `attach` / `credentials` / `shutdown`. Sockets live in a flat 0700
  per-user root (`~/.daintree/sockets/d<hash12>.sock`, override
  `DAINTREE_ASSISTANT_SOCKET_DIR`) because state-dir paths overflow darwin's
  104-byte `sun_path`.
- **A second cockpit** while one is attached gets `OwnerBusy` and exits with a clear
  message — there is never a second scheduler on one DB. (Daintree itself never
  launches two assistants per project — it displaces — so this only affects manual
  terminal launches.)
- **One-shot / doctor** attach briefly (never spawn) and release on exit;
  `DAINTREE_ASSISTANT_NO_DAEMON=1` disables spawn entirely (tests, kill switch).
- The daemon **idle-exits** (code 0) when nothing is left to supervise: no live
  watchers/async, no scheduled timers, an empty inbox, no wake activity — held for
  15 minutes. New work simply respawns one on the next interactive launch.

## Durability semantics

| State | Scope | Crash/restart behavior |
| --- | --- | --- |
| Timers | project (unchanged) | fire on the next owner's tick; missed repeats fold into one catch-up fire |
| Watchers | **project** (was session) | adopted as-is by the next owner; each check reconciles against the LIVE terminal/PR state, so a stale row self-corrects |
| Async futures | **project** (was session) | live rows re-enter the poll set (`Coordinator.Start` adoption); the FSM re-poll is idempotent |
| Attention inbox | **project** (was wiped per open) | carries over; the next owner surfaces it |
| Conversation | project | daemon continues the `runtime_state` current-session pointer; the backend state token is mirrored per session so skill-selection cadence survives the handover |

**Exactly-once completion delivery.** The async coordinator finalizes rows under the
live-claim, then publishes ONE group event, then stamps `queueEventId` on the whole
group **in one statement**. The crash windows resolve as:

- crash before finalize → rows still live → adopted → re-polled → published;
- crash after finalize, before publish → terminal rows with NULL `queueEventId` →
  adoption retries publish-only under the **same group dedupe key**;
- crash after publish, before stamp → the retry hits the queue's dedupe (same key,
  still unresolved) → count bump, no duplicate wake.

Watcher checks flush their publishes only after the finalize claim wins (a watcher
cancelled mid-check never wakes anyone); a lost publish is regenerated by the next
check because the underlying terminal state still holds. Timer fires remain
at-most-once per occurrence (`ClaimDueTimer`).

## Autonomous wake turns

The daemon's wake loop mirrors the embedded host's reactor: scheduler `onAttention`
→ `agent.IsActionableWake` filter → `BuildWakePrompt` + an `[unattended]` note →
`Session.Send(IsWake: true)` — single-flight, one retry, chained bursts. Every
successful wake is accumulated into the durable `detached_activity` record, which
the next attaching assistant consumes into its one-time "While you were away" notice
(cockpit note lines / REPL banner lines).

Safety: the daemon App is created with `DispatchActor: domain.ActorWake`, so every
tool dispatch takes the registry's **non-interactive branch** — tier gate, then
grant-or-denial for confirm-required tools. A denial publishes the standard blocked
inbox event with a pre-filled `grant.create` recommendation (`actorType: "wake"`),
which is exactly the "pending approval" the user resolves on attach.

## Credential lifecycle (the DAINTREE_HOST.md interplay)

Daintree injects a **per-session MCP bearer** and **revokes** it on window close,
view eviction, project displacement, and every re-provision; repeated auth failures
trip its abuse policy (see `DAINTREE_HOST.md` §3–4, §7). The daemon therefore:

- receives the freshest credentials on **every attach** (and standalone
  `credentials` pushes); a push during a live span **rebuilds the span** so the new
  token takes effect immediately;
- treats 401/403/`SESSION_BINDING_GONE`/`BINDING_STALE` statuses as **revoked** —
  it stops reconnecting entirely (never hammers a dead token) and publishes ONE
  deduped `Supervision blocked` inbox item that says how to resume (open the
  assistant in Daintree);
- treats connection-refused (Daintree closed) as an outage: reconnects on a 30s
  budget, blocks after 60s of sustained failure **only when live work is starved**,
  and resolves the blocked item on reconnect;
- **never abandons work blind**: async deadlines pause while MCP is unreachable
  (an invocation can't be "expired" without evidence), watchers keep re-arming.

The daemon survives Daintree killing the assistant's PTY (window close / app quit)
because it runs in its own session (`Setsid`) — but its token dies with the session,
so a fully-detached daemon can only *watch* again once a new assistant launch pushes
fresh credentials. That limit is inherent to Daintree's env-only, per-session token
model and is stated honestly in the blocked inbox item.

## Files

- `internal/ipc` — flock leases, socket path derivation, NDJSON control protocol.
- `internal/supervisor` — the daemon runtime (`runtime.go`), wake reactor
  (`wake.go`), client-side acquisition/spawn (`attach.go`, `spawn_unix.go`).
- `internal/storage` — `BeginOwnership`, `runtime_state`, unpublished-async queries,
  atomic group stamps (schema 9).
- `internal/asyncwork` — `Coordinator.Start` adoption + publish retries.
- `internal/cli/daemon.go` — `daemon` / `daemon stop` / `status` subcommands +
  `acquireOwnership` used by every App-creating path.
- `internal/e2e/daemon_test.go` — the crash/restart matrix (kill -9, publish-retry,
  revocation, grants, attach conflicts, idle exit).
