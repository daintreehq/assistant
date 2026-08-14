# Daintree Assistant backend — CLI integration

The CLI is a **thin local runtime**. It no longer acts as a model client; instead it
talks to the **Daintree Assistant backend** (a Daintree-native HTTP API — *not*
OpenAI-compatible). The backend owns the system prompt, developer instructions, skill /
runbook selection, model choice, prompt assembly, the utility-model prompts, and prompt
caching. The CLI owns the terminal UI, the visible conversation, the local tool registry
+ execution, permissions, runtime / project context collection, memory + scheduler state,
stream rendering, and the opaque backend state token.

```
User → Daintree CLI ──(structured startup + visible conversation + runtime/turn + tools)──► Daintree backend
        │  stores conversation, exposes & runs local tools,                            owns prompts, skills,
        │  streams assistant text, persists backend state                              model routing
        ◄──(named-event SSE: meta / delta / done / error)────────────────────────────┘
                                                                                             │
                                                                            (caller's key, request-scoped)
                                                                                             ▼
                                                                                         OpenRouter
                                                                                             │
                                                                                             ▼
                                                                                   the selected model
```

**OpenRouter is the only upstream transport.** The backend reaches every model — the main
orchestration model and every utility model behind summarize / extract / classify /
checkpoint / workflow tasks — through OpenRouter, using the caller's own key on a
per-request basis. Model *identities* in this repo (DeepSeek V4 Flash, GPT-5.6 Sol) are
OpenRouter route ids, not direct provider integrations, and where a comment names
model-specific protocol behaviour it means "that model's behaviour when reached through
OpenRouter." The CLI carries no provider key, no provider client, and no pricing table;
reintroducing one would let a handler bypass the backend that owns prompts, skills, and
credentials.

## Endpoint and sign-in

Two constants: `backend.DefaultBaseURL` = `https://assistant.daintree.org` (the deployed
backend, and the default for a fresh install) and `backend.LocalBaseURL` =
`http://127.0.0.1:8473` (a backend you run yourself).

**Every request authenticates, in every environment.** There is no unauthenticated mode
to fall back to — not even locally. The `Authorization: Bearer <key>` token is the
*caller's own* API key, and it doubles as the upstream credential funding that turn's
model calls: the server holds no provider credential of its own. For early testers the
key is literally their OpenRouter key (`sk-or-v1-…`); later it becomes a subscription key
the backend maps server-side, with no wire change. Only `/healthz`, `/readyz` and
`/version` are open.

The backend validates the key **structurally only** (printable non-space ASCII, bounded
length). That gives two deliberately distinct failures:

| condition | response | CLI predicate | meaning |
|---|---|---|---|
| missing / malformed bearer | `401 invalid_api_key` | `Error.IsAuth()` | fix your header — sign in again |
| well-formed key the provider rejects | `401 provider_invalid_api_key` | `Error.IsUpstreamAuth()` | fix your account — bad or revoked key |

Note the two 401s. They mean opposite things and are separated by **code**, never by
status: `IsAuth()` deliberately excludes the provider codes, because telling someone
whose key the provider revoked to "check you pasted it in full" sends them round a
re-entry loop that cannot work. See the upstream taxonomy below.

Because validation is structural, `/v1/daintree/capabilities` answers **200 for any
well-formed string**. That is why sign-in also calls:

```
POST /v1/daintree/auth/verify
  →  {"valid": true, "usable": true, "reason": "ok", "detail": "...",
      "label": "...", "limit_remaining": 1.23}
```

It asks the provider directly (its key-introspection call — no tokens spent) and is the
only check that can catch a key that is well-formed but wrong, revoked, or unfunded.
`valid:false` comes back as **200**, not 401: "this key is invalid" is a successful
answer to the question, and a 401 would tell the client to retry the same header. A
provider we cannot reach propagates as 502 `upstream_error`, because then we do not
know — and "could not check" must never be reported as "invalid".

`valid` and `usable` answer different questions — "does the provider recognise this
credential" versus "can the account behind it fund a turn" — and `reason` (`ok` /
`provider_rejected` / `credits_exhausted`) is the stable outcome to branch on. A
recognised key with a spent balance is `valid: true, usable: false`; a client reading
only `valid` shows a successful sign-in and then fails on the first real request.
`KeyVerification.Usable` is a `*bool` so a backend that predates the field decodes as
"not reported" rather than as `false`, with `IsUsable()` falling back to
`limit_remaining`.

