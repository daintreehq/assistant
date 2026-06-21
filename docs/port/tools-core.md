# Port-spec: Tools Core (ToolRegistry, ToolResult, ToolContext, Safety Policy)

Authoritative reference for porting the tool dispatch + safety subsystem to Go
(Bubble Tea UI). Sourced verbatim from:

- `src/tools/registry.ts` — `ToolRegistry` (projection, dispatch pipeline, audit)
- `src/tools/types.ts` — `ToolContext`, `ToolDef`, `ConfirmRequest`, `ok`/`fail`, `NO_ARGS`
- `src/safety/policy.ts` — tier gating, `ALWAYS_CONFIRM`, no-file-edit guard, secret-file guards, path containment
- `src/schemas.ts` — `RiskClass`, `Tier`, `ToolResult`/`ToolError`, `AuditRecord`, `AutomationGrant*`
- `src/tools/mcpTools.ts` — `daintree.call` denylist (`WRAPPED_MCP_TOOLS`) + `USE_TYPED_WRAPPER`/`FILE_EDIT_FORBIDDEN` guards
- `src/storage/db.ts` — `audit_log` schema, `insertAudit`, `consumeGrant`, `grantAuthorizes`

This subsystem is the choke point: **every** tool invocation (from the agent
loop, watchers, timers, workflows) flows through `ToolRegistry.dispatch`, which
validates args, applies the safety policy, runs the handler, and writes an audit
row. The no-file-edit invariant and tier/confirmation matrix live here.

---

## 1. Core enums & value sets (MUST stay wire/schema-compatible)

These strings appear in JSON tool schemas, the `audit_log` table, debug-log JSONL,
and grant rows. Preserve the exact spelling and ordering.

### 1.1 `RiskClass` (8 values — `schemas.ts` `RiskClass`)

| Value      | Meaning (comment verbatim)                                  |
|------------|-------------------------------------------------------------|
| `read`     | no state mutation                                           |
| `local`    | mutates only CLI daemon state (timers/watchers/queue)       |
| `ui`       | focuses/opens panels, changes Daintree UI state             |
| `terminal` | sends input / spawns visible terminals                      |
| `project`  | creates/deletes worktrees, runs recipes                     |
| `git`      | stages/commits/pushes/reverts                               |
| `external` | forge / network / provider actions                          |
| `system`   | destructive or broad actions                                |

Declaration order is `read, local, ui, terminal, project, git, external, system`
(note: enum order differs from the `TIER_ALLOWED` set-build order, which lists
`external` before `git`; ordering is irrelevant to behavior — use sets).

### 1.2 `Tier` (3 values — `schemas.ts` `Tier`)

`supervisor` | `operator` | `system`. Default tier is `system`
(`DAINTREE_ASSISTANT_TIER`, default `system`).

### 1.3 `ToolActor` (5 values — `types.ts`)

`main` | `watcher` | `timer` | `workflow` | `system`.
`main` is the only **interactive** actor; the rest are **non-interactive** and
can never run a confirm-required tool without a scoped automation grant.

### 1.4 Audit outcome (5 values — `AuditRecord.outcome`, `audit_log.outcome`)

`ok` | `error` | `denied` | `dedup` | `grant_ok`.
(`dedup` is defined in the type but never written by the dispatch path in
registry.ts — it is reserved/used elsewhere.)

### 1.5 `AutomationGrantActorType` / `AutomationGrantSource`

- Actor type: `watcher` | `timer`.
- Source: `local` | `daintree`. Only `local` is mintable today; `daintree` is a
  reserved column for a future Daintree-backed grant.

---

## 2. `ToolResult` envelope (the universal return type)

### 2.1 `ToolError` (`schemas.ts`)

| Field         | Type      | Notes                                  |
|---------------|-----------|----------------------------------------|
| `code`        | string    | machine code (e.g. `INVALID_ARGS`)     |
| `message`     | string    | human/LLM-facing                       |
| `recoverable` | bool      | can the model retry/recover?           |
| `details`     | unknown?  | optional structured detail (Zod issues, MCP structuredContent) |

### 2.2 `ToolResult<T>` (`schemas.ts`)

| Field     | Type        | Notes                                         |
|-----------|-------------|-----------------------------------------------|
| `ok`      | bool        | success flag                                  |
| `result`  | T?          | payload on success                            |
| `error`   | ToolError?  | present on failure                            |
| `summary` | string      | REQUIRED — one-line human/LLM-facing summary  |
| `auditId` | string?     | id of the `audit_log` row written for this call; **mutated onto the result after the handler returns** (see §6) |

### 2.3 Constructors (`types.ts`)

```
ok<T>(summary: string, result?: T): ToolResult<T>
  → { ok: true, summary, result }

fail(code: string, message: string,
     opts?: { recoverable?: boolean; details?: unknown }): ToolResult
  → { ok: false, summary: message, error: { code, message,
        recoverable: opts.recoverable ?? true, details: opts.details } }
```

Critical contracts:
- `fail` sets `summary === message` (the error message doubles as the summary).
- `recoverable` **defaults to `true`** when not specified. Several call sites pass
  `recoverable: false` explicitly (see §3.2 error-code table) — preserve each.
