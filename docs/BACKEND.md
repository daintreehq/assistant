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

## Endpoint (development)

**Hardcoded** to `http://127.0.0.1:8473`, **unauthenticated**. The assistant supports
exactly this one endpoint for now; a later phase swaps in the production URL and a real
login flow. The only override is the dev/test env var `DAINTREE_BACKEND_URL` (used by
e2e tests to point at a fake backend). There is no product config knob. The constant
lives at `backend.DefaultBaseURL`.

Run the backend from its sibling repo during local development:

```bash
cd ../assistant-backend
python -m daintree_assistant_server   # serves on 127.0.0.1:8473 (its .env pins the port)
```

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
  is always first (carries the refreshed `state` token + the first-class `skills` block).
  Tool-call deltas accumulate by index (OpenAI-style fragments). EOF before `done` is an
  error — the parser never fabricates a successful finish.
- `client.go` — `RespondStream`, `RunTask`, `Capabilities`, `Health`, `Ready`, `Version`.
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
  joins the whole connect+prefetch gate rather than racing an empty snapshot; duplicate boot
  connects reuse it. The primary lifecycle has an 8-second cancellation budget. A completed
  degraded attempt fails open for later turns (manual `/reconnect` owns retries), while a
  splash attempt canceled by handoff remains retryable once by bootstrap/the first turn.
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
  backend's selector picks and injects runbooks and returns a `skills` block (active set
  + a synthetic-load `prelude` the CLI surfaces). The CLI keeps only the local
  run-tracking tools `skill.run.get` / `skill.step.advance` (the backend prompt drives them).
- **Opaque state token.** `meta.state` is stored verbatim and replayed on the next request;
  the CLI never inspects, signs, or mutates it. A missing token is valid for a new session.
- **One `turn_id` per user request** across the whole tool-call loop; `round` increments
  per continuation; `instruction_revision` bumps when a mid-turn injection is folded in.
- **Utility work is server-owned tasks** (`/v1/daintree/tasks`): `checkpoint`,
  `memory_distill`, `watcher_classify`, `terminal_judge`, `terminal_summarize`,
  `terminal_extract_text`, `terminal_extract_json`, `extraction_verdict`,
  `skill_step_consistency`. The CLI sends task *data* only — never prompts.

## When changing protocol behavior

Inspect both sides:
`../assistant-backend/docs/DAINTREE_API.md`,
`../assistant-backend/src/daintree_assistant_server/contracts/daintree_api.py`,
`../assistant-backend/src/daintree_assistant_server/services/validation.py`.
