# Port spec: Agent loop (`src/agent/loop.ts` + `src/agent/events.ts`)

Authoritative reference for porting the main-thread agentic turn loop and its
structured event sink to Go (Bubble Tea UI). Implementation agents should port
**from this file without re-reading the TypeScript**. Source of truth at time of
writing: `src/agent/loop.ts` (1114 lines) and `src/agent/events.ts` (319 lines),
with supporting contracts in `src/schemas.ts`, `src/models/fireworks.ts`,
`src/tools/types.ts`, `src/agent/wake.ts`, `src/skills/types.ts`,
`src/skills/render.ts`, `src/storage/db.ts`.

---

## 1. What this subsystem does

`AgentSession` runs **one user (or autonomous) turn** to completion:

1. (optional) **auto-compact** the conversation if it's grown too large.
2. Push the user message into history.
3. Build the per-turn **tool projection** (which tools the model may call).
4. Loop up to **12 iterations**: stream the large model → if it returns tool
   calls, dispatch each through the `ToolRegistry`, feed results back; if it
   returns plain text, that's the final answer.
5. Enforce a **repeated-identical-failure circuit breaker** (warn at 2, abort
   at 3) so a model that hammers one broken call can't burn the whole budget.
6. Persist every message to SQLite (`conversation` table) for resume.
7. Emit a structured **event stream** (`AgentEventSink`) consumed by the UI and a
   durable `run_events` logger — never writes to stdout directly.

It also owns **skill state** (3 control messages at fixed indices, on-demand
skill loading capped at 3), **conversation rehydration** on resume (orphan
trimming, dup-seq detection, clear/compact markers), **clear/compact**, and
**oversized-tool-result truncation** into session artifacts.

`send()` **never throws** on a model/tool failure — it catches and returns a
sentinel string. Callers (wake reactors) pattern-match the prefix to detect a
non-result (see §10).

---

## 2. Magic constants, limits, thresholds (EXACT values — port verbatim)

| Constant | Value | Where / meaning |
|---|---|---|
| `MAX_TOOL_ITERATIONS` | `12` | loop.ts. Max model↔tool round-trips per turn. On exceed: return `"Reached the tool-iteration limit without a final answer."` |
| `REPEAT_FAILURE_WARN` | `2` | Identical-failure count that injects ONE corrective nudge. |
| `REPEAT_FAILURE_ABORT` | `3` | Identical-failure count that aborts the turn. |
| `CANCELLED_REPLY` | `"Turn cancelled"` | **Exported.** Sentinel returned on user-cancel. |
| `CONTROL_MESSAGE_COUNT` | `3` | Control messages always at front (indices 0,1,2). |
| `CLEAR_MARKER` | `"[conversation cleared — context reset to initial state]"` | **Exported.** Durable-log breadcrumb from `clear()`. Note the em-dash `—` (U+2014). |
| `MAIN_PROMPT_CACHE_KEY` | `"daintree-main"` | Fireworks `prompt_cache_key` for large-model calls. Plain, unversioned. Keep byte-stable. |
| `SKILL_CONTEXT_MUTATING_TOOLS` | `Set{"skill.find","skill.load"}` | `risk:"read"` tools withheld on read-only turns. |
| `MAX_TOOL_RESULT_CHARS` | `8000` | Inline serialized tool result cap; overflow → artifact stub. |
| `TRUNCATION_PREVIEW_CHARS` | `1500` | Preview slice inside truncation stub. |
| `TRUNCATION_SUMMARY_CHARS` | `500` | Summary slice inside truncation stub. |
| `MAX_STORED_ARTIFACTS` | `64` | Per-session artifact cap; oldest-first eviction. |
| `AUTO_COMPACT_TOKEN_THRESHOLD` | `60_000` | Estimated-token threshold to auto-compact. Also the context-pressure denominator in `usage`. |
| `CHARS_PER_TOKEN` | `4` | Token estimator divisor. |
| `MAX_RUN_EVENT_PAYLOAD` | `8000` | events.ts. Max serialized `run_events` payload; overflow → truncation marker. |
| Skill cap | `3` | `resolveKnownIds` slices to 3; `SkillSelection.skillIds` max 3. |
| Compaction marker (durable) | `"[conversation compacted — earlier turns dropped from context]"` | Written by `compact()` (NOT exported; matched on prefix `"[conversation compacted"`). |
| Compaction note (live) | `"[compacted summary of earlier conversation]\n${summary}"` | role `user`, kept live after compact. |
| Inject-note prefix | `"[system event]\n${note}"` | role `user`. |
| `userInput` log slice | `1000` | `logSelection` slices userInput to 1000 chars. |
| run id format | `run_${randomUUID().slice(0,8)}` | 8 hex chars. |
| artifact id format | `artifact_${randomUUID().slice(0,8)}` | 8 hex chars. |
| run-event id format | `rne_${randomUUID().slice(0,8)}` | (Db-assigned) |
| conversation msg id | `msg_${randomUUID().slice(0,8)}` | (Db-assigned) |
| skill-selection id | `rsl_${randomUUID().slice(0,8)}` | (Db-assigned) |
| serializePayload preview headroom | `MAX_RUN_EVENT_PAYLOAD - 200` = `7800` | events.ts truncation marker preview slice. |

**Note**: `randomUUID().slice(0,8)` takes the first 8 chars of a v4 UUID string
(i.e. first 8 hex digits of `xxxxxxxx-...`). Go: use `github.com/google/uuid`
`uuid.NewString()[:8]` or `crypto/rand` → hex.

---

## 3. `CORE_TOOL_NAMES` (exact list, order matters for dedup but Set collapses)

Always offered to the model regardless of loaded skills. Union with loaded
skills' `requiredTools` forms the per-turn projection.

```
context.snapshot
fs.read
fs.list
fs.search
queue.digest
daintree.status
tool.search
terminal.read
terminal.extract
skill.step.advance
skill.run.get
skill.find
skill.load
memory.recall
memory.list
artifact.read
```

These are **internal dotted names**. The wire (OpenAI) name uses `__` (e.g.
`fs__read`); `registry.resolveWireName(wireName)` maps back. Preserve both forms.

---

## 4. Exported types / interfaces / functions