- Handlers **never throw to the caller**; they return `fail(...)`. The registry's
  `runHandler` catches any throw and converts it to `fail("TOOL_THREW", message,
  { recoverable: true })`.

---

## 3. `ToolRegistry` (registry.ts)

### 3.1 Internal state & wire-name mapping

| Field            | Type                 | Purpose                                              |
|------------------|----------------------|------------------------------------------------------|
| `tools`          | `Map<string,ToolDef>`| internal dotted name → def                           |
| `wireToInternal` | `Map<string,string>` | OpenAI wire name → internal name (rebuilt per projection) |
| `internalToWire` | `Map<string,string>` | internal name → wire name (rebuilt per projection)   |

**Wire-name rule (load-bearing).** OpenAI / Fireworks constrain function names to
`OPENAI_NAME_RE = /^[a-zA-Z0-9_-]{1,64}$/`. Internal tool names use dot notation
(`fs.read`, `daintree.call`). `toWireName(name)` replaces **every** `.` with `__`
(`name.replaceAll(".", "__")` → `fs__read`, `daintree__call`). `resolveWireName`
reverses it via the map (NOT by string substitution — collisions are possible).

Methods:

| Method | Signature | Behavior |
|--------|-----------|----------|
| `register` | `(tool: ToolDef) → void` | inserts; **throws** `Duplicate tool registration: <name>` if name already present. |
| `registerAll` | `(tools: ToolDef[]) → void` | loops `register`. |
| `get` | `(name) → ToolDef \| undefined` | by internal name. |
| `list` | `() → ToolDef[]` | all values. |
| `assertSafe` | `() → void` | calls `assertNoFileEditTools([...keys])` — enforces no-file-edit at startup (see §5). |
| `toOpenAITools` | `(filterNames?: string[]) → ChatTool[]` | projects to OpenAI function specs; rebuilds both alias maps; **throws** on illegal wire name or wire-name collision (fail-fast at projection). `filterNames` matches **internal** names. |
| `resolveWireName` | `(wireName) → string \| undefined` | maps wire→internal from the **most recent** `toOpenAITools` projection. Unknown ⇒ `undefined`. |
| `dispatch` | `(name, rawArgs, ctx) → Promise<ToolResult>` | the pipeline (§4). Never throws. |

**`toOpenAITools` projection rules** (fail-fast, do NOT silently drop):
1. Select tools whose internal name ∈ `filterNames` (or all if no filter).
2. For each: `wire = toWireName(name)`. If `!OPENAI_NAME_RE.test(wire)` → throw
   ``Tool '<name>' produces wire name '<wire>', which does not match <re>``.
3. If two distinct internal names produce the same wire name → throw
   ``Wire-name collision: '<a>' and '<b>' both map to '<wire>'``.
4. Replace `this.wireToInternal`/`this.internalToWire` wholesale (so a later
   narrowed projection never resolves a name from a previous wider one).
5. Emit `{ type: "function", function: { name: wire, description, parameters } }`.

`ChatTool` shape (from `models/fireworks.ts`): `{ type: "function"; function: {
name; description; parameters } }`. `parameters` is the raw JSON Schema object
from `ToolDef.parameters`.

### 3.2 Error codes produced by `dispatch`/`runHandler` (verbatim)

| Code | recoverable | Where | Audit outcome |
|------|-------------|-------|---------------|
| `UNKNOWN_TOOL` | `false` | tool not in map | `error` |
| `INVALID_ARGS` | `true` | Zod `safeParse` failed; `details` = Zod issues array | `error` |
| `TIER_DENIED` | `false` | `decide().allowed === false`; message = `decision.reason` | `denied` |
| `CONFIRMATION_REQUIRED` | `false` | non-`main` actor, no grant; message: ``<name> (<risk>) needs user confirmation and cannot be run by a non-interactive '<actor>' actor.`` | `denied` |
| `USER_DECLINED` | `true` | `main` actor declined the confirm prompt | `denied` |
| `TOOL_THREW` | `true` | handler threw; message = `err.message` | `error` |

`daintree.call`-specific codes (mcpTools.ts, §7): `USE_TYPED_WRAPPER`,
`FILE_EDIT_FORBIDDEN` (recoverable:false), `MCP_UNAVAILABLE`, `MCP_TOOL_ERROR`,
`CANCELLED` (recoverable:false).

---

## 4. Dispatch pipeline — exact ordering (registry.ts `dispatch`)

`started = Date.now()` is captured FIRST and threaded through every audit row
(`durationMs = Date.now() - started`). Order is load-bearing:

1. **Tool lookup.** `tool = tools.get(name)`. Missing → `fail("UNKNOWN_TOOL",
   "No such tool: <name>", { recoverable:false })`, audit, return.

2. **Arg validation.** `args = rawArgs ?? {}`. If `tool.schema` present, run
   `schema.safeParse(args)`. On failure → `fail("INVALID_ARGS", "Invalid
   arguments for <name>: <issues>", { recoverable:true, details: issues })`,
   audit, return. On success `args = parsed.data` (the **parsed/coerced** data,
   not the raw input — Zod transforms/defaults are applied). No schema ⇒ args
   passed through unvalidated.
   - Issues are joined with `; ` via `summarizeIssue` (§4.1).

