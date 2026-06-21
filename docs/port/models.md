# Port spec — `models/` subsystem (ModelRouter + FireworksClient + prompts + pricing)

Authoritative port reference for the model layer. An implementation agent should be
able to port this faithfully into Go **without re-reading the TypeScript**. Source of
truth (TS): `src/models/router.ts`, `src/models/fireworks.ts`, `src/models/pricing.ts`,
`src/models/prompts/{index,base,runtimeContext,skills,daintreeMcp}.ts`, plus the
reliability primitives in `src/reliability.ts` and the relevant config/schema
constants in `src/config.ts` / `src/schemas.ts`.

> NOTE on model ids: the legacy `docs/FIREWORKS.md` and several code comments still say
> the large/medium model is `minimax-m3`. The **actual runtime default** (in
> `src/config.ts` `DEFAULTS`) is **`glm-5p2`** for both large and medium. The comments
> are stale; **port the config defaults verbatim** (see §1) and treat "minimax-m3" in
> prose as historical. Pricing knows both (§4).

---

## 0. What this subsystem does

Maps a 3-tier abstraction (`small` / `medium` / `large`) onto concrete Fireworks model
ids, and wraps the Fireworks **OpenAI-compatible** HTTP API. It provides three call
shapes — non-streaming `chat`, streaming `chatStream` (token callback), and strict
`json` (validated `json_object`) — with bounded retry, per-attempt timeouts, abort/cancel
handling, a `<think>…</think>` reasoning filter, usage+cached-token accounting, and a
cost estimator. It also owns the **layered main-thread system prompt** (a byte-stable
cached base prefix + two dynamic control layers) and all the small-model sub-agent
prompts (watcher / judge / summarizer / extractor).

---

## 1. Model tiers, ids, defaults, env precedence

### 1.1 `ModelTier` enum (from `schemas.ts`)
Value set (exact, order matters for nothing but keep it): `"small" | "medium" | "large"`.

### 1.2 Tier → model id mapping (`ModelRouter.modelFor`)
| Tier   | Source field          | Default (`config.DEFAULTS`)                          |
| ------ | --------------------- | ---------------------------------------------------- |
| small  | `cfg.smallModel`      | `accounts/fireworks/models/deepseek-v4-flash`        |
| medium | `cfg.mediumModel`     | `accounts/fireworks/models/glm-5p2`                  |
| large  | `cfg.largeModel`      | `accounts/fireworks/models/glm-5p2`                  |

`modelFor` is a plain switch; `default` falls through to `large`. **Medium routes to the
same model id as large today** (both default `glm-5p2`) but the tier is a distinct stable
contract — keep them as separate fields.

### 1.3 Other connection defaults
| Constant            | Value                                            |
| ------------------- | ------------------------------------------------ |
| `fireworksBaseUrl`  | `https://api.fireworks.ai/inference/v1`          |
| `defaultMcpUrl`     | `http://127.0.0.1:45454/mcp` (not used by models)|
| API key fallback    | when key empty, SDK is constructed with literal `"missing-key"` (then `guard()` rejects before any call) |

### 1.4 Env-var resolution (precedence: explicit override → env → project `.env` → assistant own `.env` → default)
Model-relevant vars (read in `loadConfig`):
| Field            | Override key   | Env var                | Default                    |
| ---------------- | -------------- | ---------------------- | -------------------------- |
| `fireworksApiKey`| `fireworksApiKey` | `FIREWORKS_API_KEY` | `""`                       |
| `fireworksBaseUrl`| — (env only)  | `FIREWORKS_BASE_URL`   | `DEFAULTS.fireworksBaseUrl`|
| `largeModel`     | `largeModel`   | `DAINTREE_LARGE_MODEL` | `DEFAULTS.largeModel`      |
| `mediumModel`    | — (env only)   | `DAINTREE_MEDIUM_MODEL`| `DEFAULTS.mediumModel`     |
| `smallModel`     | `smallModel`   | `DAINTREE_SMALL_MODEL` | `DEFAULTS.smallModel`      |
| `offline`        | `offline`      | `DAINTREE_ASSISTANT_OFFLINE` == `"1"` (TRUSTED env only) | `false` |

`firstString(...)` semantics: returns the first arg that is non-empty after `.trim()`;
**trims the returned value**. Port that trimming behavior.

`offline` is read from the **trusted env snapshot** (taken before any `.env` load) — a
bound project's `.env` must never be able to flip offline/tier/auto-approve. Keep this
separation in the Go config loader (it lives in the config port-spec, noted here because
`offline` gates every model call via `guard()`).

---

## 2. FireworksClient — wire contract

The TS wraps the official `openai` Node SDK pointed at the Fireworks base URL, with
`maxRetries: 0` (we own all retry logic). **Go has no official Fireworks SDK** — port
this as a hand-written HTTP/SSE client speaking the OpenAI Chat Completions wire format.
The exact request/response JSON below is the contract.

### 2.1 Client construction
- `baseURL = cfg.fireworksBaseUrl`
- `apiKey = cfg.fireworksApiKey || "missing-key"`
- Auth header: `Authorization: Bearer <apiKey>`
- **No SDK-level retries** (`maxRetries: 0`). All retry/backoff is explicit (§3).

### 2.2 `guard()` (runs at top of `chat`, `chatStream`, `json`)
1. if `cfg.offline` → throw `FireworksUnavailableError("offline mode")`.
2. if `!cfg.fireworksApiKey` → throw `FireworksUnavailableError("FIREWORKS_API_KEY not set")`.