### 4.1 From `loop.ts`

| Export | Kind | Signature / shape | Behaviour note |
|---|---|---|---|
| `CANCELLED_REPLY` | const string | `"Turn cancelled"` | Returned by `send` on cancel. |
| `CLEAR_MARKER` | const string | see §2 | Clear breadcrumb + rehydrate boundary. |
| `rehydrateSession` | function | `(rows: ConversationMessageRecord[]) => { restoredMessages: ChatMessage[]; initialSeq: number } \| undefined` | Reconstruct working history on resume (§7). `undefined` ⇒ start fresh. |
| `AgentSessionDeps` | interface | see §5 | Constructor deps. |
| `AgentSession` | class | see §6 | The turn engine. |
| `serializeToolResult` | function | `(res: {ok,summary,result?,error?}, artifactStore?: Map<string,string>) => string` | Serialize a tool result to JSON, truncating oversized ones into an artifact stub (§9). |

### 4.2 From `events.ts`

| Export | Kind | Shape | Note |
|---|---|---|---|
| `ToolCallEvent` | interface | `{ id: string; name: string; args: unknown; startedAt: number }` | `args` is parsed JSON, or the raw string on parse failure. `startedAt`/`endedAt` are ms epoch. |
| `ToolResultEvent` | interface | `{ id: string; name: string; result: ToolResult; endedAt: number }` | `id` matches `ToolCallEvent.id`. |
| `AgentUsageEvent` | interface | see §11 | Token/cost/context-pressure for one model call. |
| `AgentEventSink` | interface | see §11 | The event vocabulary. |
| `noopAgentEvents` | const | all-no-op `AgentEventSink` | Default sink. |
| `RunIdRef` | type | `{ current: string \| undefined }` | Shared mutable run-id holder. |
| `multiSink` | function | `(...sinks: AgentEventSink[]) => AgentEventSink` | Fan-out, each sink isolated by try/catch. |
| `RunEventSink` | class | `implements AgentEventSink`, ctor `(db: Db, ref: RunIdRef)` | Persists events to `run_events` (§12). |

---

## 5. `AgentSessionDeps` (constructor input)

```ts
interface AgentSessionDeps {
  router: ModelRouter;            // .stream(tier,opts,onToken), .chat(tier,opts), .modelFor?(tier)
  registry: ToolRegistry;         // .toOpenAITools(names?), .resolveWireName(w), .dispatch(name,args,ctx), .list()
  skillRegistry: SkillRegistry;   // .metadataForSelection(), .getMany(ids), .has(id)
  ctx: ToolContext;               // base, run-agnostic, signal-free (see §13)
  promptContext: MainPromptContext;
  sessionId: string;
  restoredMessages?: ChatMessage[]; // present (even empty) ⇒ resume: rebuild controls but DON'T re-persist
  initialSeq?: number;              // seq to continue from on resume (max stored seq + 1)
  events?: AgentEventSink;          // defaults to noopAgentEvents
  runIdRef?: RunIdRef;              // stamped with current run id per turn
}
```

**Resume discriminator**: `restoredMessages !== undefined` ⇒ resumed session.
Controls are rebuilt fresh (so cached prefix stays byte-stable) but NOT
re-persisted (they already exist in DB). `seq` continues from `initialSeq ??
control.length`. Otherwise fresh session: controls are persisted.

---

## 6. `AgentSession` — internal state & methods

### 6.1 Private state
- `messages: ChatMessage[]` — the live model history.
- `seq: number` — next DB seq to write (monotonic, never collides on resume).
- `activeSkillIds: string[]` — loaded skill ids (≤3).
- `skillBundle: RenderedSkillBundle` — current rendered bundle.
- `skillCatalog: string` — static menu, built once in ctor.
- `events: AgentEventSink`.

### 6.2 Control message layout (FIXED indices)
```
messages[0] = { role:"system", content: BASE_SYSTEM_PROMPT }           // cached prefix, immutable mid-session
messages[1] = { role:"system", content: runtimeContext + "\n\n" + skillCatalog }
messages[2] = { role:"system", content: buildLoadedSkillsMessage(skillBundle) }
```
`composeRuntimeContext` = `buildRuntimeContextMessage(promptContext)` then, if
`skillCatalog` non-empty, `${runtime}\n\n${skillCatalog}`. The catalog rides on
[1] (appended, never interleaved) so [2] stays the loaded-skills slot and the
"# Runtime context" header stays at the top of [1].

### 6.3 Public/notable methods

| Method | Signature | Behaviour |
|---|---|---|
| ctor | `(deps)` | Build catalog + 3 controls; resume vs fresh per §5. |
| `refreshRuntimeContext` | `(promptContext) => void` | Rewrites ONLY `messages[1]` (re-appends catalog). Cached prefix [0] untouched. Does NOT re-persist. |
| `getMessages` | `() => ReadonlyArray<ChatMessage>` | |
| `injectNote` | `(note: string) => void` | Push `{role:"user", content:"[system event]\n"+note}`. |
| `compact` | `(summary: string) => void` | Keep controls[0..3); replace working history with one `user` note `"[compacted summary…]\n"+summary`. Persist a `system` marker `"[conversation compacted — earlier turns dropped from context]"` then the note. |
| `clear` | `() => void` | Truncate `messages` to first 3; persist `system` row `CLEAR_MARKER`. Loaded skills left as-is. |
| `maybeAutoCompact` | `(signal?) => Promise<void>` (private) | §8. |
| `send` | `(userInput, {readOnly?, signal?}={}) => Promise<string>` | Mints `run_<uuid8>`, sets `runIdRef.current`, runs `runTurn`, clears ref in `finally`. |
| `runTurn` | private, the core loop | §6.4. |
| `buildToolFilter` | `() => string[] \| undefined` (private) | No active skills ⇒ `undefined` (full registry). Else `unique(CORE_TOOL_NAMES ∪ skills.flatMap(requiredTools))`. |
| `readOnlyToolNames` | `() => string[]` (private) | `registry.list().filter(t => t.risk==="read" && !SKILL_CONTEXT_MUTATING_TOOLS.has(t.name)).map(name)`. |
| `pushMessage` | `(m) => void` (private) | `messages.push(m)` + `persistMessage(m)`. |
| `persistMessage` | `(m) => void` (private) | best-effort `db.insertMessage({sessionId, seq: seq++, role, content: contentToText(m.content), toolCallsJson: m.tool_calls?JSON.stringify:undefined, toolCallId: m.tool_call_id})`. Swallows errors. |
| `getActiveSkillIds` | `() => ReadonlyArray<string>` | |
| `findSkills` | `(query, signal?) => Promise<SkillFindResult>` | §14. |
| `setSkills` | `(ids) => void` | `applySkillBundle(getMany(resolveKnownIds(ids)))`. |
| `loadAdditionalSkills` | `(ids) => string[]` | merge new ids FIRST: `resolveKnownIds([...ids, ...activeSkillIds])`, apply, return active. |
| `resolveKnownIds` | `(ids) => string[]` (private) | `unique(ids).filter(has).slice(0,3)` — filter-before-cap so a hallucinated id can't evict a valid one. |
| `describeSkills` | `() => string` | `/skills loaded` render. |
| `applySkillBundle` | `(skills) => void` (private) | `skillBundle = renderSkillBundle(skills); activeSkillIds = bundle.ids; messages[2] = loaded-skills msg`. |
| `logSelection` | `(userInput, selection, selectedIds) => void` (private) | best-effort `db.insertSkillSelection(...)`, `userInput.slice(0,1000)`. |

