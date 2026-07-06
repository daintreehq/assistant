# Workflow Intelligence (DWIL)

The **client-owned, backend-assisted workflow execution graph**: the assistant
is the durable source of truth for plans, progress, resource links, async
completions, blockers, and next actions; the backend stays stateless on Cloud
Run and contributes three pure request/response planning tasks plus prompt
rendering. This document covers the CLI side (this repo). The backend side
(task profiles, prompts, `TurnContext.workflow_state`, capabilities flag) lands
in `../assistant-backend` — see "Backend contract" below for exactly what this
client expects.

**Rollout flag:** `DAINTREE_WORKFLOW_INTELLIGENCE=1` (trusted-or-own env, like
`DAINTREE_ASSISTANT_DEBUG_LOG`; never the project `.env`). Off by default.
When off, the runtime is byte-identical to before the feature: no graph tools
register, no observer installs, no `workflow_state` field is ever sent (the
backend validates `TurnContext` with `extra="forbid"`, so a backend without
the matching contract must never see the field).

## The graph

`internal/workflowgraph` owns the typed model: a `Graph` is one user goal as a
DAG of `Node`s (kind ∈ orient/delegate/wait/verify/synthesize/ask_user/
approval/cleanup/report; status ∈ pending/ready/running/waiting/blocked/done/
skipped/failed/cancelled) plus `Edge`s, `Resource`s (terminal/watcher/async/
worktree/branch/pr/…), `Blocker`s, `Decision`s, a bounded `Evidence` ring, and
an advisory `NextAction`.

Hard invariants (`validate.go`, enforced on EVERY ingest path):

- unique node ids; edges/dependsOn reference existing nodes; acyclic over the
  UNION of `DependsOn` and `Edges`;
- node/graph status transitions follow the legal-transition tables
  (`done/skipped/cancelled` are hard-terminal; `failed → ready` is the retry);
- `ActiveNodeIDs` is DERIVED (rebuilt mechanically from node statuses — never
  hand-maintained), and pending nodes auto-promote to ready when their
  prerequisites are done/skipped;
- `NextAction.toolName` must exist in the live tool registry, else the action
  is dropped with a warning;
- a graph can be `done` only when every node is terminal-and-not-failed, OR
  the success criteria were explicitly waived with a reason;
- structural caps keep the graph compact (≤64 nodes, ≤128 edges, evidence ring
  ≤200 with oldest-first pruning; field clamps at every ingest).

**One write path.** Every mutation — backend patch, tool call, observer,
async settle — flows through `ApplyPatch` (deep copy → mutate → recompute
derived state → validate → commit-or-nothing) and lands via
`Store.UpdateWorkflowGraphSnapshot(id, expectedRevision, …)`, an optimistic
revision guard (typed `domain.ErrWorkflowGraphRevisionConflict`; the service
reloads and retries). A stale backend patch can never clobber newer local
state; a hallucinated patch is rejected before anything commits.

## Storage (schema v10)

Four project-scoped tables (`internal/storage/workflowgraph.go`):

- `workflow_graphs` — the JSON snapshot + promoted status/goal/revision
  columns (the typed model serializes via `workflowgraph.EncodeSnapshot`).
- `workflow_events` — append-only projection log (planned / reconciled /
  evidence / resource_linked / async_settled / cancelled), the reconcile
  task's recent-events feed.
- `workflow_resource_links` — the REVERSE index (natural key
  workflowId+type+ref): an async completion or queue event carrying only its
  own id maps back to the owning graph/node here.
- `workflow_reconcile_runs` — forensic rows per backend reconcile call
  (base/applied revision, input/output hashes, outcome).

Graphs are project-scoped like watchers: rows survive process boundaries and
the supervisor daemon reads the same state (its wake turns get the same
digests).

## Tools

Seven flag-gated tools extend the `workflow.*` family (the flat
create/get/list/update ledger tools are untouched):

- `workflow.plan` (local) — backend `workflow_plan.v1` → validated, stored
  graph. Planning over a LIVE graph is refused without `forceReplan` (which
  cancels the old graph as superseded).
- `workflow.getGraph` (read) — compact (default) or full view.
- `workflow.next` (read) — locally-computed ready/waiting nodes + blockers +
  stored next action.
