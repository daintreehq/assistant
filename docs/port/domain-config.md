# Port Spec — Domain, Config, Schemas, Reliability, Queue, Cadence

Authoritative reference for porting these TypeScript sources to Go (Bubble Tea UI). An
implementation agent should be able to port from this file WITHOUT re-reading the TS.

Source files covered:
- `src/config.ts` — configuration resolution + the trusted-env security boundary.
- `src/schemas.ts` — all Zod schemas / domain types / enums / DB-row interfaces.
- `src/projectInstructions.ts` — `DAINTREE.md` loading.
- `src/debugLog.ts` — full-fidelity per-session debug trace.
- `src/reliability.ts` — retry / backoff / timeout / rate-limit-detect helpers.
- `src/watcherCadence.ts` — scheduler/watcher timing constants.
- `src/queue.ts` — the attention queue (+ relevant `src/storage/db.ts` severity ordering).

> Conventions referenced throughout: `*Json` interface fields hold a JSON-serialized
> string of the named shape (the store layer (de)serializes). Timestamps are **wall-clock
> milliseconds since the Unix epoch** (`Date.now()` → `int64` in Go), NOT RFC3339 — except
> the debug-log header timestamp and the log filename date, which are ISO-8601.

---

## 1. Configuration (`src/config.ts`)

### 1.1 Resolution order (HIGHEST priority first) — MUST PRESERVE

1. Explicit overrides (CLI flags) passed to `loadConfig(overrides)`.
2. Real process environment variables (incl. those Daintree injects on launch).
3. `.env` in the **project root** (`<projectPath>/.env`).
4. The **assistant's own** package `.env` (lower precedence fallback; fills gaps only).
5. Built-in defaults (`DEFAULTS`).

`dotenv` **never overrides an already-set env var**, so real env (2) always beats either
`.env` file (3, 4). The project `.env` is loaded before the own `.env`, and since dotenv
doesn't override, the project `.env` wins over the own `.env`.

### 1.2 THE TRUSTED-ENV SECURITY BOUNDARY — CRITICAL, MUST PRESERVE EXACTLY

A snapshot of `process.env` is taken **BEFORE any `.env` file is loaded**:
```
trustedEnv := copy of process.env   // taken before dotenv.config()
```
Rationale (preserve as a comment): the bound project is arbitrary/untrusted code; a
repo-local `.env` must not be able to silently escalate the assistant.

The following settings are read **ONLY from `trustedEnv` or an explicit override** — NEVER
from a loaded `.env`:

| Setting | Source restriction | Reason |
|---|---|---|
| `tier` (`DAINTREE_ASSISTANT_TIER`) | trusted env / override | escalation to `system` |
| `autoApprove` (`DAINTREE_ASSISTANT_AUTO_APPROVE`) | trusted env / override | unattended mutation |
| `offline` (`DAINTREE_ASSISTANT_OFFLINE`) | trusted env / override | exec control |
| `stateDir` (`DAINTREE_ASSISTANT_STATE_DIR`) | trusted env / override | redirect the transcript/audit DB (exfiltration) |
| `logDir` (`DAINTREE_ASSISTANT_LOG_DIR`) | trusted env / override | redirect full-fidelity log into a project-readable path (exfiltration) |

The following MAY come from the merged env (project `.env` allowed) because they are
either UI-cosmetic or can only ever write into the trusted-only `logDir`:

| Setting | Env var | Why merged-env is safe |
|---|---|---|
| `debugLog` | `DAINTREE_ASSISTANT_DEBUG_LOG` | output only ever goes to trusted `logDir` |
| `splash` | `DAINTREE_ASSISTANT_NO_SPLASH` | UI cosmetic |
| `reservedColumns` | `DAINTREE_ASSISTANT_RESERVED_COLUMNS` | UI cosmetic |
| `fireworksApiKey` | `FIREWORKS_API_KEY` | (read from merged env — secret intentionally allowed from project `.env`) |
| `fireworksBaseUrl` | `FIREWORKS_BASE_URL` | merged env |
| model overrides | `DAINTREE_{LARGE,MEDIUM,SMALL}_MODEL` | merged env |
| `mcpUrl`/`mcpToken`/`projectId`/`windowId` | `DAINTREE_MCP_URL`/`_TOKEN`/`DAINTREE_PROJECT_ID`/`DAINTREE_WINDOW_ID` | merged env (Daintree-injected) |

> In Go: load real `os.Environ()` into `trustedEnv` first. Implement a dotenv loader (or
> `github.com/joho/godotenv` with `Load` semantics — note: godotenv `Load` does NOT
> override existing keys, matching dotenv). Apply project `.env` then own `.env`. Keep the
> two distinct lookup helpers: `trusted(key)` (from the pre-`.env` snapshot only) and
> `merged(key)` (from the post-`.env` env).

### 1.3 `AppConfig` fields (exact)

| Field | Type | Source / default | Notes |
|---|---|---|---|
| `projectPath` | string | override / `cwd`, then `path.resolve` | absolute project dir |
| `stateDir` | string | trusted `STATE_DIR`/override → else per-project subdir → else flat root | see 1.4 |
| `dbPath` | string | `path.join(stateDir, "state.db")` | |
| `logDir` | string | trusted `LOG_DIR`/override → `~/.daintree/logs`; always `path.resolve` | GLOBAL, not per-project |
| `fireworksApiKey` | string | merged `FIREWORKS_API_KEY` ?? `""` | |
| `fireworksBaseUrl` | string | merged `FIREWORKS_BASE_URL` ?? default | |
| `largeModel` | string | override / merged `DAINTREE_LARGE_MODEL` ?? default | |
| `mediumModel` | string | merged `DAINTREE_MEDIUM_MODEL` ?? default | no override field |
| `smallModel` | string | override / merged `DAINTREE_SMALL_MODEL` ?? default | |
| `mcpUrl` | string? | override / merged `DAINTREE_MCP_URL` | optional |
| `mcpToken` | string? | override / merged `DAINTREE_MCP_TOKEN` | optional |
| `projectId` | string? | override / merged `DAINTREE_PROJECT_ID` | drives per-project stateDir |
| `windowId` | string? | override / merged `DAINTREE_WINDOW_ID` | env-only conceptually; surfaced but does NOT affect path yet; affects `reservedColumns` default |
| `tier` | `Tier` enum | override / trusted `DAINTREE_ASSISTANT_TIER` ?? `"system"` | **fail closed** (see 1.5) |
| `autoApprove` | bool | override ?? `trusted DAINTREE_ASSISTANT_AUTO_APPROVE == "1"` | trusted only |
| `offline` | bool | override ?? `trusted DAINTREE_ASSISTANT_OFFLINE == "1"` | trusted only |
| `projectInstructions` | string? | override only | pre-loaded by entry path; `loadConfig` never reads FS for this |
| `debugLog` | bool | override ?? `merged DAINTREE_ASSISTANT_DEBUG_LOG == "1"` | merged env OK |
| `splash` | bool | override ?? `merged DAINTREE_ASSISTANT_NO_SPLASH != "1"` | default **true** |
| `reservedColumns` | number | see 1.6 | floor 1; default 2 if embedded else 1 |