### 6.4 `runTurn` algorithm (port faithfully — ordering is load-bearing)

```
1. if signal.aborted: events.assistantCancelled(""); return CANCELLED_REPLY
   (NO model work, do NOT push user message — leaves no orphan turn)
2. await maybeAutoCompact(signal)
3. if signal.aborted (again): events.assistantCancelled(""); return CANCELLED_REPLY
   (re-check: a cancel landing in the auto-compact window must also leave no orphan
    turn — issue #61 pull-back depends on this)
4. pushMessage({role:"user", content:userInput})
5. allowedNames = readOnly ? readOnlyToolNames() : buildToolFilter()
6. runCtx = {...deps.ctx, runId, signal, activeToolNames: allowedNames}
7. allowedSet = readOnly ? new Set(allowedNames) : undefined
8. failureCounts = Map<sig,count>(); stuckNudged = false
9. try { tools = registry.toOpenAITools(allowedNames) }
   catch e: events.error("Tool projection failed: "+msg); return "Tool projection failed: "+msg
   (NOTE: this string prefix is a WAKE_FAILURE_PREFIX — keep verbatim)

10. for i in 0..MAX_TOOL_ITERATIONS-1:
   a. if signal.aborted: assistantCancelled(""); return CANCELLED_REPLY
   b. events.assistantStart()
   c. try result = router.stream("large", {messages, tools, toolChoice:"auto",
        promptCacheKey: MAIN_PROMPT_CACHE_KEY, signal}, tok => events.assistantToken(tok))
      catch:
        - CancelledError       → assistantCancelled(""); return CANCELLED_REPLY
        - FireworksUnavailable  → msg="Model unavailable: "+e.message; events.error(msg); return msg
        - other                → msg="Model error: "+(e.message||String); events.error(msg); return msg
   d. emit usage event (see §11; computed BEFORE appending assistant msg)
   e. pushMessage({role:"assistant", content: result.content||null,
        tool_calls: result.toolCalls.length ? result.toolCalls : undefined})
   f. if result.toolCalls.length===0:
        events.assistantEnd(result.content, result.reasoning||undefined)
        return result.content
   g. // execute each tool call
      worstRepeat = undefined
      for c in 0..calls.length-1:
        call = calls[c]
        internalName = registry.resolveWireName(call.function.name) ?? call.function.name
        parse args = call.function.arguments ? JSON.parse(...) : {}   // catch → parseFailed
        startedAt = Date.now()
        if parseFailed:
          events.toolCall({id, name:internalName, args: call.function.arguments, startedAt})
          res = fail INVALID_TOOL_ARGS_JSON (recoverable:true) summary "Invalid JSON arguments for X; not executed."
        elif allowedSet && !allowedSet.has(internalName):
          events.toolCall({id, name:internalName, args, startedAt})
          res = fail READ_ONLY_TURN (recoverable:false) summary "X is not available on an autonomous read-only turn."
        else:
          events.toolCall({id, name:internalName, args, startedAt})
          res = await registry.dispatch(internalName, args, runCtx)
        events.toolResult({id, name:internalName, result:res, endedAt:Date.now()})
        pushMessage({role:"tool", tool_call_id:call.id, name:internalName,
                     content: serializeToolResult(res, runCtx.artifactStore)})
        if !res.ok:
          sig = JSON.stringify([internalName, call.function.arguments ?? "", res.error?.code ?? ""])
          count = (failureCounts.get(sig) ?? 0) + 1; failureCounts.set(sig, count)
          if !worstRepeat || count>worstRepeat.count: worstRepeat = {name:internalName, count, res}
        if signal.aborted:
          // push CANCELLED stub for EVERY remaining undispatched call (r=c+1..end)
          //   to keep transcript well-formed (assistant tool_calls must each have a result)
          for r in c+1..calls.length-1:
            pendingName = resolveWireName(...) ?? raw
            pushMessage({role:"tool", tool_call_id:pending.id, name:pendingName,
              content: serializeToolResult(fail CANCELLED "Turn cancelled." (recoverable:false)
                summary "Turn cancelled before this tool was executed.", runCtx.artifactStore)})
          assistantCancelled(""); return CANCELLED_REPLY
      // after all calls in batch have a result:
      if worstRepeat && worstRepeat.count >= REPEAT_FAILURE_ABORT(3):
        detail = error.code ? `${code}: ${message??""}`.trim() : res.summary
        msg = `Stopped: called ${name} ${count} times this turn with identical arguments, each failing the same way (${detail}). Tell the user what's blocking and what you tried rather than repeating the call.`
        events.error(msg); return msg     // prefix "Stopped: called " is a WAKE_FAILURE_PREFIX
      if worstRepeat && worstRepeat.count >= REPEAT_FAILURE_WARN(2) && !stuckNudged:
        stuckNudged = true
        pushMessage({role:"user", content:`[system event]\nYou have called ${name} ${count} times this turn with byte-identical arguments and it failed the same way each time${code?` (${code})`:""}. Repeating the exact same call will keep failing. Read the error, CHANGE the arguments (or use a different tool/approach), or stop and report what's blocking you — do not emit the same arguments again.`})

