# Troubleshooting

**Run `daintree-assistant doctor` first.** It is not a formality — it names most problems
precisely and tells you the one thing to do next. This page is the decision tree behind it.

Every check has a stable id. The ones with a failure mode worth explaining are below;
`tools.registered` and `project.instructions` are informational and are not listed.

---

## Reading doctor

```
  ok    platform         darwin/arm64 — background supervision supported
  ok    binary on PATH   /opt/homebrew/bin/daintree-assistant (v0.4.1, this build)
  FAIL  upstream credential  the provider rejected this credential: ...
                             → The backend's own upstream credential is rejected — this
                               is a backend-side problem, not yours.
```

- **`FAIL`** — broken. This is why the exit code is non-zero.
- **`WARN`** — degraded, but you can work. Worth understanding.
- **`?`** — could not be checked. Not a problem in itself.
- **`-`** — not applicable here.

For scripts:

```bash
set -o pipefail
daintree-assistant doctor --json | jq -e '.summary.healthy'
```

Both parts matter: without `pipefail` the shell sees `jq`'s status, not doctor's, and
without `-e` a `false` result still exits 0.

---

## `auth.credentialUsable` — the provider rejected this credential

There is nothing for you to paste, and there is no case where this one is yours. The row
reports on the **backend's own** upstream provider credential — the one the deployment
spends — and its detail says so. It is not a question about your account;
`daintree-assistant auth status` answers that one. A rejection here is ours to fix;
report it.

`DAINTREE_API_KEY` and `--api-key-file` do not change that, even though the row mentions
one when it is set. They supply a bearer that says who is CALLING, never who pays: the
backend funds every model call from its own credential either way. So a credential
rejected upstream cannot have been yours, and unsetting the variable will not fix it.

## `auth.credentialUsable` — valid but NO CREDIT remaining

Real, and every turn will fail until the account behind it is topped up — but that
account is the deployment's, not yours, so it cannot be topped up from here. Report it.
Remember that background work spends too: watcher checks and async completions are model
calls.

## `auth.credentialUsable` — this backend does not serve `/v1/daintree/auth/verify`

This is an **endpoint problem, not a verdict about any credential** — the check never ran,
so nothing was learned either way. Either the endpoint is out of date or something (a
corporate proxy, a captive portal) is intercepting the route. Retry off the proxy, or
point `DAINTREE_BACKEND_URL` at a local backend meanwhile. A **loopback** endpoint is
allowed to lack this route — that is the development loop — but a remote one is not.

## A turn ended with `Account problem: …`

The turn stopped at the account door rather than at the model, and the message names the
one thing to do. Three groups, and they need opposite responses:

| What it says | What it means | What to do |
| --- | --- | --- |
| requires an account / session was ended elsewhere | identity | `daintree-assistant auth login` |
| would not accept the stored credential for this turn | the credential may simply not have been renewed on this turn | try again first; only run `auth login` if it persists |
| does not accept this client's credentials / not for this operation | the deployment refuses a credential that is FINE | nothing local helps — signing in again produces the same result |
| no plan that includes the assistant | the sign-in is good, the plan is missing | `auth status --refresh`, which prints the subscribe link when the deployment publishes one |
| plan is not currently active | a plan exists and has lapsed | the **billing portal** — a second checkout is how people pay twice |
| reached its usage limit for the period | the plan's cap is spent | wait for the period to roll over, or change the plan |
| could not be checked just now | a dependency is down, nothing was established | wait and retry; your sign-in is unaffected |

The last row is the one worth reading twice: "could not check" is never "you are not
subscribed", and nothing in that state should send you to a login or a checkout.

A per-account request-rate limit is the exception to all of this: it arrives as an
ordinary `Model rate-limited:` reply rather than `Account problem:`, because it clears on
its own within seconds.

If it says the machine **could not produce a credential**, no request was made at all.
That covers a locked or unavailable keychain, and also a refresh or discovery that could
not complete. Run `daintree-assistant auth status` to see which: against a store that
cannot be written, signing in again hits the same failure, whereas a refresh grant that
has genuinely ended does need a fresh login.

## `auth status` shows a plan, but says it could not be checked

Both are true, and the pairing is deliberate. When a `--refresh` cannot reach the backend,
the plan this process last learned stays on the block rather than blanking — blanking
would report a subscription as gone because the network was. What changes is the state and
the timestamps: `plan checked` says how old the billing answer is, `verified` says when
the session itself was last confirmed, and `plan source  cache (may be out of date)`
appears when the backend answered from its own cache rather than a live lookup.

Read the timestamps before acting on the plan. Nothing about it is stored between
processes, so a brand-new process shows no plan at all until something asks.

## `backend.reachable` — UNREACHABLE

Network, DNS, or a stopped local backend. The deployed backend scales to zero, so a cold
start is slow but should not fail. For local:

```bash
cd ../assistant-backend && python -m daintree_assistant_server
```

## `backend.tasks` — DRIFT / NONE advertised

The CLI and the backend disagree about task ids. Every id listed is one this build will
actually send, so a missing one is a guaranteed 404 mid-turn. Update whichever side is
older. `docs/generated/COMPATIBILITY.md` lists exactly what this build expects.

---

## `mcp.daintree` — not configured (DEGRADED LOCAL MODE)