3. **Tier gate.** `decision = decide(tool.risk, ctx.config.tier)` (§5.1). If
   `!decision.allowed` → `fail("TIER_DENIED", decision.reason ?? "denied",
   { recoverable:false })`, audit as `denied`, return.

4. **Confirmation (only if `decision.needsConfirmation`):**
   - **Branch A — non-interactive actor (`ctx.actor !== "main"`):**
     - If `ctx.actorId` set: try `ctx.db.consumeGrant(actorId, actor, name,
       tool.risk, started)`. If a grant is returned → `runHandler(..., "grant_ok",
       grant)` (success audited as `grant_ok`, stamping `grantSource`/`grantId`).
     - Else (no actorId, or no matching grant) → `fail("CONFIRMATION_REQUIRED",
       …, { recoverable:false })`, audit as `denied`, **then publish a queue event**
       (best-effort, wrapped in try/catch — must never break the call):
       ```
       { source:"system", severity:"info",
         title:`Autonomous action blocked: <name>`,
         summary: res.summary,
         dedupeKey:`denied:<actor>:<actorId? actorId+':' : ''><name>` }
       ```
       The `dedupeKey` is intentionally **tick-free** so repeated denials of the
       same (actor, tool) collapse into one count-bumped inbox row; the actorId
       segment keeps distinct watchers/timers from collapsing together.
   - **Branch B — interactive `main` actor:**
     - If `ctx.config.autoApprove` (`DAINTREE_ASSISTANT_AUTO_APPROVE`) → skip the
       Y/N step, `runHandler(...)` (audited as normal `ok`). The tier gate above
       already bounded what's permitted; auto-approve only removes the prompt.
     - Else call `ctx.confirm({ toolName: name, risk: tool.risk, summary:
       tool.description, consequence: tool.consequence, args })`.
       - **A thrown confirm prompt is treated as a decline** (`approved = false`),
         never an approval.
       - If not approved → `fail("USER_DECLINED", "User declined <name>.",
         { recoverable:true })`, audit as `denied`, return.

5. **Run.** `runHandler(tool, name, args, ctx, started)` (default `okOutcome:"ok"`).

### 4.1 `summarizeIssue` — Zod issue → one-line model-actionable message

Non-obvious "why" worth preserving: Zod collapses a failed `z.union(...)` to the
useless `"Invalid input"`, which stranded the watcher loop (model sent `stopWhen:
{}`, got no steer, repeated the identical broken call until the turn died).

Logic:
- `path = issue.path.join(".")`.
- If `issue.code === "invalid_union"` **and** `issue.unionErrors` present:
  - `depth = issue.path.length`.
  - Collect, across all union branches, every `sub.path[depth]` where
    `sub.path.length > depth`, into a `Set<string>` of discriminating keys.
  - If non-empty: return
    ``<path>: the value matched none of the allowed shapes — provide an object
    with exactly one of these keys: <sorted keys joined by ", ">``.
- Otherwise fall back to `` `<path>: <issue.message>` ``.

Go note: zod has no Go equivalent. If validation uses a different library, this
exact union-disambiguation message can be approximated; the **wording is a
prompt-engineering contract** the model relies on for self-correction — keep the
"provide an object with exactly one of these keys" phrasing.

---

## 5. Safety policy (policy.ts)

### 5.1 Tier gating & confirmation matrix

`TIER_ALLOWED: Record<Tier, ReadonlySet<RiskClass>>` — which risk classes each
tier may perform **at all**:

| Tier | Allowed risk classes |
|------|----------------------|
| `supervisor` | `read, local, ui` |
| `operator` | `read, local, ui, terminal, project, external` |
| `system` | `read, local, ui, terminal, project, external, git, system` |

Note: `operator` does **not** include `git` or `system`; only `system` tier does.

`ALWAYS_CONFIRM: ReadonlySet<RiskClass>` — classes that always require explicit
confirmation before running: **`terminal, project, git, external, system`**.
(`read`, `local`, `ui` never require confirmation.)

```
tierAllowsRisk(tier, risk) → bool        // TIER_ALLOWED[tier].has(risk)

decide(risk, tier, opts?: { hasScopedApproval?: boolean }) → PolicyDecision
  1. if !tierAllowsRisk(tier, risk):
       { allowed:false, needsConfirmation:false,
         reason:`'<risk>' actions require a higher tier than '<tier>'. Switch tier with /permissions.` }
  2. needsConfirmation = ALWAYS_CONFIRM.has(risk) && !opts.hasScopedApproval
     return { allowed:true, needsConfirmation }
```

`PolicyDecision`: `{ allowed: bool; needsConfirmation: bool; reason?: string }`.

> The `hasScopedApproval` opt exists but `registry.dispatch` does NOT pass it —
> the grant check happens **after** `decide()` in the registry, not inside it.
> Preserve the parameter for callers that pre-resolve approval, but the dispatch
> path always calls `decide(tool.risk, ctx.config.tier)` (2 args).

### 5.2 No-file-edit guard (HARD INVARIANT)

`FileEditAttemptError` (Error subclass): `code = "FILE_EDIT_FORBIDDEN"`,
`name = "FileEditAttemptError"`.

**`FORBIDDEN_TOOL_FRAGMENTS` (verbatim, 11 fragments):**

