# Port spec: `src/tools/*` — the tool families

Authoritative reference for porting the assistant's **tool subsystem** to Go.
Every dotted tool name, its risk class, its JSON arg schema, and its return shape
is a hard wire/schema contract — the Go port MUST reproduce them byte-for-byte
(tool names go on the OpenAI/Fireworks wire; arg JSON-schemas are sent to the
model; risk classes drive the safety gate; result envelopes are persisted to the
audit log).

Source files covered: `types.ts`, `registry.ts`, `index.ts`, and the 14 tool
family modules — `fsTools`, `mcpTools`, `timerTools`, `watcherTools`,
`queueTools`, `contextTools`, `extractionTools`, `agentTaskTools`, `grantTools`,
`workflowTools`, `skillRunTools`, `auditTools`, `memoryTools`, `artifactTools`.
Cross-cutting types live in `src/schemas.ts`; the safety gate in
`src/safety/policy.ts`; cadence constants in `src/watcherCadence.ts`.

---

## 1. Framework contracts (`types.ts`, `registry.ts`, `schemas.ts`)

### 1.1 Core enums (`schemas.ts`) — exact value sets (wire-critical)

| Enum | Values (exact, ordered) |
|---|---|
| `RiskClass` | `read`, `local`, `ui`, `terminal`, `project`, `git`, `external`, `system` |
| `Tier` | `supervisor`, `operator`, `system` |
| `ModelTier` | `small`, `medium`, `large` |
| `AgentState` | `idle`, `working`, `waiting`, `directing`, `completed`, `exited` |
| `Severity` | `debug`, `info`, `attention`, `urgent`, `blocked`, `done`, `error` |
| `EventSource` | `timer`, `terminal_watcher`, `worktree_watcher`, `pr_watcher`, `workflow`, `model_worker`, `system`, `user` |
| `EpistemicKind` | `observed`, `inferred`, `unverified` |
| `WatcherClassification` | `no_change`, `still_working`, `waiting_for_input`, `permission_prompt`, `command_failed`, `tests_failed`, `tests_passed`, `merge_conflict`, `completed_success`, `completed_unverified`, `completed_unknown`, `terminal_exited`, `rate_limited`, `needs_large_model`, `unknown` |
| `VerificationVerdict` | `verified`, `failed`, `unknown` |

> `RiskClass` order in `schemas.ts` lists `git` before `external`; the safety
> `ALWAYS_CONFIRM`/`TIER_ALLOWED` sets list `external` before `git`. Order is not
> semantically meaningful (they are Sets) — only membership matters.

### 1.2 `ToolResult<T>` envelope (the universal return type)

```
ToolResult<T> {
  ok: boolean
  result?: T            // present on success (may be omitted)
  error?: ToolError     // present on failure
  summary: string       // one-line human/LLM-facing
  auditId?: string      // id of the audit_log row, stamped after dispatch
}
ToolError { code: string; message: string; recoverable: boolean; details?: unknown }
```

Helpers (`types.ts`):
- `ok<T>(summary, result?) → ToolResult<T>` → `{ ok:true, summary, result }`.
- `fail(code, message, opts?) → ToolResult` → `{ ok:false, summary:message, error:{ code, message, recoverable: opts.recoverable ?? true, details: opts.details } }`. **Default `recoverable` is `true`.**
- `NO_ARGS` = `{ type:"object", properties:{}, additionalProperties:false }` (shared empty-args JSON-schema).

Go mapping: `type ToolResult[T any] struct {...}` is awkward with generics in
audit; prefer `ToolResult { OK bool; Result any; Error *ToolError; Summary string; AuditID string }`. Keep `Ok(summary, result)` / `Fail(code, msg, opts)` constructors.

### 1.3 `ToolDef` + `ToolContext`

`ToolDef<A>`:
| field | type | notes |
|---|---|---|
| `name` | string | dotted internal name (`fs.read`) |
| `description` | string | model-facing, can be long |
| `risk` | `RiskClass` | drives the gate |
| `consequence?` | string | short human-facing line for the approval sheet; UI falls back to a per-risk phrase when absent |
| `parameters` | JSON-schema object | sent to the model as OpenAI function `parameters` |
| `schema?` | Zod schema | runtime validation of parsed args |
| `handler` | `(args, ctx) => Promise<ToolResult>` | never throws to caller |

`ToolActor` = `"main" | "watcher" | "timer" | "workflow" | "system"`.