`DEFAULTS` (exact constant values):
| Key | Value |
|---|---|
| `fireworksBaseUrl` | `https://api.fireworks.ai/inference/v1` |
| `largeModel` | `accounts/fireworks/models/glm-5p2` |
| `mediumModel` | `accounts/fireworks/models/glm-5p2` |
| `smallModel` | `accounts/fireworks/models/deepseek-v4-flash` |
| `defaultMcpUrl` | `http://127.0.0.1:45454/mcp` (declared but NOT applied as a default for `mcpUrl`; `mcpUrl` is left undefined when unset → degraded local mode) |

> NOTE: the CLAUDE.md doc text mentions `minimax-m3`/`deepseek-v4-flash`; the ACTUAL code
> defaults are the `glm-5p2` / `deepseek-v4-flash` values above. Port the **code** values.

### 1.4 State directory precedence (exact)

`stateRoot = ~/.daintree/assistant-cli`
```
stateDir =
  firstString(override.stateDir, trusted.DAINTREE_ASSISTANT_STATE_DIR)
  ?? (projectId ? join(stateRoot, projectIdToDir(projectId)) : stateRoot)
```
Then `os.MkdirAll(stateDir, 0o777-umask)` (TS uses `fs.mkdirSync(recursive)`). Window-level
isolation is deliberately deferred — `windowId` does NOT yet affect the path.

### 1.5 Tier fail-closed (MUST PRESERVE)

```
rawTier = override.tier ?? trusted.DAINTREE_ASSISTANT_TIER
parsed  = Tier.safeParse(rawTier ?? "system")
tier    = parsed.ok ? parsed : "supervisor"   // explicit-but-invalid drops to LEAST privilege
```
Default when unset is `system`; an explicitly-set INVALID value falls back to the
least-privileged `supervisor`, never silently to `system`.

### 1.6 `reservedColumns` resolution (exact)

```
raw    = firstString(override.reservedColumns?.toString(), merged.DAINTREE_ASSISTANT_RESERVED_COLUMNS)
parsed = raw == undefined ? undefined : parseInt(raw, 10)
if parsed != undefined && isFinite(parsed): return max(1, parsed)
return windowId ? 2 : 1
```
Floor at 1 (the DECAWM autowrap column). A non-finite/negative override is ignored so a bad
value can never widen content back into the autowrap column.

### 1.7 Exported helpers

| Symbol | Signature | Behavior |
|---|---|---|
| `firstString(...vals)` | `(...(string\|undefined)) → string\|undefined` | first arg that is non-empty after `.trim()`, returned trimmed. |
| `assistantVersion()` | `() → string` | reads own `package.json` `version` via an `fs` walk (≤8 parents from module dir up to filesystem root, looking for `package.json`); cached; fallback `"0.0.0"`. In Go: embed version via `runtime/debug.ReadBuildInfo()` or ldflags. |
| `projectIdToDir(rawId)` | `(string) → string` | `slug + "-" + sha256(rawId)[:8 hex]`. Slug: lowercase, `[^a-z0-9_-]→"-"`, collapse `-+`→`-`, strip leading/trailing `-`, truncate to 40, strip trailing `-` again. If slug empty → just the 8-hex hash. **Wire-compatible: path names depend on this exact algorithm.** |
| `loadConfig(overrides)` | `(ConfigOverrides) → AppConfig` | the resolver above. |
| `describeConfig(cfg)` | `(AppConfig) → Record<string,string>` | secret-redacted view for `/status`. Redaction: `s ? "${s[:4]}…${s[-2:]} (${len})" : "(unset)"`. Applied to `fireworksApiKey` and `mcpToken`. `projectInstructions` shown as `"${utf8 byte length} bytes"` or `"(none)"`. `mcpUrl` shows `"(unset → degraded local mode)"` when unset. |

`ConfigOverrides` fields: `projectPath, stateDir, projectId, windowId, mcpUrl, mcpToken,
fireworksApiKey, largeModel, smallModel, tier, autoApprove, offline, debugLog, splash,
reservedColumns, logDir, projectInstructions` (all optional). Note: NO `mediumModel`,
`fireworksBaseUrl`, or `mcpToken`-distinct override beyond what's listed.

### 1.8 Go mapping (config)

- Package `internal/config`.
- `type AppConfig struct{...}` with the exact fields (pointers / `string` "" for optionals).
- Use `os.UserHomeDir()`, `path/filepath`, `crypto/sha256`, `encoding/hex`.
- Dotenv: `github.com/joho/godotenv` (`Load` = no-override). Keep the trusted snapshot
  ordering: snapshot `os.Environ()` into a map BEFORE calling `godotenv.Load`.
- **DELETE**: `splash` and `reservedColumns` are OpenTUI/xterm-specific — keep them in the
  config struct only if the Bubble Tea UI port needs an analogous gutter; otherwise drop
  `reservedColumns` and the splash logic. `assistantVersion()`'s `import.meta.url` walk is
  Node-specific — replace with build-time version injection.