`backend.CheckSignIn` is the shared client-side helper both entry points run, so the
startup flow and `/login` cannot diverge on what "verified" means. It gates hard on
capabilities and on an explicit `valid:false`, and downgrades to a **warning** when the
provider was unreachable or the key is recognised but out of credit. Cancellation and
timeout are the exception: they are hard failures, since neither is evidence about the
key nor consent to persist an unverified one.

**A backend that does not serve the route at all is a compatibility failure**, scoped by
`backend.AllowsUnverifiedSignIn`:

| endpoint | `/v1/daintree/auth/verify` absent (404 / 405 / 501) | rationale |
|---|---|---|
| any **remote** host — Official, staging, custom | **hard failure** — `ErrBackendIncompatible` | the deployed backend has served the route since 2026-08, so its absence means an obsolete deployment or an intercepting proxy. Warning through would persist an unverified *spendable* key — the exact thing verification exists to prevent |
| **loopback** (`127.0.0.0/8`, `::1`, `localhost`) | warning, sign-in proceeds | the `python -m daintree_assistant_server` development loop; there is no network to intercept and no third party to trust |

The predicate is deliberately **"is this local?"**, never "is this the official
endpoint?". The latter fails **open**: its alias surface is unbounded — `:443`, an empty
port, a trailing DNS root dot, an IDNA spelling, userinfo — and every spelling the check
failed to anticipate would silently take the lenient path against a *remote* host. The
loopback test fails **closed**: no `evil.example` spelling parses to `127.0.0.1`, and an
unparseable URL is treated as remote. Both are pinned by `TestAllowsUnverifiedSignIn`.

404, 405, and 501 all map to `ErrVerifyUnsupported`: the client issues the contractually
correct `POST`, so any of the three is evidence the route is absent or intercepted.
Transport failures and other 5xx do **not** — those mean "could not check", which stays a
warning and must never be reported as a verdict about the key.

`ErrBackendIncompatible` is deliberately distinct from `ErrKeyRejected` because the fixes
are opposite: re-pasting the key cannot help, so both sign-in surfaces say "your key is
fine — retry off any proxy, or point at a Local backend meanwhile" rather than sending a
tester hunting for a credential problem they do not have.

### Key hygiene: the client scrubs on the way out

The bearer token is the caller's spendable credential, and an upstream we do not control
can echo the `Authorization` header back at us. Every path where that could happen is
scrubbed **inside the client**, not at the display sites — there are many sinks (turn
error rendering, doctor rows, login messages, the retry hook, the debug-log writer) and
exactly one place they all get their values from:

| path | where | why it needs its own scrub |
|---|---|---|
| `verify` **200** body (`detail`, `label`) | `VerifyKey` | the *success* path — no error wrapper covers it, and it feeds the login confirmation, `/auth`, and the cockpit sheet |
| any JSON endpoint's error body | `readErrorResponse` → `scrubBackendError` | scrubbing here also cleans the error handed to the retry-observability hook |
| marshal / decode errors | `doJSON` → `scrubError` | a decoder's message can echo the payload |
| terminal SSE `error` event | `respondStreamOnce` → `scrubError` | `parseRespondStream` is a free function with no access to the key |

`Message`, `Param`, `Code`, and `Type` are all scrubbed: the last two are nominally
stable machine identifiers, but they are still backend-controlled strings. This is
defense-in-depth, not a licence for the backend to leak — but custom endpoints and
proxies are outside our control, so the client cannot assume good behaviour upstream.

### Loopback URLs never take a proxy

The default transports use `proxyExceptLoopback`, not `http.ProxyFromEnvironment`. Go's
stock bypass fires only for the exact lowercase host `localhost` and parseable loopback
IP literals, while `AllowsUnverifiedSignIn` also accepts `LOCALHOST`, `localhost.`,
`dev.localhost`, and `127.0.0.1.` — all of which genuinely address this machine. Left
unaligned, those four spellings would be classified onto the *lenient* sign-in path while
being *routed* through `HTTP_PROXY`: over plain http the proxy would receive the spendable
token in clear text, and a proxy answering capabilities-then-404 could push the request
back onto the lenient path and persist an unverified key. Deriving both from the same
predicate makes them agree by construction, so widening the loopback definition later
cannot silently reopen the gap. Pinned by
`TestProxyIsBypassedForEverySpellingClassifiedAsLocal`.

**The CLI never probes the provider itself.** It holds no provider client by design, and
the caller key becomes a subscription key later — at which point only the backend can
resolve it. Adding an OpenRouter call here would break both properties.

