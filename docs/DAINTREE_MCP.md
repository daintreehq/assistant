# Daintree MCP — integration notes

The CLI connects to Daintree's **local MCP server** as an external client. In production
Daintree launches the CLI and passes the connection details via environment / flags.

> **Source of truth.** There is no longer a local prompt surface here — the base prompt,
> the developer instructions, and every runbook body are **backend-owned**
> (`../assistant-backend/src/daintree_assistant_server/prompts/` and
> `.../runbooks/files/*.md`), so what the model reasons against is authored over there.
> The CLI-side verified contract is
> [`DocumentedMcpToolNames`](../internal/mcp/tools.go) — the exact, ordered, test-pinned
> drift baseline the CLI checks against the live server at startup. This doc is the
> human-facing companion to that list; if they disagree, `internal/mcp/tools.go` wins.
> A prompt or runbook that describes an MCP tool wrongly is fixed in the **backend** repo,
> not here.

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
can be told apart from another. It is read into config (`AppConfig.WindowID`) and
surfaced by `/status` via `config.DescribeConfig`. Daintree injects it as env; a headless
caller can also say it in argv as `--window-id`, or per session as `windowId` on
`daintree.session.open` (both beat the env, like every other harness flag). (`/doctor`
reports connection health and a live MCP probe rather than the raw config values.)

The client (`internal/mcp`) uses `github.com/modelcontextprotocol/go-sdk`: it connects
over the Streamable HTTP transport with an `Authorization: Bearer <token>` header and
falls back to the legacy SSE transport on failure.

`ListTools` → tools with `{ name, description, inputSchema }`.
`CallTool(name, arguments)` → content text plus optional `structuredContent` and an
`isError` flag, normalized into a `domain.ToolResult`.

## Tiers

`workbench` (read/safe) ⊂ `action` (+mutations) ⊂ `system` (+dangerous). External API-key
clients get an `external` tier (~70 read-safe + creation tools, no dangerous mutators like
`git.push`). The CLI treats the tier as advisory and adds its OWN safety layer on top.

As of daintreehq/daintree#12140, `worktree.delete`, `worktree.deleteOwned` and
`worktree.resource.teardown` sit at the **action** tier, not `system` — cleaning up a
worktree it just finished with is ordinary orchestration for an in-app assistant. That
moved the boundary from "irreversible or not" to "is the tier the only gate this tool
has": all three are `danger: "confirm"`, so admission still ends at a confirmation —
pre-authorized by a native automation grant, or else a dialog — whereas `git.commit` and
`forge.assignIssue` are `danger: "safe"` and the tier is all they have.
**None of this changes the CLI**, which classifies independently and deliberately more
narrowly: `worktree.delete` is in `neverDynamic` (internal/tools/mcpx/policy.go) and
stays behind `daintree.call`'s system-tier typed confirmation whatever tier Daintree
puts it at. Read that as the two layers agreeing to disagree, not as drift to fix.

## Representative action / tool ids (RAW names — prefer the typed wrapper where one exists)

These are Daintree's raw action ids. Many are reachable through `daintree.call`, but the
ones we wrap typed are denylisted there (see `wrappedMCPTools`) and must go through the
wrapper — the raw forward would skip its validation.

Terminals: `terminal.list`, `terminal.new`, `terminal.getOutput`, `terminal.getStatus`,
`terminal.sendCommand`, `terminal.inject`, `terminal.waitUntilIdle`, `terminal.rename`,
`terminal.close`, `terminal.kill`, `terminal.killBatch`, `terminal.moveToWorktree`,
`terminal.arm` / `terminal.disarm` / `terminal.disarmAll`.

> `terminal.killBatch` (daintreehq/daintree#12141) takes an explicit id list (≤32,
> distinct) and raises ONE confirmation with a checkbox per row, reporting five
> per-target buckets: `killedIds`, `excludedIds` (a human unticked it — never retry
> those), `notFoundIds`, `skippedIds` and `failedIds`. It is agent-only and has no
> CLI wrapper, because the cleanup path deliberately prefers `terminal.close` — kill
> deletes permanently where close trashes. Reach for it only when the user asked to
> kill rather than close.