### 2.3 Request payload — `chat` (non-stream)
POST `<baseURL>/chat/completions`, body:
```jsonc
{
  "model":       opts.model,
  "messages":    toWireMessages(opts.messages),   // §2.7
  "tools":       opts.tools,                       // omit/undefined when none
  "tool_choice": opts.toolChoice,                  // "auto" | "none" | "required" | undefined
  "temperature": opts.temperature ?? 0.3,          // DEFAULT 0.3
  "max_tokens":  opts.maxTokens,                    // undefined => omit
  "prompt_cache_key": opts.promptCacheKey          // ONLY present when set
}
```
Notes: `tools`/`tool_choice`/`max_tokens` are passed through as-is (the SDK drops
`undefined`). In Go, **omit a key entirely when the TS value is undefined** (use
`omitempty`/pointer fields) — do not send `null`. `prompt_cache_key` is spread in only
when `opts.promptCacheKey` is truthy.

### 2.4 Request payload — `chatStream`
Same as `chat`, **plus**:
```jsonc
"stream": true,
"stream_options": { "include_usage": true }   // forces a final usage-only chunk
```
`stream_options.include_usage` is required — without it the OpenAI-compatible API reports
**no usage on streamed calls**.

### 2.5 Request payload — `json`
```jsonc
{
  "model":          opts.model,
  "messages":       toWireMessages(opts.messages),
  "temperature":    opts.temperature ?? 0,         // DEFAULT 0 (note: NOT 0.3)
  "max_tokens":     opts.maxTokens,
  "response_format": { "type": "json_object" }
}
```
**No `tools`/`tool_choice`** on the json path (the TS type strips them). The model
**must** see the word "JSON" in the prompt for `json_object` mode (Fireworks/OpenAI
requirement) — all our json prompts do. `prompt_cache_key` is NOT sent on the json path
(the TS omits it here).

### 2.6 Response parsing
Non-stream (`chat`): take `resp.choices[0]`.
- `content`: run `choice.message.content ?? ""` through a fresh `ThinkFilter` (push then
  end), then `.trim()` → `result.content`. Reasoning → `filter.reasoning.trim()`.
- `toolCalls`: `normalizeToolCalls(choice.message.tool_calls)` (§2.8).
- `finishReason`: `choice.finish_reason ?? "stop"`.
- `usage` (when `resp.usage` present): map `prompt_tokens`→`promptTokens`,
  `completion_tokens`→`completionTokens`, `total_tokens`→`totalTokens`, and
  `prompt_tokens_details.cached_tokens`→`cachedTokens` (nested, optional).

`json`: `raw = resp.choices[0].message.content ?? "{}"`; `cleaned = stripThink(raw)`;
`JSON.parse(extractJson(cleaned))`; then validate against the Zod schema. See §6/§7.

### 2.7 `toWireMessages(messages)` — message reduction (CRITICAL wire contract)
Maps internal `ChatMessage[]` to exactly the fields Fireworks accepts. Per role:
- **`tool`**: `{ role:"tool", content: contentToText(m.content), tool_call_id: m.tool_call_id }`.
  Tool results are always flattened to text (defensive `contentToText`). The internal
  `name` helper field is **dropped** — extra fields cause a 400 on replay.
- **`assistant`**: `{ role:"assistant", content: m.content }` and, **only if
  `m.tool_calls?.length`**, add `tool_calls: m.tool_calls.map(t => ({ id, type:"function",
  function:{ name, arguments } }))`. Emit **only** `{id,type,function}` — any custom field
  (e.g. `call_id`, `name` at the call level) triggers Fireworks `400 invalid_request_error`.
- **`user` / `system`**: `{ role: m.role, content: m.content ?? "" }`. A multimodal
  `ChatContentPart[]` is **forwarded verbatim** (array preserved, NEVER collapsed to a
  string); `null` content coalesces to `""`.

### 2.8 `normalizeToolCalls(calls)`
- `undefined` → `[]`.
- Filter to calls with `c.function?.name` truthy.
- Map each to `{ id: c.id ?? "call_" + Math.abs(hashString(name)), type:"function",
  function:{ name, arguments: c.function.arguments ?? "{}" } }`.
- Synthesized id uses `hashString` (§9) when `c.id` is missing.

### 2.9 Streaming accumulation algorithm (`chatStream`)
Outer retry loop (`for attempt = 0; ; attempt++`), with `emitted` flag tracked **across**
attempts (declared outside the loop). Per attempt, fresh accumulators:
- `filter = new ThinkFilter()`
- `toolAcc: Map<number, {id,name,args}>` keyed by tool-call index
- `finishReason = "stop"`, `usage` undefined

For each SSE chunk:
1. If `chunk.usage` present, capture it (overwrites; usage may ride the final content
   chunk on some providers — capture whenever present). Same field mapping as §2.6
   including nested `prompt_tokens_details.cached_tokens`.
2. `choice = chunk.choices[0]`; if falsy (`undefined`/empty array, e.g. the usage-only
   chunk), `continue`.
3. `delta = choice.delta`:
   - `delta.content` → `visible = filter.push(content)`; if `visible` non-empty: set
     `emitted = true` and call `onToken(visible)`.
   - `delta.tool_calls` → for each `tc`: `idx = tc.index ?? 0`; upsert into `toolAcc`:
     if `tc.id` set `id`; if `tc.function?.name` set `name`; if `tc.function?.arguments`
     **append** to `args`.
   - `choice.finish_reason` → assign to `finishReason`.
4. After the stream ends: `tail = filter.end()`; if non-empty set `emitted = true` and
   `onToken(tail)`.