```
write_file
writefile
apply_patch
applypatch
edit_file
editfile
fs.write
fs.edit
file.write
file.edit
patch.apply
```

```
isForbiddenToolName(name) → bool
  // n = name.toLowerCase(); returns FRAGMENTS.some(frag => n.includes(frag))
  // substring match, case-insensitive.

assertNoFileEditTools(toolNames) → void
  // offenders = toolNames.filter(isForbiddenToolName)
  // if offenders.length > 0: throw FileEditAttemptError(
  //   `Refusing to register file-mutating tools: <offenders joined ", ">. The CLI must delegate edits to a spawned agent.`)
```

`isForbiddenToolName` is ALSO applied at runtime inside `daintree.call` (§7) to a
raw forwarded MCP tool name — the registration-time guard only covers local tool
names, so the escape hatch re-checks.

### 5.3 Secret-file guards (read-only leak prevention)

Used by the read-only `fs` tools and recursive search to refuse reading
credential-bearing files (they'd leak into the durable audit log / conversation
history). Matching is case-insensitive.

**`SECRET_BASENAMES` (Set, exact basename match):**
```
.env  .envrc  .npmrc  .netrc  .pgpass  .htpasswd
credentials  credentials.json
id_rsa  id_dsa  id_ecdsa  id_ed25519
.dockercfg  .git-credentials
secrets.json  secrets.yaml  secrets.yml
service-account.json  serviceaccount.json
```

**`SECRET_SUFFIXES` (basename endsWith):**
```
.pem  .key  .p12  .pfx  .keystore  .jks  .asc  .gpg  .ppk
```

**`SECRET_DIR_SEGMENTS` (Set, path segment match):**
```
.ssh  .aws  .gnupg  .kube  .azure  .gcloud  .docker
```

```
isSensitiveSegment(seg) → bool        // caller passes a LOWERCASED segment
  if SECRET_DIR_SEGMENTS.has(seg): true
  if seg === ".env" || seg.endsWith(".env") || seg.startsWith(".env."): true
  else false

isSensitivePath(relOrPath) → bool
  lower = relOrPath.toLowerCase()
  base  = path.basename(lower)
  if SECRET_BASENAMES.has(base): true
  if SECRET_SUFFIXES.some(s => base.endsWith(s)): true
  // EVERY path segment is checked (split on / or \) so a sensitive file/dir
  // anywhere in the path is caught: nested/.env/x, home/.aws/credentials, config/prod.env
  return lower.split(/[\\/]/).some(isSensitiveSegment)
```

### 5.4 Project-root path containment

```
resolveInsideProject(projectPath, rel) → string   // throws FileEditAttemptError on escape
  lexicalRoot = path.resolve(projectPath)
  target      = path.resolve(lexicalRoot, rel)
  assertInside(lexicalRoot, target, rel)                          // (1) lexical
  assertInside(realpath(lexicalRoot), realpath(target), rel)      // (2) symlink-resolved
  return target
```

- `assertInside(root, p, rel)`: normalize `root` to end with the OS path
  separator; throw `FileEditAttemptError("Path escapes the project root: <rel>")`
  unless `p === root` or `p.startsWith(root + sep)`.
- `realpathOfExisting(p)`: realpath the **nearest existing ancestor** and
  re-append the missing remainder, so a repo-local symlink can't point outside the
  project (e.g. to `/etc/passwd`) while benign system symlinks (`/tmp →
  /private/tmp`) on both sides cancel out. Two-pass (lexical THEN symlink) is
  deliberate: lexical catches `../` traversal even for not-yet-existing paths.

Go mapping: `filepath.Abs`/`filepath.Clean` for lexical, `filepath.EvalSymlinks`
on the nearest existing ancestor for the symlink pass. Use `os.PathSeparator`.

---

## 6. Audit logging (registry.ts `runHandler` + `audit`)

### 6.1 `runHandler`

```
runHandler(tool, name, args, ctx, started,
           okOutcome: "ok"|"grant_ok" = "ok", grant?) → Promise<ToolResult>
```
- `try { res = await tool.handler(args, ctx) }` → audit with outcome
  `res.ok ? okOutcome : "error"` (a grant-authorized **failure** is still audited
  as `error`, not `grant_ok`), return `res`.
- `catch (err)` → `res = fail("TOOL_THREW", err.message)`, audit as `error`,
  return. **Never rethrows.**

### 6.2 `audit` (best-effort; must never break a tool call)

Two side-channels, both wrapped so a failure can't break the call:

1. **Debug log** (`logDebug(ctx.config, "tool.call", {...})`) — full-fidelity,
   **untruncated** args+result. JSONL event name = `tool.call`. Payload fields:
   `tool, actor, actorId, outcome, ok, durationMs, summary, args, result, error`.

2. **`ctx.db.insertAudit({...})`** wrapped in try/catch (`/* auditing must never
   break a tool call */`). On success, `res.auditId = row.id` is **mutated onto
   the returned ToolResult**. Insert fields:
   - `actor: ctx.actor`
   - `toolName: name`
   - `argsJson: capJson(safeJson(args))`
   - `outcome`
   - `durationMs: Date.now() - started`
   - `summary: res.summary`
   - `resultJson: res.result !== undefined ? capJson(safeJson(res.result)) : undefined`
   - `grantSource: outcome === "grant_ok" ? grant?.source : undefined`
   - `grantId:    outcome === "grant_ok" ? grant?.id     : undefined`
   - `runId: ctx.runId`

   Grant provenance is stamped **only** on a `grant_ok` row, so a non-grant row
   never carries a misleading source.