- `workflow.attachResource` / `workflow.recordEvidence` (local) — manual
  linking/evidence (most linking is automatic, below).
- `workflow.reconcile` (local) — backend `workflow_reconcile.v1` over the
  current snapshot + recent events; the returned patch is validated on a copy
  and applied under the read revision. `apply:false` previews. It PATCHES,
  never re-plans.
- `workflow.cancel` (local) — graph/node state only; NEVER closes terminals,
  deletes worktrees, or cancels external work.

## Automatic capture

- **Dispatch observer** (`tools.Registry.SetDispatchObserver`, installed in
  `app.Create`): every completed dispatch is offered to the service as a pure
  after-the-fact side-channel (panic-guarded, like the audit sinks — it can
  never alter tool behaviour or safety decisions). Material calls only:
  mutations (terminal/project/external/git/system risk), accepted async work,
  watcher/timer creation, and unrecoverable real-tool failures — never plain
  reads, `workflow.*` bookkeeping, denials, or model slip-ups
  (UNKNOWN_TOOL/INVALID_ARGS). Targeting: explicit `workflowId`/
  `workflowNodeId` args win; else a SINGLE open graph (and its single
  in-flight node, when unambiguous); else skip — junk attribution is worse
  than none. Resources are extracted from the typed `ToolResult.Async` handle
  and a bounded known-key walk of the result payload
  (terminalId/watcherId/timerId/worktreeId/asyncId/branch/prUrl/…).
- **Async settlement** (`asyncwork.Deps.WorkflowSink`): after the coordinator
  publishes a completion (never before — a sink failure must not lose the
  wake), each settled invocation routes back through the resource-link
  reverse index: evidence lands on the graph, the linked node transitions
  waiting/running → done|failed|cancelled, and the link row records the final
  status. The wake turn then continues the ORIGINAL workflow via the digests
  below.

## Turn context

`TurnContext.workflow_state` carries ≤5 compact digests of the open graphs
(id, goal, status, "2/5 nodes done; current: …", active nodes, newest
resources, open blockers, next action, last event), re-read every round like
the async ledger, clamped per-field to the backend's max_lengths and bounded
to 16 KiB total (`backend.CapWorkflowDigests`). Wired only when the feature
flag is on (nil `SessionDeps.WorkflowDigestLister` ⇒ field omitted).

## Surfaces

- `/workflows` prepends the open graphs above the flat ledger runs.
- `/workflow` lists graphs; `/workflow <wfg_id>` shows the plan view (glyphs
  ✓ → ○ ⚠ ×); `/workflow resume [msg]` runs `workflow_resume_digest.v1`;
  `/workflow reconcile <id>` reconciles manually; `/workflow cancel <id>`
  cancels locally.
- The cockpit operations deck gains a WORKFLOWS section (cap 3, two lines per
  graph, width-clamped) that vanishes entirely when there are no open graphs.

## Safety

The graph records and recommends; it never executes. `NextAction` (and its
`requiresConfirmation` flag) is informational — dispatch remains the sole
authority for tier gates, confirmations, and grants. No new path edits files,
and graph/node cancellation never touches terminals or external work. All
graph tools are read/local risk (pinned by test).

## Backend contract (what this client expects)

- Tasks `workflow_plan.v1`, `workflow_reconcile.v1`,
  `workflow_resume_digest.v1` at `/v1/daintree/tasks` with the pydantic
  input/output models from the DWIL spec (snake_case; see
  `internal/backend/workflowtasks.go` for the exact wire shapes the CLI
  sends/decodes).
- `TurnContext` gains `workflow_state: list[WorkflowDigest]` (id ≤128, goal ≤
  512, status ≤64, line fields ≤512 — the client clamps first).
- `/v1/daintree/capabilities` may advertise the feature; the CLI currently
  gates on the env flag alone (capabilities gating can be added when the
  backend lands).

Until the backend ships those, running with the flag on degrades gracefully:
plan/reconcile/resume fail with a clear "task rejected" error while every
local operation (get/next/attach/evidence/cancel, observer, async linking,
digests) works — but the digests only reach the model once the backend
accepts the `workflow_state` field, so keep the flag off against an old
backend.
