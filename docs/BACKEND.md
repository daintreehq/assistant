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
User → Daintree CLI ──(visible conversation + structured context + tool inventory)──► Daintree backend
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
  `session{id,turn_id,instruction_revision,round}`, `state`, `input{messages,tools,tool_choice}`,
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
  The CLI holds **no client-side control prefix** — `domain.ControlMessageCount == 0`,
  and a fresh conversation starts at index 0.
- **Runtime + turn context are structured data**, not prose. The old system/footer
  messages became `request.runtime` (tier, project, MCP, agents, scheduler, worktree,
  project instructions) and `request.turn` (goal, wake, workflow runs, async operations,
  memories, session-ended watchers). The backend renders them.
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