### 6.3 JSON capping (`MAX_AUDIT_JSON = 4000`)

```
safeJson(v) → string    // JSON.stringify(v) ?? "null"; on throw → '"<unserializable>"'

capJson(s) → string     // MAX_AUDIT_JSON = 4000 bytes (chars)
  if s.length <= 4000: return s
  return JSON.stringify({
    truncated: true,
    bytes: Buffer.byteLength(s, "utf8"),        // UTF-8 byte length of the FULL blob
    preview: s.slice(0, 4000 - 200)             // = first 3800 chars, headroom for wrapper+escaping
  })
```
"Why": tool results can carry large file contents / terminal scrollback; the audit
keeps a redacted preview + byte count so a single read can't bloat the DB.
Go: `len(s)` (bytes) vs the char-length distinction — TS uses `.length` (UTF-16
code units) for the threshold/slice and `Buffer.byteLength` (UTF-8) for `bytes`.
For a faithful port, threshold on rune-count or byte-count consistently; the
`bytes` field must be UTF-8 byte length. The 3800-char preview floor is what keeps
the stored row near the 4000 cap rather than ballooning.

### 6.4 `audit_log` table (db.ts — MUST stay schema-compatible)

```sql
CREATE TABLE IF NOT EXISTS audit_log (
  id TEXT PRIMARY KEY,        -- "aud_" + first 8 chars of a uuid
  ts INTEGER NOT NULL,        -- wall-clock ms (Date.now())
  actor TEXT NOT NULL,        -- main|watcher|timer|workflow|system
  toolName TEXT NOT NULL,
  argsJson TEXT NOT NULL,
  outcome TEXT NOT NULL,      -- ok|error|denied|dedup|grant_ok
  durationMs INTEGER NOT NULL,
  summary TEXT NOT NULL,
  resultJson TEXT,            -- nullable
  grantSource TEXT,           -- nullable; only set on grant_ok
  grantId TEXT,               -- nullable; only set on grant_ok
  runId TEXT                  -- nullable
);
CREATE INDEX IF NOT EXISTS idx_audit_ts ON audit_log (ts);
```
`AuditRecord` id format: `` `aud_${randomUUID().slice(0,8)}` ``. Timestamps are
**integer epoch milliseconds** everywhere (`Date.now()`), not RFC3339 strings.

---

## 7. `daintree.call` escape hatch — denylist & guards (mcpTools.ts)

`daintree.call` is the raw passthrough to **any** Daintree MCP tool: risk
`system`, always confirmed, requires `system` tier. Args schema `CallArgs`:
`{ name: string; arguments?: Record<string,unknown>; requestKey?: string }`.
JSON Schema: `additionalProperties:false`, `properties.arguments` is an object
with `additionalProperties:true`, `required:["name"]`.

Handler ordering (each guard returns before the actual MCP call):

1. **Typed-wrapper denylist.** `wrapper = WRAPPED_MCP_TOOLS[args.name]`. If found
   → `fail("USE_TYPED_WRAPPER", "Do not call <name> through daintree.call — use
   the typed wrapper instead: <wrapper>. It takes named, validated parameters, so
   you can't drop a required argument. Switch tools; do not retry this raw call.")`.
   (default recoverable:true.)

   **`WRAPPED_MCP_TOOLS` (raw MCP name → redirect text), verbatim keys:**

   | Raw MCP tool name | Redirect wrapper |
   |-------------------|------------------|
   | `agent.launch` | `agentTask.spawnForEdits` (mode "explore"/"edit") |
   | `terminal.getOutput` | `terminal.read` / `terminal.summarize` / `terminal.extract` |
   | `panel.focus` | `terminal.focus` |
   | `terminal.sendCommand` | `terminal.sendCommand` (typed wrapper) |
   | `terminal.arm` | `terminal.arm` |
   | `terminal.disarm` | `terminal.disarm` |
   | `terminal.disarmAll` | `terminal.disarmAll` |
   | `copyTree.injectToTerminal` | `copyTree.injectToTerminal` |
   | `copyTree.generateAndCopyFile` | `copyTree.generateAndCopyFile` |
   | `git.snapshotRevert` | `git.snapshotRevert` |
   | `git.snapshotDelete` | `git.snapshotDelete` |

   "Why": the escape hatch invites two recurring failure modes — reaching for it
   when a wrapper exists, then sending `arguments: {}` and retrying the identical
   broken call. Keep this in sync with the wrappers and with `daintreeMcp.ts`.

2. **No-file-edit re-check.** `if (isForbiddenToolName(args.name))` →
   `fail("FILE_EDIT_FORBIDDEN", "Refusing to call <name> via daintree.call — the
   assistant never edits files directly. Spawn a visible agent
   (agentTask.spawnForEdits) to make changes.", { recoverable:false })`.

3. **Connectivity.** `if (!ctx.mcp.isConnected())` →
   `fail("MCP_UNAVAILABLE", "Daintree MCP is not connected; cannot call <name>.")`.