5. Build `toolCalls`: take `toolAcc` entries **sorted by index ascending**, map to
   `{ id: v.id || "call_" + Math.abs(hashString(v.name + v.args)), type:"function",
   function:{ name: v.name, arguments: v.args || "{}" } }`, then **filter out any with
   empty `function.name`**.
6. Return `{ content: filter.visible.trim(), reasoning: filter.reasoning.trim(),
   toolCalls, finishReason, usage }`.

SSE transport: Fireworks emits `data: <json>` lines terminated by `data: [DONE]`. The
Go client must parse the SSE stream itself (split on `\n\n`, strip `data: ` prefix, stop
on `[DONE]`).

### 2.10 Cancel/abort handling
The TS uses an `AbortSignal` (the UI's Escape-to-cancel). In Go this is `context.Context`.
- A user abort must surface as `CancelledError` (`code: "CANCELLED"`, default message
  `"Turn cancelled"`), **never** the raw transport error.
- `isAbortError(err)`: true when the SDK's `APIUserAbortError` OR an `Error` named
  `"AbortError"`. In Go: detect `ctx.Err() == context.Canceled` / a wrapped abort.
- `chat`/`json`: on caught error, if `isAbortError` → throw `CancelledError`, else rethrow.
- `chatStream`: on caught error: if `isAbortError` → `CancelledError`; **also** if
  `opts.signal?.aborted` (a cancel that raced a transient error) → `CancelledError`.
- A cancel landing mid-backoff (`abortableSleep` rejects with AbortError) is normalized to
  `CancelledError`.

---

## 3. Retry / rate-limit / timeout (`reliability.ts`)

### 3.1 Constants (exact)
| Constant                     | Value      | Meaning                                            |
| ---------------------------- | ---------- | -------------------------------------------------- |
| `MODEL_RETRY_POLICY.maxRetries` | `3`     | ADDITIONAL attempts after the first (⇒ 4 total)    |
| `MODEL_RETRY_POLICY.baseDelayMs`| `500`   | backoff base                                       |
| `MODEL_RETRY_POLICY.maxDelayMs` | `10_000`| backoff ceiling (10s)                              |
| `MODEL_REQUEST_TIMEOUT_MS`   | `60_000`   | per-attempt timeout for `chat`/`json` (60s)        |
| `MODEL_STREAM_TIMEOUT_MS`    | `300_000`  | per-attempt timeout for `chatStream` (300s/5min)   |
| `MAX_RETRY_AFTER_MS`         | `30_000`   | cap on an honoured `Retry-After` (private)         |
| `MCP_READ_RETRY_POLICY`      | `{2, 250, 2000}` | (MCP only — not models, but same module)     |
| `MCP_READ_TIMEOUT_MS`        | `20_000`   | (MCP only)                                          |
| `RATE_LIMIT_TAIL_WINDOW`     | `1500`     | (watcher rate-limit scan; not the model client)    |

### 3.2 `fullJitterDelay(attempt, baseMs, maxMs)`
`ceiling = min(maxMs, baseMs * 2^max(0,attempt))`; return a **uniform random integer in
`[0, ceiling]`** (`floor(random()*(ceiling+1))`). FULL jitter. `attempt` is 0-based.

### 3.3 `isRetriableModelError(err)`
- user abort → `false`.
- `APIError`: `status === undefined` (connection error / timeout) → `true`;
  else `status === 429 || status >= 500` → `true`; any other 4xx → `false`.
- anything else → `false`.
In Go: classify by HTTP status code (429 or ≥500 retriable), and treat transport/dial/
timeout errors (no status) as retriable.

### 3.4 `isRateLimitModelError(err)` → `APIError && status === 429`.

### 3.5 `parseRetryAfterMs(headers)`
Tolerates a `Headers`-like (`.get`) or a plain record. Prefer **`retry-after-ms`**
(non-standard) parsed as a number (finite, ≥0). Else `retry-after`: numeric seconds →
`*1000`; or an HTTP-date → `max(0, Date.parse - now)`. Returns `undefined` if nothing
parseable.

### 3.6 `modelRetryDelayMs(attempt, err, policy = MODEL_RETRY_POLICY)`
If 429 and `parseRetryAfterMs` yields a value → `min(value, MAX_RETRY_AFTER_MS)`. Else
`fullJitterDelay(attempt, base, max)`.

### 3.7 `retryModelCall(attempt, {policy, signal})` — used by `chat` and `json`
`for i = 0; ; i++`: try `attempt()`; on error: if `i >= maxRetries` rethrow; if
`signal.aborted` rethrow; if `!isRetriableModelError` rethrow; else
`abortableSleep(modelRetryDelayMs(i, err, policy), signal)` then loop. The final
attempt's error propagates (caller normalizes abort).

### 3.8 Streaming retry is BESPOKE and PRE-TOKEN ONLY (do not reuse `retryModelCall`)
`chatStream` has its own loop. **Retry is allowed only before the first visible token has
reached the caller.** After `emitted = true`, a later failure propagates unchanged —
retrying would duplicate output into the immutable transcript. Retry condition (to
**break/throw**, i.e. stop retrying):
```
emitted || attempt >= MODEL_RETRY_POLICY.maxRetries || !isRetriableModelError(err)
```
Otherwise `abortableSleep(modelRetryDelayMs(attempt, err))` then continue.

### 3.9 `abortableSleep(ms, signal)`
Sleeps `ms`, or rejects with an `AbortError`-named error the instant the signal fires;
removes its listener on both paths (no leak). In Go: `select { case <-time.After(ms):
case <-ctx.Done(): return ctx.Err() }`.

### 3.10 Per-attempt timeout shape
The TS passes `timeout` as a **plain number** to the SDK's per-request options (and the
turn's `signal` separately) — deliberately NOT a combined `AbortSignal.any` per attempt
(it leaks listeners onto the long-lived turn signal — Node #54614). In Go: derive a
per-attempt `context.WithTimeout(turnCtx, timeoutMs)` for each request; cancel it after
the attempt. The 60s (chat/json) vs 300s (stream) split must be preserved.

---

## 4. Pricing (`pricing.ts`)

Static, approximate USD/million-token rates (Fireworks serverless ~2026-06). Drift is
expected; UI shows a coarse signal. Unknown model → `undefined` (UI shows "$?", never a
misleading `$0.000`).

### 4.1 `RATES` (longest-matching **prefix** wins, lowercased bare id)
| prefix        | inputPerM | outputPerM |
| ------------- | --------- | ---------- |
| `glm-5p2`     | `0.55`    | `2.19`     |
| `minimax-m3`  | `0.3`     | `1.2`      |
| `deepseek-v4` | `0.56`    | `1.68`     |
| `deepseek-v3` | `0.56`    | `1.68`     |

`CACHED_INPUT_DISCOUNT = 0.5` — cached prompt tokens bill at half the input rate.

### 4.2 `bareModelId(model)`
Strip any `accounts/<x>/models/<id>` path: if it contains `/`, take the substring after
the last `/`; else return as-is.

### 4.3 `rateFor(model)`
Lowercase, `bareModelId`, then scan `RATES` for entries where `bare.startsWith(prefix)`,
keeping the **longest matching prefix**. Returns the rate or `undefined`.

### 4.4 `estimateCostUsd(model, promptTokens, completionTokens, cachedTokens = 0)`
`rate = rateFor(model)`; if none → `undefined`.
```
cached     = max(0, min(cachedTokens, promptTokens))
freshInput = promptTokens - cached
inputCost  = (freshInput*inputPerM + cached*inputPerM*0.5) / 1_000_000
outputCost = (completionTokens*outputPerM) / 1_000_000
return inputCost + outputCost      // USD
```
Return type is `number | undefined` — in Go use `(float64, bool)` or `*float64`.

---

## 5. `<think>` filter — exact algorithm (`ThinkFilter`, `keepBack`)

Incrementally separates `<think>…</think>` reasoning from visible content across streamed
deltas, holding back a possibly-partial tag at the tail so a split token (`"<thi"` /
`"<think>"` straddling two chunks) is never mis-emitted.

State: `inThink bool`, `buf string` (carry), `reasoning string`, `visible string`.

### 5.1 `push(delta) -> string` (returns newly visible text)
```
buf += delta
out = ""
loop while buf.length > 0:
  if not inThink:
    open = buf.indexOf("<think>")
    if open == -1:
       safe = keepBack(buf, "<think>")     // largest prefix safe to emit
       out += buf[0:safe]; buf = buf[safe:]; break
    out += buf[0:open]
    buf = buf[open + len("<think>"):]
    inThink = true
  else:
    close = buf.indexOf("</think>")
    if close == -1:
       safe = keepBack(buf, "</think>")
       reasoning += buf[0:safe]; buf = buf[safe:]; break
    reasoning += buf[0:close]
    buf = buf[close + len("</think>"):]
    inThink = false
visible += out
return out
```

### 5.2 `end() -> string` (flush at end of stream)
```
rest = buf; buf = ""
if inThink: reasoning += rest; return ""    // unterminated think is all reasoning
else: visible += rest; return rest
```

### 5.3 `keepBack(s, tag) -> int`
Largest prefix length of `s` safe to emit without splitting a possible `tag`:
```
maxOverlap = min(len(tag)-1, len(s))
for k = maxOverlap; k > 0; k--:
   if s[len(s)-k:] == tag[0:k]: return len(s)-k
return len(s)
```
i.e. it finds the longest suffix of `s` that is a prefix of `tag` and holds it back.

> Go caution: TS string indexing is **UTF-16 code units**; `<think>` tags are ASCII, so
> indexing/length on ASCII tags is fine, but `buf` may contain multi-byte runes. Port by
> operating on **bytes** (the tags are ASCII and `indexOf`/slicing is byte-safe for the
> tag boundaries; do not split a rune when emitting `out`). Simplest faithful port:
> operate on a byte slice — `bytes.Index`, byte slicing — matching the TS code-unit
> behavior closely enough since think-tags are ASCII. The held-back tail is re-joined next
> push, so a transiently rune-split tail is harmless.

### 5.4 `stripThink(s)` (used by `json`)
Regex replace `/<think>[\s\S]*?<\/think>/g` → `""`, then `.trim()`. Non-greedy, dot-all,
global. In Go: `regexp.MustCompile("(?s)<think>.*?</think>")` with `ReplaceAllString`,
then `strings.TrimSpace`.

---

## 6. `extractJson(s)` — balanced-span extractor (exported, used by `json`)

Pull the first balanced JSON object/array out of a string, ignoring trailing prose or
`<think>` residue. String- and escape-aware so braces inside string literals don't
unbalance the count.
```
start = first index of '[' or '{'  (regex /[[{]/)
if start == -1: return s
open = s[start]; close = (open=='{') ? '}' : ']'
depth=0; inString=false; escaped=false
for i = start .. end:
   ch = s[i]
   if inString:
     if escaped: escaped=false
     else if ch=='\\': escaped=true
     else if ch=='"': inString=false
     continue
   if ch=='"': inString=true
   else if ch==open: depth++
   else if ch==close: depth--; if depth==0: return s[start..i+1]
return s[start:]   // unbalanced — let JSON.parse report the error
```
Note: only counts the SAME bracket kind as the opener (mixed `{`…`]` won't balance — by
design, mirrors TS).

---

## 7. Exported types/interfaces (TS → Go mapping)

### 7.1 `fireworks.ts`
| TS type                     | Fields / shape                                                                 | Go target |
| --------------------------- | ----------------------------------------------------------------------------- | --------- |
| `ToolCallRequest`           | `{ id:string; type:"function"; function:{name:string; arguments:string} }`    | struct (arguments is JSON string) |
| `ChatTextPart`              | `{ type:"text"; text:string }`                                                | struct/variant |
| `ChatImageUrlPart`          | `{ type:"image_url"; image_url:{ url:string } }` (NO `detail` — Fireworks ignores it) | struct/variant |
| `ChatContentPart`           | union of the two above                                                        | tagged union / iface |
| `ChatMessage`               | `{ role:"system"\|"user"\|"assistant"\|"tool"; content:string\|ChatContentPart[]\|null; tool_calls?:ToolCallRequest[]; tool_call_id?:string; name?:string }` | struct; `content` as a small union type |
| `ChatTool`                  | `{ type:"function"; function:{ name:string; description:string; parameters:Record<string,unknown> } }` | struct; `parameters` = `json.RawMessage`/`map[string]any` |
| `ChatOptions`               | `{ model; messages; tools?; toolChoice?:"auto"\|"none"\|"required"; temperature?; maxTokens?; promptCacheKey?; signal? }` | struct + `ctx` for signal |
| `ChatResult`                | `{ content:string; reasoning:string; toolCalls:ToolCallRequest[]; finishReason:string; usage?:{promptTokens?;completionTokens?;totalTokens?;cachedTokens?} }` | struct; usage pointer |
| `FireworksUnavailableError` | `code = "FIREWORKS_UNAVAILABLE"`                                               | sentinel error type |
| `ImageInputNotSupportedError`| `code = "IMAGE_INPUT_NOT_SUPPORTED"`                                          | sentinel error type |
| `CancelledError`            | `code = "CANCELLED"`, default msg `"Turn cancelled"`                           | sentinel error type |
| `ThinkFilter`               | class (see §5)                                                                 | struct with methods |

### 7.2 Exported functions (`fireworks.ts`)
| Function                    | Signature                                                | Behavior |
| --------------------------- | -------------------------------------------------------- | -------- |
| `textPart(text)`            | `(string) -> ChatTextPart`                               | trivial constructor |
| `imageDataPart(base64, mimeType="image/png")` | `(string,string) -> ChatImageUrlPart`  | wraps as `data:<mime>;base64,<b64>`; default PNG (Daintree screenshots) |
| `hasImageContent(messages)` | `(ChatMessage[]) -> boolean`                             | any message whose content array has an `image_url` part — drives tier gate |
| `contentToText(content)`    | `(string\|ChatContentPart[]\|null) -> string`            | null→`""`; string→self; array→join parts with `\n`, image parts → `"[image omitted]"` (never base64) |
| `extractJson(s)`            | `(string) -> string`                                     | §6 |
| (private) `toWireMessages`, `normalizeToolCalls`, `stripThink`, `hashString`, `keepBack`, `isAbortError` | — | see §2/§5/§9 |
| `FireworksClient.chat`      | `(ChatOptions) -> Promise<ChatResult>`                   | §2.3/§2.6 + retry |
| `FireworksClient.chatStream`| `(ChatOptions, onToken?) -> Promise<ChatResult>`         | §2.4/§2.9 + pre-token retry |
| `FireworksClient.json<S>`   | `(Omit<ChatOptions,"tools"\|"toolChoice">, schema) -> Promise<infer S>` | §2.5; strip think → extractJson → parse → validate |

### 7.3 `router.ts` — `ModelRouter`
Constructor `(cfg, fw?)` — `fw` defaults to `new FireworksClient(cfg)`. Public `readonly fw`.
| Method                          | Signature                                                       | Behavior |
| ------------------------------- | -------------------------------------------------------------- | -------- |
| `modelFor(tier)`                | `(ModelTier) -> string`                                        | §1.2 switch |
| `chat(tier, opts)`              | `(ModelTier, Omit<ChatOptions,"model">) -> Promise<ChatResult>`| assertImageTier → log → fw.chat → log; cancel logged as `model.cancelled` |
| `stream(tier, opts, onToken?)`  | `(ModelTier, …, cb) -> Promise<ChatResult>`                    | same wrapper around `fw.chatStream` |
| `json<S>(tier, opts, schema)`   | `(ModelTier, Omit<…,"model"\|"tools"\|"toolChoice">, schema) -> Promise<infer S>` | wrapper around `fw.json` |
| `describe()`                    | `() -> Record<string,string>`                                 | `{ large, medium, small }` model ids |
| (private) `assertImageTier`     | throws `ImageInputNotSupportedError` if `tier !== "large"` AND `hasImageContent(messages)` | **tier gate** |
| (private) `logRequest/Response/Error` | debug-log only; `redactImageData` masks base64 in logs  | see below |

**Image tier gate (CRITICAL):** only `large` is vision-capable. `small` is text-only and
`medium` routes through, so an image bound for either must fail with a **clear local
error before any wire call**. Gate on tier *semantics*, not the resolved model id (medium
rejects even though it currently resolves to the large model id). Enforced at the top of
`chat`/`stream`/`json`.

**Cancel tracing:** when the wrapped call throws `CancelledError`, the router logs
`model.cancelled` (not `model.error`) and rethrows — a clean user abort must not read as
a model failure.

`redactImageData(message)`: in debug logs, replace base64 `data:` image URLs with
`"<redacted base64 ~<N>kb>"` where `N = ceil(url.length*3/4/1024)`; non-`data:` schemes →
`"<redacted image url>"`. Text/string content passes through. (Port only if you port the
debug-log subsystem; the masking math is the load-bearing detail.)

---

## 8. Prompts (`prompts/`)

### 8.1 Layering (CRITICAL — cache stability)
The main-thread system prompt is **three stable control messages**:
- **message[0]** = `BASE_SYSTEM_PROMPT` (`base.ts`) — permanent identity + hard rules +
  the static `DAINTREE_MCP_REFERENCE`. **This is the cached prefix; it MUST be byte-stable.**
- **message[1]** = `buildRuntimeContextMessage(ctx)` (`runtimeContext.ts`) — tier / project
  / MCP status / model ids / skill catalog / project instructions (everything dynamic).
- **message[2]** = `buildLoadedSkillsMessage(bundle)` (`skills.ts`) — bodies of skills
  loaded for the current task.

`buildMainSystemPrompt(ctx)` (legacy single-string view) =
`BASE_SYSTEM_PROMPT + "\n\n" + buildRuntimeContextMessage(ctx)`. Concatenation joiner is
`"\n\n"`. **Preserve every joiner and whitespace exactly** — these strings are cache keys.

### 8.2 `BASE_SYSTEM_PROMPT` structure (`base.ts`)
`BASE_SYSTEM_PROMPT = IDENTITY_AND_RULES + "\n\n" + DAINTREE_MCP_REFERENCE`.
`IDENTITY_AND_RULES` sections (markdown, verbatim — port the **exact text**, do not
paraphrase): `You are the **Daintree Assistant** …`, then `Mission:`, `Hard rules:`,
`Tool-use discipline:`, `Skills — your main way of learning…`, `Communication:`.
Key invariants encoded in the text (must survive verbatim): never edits files; reads only
via `fs.list/fs.read/fs.search`; file changes go through `agentTask.spawnForEdits`
(mode `edit`|`explore`); always title every spawn; prefer typed wrappers over
`daintree.call`; never re-issue an identically-failing call; report tool outcomes
faithfully (quote real `terminalId`/`watcherId`, never invent); a `terminal_exited`
signal is to VERIFY not a fact; watchers/timers run only in foreground; **never format
output as a markdown table** (narrow inline surface — use bulleted lists).

### 8.3 `DAINTREE_MCP_REFERENCE` (`daintreeMcp.ts`) — static, part of cached prefix
A large verbatim markdown block: `# Daintree integration reference (verified)` with
sections `## How you act`, `## Playbook: spawn an agent and relay what it said`,
`## Daintree MCP surface (what the wrappers call; verified shapes)`, `## Gotchas`. Port
verbatim. It intentionally names **non-existent** tools as negative examples
(`terminal.listStatus`, `terminal.waitForAny`) — keep them.

Also exports `DOCUMENTED_MCP_TOOL_NAMES: string[]` — a hand-maintained list of 60 verified
Daintree MCP tool names (used at startup to detect drift; any name absent from the live
server's list means the doc went stale). Port the array verbatim. Full list (exact order):
`actions.getContext, actions.list, actions.search, actions.getSchema, agent.focusNextAgent,
agent.focusNextWaiting, agent.focusNextWorking, agent.focusPreviousAgent, agent.launch,
copyTree.generate, copyTree.generateAndCopyFile, copyTree.injectToTerminal,
forge.addIssueComment, forge.addIssueLabel, forge.approvePR, forge.assignIssue,
forge.closeIssue, forge.closePR, forge.commentOnPR, forge.convertPRToDraft,
forge.createIssue, forge.createPR, forge.dismissReview, forge.editIssue, forge.editPR,
forge.getIssue, forge.getPR, forge.listIssues, forge.listPRs, forge.markPRReadyForReview,
forge.mergePR, forge.removeIssueLabel, forge.reopenIssue, forge.reopenPR,
forge.requestChanges, forge.requestReviewers, forge.unassignIssue, git.getProjectPulse,
git.snapshotDelete, git.snapshotRevert, panel.focus, recipe.list, recipe.run,
terminal.arm, terminal.disarm, terminal.disarmAll, terminal.getOutput, terminal.getStatus,
terminal.list, terminal.sendCommand, terminal.waitUntilIdle, workflow.focusNextAttention,
workflow.prepBranchForReview, workflow.startWorkOnIssue, worktree.createWithRecipe,
worktree.getCurrent, worktree.list`.

### 8.4 `runtimeContext.ts`
`MainPromptContext` interface fields: `tier:Tier; projectPath:string; projectId?:string;
mcpConnected:boolean; mcpStatusLine:string; largeModel:string; smallModel:string;
activeWorktree?:string; schedulerActive:boolean; projectInstructions?:string`.

`TIER_BLURB: Record<Tier,string>` — one verbatim blurb per tier (`supervisor` read-only,
`operator`, `system` high-risk). Port the three strings exactly.

`buildRuntimeContextMessage(ctx)` builds a `\n`-joined line list:
```
# Runtime context
Permission tier: <tier> — <TIER_BLURB[tier]>
Project path: <projectPath>
Project id: <projectId ?? "(none)">
Active worktree: <activeWorktree ?? "(unknown — read with context.snapshot)">
Daintree MCP: <mcpStatusLine>
Models: large=<largeModel>, small=<smallModel>
```
Then conditional appends (each verbatim):
- if `!mcpConnected`: a `NOTE: Daintree MCP is NOT connected …` degraded-mode line.
- if `!schedulerActive`: a `NOTE: the scheduler is NOT running …` dormant line.
- if `projectInstructions`: push `""`, `# Project instructions`, a framing sentence, then
  the raw `projectInstructions` content.
Join with `"\n"`.

### 8.5 `skills.ts`
- `buildSkillCatalogMessage(skills: SkillMetadata[])`: `""` if empty. Else `# Skill catalog`
  + framing paragraphs + `Available skills:\n` + entries, each:
  `- <id> — <summary>\n  When to use: <whenToUse>` joined by `\n`.
- `buildLoadedSkillsMessage(skills: RenderedSkillBundle)`: if `items.length===0` → a
  `# Loaded skills\nNo task-specific skills are currently loaded…` fallback. Else
  `# Loaded skills` + framing + a `Step tracking:` paragraph + bodies, each:
  `## Skill <i+1>: <title>\nSkill id: <id>\nVersion: <version>\n<body>` joined by `\n\n`.

`SkillMetadata` fields used: `id, summary, whenToUse`. `RenderedSkillBundle` (from
`skills/render.ts`): `{ ids:string[]; hash:string (12-char sha256 of "id@version|…");
cacheKey:string (=`daintree-main-v1-skills-<hash>`, debug only); items:Skill[] }`. `Skill`
item fields used here: `id, version, title, body` (sorted by id). The render hash/cacheKey
is **debug-only** — the actual Fireworks `prompt_cache_key` stays the stable
`MAIN_PROMPT_CACHE_KEY` regardless of loaded skills.

### 8.6 Small-model sub-agent prompts (`prompts/index.ts`) — port verbatim
| Export                          | Kind     | Notes |
| ------------------------------- | -------- | ----- |
| `WATCHER_SYSTEM_PROMPT`         | const    | classifier; returns JSON with `classification`/`confidence`/`summary`/`evidence`/`recommendedAction`. The model-facing classification value set is the **14**: `no_change, still_working, waiting_for_input, permission_prompt, command_failed, tests_failed, tests_passed, merge_conflict, completed_success, completed_unknown, terminal_exited, rate_limited, needs_large_model, unknown`. (`completed_unverified` is engine-only, NOT in the prompt.) |
| `buildWatcherUserPrompt(args)`  | fn       | args `{goal, agentState?, runtimeStatus?, lastOutputAt?, previous?, tail}` → templated string |
| `JUDGE_SYSTEM_PROMPT`           | const    | yes/no judge; returns `{reason, confidence, matched}` — `reason` BEFORE `matched` on purpose (implicit CoT) |
| `buildJudgeUserPrompt(args)`    | fn       | args `{question, goal, agentState?, runtimeStatus?, waitingReason?, lastOutputAt?, tail}` |
| `SUMMARIZER_SYSTEM_PROMPT`      | const    | terse factual summary; no "think out loud" preamble |
| `buildSummarizerUserPrompt(args)`| fn      | args `{purpose, tail}` |
| `EXTRACTOR_SYSTEM_PROMPT`       | const    | extract value; text or `{ "result": <value> }` json; no preamble |
| `buildExtractorUserPrompt(args)`| fn      | args `{instruction, format:"text"\|"json", jsonSchema?, tail, terminalIds:string[]}`; header switches singular/plural on `terminalIds.length` |

Each `build*UserPrompt` uses fixed templates with fallbacks like `?? "unknown"`,
`?? "none"`, and `tail || "(no output captured)"`. Port the templates byte-for-byte —
they are not cached but they are the contract the small model is tuned against.

### 8.7 Schemas the json prompts validate against (`schemas.ts`)
- `WatcherVerdict` (strict): `{ classification: WatcherClassification, confidence:number
  [0,1], summary:string, evidence:string[] default [], recommendedAction: enum(none,
  focus_terminal, ask_user, send_input, spawn_helper, open_review) default "none" }`.
- `ModelJudgeAnswer` (strict): `{ reason:string, confidence:number [0,1], matched:boolean }`.
- `WatcherClassification` enum is **15** values (the 14 above + `completed_unverified`,
  which only the engine sets). See §7-adjacent in schemas.

---

## 9. Misc constants & helpers

| Item                       | Value / behavior |
| -------------------------- | ---------------- |
| `MAIN_PROMPT_CACHE_KEY`    | `"daintree-main"` (in `agent/loop.ts`). Plain, **unversioned**. Passed as `prompt_cache_key` on the main-thread stream call (`loop.ts:589`). Never bump — a prefix edit just misses on changed tokens; it never serves stale content. **Hard contract: keep this exact string.** |
| Skill render cache key     | `"daintree-main-v1-skills-<12charhash>"` — DEBUG/LOG ONLY, not sent to Fireworks. |
| `CLEAR_MARKER`             | `"[conversation cleared — context reset to initial state]"` (loop.ts) — adjacent, not strictly models. |
| Default `temperature`      | `0.3` for `chat`/`chatStream`; `0` for `json`. |
| Synthetic tool-call id     | `"call_" + Math.abs(hashString(...))`. |
| `hashString(s)`            | JS string hash: `h=0; for each char: h = (h<<5)-h + charCodeAt(i); h |= 0` (forces int32). Then callers take `Math.abs`. **Port exactly** (32-bit signed wrap via `int32`) so synthesized ids match — they appear in transcripts. Iterate over **UTF-16 code units** (`charCodeAt`), not runes, for bit-exact parity. |

---

## 10. Concrete Go mapping proposal

### 10.1 Packages
- `internal/models` — `Router` (was `ModelRouter`), `Fireworks` client, `ChatOptions`,
  `ChatResult`, `ChatMessage`, content parts, `ThinkFilter`, `ExtractJson`, pricing.
- `internal/models/prompts` — base prompt const, runtime-context/skill-catalog/loaded-skills
  builders, the small-model sub-agent prompt builders, `DocumentedMCPToolNames`.
- `internal/reliability` — `RetryPolicy`, `FullJitterDelay`, `IsRetriableModelError`,
  `ParseRetryAfterMs`, `ModelRetryDelayMs`, `AbortableSleep` (→ ctx-based), the constants.

### 10.2 Key Go types / interfaces
- `type Tier string` (`small`/`medium`/`large`); `type PermTier string`
  (`supervisor`/`operator`/`system`).
- `ChatMessage` with `Content` as an interface or a small struct holding
  `*string | []ContentPart`; `ContentPart` as a tagged struct (`Type string` +
  `Text`/`ImageURL`). Custom `MarshalJSON` to emit exactly the wire shape (§2.7) and to
  **omit** undefined keys (pointers + `omitempty`, or hand-rolled marshaling).
- Sentinel errors via `errors.New` / typed structs implementing `error`:
  `ErrFireworksUnavailable` (code `FIREWORKS_UNAVAILABLE`), `ErrImageInputNotSupported`
  (`IMAGE_INPUT_NOT_SUPPORTED`), `CancelledError` (`CANCELLED`). Preserve the `code`
  strings if anything downstream switches on them.
- `Usage` struct with pointer fields (`*int`) to mirror optional `cachedTokens` etc.

### 10.3 Libraries
- **HTTP**: stdlib `net/http`. Do NOT pull an OpenAI Go SDK — hand-roll the request to
  keep wire control (the `as never` casts and field-omission rules in TS are exactly the
  control we need; a fat SDK would re-add its own retries/fields). `Authorization: Bearer`.
- **SSE**: parse manually (`bufio.Scanner` over the body, split events on blank line,
  strip `data: `, stop on `[DONE]`). `bufio.Scanner` default token size is too small for
  big chunks — set a large buffer or use a custom split.
- **JSON**: stdlib `encoding/json`. For request bodies, custom marshaling or pointer
  fields to honor omit-when-undefined.
- **Schema validation** (the Zod `.parse` on json results): port `WatcherVerdict` /
  `ModelJudgeAnswer` as structs with explicit defaulting (`evidence` default `[]`,
  `recommendedAction` default `"none"`) and range checks (`confidence ∈ [0,1]`). A
  validation lib is optional; explicit checks match the Zod semantics most faithfully.
- **Cancellation**: `context.Context` everywhere the TS uses `AbortSignal`. Per-attempt
  `context.WithTimeout` (60s chat/json, 300s stream).
- **Backoff**: hand-roll `FullJitterDelay` with `math/rand` (matches exactly); do not pull
  a backoff lib (the full-jitter formula and the Retry-After cap are load-bearing).

### 10.4 Faithful-port checklist (do not silently "improve")
- Keep `temperature` defaults split (0.3 vs 0) and `max_tokens` omission.
- Keep `stream_options.include_usage` on every stream.
- Keep `prompt_cache_key` ONLY on `chat`/`chatStream` when set, and NEVER on `json`.
- Keep pre-token-only streaming retry (no retry after first emitted token).
- Keep the image tier gate on tier semantics, before any wire call.
- Keep `<think>` filter + `keepBack` byte-exact behavior, and `stripThink` regex.
- Keep `MAIN_PROMPT_CACHE_KEY = "daintree-main"` and all prompt text byte-stable.

---

## 11. DELETE — do NOT port (Node/Bun/SDK-specific)

- The dependency on the `openai` npm SDK and `OpenAI`/`APIUserAbortError`/`APIError`
  imports — replaced by a hand-rolled HTTP/SSE client + Go HTTP-status classification.
- `AbortSignal` / `AbortSignal.any` plumbing and the Node #54614 workaround commentary —
  Go uses `context.Context`; the "don't combine signals per attempt" caveat is moot.
- `as never` / `as Record<string,unknown>` casts (TS type-erasure shims) — Go marshaling
  handles the wire shape directly.
- The `zod` import and `z.infer<S>` generic plumbing in `json<S>` — replace with concrete
  Go structs + explicit validation.
- `debugLog`/`logDebug` calls in the router (`model.request/response/error/cancelled`) and
  `redactImageData` — port only if/when the debug-log subsystem is ported; otherwise stub.
  (If kept, preserve the `redactImageData` size-marker math.)
- `MCP_READ_RETRY_POLICY` / `MCP_READ_TIMEOUT_MS` / `isRetriableMcpError` /
  `detectRateLimitSignature` / `RATE_LIMIT_TAIL_WINDOW` / `RATE_LIMIT_SIGNATURE` — these
  live in `reliability.ts` but belong to the **MCP / watcher** subsystems, not models.
  Port them with those subsystems, not here.
- `bun:sqlite` / `node:sqlite` driver adaptivity, OpenTUI, React — entirely unrelated to
  this subsystem.