11. // fell out of the loop:
    msg = "Reached the tool-iteration limit without a final answer."
    events.error(msg); return msg          // WAKE_FAILURE_PREFIX
```

**Circuit-breaker signature** is `JSON.stringify([internalName, rawArgsString,
errorCode])` — the EXACT raw argument string the model emitted (not the parsed
object), plus error code. Only a byte-for-byte repeat that fails the SAME way
increments the same counter. Changed args or a different error reset to their own
signature. Port this exactly (use the raw arguments JSON string, not re-encoded).

**Ordering invariant**: when a cancel lands mid-batch, EVERY remaining tool_call
id MUST get a `tool` reply (the CANCELLED stub), or the persisted history is
structurally invalid for Fireworks on next replay (assistant `tool_calls` with no
matching `tool` result → 400). This is the same reason `dropOrphanToolCallTail`
exists on rehydrate.

---

## 7. `rehydrateSession` (resume reconstruction)

`(rows) => { restoredMessages, initialSeq } | undefined`

```
1. rows.length===0 → undefined  (start fresh)
2. dup-seq detection: if unique(rows.map(seq)).size !== rows.length → undefined
   (fingerprint of the pre-fix bug that double-wrote seq 0,1,2…; replaying a tangle
    is worse than losing it)
3. initialSeq = rows.reduce(max seq, 0) + 1
   (use reduce, NOT Math.max(...spread): a long session can exceed the arg-count limit)
4. markerIdx = LAST row where role==="system" AND
     (content.startsWith("[conversation compacted") OR content === CLEAR_MARKER)
5. working = markerIdx>=0 ? rows.slice(markerIdx+1)         // after compact/clear marker
                          : rows.filter(r => r.seq >= CONTROL_MESSAGE_COUNT)  // drop control rows 0,1,2
6. restoredMessages = dropOrphanToolCallTail(dropOrphanToolResults(working.map(recordToChatMessage)))
7. return { restoredMessages, initialSeq }
```

The marker row itself is skipped (it's a durable-log breadcrumb, not a model
message). A CLEAR marker as the last marker ⇒ working history is empty.

### 7.1 `recordToChatMessage(r)` (private)
```
m = { role: r.role, content: r.content === "" ? null : r.content }
if r.role==="assistant" && r.toolCallsJson: try m.tool_calls = JSON.parse(...) catch (drop calls, keep text)
if r.role==="tool" && r.toolCallId: m.tool_call_id = r.toolCallId
return m
```
Malformed tool-call JSON is dropped silently (one bad row never aborts a resume).

### 7.2 `dropOrphanToolResults(messages)` (private)
Single forward pass. Track every declared tool_call id (`m.tool_calls[].id`).
Filter out `role==="tool"` messages whose `tool_call_id` isn't in the declared
set (or has none). Non-tool messages always kept. Drops orphan tool results
Fireworks would reject (parent's `toolCallsJson` was malformed/lost).

### 7.3 `dropOrphanToolCallTail(messages)` (private)
Find the LAST assistant message with ≥1 tool_calls (`lastCall`). If none, return
unchanged. Collect tool_call_ids answered by `tool` messages after `lastCall`. If
every `tool_calls[].id` of that assistant message is answered ⇒ keep all; else
`messages.slice(0, lastCall)` (cut the incomplete trailing exchange). Only the
tail is checked — a mid-history break implies an already-unusable DB.

---

## 8. `maybeAutoCompact(signal?)` (private)

```
1. if estimateTokens(messages) <= AUTO_COMPACT_TOKEN_THRESHOLD(60000): return
2. if messages.length <= CONTROL_MESSAGE_COUNT+1 (=4): return  (no real history)
3. history = messages.slice(3).map(m => ({...m, content: contentToText(m.content)}))
   (flatten multimodal to text — small model is text-only; an image-bearing turn
    would otherwise trip the vision tier gate and silently fail EVERY auto-compact,
    growing history unbounded)
4. try:
     result = await router.chat("small", { messages: [
       {role:"system", content:"Summarize the conversation below in 2-3 sentences: the current goals, key decisions made, and any pending work. Be concise and factual."},
       ...history ], signal })
     summary = result.content.trim()
     if !summary: events.info("Auto-compact skipped: empty summary"); return
     compact(summary); events.info("Auto-compacted conversation")
   catch: events.info("Auto-compact skipped: summary failed")  (keep full history, continue)
```
Best-effort; any failure leaves the conversation untouched and the turn proceeds.

### 8.1 `estimateTokens(messages)` (private, dependency-free)
```
chars = sum over messages of:
  contentToText(m.content).length
  + sum over (m.tool_calls ?? []) of tc.function.arguments?.length ?? 0
return ceil(chars / CHARS_PER_TOKEN(4))
```
Counts content (multimodal flattened to text; base64 images collapse to
`[image omitted]` — NOT counted as base64) plus tool-call argument JSON length
(assistant tool-call turns carry `content:null` but big args). Approximate by
design.

---

## 9. `serializeToolResult(res, artifactStore?)` (exported)

```
payload = { ok, summary, result, error }
try s = JSON.stringify(payload)
catch s = JSON.stringify({ ok, summary })          // unserializable result/error
if s.length <= MAX_TOOL_RESULT_CHARS(8000): return s

// overflow path (DO NOT slice s mid-JSON — issue #78):
totalChars = s.length
totalBytes = Buffer.byteLength(s, "utf8")           // UTF-8 byte length
preview = s.slice(0, TRUNCATION_PREVIEW_CHARS(1500))
artifactId = undefined
if artifactStore:
  while artifactStore.size >= MAX_STORED_ARTIFACTS(64):
    oldest = first key (insertion order); if undefined break; delete it
  artifactId = "artifact_"+uuid8; artifactStore.set(artifactId, s)