4. **Forward.** `callArgs = { ...(args.arguments ?? {}), ...(args.requestKey ? {
   requestKey: args.requestKey } : {}) }` → `ctx.mcp.callTool(name, callArgs,
   ctx.signal)`.
   - `res.isError` → `fail("MCP_TOOL_ERROR", res.text || "...returned an error.",
     { details: { structuredContent: res.structuredContent } })`.
   - else → `ok("Called <name>.", { text, structuredContent, isError })`.
   - on throw, if `ctx.signal?.aborted` → `fail("CANCELLED", "Turn cancelled
     during <name>.", { recoverable:false })`, else `fail("MCP_TOOL_ERROR",
     "Daintree MCP call <name> failed: <msg>")`.

---

## 8. `ToolDef` & `ToolContext` (types.ts)

### 8.1 `ToolDef<A>`

| Field | Type | Notes |
|-------|------|-------|
| `name` | string | internal **dotted** name |
| `description` | string | model-facing; can be long/instructional |
| `risk` | RiskClass | drives §5 |
| `consequence?` | string | short human Y/N prose for the approval sheet; UI falls back to a per-risk phrase if absent |
| `parameters` | `Record<string,unknown>` | raw JSON Schema for OpenAI `parameters` |
| `schema?` | `z.ZodType<A>` | optional runtime validation of parsed args |
| `handler` | `(args: A, ctx: ToolContext) => Promise<ToolResult>` | the implementation |

`NO_ARGS` constant: `{ type:"object", properties:{}, additionalProperties:false }`
— standard empty-object schema for no-arg tools.

### 8.2 `ConfirmRequest` (passed to `ctx.confirm`)

`{ toolName: string; risk: RiskClass; summary: string; args: unknown;
consequence?: string }`. The approval sheet leads with `consequence` (a
plain-English statement of the actual effect / reversibility / secret exposure),
falling back to a per-risk-class phrase when absent. Keep it one short truncated
line.

### 8.3 `ToolContext` — everything a handler can reach (built once at startup)

Required:

| Field | Type | Notes |
|-------|------|-------|
| `config` | `AppConfig` | carries `tier`, `autoApprove`, etc. (read in dispatch) |
| `mcp` | `DaintreeMcpClient` | MCP transport |
| `db` | `Db` | storage; `consumeGrant`/`insertAudit` used by registry |
| `queue` | `Queue` | attention queue; registry publishes denial events |
| `router` | `ModelRouter` | model access |
| `projectPath` | string | project root (used by fs path containment) |
| `actor` | `ToolActor` | gates confirmation branch |
| `confirm` | `(ConfirmRequest) => Promise<boolean>` | approve a mutating action |
| `log` | `(msg: string) => void` | out-of-band line to the user |

Optional (per-turn / per-actor / test-stripped — handlers fail gracefully when
absent):

| Field | Type | Set by / used for |
|-------|------|-------------------|
| `sessionId?` | string | skill step-progress checkpoints keyed to live session |
| `actorId?` | string | `wch_…`/`tmr_…` of the non-interactive actor; **required for grant lookup** in dispatch Branch A |
| `runId?` | string | one `AgentSession.send()` turn; stamped on each audit row to group a turn |
| `signal?` | `AbortSignal` | Escape-to-cancel; long handlers thread it and return `fail("CANCELLED", …)` |
| `activeToolNames?` | string[] | tools offered this turn; discovery tools mark `callable`. `undefined` ⇒ all callable |
| `daemonActive?` | `() => boolean` | whether scheduler is running; absent ⇒ assume active |
| `artifactStore?` | `Map<string,string>` | session-scoped oversized-result store (`artifact_…` ids); `artifact.read` pages it |
| `skillSource?` | `SkillSource` | read-only skill library view for `skill.load` |
| `loadSkills?` | `(ids: string[]) => string[]` | merge skill ids mid-turn; main actor only |
| `findSkills?` | `(query, signal?) => Promise<SkillFindResult>` | NL→skills resolution; main actor only |

---

## 9. Automation grants (db.ts — the non-interactive escape valve)

`AutomationGrantRecord`:

| Field | Type | Notes |
|-------|------|-------|
| `id` | string | grant id |
| `actorId` | string | `wch_…` or `tmr_…` |
| `actorType` | `watcher`\|`timer` | |
| `allowedRiskClassesJson` | `string \| null` | JSON `RiskClass[]` or null |
| `allowedToolNamesJson` | `string \| null` | JSON `string[]` or null |
| `expiresAt` | number | wall-clock ms |
| `maxUses` | number | |
| `usesRemaining` | number | |
| `revokedAt` | `number \| null` | explicit revocation only; use-exhaustion does NOT stamp it |
| `createdAt` | number | |
| `source` | `local`\|`daintree` | always `local` today |

**Authorization (union semantics):** `grantAuthorizes(g, toolName, riskClass)` →
true if `toolName ∈ allowedToolNamesJson` **OR** `riskClass ∈
allowedRiskClassesJson`. At least one list must be non-empty (enforced in the TS
layer at mint time, not the schema).