> `terminal.moveToWorktree` files a pane under another OPEN worktree; `worktreeId` is
> matched exactly (a branch name is never accepted) and a pane sharing a tab group takes
> the group with it. It NEVER restarts the process — the moved agent keeps running in the
> directory it launched from until it is told `Please continue in the directory <path>`.
> `terminal.moveToNewWorktree` refuses agent dispatch outright: create the worktree first,
> then move. (`terminal.armByState` / `armAll` and `fleet.*` are renderer-only —
not MCP-callable; see the arming note below.)

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
`worktree.waitUntilReady`, `worktree.delete`, `worktree.setActive`, `worktree.refresh`,
`worktree.listBranches`,
`worktree.compareDiff` (read-only: the files that differ between two worktrees' branches).
Resource lifecycle: `worktree.resource.provision`, `worktree.resource.teardown`,
`worktree.resource.pause`, `worktree.resource.resume`, `worktree.resource.status`.
(`worktree.resource.connect` is **renderer-only** — no MCP surface, despite earlier
docs listing it here.)

> **Shapes changed in daintreehq/daintree#12157.** `worktree.createWithRecipe` now takes
> a discriminated `source` union (`newBranch` | `existingBranch` | `pullRequest`) in
> place of five conditionally-optional top-level fields whose legal combinations were
> only enforced inside `run()`. `worktree.create` returns `{ worktreeId, branch }` and
> `forge.getPR` returns `{ pr }`, both previously bare values that could advertise no
> output schema. The CLI absorbs all three without code: its `createWithRecipe` wrapper
> forwards an opaque `arguments` record, so the model reads the live shape from
> `tool.schema`; and `extractPrFields` already unwraps a `pr` envelope
> (internal/daemon/prwatcher.go `candidateObjects`). Creation also now reports
> `setupStatus` separately from `lifecycleStatus`, because git returning is not the same
> as config copy, submodule init and the setup script having finished — three failure
> paths there used to report as ready.

Git: `git.getProjectPulse` (HISTORICAL activity/pulse), `git.getStagingStatus` (live
working-tree state), `git.commit`, `git.push`, `git.stageAll`, `git.stageFile`,
`git.unstageAll`, `git.unstageFile`, `git.listCommits`, `git.getFileDiff`.
(Of these, only `git.getProjectPulse` has a typed CLI wrapper — the rest, incl.
`git.commit`/`git.push`/`git.getStagingStatus`, are reached via `daintree.call`. The
former `git.snapshot*` family was removed from Daintree as part of a feature cleanup;
the CLI dropped its `git.snapshotRevert`/`git.snapshotDelete` wrappers in lockstep.)

Forge (reads): `forge.listIssues`, `forge.listPRs`, `forge.getIssue`, `forge.getPR`,
`forge.getPRs` (batch: 2-20 distinct known numbers in one round trip, added in
daintreehq/daintree#12157). `forge.getPRs` reports a per-number status — `found`,
`not_found` (asked, and it is not there) or `unresolved` (nothing was learned). The old
IPC handler collapsed the last two by dropping the entry, which made a rate-limited
lookup indistinguishable from a deleted PR; the typed wrapper keeps them apart because
acting on that confusion closes work that exists.
Forge (issue writes): `forge.createIssue`, `forge.closeIssue`, `forge.reopenIssue`,
`forge.editIssue`, `forge.addIssueComment`, `forge.addIssueLabel`,
`forge.removeIssueLabel`, `forge.assignIssue`, `forge.unassignIssue`.
Forge (PR writes): `forge.createPR`, `forge.closePR`, `forge.reopenPR`, `forge.mergePR`,
`forge.convertPRToDraft`, `forge.markPRReadyForReview`, `forge.commentOnPR`, `forge.editPR`.
Forge (review writes): `forge.approvePR`, `forge.requestChanges`, `forge.dismissReview`,
`forge.requestReviewers`. All forge writes are `external`-risk and the CLI always confirms
them (its own safety layer). On the Daintree side the issue writes split two ways since
daintreehq/daintree#12143: `forge.createIssue`, `forge.addIssueComment` and
`forge.reopenIssue` were raised to `danger: "confirm"` — each publishes a durable public
record this capability cannot retract, and closing an issue removes neither the issue nor
its watcher notifications — while `forge.assignIssue`/`unassignIssue` and the label pair
stay `danger: "safe"` as idempotent state-sets with exact inverses. The CLI confirmed all
of them before and still does, so this changes nothing here; it is recorded because
`localTargetPolicies` derives its `Danger` strings from this file. **The forge READS have
typed CLI wrappers** (`listIssues`/`getIssue`/`listPRs`/`getPR`/`getPRs`, plus
`forge.getChecks` over `forge.getCIStatus` and `forge.listIssueComments`) — every forge
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
> failedTerminalCount, assignedToSelf, … }`. The CLI **wrapper** then attaches a
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

Meta: `actions.list`, `actions.getContext`, `actions.search`, `actions.getSchema`.

> **First-party existence catalog (daintreehq/daintree#12139).** Tools above the
> session's tier used to be invisible, not merely refused: they were absent from
> `tools/list` AND from `actions.list`/`search`, and an out-of-tier `actions.getSchema`
> returned a `NOT_FOUND` deliberately identical to an unknown id's. So an assistant asked
> to clean up worktrees reported that Daintree exposes no worktree-delete action. For
> renderer-owned sessions `actions.list` and `actions.search` now carry an
> `unavailable[]` array (paginated independently, with its own `unavailableHasMore`), and
> `actions.getSchema` answers `TIER_NOT_PERMITTED` with a stub — exactly
> `{id, title, band, minimumTier, callable: false}`, no description and no schemas.
> **Consult it before reporting a capability as missing**, and name the required tier
> rather than denying the action exists. Nothing in the catalog is dispatchable, and
> external/api-key clients get the previous payload field for field. The CLI needs no
> decoder change: nothing here parses MCP results strictly. A SUCCESSFUL action result
> keeps both `text` and `structuredContent` through `passthrough` (passthrough.go:56),
> `daintree.invoke` (invoke.go:203) and raw `daintree.call` (discovery.go:452); the error
> paths repackage them into a failure envelope, and the local discovery tools build their
> own catalog results from `ListTools` rather than forwarding a call result at all. No
> path decodes a denial into a typed struct, so the widened shape reaches the model
> instead of a decoder that would reject it.

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

### Verified call/response shapes

- `terminal.getStatus({ terminalIds: string[] (1–256), includeOutput?: { lines 1–50, stripAnsi } })`
  → `{ terminals: [{ terminalId, agentId, agentState, waitingReason?, exitCode?, spawnedAt?, lastTransitionAt?, lastCheckResult?, recentOutput?, armed?, error? }] }`.
  There is **no** flat `agentState` and **no** `runtimeStatus`. `exitCode` is tri-state —
  a **number** on a clean exit, **null** on a signal kill, **absent** while running — so
  its *presence* (not value) signals the exit. `spawnedAt` / `lastTransitionAt` are
  epoch-ms timestamps (`lastTransitionAt` = when the agent entered its CURRENT state, not
  when it last produced output). `lastCheckResult` (when present) is a best-effort parse
  of the agent's last test/lint/build summary — useful evidence, **not** authoritative.
  A per-entry `error` appears for an unknown/dead id. All are read defensively.
- `terminal.getOutput({ terminalId, maxLines 1–1000 })` → `{ terminalId, content, lineCount, truncated }`.
  Scrollback is in `content`.
- `agent.launch({ agentId, name?, worktreeId?, model?, prompt, requestKey })` →
  `{ terminalId, location, spawnStatus? }` (no `worktreeId` or `taskId`). Optional
  `spawnStatus: "missing-cli"` is an atomic negative result: Daintree opened a setup
  diagnostic panel and did **not** spawn an agent PTY, so the Assistant fails the saga.
  `model?` (optional string) overrides the model the spawned agent runs under; omit
  it to use the agent's default.
- `terminal.armByState` / `terminal.armAll` / `terminal.armDefault` and the whole
  `fleet.*` family are **renderer-only** (no `mcpOutputSchema`) — **not** callable over
  MCP, not even via `daintree.call`. The only MCP-exposed arming surface is
  `terminal.arm` / `terminal.disarm` / `terminal.disarmAll` (each → `{ armed: string[] }`).
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
  (completion trust still requires the git verification pass — see gaps below).
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

A forced `worktree.delete` is stronger than that. Since daintreehq/daintree#12131 an
MCP-dispatched `force: true` whose live target resolves to a protected branch, the main
worktree, tracked changes or at-risk submodule content escalates to the TYPED-NAME
confirmation — the user types the branch or worktree name — and the gate is re-derived
immediately before dispatch, so a worktree that turned dirty after approval is refused
with `CONFIRMATION_REQUIRED` rather than deleted. **A Daintree automation grant does not
waive this**: since #12131 a grant pre-authorizes only the standard confirmation tier, and
a granted force delete whose live tier is escalated gives up its pre-authorization and
raises the dialog anyway. Note the refusal arrives marked `retriable: false`, so treat it
as "the context changed, re-decide" rather than "retry the identical call".

Separately, `terminal.killAll` and `terminal.closeAll` are barred from native automation
grants entirely (daintreehq/daintree#12128): they resolve their targets from live renderer
state inside `run()`, so a `maxUses: 10` grant read as ten careful approvals while
actually authorizing ten unbounded sweeps. They still run — they just always face the
confirm dialog and never spend a grant use. `terminal.killBatch` (daintreehq/daintree#12141)
declares itself `per-resolved-target` in that same policy, so it is bounded the same way.

If a call fails with `SESSION_BINDING_GONE` or `BINDING_STALE`, the bound Daintree window
is gone — stop retrying that session and tell the user.

## Known Daintree-side gaps

These are limitations in Daintree's MCP surface that the CLI works around locally. They are
**not fixable in this repo** — they are tracked here so the workarounds can be retired if and
when Daintree closes the gap.

1. **No test/lint completion signal.** `terminal.getStatus` now exposes a numeric `exitCode`
   (plus `spawnedAt` / `lastTransitionAt`), which the watcher consumes as signal evidence — a
   nonzero exit is surfaced as failure evidence on a `terminal_exited` event
   (`internal/daemon/watcher.go`). It is **not** a completion trust gate: there is still no
   test/lint runner signal, so the irreversible-action gate from **issue #3** continues to
   derive completion trust from a deterministic git-cleanliness check, not the exit code.
2. **`agent.launch` returns `{ terminalId, location, spawnStatus? }`.** No `worktreeId` or
   `taskId`, so `internal/tools/agenttaskx` degrades gracefully (caller-supplied
   `worktreeId`, no `taskId`). `spawnStatus: "missing-cli"` is a clean unavailable failure
   even though the diagnostic panel has an id. If Daintree returns the missing identity
   fields later, the spawn tool can stop guessing.
3. **`DAINTREE_WINDOW_ID` contract.** Daintree injects it but it had been referenced only inside
   a prompt string; it is now read into config so per-window/per-project state isolation can use
   it. This overlaps directly with **issue #4** (per-project state isolation) — #4 owns the
   richer per-window state-dir derivation; this repo only standardizes reading the env into config.
4. **No live capability probe in the protocol.** A healthy connection + non-empty tool list does
   not prove the token's tier can actually call anything. `/doctor` works around this by calling
   `actions.getContext` (workbench tier, read-only) as a functional probe. If Daintree adds a
   dedicated health/capability endpoint, the probe can target that instead.

For discovery beyond the lists above, use `tool.search` / `daintree.listTools` rather than
guessing tool names.

## Keeping the references in sync

There are three CLI-side records of the Daintree MCP surface; when Daintree's tool surface
changes, update them together:

1. **This doc** (`docs/DAINTREE_MCP.md`) — the human-facing companion. May list more raw
   Daintree tools than the model is told about (the catalog above is illustrative, not the
   model's allowlist).
2. **The model-facing reference** — now in the **backend** repo
   (`../assistant-backend/src/daintree_assistant_server/prompts/`), not here. It names the
   local wrappers plus a few high-value unwrapped tools and points the model at
   `tool.search` for the rest; it deliberately does **not** enumerate every server tool.
   The old `internal/prompts/daintree_mcp.go` was deleted with the rest of the local
   prompt machinery — a wrong description there is a backend fix.
3. **The drift baseline** — `DocumentedMcpToolNames` (`internal/mcp/tools.go`), the single
   authoritative CLI-side list, pinned by tests. This is an **exact, minimal,
   verified** subset: at startup the CLI checks each name is still on the live
   server (a missing one signals the doc went stale). Drift is *missing-only* — extra live
   tools (like `worktree.compareDiff` or the `worktree.resource.*` family) are expected and
   ignored, so they don't need to be added here to be callable via `daintree.call`.