note = artifactId
  ? `Output truncated to a ${preview.length}-char preview of ${totalChars} total. Call the artifact.read tool with artifactId "${artifactId}" (and offset/limit) to page through the full result.`
  : `Output truncated to a ${preview.length}-char preview of ${totalChars} total; the full result is not retrievable in this context.`
err = res.error  (as {code?, recoverable?})
return JSON.stringify({
  ok: res.ok,
  summary: res.summary.slice(0, TRUNCATION_SUMMARY_CHARS(500)),
  result: {
    truncated: true,
    ...(artifactId ? {artifactId} : {}),
    ...(err ? {errorCode: err.code, recoverable: err.recoverable} : {}),
    totalChars, totalBytes, preview, note
  }
})
```

**Wire-shape contract** (the model and `artifact.read` depend on it): the
truncated stub MUST be valid JSON with keys `ok`, `summary`, and a `result`
object containing `truncated:true`, optional `artifactId`, optional
`errorCode`/`recoverable`, `totalChars`, `totalBytes`, `preview`, `note`. Field
ordering inside `result` matters for the artifact-read round-trip test that
re-serializes a slice and checks it stays under the cap.

`artifactStore` is `Map<string,string>` with **insertion-order iteration** —
Go must use an ordered structure (slice of keys + map, or a linked map) to evict
oldest-first.

---

## 10. Wake / autonomous-turn contracts (`src/agent/wake.ts`)

These are NOT in loop.ts but are the tight coupling that constrains the
sentinel strings. Port together.

### 10.1 `WAKE_FAILURE_PREFIXES` (exact, order-insensitive)
A `send()` reply is a "non-result" iff it `startsWith` one of:
```
"Model unavailable:"
"Model error:"
"Tool projection failed:"
"Reached the tool-iteration limit"
"Stopped: called "
"Turn cancelled"
```
`isWakeFailureReply(reply)` = any prefix matches. **Every sentinel string
returned by `runTurn` must keep its exact prefix** or wake reactors will record
terminals as summarized on a failed turn (silently swallowing the real summary).

### 10.2 `isActionableWake(e: QueueEvent) => boolean`
`e.source === "terminal_watcher" && Boolean(e.target?.terminalId)`. Only a
terminal-watcher event with a real terminalId triggers an autonomous turn.

### 10.3 `buildWakePrompt(events, {alreadySummarized?: ReadonlySet<string>})`
Builds the internal nudge text fed as a **read-only** turn. Tracks
already-summarized terminal IDs (cross-burst + within-batch) so a terminal whose
lifecycle surfaces multiple events (`waiting_for_input` then `terminal_exited`)
is summarized once and later events are downgraded to one-line acks. Full string
templates are in wake.ts; reproduce them verbatim if porting wake (the
"(already reported …)" marker text is model-facing). The session-scoped
`alreadySummarized` set lives in the UI controller, NOT in `AgentSession`.

### 10.4 Read-only turn enforcement (in `runTurn`, §6.4)
- Tool LIST is filtered to `readOnlyToolNames()` (read-risk minus
  `skill.find`/`skill.load`).
- Dispatch ALSO enforces `allowedSet` — any call outside it is refused with
  `READ_ONLY_TURN` (recoverable:false), because `resolveWireName` can fall
  through to a raw name so list-filtering alone isn't sufficient.
- Skill re-selection is implicitly skipped (no pre-turn auto-selection exists at
  all; the model pulls skills via `skill.find`/`skill.load`, both withheld
  read-only).

---

## 11. `AgentEventSink` vocabulary (the event interface)

```ts
interface AgentEventSink {
  assistantStart(): void;                                  // a new round is about to stream
  assistantToken(token: string): void;                     // one streamed token
  assistantEnd(content: string, reasoning?: string): void; // final round, think-stripped content; reasoning = <think> block (empty for non-reasoning models)
  assistantCancelled(content: string): void;               // user abort mid-flight; content = partial streamed text (often "")
  toolCall(event: ToolCallEvent): void;
  toolResult(event: ToolResultEvent): void;
  error(message: string): void;                            // fatal-for-this-turn
  info(message: string): void;                             // informational
  usage?(event: AgentUsageEvent): void;                    // OPTIONAL — most sinks ignore it
}
```

`usage` is optional: a sink may omit it. `multiSink` fans `usage` separately with
its own `?.` guard.

### 11.1 `AgentUsageEvent`
Emitted once per streamed round, AFTER the model call returns and BEFORE the
assistant message is appended (so `contextTokens` reflects the prompt actually
sent).
```ts
{
  promptTokens: number;       // result.usage?.promptTokens ?? 0
  completionTokens: number;   // result.usage?.completionTokens ?? 0
  totalTokens: number;        // result.usage?.totalTokens ?? promptTokens+completionTokens
  cachedTokens?: number;      // result.usage?.cachedTokens
  contextTokens: number;      // estimateTokens(this.messages)  — context-pressure numerator
  contextThreshold: number;   // AUTO_COMPACT_TOKEN_THRESHOLD (60000) — denominator
  costUsd: number | undefined;// estimateCostUsd(model,prompt,completion,cached) — ONLY when result.usage present, else undefined ("no data")
  tier: string;               // "large"
  model: string;              // bareModelId(router.modelFor?("large") ?? "large")
}
```
`model` uses `bareModelId(...)` to strip the long Fireworks account path. When the
provider reports no usage, `costUsd` is left `undefined` so the UI shows "no data"
not a misleading `$0.000`.

---

## 12. `RunEventSink` — durable `run_events` persistence (events.ts)

`implements AgentEventSink`, ctor `(db: Db, ref: RunIdRef)`. Private state:
`seq=0`, `seqRunId=undefined`, `contentBuffer=""`.

### 12.1 Buffering rule (important)
Streamed tokens are NOT written one-row-each. They accumulate in `contentBuffer`
and flush as a single `assistant:content` row when the round ends (a tool call
begins, an error/usage fires, or a new round starts). This captures
**intermediate** assistant prose (text in the same round as a tool call) that
never reaches `assistantEnd` (which only fires on the final tool-free round) and
would otherwise be lost. The final round's content arrives via `assistantEnd` as
`assistant:end`; its streamed buffer is dropped to avoid duplication.

### 12.2 Method → row mapping
| Sink method | Action | Row type | Payload |
|---|---|---|---|
| `assistantStart` | flushContent() then write | `assistant:start` | none |
| `assistantToken` | `contentBuffer += token` | — | (buffered) |
| `assistantEnd(content,reasoning?)` | `contentBuffer=""`; write | `assistant:end` | `reasoning ? {content,reasoning} : {content}` (omit empty reasoning) |
| `assistantCancelled(content)` | `contentBuffer=""`; write | `assistant:cancelled` | `{content}` |
| `toolCall(e)` | flushContent(); write | `tool:call` | `{id, name, args}` |
| `toolResult(e)` | write | `tool:result` | `{id, name, ok: result.ok, summary: result.summary, auditId: result.auditId}` |
| `error(msg)` | flushContent(); write | `error` | `{message}` |
| `info(msg)` | write | `info` | `{message}` |
| `usage(e)` | flushContent(); write | `usage` | all AgentUsageEvent fields (promptTokens, completionTokens, totalTokens, cachedTokens, contextTokens, contextThreshold, costUsd, tier, model) |
| `flushContent()` | if buffer non-empty: emit + clear | `assistant:content` | `{content}` |

### 12.3 `write(type, payload?)`
```
runId = ref.current; if !runId: return  (emitted outside a run)
if runId !== seqRunId: seqRunId = runId; seq = 0   (new run resets monotonic seq)
try db.insertRunEvent({ runId, seq: seq++, type, payload: payload===undefined?undefined:serializePayload(payload) })
catch: swallow  (durable logging must never break a live turn)
```

### 12.4 `serializePayload(payload)` (private, events.ts)
```
try json = JSON.stringify(payload) ?? "null"
catch return JSON.stringify({error:"unserializable"})
if json.length <= MAX_RUN_EVENT_PAYLOAD(8000): return json
return JSON.stringify({ truncated:true, bytes: Buffer.byteLength(json,"utf8"),
                        preview: json.slice(0, MAX_RUN_EVENT_PAYLOAD-200 = 7800) })
