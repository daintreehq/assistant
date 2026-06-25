# DeepSeek AI — integration notes

The Daintree Assistant talks to **DeepSeek AI**, which is **wire-compatible with the
OpenAI Chat Completions API**. `internal/models/deepseek.go` (`DeepSeekClient`) speaks
it directly over the standard library `net/http` — no SDK — pointed at the DeepSeek base
URL.

## Connection

| Setting   | Value                                                   |
| --------- | ------------------------------------------------------- |
| Base URL  | `https://api.deepseek.com` (override `DEEPSEEK_BASE_URL`) |
| Auth      | `Authorization: Bearer <DEEPSEEK_API_KEY>`             |
| API key   | `DEEPSEEK_API_KEY` env var (read from the project `.env`) |

**Config resolution (security boundary):** `DEEPSEEK_API_KEY` is *merged* — real env >
project `.env` > the assistant's own `.env`. `DEEPSEEK_BASE_URL` is **trusted-or-own** —
real env or the assistant's OWN `.env` only, NEVER a project `.env`. A bound (untrusted)
repo must not be able to redirect where a trusted key is sent. See `internal/config`.

`DeepSeekClient` builds an `*http.Request` to `<baseURL>/chat/completions` with the
bearer header and a JSON body, and streams the SSE response. `models.Router`
(`internal/models/router.go`) maps a `domain.ModelTier` to a model id (`ModelFor`) and
exposes `Chat`, `Stream(onToken)`, and `JSON`.

## Models

| Tier   | Model id            | Use                                         |
| ------ | ------------------- | ------------------------------------------- |
| large  | `deepseek-v4-flash` | Main thread: reasoning, orchestration       |
| medium | `deepseek-v4-flash` | Routes to `large` in v1                     |
| small  | `deepseek-v4-flash` | Watchers, summaries, classification, timers |

All tiers default to `deepseek-v4-flash` — it is the validated orchestration model, and
the loaded skills supply the playbooks that make it sufficient on the main thread.
Override any tier with `DAINTREE_{LARGE,MEDIUM,SMALL}_MODEL`. (There is a `medium` tier
in the abstraction; for v1 it routes to `large`.)

## Chat completions

Standard OpenAI request body: `{ model, messages, tools, tool_choice, stream }`.

### Tool / function calling

- `tools`: array of `{ type: "function", function: { name, description, parameters } }`
  where `parameters` is a JSON Schema with `additionalProperties: false`.
- `tool_choice`: `"auto" | "none" | { type: "function", function: { name } }`.
- Response: `choices[0].message.tool_calls` is an array of
  `{ id, type: "function", function: { name, arguments } }` where `arguments` is a **JSON string**.
- `finish_reason` is `"tool_calls"` when the model wants to call tools.
- **Gotcha:** when replaying assistant tool-call messages back to the API, send only the
  standard fields (`id`, `type`, `function`). Do NOT add custom properties — DeepSeek returns
  `400 invalid_request_error` if it sees unknown fields like `call_id`.
- A `tool` result message has shape `{ role: "tool", tool_call_id, content }`.

### Streaming (SSE)

- Set `stream: true`. Each chunk: `choices[0].delta` carries `{ content?, tool_calls? }`.
- Tool-call deltas arrive in fragments keyed by `index`; accumulate `function.arguments`
  string fragments per index.
- Stream ends with `data: [DONE]`.
- **Reasoning models** may emit `<think>...</think>` blocks directly inside `delta.content`.
  The think-filter in `internal/models` strips/segregates `<think>` content from
  user-facing output (correctly across chunk boundaries — see `models` tests).

### JSON / structured output

For small-model classification we use `response_format: { type: "json_object" }`
(`Router.JSON`) and unmarshal + validate the result into the target Go struct. Always
include the word "JSON" in the prompt when using json_object mode.

### Thinking control (important)

`deepseek-v4-flash` is a reasoning model: by default it runs a `<think>` phase
(returned as `message.reasoning_content`). The assistant wants most calls **think-free**
(the small tier always, and flash on the main thread too — the loaded skills carry the
playbooks, so a provider-side `<think>` is pure latency/cost). DeepSeek splits the two
controls, and this is the migration gotcha:

- **Off switch — `thinking`:** `{"type": "disabled"}` skips the think phase entirely;
  `{"type": "enabled"}` forces it. This is how we express think-free.
- **Depth — `reasoning_effort`:** `high | low | medium | max | xhigh`. There is **no
  `"none"` variant** — sending `reasoning_effort: "none"` returns `400
  invalid_request_error`. (This is the one place DeepSeek diverges from the Fireworks
  build, which used `reasoning_effort: "none"` to disable thinking.)

`internal/models/deepseek.go` translates the Router's abstract `"none"` intent into
`thinking:{type:"disabled"}` (omitting `reasoning_effort`); any explicit effort passes
through as `reasoning_effort`. See `ChatOptions.ReasoningEffort` and `buildBody`.

### Useful DeepSeek-specific params (optional)

- `prompt_cache_key` (string) — accepted; DeepSeek also caches context prefixes
  automatically (usage reports `prompt_cache_hit_tokens` / `prompt_cache_miss_tokens`).
- `max_tokens`, `temperature`, `top_p`, `min_p`.
