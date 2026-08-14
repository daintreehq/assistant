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
| `Requires` | `Connection` | — | The external connection the tool needs to do its job (`RequiresDaintreeMCP`, `RequiresDocsMCP`, `RequiresInteractive`; zero value = purely local). **Documentation, not a gate** — dispatch never reads it. It feeds the generated reference, `doctor`, and the degraded-mode banner, so "what still works while Daintree is disconnected?" has one answer. Usually stamped per family in `app.DefaultToolBuilder`. |
| `Parallelizable` | bool | — | Opt this tool into concurrent dispatch with any other read-only batch sibling. Only for reads with no side effects and no ordering dependency; double-gated on `RiskRead`. **Never** on a barrier/wait tool. |
| `ParallelHomogeneous` | bool | — | Narrower: lets a MUTATING tool batch with consecutive same-name siblings that are already fully authorized (the spawn fan-out case). Pair with `ParallelConflictKey` to keep two calls off one shared target. |

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
- **Interactive-only:** `AskChoice` (present for the `main` actor; nil for a
  watcher/timer/workflow) — presents a multiple-choice question and blocks on the answer.
  `user.askMultipleChoice` uses it; a nil check yields `QUESTION_NOT_INTERACTIVE`.
- **Liveness:** `ToolCallID`, `ReportProgress` (emit substep beats — may be nil; guard it
  before calling, see the handler contract above).
- **Per-turn / per-actor:** `SessionID`, `ActorID` (the `wch_…`/`tmr_…` — required for the
  grant lookup), `RunID` (stamped on the audit row), `ActiveToolNames` (the turn's
  allowlist; nil ⇒ all), `DaemonActive`.

Cross-subsystem deps are reached through the small consumer-defined interfaces in
`internal/tools` (`Store`, `MCPClient`, `Queue`, `Router`) — never the concrete packages —
so the package compiles in isolation and a family's `Deps` adapter is a thin shim.

## Tool families

**The inventory is generated: [`generated/TOOLS.md`](generated/TOOLS.md).** Every
registered tool, with its risk, minimum tier, confirmation behaviour, grantability,
connection dependency, parallel-safety class, and feature flag — projected from the live
registry and diffed in CI. Do not restate it here, and do not hand-edit it: this section
used to carry a per-tool table, and it rotted into naming four tools that no longer exist
(`agent.focusNextWaiting`, `terminal.extract.async`, `workflow.focusNextAttention`) while
omitting three whole families.

What belongs here instead is the structural map — which sub-package owns what, which is
what you need when deciding where a NEW tool goes:

| Family (`internal/tools/…`) | Owns |
|---|---|
| `fsx` | read-only project filesystem (`fs.*`) — never writes, refuses credential-bearing paths |
| `mcpx` | raw Daintree MCP: discovery, `daintree.call`, terminal control, copy-tree |
| `mcpwrap` | typed wrappers over Daintree MCP actions: recipes, worktrees, forge, git |
| `contextx` | operational overview + verbatim/summarized terminal reads |
| `extractionx` | fact extraction from terminal output, and the foreground cohort wait |
| `asyncx` | durable background supervision (`terminal.run.async`, the async ledger) |
| `agenttaskx` | the ONLY agent-spawn path, plus its durable launch sagas |
| `watcher` · `timer` · `queue` · `grant` | unattended supervision and the authority it requires |
| `workflow` | the durable work ledger, plus the flag-gated execution graph |
| `memory` · `scratchx` | state that outlives a session, and state scoped inside one |
| `skill` | local run-tracking only — selection is server-owned ([`SKILLS.md`](SKILLS.md)) |
| `auditx` · `artifactx` | the audit trail, and paging oversized results |
| `docsx` | the public no-auth Daintree documentation MCP (a SECOND client) |
| `questionx` | `user.askMultipleChoice` — one finite question, interactive sessions only |

Each family exposes one `Tools(Deps) []tools.Tool` constructor and reaches its
dependencies through a small `Deps` struct, never a concrete package import.

### Declare the connection your tool needs