---

## 2. Schemas & Domain Types (`src/schemas.ts`)

> Every Zod `.enum` is a closed string set; port as a Go string type + `const` set +
> validator. `.strict()` = reject unknown keys; `.strip()` = drop unknown keys;
> `.passthrough()` = keep unknown keys. `.catch(x)` = on parse failure yield `x`.

### 2.1 Enums (exhaustive value sets)

| Enum | Values (in order) | `.strict`? | Notes |
|---|---|---|---|
| `RiskClass` | `read, local, ui, terminal, project, git, external, system` | — | drives confirmation matrix. (NB order here differs from CLAUDE.md prose; use this.) |
| `Tier` | `supervisor, operator, system` | — | permission tiers |
| `ModelTier` | `small, medium, large` | — | |
| `AgentState` | `idle, working, waiting, directing, completed, exited` | — | Daintree state mirror |
| `Severity` | `debug, info, attention, urgent, blocked, done, error` | — | enum order ≠ rank order (see §6) |
| `EventSource` | `timer, terminal_watcher, worktree_watcher, pr_watcher, workflow, model_worker, system, user` | — | |
| `EpistemicKind` | `observed, inferred, unverified` | — | provenance of a fact |
| `WatcherClassification` | `no_change, still_working, waiting_for_input, permission_prompt, command_failed, tests_failed, tests_passed, merge_conflict, completed_success, completed_unverified, completed_unknown, terminal_exited, rate_limited, needs_large_model, unknown` | — | `completed_unverified` set ONLY by engine, never by model (omit from model-facing prompt enum) |
| `VerificationVerdict` | `verified, failed, unknown` | — | |
| `JsonOutputStatus` | `success, error, cancelled` | — | |
| `JsonlEventType` | `assistant:start, assistant:content, assistant:end, assistant:cancelled, tool:call, tool:result, error, info, result` | — | reuses RunEventRecord type strings + `result` |
| `WatcherVerdict.recommendedAction` (inline enum) | `none, focus_terminal, ask_user, send_input, spawn_helper, open_review` | — | default `none` |
| `WatchCondition.runtimeStatusIs` (inline) | `running, exited` | — | |

### 2.2 Object schemas (field-by-field)

**`EventTarget`** `.strict()` — all optional: `projectId?`, `worktreeId?`, `terminalId?`,
`workflowRunId?` (all `string`).

**`RecommendedAction`** `.strict()`: `label: string`, `toolName: string`, `args?: unknown`,
`risk?: RiskClass`, `requiresConfirmation?: bool`.

**`QueuePublishArgs`** `.strict()`: `source: EventSource`, `severity: Severity`,
`title: string`, `summary: string`, `target?: EventTarget`, `evidence?: string[]`,
`recommendedActions?: RecommendedAction[]`, `dedupeKey?: string`, `ttlMs?: number`,
`epistemicKind?: EpistemicKind`. (`epistemicKind` MUST be declared — `.strict()` would
otherwise strip it before it reaches the DB.)

**`WatcherVerdict`** `.strict()` (small-model output contract): `classification:
WatcherClassification`, `confidence: number [0,1]`, `summary: string`, `evidence: string[]
default []`, `recommendedAction: <enum> default "none"`.

**`ModelJudgeAnswer`** `.strict()`: field order is load-bearing — `reason: string` FIRST
(implicit chain-of-thought), then `confidence: number [0,1]`, then `matched: bool`.
**Preserve emission order in the JSON schema / struct tags.**

**`VerificationResult`** `.strip()`: `verdict: VerificationVerdict .catch("unknown")`,
`hasGitChanges: bool`, `changedFiles: int ≥0 default 0`, `changedFileList: string[] default
[]`, `gitSummary: string`, `acceptanceCriteria?: string`, `criteriaMetSummary?: string`,
`unresolvedWarnings: string[] default []`. `.catch("unknown")` lets a legacy blob with old
enum values (`clean`/`dirty`) deserialize safely to `unknown`.

**`JsonlEventSchema`** `.passthrough()`: `type: JsonlEventType`, `ts: number`, `seq: int ≥0`
(+ arbitrary extra per-type fields kept).

**`JsonResultEnvelopeSchema`** `.strict()`: `type: literal "result"`, `ts: number`,
`seq: int ≥0`, `schemaVersion: literal 1`, `status: JsonOutputStatus`,
`exitCode: 0|1|2` (union of literals — 3 reserved, not valid here), `content: string`,
`error: {message: string} | null`.

### 2.3 `WatchCondition` recursive DSL (`z.lazy` union, each branch `.strict()`)

Discriminated by the present key. Variants + validation guards (reject degenerate
conditions at creation — preserve every guard):

| Variant | Shape | Guard |
|---|---|---|
| `stateIs` | `{stateIs: AgentState}` | — |
| `runtimeStatusIs` | `{runtimeStatusIs: "running"\|"exited"}` | — |
| `contains` | `{contains: string}` | non-empty after trim |
| `regex` | `{regex: string}` | `min(1)` AND must compile as a regex |
| `noOutputForMs` | `{noOutputForMs: number}` | `.int().positive()` (also rejects Infinity → would persist as `null` → compared as 0 → fires immediately) |
| `modelJudge` | `{modelJudge: string}` | non-empty after trim |
| `all` | `{all: WatchCondition[]}` | `min(1)` |
| `any` | `{any: WatchCondition[]}` | `min(1)` |
| `not` | `{not: WatchCondition}` | — |

> Go: model as a tagged struct `WatchCondition` with pointer fields + a custom
> `UnmarshalJSON` that dispatches on the present key and runs the guards. The regex guard
> must use `regexp.Compile`. **Preserve the "why" comments** — degenerate conditions create
> watchers that can never fire (false supervision).

### 2.4 DB-row interfaces (persisted records)

All `createdAt`/`updatedAt`/`*At` are epoch-ms `int64`. `*Json` fields are JSON strings.

**`TimerRecord`**: `id, title, fireAt(int64), repeatEveryMs?, repeatUntil?, maxRuns?,
runCount(int), payloadType("enqueue"|"run_check"|"call_safe_tool"), payloadJson,
targetJson?, status("scheduled"|"fired"|"cancelled"|"done"), createdAt, lastFiredAt?`.

