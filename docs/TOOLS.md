# Adding a tool

A **tool** is one capability the assistant model can call during a turn — a read, a
Daintree MCP action, a timer, a watcher, a memory write. Tools are the assistant's
**primary extension surface**: behavior the model can *do* is added here, behavior it
should *know* is added as a [skill](SKILLS.md).

> **The CLI never edits files.** Every registered tool name is checked at startup by
> `AssertSafe` and a file-mutating name is rejected outright (see [below](#the-no-file-edit-invariant)).
> Edits happen by spawning a visible Daintree agent (`agentTask.spawnForEdits`), never
> by a tool that writes to disk. Do not add one.

This mirrors [`SKILLS.md`](SKILLS.md): that doc is for *runbooks the model reads*; this
one is for *capabilities the model invokes*. The canonical Go shapes live in
[`internal/tools/types.go`](../internal/tools/types.go); the dispatch pipeline is in
[`internal/tools/dispatch.go`](../internal/tools/dispatch.go); the read-only exemplar is
[`internal/tools/exemplar_fs.go`](../internal/tools/exemplar_fs.go).

## TL;DR — add a tool

1. Pick (or create) a family sub-package under `internal/tools/<group>/`. A family
   exposes a single `Tools(Deps) []tools.Tool` constructor that returns its tools; its
   external dependencies (Store, MCP, Router, Queue) arrive through a small `Deps`
   struct, never a concrete package import.
2. Build a `*tools.Tool` (or `tools.Tool`) with a `Name`, `Description`, `Risk`,
   `Schema`, optional `Decode`, and a `Handle` function.
3. Wire the family into [`internal/app/tools.go`](../internal/app/tools.go) — append
   `yourgroup.Tools(yourgroup.Deps{…})` to the collected set. The app `RegisterAll`s the
   batch and then calls `AssertSafe()`; a duplicate name or a forbidden (file-edit) name
   fails fast at startup.
4. `go build ./... && go test ./...`. Add a table-driven handler test in your family.

No DB reset, no schema change. The model sees the new tool on its next turn (subject to
the per-turn projection — see [tier gating](#risk-classes-tiers-confirmation)).

## The `tools.Tool` shape

From [`internal/tools/types.go`](../internal/tools/types.go):

| Field | Type | Required | Notes |
|---|---|---|---|
| `Name` | string | ✅ | Internal **dotted** name (`fs.read`, `timer.schedule`). The registry maps it to/from the OpenAI wire form (dots → `__`, so `fs__read`). Must be unique and MUST NOT contain a file-edit fragment. |
| `Description` | string | ✅ | Model-facing. Can be long and instructional — this is how the model learns what the tool does and when to reach for it. |
| `Risk` | `domain.RiskClass` | ✅ | `read \| local \| ui \| terminal \| project \| external \| git \| system`. Drives tier gating + the confirmation matrix. Pick the **riskiest** thing the handler does. |
| `Consequence` | string | — | Short human Y/N prose that leads the approval sheet ("Runs `<cmd>` in terminal X"). Falls back to a per-risk phrase when empty. Only meaningful for confirmed (mutating) tools. |
| `Schema` | `json.RawMessage` | ✅ | The JSON Schema object advertised as the OpenAI `parameters`. **Must set `"additionalProperties": false`.** Use `tools.NoArgs` for a no-argument tool. |
| `Decode` | `DecodeFunc` | — | Validates/coerces args before the handler runs. Almost always `tools.StrictDecoder(func() any { return &myArgs{} })`. nil ⇒ raw args pass through unvalidated. |
| `Handle` | `Handler` | ✅ | `func(ctx context.Context, args json.RawMessage, tctx *ToolContext) ToolResult`. Does the work. |

### The handler contract

```go
func handleThing(ctx context.Context, args json.RawMessage, tctx *tools.ToolContext) tools.ToolResult {
    var a thingArgs
    if err := json.Unmarshal(args, &a); err != nil { // args are already Decode-validated
        return tools.Fail("INVALID_ARGS", "thing: "+err.Error())
    }
    // … do the work …
    return tools.Ok("Did the thing.", map[string]any{"id": id})
}
```

- **Return a `ToolResult` envelope** — `tools.Ok(summary, result)` on success (summary is
  required model-facing prose; result is any JSON-able payload, may be nil), or
  `tools.Fail(code, message, opts…)` on a failure. Failure options:
  `tools.Unrecoverable()` (the model should not retry — e.g. denied, not-found) and
  `tools.WithDetails(x)` (attach structured detail).
- **NEVER panic to the caller.** `Dispatch` recovers any panic into a `TOOL_THREW`
  failure and still audits — but a handler should return `Fail(...)` for *expected*
  errors rather than relying on the firewall.
- **Honor `ctx` cancellation** — a turn can be cancelled (Escape). Long handlers should
  watch `ctx.Done()`. To surface a substep, emit a beat via `tctx.ReportProgress(...)` —
  but **nil-check it first**: the field is unset outside the cockpit (tests, the classic
  REPL), so calling it unconditionally panics.
- **Side-channels never break a call.** Audit (debug log + DB row) and progress beats are
  panic-guarded by the registry — you never call confirm or audit yourself.

### Validating args with `StrictDecoder`

`tools.StrictDecoder` builds a `DecodeFunc` that decodes into a fresh args value
**rejecting unknown fields** and returns the canonical re-marshaled args (so defaults and
coercion reach the handler). If the args struct implements `Validate() error`
(`tools.Validator`), it runs after decode — that is where numeric/semantic bounds live
(the Go analogue of Zod refinements). A returned error becomes the `INVALID_ARGS` detail.

```go
type scheduleArgs struct {
    DelayMs int `json:"delayMs"`
}
func (a *scheduleArgs) Validate() error {
    if a.DelayMs <= 0 { return errors.New("delayMs must be positive") }
    return nil
}
// Tool: Decode: tools.StrictDecoder(func() any { return &scheduleArgs{} })
```

## The dispatch pipeline

Every tool call — from the agent loop, a watcher, a timer, or a workflow — flows through
`Registry.Dispatch`. The order is **load-bearing**; each stage can short-circuit with a
specific model-facing code. `Dispatch` never returns an error and never panics to the
caller — every failure rides in the `ToolResult`.

1. **Lookup** — unknown name → `UNKNOWN_TOOL` (with a "did you mean?" hint).
2. **Projection gate** — if the turn narrowed the toolset (`ActiveToolNames`) and this
   tool isn't in it → `TOOL_NOT_OFFERED`. Defense in depth over schema projection.
3. **Arg decode** — runs `Decode`; a reject → `INVALID_ARGS` (carries structured issues).
   Validation precedes the tier gate, so a malformed high-tier call returns
   `INVALID_ARGS`, not `TIER_DENIED`.
4. **Tier gate** — `safety.Decide(risk, tier)`; a tier that can't perform the risk class
   → `TIER_DENIED` (non-recoverable; steers to `/permissions`).
5. **Confirmation** — only when the matrix requires it:
   - **Interactive `main` actor** → an approval sheet (unless `AutoApprove`); a decline →
     `USER_DECLINED`.
   - **Non-interactive actor** (watcher/timer/workflow) → a scoped **automation grant** is
     the only path; no matching grant → `CONFIRMATION_REQUIRED` (and an "Autonomous action
     blocked" inbox event).
6. **Run** — the handler, wrapped in panic recovery (panic → `TOOL_THREW`).
7. **Audit** — a debug-log line + a DB audit row, both best-effort and panic-guarded; the
   new row id is stamped onto `ToolResult.AuditID`.

The exact codes are constants in `dispatch.go` — keep their spelling stable; the model and
the audit log switch on them.

## The no-file-edit invariant

`Registry.AssertSafe()` runs at startup (`safety.AssertNoFileEditTools`) and **refuses to
boot** if any registered name contains a file-mutating fragment. Matching is a
case-insensitive substring test, so each entry catches its spelling variants:

```
write_file  writefile  apply_patch  applypatch  edit_file  editfile
fs.write  fs.edit  file.write  file.edit  patch.apply  fs__write
savefile  save_file  rename_file  renamefile  remove_file  removefile
delete_file  deletefile  file.delete  file.remove  file.rename  file.save
workspace.patch  patchfile  patch_file
```

The same check runs inside `daintree.call` against the raw forwarded MCP name, so the raw
escape hatch can't smuggle a file-edit either. If you need the assistant to change project
files, spawn a visible agent with `agentTask.spawnForEdits` (mode `edit`) — that is the
*only* mutation path, and it is supervised.

## Risk classes, tiers, confirmation

`safety.Decide(risk, tier)` gates every dispatch. Tiers widen the allowed set:

| Tier | May perform |
|---|---|
| `supervisor` | `read` `local` `ui` |
| `operator` | + `terminal` `project` `external` |
| `system` | + `git` `system` |

The **always-confirm** classes are `terminal` `project` `external` `git` `system`: the
interactive `main` actor must confirm; a non-interactive actor needs a scoped grant.
`read` / `local` / `ui` never confirm. Choose the risk class by the strongest effect the
handler has — a tool that reads *and* sends a command is `terminal`, not `read`.

## The `ToolContext`

Everything a handler can reach, built once at startup with per-turn/per-actor fields
filled by the caller (handlers degrade gracefully when an optional field is zero):

- **Always present:** `Config` (carries `Tier`, `AutoApprove`), `MCP`, `DB`, `Queue`,
  `Router`, `ProjectPath`, `Actor`, `Confirm`, `Log`.
- **Liveness:** `ToolCallID`, `ReportProgress` (emit substep beats — may be nil; guard it
  before calling, see the handler contract above).
- **Per-turn / per-actor:** `SessionID`, `ActorID` (the `wch_…`/`tmr_…` — required for the
  grant lookup), `RunID` (stamped on the audit row), `ActiveToolNames` (the turn's
  allowlist; nil ⇒ all), `DaemonActive`.

Cross-subsystem deps are reached through the small consumer-defined interfaces in
`internal/tools` (`Store`, `MCPClient`, `Queue`, `Router`) — never the concrete packages —
so the package compiles in isolation and a family's `Deps` adapter is a thin shim.

## Tool families (current snapshot)

The registry is the source of truth (`internal/tools` + the wiring in
`internal/app/tools.go`); keep this table in step when you add or rename a tool.

| Family (`internal/tools/…`) | Tools |
|---|---|
| `fsx` | `fs.list` `fs.read` `fs.search` |
| `mcpx` | `daintree.status` `daintree.listTools` `tool.search` `daintree.call` · `terminal.focus` `terminal.sendCommand` `terminal.arm` `terminal.disarm` `terminal.disarmAll` · `copyTree.generate` `copyTree.generateAndCopyFile` `copyTree.injectToTerminal` · `agent.focusNextWaiting` `agent.focusNextWorking` `agent.focusNextAgent` `agent.focusPreviousAgent` |
| `mcpwrap` | `recipe.list` `recipe.run` · `worktree.list` `worktree.getCurrent` `worktree.createWithRecipe` · `forge.listIssues` `forge.getIssue` `forge.listPRs` `forge.getPR` · `git.snapshotRevert` `git.snapshotDelete` · `workflow.startWorkOnIssue` `workflow.prepBranchForReview` `workflow.focusNextAttention` |
| `contextx` | `context.snapshot` `terminal.read` `terminal.summarize` |
| `extractionx` | `terminal.extract` `terminal.extract.async` |
| `timer` | `timer.schedule` `timer.list` `timer.cancel` |
| `watcher` | `watcher.terminal.create` `watcher.watchPR` `watcher.list` `watcher.cancel` |
| `queue` | `queue.publish` `queue.digest` `queue.resolve` |
| `grant` | `grant.create` `grant.list` `grant.revoke` |
| `workflow` | `workflow.create` `workflow.get` `workflow.list` `workflow.update` |
| `skill` | `skill.find` `skill.load` `skill.run.get` `skill.step.advance` |
| `auditx` | `audit.export` |
| `memory` | `memory.recall` `memory.list` `memory.save` `memory.forget` `memory.pin` `memory.unpin` |
| `artifactx` | `artifact.read` |
| `agenttaskx` | `agentTask.spawnForEdits` `agentTask.superviseTerminal` `agentTask.status` `agentTask.list` |

`mcpwrap` are typed wrappers over Daintree MCP actions. Forwarding many of these raw
through `daintree.call` is refused with `USE_TYPED_WRAPPER` and redirected to the wrapper
(the exact denylist is `wrappedMCPTools` in `internal/tools/mcpx/discovery.go` — it covers
the tools whose wrapper does validation worth protecting, so a few simple read passthroughs
aren't on it). Reach for the wrapper, not the raw call. The model-facing reference for the
Daintree MCP surface is
[`DAINTREE_MCP.md`](DAINTREE_MCP.md) (and the embedded prompt in
`internal/models/prompts/daintree_mcp.go`).

## Worked example

The canonical read tool, from [`internal/tools/exemplar_fs.go`](../internal/tools/exemplar_fs.go):

```go
type fsReadArgs struct {
    Path string `json:"path"`
}

var fsReadSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["path"],
  "properties": {
    "path": { "type": "string", "description": "Project-relative file path to read." }
  }
}`)

func NewFsReadTool() *Tool {
    return &Tool{
        Name:        "fs.read",
        Description: "Read a UTF-8 text file inside the project. Refuses credential files.",
        Risk:        domain.RiskRead,
        Schema:      fsReadSchema,
        Decode:      StrictDecoder(func() any { return &fsReadArgs{} }),
        Handle:      handleFsRead,
    }
}

func handleFsRead(_ context.Context, args json.RawMessage, tctx *ToolContext) ToolResult {
    var a fsReadArgs
    _ = json.Unmarshal(args, &a) // already Decode-validated
    // Secret guard FIRST — never surface a credential file into the audit log / history.
    if safety.IsSensitivePath(a.Path) {
        return Fail(domain.CodeDenied, "Refusing to read "+a.Path+": looks like a credential file.", Unrecoverable())
    }
    abs, err := safety.ResolveInsideProject(tctx.ProjectPath, a.Path) // project-root containment
    if err != nil {
        return Fail(domain.CodeDenied, err.Error(), Unrecoverable())
    }
    data, err := os.ReadFile(abs)
    if err != nil {
        return Fail(domain.CodeNotFound, "fs.read: "+err.Error())
    }
    return Ok("Read "+a.Path+".", map[string]any{"path": a.Path, "content": string(data)})
}
```

Note the two read-only safety helpers every fs tool uses: `safety.IsSensitivePath`
(refuse credential files) and `safety.ResolveInsideProject` (block path traversal). A
read tool that touches the filesystem should use both.

## Authoring checklist

- [ ] `Name` is dotted, unique, and contains **no** file-edit fragment.
- [ ] `Schema` is a JSON object with `"additionalProperties": false` (or `tools.NoArgs`).
- [ ] `Risk` is the riskiest class the handler actually performs.
- [ ] `Decode` is a `StrictDecoder` over a typed args struct (with `Validate()` for bounds).
- [ ] `Handle` returns `Ok`/`Fail`, never panics, honors `ctx` cancellation.
- [ ] Mutating tool: set a clear `Consequence` for the approval sheet.
- [ ] Family wired into `internal/app/tools.go`; `AssertSafe` still passes at startup.
- [ ] Table-driven handler test added; `go build ./... && go test ./...` is green.
- [ ] This doc's family table updated if you added/renamed a tool.

## Reference internals

- Types + envelope: [`internal/tools/types.go`](../internal/tools/types.go),
  `domain.ToolResult` (`Ok`/`Fail`).
- Decode helpers: [`internal/tools/audit.go`](../internal/tools/audit.go)
  (`StrictDecoder`, `DecodeStrict`, `Validator`).
- Dispatch pipeline + codes: [`internal/tools/dispatch.go`](../internal/tools/dispatch.go).
- Registry (`Register` / `RegisterAll` / `AssertSafe` / projection):
  [`internal/tools/registry.go`](../internal/tools/registry.go).
- Safety policy (tiers, confirm matrix, no-file-edit, path/secret guards):
  [`internal/safety/policy.go`](../internal/safety/policy.go).
- Wiring (every family's `Tools(Deps)` collected, registered, asserted):
  [`internal/app/tools.go`](../internal/app/tools.go).
