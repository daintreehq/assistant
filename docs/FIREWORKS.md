# Fireworks AI — integration notes

The Daintree Assistant CLI talks to **Fireworks AI**, which is **wire-compatible with the
OpenAI Chat Completions API**. We use the official `openai` npm SDK pointed at the Fireworks
base URL.

## Connection

| Setting   | Value                                                   |
| --------- | ------------------------------------------------------- |
| Base URL  | `https://api.fireworks.ai/inference/v1`                 |
| Auth      | `Authorization: Bearer <FIREWORKS_API_KEY>`             |
| API key   | `FIREWORKS_API_KEY` env var (read from `.env` in repo)  |

```ts
import OpenAI from "openai";
const client = new OpenAI({
  baseURL: "https://api.fireworks.ai/inference/v1",
  apiKey: process.env.FIREWORKS_API_KEY,
});
```

## Models

| Tier  | Model id                                          | Use                                         |
| ----- | ------------------------------------------------- | ------------------------------------------- |
| large | `accounts/fireworks/models/minimax-m3`            | Main thread: reasoning, orchestration       |
| small | `accounts/fireworks/models/deepseek-v4-flash`     | Watchers, summaries, classification, timers |

(There is a `medium` tier in the abstraction; for v1 it routes to `large`.)

## Chat completions

Standard OpenAI shape: `client.chat.completions.create({ model, messages, tools, tool_choice, stream })`.

### Tool / function calling

- `tools`: array of `{ type: "function", function: { name, description, parameters } }`
  where `parameters` is a JSON Schema with `additionalProperties: false`.
- `tool_choice`: `"auto" | "none" | { type: "function", function: { name } }`.
- Response: `choices[0].message.tool_calls` is an array of
  `{ id, type: "function", function: { name, arguments } }` where `arguments` is a **JSON string**.
- `finish_reason` is `"tool_calls"` when the model wants to call tools.
- **Gotcha:** when replaying assistant tool-call messages back to the API, send only the
  standard fields (`id`, `type`, `function`). Do NOT add custom properties — Fireworks returns
  `400 invalid_request_error` if it sees unknown fields like `call_id`.
- A `tool` result message has shape `{ role: "tool", tool_call_id, content }`.

### Streaming (SSE)

- Set `stream: true`. Each chunk: `choices[0].delta` carries `{ content?, tool_calls? }`.
- Tool-call deltas arrive in fragments keyed by `index`; accumulate `function.arguments`
  string fragments per index.
- Stream ends with `data: [DONE]`.
- **Reasoning models** may emit `<think>...</think>` blocks directly inside `delta.content`.
  The CLI strips/segregates `<think>` content from user-facing output.

### JSON / structured output

For small-model classification we use `response_format: { type: "json_object" }` and validate
with Zod. Always include the word "JSON" in the prompt when using json_object mode.

### Useful Fireworks-specific params (optional)

- `prompt_cache_key` (string) — cache a static system-prompt prefix to cut latency/cost.
- `max_tokens`, `temperature`, `top_p`, `min_p`.