**`WatcherRecord`**: `id, kind("terminal"|"pr_state"), title, goal, targetsJson(JSON
string[]), cadenceMs(int), isSupervisor?(bool), modelTier(ModelTier), startAfterMs?,
stopAfterMs?, stopWhenJson?, alertWhenJson?, optionsJson?,
status("created"|"active"|"paused"|"condition_met"|"timeout"|"cancelled"|"error"),
lastClassification?(string), lastEpistemicKind?(EpistemicKind), lastCheckedAt?,
nextCheckAt(int64, required), createdAt`. Unknown `kind` fails closed to `error`.

**`AuditRecord`**: `id, ts, actor("main"|"watcher"|"timer"|"workflow"|"system"), toolName,
argsJson, outcome("ok"|"error"|"denied"|"dedup"|"grant_ok"), durationMs, summary,
resultJson?, grantSource?(AutomationGrantSource), grantId?, runId?`.

**`RunEventRecord`**: `id("rne_<uuid8>"), runId, seq(int, from 0), ts, type(string e.g.
"assistant:start"), payload?(JSON string)`.

**`RunSummaryRecord`** (computed, not persisted): `runId, firstTs, lastTs, eventCount(int)`.

**`AutomationGrantRecord`**: `id, actorId("wch_…"|"tmr_…"), actorType(AutomationGrantActorType),
allowedRiskClassesJson(string|null), allowedToolNamesJson(string|null), expiresAt(int64),
maxUses(int), usesRemaining(int), revokedAt(int64|null), createdAt, source(AutomationGrantSource)`.
Authorization = tool-name in allowedToolNames OR risk in allowedRiskClasses (union). At
least one list non-empty — enforced in code, not schema. `revokedAt` = explicit revoke
ONLY; use-exhaustion is `usesRemaining==0` and does NOT stamp `revokedAt`.

**`ConversationMessageRecord`**: `id("msg_<uuid8>"), sessionId, seq(int),
role("system"|"user"|"assistant"|"tool"), content, toolCallsJson?, toolCallId?, createdAt`.

**`SkillSelectionLogRecord`**: `id("rsl_<uuid8>"), ts, sessionId, userInput,
selectedSkillIdsJson, confidence(number), taskType?, reason?`.

**`WorkflowRunRecord`**: `id("wfr_<uuid8>"), issueNumber?(int), issueUrl?, issueTitle?,
branch?, worktreeId?, prNumber?(int), prUrl?, terminalIdsJson?, watcherIdsJson?,
queueEventIdsJson?, status(WorkflowRunStatus), nextActionJson?(serialized RecommendedAction),
notesJson?(JSON string[]), createdAt, updatedAt(required), completedAt?`.

**`MemoryRecord`**: `id("mem_<uuid8>"), content, category?, source(MemorySource),
pinnedAt?, deletedAt?, createdAt, updatedAt`. FTS5 external-content shadow table
`memories_fts` kept in sync by triggers; `forget` is soft-delete (stamp `deletedAt`);
recall/list filter `deletedAt IS NULL`. (One DB == one project's memory; no `projectId`
column.)

**`SkillStepProgress`**: `index(int, 1-based), status(SkillStepStatus), notes?, ts`.

**`SkillRunStateRecord`**: `id("rrs_<uuid8>"), sessionId, skillId, currentStep(int, 0=not
started), stepsJson(JSON SkillStepProgress[]), status(SkillRunStatus), startedAt,
updatedAt, completedAt?`. Natural key `(sessionId, skillId)`.

**`AgentLaunchRecord`**: `id("agt_<uuid8>"), idempotencyKey(deterministic content hash),
agentId, worktreeId?, mode(string "edit"|"explore"), title, name(deterministic launch
name for terminal.list reconciliation), terminalId?, watcherId?, stage(AgentLaunchStage),
errorCode?, errorMessage?, createdAt, updatedAt`. Session-scoped: `cancelStaleAgentLaunches`
marks non-terminal rows `failed` on DB open.

### 2.5 String-union types (not Zod enums, plain TS unions)

| Type | Values |
|---|---|
| `AutomationGrantActorType` | `watcher, timer` |
| `AutomationGrantSource` | `local, daintree` (only `local` minted today) |
| `WorkflowRunStatus` | `pending, active, blocked, done, cancelled, failed` (last 3 + done are terminal) |
| `MemorySource` | `user, assistant, compact` |
| `SkillRunStatus` | `active, completed, abandoned` |
| `SkillStepStatus` | `done, skipped` |
| `AgentLaunchStage` | `launch_requested, agent_started, terminal_bound, watcher_attached, confirmed, failed, ambiguous` (only `confirmed`/`failed` terminal; `ambiguous` stays live for reconciliation) |
| `TimerRecord.payloadType` | `enqueue, run_check, call_safe_tool` |
| `TimerRecord.status` | `scheduled, fired, cancelled, done` |
| `WatcherRecord.kind` | `terminal, pr_state` |
| `WatcherRecord.status` | `created, active, paused, condition_met, timeout, cancelled, error` |
| `AuditRecord.actor` | `main, watcher, timer, workflow, system` |
| `AuditRecord.outcome` | `ok, error, denied, dedup, grant_ok` |
| `ConversationMessageRecord.role` | `system, user, assistant, tool` |

### 2.6 `ToolResult` envelope (TS interface, not Zod)

```
ToolError  { code: string; message: string; recoverable: bool; details?: unknown }
ToolResult<T> { ok: bool; result?: T; error?: ToolError; summary: string; auditId?: string }
```
`summary` is the one-line human/LLM-facing message; `auditId` = id of the `audit_log` row.

### 2.7 Constants in schemas.ts

| Constant | Value | Notes |
|---|---|---|
| `VERIFICATION_EVIDENCE_PREFIX` | `"verification:"` | evidence string carrying a serialized `VerificationResult` |
| `JSON_OUTPUT_SCHEMA_VERSION` | `1` | plain monotonic int; bump only on breaking line-shape change |
| `ONE_SHOT_EXIT_CODE` | `{success:0, error:1, cancelled:2, toolFailure:3}` | 3 is RESERVED (loop has no terminal tool-failure signal today) |