Expected from a plain terminal. **Not** a normal way to run: without it there are no
terminals to read, no agents to spawn, no worktrees to inspect. Launch from inside
Daintree.

What still works: filesystem reads, memory, timers, the attention inbox, grants, the audit
trail, the async and workflow ledgers, `daintree.status`, and `context.snapshot`.

**Watchers are the subtle one.** `watcher.terminal.create` will write a durable row, but
the engine polls through Daintree — so a watcher created while disconnected observes
nothing until the link returns. Creating one is bookkeeping, not supervision.

## `mcp.daintree` — configured but NOT connected

Daintree closed, revoked, or replaced this session's token. Daintree rotates it on every
re-provision and revokes it on window close, eviction, and displacement. Reopen the
assistant from Daintree, or `/reconnect`.

Do **not** loop on reconnects with a dead token: repeated auth failures trip Daintree's
abuse policy. The CLI already latches on a terminal credential failure and waits for fresh
credentials.

---

## `state.owner` — could not take the owner lease

Exactly one process at a time may own a project's state. Usually another assistant is open.

```bash
daintree-assistant status        # is a daemon running?
daintree-assistant daemon stop
```

If nothing is running, the state dir is probably not writable — check `state.dir` below.

## `state.dir` — not writable

Permissions, a full disk, or a read-only mount. Writability is proven by writing, so this
is not a mode-bit guess. Fix the permissions or point `DAINTREE_ASSISTANT_STATE_DIR`
somewhere writable.

## `state.dir` — readable by other users

Run the `chmod` doctor prints. No secret lives there — the state dir holds only a
non-secret `auth/credential.json` descriptor naming which account this machine has, if
any, while the token itself is in your OS keychain — but it still holds your conversation
database and
artifacts, so a mode wider than the CLI wrote means something else changed it.

## `state.schema` — the database is from an older version

Pre-release policy is a single clean schema baseline rather than a migration chain. An
interactive launch moves the old database aside to a timestamped backup and recreates it,
telling you where the backup went. A non-TTY launch fails loudly instead, so a script never
destroys state silently. To do it deliberately:

```bash
daintree-assistant reset project-state
```

**The opposite case — the database is NEWER than this binary understands — is different,
and never a reset.** It means a *newer* install already upgraded this project's database,
and an older copy of the binary (often the `binary.duplicates` problem below) just opened
it. Resetting would destroy state the newer install wrote and needs; there is nothing to
back up your way out of here. The fix is to update this binary (`make install` from a
current checkout, or remove the stale duplicate from PATH), not to reset anything.

---

## `binary.duplicates` — more than one copy on PATH

Daintree resolves the CLI **by name**, so an older copy earlier on PATH wins — and the
symptom is not "wrong version", it is a feature that mysteriously doesn't exist or a bug
that was fixed weeks ago. Doctor lists every path and what each one reports. Remove the
ones you are not using, then `make install`.

## `platform.supervision` — not supported

You are on Windows, where the assistant does not run at all — the ownership lease every
stateful mode takes is an `flock`, which has no Windows port. See
[`INTERNAL_BETA.md`](INTERNAL_BETA.md#supported-platforms).

## `safety.autoApprove` — ON

Mutating actions run without asking, **on every surface** — the attached session, the line REPL,
one-shot, `--json`, and the host — because all of them are the same `main` actor. It does
not widen the tier gate, and unattended actors (watchers, timers, wake turns) still need a
scoped grant. Unset `DAINTREE_ASSISTANT_AUTO_APPROVE` unless this is an automated harness.

---

## Behaviour problems (the model, not the environment)

Doctor is green and it still does the wrong thing. Triage into one of four buckets — it
determines who can fix it:

| Bucket | Looks like | Fixed in |
| --- | --- | --- |
| **Selector / runbook** | wrong or missing runbook, bad plan, stops early, dumps a speculative 60-step plan | the backend's prompts/runbooks |
| **Tool contract** | invalid arguments, a tool used for the wrong job, an unclear schema | this repo's tool definitions |
| **Runtime** | backend, MCP, state, daemon, retries, lifecycle | this repo's runtime |
| **UI / support** | rendering, cancellation, install, confusing messaging | this repo's attached session and CLI |

Then:

```bash
daintree-assistant support-bundle --include-audit
```

and open an issue with what you expected versus what happened. The single most valuable
report is **"it said it did something it did not do"** — a loud failure is a small bug; a
confident false success is the one we most need to see.

---

## Things that are working as designed

**It asked before doing something.** Terminal, project, external, git, and system actions
confirm; git and system need a typed phrase. That is the safety model.

**A fresh session's transcript is empty but watchers are still running.** Deliberate.
Project state is project-scoped, not session-scoped; the "While you were away" note tells
you what happened while you were gone.

**It said it would tell you when something finished, and Daintree was closed.** Supervision
reaches your terminals through Daintree. It pauses rather than fabricating an outcome, and
resumes on the next launch. (If `daintree-assistant status` says no daemon is running, the
supervisor never started — spawning it is deliberately non-fatal — and background work will
not survive the attached session exiting at all.)

**A long wait became background work.** Foreground waits have a shared budget so a chain of
them cannot block a turn indefinitely. Past it, the work moves to async.