```

### 12.5 `multiSink(...sinks)`
Returns an `AgentEventSink` whose every required method loops the sinks, each
call wrapped in try/catch (one sink's failure can't starve others). `usage` is
fanned separately with a per-sink `?.` guard (it's optional on the interface).

---

## 13. External contracts that MUST stay compatible

### 13.1 SQLite tables/columns (read by/written by this subsystem)

**`conversation`** (via `db.insertMessage` / `db.listMessages`):
columns `id, sessionId, seq, role, content, toolCallsJson, toolCallId, createdAt`.
`ConversationMessageRecord`:
```
id: string ("msg_<uuid8>")
sessionId: string
seq: number
role: "system" | "user" | "assistant" | "tool"
content: string          (image parts persisted as "[image omitted]", never base64)
toolCallsJson?: string   (JSON of ToolCallRequest[])
toolCallId?: string
createdAt: number        (ms epoch)
```
`listMessages` returns rows `ORDER BY seq`.

**`run_events`** (via `db.insertRunEvent`): columns
`id, runId, seq, ts, type, payload`. `RunEventRecord`:
```
id: "rne_<uuid8>"
runId: string
seq: number      (monotonic within run, starts at 0)
ts: number
type: string     (e.g. "assistant:start","tool:call","tool:result","assistant:content","assistant:end","assistant:cancelled","error","info","usage")
payload?: string (JSON, absent for zero-payload events)
```

**`skill_selection_log`** (via `db.insertSkillSelection`): columns
`id, ts, sessionId, userInput, selectedSkillIdsJson, confidence, taskType, reason`.
`SkillSelectionLogRecord` mirrors these (`id` = `rsl_<uuid8>`).

### 13.2 JSONL event-type vocabulary (`schemas.ts` `JsonlEventType`)
The one-shot `--json` stream reuses the SAME `RunEventRecord.type` strings:
```
assistant:start, assistant:content, assistant:end, assistant:cancelled,
tool:call, tool:result, error, info, result
```
(`result` is the extra terminal line unique to the JSONL stream — not a
RunEventSink type.) `JsonlEventSchema` requires `{type, ts, seq>=0}` +
passthrough. Keep these strings byte-identical across the durable log and the
JSONL stream.

### 13.3 One-shot exit codes (`ONE_SHOT_EXIT_CODE`)
`{ success:0, error:1, cancelled:2, toolFailure:3 }`. `JSON_OUTPUT_SCHEMA_VERSION
= 1`. `toolFailure(3)` is RESERVED (the loop has no terminal tool-failure signal
today: failed tool calls are recoverable context and the turn continues). A turn
ending after a tool error still exits 0.

### 13.4 Prompt cache key
`MAIN_PROMPT_CACHE_KEY = "daintree-main"` passed as `promptCacheKey` on every
large-model stream. Plain, unversioned. `BASE_SYSTEM_PROMPT` (messages[0]) is the
cached prefix — keep it byte-stable; dynamic facts live in [1]/[2].

### 13.5 `ToolResult` envelope (`schemas.ts`)
```ts
interface ToolError { code: string; message: string; recoverable: boolean; details?: unknown }
interface ToolResult<T=unknown> { ok: boolean; result?: T; error?: ToolError; summary: string; auditId?: string }
```
Synthetic results the loop constructs:
- Parse failure: `{ok:false, summary:"Invalid JSON arguments for <name>; not executed.", error:{code:"INVALID_TOOL_ARGS_JSON", message:"Arguments were not valid JSON.", recoverable:true}}`
- Read-only refusal: `{ok:false, summary:"<name> is not available on an autonomous read-only turn.", error:{code:"READ_ONLY_TURN", message:"Mutating tools are disabled on autonomous wake-up turns; only read-only inspection is allowed.", recoverable:false}}`
- Cancel stub: `{ok:false, summary:"Turn cancelled before this tool was executed.", error:{code:"CANCELLED", message:"Turn cancelled.", recoverable:false}}`

### 13.6 `RiskClass` enum (drives `readOnlyToolNames`)
`["read","local","ui","terminal","project","git","external","system"]`. Read-only
turn offers only `risk==="read"` tools, minus `skill.find`/`skill.load`.

### 13.7 `ChatMessage` / `ToolCallRequest` wire shapes (`fireworks.ts`)
```ts
interface ToolCallRequest { id: string; type: "function"; function: { name: string; arguments: string } }
type ChatContentPart = {type:"text", text:string} | {type:"image_url", image_url:{url:string}}
interface ChatMessage {
  role: "system"|"user"|"assistant"|"tool";
  content: string | ChatContentPart[] | null;
  tool_calls?: ToolCallRequest[];
  tool_call_id?: string;
  name?: string;
}
```
`contentToText(content)`: `null→""`; string→itself; array → parts joined by `\n`,
text parts as-is, image parts → `"[image omitted]"`.

`ChatResult` (from `router.stream`/`router.chat`):
`{ content, reasoning, toolCalls: ToolCallRequest[], finishReason, usage?: {promptTokens?, completionTokens?, totalTokens?, cachedTokens?} }`.

Error classes: `CancelledError{code:"CANCELLED"}`, `FireworksUnavailableError{code:"FIREWORKS_UNAVAILABLE"}`.

### 13.8 Skill types
```ts
SkillSelection = { skillIds: string[] (max 3); confidence: number [0,1]; reason: string; taskType: string }
SkillFindResult = {
  ok: boolean; matched: boolean; query: string; reason: string; confidence: number;
  selected: { id: string; title: string; summary: string }[];
  activeSkillIds: string[];
}
RenderedSkillBundle = { ids: string[] (sorted); hash: string (12-char sha256 over "id@version|..."); cacheKey: string ("daintree-main-v1-skills-"+hash); items: Skill[] }
```
`renderSkillBundle([])` for the empty bundle. Bundle is sorted by id; hash is
`sha256(sorted.map("id@version").join("|")).hex.slice(0,12)`.

---

## 14. `findSkills(query, signal?)` (the `skill.find` engine)

```
1. try selection = await selectSkills({router, candidates: skillRegistry.metadataForSelection(), query, signal})
   catch: return {ok:false, matched:false, query, reason:"skill selector unavailable", confidence:0, selected:[], activeSkillIds:[...activeSkillIds]}