### 2.8 Exported helper functions in schemas.ts

| Function | Signature | Behavior |
|---|---|---|
| `classificationEpistemicKind(classification, usedModel=false)` | `(WatcherClassification\|string\|undefined, bool) → EpistemicKind` | maps a classification to provenance. `terminal_exited`/`waiting_for_input`/`rate_limited` → `inferred` iff `usedModel` else `observed`. `permission_prompt`/`still_working`/`tests_failed`/`tests_passed`/`command_failed`/`merge_conflict`/`completed_success`/`completed_unverified`/`completed_unknown` → `inferred`. default (`no_change`/`unknown`/`needs_large_model`/unrecognized) → `unverified`. |
| `isCompositeCondition(c)` | `(WatchCondition) → bool` (type guard) | true if `c` has key `all`, `any`, or `not`. |

### 2.9 Go mapping (schemas)

- Package `internal/domain` (types) + validation in same package.
- Enums: `type RiskClass string` + `const(...)` + `var validRiskClass = map[...]`. A small
  generic `Enum[T]` validator helper avoids boilerplate.
- DB rows: structs with `int64` epoch-ms, `*int64`/`*string` for optionals,
  `sql.NullInt64`/`sql.NullString` if reading via `database/sql`.
- JSON contracts (`WatcherVerdict`, `ModelJudgeAnswer`, `JsonlEvent`, `JsonResultEnvelope`,
  `VerificationResult`, `WatchCondition`, `QueuePublishArgs`) need struct tags that preserve
  field NAMES and (for `ModelJudgeAnswer`) field ORDER. Go `encoding/json` emits struct
  fields in declaration order — declare `Reason, Confidence, Matched` in that order.
- Replicate Zod semantics: `.strict()` → custom decoder with `DisallowUnknownFields`;
  `.passthrough()` → embed a `map[string]json.RawMessage` for extras; `.catch("unknown")`
  → post-unmarshal validate-and-default; defaults (`[]`, `0`, `"none"`) applied after decode.
- Validation lib optional (`go-playground/validator`), but custom is fine given the count.

---

## 3. Project Instructions (`src/projectInstructions.ts`)

| Symbol | Value/Signature | Behavior |
|---|---|---|
| `PROJECT_INSTRUCTIONS_FILENAME` | `"DAINTREE.md"` | per-repo instruction file at project root |
| `PROJECT_INSTRUCTIONS_MAX_BYTES` | `16 * 1024` = `16384` | hard cap; oversized files are SKIPPED with a warning, NOT truncated |
| `ProjectInstructionsResult` | `{ content?: string; warning?: string }` | content trimmed; absent when nothing to inject |
| `loadProjectInstructions(projectPath)` | `async (string) → ProjectInstructionsResult` | see below |

`loadProjectInstructions` behavior (NEVER throws):
1. `file = resolve(projectPath, "DAINTREE.md")` (against projectPath, not cwd).
2. `stat`; if not a regular file → `{}` (silent skip).
3. if `stat.size > MAX_BYTES` → `{warning: "Skipping DAINTREE.md: <size> bytes exceeds the 16384-byte limit."}`.
4. `readFile` utf8; **re-check** `byteLength(raw, utf8) > MAX_BYTES` (stat+read are two
   syscalls; guard against growth/undercount) → warn + skip.
5. `trimmed = raw.trim()`; if empty → `{}`.
6. else → `{content: trimmed}`.
7. `catch`: `ENOENT` → `{}` (silent, normal case); other error → `{warning: "Could not read DAINTREE.md: <msg>"}`.

> Go: `internal/projectinstructions`. Use `os.Stat`, `os.ReadFile`, `len([]byte)` for utf8
> byte length, `strings.TrimSpace`. ENOENT → `errors.Is(err, fs.ErrNotExist)`. Folded into
> the DYNAMIC prompt layer (message[1]), never the cached base prefix.

---

## 4. Debug Log (`src/debugLog.ts`)

A per-session, full-fidelity, append-only human-readable trace. No-op when disabled; NEVER
throws into the caller (a write failure warns ONCE on stderr, then is swallowed).

### 4.1 Constants & file format

| Symbol | Value | Notes |
|---|---|---|
| `MAX_LOG_AGE_MS` | `7 * 24 * 60 * 60 * 1000` = `604800000` | logs older than this (by mtime) deleted at boot |
| `INLINE_MAX` | `120` (module-private) | a string ≤120 chars and no `\n` renders inline as `key=value`; else block |
| `SESSION_LOG_RE` | `/^\d{4}-\d{2}-\d{2}-.+\.log$/` | only these filenames are eligible for pruning |
| filename | `<YYYY-MM-DD>-<safeId>.log` | date = ISO `toISOString().slice(0,10)`; `safeId = id.replace(/[^\w.-]/g,"") || "session"` |
| dir mode | `0o700` | owner-only (full-fidelity logs contain model msgs/tool args/terminal output) |
| file mode | `0o600` | owner-only |

### 4.2 Line format

```
<ISO-timestamp>  <event>[  k1=v1 k2=v2 ...]\n
[  <blockKey>:\n    <indented full value>\n ...]
```
- Inline scalars: `null`/`undefined`/number/boolean, or string ≤120 chars without newline.
  `undefined` fields are OMITTED (noise). `null` renders as literal `null`.
- Block values: objects/arrays via `JSON.stringify(v, null, 2)`; strings as-is. Each line
  prefixed with 4 spaces; block header is `  <key>:`.
- Values are NEVER truncated.

### 4.3 Functions