### Signing in

```bash
daintree-assistant login    # choose official / custom / local, paste the key
daintree-assistant logout   # forget it
daintree-assistant doctor   # `signed in` + `key valid` rows
```

Inside the cockpit, `/auth` shows the active sign-in (read-only) and `/login` opens a
sheet that re-authenticates **in place** — endpoint picker, masked key entry, verify,
then a hot swap of the live backend client via `App.SignIn`. No restart: every consumer
holds a `backend.Swappable` (never a raw client), so `agent.Session`, the watcher engine,
the async coordinator, and the workflow layer all follow the swap. A turn already
streaming finishes on the old client — an endpoint cannot change mid-stream without
corrupting the transcript — so the change applies from the next message. `/login` is
REFUSED while a turn is running: a turn is multi-round (Session re-calls `RespondStream`
after every tool round), and swapping between rounds would send the next round to a
different endpoint carrying a `state` token the previous one signed.

A custom endpoint may only use `http://` for **loopback** hosts — every request carries
the key as a bearer token, so plain HTTP to a remote host would put a spendable secret on
the wire. Embedded userinfo (`https://user:pass@host`) is rejected for the same reason.

`login` writes `{backend_url, api_key}` as 0600 JSON at the **per-user state root**
(`~/.daintree/assistant-cli/credentials.json`) — one sign-in serves every project. An
explicit `DAINTREE_ASSISTANT_STATE_DIR` / `--state-dir` moves it alongside that dir
instead, which is what keeps tests and benchmarks from reading or clobbering a real
sign-in. Nothing is written until verification passes, so a typo never persists. Implementation: `internal/credentials` (storage),
`internal/cli/login.go` (flow), `internal/config` (resolution).

Startup gates on a resolved key (`cli.ensureSignedIn`): an interactive TTY launch runs
the login flow inline, before the ownership lease and before `app.Create`; every
non-interactive path (one-shot, `--json`, `host`, `daemon`) fails fast with
`not signed in — run daintree-assistant login`.

### Overrides

`DAINTREE_API_KEY` and `DAINTREE_BACKEND_URL` are **trusted-env only** — a bound
project's `.env` can supply neither. That is a security boundary, not tidiness: the key
is spendable and the URL decides where it is sent, so a cloned repo must not be able to
inject either. Overriding the URL keeps the stored key (the key is the caller's own
credential, equally valid against any endpoint), which is exactly the local dev loop:

```bash
cd ../assistant-backend
python -m daintree_assistant_server            # serves on 127.0.0.1:8473 (its .env pins the port)

DAINTREE_BACKEND_URL=http://127.0.0.1:8473 daintree-assistant   # same sign-in, local backend
```

e2e tests use the same override to point at a fake backend, plus `DAINTREE_API_KEY` to
clear the sign-in gate.

## Wire contract

Authoritative spec: `../assistant-backend/docs/DAINTREE_API.md`. Exact field types:
`../assistant-backend/src/daintree_assistant_server/contracts/daintree_api.py`. Request
validation: `.../services/validation.py`.

The Go client mirrors it in `internal/backend`:

- `contracts.go` — the strict request envelope (`RespondRequest`: `protocol_version`,
  `session{id,turn_id,instruction_revision,round}`, `state`, `startup`,
  `input{messages,tools,tool_choice}`,
  `runtime`, `turn`, `selection`, `generation`, `client`), the response / stream payloads,
  the tasks envelope, and capabilities.
- `sse.go` — the **named-event** SSE parser (`meta` → `delta` → `done` / `error`). `meta`
  is always first (carries the refreshed `state` token + the first-class `skills` block)
  and is flushed as soon as selection finishes, before the upstream model connects. The
  client immediately emits de-duplicated `newly_loaded` refs through `OnSkillLoaded`, while
  deferring stateful `OnMeta` until an attempt commits. A retry after meta adopts its signed
  `state` in the next POST so the backend reuses the same selection. If a terminal retry
  then dies before its own meta, the last received/adopted meta is forwarded once so that
  state is still persisted. Terminal error events may carry a top-level `retry_after`
  string, which feeds the bounded retry delay; provider timeouts are transient. Tool-call
  deltas accumulate by index (OpenAI-style fragments). EOF before `done` is an error — the
  parser never fabricates a successful finish.
