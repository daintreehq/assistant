# Daintree MCP — integration notes

The CLI connects to Daintree's **local MCP server** as a separate local process (an
out-of-process MCP client — *not* the `external` API-key tier; see Tiers). In production
Daintree launches the CLI and passes the connection details via environment variables.

> **Source of truth.** This doc is the CLI repo's human-facing record of the verified
> Daintree MCP contract. The **model-facing** guidance is backend-owned now: it lives in
> the `../assistant-backend` skill files (e.g.
> `src/daintree_assistant_server/skills/files/daintree.foundation.md`), injected
> server-side — the old embedded prompt reference (`internal/prompts/daintree_mcp.go`)
> was deleted with the backend migration. Keep this doc and the backend skills in sync;
> the machine-checked pieces (retry safety, the wrapper drift baseline) are derived from
> the live server and the local wrappers — see "Keeping the references in sync" below.

## Connection

| Setting        | Value                                                        |
| -------------- | ----------------------------------------------------------- |
| Transport      | Streamable HTTP (primary) at `/mcp`; legacy SSE at `/sse`    |
| Host/port      | `127.0.0.1:45454` (default; configurable)                   |
| URL env var    | `DAINTREE_MCP_URL` (e.g. `http://127.0.0.1:45454/mcp`)      |
| Token env var  | `DAINTREE_MCP_TOKEN` (bearer)                               |
| Project id     | `DAINTREE_PROJECT_ID` (optional)                            |
| Window id      | `DAINTREE_WINDOW_ID` (Daintree-injected; identifies the bound window) |
| Auth header    | `Authorization: Bearer <token>`                            |

`DAINTREE_WINDOW_ID` is injected by Daintree so a CLI bound to one Daintree window
can be told apart from another. It is read into config (`AppConfig.WindowID`, env-only —
never a CLI flag) and surfaced by `/status` via `config.DescribeConfig`. (`/doctor`
reports connection health and a live MCP probe rather than the raw config values.)

The client (`internal/mcp`) uses `github.com/modelcontextprotocol/go-sdk`: it connects
over the Streamable HTTP transport with an `Authorization: Bearer <token>` header and
falls back to the legacy SSE transport on failure.

`ListTools` → tools with `{ name, description, inputSchema }`.
`CallTool(name, arguments)` → content text plus optional `structuredContent` and an
`isError` flag, normalized into a `domain.ToolResult`.

## Tiers

`workbench` (read/safe) ⊂ `action` (+mutations) ⊂ `system` (+dangerous). The assistant's
session is granted **`system`**, so every MCP-exposed tool below is reachable to it
(renderer-only actions like `terminal.armByState` stay unreachable regardless of tier).
External
API-key clients (Cursor, Claude Code, scripts) get a separate **`external`** tier that
is now a **curated allowlist of ~25 orchestration primitives**
(`shared/config/mcpExternalTierAllowlist.ts` in `daintreehq/daintree`, budget-enforced
at 15–26 tools / 16,000 description bytes) — **zero `forge.*` tools** and no unlisted
extras: the cut is a real dispatch allowlist, not `tools/list` filtering, so a tool
absent from an external session's list is genuinely uncallable there. The cut removed
**no** workbench/action/system permissions — if anything the assistant's advertised
`tools/list` *grew* slightly, because the same change retired the old "discoverable"
callable-but-unlisted visibility marker. The CLI treats its tier as advisory and adds
its OWN safety layer on top.

## Representative action / tool ids (call via `daintree.call`)

Terminals: `terminal.list`, `terminal.new`, `terminal.getOutput`, `terminal.getStatus`,
`terminal.sendCommand`, `terminal.inject`, `terminal.waitUntilIdle`, `terminal.rename`,
`terminal.close`, `terminal.kill`, `terminal.arm` / `terminal.disarm` /
`terminal.disarmAll`. (`terminal.armByState` / `armAll` and the `fleet.*` dispatch
family are renderer-only — not MCP-callable; the one exception is the read-only
`fleet.getRunStatus` observer, which IS MCP-exposed. See the arming note below.)