| Function | Signature | Behavior |
|---|---|---|
| `logDebug(cfg, event, fields={})` | `(DebugLogConfig\|undefined, string, Record<string,unknown>) → void` | no-op unless `cfg.debugLog && cfg.logDir`. `mkdir(logDir, 0700)`, resolve target file, format + `appendFile(0600)`. Catches all errors → `warnOnce`. |
| `startDebugLog(cfg, sessionId?)` | `(AppConfig, string?) → string\|undefined` | no-op→undefined when off. Else: prune old logs, set `activeLogPath = join(logDir, <date>-<sessionId\|randomId>.log)`, emit `session.start` header, return the path. Call once per process after config load. |
| `currentDebugLogPath()` | `() → string\|undefined` | the active path once logging started |
| `DebugLogConfig` | `Pick<AppConfig,"debugLog"\|"logDir">` | minimal config slice |

`session.start` header fields: `project(projectPath), projectId, windowId, tier, largeModel,
mediumModel, smallModel, mcpUrl (??"(unset)"), offline, autoApprove, stateDir, logDir,
pid, node(process.version)`.

### 4.4 Target resolution & pruning (module-private)

- `resolveTarget(logDir)`: reuse `activeLogPath` iff `dirname(activeLogPath)==logDir`; else
  lazily open a fresh `<date>-<randomId>.log` so stray writes still coalesce.