**`consumeGrant(actorId, actorType, toolName, riskClass, now=Date.now())`** — the
atomic consume the dispatch path calls:
- Prepared UPDATE: `UPDATE automation_grants SET usesRemaining = usesRemaining - 1
  WHERE id = ? AND usesRemaining > 0 AND revokedAt IS NULL AND expiresAt > ?`.
- Iterate `listGrants(actorId, now)`: skip if `actorType` mismatches or
  `!grantAuthorizes(...)`; run the UPDATE with `(g.id, now)`; **first grant whose
  UPDATE affects a row wins** → return `getGrant(g.id)`. None → `undefined`.
- The `WHERE` clause is what makes consume atomic against TTL/revocation/exhaustion
  races. In Go, run it inside a transaction (or rely on SQLite's single-writer
  serialization) and read back the row only on `changes > 0`.

Related: `revokeGrant(id, now)` (stamps `revokedAt` if still live, returns bool),
`revokeGrantsByActor(actorId, now)` (revokes all live grants for an actor — called
on watcher/timer stop/cancel; returns count).

---

## 10. Magic constants / limits (exhaustive)

| Constant | Value | Location | Meaning |
|----------|-------|----------|---------|
| `OPENAI_NAME_RE` | `/^[a-zA-Z0-9_-]{1,64}$/` | registry.ts | legal OpenAI function-name pattern |
| wire-name transform | `"."` → `"__"` (all) | registry.ts `toWireName` | dotted→wire |
| `MAX_AUDIT_JSON` | `4000` | registry.ts | max audited JSON length (chars); preview = first `3800` |
| preview headroom | `200` | registry.ts | `MAX_AUDIT_JSON - 200` for wrapper+escaping |
| `FORBIDDEN_TOOL_FRAGMENTS` | 11 fragments (§5.2) | policy.ts | file-edit denylist |
| `SECRET_BASENAMES` | 18 entries (§5.3) | policy.ts | credential basenames |
| `SECRET_SUFFIXES` | 9 entries (§5.3) | policy.ts | credential suffixes |
| `SECRET_DIR_SEGMENTS` | 7 entries (§5.3) | policy.ts | credential dirs |
| `WRAPPED_MCP_TOOLS` | 11 entries (§7) | mcpTools.ts | daintree.call denylist |
| audit id prefix | `aud_` + 8 uuid chars | db.ts | |
| `recoverable` default | `true` | types.ts `fail` | |
| `decide` opt | `hasScopedApproval?` (unused by dispatch) | policy.ts | |

No timeouts/cadences live in these files (the scheduler's 3s tick and watcher
cadences are elsewhere).

---

## 11. External contracts (preserve byte/wire/schema-for-schema)

- **Tool names** (internal dotted) and their **wire form** (`.`→`__`). The wire
  form is what the model sees and returns; the round-trip via the alias map MUST
  be lossless. Tool **JSON Schemas** (`ToolDef.parameters`) and **risk classes**
  are part of the model-facing contract.
- **`audit_log`** table + columns + id format + integer-ms timestamps (§6.4).
- **`automation_grants`** semantics (union authorization, atomic consume WHERE
  clause) (§9).
- **Debug-log JSONL event name** `tool.call` and its payload keys (§6.2).
- **Error codes** are model-facing recovery signals — exact strings
  (`INVALID_ARGS`, `TIER_DENIED`, `CONFIRMATION_REQUIRED`, `USER_DECLINED`,
  `TOOL_THREW`, `UNKNOWN_TOOL`, `USE_TYPED_WRAPPER`, `FILE_EDIT_FORBIDDEN`,
  `MCP_UNAVAILABLE`, `MCP_TOOL_ERROR`, `CANCELLED`).
- **Queue denial event** shape + tick-free `dedupeKey` (§4 Branch A).
- **Env vars** read here: `DAINTREE_ASSISTANT_TIER` (→ `config.tier`),
  `DAINTREE_ASSISTANT_AUTO_APPROVE` (→ `config.autoApprove`). Resolution order
  (from config.ts): CLI overrides → env → project `.env` → assistant's own `.env`
  → defaults.
- **The `/permissions` reference** in the `TIER_DENIED` reason and `/clear`-style
  command surface are CLI-command names the message text refers to.

---

## 12. Proposed Go mapping

### Packages

- `internal/tools` — `ToolDef`, `ToolContext`, `ToolResult`/`ToolError`, `Ok`/
  `Fail`, `NoArgs`, `ToolRegistry`, wire-name mapping, dispatch pipeline, audit.
- `internal/safety` — `RiskClass`/`Tier` consts, `TierAllowed`/`AlwaysConfirm`
  sets, `Decide`, `PolicyDecision`, `FileEditAttemptError`, `IsForbiddenToolName`,
  `AssertNoFileEditTools`, secret-file guards, `ResolveInsideProject`.
- `internal/schema` (or reuse `safety`) — the enum string consts shared across
  storage/tools.

### Key Go types

