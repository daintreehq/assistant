# Daintree Assistant backend — CLI integration

The CLI is a **thin local runtime**. It no longer acts as a model client; instead it
talks to the **Daintree Assistant backend** (a Daintree-native HTTP API — *not*
OpenAI-compatible). The backend owns the system prompt, developer instructions, skill /
runbook selection, model choice, prompt assembly, the utility-model prompts, prompt
caching, and the upstream model credentials (DeepSeek). The CLI owns the terminal UI,
the visible conversation, the local tool registry + execution, permissions, runtime /
project context collection, memory + scheduler state, stream rendering, and the opaque
backend state token.

```
User → Daintree CLI ──(structured startup + visible conversation + runtime/turn + tools)──► Daintree backend
        │  stores conversation, exposes & runs local tools,                            owns prompts, skills,
        │  streams assistant text, persists backend state                              model routing, DeepSeek
        ◄──(named-event SSE: meta / delta / done / error)────────────────────────────┘
```

## Endpoint & authentication

The default endpoint is **production**: `backend.DefaultBaseURL` =
`https://assistant.daintree.org`. Requests carry `Authorization: Bearer <key>`
(`Client.setHeaders` in `internal/backend/client.go`) — but only when a key is configured;
an empty key sends **no** `Authorization` header at all, which is what makes the
unauthenticated local backend work unchanged.

**Login.** On the first interactive launch with no persisted login and no
`DAINTREE_BACKEND_URL`, the CLI runs a two-question flow (`internal/cli/login.go`):
endpoint (the default, or a custom URL — validated by `config.NormalizeEndpoint`: absolute
http/https, a host, no userinfo/query/fragment, trailing slashes stripped) then the API key
(opaque to the CLI, only obvious paste accidents are rejected). Both are persisted to
`~/.daintree/credentials.json` — written atomically, **file 0600 inside a 0700 dir**, and
deliberately **global** rather than per-project (unlike the state dir, login does not repeat
per project). The endpoint is stored even when it is the default, so changing the code
default can never silently retarget an existing key. `/login` re-runs the same flow at any
time: the surface returns `domain.ErrLoginRequested`, the launcher runs the prompt once the
terminal is free, then rebuilds the App so the fresh credentials take effect.

**Resolution + the pairing rule** (`config.LoadConfig`): explicit override → `DAINTREE_BACKEND_URL`
(**trusted env only** — never a value loaded from a project `.env`, since a bound project must
not be able to redirect where the key is sent) → the persisted login endpoint →
`backend.DefaultBaseURL`. The persisted key is attached **only** when the resolved URL equals
the persisted endpoint (`sameEndpoint`, fail-closed: an endpoint that does not normalize never
matches). So pointing the CLI at a local or fake backend via the env var never hands it the
real key. A credential file that exists but is corrupt/invalid is a hard error *when it would
have chosen the endpoint* — with an override set it is moot and can't brick a dev/e2e run.

**Auth failures.** A 401/403 (`Error.IsAuth`) is a rejected or missing key, not an outage. It
is non-retriable, so it surfaces on the first attempt rather than after the transient poll:
the turn fails with *"Backend authentication failed — the API key was rejected or missing.
Run /login to set the backend endpoint and API key."* `doctor` reports the same fix rather
than "backend down".

**Local development** now needs the override explicitly — `127.0.0.1:8473` is no longer the
default:

```bash
cd ../assistant-backend
python -m daintree_assistant_server   # serves on 127.0.0.1:8473 (its .env pins the port)

# in the assistant shell — also suppresses the first-run login gate
export DAINTREE_BACKEND_URL=http://127.0.0.1:8473
```

(Or log in with option 2 / a pasted URL and point the persisted endpoint at the local
backend; e2e tests use the env var to reach a fake backend.)

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