- `randomId()`: `Math.random().toString(36).slice(2,10)` (8-char base36). In Go: 8 random
  base36 chars (or hex) — format need not match byte-for-byte (it's a random id), but the
  filename PATTERN must match `SESSION_LOG_RE`.
- `pruneOldLogs(dir)`: readdir (missing dir → no-op), for each name matching `SESSION_LOG_RE`,
  if `statSync(p).mtimeMs < (now - MAX_LOG_AGE_MS)` → unlink. Best-effort, never throws.
- `warnOnce`: prints `[debugLog] write failed (logging disabled for this run): <msg>` once.

> Go: package `internal/debuglog`. Use `os.MkdirAll(dir, 0o700)`, `os.OpenFile(..., 0o600)`
> with `O_APPEND|O_CREATE|O_WRONLY`, `time.Now().UTC().Format("2006-01-02")` and
> `time.RFC3339Nano` (or `.Format(time.RFC3339)` — match `new Date().toISOString()` ⇒
> `2006-01-02T15:04:05.000Z`). Package-level `activeLogPath` + `sync.Once`-style `warnedOnce`
> (a plain bool + mutex). JSON block via `json.MarshalIndent(v, "", "  ")`.

---

## 5. Reliability (`src/reliability.ts`)

Dependency-light transient-failure helpers. Pure / built-ins only — fully unit-testable.

### 5.1 Constants

| Symbol | Value | Notes |
|---|---|---|
| `MODEL_RETRY_POLICY` | `{maxRetries:3, baseDelayMs:500, maxDelayMs:10_000}` | Fireworks model calls. `maxRetries` = ADDITIONAL attempts (3 ⇒ up to 4 total) |
| `MODEL_REQUEST_TIMEOUT_MS` | `60_000` | per-attempt timeout, one-shot (chat/json) |
| `MODEL_STREAM_TIMEOUT_MS` | `300_000` | per-attempt timeout, streaming (backstop, not a cap) |
| `MCP_READ_RETRY_POLICY` | `{maxRetries:2, baseDelayMs:250, maxDelayMs:2_000}` | read-only MCP only; NEVER mutating tools (double-apply) |
| `MCP_READ_TIMEOUT_MS` | `20_000` | per-attempt timeout, MCP reads |
| `MAX_RETRY_AFTER_MS` | `30_000` (module-private) | cap on honoured Retry-After |
| `RATE_LIMIT_TAIL_WINDOW` | `1500` | largest tail char window scanned for rate-limit signature |
| `RATE_LIMIT_SIGNATURE` | regex (below) | module-private |

`RetryPolicy` interface: `{ maxRetries: number; baseDelayMs: number; maxDelayMs: number }`.

`RATE_LIMIT_SIGNATURE` (case-insensitive) — preserve VERBATIM:
```
\b(?:429|529)\b|too many requests|rate[ _-]?limit(?:ed|ing|s)?|quota (?:exceeded|exhausted)|insufficient[_ ]quota|retry[ -]?after\b|resource (?:exhausted|temporarily unavailable)|server is temporarily limiting|\boverloaded\b|you(?:'ve| have) hit your limit|exceed (?:your|the) .{0,40}rate limit
```

### 5.2 Functions

| Function | Signature | Behavior |
|---|---|---|
| `fullJitterDelay(attempt, baseMs, maxMs)` | `(int,int,int) → int` | `ceiling = min(maxMs, baseMs * 2^max(0,attempt))`; return `floor(random()*(ceiling+1))`. attempt 0-based. Full jitter ⇒ uniform in `[0, ceiling]`. |
| `isRetriableModelError(err)` | `(unknown) → bool` | false if user-abort; if OpenAI `APIError`: `status===undefined`(conn/timeout)→true, else `status===429 \|\| status>=500`. Other errors→false. |
| `isRateLimitModelError(err)` | `(unknown) → bool` | `APIError && status===429`. |
| `parseRetryAfterMs(headers)` | `(unknown) → int\|undefined` | prefers `retry-after-ms` (ms); else `retry-after` as seconds (→×1000) or HTTP-date (→`when-now`, floored 0). Tolerates a `.get(k)` object or a lowercased record. |
| `modelRetryDelayMs(attempt, err, policy=MODEL_RETRY_POLICY)` | `(int,unknown,RetryPolicy) → int` | on a 429 with parseable Retry-After → `min(retryAfter, 30_000)`; else `fullJitterDelay`. |
| `abortableSleep(ms, signal?)` | `(int, AbortSignal?) → Promise<void>` | resolves after `ms`, or rejects with an `AbortError`-named error when signal fires; removes its listener on both paths (no leak). |
| `retryModelCall(attempt, {policy?, signal?})` | `(()=>Promise<T>, opts) → Promise<T>` | loop: try; on error stop+throw if `i>=maxRetries`, signal aborted, or non-retriable; else `abortableSleep(modelRetryDelayMs(...))` and retry. NOT for streaming. |
| `isRetriableMcpError(err)` | `(unknown) → bool` | `McpError` with code `RequestTimeout`(-32001) or `ConnectionClosed`(-32000); else message matches `/fetch failed\|ECONNRESET\|ETIMEDOUT\|ECONNREFUSED\|socket hang up\|network error\|timed out/i`. |
| `detectRateLimitSignature(text?)` | `(string\|undefined) → bool` | false if empty; else `RATE_LIMIT_SIGNATURE.test(text)`. Caller passes a bounded RECENT slice (`RATE_LIMIT_TAIL_WINDOW`). |
| `isModelAbort(err)` (private) | | `APIUserAbortError` OR `Error.name==="AbortError"`. |

### 5.3 Go mapping (reliability)

- Package `internal/reliability`.
- Backoff: `math/rand` for full jitter; `2^attempt` via `math.Pow` or bit shift.
- `abortableSleep` → `context.Context`: `select { case <-time.After(d): case <-ctx.Done(): return ctx.Err() }`.
- `retryModelCall[T any](ctx, attempt func() (T,error), policy RetryPolicy, isRetriable func(error) bool)`.
- Error classification: the OpenAI/MCP TS SDK error types don't exist in Go — replace with
  the Go client's error types (e.g. an HTTP status accessor) and an `errors.As`-based
  predicate. Preserve the SEMANTICS: retry on 429/5xx/conn-error/timeout; never on abort or
  other 4xx. MCP codes `-32001`/`-32000` are JSON-RPC error codes (transport-agnostic) — keep.
- `parseRetryAfterMs`: parse `Retry-After-Ms`, `Retry-After` (seconds or `http.ParseTime`).
- **DELETE/REPLACE**: the `AbortSignal.any` leak workaround comment (Node #54614) is
  Node-specific; in Go a single `context.Context` per attempt has no such issue. Keep the
  per-attempt-timeout design (don't compose nested cancels) as a NOTE.

---

## 6. Queue (`src/queue.ts` + `src/storage/db.ts`)

The attention queue: all sub-threads publish here instead of interrupting the main thread.
Deduped by `dedupeKey`; surfaced via `/inbox`.

### 6.1 `Queue` class methods

| Method | Signature | Behavior |
|---|---|---|
| `publish(args)` | `(QueuePublishArgs) → QueueEvent` | `QueuePublishArgs.parse(args)`; `expiresAt = ttlMs ? Date.now()+ttlMs : undefined`; `db.upsertEvent({...})`. |
| `digest(opts={})` | `(QueueDigestOptions) → QueueEvent[]` | `db.listEvents(opts)`. |
| `resolve(id)` | `(string) → bool` | `db.resolveEvent(id)` (stamps `resolvedAt`, returns whether a row changed). |
| `markNotified(ids)` | `(string[]) → void` | `db.markNotified(ids)`. |
| `format(events)` | `(QueueEvent[]) → string` | compact `/inbox` digest (below). |

`format` output (preserve glyphs & layout):
- empty → `"Inbox is empty."`
- per event: `  <icon> <id> <title><target><dup>\n     <summary><evidence>` joined by `\n`.
  - icon by severity: `debug:"·"  info:"ℹ"  done:"✓"  attention:"!"  blocked:"⛔"  urgent:"‼"  error:"✗"`.
  - target suffix: ` [term <terminalId>]` else ` [wt <worktreeId>]` else `""`.
  - dup suffix: ` (×<count>)` when `count > 1`.
  - evidence: `\n     evidence: <e1> | <e2> | …` when present.

### 6.2 `QueueEvent` interface

`id, source(EventSource), severity(Severity), title, summary, target?(EventTarget),
evidence?(string[]), recommendedActions?(RecommendedAction[]), dedupeKey?,
epistemicKind?(EpistemicKind), createdAt(int64), updatedAt?(int64), expiresAt?, resolvedAt?,
count(int)`. `updatedAt` advances on each dedupe bump; `createdAt` stays fixed (recency).

### 6.3 `QueueDigestOptions`

`severityAtLeast?(Severity), maxItems?(int), includeResolved?(bool), notifiedIsNull?(bool)`.

### 6.4 SEVERITY ORDERING — CRITICAL (rank ≠ enum order)

`SEVERITY_ORDER` numeric rank (used for `severityAtLeast` filter AND `ORDER BY ... DESC`):

| Severity | Rank |
|---|---|
| `debug` | 0 |
| `info` | 1 |
| `done` | 2 |
| `attention` | 3 |
| `blocked` | 4 |
| `urgent` | 5 |
| `error` | 6 |

> NOTE: this differs from the `Severity` enum DECLARATION order
> (`debug,info,attention,urgent,blocked,done,error`). The RANK order is authoritative for
> sorting/filtering. The SQL `CASE` mirror uses `ELSE 1` (unknown severity ⇒ treated as
> `info`). Preserve both the map and the `ELSE 1` fallback.

### 6.5 `listEvents` query semantics (preserve)

WHERE: `(expiresAt IS NULL OR expiresAt > now)`; if `!includeResolved` → `resolvedAt IS
NULL`; if `notifiedIsNull` → `notifiedAt IS NULL`; if `severityAtLeast` → `SEV_CASE >=
rank`. ORDER BY `SEV_CASE DESC, COALESCE(updatedAt, createdAt) DESC`. `LIMIT maxItems` when
set. (A recurring deduped event stays near the top because ordering keys on `updatedAt`
while `createdAt` is pinned.)

### 6.6 `upsertEvent` dedupe semantics (preserve)

If `dedupeKey` set, find the newest unresolved, non-expired event with that key:
- If found: `count += 1`, refresh `title/summary/severity`, OVERWRITE `recommendedActions`
  (clear to null when new event has none), set `updatedAt = now`, refresh `expiresAt`, do
  NOT touch `createdAt`. `evidence` and `epistemicKind` fall back to existing value when the
  new publish omits them (so a deduped watcher event never loses its latest
  `VerificationResult` / provenance).
- Else insert a new `evt_<uuid8>` row, `count = ev.count ?? 1`.

> WHY (preserve): the scheduler's "is this new?" check keys on `createdAt`; refreshing it on
> dedupe made recurring events re-notify every tick.

### 6.7 SQLite contract — table/column names (MUST match for schema compat)

`events` table columns: `id, source, severity, title, summary, targetJson, evidenceJson,
recommendedActionsJson, dedupeKey, epistemicKind, createdAt, updatedAt, notifiedAt,
expiresAt, resolvedAt, count`. Indexes: `idx_events_open (resolvedAt, severity, createdAt)`,
`idx_events_dedupe (dedupeKey, resolvedAt)`. Booleans stored as `0/1` INTEGER. Targets/
evidence/recommendedActions stored as JSON-string columns (suffix `Json`).

### 6.8 Go mapping (queue)

- Package `internal/queue` (Queue) over a `storage` DB iface.
- `severityRank map[Severity]int` + a SQL `CASE` builder; keep `ELSE 1`.
- Use `database/sql` + `modernc.org/sqlite` (pure-Go, no cgo) OR `mattn/go-sqlite3`.
  Keep the EXACT table/column names and index definitions for on-disk compat with existing
  `state.db` files (CLAUDE.md says dev hard-resets the DB, so cross-rewrite compat may be
  optional — confirm with maintainer; the names are still the safest default).
- `format` glyphs are multibyte unicode — Go string literals carry them fine.

---

## 7. Watcher Cadence (`src/watcherCadence.ts`)

| Constant | Value | Meaning |
|---|---|---|
| `SCHEDULER_TICK_MS` | `3_000` | scheduler tick; also the FLOOR for any supervisor cadence |
| `SUPERVISOR_DEFAULT_CADENCE_MS` | `3_000` | default for supervisor watchers (CLI-spawned workers); kept equal to the tick so stored cadence is honoured exactly |
| `MONITOR_DEFAULT_CADENCE_MS` | `120_000` | default for user-created background ("monitor") watchers (slow ⇒ low classification cost) |
| `PR_WATCHER_CADENCE_MS` | `60_000` | fixed (not user-configurable) cadence for `pr_state` watchers; each check is a `forge.getPR` call (≈60 req/hr/watcher) |
| `WATCHER_SPAWN_GRACE_MS` | `20_000` | grace after watcher creation during which a target terminal absent from `terminal.getStatus` is "still registering", NOT exited; once observed at least once, absence is a real exit |

Effective check interval = `max(cadenceMs, SCHEDULER_TICK_MS)`.

> Go: package `internal/watcher` constants as `time.Duration` (`3*time.Second`, etc.) OR
> plain `int64` ms to match the DB `cadenceMs` column. Pure values — straight port.

---

## 8. ID & Timestamp Conventions (cross-cutting wire contract)

- **IDs**: `<prefix>_<first 8 hex chars of a v4 UUID>`. Prefixes:
  `tmr_`(timer), `wch_`(watcher), `evt_`(queue event), `aud_`(audit), `rne_`(run event),
  `grt_`(automation grant), `msg_`(conversation message), `rsl_`(skill selection log),
  `wfr_`(workflow run), `rrs_`(skill run state), `mem_`(memory), `agt_`(agent launch).
  > Go: `strings.ReplaceAll(uuid.NewString(),"-","")[:8]` (e.g. `github.com/google/uuid`),
  > or `hex.EncodeToString(crypto/rand 4 bytes)`. The TS slices a hyphenated UUIDv4 string
  > `[:8]` — that's the first 8 hex chars of the time-low field. Match the `<prefix>_` + 8
  > lowercase hex char shape; the exact bytes are random so they need not match.
- **Timestamps**: epoch-ms `int64` (`Date.now()`), everywhere EXCEPT debug-log header/line
  timestamps (ISO-8601 `toISOString()`) and the log-filename date (`YYYY-MM-DD`).

---

## 9. Things to DELETE (do NOT port)

- `AppConfig.splash` + `DAINTREE_ASSISTANT_NO_SPLASH` — OpenTUI splash animation. (Reassess
  only if the Bubble Tea port wants an analogue.)
- `AppConfig.reservedColumns` + `DAINTREE_ASSISTANT_RESERVED_COLUMNS` and its entire
  xterm-overlay-scrollbar rationale — OpenTUI/xterm.js-specific.
- `assistantVersion()`'s `import.meta.url`/`fileURLToPath` package-root walk — replace with
  Go build-time version injection (ldflags / `debug.ReadBuildInfo`).
- The OpenAI-SDK (`APIError`, `APIUserAbortError`) and MCP-SDK (`McpError`, `ErrorCode`)
  type imports in reliability.ts — replace with the Go client equivalents; keep the
  classification SEMANTICS, not the TS types.
- The `AbortSignal.any` leak workaround comment (Node #54614) — irrelevant under Go's
  `context.Context`.
- `dotenv` package — replace with `godotenv` (preserving no-override semantics) or a small
  custom parser.
- React/OpenTUI references in doc comments — none of these modules import UI, but the
  splash/reservedColumns config fields are the only UI coupling and are listed above.

---

## 10. Suggested Go package layout

| Package | Contents |
|---|---|
| `internal/config` | `AppConfig`, `ConfigOverrides`, `LoadConfig`, `DescribeConfig`, `projectIdToDir`, `firstString`, `DEFAULTS`, trusted-env machinery |
| `internal/domain` | all enums, DB-row structs, `ToolResult`/`ToolError`, `WatchCondition` + decoder, JSON contract structs, `classificationEpistemicKind`, `isCompositeCondition`, constants (`VERIFICATION_EVIDENCE_PREFIX`, `JSON_OUTPUT_SCHEMA_VERSION`, `ONE_SHOT_EXIT_CODE`) |
| `internal/projectinstructions` | `LoadProjectInstructions`, filename + max-bytes constants |
| `internal/debuglog` | `LogDebug`, `StartDebugLog`, `CurrentDebugLogPath`, pruning, constants |
| `internal/reliability` | retry/backoff/timeout/rate-limit helpers + policies/constants |
| `internal/queue` | `Queue`, severity rank/`CASE`, `format`, `QueueDigestOptions` |
| `internal/watcher` (or `internal/cadence`) | cadence constants |