`ToolContext` (built once at startup; per-turn fields layered via a derived ctx).
Fields the handlers actually read (port all):
- `config` (AppConfig: `tier`, `autoApprove`, model env, debug-log cfg, …)
- `mcp` (DaintreeMcpClient: `isConnected()`, `status()`, `callTool(name,args,signal)`, `listTools(force?,signal)`)
- `db` (the SQLite store — every persistence method below)
- `queue` (`publish(args)`, `digest(filters)`, `format(events)`, `resolve(id)`)
- `router` (`chat(tier,opts)`, `json(tier,opts,schema)`)
- `projectPath` (string)
- `sessionId?`, `actor`, `actorId?` (`wch_…`/`tmr_…`), `runId?`, `signal?` (AbortSignal)
- `activeToolNames?` (string[]; `undefined` ⇒ unconstrained — used by discovery tools' `callable` flag)
- `confirm(req) => Promise<bool>`, `log(msg)`, `daemonActive?() => bool`
- `artifactStore?` (Map<string,string> — session-scoped oversized results)
- `skillSource?` (`get(id)` → `{id,title,summary}` | undefined)
- `loadSkills?(ids) => string[]`, `findSkills?(query,signal) => Promise<SkillFindResult>`

`ConfirmRequest` = `{ toolName, risk, summary, args, consequence? }`.

Go mapping: `ToolContext` becomes a struct of interfaces (`MCPClient`, `DB`,
`Queue`, `Router`, `SkillSource`) + funcs (`Confirm func(ConfirmRequest)(bool,error)`,
`LoadSkills func([]string)[]string`, `FindSkills func(string, context.Context)(SkillFindResult,error)`).
`signal AbortSignal` → `context.Context`. `daemonActive` → `func() bool`.

### 1.4 Registry behavior (`registry.ts`) — MUST preserve exactly

- **Wire-name aliasing.** Internal names use dots; OpenAI/Fireworks function names
  must match `^[a-zA-Z0-9_-]{1,64}$`. `toWireName` = `name.replaceAll(".", "__")`.
  `resolveWireName` maps a model's wire name back to the internal dotted name.
  Alias maps are rebuilt on every `toOpenAITools(filterNames?)` projection.
  **Projection throws** on an illegal wire name or a wire-name collision (fail-fast).
- `register` throws on duplicate name. `assertSafe()` runs `assertNoFileEditTools`
  over all registered names at startup (see §9).
- **`dispatch(name, rawArgs, ctx)` ordering (never throws; always audits):**
  1. unknown tool → `fail("UNKNOWN_TOOL", …, recoverable:false)`, audit, return.
  2. args = `rawArgs ?? {}`. If `schema`, `safeParse`; on failure →
     `fail("INVALID_ARGS", "Invalid arguments for <name>: <joined issues>", recoverable:true, details:issues)`. Issue formatting via `summarizeIssue` (see below).
  3. `decide(tool.risk, config.tier)`; if not allowed → `fail("TIER_DENIED", reason, recoverable:false)`, audit outcome `"denied"`.
  4. If `decision.needsConfirmation`:
     - **actor ≠ "main"** (non-interactive): if `actorId` set, try
       `db.consumeGrant(actorId, actor, name, risk, started)` atomically; on a hit
       run handler with audit outcome `"grant_ok"` (stamping `grantSource`/`grantId`).
       Otherwise → `fail("CONFIRMATION_REQUIRED", …, recoverable:false)`, audit
       `"denied"`, **and publish** a `queue.publish` info event titled
       `"Autonomous action blocked: <name>"` with `dedupeKey =
       "denied:<actor>:<actorId?:>:<name>"` (wrapped in try/catch — surfacing must
       never break the call).
     - **actor == "main"** and `config.autoApprove` → run handler straight through
       (audited as normal `"ok"`).
     - else `await ctx.confirm({toolName,risk,summary:tool.description,consequence,args})`
       (a thrown prompt = decline). Not approved → `fail("USER_DECLINED", …, recoverable:true)`, audit `"denied"`.
  5. `runHandler`: on success audit outcome (`"ok"` or `"grant_ok"`); a returned
     `!ok` result audits `"error"`; a **thrown** handler →
     `fail("TOOL_THREW", message, recoverable:true)`, audit `"error"`. Never throws.
- **Audit row.** Always written (try/catch — auditing must never break a call).
  Also emits a full-fidelity `logDebug(config, "tool.call", {...})` trace (untruncated
  args/result). Audit JSON is capped at **`MAX_AUDIT_JSON = 4000`** bytes via
  `capJson` → `{ truncated:true, bytes:<utf8 byte len>, preview:<first 3800 chars> }`.
  `safeJson` returns `"null"` on stringify failure and `'"<unserializable>"'` on throw.
- `summarizeIssue`: for a Zod `invalid_union` with sub-errors, collect the
  discriminating keys each branch wanted **at the union's own depth** and emit
  `"<path>: the value matched none of the allowed shapes — provide an object with
  exactly one of these keys: <sorted keys>"`. This is load-bearing: it unstuck a
  watcher re-issue loop on `stopWhen: {}`. Everything else → `"<path>: <message>"`.
  Go: replicate against your JSON-schema validator's union/oneOf failure.

Outcome enum on audit rows: `ok | error | denied | dedup | grant_ok`.

### 1.5 Registry assembly (`index.ts`)

`buildAllTools()` concatenates, **in this order**: fs, mcp, timer, watcher, queue,
context, extraction, agentTask, grant, workflow, skillRun, audit, memory, artifact.
Order only affects list iteration (and thus model presentation when unfiltered).

---

## 2. Safety gate (`safety/policy.ts`) — drives every dispatch

- `TIER_ALLOWED`: `supervisor` = {read, local, ui}; `operator` = {read, local, ui,
  terminal, project, external}; `system` = all eight.
- `ALWAYS_CONFIRM` = {terminal, project, git, external, system}. (`read`/`local`/`ui`
  never confirm.)
- `decide(risk, tier, {hasScopedApproval?})`: not-allowed → `{allowed:false,
  needsConfirmation:false, reason:"'<risk>' actions require a higher tier than
  '<tier>'. Switch tier with /permissions."}`; else `{allowed:true,
  needsConfirmation: ALWAYS_CONFIRM.has(risk) && !hasScopedApproval}`.
- `FORBIDDEN_TOOL_FRAGMENTS` (substring, lowercased): `write_file`, `writefile`,
  `apply_patch`, `applypatch`, `edit_file`, `editfile`, `fs.write`, `fs.edit`,
  `file.write`, `file.edit`, `patch.apply`. `isForbiddenToolName` + `assertNoFileEditTools`.
- **Secret-file guard** (used by fs tools):
  - `SECRET_BASENAMES`: `.env`, `.envrc`, `.npmrc`, `.netrc`, `.pgpass`,
    `.htpasswd`, `credentials`, `credentials.json`, `id_rsa`, `id_dsa`, `id_ecdsa`,
    `id_ed25519`, `.dockercfg`, `.git-credentials`, `secrets.json`, `secrets.yaml`,
    `secrets.yml`, `service-account.json`, `serviceaccount.json`.
  - `SECRET_SUFFIXES`: `.pem`, `.key`, `.p12`, `.pfx`, `.keystore`, `.jks`, `.asc`,
    `.gpg`, `.ppk`.
  - `SECRET_DIR_SEGMENTS`: `.ssh`, `.aws`, `.gnupg`, `.kube`, `.azure`, `.gcloud`, `.docker`.
  - `isSensitiveSegment(seg)`: in `SECRET_DIR_SEGMENTS`, OR `seg === ".env"`, OR
    `endsWith(".env")`, OR `startsWith(".env.")`.
  - `isSensitivePath(p)`: lowercase; basename in `SECRET_BASENAMES` or ends with a
    secret suffix; OR any path segment is sensitive.
- `resolveInsideProject(projectPath, rel)`: resolve against root, assert lexical
  containment, then assert symlink-resolved containment (realpath of nearest
  existing ancestor on both sides). Throws `FileEditAttemptError("Path escapes the
  project root: <rel>")`. Go: `filepath.Abs`/`Clean` + `filepath.EvalSymlinks` on
  nearest existing ancestor; ensure `target == root || strings.HasPrefix(target, root+sep)`.

---

## 3. Cadence constants (`watcherCadence.ts`)

| Const | Value | Use |
|---|---|---|
| `SCHEDULER_TICK_MS` | `3_000` | scheduler tick; floor for supervisor cadence |
| `SUPERVISOR_DEFAULT_CADENCE_MS` | `3_000` | supervisor watchers (CLI-spawned workers) |
| `MONITOR_DEFAULT_CADENCE_MS` | `120_000` | user-created background watchers (default `watcher.terminal.create` cadence) |
| `PR_WATCHER_CADENCE_MS` | `60_000` | fixed pr_state cadence (not user-configurable) |
| `WATCHER_SPAWN_GRACE_MS` | `20_000` | grace before an absent target reads as exited |

---

## 4. Tool inventory by file

The full registered set is **~75 tools**. Risk class and arg-required fields are
contract. Unless noted, every `parameters` object is
`{type:"object", additionalProperties:false, properties:{…}, required:[…]}`.

The remaining sections (§4.1–§4.14) enumerate each tool. Subsequent pieces of this
document cover them file-by-file.

### 4.1 `fsTools.ts` — read-only project filesystem (risk `read`)

Constants: `SKIP_DIRS` = {`.git`, `node_modules`, `dist`, `build`, `coverage`,
`.next`, `.turbo`, `.cache`, `vendor`}. `DEFAULT_MAX_BYTES = 200_000` (single
read/per-file search cap). `SEARCH_MAX_FILE_BYTES = 1_000_000` (files bigger are
skipped by search). `looksBinary(buf)`: NUL byte in first chunk OR >30% control
bytes in first min(len,4096) → binary (allow tab 9, LF 10, CR 13, printable ≥32).

| Tool | risk | args (Zod) | returns `result` |
|---|---|---|---|
| `fs.list` | read | `path?:string`, `depth?:int (1..10)` | `{path, depth, entries:[{name,type:"file"\|"dir"}]}` sorted by name (localeCompare). default path=".", depth=1. |
| `fs.read` | read | `path:string` (required), `maxBytes?:int (1..200_000)` | `{path, content, bytes, truncated}` |
| `fs.search` | read | `query:string (min 1)` (required), `glob?:string`, `maxResults?:int (1..500)` | `{query, glob, capped, matches:[{file,line,text}]}` |

Behavior (all): resolve via `resolveInsideProject`; refuse sensitive paths with
`fail("FS_SENSITIVE", …, recoverable:false)` — `fs.list`/`fs.read` re-check the
**realpath** (symlink target) too. `fs.read`: byte-aware read (open + read ≤limit),
not-a-file → `fail("FS_READ", recoverable:false)`, binary → `fail("FS_BINARY",
recoverable:false)`; `limit = min(maxBytes ?? 200_000, 200_000)`. `fs.search`:
pure recursive walk skipping `SKIP_DIRS` and sensitive segments (also pruned at
walk time, guarding symlinks); `glob` is a filename **suffix** filter (`endsWith`);
match text trimmed + sliced to 300 chars; line numbers 1-based; default maxResults
50; `capped` true when limit hit. Error codes: `FS_LIST`, `FS_READ`, `FS_SEARCH`,
`FS_SENSITIVE`, `FS_BINARY`.

Go: `os`/`io/fs`, `filepath.WalkDir`. Reproduce the binary sniff and 300-char/300k
caps exactly. The `walkFiles` SKIP_DIRS + sensitive-segment pruning must match.

### 4.2 `artifactTools.ts` — oversized-result paging (risk `read`)

Constants: `DEFAULT_READ_CHARS = 3500`, `MAX_READ_CHARS = 3500` (so one read can't
re-overflow `MAX_TOOL_RESULT_CHARS = 8000`; JSON re-escaping ≈ 2×).

| Tool | risk | args | returns |
|---|---|---|---|
| `artifact.read` | read | `artifactId:string (min 1)` req, `offset?:int (≥0)`, `limit?:int (1..3500)` | `{artifactId, offset, limit, totalChars, content, nextOffset, eof}` |

No `ctx.artifactStore` → `fail("ARTIFACT_UNAVAILABLE", recoverable:false)`. Missing
id → `fail("ARTIFACT_NOT_FOUND", recoverable:false)`. offset clamped to
`[0,totalChars]`; limit clamped to `MAX_READ_CHARS`; `eof = nextOffset >= totalChars`.
**Character (not byte) slicing** — Go must slice on runes to match JS string indexing
behavior for the chars the model sees (UTF-16 vs runes is a known divergence; the
stored artifact is JSON text, usually ASCII-heavy — acceptable, but note it).

### 4.3 `timerTools.ts` — durable timers

`TimerPayload` discriminated union on `type`: `{type:"enqueue", message?:string}`
OR `{type:"call_safe_tool", toolCall:{toolName:string(min1), args?:record}}`.
**`run_check` is no longer creatable** (legacy rows still deserialize and fire as a
plain reminder). `TimerTarget` = `{projectId?,worktreeId?,terminalId?,workflowRunId?}` (strict).

| Tool | risk | args | returns |
|---|---|---|---|
| `timer.schedule` | local | `title:string` req, `fireAt?:ISO-8601 datetime`, `delayMs?:int>0`, `repeat?:{everyMs:int>0 req, maxRuns?:int>0, until?:ISO datetime}`, `payload:TimerPayload` req, `target?:TimerTarget` | `{timerId, fireAt:ISO, daemonActive}` |
| `timer.list` | read | `{}` | `{timers:[{id,title,fireAt:ISO,repeatEveryMs,maxRuns,runCount,payloadType}]}` (status `scheduled` only) |
| `timer.cancel` | local | `id:string` req | `{timerId, status:"cancelled"}` |

`timer.schedule`: `fireAt = Date.parse(fireAt) ?? now+delayMs ?? NaN`; NaN →
`fail("TIMER_FIRE_AT", recoverable:false)`. Invalid `repeat.until` →
`fail("TIMER_REPEAT_UNTIL", recoverable:false)`. Always append a foreground-only
lifecycle NOTE to the summary (different text when `daemonActive()` is false).
`timer.cancel`: not found → `fail("TIMER_NOT_FOUND", recoverable:false)`; sets
status cancelled AND `db.revokeGrantsByActor(id)` (a cancelled timer keeps no grant).

DB methods: `insertTimer({title,fireAt,repeatEveryMs?,repeatUntil?,maxRuns?,
payloadType,payloadJson,targetJson?})`, `listTimers("scheduled")`, `getTimer(id)`,
`updateTimer(id,{status})`, `revokeGrantsByActor(id)`.

### 4.4 `queueTools.ts` — attention queue

`QueuePublishArgs` (strict) = `{source:EventSource, severity:Severity, title,
summary, target?:EventTarget, evidence?:string[], recommendedActions?:[{label,
toolName, args?, risk?, requiresConfirmation?}], dedupeKey?, ttlMs?:number,
epistemicKind?:EpistemicKind}`.

| Tool | risk | args | returns |
|---|---|---|---|
| `queue.publish` | local | `QueuePublishArgs` (req: source, severity, title, summary) | the published `QueueEvent` (incl `count`); summary notes `(×N)` on dedupe |
| `queue.digest` | read | `severityAtLeast?:Severity`, `maxItems?:int>0`, `includeResolved?:bool` | `{events:QueueEvent[], text:<formatted digest>}` |
| `queue.resolve` | local | `id:string` req | not found → `fail("QUEUE_NOT_FOUND")`; else `{id, resolved}` |

> Note: the `queue.publish` JSON-schema sent to the model omits `epistemicKind`
> from `properties` even though the Zod schema accepts it. Reproduce the schema
> as-is (model never sets it; internal callers do).

### 4.5 `grantTools.ts` — automation grants

Constants: `UNGRANTABLE_TOOLS` = {`grant.create`, `grant.revoke`}.
`MUTATING_GRANT_RISKS` = {terminal, project, git, external, system}.

| Tool | risk | args | notes |
|---|---|---|---|
| `grant.create` | local | `actorId:string(min1)` req, `actorType:"watcher"\|"timer"` req, `allowedRiskClasses?:RiskClass[]`, `allowedToolNames?:string(min1)[]`, `ttlMs:int>0 (≤ 30*24*60*60*1000)` req, `maxUses:int>0 (≤1000)` req | **main actor only** |
| `grant.list` | read | `actorId?:string` | live grants (non-revoked, non-expired, uses>0) |
| `grant.revoke` | local | `id:string` req | **main actor only** |

`grant.create` logic (preserve exactly):
1. `actor !== "main"` → `fail("GRANT_ACTOR_FORBIDDEN", recoverable:false)`.
2. both lists empty → `fail("GRANT_EMPTY_SCOPE", recoverable:false)`.
3. any tool name in `UNGRANTABLE_TOOLS` → `fail("GRANT_UNGRANTABLE_TOOL", recoverable:false)`.
4. **grantScopeMutates** = `tools.length>0 || risks.some(MUTATING_GRANT_RISKS)`.
   If mutates AND not `config.autoApprove` → `ctx.confirm({risk:"system",
   consequence:"Pre-authorizes an automation actor…", summary:"Pre-authorize <type>
   <id> to run […] unattended (<n> use(s), TTL <ms>ms)?"})`; declined →
   `fail("USER_DECLINED", recoverable:true)`. (This is the only guard since `local`
   never auto-confirms.)
5. `db.insertGrant({actorId, actorType, allowedRiskClassesJson: risks.length?JSON:null,
   allowedToolNamesJson: tools.length?JSON:null, expiresAt: now+ttlMs, maxUses})`.
   Returns `{id, actorId, actorType, expiresAt, maxUses}`.

`grant.list` view: `{id, actorId, actorType, allowedRiskClasses:[], allowedToolNames:[],
expiresAt:ISO, usesRemaining, maxUses, source}`. `grant.revoke`: not-main →
`GRANT_ACTOR_FORBIDDEN`; `db.revokeGrant(id)` false → `fail("GRANT_NOT_FOUND", recoverable:false)`.

DB: `insertGrant`, `listGrants(actorId?)`, `revokeGrant(id)`, `consumeGrant(actorId,
actor, toolName, risk, started)` (atomic, returns the `AutomationGrantRecord` or null).
Grant authorization = toolName ∈ allowedToolNames OR risk ∈ allowedRiskClasses (union).

### 4.6 `memoryTools.ts` — cross-session project memory (FTS5-backed)

| Tool | risk | args | returns |
|---|---|---|---|
| `memory.recall` | read | `query:string` req, `category?`, `limit?:int(1..50)` (default 10) | `{memories:[view]}` BM25-ranked |
| `memory.list` | read | `category?`, `pinnedOnly?:bool`, `limit?:int(1..200)` (default 50) | `{memories:[view]}` pinned-first then recent |
| `memory.save` | local | `content:string(min1)` req, `category?`, `source?:"user"\|"assistant"` (default assistant) | `{id, memory:view}` |
| `memory.forget` | local | `id:string` req | soft-delete; not found → `fail("MEMORY_NOT_FOUND")` |
| `memory.pin` | local | `id:string` req | idempotent; `{id, memory:view}` |
| `memory.unpin` | local | `id:string` req | idempotent |

> `source` excludes `"compact"` (reserved internal). memory view =
> `{id, content, category, source, pinned:(pinnedAt!=null), createdAt, updatedAt}`.
> DB: `recallMemories(query,{category?,limit?})`, `listMemories({category?,pinnedOnly?,
> limit?})`, `insertMemory`, `forgetMemory`, `pinMemory`, `unpinMemory`. FTS5 external-content
> table `memories_fts` shadows `content`, synced by triggers. Go: SQLite FTS5 via
> `mattn/go-sqlite3` (FTS5 build tag) or `modernc.org/sqlite`.

### 4.7 `watcherTools.ts` — terminal & PR watchers

`WatchCondition` DSL (from `schemas.ts`, recursive Zod union, **strict** objects;
pick exactly one key):
- `{stateIs: AgentState}`
- `{runtimeStatusIs: "running"|"exited"}`
- `{contains: string}` (refined non-empty/non-whitespace)
- `{regex: string}` (min 1, must compile)
- `{noOutputForMs: int>0}` (positive int — rejects Infinity)
- `{modelJudge: string}` (non-empty/non-whitespace)
- `{all: WatchCondition[] (min 1)}`
- `{any: WatchCondition[] (min 1)}`
- `{not: WatchCondition}`

The **model-facing JSON-schema** (`WATCH_CONDITION_SCHEMA`) is hand-written and
differs from the Zod authority — preserve its quirks exactly:
- Uses `anyOf` (NEVER `oneOf` — Fireworks unsupported).
- No `$ref`/deep recursion: combinators (`all`/`any`/`not`) flatten to **one level**
  whose children are atomic leaves only (`items:{anyOf: WATCH_CONDITION_LEAVES}`).
- The DSL `not` is a property literally named `not`, NOT the JSON-Schema `not` keyword.
- `stateIs` enum mirrors `AgentState`; `noOutputForMs` leaf uses `type:"integer", minimum:1`.

| Tool | risk | args | returns |
|---|---|---|---|
| `watcher.terminal.create` | local | `terminalIds:string[] (1..256)` req, `title` req, `goal` req, `cadenceMs?:int>0` (default `MONITOR_DEFAULT_CADENCE_MS`=120000), `startAfterMs?:int≥0`, `stopAfterMs?:int>0`, `stopWhen?:WatchCondition`, `alertWhen?:WatchCondition`, `modelTier?:"small"\|"medium"` (default small) | `{id, nextCheckAt, daemonActive}` |
| `watcher.watchPR` | local | `prNumber:int>0` req, `cwd?`, `title?` (default `PR #N`), `startAfterMs?:int≥0`, `stopAfterMs?:int>0` | `{id, prNumber, cadenceMs, nextCheckAt, daemonActive}` |
| `watcher.list` | read | `{}` | `{watchers:WatcherRecord[]}` (status `active`) |
| `watcher.cancel` | local | `id:string` req | not found → `fail("WATCHER_NOT_FOUND", recoverable:false)`; else `{id}` |

`watcher.terminal.create`: `kind:"terminal"`, `isSupervisor:false`,
`nextCheckAt = now + (startAfterMs ?? 0)`, stop/alert serialized to JSON. Always
append the foreground-only lifecycle NOTE (session-scoped — watchers do NOT resume).
`watcher.watchPR`: `kind:"pr_state"`, `cadenceMs = PR_WATCHER_CADENCE_MS` (60000,
fixed), `modelTier:"small"` (no model actually consulted), `targetsJson =
JSON.stringify(["PR #N"])` (display label, keeps NOT NULL valid), `optionsJson =
PrWatcherOptions{cwd?, prNumber, lastState?, lastIsDraft?, lastUpdatedAt?}` (all
baseline undefined initially). `watcher.cancel` also calls `db.revokeGrantsByActor(id)`.
Both creators emit `logDebug("watcher.created", {...})`.

DB: `insertWatcher(rec)`, `listWatchers("active")`, `getWatcher(id)`,
`updateWatcher(id,{status})`, `revokeGrantsByActor(id)`. `WatcherRecord` schema in
§5. `summarizeWatcher` renders pr_state vs terminal differently (terminal includes
modelTier suffix).

### 4.8 `contextTools.ts` — snapshots & terminal reads (risk `read`)

| Tool | risk | args | returns |
|---|---|---|---|
| `context.snapshot` | read | `{}` (NO_ARGS) | `{mcp, actionContext?, worktrees?, terminals?, inbox}` — best-effort, **never throws** |
| `terminal.summarize` | read | `terminalId:string` req, `purpose?:string`, `tailBytes?:int (1..100_000)` | `{terminalId, purpose, truncated, summary}` |
| `terminal.read` | read | `terminalId:string` req, `maxLines:int (1..1000)` default 200, `tailBytes?:int (1..100_000)` | `{terminalId, content, lineCount}` — VERBATIM, no model |

`context.snapshot`: `mcp.status()`; best-effort `tryCall` (returns undefined on any
failure / disconnect) of `actions.getContext`, `worktree.list`, `terminal.list`;
local inbox `queue.digest({severityAtLeast:"attention", maxItems:10})`. Builds a
text summary; degraded note when disconnected.
`terminal.summarize`: MCP not connected → `fail("MCP_UNAVAILABLE", recoverable:true)`;
aborted → `fail("CANCELLED", recoverable:false)`; reads tail (200 lines) via
`terminal.getOutput`, slices to `tailBytes`, runs `router.chat("small", {messages:
[SUMMARIZER_SYSTEM_PROMPT, buildSummarizerUserPrompt({purpose,tail})], maxTokens:512})`.
`finishReason==="length"` → prepend a "⚠ cut off… use terminal.read" warning,
`truncated:true`. Errors: `TERMINAL_OUTPUT`, `SUMMARIZE`.
`terminal.read`: reads tail (maxLines) via `terminal.getOutput`, optional `tailBytes`
tail-slice; scrollback read from `structuredContent.content` (string) else raw `text`.

### 4.9 `extractionTools.ts` — on-demand terminal extraction

Shared base schema (`baseExtractShape`): `terminalIds:string(min1)[] (1..16)` req,
`format:"text"|"json"` (default text), `jsonSchema?:string`, `wait?` (see below),
`pollIntervalMs:int (0..60_000)` default 2000, `maxAttempts:int (1..120)` default 30,
`tailBytes:int (1..100_000)` default 12000, `maxTokens:int (1..2000)` default 1024.

`wait` (inline tools only) uses `ExtractWaitSchema` = `z.preprocess(coerce empty `{}`
→ SETTLED_WAIT, WatchCondition)`. `SETTLED_WAIT = {any:[{stateIs:"waiting"},
{stateIs:"completed"},{stateIs:"exited"}]}`. **modelJudge conditions are rejected**
(`rejectModelJudge` → `fail("UNSUPPORTED_CONDITION", recoverable:false)`).

| Tool | risk | extra args | returns |
|---|---|---|---|
| `terminal.extract` | read | `instruction?:string(min1)` (omit → gate-only, no model) | gate-only: `{finished, matched, attempts, elapsedMs, terminalIds}`; with instruction: `{terminalIds, format, attempts, elapsedMs, matched, finished, truncated, result}` |
| `terminal.extract.async` | local | `instruction:string(min1)` req, `title?`, `verdictInstruction?`, `dedupeKey?`, `ttlMs?:int>0` | `{requestId, terminalIds}` (fire-and-forget) |

Both: `format==="json"` requires `jsonSchema` (superRefine → INVALID_ARGS on
`jsonSchema` path). MCP disconnected → `fail("MCP_UNAVAILABLE", recoverable:true)`.

`terminal.extract`: `pollUntil` (reads once or polls `wait` up to maxAttempts,
honoring abort). Gate-only mode (no instruction) reports booleans. `wait && !matched`
→ `fail("WAIT_TIMEOUT", recoverable:true, details:{attempts,finished})`. Else
`runExtract`. `text` path: `router.chat("small")`, `truncated = finishReason==="length"`;
prepend a "⚠ cut off… raise maxTokens or use terminal.read" note. `json` path:
`router.json("small", …, ExtractionResult={result: unknown.nullable().default(null)})`,
`truncated:false` (json path can't report it). Error: `EXTRACT`.

`terminal.extract.async`: `requestId = randomUUID()`; fires `runAsyncExtraction`
**with `signal` stripped** (outlives the turn). `runAsyncExtraction` (exported,
never throws): polls; on `wait && !matched` publishes a `model_worker`/`attention`
"wait timed out" event; else extracts; optional `runVerdict` (`router.json("small",
VERDICT_SYSTEM_PROMPT, {pass,reason}, maxTokens:200)`); publishes a `model_worker`
event severity `done`(pass/no-verdict) or `attention`(fail), `evidence:[resultText
.slice(0,2000)]`, summary truncated to 280 chars + truncation suffix; on throw
publishes an `error` event. `dedupeKey = args.dedupeKey ?? "extract:<requestId>"`.

`readSignals`/`pollUntil` reuse watcher-engine helpers (`evaluateCondition`,
`readOutput`, `readStatuses`, `nextOutputState`, `collectModelJudges`) — port those
in lockstep with the watcher engine (separate spec). Aggregation rules: tail is
unlabelled for contains/regex but labelled `[id]\n<tail>` for the model when >1
terminal; `runtimeStatus:"exited"` only when ALL exited (and the read actually
returned terminals — #108 guard: `byId.size>0`); `msSinceOutput` = MIN across
terminals; a failed deep read must NOT advance noOutputForMs state.

`delay(ms, signal)`: `setTimeout` + `unref` + abort-listener early-resolve. Go:
`time.NewTimer` + `select` on `ctx.Done()`.

### 4.10 `skillRunTools.ts` — skill step progress + load/find

| Tool | risk | args | returns |
|---|---|---|---|
| `skill.step.advance` | local | `skillId:string(min1)` req, `completedStep:int≥1` req, `nextStep?:int≥1`, `status:"done"\|"skipped"` (default done), `notes?` | `{state:view}` |
| `skill.run.get` | read | `skillId:string(min1)` req | `{state:view\|null}` (absence is a normal OK answer) |
| `skill.find` | read | `query:string(trim,min1)` req | `{query, selected, reason, activeSkillIds}` |
| `skill.load` | read | `skillId:string(trim,min1)` req | `{id, title, summary, activeSkillIds}` |

`skill.step.advance`/`run.get`: no `sessionId` → `fail("SKILL_RUN_NO_SESSION",
recoverable:false)`. `advance`: `finished = nextStep===undefined`; `currentStep =
finished ? completedStep : max(nextStep, existing.currentStep ?? 0)` (no regress);
upsert step into sorted array; `status` becomes `completed`/`active`; stamp
`completedAt` only on finish (preserve prior). Upsert keyed by `(sessionId, skillId)`.
`skill.find`: no `ctx.findSkills` → `fail("SKILL_FIND_UNAVAILABLE", recoverable:false)`;
`!result.ok` → `fail("SKILL_FIND_FAILED", recoverable:true)`; `!result.matched` →
OK "No skill matched". `skill.load`: no `skillSource` → `SKILL_SOURCE_UNAVAILABLE`
(recoverable:false); unknown id → `fail("SKILL_NOT_FOUND", recoverable:true)`; no
`loadSkills` → `SKILL_LOAD_UNAVAILABLE` (recoverable:false).

DB: `getSkillRunState(sessionId,skillId)`, `insertSkillRunState`, `updateSkillRunState(id,patch)`.
Note the `completedAt: undefined` SQL-NULL hazard — build the patch conditionally
(only set `completedAt` when finishing). `SkillFindResult` =
`{ok:bool, matched:bool, selected:[{id,title}], reason?, activeSkillIds:string[]}`.

### 4.11 `auditTools.ts` — audit export (risk `read`)

`AUDIT_EXPORT_COLUMNS` (order = `audit_log` table): `id, ts, actor, toolName,
argsJson, outcome, durationMs, summary, resultJson, grantSource, grantId`.

| Tool | risk | args | returns |
|---|---|---|---|
| `audit.export` | read | `format:"json"\|"csv"` req, `actor?`, `toolName?`, `outcome?`, `tsFrom?:int`, `tsTo?:int`, `limit:int (1..5000)` default 200 | `{format, count, content:<string>}` |

CSV: RFC 4180, header row, **CRLF** line endings, fields quoted when containing
`,"\r\n`, embedded `"` doubled, null/undefined → empty. JSON: `JSON.stringify(rows,
null, 2)`. `parseAuditExportArgs(tokens)` (CLI `/audit export …`) and `csvField`/
`auditToCsv`/`serializeAudit` are exported helpers — port for the CLI surface.
DB: `queryAudit(AuditFilters{actor?,toolName?,outcome?,tsFrom?,tsTo?,limit?})`,
newest-first, AND-combined.

### 4.12 `workflowTools.ts` — workflow ledger

`STATUS_VALUES` = `pending, active, blocked, done, cancelled, failed`.
`TERMINAL_STATUSES` = {done, cancelled, failed} (stamp `completedAt`).

| Tool | risk | args | returns |
|---|---|---|---|
| `workflow.create` | local | all optional: `issueNumber?:int`, `issueUrl?`, `issueTitle?`, `branch?`, `worktreeId?`, `prNumber?:int`, `prUrl?`, `terminalIds?:string[]`, `watcherIds?:string[]`, `queueEventIds?:string[]`, `status?:Status` (default pending), `nextAction?:RecommendedAction`, `notes?:string[]` | `{id, workflow:view}` |
| `workflow.get` | read | `id:string` req | not found → `fail("WORKFLOW_NOT_FOUND", recoverable:false)` |
| `workflow.list` | read | `status?:Status` | `{workflows:[view]}` |
| `workflow.update` | local | `id:string` req + all create fields (arrays **replace**) | `{workflow:view}` |

Array fields serialize to JSON columns (`*Json`); `nextAction` → `nextActionJson`.
`create`: a status born terminal stamps `completedAt = now`. `update`: patch only
provided fields; reaching a terminal status (first time, `existing.completedAt
=== undefined`) stamps `completedAt = now`. View deserializes JSON arrays +
`parseAction` (RecommendedAction.safeParse, undefined on garbage). DB:
`insertWorkflowRun`, `getWorkflowRun`, `listWorkflowRuns(status?)`, `updateWorkflowRun(id,patch)`.

### 4.13 `agentTaskTools.ts` — the no-file-edit escape hatch (risk `project`)

Constants: `AGENT_LAUNCH_NAME_MAX_LEN = 60`, `DEFAULT_AGENT_ID = "claude"`.
`EDIT_CONSTRAINTS_BLOCK` / `EXPLORE_CONSTRAINTS_BLOCK` (exact prose — preserve;
they are appended to the agent prompt).

| Tool | risk | consequence | args |
|---|---|---|---|
| `agentTask.spawnForEdits` | project | "Opens a visible agent terminal in a worktree that can edit project files…" | `title` req, `taskPrompt` req, `worktreeId?`, `agentId?` (default claude), `mode?:"edit"\|"explore"` (default edit), `acceptanceCriteria?`, `context?:{filePaths?:string[], includeDiff?:bool}`, `watcher?:{create:bool (req), goal?, cadenceMs?:int>0}` |

Returns `{launchId, terminalId, worktreeId?, taskId?, watcherId?, watcherWarning?}`.

Behavior (idempotent spawn saga — preserve precisely):
1. MCP disconnected → `fail("MCP_UNAVAILABLE")`. Pre-launch abort → `fail("CANCELLED", recoverable:false)`.
2. Normalize: `agentId = trim || "claude"`, `mode = mode ?? "edit"`,
   `worktreeId = trim || undefined`. `name = buildAgentLaunchName(title, agentId)`
   = `"<Capitalized agentId>: <task collapsed-ws or 'task'>"` hard-capped at 60
   (prefix always survives). `prompt = buildAgentPrompt(args)` (task + context
   lines + constraints block + acceptanceCriteria block).
3. `idempotencyKey = sha256(canonical JSON of sorted {taskPrompt, worktreeId|"",
   agentId, mode}).slice(0,16)` — title/context excluded.
4. `db.findActiveAgentLaunch(idempotencyKey)`: if existing with `terminalId` →
   `finishBoundLaunch(…, "idempotent")`. If existing unbound → `reconcileViaTerminalList`
   (match `terminal.list` entry where name == launch name AND agentId/worktreeId
   match when present; bind only on a SINGLE match); reconciled → `finishBoundLaunch(…,
   "reconciled")`; else retire the record `failed`/`LAUNCH_NOT_FOUND` and fall through.
5. **Write-ahead**: `db.insertAgentLaunch({idempotencyKey, agentId, worktreeId, mode,
   title, name, stage:"launch_requested"})` BEFORE the MCP call.
6. `mcp.callTool("agent.launch", {agentId, name, worktreeId?, prompt, requestKey:
   idempotencyKey}, signal)`. Throw + aborted → mark `failed`/`CANCELLED`, `fail("CANCELLED")`.
   Throw (not aborted) → mark `ambiguous`/`AGENT_LAUNCH_THREW`, reconcile; reconciled
   → finish; else `fail("AGENT_LAUNCH_AMBIGUOUS", recoverable:true)`.
   `res.isError` → mark `failed`/`AGENT_LAUNCH_FAILED`, `fail("AGENT_LAUNCH_FAILED")`.
7. Mark `agent_started`. `terminalId = extractField(res,"terminalId")` (structuredContent,
   nested task/agent/result/data, or regex on text). No terminalId → mark
   `ambiguous`/`NO_TERMINAL_ID`, reconcile; else `fail("AGENT_LAUNCH_AMBIGUOUS",
   recoverable:true)`. Else mark `terminal_bound` and `finishBoundLaunch(…, "fresh")`.
8. `finishBoundLaunch`: if `watcher.create && !watcherId`, `db.insertWatcher({kind:
   "terminal", title:"watch <title>", goal, targetsJson:[terminalId], cadenceMs:
   watcher.cadenceMs ?? SUPERVISOR_DEFAULT_CADENCE_MS (3000), isSupervisor:true,
   modelTier:"small", nextCheckAt:now, optionsJson:{verificationScope:{worktreeId}?,
   spawnMode:mode, acceptanceCriteria?}})`; failure → `watcherWarning` (record stays
   `terminal_bound` for retry). Else advance `confirmed`. Foreground-only lifecycle
   NOTE when a watcher exists.

DB: `findActiveAgentLaunch(key)`, `insertAgentLaunch(rec)`, `updateAgentLaunch(id,patch)`.
`cancelStaleAgentLaunches()` on DB open marks non-terminal rows `failed` (session-scoped).
Go: `crypto/sha256` + `encoding/hex`; `encoding/json` with sorted keys (the input is
a flat object — sort keys manually for canonical form).

### 4.14 `mcpTools.ts` — Daintree MCP discovery, passthrough, typed wrappers

The largest family. Three categories: discovery/raw-call, typed Daintree action
wrappers (copyTree/terminal/agent/git/recipe/worktree/workflow), and forge
issue/PR/review wrappers.

#### Discovery & raw call

| Tool | risk | args | notes |
|---|---|---|---|
| `daintree.status` | read | NO_ARGS | works disconnected; returns `mcp.status()` |
| `daintree.listTools` | read | `{}` (strict) | disconnected → `MCP_UNAVAILABLE`; `{tools:[{name,description,callable}], note}` |
| `tool.search` | read | `query:string` req, `max?:int (1..100)` default 20 | substring on name/description; `{query, matches:[{name,description,callable}], note}` |
| `daintree.call` | **system** | `name:string` req, `arguments?:record`, `requestKey?:string` | raw escape hatch; consequence set |

`callable` = `makeCallablePredicate(ctx.activeToolNames)` (undefined ⇒ all true).
`CALLABLE_NOTE` constant explains `callable:false`. `daintree.call`: if `name` ∈
`WRAPPED_MCP_TOOLS` → `fail("USE_TYPED_WRAPPER", <redirect msg>)`; if
`isForbiddenToolName(name)` → `fail("FILE_EDIT_FORBIDDEN", recoverable:false)`;
disconnected → `MCP_UNAVAILABLE`; else passthrough; isError → `MCP_TOOL_ERROR`;
aborted → `CANCELLED`. Returns `{text, structuredContent, isError}`.

`WRAPPED_MCP_TOOLS` (raw MCP name → redirect message; these are refused via
`daintree.call`): `agent.launch`, `terminal.getOutput`, `panel.focus`,
`terminal.sendCommand`, `terminal.arm`, `terminal.disarm`, `terminal.disarmAll`,
`copyTree.injectToTerminal`, `copyTree.generateAndCopyFile`, `git.snapshotRevert`,
`git.snapshotDelete`.

#### `passthrough(ctx, mcpName, args, requestKey?)` — shared forwarder

Disconnected → `fail("MCP_UNAVAILABLE")`. Calls `mcp.callTool(mcpName, {...args,
requestKey?}, signal)`. isError → `fail("MCP_TOOL_ERROR", "Daintree refused <name>:
<text>" or generic, details:{structuredContent, rawText})`. aborted → `fail("CANCELLED",
recoverable:false)`. else `ok("Called <name>.", {text, structuredContent})`.

#### Recipe / worktree / focus / copyTree / git-snapshot wrappers

| Tool | risk | args | MCP target / notes |
|---|---|---|---|
| `recipe.list` | read | `arguments?:record` | → `recipe.list` |
| `recipe.run` | project | `recipeId:string` req, `arguments?:record`, `requestKey?` | → `recipe.run` ({...args, recipeId} — explicit recipeId wins) |
| `worktree.createWithRecipe` | project | `arguments:record` req, `requestKey?` | → `worktree.createWithRecipe` |
| `terminal.focus` | ui | `terminalId:string` req | → `panel.focus` with `{panelId: terminalId}` |
| `copyTree.generate` | read | `worktreeId?`, `options?:record` | → `copyTree.generate` |
| `terminal.sendCommand` | terminal | `terminalId:string(trim,min1)` req, `command:string(trim,min1)` req | → `terminal.sendCommand` |
| `terminal.arm` | terminal | `terminalId:string(trim,min1)` req (strict) | → `terminal.arm` via `terminalArmingPassthrough` |
| `terminal.disarm` | terminal | same | → `terminal.disarm` via arming passthrough |
| `terminal.disarmAll` | terminal | `{}` (strict) | → `terminal.disarmAll` via arming passthrough |
| `copyTree.injectToTerminal` | terminal | `terminalId:string(trim,min1)` req, `worktreeId?`, `options?:record` | → `copyTree.injectToTerminal` |
| `agent.focusNextWaiting` | ui | NO_ARGS | → `agent.focusNextWaiting` |
| `agent.focusNextWorking` | ui | NO_ARGS | → `agent.focusNextWorking` |
| `agent.focusNextAgent` | ui | NO_ARGS | → `agent.focusNextAgent` |
| `agent.focusPreviousAgent` | ui | NO_ARGS | → `agent.focusPreviousAgent` |
| `workflow.focusNextAttention` | ui | NO_ARGS | → `workflow.focusNextAttention` |
| `copyTree.generateAndCopyFile` | **system** | `worktreeId?`, `options?:record` | → `copyTree.generateAndCopyFile` |
| `git.snapshotRevert` | git | `worktreeId:string(trim,min1)` req | → `git.snapshotRevert` |
| `git.snapshotDelete` | git | `worktreeId:string(trim,min1)` req | → `git.snapshotDelete` |

`terminalArmingPassthrough`: runs `passthrough`, then extracts `{armed:string[]}`
via `extractArmedSet` (structuredContent first, then JSON-parsed text). Missing
armed set → `fail("MCP_TOOL_ERROR", "<name> did not report the resulting armed
set…")`. Else `ok("<action> Armed terminals now: <list|none>.")`. `options` /
`arguments` are opaque records forwarded verbatim (do NOT model keys).

#### Forge wrappers

Reads (risk `read`): `forge.listIssues` (`arguments?:record`, strict), `forge.getIssue`
(`arguments?:record`, strict), `forge.listPRs` (`arguments?:record`, strict),
`forge.getPR` (`cwd?`, `prNumber:int>0` req). Reads forward `args.arguments ?? {}`.

Writes (risk `external`, always confirm) built by `forgeWrite(name, desc, schema,
parameters)` — handler lifts `requestKey` out and forwards the rest to the
same-named MCP action. Each carries a `consequence` from `FORGE_WRITE_CONSEQUENCES`.
Shared fields: `cwd?:string`, `requestKey?:string`, `issueNumber:int>0`,
`prNumber:int>0` (positive integers, never strings).

| Tool | required args |
|---|---|
| `forge.createIssue` | `title` (+ `body?`, `labels?:string[]`, `cwd?`) |
| `forge.closeIssue` | `issueNumber` (+ `stateReason?:"completed"\|"not_planned"\|"duplicate"`) |
| `forge.reopenIssue` | `issueNumber` |
| `forge.editIssue` | `issueNumber` (+ at least one of `title?`/`body?` — refine) |
| `forge.addIssueComment` | `issueNumber`, `body` |
| `forge.addIssueLabel` | `issueNumber`, `label` |
| `forge.removeIssueLabel` | `issueNumber`, `label` |
| `forge.assignIssue` | `issueNumber`, `username` |
| `forge.unassignIssue` | `issueNumber`, `username` |
| `forge.createPR` | `head`, `base`, `title` (+ `body?`, `draft?:bool`) |
| `forge.closePR` | `prNumber` |
| `forge.reopenPR` | `prNumber` |
| `forge.mergePR` | `prNumber` (+ `mergeMethod?:"merge"\|"squash"\|"rebase"`, `commitTitle?`, `commitMessage?`) |
| `forge.convertPRToDraft` | `prNumber` |
| `forge.markPRReadyForReview` | `prNumber` |
| `forge.commentOnPR` | `prNumber`, `body` |
| `forge.editPR` | `prNumber` (+ at least one of `title?`/`body?` — refine) |
| `forge.approvePR` | `prNumber` (+ `body?`) |
| `forge.requestChanges` | `prNumber`, `body` |
| `forge.dismissReview` | `prNumber`, `reviewId:int>0`, `message` |
| `forge.requestReviewers` | `prNumber` (+ at least one of `users?:string[]`/`teams?:string[]` — refine) |

#### Workflow mutations

| Tool | risk | args | notes |
|---|---|---|---|
| `workflow.startWorkOnIssue` | external | `arguments:record` req, `requestKey?`, `attachWatcher?:bool` (default true) | passthrough then `attachSupervisorWatcher` unless `attachWatcher===false` or passthrough failed |
| `workflow.prepBranchForReview` | external | `arguments:record` req, `requestKey?` | passthrough only |

`attachSupervisorWatcher`: reads `structuredContent.{terminalId, worktreeId,
issueTitle, issueNumber}`; no terminalId → return res untouched (setup still
succeeded). Dedup: if an active supervisor watcher already targets the terminal,
reuse it. Else `db.insertWatcher({kind:"terminal", title:"watch <issueTitle|issue
#N>", goal, targetsJson:[terminalId], cadenceMs:SUPERVISOR_DEFAULT_CADENCE_MS (3000),
isSupervisor:true, modelTier:"small", nextCheckAt:now, optionsJson:{verificationScope:
{worktreeId}?, spawnMode:"edit"}})`. Insert failure → warning, not a failed call.
Foreground-only lifecycle NOTE appended. `attachWatcher` is assistant-side only —
NEVER forwarded to Daintree.

---

## 5. Persisted record schemas (`schemas.ts`) — SQLite column contracts

These back the DB methods the tools call. Column/JSON names are a storage contract.

- **`TimerRecord`**: `id, title, fireAt:ms, repeatEveryMs?, repeatUntil?, maxRuns?,
  runCount, payloadType:"enqueue"|"run_check"|"call_safe_tool", payloadJson,
  targetJson?, status:"scheduled"|"fired"|"cancelled"|"done", createdAt, lastFiredAt?`.
- **`WatcherRecord`**: `id, kind:"terminal"|"pr_state", title, goal, targetsJson
  (JSON string[]), cadenceMs, isSupervisor?, modelTier:ModelTier, startAfterMs?,
  stopAfterMs?, stopWhenJson?, alertWhenJson?, optionsJson?, status:"created"|
  "active"|"paused"|"condition_met"|"timeout"|"cancelled"|"error", lastClassification?,
  lastEpistemicKind?, lastCheckedAt?, nextCheckAt, createdAt`.
- **`AuditRecord`**: `id, ts, actor, toolName, argsJson, outcome:"ok"|"error"|"denied"
  |"dedup"|"grant_ok", durationMs, summary, resultJson?, grantSource?, grantId?, runId?`.
- **`RunEventRecord`**: `id (rne_<uuid8>), runId, seq (0-based), ts, type, payload?`.
- **`AutomationGrantRecord`**: `id, actorId (wch_/tmr_), actorType:"watcher"|"timer",
  allowedRiskClassesJson:string|null, allowedToolNamesJson:string|null, expiresAt:ms,
  maxUses, usesRemaining, revokedAt:number|null, createdAt, source:"local"|"daintree"`.
- **`WorkflowRunRecord`**: `id (wfr_<uuid8>), issueNumber?, issueUrl?, issueTitle?,
  branch?, worktreeId?, prNumber?, prUrl?, terminalIdsJson?, watcherIdsJson?,
  queueEventIdsJson?, status:WorkflowRunStatus, nextActionJson?, notesJson?, createdAt,
  updatedAt, completedAt?`.
- **`MemoryRecord`**: `id (mem_<uuid8>), content, category?, source:"user"|"assistant"
  |"compact", pinnedAt?, deletedAt?, createdAt, updatedAt`. (FTS5 `memories_fts`.)
- **`SkillRunStateRecord`**: `id (rrs_<uuid8>), sessionId, skillId, currentStep,
  stepsJson (JSON SkillStepProgress[]), status:"active"|"completed"|"abandoned",
  startedAt, updatedAt, completedAt?`. Natural key `(sessionId, skillId)`.
- **`AgentLaunchRecord`**: `id (agt_<uuid8>), idempotencyKey, agentId, worktreeId?,
  mode, title, name, terminalId?, watcherId?, stage:AgentLaunchStage, errorCode?,
  errorMessage?, createdAt, updatedAt`. `AgentLaunchStage` = `launch_requested,
  agent_started, terminal_bound, watcher_attached, confirmed, failed, ambiguous`
  (only `confirmed`/`failed` terminal).
- **`QueueEvent`**: `id, source, severity, title, summary, target?, evidence?,
  recommendedActions?, dedupeKey?, epistemicKind?, createdAt, updatedAt?, expiresAt?,
  resolvedAt?, count`.
- **`VerificationResult`** (queue evidence, prefix `VERIFICATION_EVIDENCE_PREFIX =
  "verification:"`): `verdict (catch→unknown), hasGitChanges, changedFiles (≥0,
  default 0), changedFileList (default []), gitSummary, acceptanceCriteria?,
  criteriaMetSummary?, unresolvedWarnings (default [])`. `verdict` uses `.catch
  ("unknown")` (legacy "clean"/"dirty" → unknown).
- **`WatcherVerdict`** (small-model JSON, strict): `classification:WatcherClassification,
  confidence:0..1, summary, evidence (default []), recommendedAction:"none"|
  "focus_terminal"|"ask_user"|"send_input"|"spawn_helper"|"open_review" (default none)`.
- **`ModelJudgeAnswer`** (strict): `reason, confidence:0..1, matched:bool` — **`reason`
  before `matched`** (implicit CoT; preserve field order in the JSON schema).

ID prefixes (preserve the `<prefix>_<uuid8>` convention): `tmr_`, `wch_`, `grt_`,
`wfr_`, `mem_`, `rrs_`, `agt_`, `rne_`, plus audit/queue ids. Timestamps are
Unix epoch **milliseconds** (`Date.now()`); ISO-8601 only at tool boundaries that
explicitly emit it (timer `fireAt`, grant `expiresAt` in views). Go: `time.Now()
.UnixMilli()`; ISO via `time.RFC3339` / `.UTC().Format`.

## 6. One-shot / JSONL contract (`schemas.ts`, used by CLI not tools directly)

`JSON_OUTPUT_SCHEMA_VERSION = 1`. `ONE_SHOT_EXIT_CODE = {success:0, error:1,
cancelled:2, toolFailure:3 (reserved)}`. `JsonlEventType` reuses RunEventRecord
type strings + `result`: `assistant:start, assistant:content, assistant:end,
assistant:cancelled, tool:call, tool:result, error, info, result`. Port for the
non-interactive `--json` mode (separate spec), listed here for completeness.

---

## 7. Go package mapping proposal

| TS source | Go package | key types / notes |
|---|---|---|
| `tools/types.ts`, `registry.ts` | `internal/tools` | `ToolDef`, `ToolContext`, `Registry`, `Dispatch`, wire-name aliasing, audit cap |
| `schemas.ts` enums/records | `internal/schema` | typed string consts + structs; keep JSON tags matching the `*Json` column names |
| `safety/policy.ts` | `internal/safety` | `Decide`, `TierAllows`, sensitive-path/forbidden-name guards |
| `watcherCadence.ts` | `internal/schema` (consts) | `time.Duration` consts |
| each `*Tools.ts` | `internal/tools/<family>` | one file per family returning `[]ToolDef` |

- **Validation**: Zod has no direct Go analog. Recommended: keep the hand-written
  JSON-schema objects as the model-facing spec (a `map[string]any` or embedded JSON),
  and write per-tool typed arg structs decoded with `encoding/json` +
  `github.com/go-playground/validator/v10` (or hand-rolled checks) reproducing the
  bounds (min/max/int/positive). Reproduce `summarizeIssue`'s union-failure menu for
  the watcher DSL specifically, since it is load-bearing.
- **AbortSignal → `context.Context`** everywhere `signal` appears; `ctx.Err() ==
  context.Canceled` maps to the `CANCELLED` failures.
- **uuid**: `github.com/google/uuid` (take first 8 hex of `New().String()` minus
  dashes for the `<prefix>_<uuid8>` ids; agentTask key uses `crypto/sha256`).
- **SQLite**: `modernc.org/sqlite` (pure-Go, no cgo) or `mattn/go-sqlite3` with the
  `sqlite_fts5` build tag (memory recall needs FTS5). Runtime-adaptive Bun/Node
  driver split disappears.
- **JSON-schema for the model**: emit the same byte shapes; Fireworks needs `anyOf`
  not `oneOf`, no `$ref`, the `not`-as-property quirk for the watch DSL.

## 8. Risks / non-obvious contracts to preserve

1. **Tool names + arg JSON-schemas + risk classes are a hard wire contract.** Any
   drift breaks model tool-calling or the safety gate. ~75 names enumerated above.
2. **Wire-name aliasing** (`.`→`__`) and collision/illegal-name fail-fast at projection.
3. **`fail` defaults `recoverable:true`**; many specific sites override to `false`
   (every `recoverable:false` above is intentional and changes model retry behavior).
4. **Audit JSON cap = 4000 bytes**, preview to `4000-200`; CSV export columns/order/CRLF.
5. **Grant gate**: `grant.create` mutating-scope confirm is the ONLY guard (local risk
   never auto-confirms); non-main actors blocked from create/revoke; `consumeGrant`
   is atomic; cancel of timer/watcher revokes its grants.
6. **agentTask idempotency**: sha256 of {taskPrompt, worktreeId|"", agentId, mode}
   first-16-hex; write-ahead saga record before the MCP call; ambiguous vs failed vs
   cancelled distinction; reconcile via single-match terminal.list; stale launches
   retired on DB open.
7. **Extraction**: `terminal.extract.async` strips the abort signal (outlives turn);
   modelJudge rejected in extraction waits; empty `wait:{}` coerced to SETTLED_WAIT
   for inline extract only (not real watchers); truncation warnings PREPENDED.
8. **Foreground-only lifecycle NOTE** appended by every timer/watcher/spawn creator;
   watchers are session-scoped (discarded on close), timers persist & resume.
9. **Sensitive-path refusal** re-checks the symlink realpath; secret basenames/
   suffixes/dir-segments lists are exact; `resolveInsideProject` double containment.
10. **`workflow.startWorkOnIssue.attachWatcher`** is assistant-side, never forwarded.
11. **`recipe.run`** forces top-level `recipeId` to win over a nested `arguments.recipeId`.
12. **`ModelJudgeAnswer` field order** (reason before matched) is intentional CoT.
13. **`VerificationResult.verdict` `.catch("unknown")`** — legacy blobs must not
    deserialize to a false `verified`.

## 9. DELETE — do NOT port (Node/Bun/React/OpenTUI-specific)

- The runtime-adaptive SQLite driver split (`bun:sqlite` vs `node:sqlite`) — Go uses
  one driver.
- `logDebug` is fine to port, but its Node-env flag plumbing simplifies to Go config.
- Node `Buffer`/`Buffer.byteLength` usage in fs/registry → `[]byte`/`len`.
- `node:crypto` (`randomUUID`, `createHash`) → `crypto/*` + `google/uuid`.
- `setTimeout().unref()` machinery in `extractionTools.delay` → `context` + `time.Timer`.
- The Zod schema objects themselves are deleted; only the **hand-written JSON-schema
  `parameters`** survive as the model-facing spec, plus equivalent Go validation.
- Nothing in this subsystem imports OpenTUI/React; no UI deletions here.