> `terminal.waitUntilIdle` blocks on **one** terminal — targeted use only; never fan out
> many concurrent waits. Batch reads by passing several `terminalIds` to
> `terminal.getStatus` instead. There is **no** `terminal.listStatus` and **no**
> `terminal.waitForAny`. NOTE: over MCP `terminal.waitUntilIdle` runs **main-process
> only** (the renderer registration is a stub that throws), and an *interactive* session
> caps the wait at 60s server-side regardless of `timeoutMs` (headless can wait up to
> ~2h). The CLI does **not** route finish-detection through it — its `terminal.awaitAll`
> / `terminal.extract wait:{}` poll `terminal.getStatus` + `terminal.getOutput`
> themselves — so treat it as background-only and don't reach for it.

Worktrees: `worktree.list`, `worktree.getCurrent`, `worktree.createWithRecipe`,
`worktree.delete`, `worktree.setActive`, `worktree.refresh`, `worktree.listBranches`,
`worktree.compareDiff` (read-only: the files that differ between two worktrees' branches).
Resource lifecycle: `worktree.resource.provision`, `worktree.resource.teardown`,
`worktree.resource.pause`, `worktree.resource.resume`, `worktree.resource.status`.
(`worktree.resource.connect` is **renderer-only** — no MCP surface, despite earlier
docs listing it here.)

Git: `git.getProjectPulse` (HISTORICAL activity/pulse), `git.getStagingStatus` (live
working-tree state), `git.commit`, `git.push`, `git.stageAll`, `git.stageFile`,
`git.unstageAll`, `git.unstageFile`, `git.listCommits`, `git.getFileDiff`.
(Of these, only `git.getProjectPulse` has a typed CLI wrapper — the rest, incl.
`git.commit`/`git.push`/`git.getStagingStatus`, are reached via `daintree.call`. The
former `git.snapshot*` family was removed from Daintree as part of a feature cleanup;
the CLI dropped its `git.snapshotRevert`/`git.snapshotDelete` wrappers in lockstep.)

Forge (reads): `forge.listIssues`, `forge.listPRs`, `forge.getIssue`, `forge.getPR`,
`forge.listIssueComments`, `forge.getCIStatus` (shapes below).

> **Forge list arguments & the `view` projection.** The two list reads take a worktree
> selector (`worktreeId` / `worktreePath`, or legacy `cwd`) plus `cursor` (opaque — pass
> a previous response's `nextCursor` verbatim; an empty string is rejected, not "page
> one"), `perPage` (1–100, default 20), `sort: "created"|"updated"` (default created),
> `direction: "asc"|"desc"` (default desc), `bypassCache` (default false), and `state`
> (default open) — `"open"|"closed"|"all"` on `forge.listIssues`,
> `"open"|"closed"|"merged"|"all"` on `forge.listPRs`. `forge.listIssues` additionally takes `search`, a **provider-native**
> query fragment (GitHub search dialect on GitHub — not a plain-text filter);
> `forge.listPRs` deliberately has no `search` key. Both accept `view: "summary"|"full"`
> — `summary` (the default) drops each row's body and raw provider payload (the
> decision-making projection), `full` returns the provider's complete normalized rows.
> Unknown argument keys are rejected, not ignored (the schemas are strict).

Forge (issue writes): `forge.createIssue`, `forge.closeIssue`, `forge.reopenIssue`,
`forge.editIssue`, `forge.addIssueComment`, `forge.addIssueLabel`,
`forge.removeIssueLabel`, `forge.assignIssue`, `forge.unassignIssue`.
Forge (PR writes): `forge.createPR`, `forge.closePR`, `forge.reopenPR`, `forge.mergePR`,
`forge.convertPRToDraft`, `forge.markPRReadyForReview`, `forge.commentOnPR`, `forge.editPR`.
Forge (review writes): `forge.approvePR`, `forge.requestChanges`, `forge.dismissReview`,
`forge.requestReviewers`. All forge writes are always-confirm mutations — each is reached
via `daintree.call`, whose local risk class is `system` (the CLI's own safety layer
prompts unless an auto-approve session or a scoped automation grant covers it — note
that on the Daintree side several issue writes are `danger:safe`, but the CLI confirms
regardless). **Only the four forge READS
(`listIssues`/`getIssue`/`listPRs`/`getPR`) have typed CLI wrappers** — every forge
WRITE is reached via `daintree.call`. Most PR/review writes return `void`; only
`forge.createPR`/`forge.editPR` return the PR object (re-read with `forge.getPR` after
other writes).

> The `forge.open*` actions (`forge.openIssue`, `forge.openPR`, `forge.openIssues`,
> `forge.openPRs`, `forge.openCommits`) exist on the Daintree server but are
> **renderer/UI-only** — they open a browser/editor window via Electron IPC and do
> nothing useful for a headless MCP client, so the CLI deliberately omits them. Use the
> `forge.list*` / `forge.get*` reads instead.

Agents/Recipes: `agent.launch`, `agent.getState` (live single-agent state, keyed by
**agent** id), `agent.terminal`, `agent.listAvailable`, `agent.listToolbar`, `cliAvailability.get`,
`agentSessionHistory.list`, `recipe.list`, `recipe.run`.

> **Agent discovery.** `agent.listAvailable` is the canonical narrow snapshot. It reads
> the main process's current effective direct-agent registry (built-in + user + plugin),
> excludes assistant-only `daintree-assistant`, and returns `{ complete,
> availabilityComplete, agents: [{ id, displayName, source, availability?, installed?,
> launchable?, pinned?, toolbarVisible? }] }`. `launchable` is true only for `ready` or
> `unauthenticated`, false for a known non-runnable state, and omitted when availability is
> still unknown. Built-ins carry explicit tri-state `pinned`
> plus resolved `toolbarVisible`; user/plugin agents have no toolbar fields. An omitted
> availability means the CLI probe cache has not covered that new registry row yet — it is
> not equivalent to `missing`. `ready` is immediately launchable; `unauthenticated` may
> prompt for login; `installed` and `blocked` need setup/support before direct launch.
>
> `agent.listToolbar` remains the built-in-only toolbar view, and `cliAvailability.get`
> remains the raw coarse status map. The assistant uses the canonical combined action
> during its splash and for spawn validation, so registry membership, display names, and
> toolbar state come from one coherent read. Never substitute broad `agentSettings.get`:
> settings can include flags, presets, and environment configuration, and its keys are not
> an availability catalog.

> **Closed / resumable sessions.** `agentSessionHistory.list` (workbench tier,
> `danger:safe`, reached via `daintree.call` — no typed wrapper) lists the closed agent
> sessions the user can relaunch, read from Daintree's on-disk journal. Optional
> `worktreeId` scopes it to one worktree (an empty string is **rejected** — omit it to
> list every resumable session across all worktrees and projects). Returns `{ sessions:
> [{ sessionId, agentId, worktreeId, title, projectId, savedAt (epoch ms, newest-first),
> agentLaunchFlags?, agentModelId?, cwd?, branch? }] }`, capped/pruned by the journal's
> retention policy, and it **never errors** — an empty/unreadable journal yields
> `{ sessions: [] }`. It is a faithful record listing, **not** a summary of what each
> session did. There is **no dedicated restore/reopen MCP tool** and **no way to read a
> closed session's transcript** (a short-lived `agentSessionHistory.getSnapshot` was added
> then removed Daintree-side — don't reach for it). Raw Daintree "restores" a session by
> replaying a record's `agentId` / `cwd` / `worktreeId` / `agentLaunchFlags` /
> `agentModelId` back into `agent.launch`. **CLI caveat:** the CLI blocks raw `agent.launch`
> (see the daintree.call denylist), and its only spawn path — `agentTask.spawnForEdits` —
> does **not** carry `agentLaunchFlags` / `agentModelId` / `cwd` and requires a fresh
> `taskPrompt`, so from the CLI a "restore" is a *fresh* agent spawned in that worktree,
> **not** a faithful resume of the original session.

> `workflow.startWorkOnIssue` is a COMPLETE synchronous Daintree workflow: it creates the
> worktree + branch, **spawns the work agent** (raw Daintree leaves it UNSUPERVISED),
> optionally runs a recipe first, best-effort injects worktree context, and optionally
> assigns the issue — returning `{ issueNumber, issueTitle, issueUrl, worktreeId,
> worktreePath, branch, terminalId, recipeLaunched, spawnedTerminalCount,
> failedTerminalCount, assignedToSelf, … }` (now published as structured output —
> `mcpOutputSchema` — so `structuredContent` carries the object). It validates `agentId`
> **up front**: an unknown id, or the literal pseudo-ids `browser` / `dev-preview`, is
> rejected before any worktree is created. The CLI **wrapper** then attaches a
> supervisor watcher to that terminal (`attachWatcher` defaults true), so from the CLI it
> is background supervision. `workflow.prepBranchForReview` is **READ-ONLY** despite the
> name — it returns a go/no-go verdict (`ready` | `blocked_uncommitted_changes` |
> `blocked_merge_conflicts` | `blocked_repo_busy` | `no_runners_detected`) plus
> uncommitted/staged counts + detected runners; it does **not** commit/push/open a PR.
> The CLI wrapper is `risk:read` (no confirmation). Recipes (`recipe.run` /
> `worktree.createWithRecipe`) spawn ALL their terminals synchronously, and a recipe
> terminal can be a **live agent** running its CLI with no watcher (agent-sourced runs
> capped at 3) — a recipe sets up a workspace, it does not supervise.

Code/Files (Daintree-side): `copyTree.generate`, `copyTree.injectToTerminal`,
`files.search`, `file.view`, `file.openInEditor`.

Meta: `actions.list`, `actions.getContext`, `actions.search`, `actions.getSchema`,
`mcp.surface` (see the discovery section below).

Project checks: `project.detectRunners`, `project.runCheck` (shapes below) — an
authoritative way to run a project-defined check over MCP and read its real exit code,
superseding guesswork over terminal scrollback (`lastCheckResult` stays heuristic
evidence).

Projects: `project.getCurrent` returns `{ project }` for the active window (or null).
The assistant keeps only id/name/path/status and the two repository-config booleans from
that record. It intentionally does not inject `project.getSettings`, whose open-ended map
can contain environment or other sensitive configuration. Presentation/recency fields
(emoji, color, pinned, last-opened/frecency, auto-park timestamps) are also omitted because
they do not improve ordinary orchestration. Broader or volatile reads — all projects, all
worktrees, and `git.getProjectPulse` — remain available on demand instead of bloating or
invalidating every request.

The curated project row and canonical `agent.listAvailable` roster are cached into the
structured `request.startup` block. `worktree.getCurrent` is re-read for every model round
and sent as typed `request.runtime.worktree`: omission means the read was unavailable,
`{current:null}` is a definitive no-current-worktree result, and a current object carries
the useful id/path/branch/issue/PR/status/last-commit metadata.

### Discovery & surface introspection

Two discovery layers exist — don't conflate them:

- **CLI-local discovery** (`tool.search`, `daintree.listTools`): both work off the CLI's
  cache-first `tools/list` inventory — `daintree.listTools` returns it, `tool.search`
  filters/ranks it locally. This is the model's normal path — prefer a typed wrapper,
  then `daintree.call` for a known unwrapped name found this way.
- **Server-side action introspection** (`actions.search` → `actions.getSchema`):
  `actions.search({ query, limit? })` returns ranked matches (no schemas);
  `actions.getSchema({ actionId })` returns the full entry (`{ ok: true, entry }`, or a
  structured `NOT_FOUND`). Use it for intent search and exact argument schemas. It
  describes the action catalog — it does **not** make an unlisted tool callable on a
  lower tier.

**`mcp.surface`** (no args, workbench tier, structured output) is the session's surface
manifest: `{ manifestVersion, appVersion, tier, hash, tools: [{ id, tier, kind:
"command"|"query", readOnlyHint, idempotentHint, deprecated? { reason, replacedBy? } }] }`.
It reports the **static tier-listed surface** — the same tool membership `tools/list`
advertises for the session (as metadata, not the full descriptor payload) — not live
per-tool grants, deliberately, so `hash` doesn't flap on short-lived approvals. The
sha256 `hash` covers structural metadata (tiers, danger, annotations, arg/result
schemas) but **excludes description/title/example prose** (reworded too often to be a
drift signal). Call it once at startup, then re-read the hash to detect drift cheaply. The CLI
does **not** consume it yet — startup drift checks the wrapper-derived baseline and retry
safety comes from `tools/list` annotations (see "Keeping the references in sync"), while
`/doctor` probes via `actions.getContext`; `mcp.surface` is the natural future target for
a whole-surface drift hash and the `/doctor` probe.

### Verified call/response shapes

- `terminal.getStatus({ terminalIds: string[] (1–256), includeOutput?: { lines 1–50, stripAnsi } })`
  → `{ terminals: [{ terminalId, agentId, agentState, waitingReason?, exitCode?, spawnedAt?, lastTransitionAt?, lastCheckResult?, recentOutput?, armed?, error? }] }`.
  There is **no** flat `agentState` and **no** `runtimeStatus`. `exitCode` is tri-state —
  a **number** on a clean exit, **null** on a signal kill, **absent** while running — so
  its *presence* (not value) signals the exit. `spawnedAt` / `lastTransitionAt` are
  epoch-ms timestamps (`lastTransitionAt` = when the agent entered its CURRENT state, not
  when it last produced output). `lastCheckResult` (when present) is a best-effort parse
  of the agent's last test/lint/build summary — useful evidence, **not** authoritative
  (for an authoritative exit code, run the check via `project.runCheck` — shape below).
  A per-entry `error` appears for an unknown/dead id. All are read defensively.
- `terminal.getOutput({ terminalId, maxLines 1–1000 })` → `{ terminalId, content, lineCount, truncated }`.
  Scrollback is in `content`.
- `agent.launch({ agentId, name?, worktreeId?, model?, prompt, requestKey })` →
  `{ launched: boolean, terminalId, location: "grid"|"dock"|null, spawnStatus:
  "missing-cli"|null, worktreeId, worktreePath, branch, cwd }` — the launch now returns
  the spawned agent's **full worktree/branch identity**. Every key is present (required)
  and every field after `launched` is nullable; a declined launch is `launched: false`
  with the identity fields `null`, never a bare `null` result. `spawnStatus:
  "missing-cli"` is an atomic negative result: Daintree opened a setup diagnostic panel
  and did **not** spawn an agent PTY (the diagnostic can still carry a `terminalId` /
  `location`), so the Assistant fails the saga. `model?` (optional string) overrides the
  model the spawned agent runs under; omit it to use the agent's default. **CLI
  caveat:** `internal/tools/agenttaskx` consumes `terminalId`, `spawnStatus`, the
  returned `worktreeId` (falling back to its caller-supplied value when omitted), and
  the legacy `taskId`; it does not yet consume `launched`, `location`, `worktreePath`,
  `branch`, or `cwd`.
- `forge.getCIStatus({ prNumber, worktreeId?|worktreePath?|cwd? })` →
  `{ ciStatus: { state, total, passed, failed, pending, requiredChecksPassing? } | null }`
  (wrapped, structured output — the wrapper is never bare; a `null` `ciStatus` means no
  such PR). `state` is `success|failure|pending|neutral|unknown` and is **the** answer —
  read it, not the counts. The counts cover **required checks only**, and `total: 0` is
  ambiguous: it appears when the required-check list could not be read in full, and on
  GitHub a PR with *no* required contexts falls back to the raw check roll-up (a red PR
  with no required checks still reports `state: "failure"` with `total: 0`). `neutral`
  means no recognized CI state at all (commonly: no CI) — there is **no** definitive
  "nothing gates the merge" discriminator in this result, so never treat `neutral` or
  `total: 0` as proof the merge is ungated. The built-in GitHub provider caches the
  status ~60 s, so poll for a settled verdict rather than trusting one read. No
  `rawData` / `freshnessToken` in the result.
- `forge.listIssueComments({ issueNumber, cursor?, perPage?, worktreeId?|worktreePath?|cwd? })`
  → `{ items, hasMore, nextCursor, totalCount? }`, **oldest-first** (the provider has no
  reliable newest-first sort — page to the end for the latest reply; `perPage` is 1–100,
  default 20, and `totalCount`, when present, is the authoritative whole-thread count).
  An empty **first** page means nobody commented: a missing issue or a provider that
  can't read threads **throws** instead, so silence is never ambiguous.
- `project.runCheck({ projectId, runnerId, cwd?, timeoutMs? })` (action tier,
  `danger:"safe"`) → `{ projectId, cwd, runnerId, runnerName, command, passed,
  exitCode: number|null, signalName: string|null, durationMs, timedOut, aborted,
  output, outputTruncated }`. Runs the check as a **real child process** (not a PTY),
  so `exitCode` is authoritative — unlike `lastCheckResult`'s heuristic scrollback
  parse. `runnerId` comes from `project.detectRunners({ projectId? })` →
  `{ runners: [...] }` (detection returns every safe runner it finds in supported
  manifests — ordinary scripts, publish/deploy included, not just checks — so verify
  what a command is before running an unfamiliar one). A failing check is a normal
  `passed: false` result; only a genuine inability to run throws. `cwd` must resolve to
  the project root or a registered worktree, not an arbitrary directory. Timeout
  defaults to 10 min (max 1 h); output is capped at 50 KiB tail-preserving and
  secret-scrubbed. Never point it at a long-lived server command — it blocks until the
  timeout.
- `terminal.armByState` / `terminal.armAll` / `terminal.armDefault` and the `fleet.*`
  dispatch family are **renderer-only** — **not** callable over MCP, not even via
  `daintree.call` (`fleet.getRunStatus`, a read-only snapshot of the user's in-app
  fleet broadcast, is the one MCP-exposed exception). The only MCP-exposed arming
  surface is `terminal.arm` / `terminal.disarm` / `terminal.disarmAll` (each →
  `{ armed: string[] }`).
- Terminal focus is **not** a `terminal.focus` MCP tool — Daintree uses
  `panel.focus({ panelId })` where the terminal id *is* the `panelId`. The local
  `terminal.focus` wrapper maps onto it.
- Read tools (workbench tier, no confirmation): `actions.getContext` / `list` / `search` /
  `getSchema`, `project.getCurrent`, `agent.listAvailable`, `agent.listToolbar`,
  `cliAvailability.get`,
  `worktree.list`, `worktree.getCurrent`, `git.getProjectPulse`, `terminal.list`.
  `agent.launch` and `terminal.waitUntilIdle` are action tier (mutations confirm).

## State models (from Daintree)

`domain.AgentState` is one of: `idle` | `working` | `waiting` | `completed` | `exited`.

- `"directing"` exists in Daintree's renderer but is **renderer-only** — you will not see
  it over MCP, so the CLI must not depend on it.
- **`"completed"` is TRANSIENT, not terminal.** `working → completed` fires only on a
  detected completion event, then bounces back to `"waiting"` (next silence) or
  `"working"` (more output) within seconds (hysteresis ~500ms). A status poll rarely
  catches it, so a single `"completed"` read must NOT be treated as "done" without the
  small-model tail check — which is exactly why finish detection keys on a confirmed
  `working → waiting` (or `exited`) plus a judge, never on catching `"completed"`.
- `waitingReason` is present **only** while `agentState` is `"waiting"` — `"prompt"`
  (silence-detected pause) or `"question"` (the agent is asking for input).
- Exit is the `"exited"` state; `exitCode` is then exposed (tri-state — see shapes above).
  The CLI treats a nonzero code as failure evidence, **not** as a completion trust gate
  (completion trust still requires the watcher's deterministic git-cleanliness check
  before any irreversible action is suggested — CLI policy, unchanged by the new
  `project.runCheck` surface).
- `agent.getState({ agentId })` **does** exist (keyed by **agent** id, not terminal id) —
  it returns a live single-agent snapshot (`{ state, waitingReason, exitCode, spawnedAt,
  terminalId, found }`; an unknown id returns `found:false` rather than erroring). The CLI
  mostly reads state through `terminal.getStatus`/`terminal.list` (via `context.snapshot`)
  and has no typed wrapper for it, but it is reachable via `daintree.call`. Agent state is
  also the subscribable resource `daintree://agent/{agentId}/state`.

`actions.getContext` returns the active project / worktree / focused terminal snapshot.

### Resources

- `daintree://agent/{agentId}/state` — subscribable, keyed by agent id.
- `daintree://terminal/{id}/scrollback`
- `daintree://worktree/{id}/pulse`

## Idempotency

Mutating tools accept a `requestKey` for dedup (`terminal.new`, `worktree.createWithRecipe`,
`agent.launch`, `recipe.run`, `git.commit`, etc.). The CLI sends an idempotency key for every
autonomous mutation. Daintree dedupes on `requestKey` and strips it before validation, so it
never trips schema checks.

## Confirmation

`danger: "confirm"` tools may trigger an MCP elicitation. The CLI confirms with the user
locally BEFORE calling such tools (see safety/policy). `git.commit`, `git.push`, and
`worktree.delete` **always** require explicit confirmation.

If a call fails with `SESSION_BINDING_GONE` or `BINDING_STALE`, the bound Daintree window
is gone — stop retrying that session and tell the user.

## Known Daintree-side gaps

Rough edges at the Daintree boundary, tracked here so the local workarounds can be
retired as the boundary improves. (Four Daintree-side gaps used to live here; two closed
in mid-2026: `agent.launch` now returns full worktree/branch identity, and
`project.runCheck` provides an authoritative check-run signal — both documented in the
contract sections above. What remains below is mostly **CLI-side adoption work**, not
host limitations.)

1. **`DAINTREE_WINDOW_ID` contract.** Daintree injects it but it had been referenced only inside
   a prompt string; it is now read into config so per-window/per-project state isolation can use
   it. This overlaps directly with **assistant-repo issue #4** (per-project state isolation) —
   #4 owns the richer per-window state-dir derivation; this repo only standardizes reading the
   env into config.
2. **No dedicated capability probe adopted.** A healthy connection + non-empty tool list does
   not prove the token's tier can actually call anything. `/doctor` covers this today by calling
   `actions.getContext` (workbench tier, read-only) as a functional probe. Daintree now ships
   `mcp.surface` (see the discovery section), which reports the session's tier and a stable
   surface hash — the natural target for both the `/doctor` probe and the startup drift
   check, but the CLI hasn't adopted it yet.

For discovery beyond the lists above, use `tool.search` / `daintree.listTools` rather than
guessing tool names.

## Keeping the references in sync

**There is no longer a hand-maintained transcription of the host's tool surface to keep in
sync.** The CLI learns the surface from the live server, so a Daintree tool change needs no
matching edit here to stay correct:

1. **Retry safety is read from the server.** Daintree ships MCP `annotations` on every
   `tools/list` entry (`readOnlyHint` / `idempotentHint` / `destructiveHint`, derived from
   each action's `kind` and `danger`). `internal/mcp` derives each tool's auto-retry
   eligibility from those annotations when it warms the tool cache — see
   `retrySafeFromAnnotations`. A tool is retryable iff the server declares it read-only and
   does not contradict itself by also declaring it destructive; anything unlisted,
   unannotated, or declared mutating is forced single-shot. This replaced the former
   `readOnlyToolNames` allowlist, which covered a fraction of the surface and could drift
   into claiming a mutation was a read.
2. **The drift baseline is derived from code, not transcribed.** `internal/app` passes
   `mcpx.WrappedMCPToolNames()` as `mcp.Options.DriftBaseline` — the raw host tool names
   that have typed local wrappers (`internal/tools/mcpx/discovery.go`). Those are exactly
   the names whose removal or rename would silently break a wrapper, and the list cannot go
   stale because a wrapper cannot exist without its entry. Drift stays *missing-only*: a
   depended-on name absent from the live server warns; extra live tools are expected and
   ignored, so they need no entry here to be callable via `daintree.call`.
3. **This doc** (`docs/DAINTREE_MCP.md`) — the human-facing companion. Illustrative, not an
   allowlist and not load-bearing: nothing in the code reads it, so a stale example here
   cannot change behavior. Update it when it would mislead a reader.
4. **The backend skills** (`../assistant-backend`,
   `src/daintree_assistant_server/skills/files/*.md` — e.g. `daintree.foundation.md`) —
   the model-facing guidance, injected server-side since the backend migration (the old
   embedded `internal/prompts/daintree_mcp.go` reference was deleted with it). Hand-written
   like this doc: they name the local wrappers plus a few high-value unwrapped tools and
   point the model at `tool.search` for the rest — deliberately **not** an enumeration of
   every server tool; update them when a surface change alters an actual workflow.

The one remaining hand-written list is `DocsToolNames` (`internal/mcp/docs.go`) for the
public documentation MCP — a fixed third-party surface with no annotations to derive from.