2. if signal.aborted: return {ok:false, matched:false, query, reason:"cancelled", confidence:0, selected:[], activeSkillIds:[...activeSkillIds]}
   (don't mutate the live skill set with an abandoned result)
3. newlyKnown = resolveKnownIds(selection.skillIds)            (this query's resolved ids, hallucinations dropped)
4. merged = resolveKnownIds([...selection.skillIds, ...activeSkillIds])   (new ids FIRST so they survive cap-of-3)
5. applySkillBundle(skillRegistry.getMany(merged))            (rewrites messages[2])
6. logSelection(query, selection, newlyKnown)                (log what the QUERY resolved, NOT the merged set)
7. selected = getMany(newlyKnown).map(r => ({id, title, summary}))
8. return {ok:true, matched: selected.length>0, query, reason: selection.reason, confidence: selection.confidence, selected, activeSkillIds:[...activeSkillIds]}
```
`logSelection` records `newlyKnown` (not merged) so a no-match/all-hallucinated
selection isn't logged as if it loaded the existing set.

`loadAdditionalSkills(ids)`: `merged = resolveKnownIds([...ids, ...activeSkillIds])`,
apply, return `[...activeSkillIds]`. New ids first ⇒ an explicit load evicts the
lowest-priority prior skill rather than being dropped.

---

## 15. Concrete Go mapping proposal

### 15.1 Packages
- `internal/agent` — `Session` (was `AgentSession`), `runTurn`, `serializeToolResult`, `rehydrateSession`, sentinels.
- `internal/agent/events` (or same package) — `EventSink` interface, `NoopEventSink`, `MultiSink`, `RunEventSink`, `RunIDRef`.
- `internal/agent/wake` — `IsActionableWake`, `BuildWakePrompt`, `IsWakeFailureReply`, `wakeFailurePrefixes`.

### 15.2 Key Go types/interfaces
```
type EventSink interface {
  AssistantStart()
  AssistantToken(token string)
  AssistantEnd(content, reasoning string)   // pass "" for no reasoning; omit in payload when empty
  AssistantCancelled(content string)
  ToolCall(ToolCallEvent)
  ToolResult(ToolResultEvent)
  Error(message string)
  Info(message string)
  Usage(UsageEvent)                         // make non-optional; NoopEventSink no-ops it
}
```
- `usage?` optional method → in Go make `Usage` a required interface method and
  have noop/adapters no-op it (Go has no optional interface methods). `MultiSink`
  still wraps each call in `recover()`-free try-equivalent — but since panics are
  the analog, wrap each fan-out call so one sink's panic can't kill the loop
  (`func(){ defer recover(); sink.X() }()`), matching the TS try/catch isolation.
- `RunIDRef` → `*atomic.Pointer[string]` or a small `struct{ mu sync.Mutex; current *string }`. The TS rationale ("single-flight, one app one session") holds; a plain pointer guarded by the session's single-goroutine assumption is fine, but document it. If concurrency is introduced, make it per-call (context value).
- `Session` holds `messages []ChatMessage`, `seq int`, `activeSkillIDs []string`, `skillBundle RenderedSkillBundle`, `skillCatalog string`, `events EventSink`, `deps SessionDeps`.
- `AbortSignal` → `context.Context`. `signal.aborted` → `ctx.Err() != nil`. Pass `ctx` to `router.Stream`/`Chat` and into `runCtx`. `CancelledError` → check `errors.Is(err, context.Canceled)` plus a typed `CancelledError` from the router. Replace `addEventListener("abort")` patterns with `ctx.Done()`.
- artifact store ordered map → either `[]string` keys + `map[string]string`, or a small ordered-map type. Eviction = drop `keys[0]`.

### 15.3 Stdlib / 3rd-party
- `encoding/json` — `JSON.stringify`/`JSON.parse`. **Caveat**: Go `json.Marshal`
  sorts struct fields by declaration order (fine) but maps by key (TS preserves
  insertion). For the truncation stub and run-event payloads use **structs**, not
  maps, to keep field order deterministic and matching the TS shape. For optional
  fields (`artifactId`, `errorCode`) use `omitempty` or `*T`.
- `crypto/sha256` + `encoding/hex` — bundle hash (slice first 12 hex chars).
- `github.com/google/uuid` — `uuid.NewString()[:8]` for `run_`/`artifact_` ids.
- `unicode/utf8` `utf8.RuneCountInString` is NOT what `Buffer.byteLength(s,"utf8")`
  returns — that's **byte** length. Use `len(s)` (Go strings are UTF-8 bytes) for
  `totalBytes`/`bytes`. For char counts (`s.length`, `.slice`), TS `.length` is
  **UTF-16 code units**; for ASCII/BMP-heavy tool JSON the difference is rare but
  real. Safest faithful port: operate on UTF-16 semantics only where slicing
  user-visible previews; for `totalChars` document that we use rune count (Go) vs
  UTF-16 units (TS) — pick rune count and note the tiny divergence, OR replicate
  UTF-16 by encoding to `utf16.Encode`. **Recommend rune-based slicing** for
  `preview`/`summary` slices to avoid splitting a multibyte rune (TS `.slice` can
  split a surrogate pair; Go rune slicing is safer and acceptable here).
- `database/sql` + a SQLite driver (the rewrite's storage layer) for the three
  tables. Match column names exactly.
- `math` — `math.Ceil` for `estimateTokens`.

### 15.4 Error/sentinel handling
`send` returns `(string, error)` is tempting, but the TS contract is **send never
returns an error for model/tool failures — it returns a sentinel string** that
wake reactors prefix-match. Preserve this: `Send(ctx, input, opts) string`
returning the sentinel strings verbatim (or `(string, nil)` always, reserving
`error` only for truly unexpected panics). The `WAKE_FAILURE_PREFIXES` matching
MUST stay string-prefix based.

---

## 16. DELETE / do-not-port (Node/Bun/React/OpenTUI-specific)

- **`noopAgentEvents`'s `usage(){}`** stays (it's vocabulary, not platform), but
  the TS optional-method machinery (`Exclude<keyof…,"usage">`, `Parameters<…>`
  generic `fan` helper in `multiSink`) is TypeScript type gymnastics — replace
  with a straightforward Go fan-out, no type-level code.
- **`Buffer.byteLength`** → `len(s)` (see §15.3). Don't port `Buffer`.
- **`randomUUID` from `node:crypto`** → `google/uuid`. Don't port the Node import.
- The comment about `AsyncLocalStorage` vs plain object (RunIdRef) — informational
  only; in Go use a pointer/atomic and keep the single-flight note.
- Nothing in this subsystem touches React/OpenTUI/Ink directly — the event sink is
  the boundary; the UI consumer (Bubble Tea model) is a SEPARATE port. Don't pull
  any rendering concern in here. The `events.ts` header comment mentioning "Ink
  UI" / "console renderer" is stale narrative; the contract is the sink interface.
- `selectSkills`, `renderSkillBundle`, `buildRuntimeContextMessage`,
  `buildLoadedSkillsMessage`, `buildSkillCatalogMessage`, `BASE_SYSTEM_PROMPT`,
  `bareModelId`, `estimateCostUsd` are **dependencies in other subsystems** — do
  NOT reimplement here; call the ported versions. They're out of scope for this
  file but their signatures (as used) are documented inline above.

---

## 17. Porting risks / non-obvious contracts (preserve these)

1. **Cancel leaves NO orphan turn**: the two `signal.aborted` re-checks (entry and
   post-auto-compact, BEFORE pushing the user message) are load-bearing (issue
   #61 pull-back). Don't collapse them.
2. **Every tool_call gets a tool reply**: on mid-batch cancel, push a CANCELLED
   stub for each remaining call. Otherwise the persisted transcript is invalid for
   Fireworks on the next resume.
3. **Circuit-breaker signature uses the RAW argument string** the model emitted
   (`call.function.arguments`), not re-serialized parsed args, plus the error
   code. Re-encoding would change byte-identity and break the "identical" check.
4. **Dup-seq detection** in `rehydrateSession` (start fresh on non-unique seqs) is
   the guard against a historical double-write bug — keep it.
5. **`initialSeq` uses reduce-max, not spread-max** — a long session can exceed
   the call-argument limit; in Go this is moot but keep the O(n) reduce.
6. **Control messages rebuilt-but-not-re-persisted on resume** — persisting them
   again would corrupt seq numbering and the durable snapshot.
7. **Marker detection**: compaction marker matched by PREFIX `"[conversation
   compacted"`, clear marker by EXACT equality with `CLEAR_MARKER`. Preserve the
   em-dash characters.
8. **`costUsd` undefined vs 0**: only compute cost when the provider reported
   usage; otherwise leave it nil so the UI shows "no data".
9. **Truncation stub is valid JSON** (issue #78): never slice the serialized
   string mid-structure. The artifact-read round-trip depends on it.
10. **RunEventSink content buffering**: intermediate prose is flushed as
    `assistant:content`; final-round prose comes via `assistant:end` and the
    buffer is dropped to avoid duplicating it. Get the flush points right
    (start/toolCall/error/usage flush; assistantEnd/Cancelled drop).
11. **`run_events.seq` resets per run** (when `ref.current` changes); monotonic
    within a run from 0.
12. **Read-only enforcement is double-gated** (tool list filter AND dispatch-time
    `allowedSet`) because `resolveWireName` can fall through to a raw name.
13. **All persistence is best-effort** (insertMessage / insertRunEvent /
    insertSkillSelection / RunEventSink.write swallow errors) — a DB failure must
    never break a live turn.
14. **Sentinel strings are an API** consumed by `wake.ts` via prefix match — any
    wording change to "Model unavailable:", "Model error:", "Tool projection
    failed:", "Reached the tool-iteration limit", "Stopped: called ",
    "Turn cancelled" must update both sides in lockstep.
15. **`buildToolFilter` returns `undefined` (full registry) when no skills are
    active** — an unconstrained turn must not be starved of tools. Don't replace
    with an empty slice (which would offer zero tools).
16. **Skill cap-of-3 with new-ids-first ordering** in `findSkills`/`loadAdditionalSkills`
    so an explicit/new load evicts the lowest-priority prior skill rather than
    being dropped. `resolveKnownIds` filters unknown ids BEFORE the slice.
