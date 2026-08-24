# Sub-agents (delegated research)

A **sub-agent** is a bounded, read-only agent loop that runs in its own isolated
conversation, so the main thread can learn one answer without paying the context
cost of finding it.

The model reaches it through one tool, `subagent.run`. Everything else described
here is machinery the model never sees.

## The problem

Some questions are cheap to *answer* and ruinously expensive to *research*:

- which of four thousand issues describes the terrain flicker;
- which files under a 900-directory tree belong in a `copyTree` scope;
- where a symbol is actually defined in a repo nobody has read yet.

Doing that work on the main thread means every listing, every search dump, and
every dead end lands permanently in the conversation the human is having. It
burns the context window, drags every later turn's prompt behind it, and buries
the answer inside the transcript that produced it.

## The shape

```
main thread                          sub-agent (own conversation)
───────────                          ────────────────────────────
subagent.run{task, deliverable}  ──▶  round 1: fs.search ×2
                                      round 2: fs.read
                                      round 3: writes the report
        report (≤12k chars)      ◀──  ▏ everything above is discarded
        + transcriptId                ▏ full transcript → artifact
```

Exactly one thing crosses back: the report. The full run — every round, every
tool call, every result at full length — is written to an artifact, so
`artifact.read <transcriptId>` recovers the detail on demand. The receipt exists;
it just is not in the conversation by default.

**Measured on a live three-way fan-out** (2026-08-21): the main thread's prompt
grew from 31,317 → 32,084 tokens across the whole delegated search — **+767
tokens** — while the sub-agents spent ~32,000 prompt tokens doing the reading.

## The three load-bearing properties

### 1. Read-only, structurally

`internal/app.subagentToolNames()` filters the registry to `domain.RiskRead`
tools (36 of 78 today), minus a small denylist. Dispatch runs under
`domain.ActorSubagent` with **no `Confirm` and no `AskChoice` hook**, so a
mutating call that somehow reached dispatch takes the non-interactive
grant-or-blocked branch and fails closed rather than prompting a human who is not
watching.

Risk class is the gate, and that is the whole safety argument: `RiskRead` is the
registry's own claim that a tool changes nothing, already enforced at
registration and already used by the tier policy for every other caller. A
hand-kept allowlist would go stale the next time a tool family is added; this
inherits an invariant the codebase maintains anyway.

That is why `subagent.run` is *itself* a read-risk tool — it needs no
confirmation, and it can be fanned out in parallel.

### 2. Bounded

| Bound | Default | Why |
|---|---|---|
| Rounds | 10 (ceiling 24) | A well-aimed brief finishes in 3–6. |
| Wall clock | 5 min | One tool call can block far longer than a round budget suggests. |
| One tool result in history | 24,000 chars | Generous — a sub-agent *should* read big things. |
| Total history | 220,000 chars | Past this it has stopped narrowing and started hoarding. |
| Report | 12,000 chars | The bound that actually protects the caller's context. |

Hitting any bound does **not** end the run empty-handed. The loop spends one more
round with tools withheld (`tool_choice: "none"`) to make the model write up what
it has, and the report comes back `status: "exhausted"`, `partial: true`. The
tool summary shouts `PARTIAL` — a partial report that reads as a complete one is
the single most damaging thing this feature can return.

### 3. Never fatal to the caller

Every failure — backend error, bad tool call, cancel, exhausted budget —
resolves to a `Report` with a `Status`, not an error the calling tool has to
invent a message for. A backend failure *after* some reading has happened is
salvaged with a wrap-up round on the history it already has; only a failure on
round 0 has nothing to sell.

## Runbooks for sub-agents

A sub-agent selects runbooks too — from a menu narrowed to the ones it can actually
execute. This is the reason it is given a real tool inventory rather than a fixed
handful: a sub-agent sent to find one issue among thousands should get the
issue-searching runbook, not rediscover it every run.

The gate is `requiredTools`, which runbooks already declare and the tool projection
already trusts. `Catalog.executable_with(tools)` keeps a runbook only when every
tool it declares is one the caller holds. So:

- a runbook needing `agentTask.spawnForEdits` is filtered out — a read-only worker
  cannot follow it, and handing it over would spend context and then steer the
  model toward calls that can only fail;