Set `Tool.Requires` (`RequiresDaintreeMCP`, `RequiresDocsMCP`, `RequiresInteractive`, or
the zero value for purely local). It gates nothing — a tool whose connection is down still
runs and returns its own clean "not connected" failure — but it is what lets the generated
reference, `doctor`, and the degraded-mode banner answer "which of these actually work
right now?" from one place. `app.DefaultToolBuilder` stamps it per family, so a new tool
inherits its family's dependency; override by name there when a family is mixed (as
`async.list`/`async.cancel` are, being local ledger reads in an otherwise MCP-bound family).

### Prefer the wrapper to the raw call

`mcpwrap` tools are typed wrappers over Daintree MCP actions. Forwarding many of these raw
through `daintree.call` is refused with `USE_TYPED_WRAPPER` and redirected to the wrapper
(the denylist is `wrappedMCPTools` in `internal/tools/mcpx/discovery.go` — it covers the
tools whose wrapper does validation worth protecting, so a few simple read passthroughs
are not on it). The human-facing reference for the Daintree MCP surface is
[`DAINTREE_MCP.md`](DAINTREE_MCP.md); the model-facing one is **backend-owned** and lives
in `../assistant-backend`.

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
- [ ] `Requires` is right — inherited from the family, or overridden by name if the family is mixed.
- [ ] Table-driven handler test added; `go build ./... && go test ./...` is green.
- [ ] **Regenerated the capability reference**, or CI will fail on the drift:
      `go test ./internal/app -run TestGeneratedDocsAreCurrent -update`
      (and `go test ./internal/commands -run TestGeneratedCommandRefIsCurrent -update`
      if you touched `COMMAND_REGISTRY`). Commit the regenerated files.
- [ ] **Removed or renamed a tool?** Hand the backend a refreshed inventory
      (`go run ./cmd/tooldump`) — see below.

## Size budgets (enforced)

The inventory is sent on **every** model round and the tester pays for it on their own
OpenRouter key — measured at ~61% of a representative request, more than double the
permanent system prompt. `internal/app/toolbudget_test.go` bounds it:

| budget | limit |
|---|---|
| ordinary tool description | 600 chars |
| orchestrator tool description | 1,200 chars (named exemptions only) |
| one parameter description | 300 chars |
| whole projection, compact wire bytes | 80,000 (88,000 with `DAINTREE_WORKFLOW_INTELLIGENCE=1`) |

Exceeding one is allowed and must be a **decision**: add the tool to `orchestratorTools`
or the argument to `parameterBudgetExceptions`, each with a reason. Both lists are
themselves gated: an entry fails if its tool/argument no longer exists, AND if the
description has since shrunk back within the ordinary budget — so an exemption cannot
outlive its justification, only its subject.

What the budgets must NOT do is push a load-bearing rule into nowhere. A rule about ONE
tool belongs in that tool's description, beside the thing it governs; a rule spanning
several tools belongs in a backend-owned skill. Deleting it to hit a byte count is the
failure mode, and no test can catch that — only review can. The usual win is not deleting
rules but **stating each one once**: a rule repeated in both the description and a
parameter is paid for on every round, twice.

## Exporting the tool inventory

The backend pins a captured copy of the projection we send in `input.tools`, and its
skill bodies name the tools in it. When a tool leaves the registry and that pin is not
refreshed, a runbook goes on instructing the model to call something that is no longer
offered — which is worse than a stale doc, because the base prompt forbids inventing a
tool, so the turn stalls on a contradiction it cannot report. That has already happened
once: six removed tools, five skills still naming them, nothing detecting it.

```bash
go run ./cmd/tooldump                         # → stdout, the projection a normal launch sends
go run ./cmd/tooldump -o tools.json
go run ./cmd/tooldump -workflow-intelligence  # …plus the DAINTREE_WORKFLOW_INTELLIGENCE=1 tools
```

The default output deliberately EXCLUDES the flag-gated execution-graph tools: pinning
them would promise the backend that `workflow.plan` is always offered, and a skill
written against that promise would name an unoffered tool for everyone who has not opted
in.

Output is deterministic and indented, so a refresh reads as an additive diff. It is the
same JSON value the wire carries — including JSON's HTML escaping, which the real request
also applies — so compacting it reproduces the payload exactly and it can be diffed
against what the backend received. Indentation and a trailing newline are the only
differences. The construction lives in
[`internal/app/toolinventory.go`](../internal/app/toolinventory.go); `cmd/tooldump` is a
thin main over it.

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