```go
// RiskClass, Tier, ToolActor, AuditOutcome: string-typed consts.
type RiskClass string   // "read","local",... 8 values
type Tier string        // "supervisor","operator","system"

type ToolError struct {
    Code        string `json:"code"`
    Message     string `json:"message"`
    Recoverable bool   `json:"recoverable"`
    Details     any    `json:"details,omitempty"`
}
type ToolResult[T any] struct {
    Ok      bool       `json:"ok"`
    Result  *T         `json:"result,omitempty"`
    Error   *ToolError `json:"error,omitempty"`
    Summary string     `json:"summary"`
    AuditID string     `json:"auditId,omitempty"`
}

type Handler func(ctx context.Context, tctx *ToolContext, rawArgs json.RawMessage) ToolResult[any]

type ToolDef struct {
    Name        string
    Description string
    Risk        RiskClass
    Consequence string
    Parameters  map[string]any        // raw JSON Schema
    Validate    func(json.RawMessage) (json.RawMessage, error) // optional; replaces Zod
    Handler     Handler
}
```

Notes:
- **Generics caveat:** `ToolResult[T]` with a type param fights the heterogeneous
  registry. Prefer a single `ToolResult` with `Result any` (untyped) for the
  dispatch path; the generic `Ok[T]` constructor can still exist for ergonomics
  but dispatch stores `any`.
- **`context.Context` replaces `AbortSignal`** — thread it into MCP/terminal calls;
  on `ctx.Err()` return `Fail("CANCELLED", …, recoverable:false)`.
- **`confirm`** becomes a `func(ConfirmRequest) (bool, error)`; a returned error is
  treated as a decline (mirror the TS `catch → approved=false`).
- **Zod replacement:** there is no drop-in. Options: (a) JSON-Schema validation via
  `github.com/santhosh-tekuri/jsonschema/v6` driven by `ToolDef.Parameters`, or
  (b) per-tool typed `Validate` closures using `encoding/json` + manual checks.
  Either way, reproduce the `INVALID_ARGS` message and the union-disambiguation
  wording (§4.1) for the watcher self-correction contract.
- **SQLite:** `modernc.org/sqlite` (pure-Go, no cgo) or `mattn/go-sqlite3`. Keep
  the `audit_log`/`automation_grants` DDL byte-identical; use `INTEGER` ms epochs.
- **uuid:** `github.com/google/uuid`; id = `"aud_" + uuid.NewString()[:8]`.
- **Wire-name maps:** `strings.ReplaceAll(name, ".", "__")` + two `map[string]string`
  rebuilt on each projection (guard collisions, fail fast).
- **JSON byte length** for the `bytes` field: `len([]byte(s))` (Go strings are
  UTF-8 already). Threshold/slice on runes for fidelity to TS `.length`, or
  document the small divergence.
- Registry mutates `result.AuditID` after the handler returns — in Go, return the
  `ToolResult` by value from dispatch and set `.AuditID` before returning (the TS
  mutation is a post-hoc field write).

### Concurrency

Dispatch is currently single-threaded per turn. The atomic grant consume relies on
SQLite serialization; keep DB writes behind a single connection/mutex or use a
`*sql.DB` with `SetMaxOpenConns(1)` for the writer to match the single-writer
assumption baked into `consumeGrant` and the audit/run_events coupling.

---

## 13. DELETE — do not port

- **Zod** machinery as-is (`z.ZodType`, `safeParse`). Replace with Go JSON-schema
  validation or typed decoders; keep the *messages*, not the library.
- **`ChatTool` / `models/fireworks.ts` import** coupling — re-derive the OpenAI
  function-spec struct in Go; don't port the TS module.
- **`Buffer.byteLength`** — use `len([]byte(s))`.
- **`AbortSignal`** — replace with `context.Context`.
- Any **React/OpenTUI/Bun** assumption — none leak into these three files, but the
  `ctx.log` / `ctx.confirm` callbacks are wired to the Bubble Tea UI in Go, not
  Ink/OpenTUI.
- The `.js`-extension ESM import convention and `NodeNext` resolution.
- `randomUUID().slice(0,8)` JS idiom → Go `uuid` slicing.

---

## 14. Edge cases / ordering guarantees to preserve

1. `started` timestamp captured before lookup; every audit `durationMs` measured
   from it (including for `UNKNOWN_TOOL`/`INVALID_ARGS` fast-fails).
2. Args are validated **before** the tier gate; a malformed call to a high-tier
   tool returns `INVALID_ARGS` (recoverable), not `TIER_DENIED`.
3. Tier gate **before** confirmation: a tier-denied tool never reaches the grant /
   confirm logic.
4. Grant check is **only** attempted when `ctx.actorId` is set; a non-interactive
   actor with no actorId always hits `CONFIRMATION_REQUIRED`.
5. A grant-authorized handler that **fails** is audited `error`, not `grant_ok`;
   only a successful grant call is `grant_ok` (and only then are
   `grantSource`/`grantId` stamped).
6. `autoApprove` only bypasses the prompt for the **main** actor; it does NOT let a
   non-interactive actor skip the grant requirement.
7. A thrown `confirm` ⇒ decline (never approve).
8. Both side-channels (debug log, DB insert) and the denial-event publish are
   wrapped so they can never break a tool call.
9. `resolveWireName` only resolves names from the **most recent** projection — a
   narrowed turn won't resolve a tool from a previous wider turn.
10. `register` throws on duplicate name; `toOpenAITools` throws on illegal/colliding
    wire names — both are fail-fast at startup/projection, never silent.
11. The no-file-edit guard runs at **both** registration (local names) and
    runtime inside `daintree.call` (raw MCP names).