- a research-shaped runbook written tomorrow becomes available to sub-agents the day
  it lands, with no second list to keep in step.

Measured against the real 36-tool inventory today: **2 of 39** runbooks pass, and one
of them is `daintree.workflow.find-issue-work` — the motivating case. That ratio is
an honest statement about the catalog, not the mechanism: it is currently almost
entirely orchestration runbooks. It should rise as research-shaped runbooks get
written, and nothing needs changing here when they do.

The active-set cap is tighter for a sub-agent (2, vs the orchestrator's setting):
it works to a hard round budget in a context it will exhaust on tool results, so
two 5k-token runbooks would cost more than they buy.

## Biasing the orchestrator toward delegating

A delegation primitive is worth nothing if it is reached for after the expensive
search has already happened. The bias is split across two surfaces, following the
base prompt's own rule (a rule that can fire on a turn loading NO runbook stays in
base; elaboration moves to the foundation runbook):

| Surface | Carries |
|---|---|
| `main/base/20-tool-discipline.md` | One bullet: when to delegate, what never to delegate, relay a partial as partial. Fires even on a degraded zero-runbook turn — which is exactly the turn where searching in the main thread does most damage. |
| `daintree.foundation` (item 8) | Fan-out batching, brief-writing, what not to delegate, how to relay a finding. |
| `daintree.explore.repository` | Rewritten around three tiers: one call yourself → **`subagent.run` (the default)** → a full explore agent for large-surface synthesis. |

That third one mattered most. The runbook previously said *"POINTER — do it
yourself"*, and the only delegate it knew was the heavyweight visible agent. A
measured run of "where is prompt-cache stability enforced? I have no idea where to
look" took **7 rounds, 15 tool calls, and grew the main context by 14,311 tokens**
— with that runbook loaded, instructing exactly that. A loaded runbook body sits closer
to the conversation than the base prompt and wins; fixing the base bullet alone
would not have moved it.

**Known gap.** `daintree.explore.repository` does not yet declare `subagent.run` in
its `requiredTools`. The backend pins a snapshot of the CLI tool inventory
(`tests/fixtures/cli_tool_names.txt`, keyed to the `cli/` submodule commit), and
that snapshot cannot name a tool until the CLI change lands and the pin moves. The
runbook BODY drives the behaviour and is already updated; refresh the fixtures with
`go run ./cmd/tooldump` and add the entry once the pin moves.

## Backend: the `subagent` profile

`RespondRequest.profile` selects the persona. Omitted (the CLI's default for
every ordinary turn) means `assistant`; the sub-agent sends `subagent`. It is not
a hint — it selects which of two assembly paths runs:

| | `assistant` | `subagent` |
|---|---|---|
| System prompt | `prompts/main/base/` | `prompts/main/subagent/` |
| Runbook selection | full catalog | **menu narrowed to executable runbooks** |
| Active-set cap | settings default | `min(default, 2)` |
| Docs lookup | on | off (it researches the project, not Daintree) |
| Step tracking in the runbook render | on | off — `runbook.step.advance` / `runbook.run.get` are denylisted from its inventory |
| Runtime / turn / display context | rendered | dropped |
| Startup block | yes | yes |

A sub-agent round is far cheaper than a main-thread one regardless: in the live
run above it cost ~10,100 prompt tokens against the main thread's ~31,300, from
the narrower tool inventory (36 vs 78) and the short persona.

`profile` is `omitempty` on the wire so an ordinary turn stays byte-identical and
the prompt cache does not split.

## Where things live

| Path | What |
|---|---|
| `internal/subagent/` | The runner: bounds, the loop, the wrap-up round, the transcript. Knows nothing about the App. |
| `internal/tools/subagentx/` | The `subagent.run` tool: argument shape, validation, progress forwarding, the result summary. |
| `internal/app/subagent.go` | The wiring: inventory filter, denylist, actor, transcript sink. |
| `../assistant-backend/.../prompts/main/subagent/` | The persona (4 partials). |

## Interface

The attached session needed no new event type — a sub-agent run is a tool call, so it uses
the surfaces tool calls already have. What it did need was three fixes, each found
by rendering the rows and looking at them:

```
├─ ◦ Sub-agent  Find the GitHub issue describing the terrain me…
├─ ⠋ Sub-agent  GitHub issue describing the t… · round 3/10 · fs.search, fs.read     4.2s
└─ ✓ Sub-agent  GitHub issue describin… · Reported back · 3 rounds, 4 tool calls · 11.5s
└─ ✓ Sub-agent  PARTIAL — stopped early · 11 rounds, 22 tool calls · 32.8s
└─ × Sub-agent  Find the GitHub issue describing the terrain me… · The sub-agent could …
```

**The row names the brief, not the tool.** `presentToolVerb` maps `subagent.run`
to the verb `Sub-agent` with the *task* as its target. A delegation the user
cannot read is one they cannot judge.

**Live progress replaces the detail while it runs** (`round 3/10 · fs.search`),
forwarded from the runner through `ToolContext.ReportProgress`. Without it the
attached session shows one frozen row for what can be a minute, and a delegation that looks
hung is one the user kills.

**Fan-out rows keep their identity.** The model is told to delegate several
questions at once, so three sub-agents run side by side under one verb. By the
default rules all three would read `round 3/10 · fs.search` while live and
`Reported back · 3 rounds…` when settled — three identical rows for three
different questions. `identityBearingTarget` keeps the brief as a prefix so each
row says which delegation it is.

Two refinements fell out of actually looking at the result:

- `briefGist` strips the leading imperative for display. Briefs are written by a
  model that opens nearly all of them the same way, so raw prefixes read
  "Find the GitHub issue describ…", "Find every Go file that reg…" — the shared
  boilerplate eats the ~30-cell budget and the identifying part is what gets cut.
- `fanOutDetail` drops the identity prefix entirely rather than shrink it past
  usefulness. This is a **safety** rule, and it was a real regression first: at 60
  columns the brief prefix pushed `PARTIAL — stopped early` off the row, rendering
  a partial finding exactly like a complete one. The volatile half is never
  sacrificed for the identity.

The result summary carries no `Sub-agent` prefix, because the row already renders
the verb — together they read `Sub-agent  Sub-agent reported back`, the same
stutter `terminal.close`'s "Ended" verb exists to avoid.

`internal/ui/subagent_activity_test.go` pins all of this, and
`TestSubagentRowsVisualSample` prints the block above so a reviewer can see the
rows without running the binary.

## Review findings worth remembering

An adversarial review of this feature found one release blocker and several real
defects. Recorded because each is a trap the next change could re-lay:

- **`package-data` globs do not recurse.** `pyproject.toml` listed
  `prompts/main/base/*.md` explicitly, so adding `prompts/main/subagent/` shipped a
  wheel with none of its partials — and since `PromptLoader` reads that directory
  *eagerly* at construction and raises when it is absent, the packaged server
  failed to boot for **every** request, not just sub-agent ones. Editable installs
  (how the test suite runs) read the source tree and cannot see this.
  `test_every_eagerly_loaded_prompt_dir_is_in_package_data` now asserts it.
- **Jinja whitespace is a wire change.** The env runs `trim_blocks=False`, so
  wrapping the step-tracking paragraph in `{% if %}…{% endif %}` with the `endif`
  on its own line emitted a stray newline into *every assistant request* — an
  unintended prompt-cache bust. Putting the `endif` at the end of the paragraph
  line makes the enabled variant byte-identical; the golden tests catch it either
  way, which is exactly what they are for.
- **Generations must not mix.** Runbook learning can swap the live catalog while the
  selector is awaiting. The assistant path resolves bodies from the *post*-await
  snapshot so bodies, metadata and the state token describe one generation; the
  sub-agent's narrowed catalog must be derived from that same snapshot. Deriving it
  from the pre-await one handed the model generation A's runbook while stamping
  generation B's revision — so the next round saw a matching revision and never
  reselected.
- **`filter_known` is not `selectable`.** A server-pinned runbook (the docs runbook)
  is hidden from selection but still *known*, and it declares no required tools, so
  it survives every narrowing. Carrying prior ids with `filter_known` would let a
  replayed assistant state inject it into a profile whose docs lookup is off. Same
  root cause made the "empty menu" check use `len()`, which never reaches zero.