- `client.go` — `RespondStream`, `RunTask`, `Capabilities`, `Health`, `Ready`, `Version`.
- `retry.go` — the transient-failure retry policy applied to **every** call above (10
  attempts, exponential from 500ms, settling into a 10–15s poll ≈ 50–75s of backoff,
  the whole call capped by a 2-minute elapsed window — sized to ride out a backend
  restart). Never replays a deterministic failure (auth 401/403, contract 400, protocol
  426), an application **verdict** (`task_output_invalid`, `upstream_error`,
  `internal_error` — keyed on `error.code`, since the backend reuses 502 for both a
  verdict and a real gateway failure; a replay would re-run the model to reach the same
  answer), or a respond turn that has already streamed visible tokens. `WithoutRetry(ctx)`
  makes a call one-shot, for `/doctor`'s probes. See
  [RUNTIME.md](RUNTIME.md#model-errors-rate-limit-backend-down-unavailable) for how it
  layers with the backend's own provider retries and the MCP read retries.

### Cost reporting

Every upstream call is funded by the caller's own OpenRouter key, so what a request cost
is **their** money. The backend reports OpenRouter's own figures — never a token-price
estimate, because the router knows which of ~24 endpoints served the call and what cache
discount applied, and anything derived client-side would be a guess presented as a bill.

| where | field | meaning |
|---|---|---|
| `/respond` body, and the terminal SSE `done` event | `cost: {total, main, selector, complete}` | the whole request, across every upstream call it made |
| `/respond` `usage.cost` | float | the MAIN completion only |
| `/tasks` `usage.cost` | float | that task's total (a repair pass is included) |
| `/v1/daintree/capabilities` | `respond.cost_reporting` | the contract, advertised so a client can degrade |

Two rules the CLI **implements** rather than infers, because getting either wrong
under-reports someone's actual bill while looking like a receipt:

1. **Absent means unknown, never free.** The block is omitted rather than zero-filled.
   Test key PRESENCE, not `!= null`.
2. **`complete: false` means `total` is a floor.** A call ran whose cost could not be
   measured — an unreported selector, or a speculative generation cancelled mid-flight
   (OpenRouter reports usage only in a stream's final chunk, which a cancelled stream
   never sends). A turn that *skipped* selection stays `complete: true`: no call
   happened, so nothing is missing.

Collapsed to one client rule: **a session total is a lower bound if any request in it was
incomplete or carried no cost block at all.**

Two gaps `complete` does not currently close, both of which the CLI handles on its own
side. A **retried** respond call bills once per attempt, but each attempt's `cost.total`
covers only its own request — the backend aggregates re-rolls *within* a request, never
across HTTP attempts — so the client forces `Complete=false` when an earlier attempt got
as far as `meta` (i.e. the selector already ran) and then failed. And a **failed** call
still bills: `task_output_invalid` is raised only after a billed completion, often after
a second billed repair, so a failed task reports unknown spend unless the failure is one
where no generation can have run (a refused socket, a 400, an unfunded or unroutable
request).

One gap remains on the backend side and is tracked as
[assistant-backend#31](https://github.com/daintreehq/assistant-backend/issues/31): a
re-rolled generation where only one attempt reported its cost yields a partial `total`
still flagged `complete: true`, and `/tasks` has no completeness field at all. Small in
magnitude, but it means `complete` is a floor on how conservative to be, not a guarantee.

`backend.ClientConfig.OnCost` is the single seam — it fires for every respond turn AND
every utility task, because the client is the only layer they both pass through. A day of
orchestration spends real money on summarize/extract/classify tasks fired from tools and
watchers that never appear as a turn. `internal/costledger` accumulates; `/cost` and
`/doctor` render, hedging an incomplete total as `≥ $x` (truncated, never rounded up) and
naming why. The ledger is deliberately unpersisted and counts from process launch or the
last `/clear`; it outlives a `/login` client swap, so changing endpoint mid-session does
not silently zero the bill.

Not counted anywhere: **skill learning**, which runs fire-and-forget on a stronger model
after the response and can cost more than the turn that triggered it. It is off by
default and forbidden in production, so no beta tester is exposed — but a local developer
who enables it will see dashboard spend that no field here accounts for.

### The upstream-failure taxonomy

The backend used to collapse every upstream 401/402/403/404 and every 5xx into one
`502 upstream_error`. It now names each condition, because they have different fixes and
only three of them are transient. **`retryable` is server-side and is NOT serialised** —
the CLI classifies from the code itself, so this table is the contract:

| code | status | retried? | whose problem |
|---|---|---|---|
| `provider_invalid_api_key` | 401 | no | your key — replace or rotate it, then `/login` |
| `provider_insufficient_credits` | 402 | no | your balance — add credit |
| `provider_key_forbidden` | 403 | no | your key's model permissions / spend limit / guardrails (signing in again cannot help) |
| `upstream_no_compliant_provider` | 503 | no | your routing policy matched no endpoint |
| `upstream_rate_limited` | 429 | **yes** | transient (honours `Retry-After`) |
| `upstream_timeout` | 504 | **yes** | transient |
| `upstream_unavailable` | 503 | **yes** | transient provider outage |
| `upstream_request_rejected` | 502 | no | **ours** — report it with the request id |
| `upstream_protocol_error` | 502 | no | **ours** — report it with the request id |
| `upstream_error` | 502 | stream only | the pre-split catch-all — still emitted for a stream error the backend could not classify, and by the key-verification path when the provider could not be reached; a genuine "we don't know", which is why the stream form is worth one more attempt while the pre-stream form (an application verdict) is not |

Two rules make this work, and both are easy to get wrong:

1. **Classify on the code, never the status.** The backend emits `meta` before it opens
   the upstream stream, so most of this taxonomy arrives as a terminal SSE `error` event
   with `HTTPStatus == 0`. A status-based rule reverses its answer depending only on how
   far the request got — and a routing dead end (a 503) reads as a transient gateway,
   burning the entire retry budget to re-derive the same empty endpoint pool.
2. **Deterministic ≠ our verdict.** `nonRetriableAppCodes` is pre-stream only (it means
   "the work ran, this is its answer"); `deterministicUpstreamCodes` applies on **both**
   transports (it means "this will hold identically on every replay").

`internal/agent/session.go`'s `upstreamFailureAdvice` renders a distinct reply for each
of these codes except `upstream_rate_limited` (caught earlier by `IsRateLimited()`, which
also raises the health badge) and `upstream_timeout` (which falls through to the generic
`Model error:` with its detail intact). Every reply starts with a registered
wake-failure prefix, so an unattended supervisor wake cannot mistake a failed turn for an
answer. `Error.RequestID` carries the backend's `X-Request-Id` — validated against a
conservative id alphabet and scrubbed like any other field we did not author, since it is
rendered into terminal scrollback — stamped from the HTTP error response and from the
streamed 200's headers for a mid-stream failure, so the two reportable codes produce a
report someone can act on.

The two reportable codes are grouped by their next step, NOT by their culprit, and the
copy says so: `upstream_request_rejected` means the provider judged the request Daintree
built to be malformed (our bug); `upstream_protocol_error` means the provider answered
with something unparseable (usually a provider or compatibility problem).
- `tasks.go` — typed helpers for the server-owned utility tasks.

## Invariants the CLI upholds

- **No `system` / `developer` messages.** Only `user` / `assistant` / `tool` reach the
  backend (the converter `internal/agent/backendconv.go` rejects anything else up front).
  `domain.ControlMessageCount == 0`: no synthetic row is persisted in visible history.
  `input.messages` contains visible history only; startup data never masquerades as a
  user-authored message.
- **Context uses dedicated structured channels.** Required `request.startup` carries
  the cacheable curated project identity, the effective agent catalog (including
  availability and toolbar state), and normalized bounded `DAINTREE.md` instructions.
  The catalog has a 16 KiB encoded-row budget; any row that does not fit is omitted whole
  while `total_count` retains the discovered total, rather than emitting a truncated,
  unusable agent id. `request.runtime` carries tier, MCP, scheduler, a freshly read typed
  worktree snapshot, and open terminals. Stable project
  and agent fields are not duplicated in this fresh tail. `request.turn` carries the goal,
  wake, workflow runs, async operations, memories, and session-ended watchers.
- **The integration surface names its endpoints.** `runtime.mcp_servers` lists every MCP
  server this process is wired to — the primary Daintree control plane and the public docs
  MCP — each as `name` + a `description` leading with its endpoint URL, so the model can
  say WHICH Daintree it is driving instead of guessing (ses_8cb40b4e). The backend renders
  it as a session-**stable** system block ahead of the tool schemas, so the list carries
  endpoints ONLY: connected/transport/tool-count fluctuate mid-session and stay on
  `runtime.mcp`, which rides the volatile tail. Endpoints are sanitized
  (`mcp.SanitizeURL`: no userinfo/query/fragment) because Daintree's per-session URL can
  carry its bearer as a query parameter. The same endpoints are reachable on demand
  through `daintree.status` (MCP) and `context.snapshot` (MCP + the assistant backend URL).
- **Splash-time discovery is bounded and parallel.** A successful MCP connect starts
  `project.getCurrent`, canonical `agent.listAvailable`, `worktree.getCurrent`, and the
  open-terminal warm-up while the logo is animating. The agent action returns the current
  effective built-in/user/plugin launch registry with display names, source, coarse CLI
  availability, and built-in tri-state pin/resolved toolbar visibility. A fast first submit
  joins the whole connect+prefetch gate rather than racing an empty snapshot; the post-logo
  bootstrap awaits that same single attempt instead of reconnecting. The primary lifecycle
  has an 8-second cancellation budget. A completed degraded attempt fails open for later
  turns (manual `/reconnect` owns retries), while an externally cancelled launch remains
  retryable once by bootstrap/the first turn.
- **The cache boundary is intentional.** The backend places the structured stable startup
  block immediately before the append-only conversation and keeps its fresh
  runtime/turn user message at the end. Worktree is re-read on every backend round, so a
  switch only changes the tail. A project or pin change changes the startup block and
  following conversation, but leaves the backend's system prompts and large tool schemas
  cached. Raw `project.getSettings` is never injected because its open-ended values may
  include environment or other sensitive configuration.
- **Worktree read state is explicit.** An omitted `runtime.worktree` means the live read was
  unavailable, `{current:null}` means Daintree definitively reports no current worktree,
  and a current object carries id/path/branch/issue/PR/status/last-commit fields.
- **Skills are server-owned.** No `skill.find` / `skill.load` (reserved + rejected). The
  backend's selector picks and injects runbooks and returns a `skills` block. The CLI
  immediately surfaces its `newly_loaded` refs as skill cards and keeps only the local
  run-tracking tools `skill.run.get` / `skill.step.advance` (the backend prompt drives them).
- **Opaque state token.** `meta.state` is stored verbatim and replayed on the next request;
  the CLI never inspects, signs, or mutates it. A missing token is valid for a new session.
- **One `turn_id` per user request** across the whole tool-call loop; `round` increments
  per continuation; `instruction_revision` bumps when a mid-turn injection is folded in.
- **Utility work is server-owned tasks** (`/v1/daintree/tasks`): `checkpoint`,
  `memory_distill`, `watcher_classify`, `terminal_judge`, `terminal_summarize`,
  `terminal_extract_text`, `terminal_extract_json`, `extraction_verdict`,
  `skill_step_consistency`, plus the flag-gated `workflow_plan`,
  `workflow_reconcile`, `workflow_resume_digest`. The CLI sends task *data* only —
  never prompts.

### Task ids are a frozen wire contract

The ids above are hardcoded on BOTH sides and must change in lockstep:

| Side | Manifest | Guard |
| --- | --- | --- |
| CLI | `internal/backend/tasks.go` (`coreTaskIDs`) + `workflowtasks.go` (`workflowTaskIDs`) | `taskcheck_test.go` parses the AST and fails if a `Task*` constant is not in the manifest |
| Backend | `task_runner` profiles | `EXPECTED_TASK_IDS` in `tests/unit/test_task_runner.py`, asserted as an exact SET |

`backend.CheckTasks(caps, workflowIntelligence)` diffs the CLI's manifest against the
live `Capabilities.Tasks`. It surfaces in three places: the `backend tasks` row in
`/doctor`, the `doctor` subcommand's banner (**and its exit code** — drift is a gating
failure), and a `backend.tasks` debug-log line at boot.

Three rules the checker encodes:

- **Extra server tasks are fine** — forward compatibility, not drift.
- **An unreported inventory is "cannot verify", never a failure.**
  `/v1/daintree/capabilities` sits behind `require_auth` *and* `require_ready`, so a
  warming backend legitimately advertises nothing.
- **Workflow ids are required only when `DAINTREE_WORKFLOW_INTELLIGENCE=1`.**

> Why the machinery: on **2026-07-07** the backend dropped a `.v1` suffix from every
> task id. The count was unchanged, and *both* sides asserted only a count — so every
> test stayed green while every task call 404'd mid-turn. Never assert `len(tasks)`.

## When changing protocol behavior

Inspect both sides:
`../assistant-backend/docs/DAINTREE_API.md`,
`../assistant-backend/src/daintree_assistant_server/contracts/daintree_api.py`,
`../assistant-backend/src/daintree_assistant_server/services/validation.py`.