- **A capped list is not an inventory.** `_available_tool_names` truncates at 100
  for selector *evidence*; using it to decide which runbooks are executable would let
  tool ORDER change the menu once a request carries more than 100 tools.
- **A caller-keyed cache needs a bound.** `Catalog._executable_cache` is keyed by
  the caller's tool names, so it is capped and evicted oldest-first.

And on the CLI side:

- **A counter that increments before the error check cannot be a "did anything
  succeed" predicate.** `round()` bumps `st.rounds` before returning a failure, so
  the `st.rounds == 0` guard for "first-round failure, nothing to salvage" was dead
  code — a round-0 failure went down the salvage path with no research behind it.
  Worse, the test covering it *passed*, because the fake ran out of script inside
  that path. `completedRounds` is the real predicate.
- **A floor is not a total.** Latching "we saw a cost" is not the same as "every
  round reported one": round 1 at $0.01 plus a silent round 2 published $0.01 as
  the run's cost. Completeness is tracked separately and poisoned permanently by
  any silent round, in either order.
- **Shedding a deadline must not shed the human.** The wrap-up round detaches from
  the run deadline on purpose. It re-linked the caller's cancel only when the run
  context was still live — so the one case that matters, "the deadline just fired
  and we are now spending another 90 seconds", was exactly the case where Escape
  did nothing. It also could not tell the caller's *own* deadline from ours, and
  would keep working after the caller gave up. The original caller context is now
  kept on the run and consulted directly.
- **A budget checked between batches is a tripwire, not a bound.** One round can
  request a dozen tools; a dozen near-cap results were appended in full before
  anything looked at `MaxTranscriptChars`. Each result is now clamped against the
  *remaining* allowance as well as the per-result cap — with a floor, because a
  dropped result leaves its tool call unmatched and the next request malformed.
- **`tool_choice: "none"` is a request, not a guarantee.** A provider that returns
  tool calls anyway would leave them unmatched in the terminal history; they are
  dropped from the pushed message and the fact is recorded in the transcript.
- **Prefix matching needs a word boundary.** `briefGist` rendered
  "Finding the relevant issue" as **"ing the relevant issue"** — "find" matched the
  start of a longer word. A corrupted label is worse than the boilerplate it was
  removing.

## Debugging a run

With `DAINTREE_ASSISTANT_DEBUG_LOG=1`:

```
subagent.start  subagentId=sub_02fc3119  maxRounds=10  toolCount=36
subagent.end    subagentId=sub_02fc3119  status=completed  rounds=3  toolCalls=3  durationMs=14214
```

The sub-agent's own tool calls appear as ordinary `tool.call` lines (they travel
the same registry, so they are audited like anything else). The full transcript
is in the `artifacts` table under the reported `transcriptId`.

## Lessons from the first live runs

Kept here because each one was a real defect the tests could not have found:

- **The report led with `I have everything needed.` / `## Report`.** Both lines
  are invisible to the sub-agent and land in the caller's context. Fixed in the
  persona prompt, with a conservative `stripReportPreamble` as backup — it strips
  only a heading that *leads* the report, because "preamble then heading" and
  "answer then heading" are structurally identical and deleting an answer costs
  far more than leaving a stray line.
- **The transcript recorded the final message twice**, once as the last round's
  prose and once under `## Report`.
- **A sub-agent read `internal/app/app.go` three times** with escalating
  `maxBytes` (3000 → 4000 → 20000), burning three rounds. Root cause was
  `fs.read`'s schema: `maxBytes` was documented as `"Max bytes to read."`, which
  gave no hint that the default is already 200,000 bytes, so the model rationed
  itself and then had to escalate. Fixed in the schema. **Known remaining gap:**
  `fs.read` has no `offset`, so a file larger than 200,000 bytes genuinely cannot
  be read past that point — the model's only recourse is a bigger `maxBytes`.
- **A prove-a-negative brief ("what enforces this rule?" — nothing does) burned
  the whole budget** hunting for a mechanism that did not exist. Fixed by telling
  the persona that a negative is a complete answer. That one brief went from 11
  rounds / 30 calls / 83s to 3 rounds / 4 calls / 11.5s.
