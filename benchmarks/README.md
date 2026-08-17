# Benchmarks

End-to-end benchmarks for the Daintree assistant. Unlike `go test ./...` these
make **real, paid backend/model calls** and are run deliberately, not in CI.

| Suite | What it measures |
| --- | --- |
| [`orchestration/`](orchestration/) | Does the assistant get real operator tasks to their final result against a scripted fake Daintree — and how long / how many rounds / how many tokens does it take? |

The backend's own prompt-level benchmark (skill selection) lives in
`../assistant-backend/benchmarks/`.

## Orchestration benchmark

```
benchmarks/orchestration/
  main.go        runner CLI (flags: -filter -trials -parallel -backend -bin -list -out)
  world/         the fake Daintree: a stateful MCP server with scripted agents
  scenario/      the case suite + objective pass predicates
  runner/        per-trial process orchestration, JSONL + debug-log metrics
  results/       saved run documents (gitignored)
```

### What's real, what's fake

- **Real:** the CLI binary (built from source), the local backend at
  `127.0.0.1:8473`, and every orchestrator model turn — the decisions under
  test. Utility-model calls (finish judge, extraction) are real too.
- **Fake:** all of Daintree. `world/` serves the Daintree MCP surface over the
  same go-sdk Streamable HTTP transport the live app uses, with **scripted
  agents**: a spawned "agent" is a timeline (working → output dribbles →
  waiting/exited), a pure function of elapsed time — **zero tokens are spent on
  the agents being supervised.**

### How grading works

A scenario passes when the orchestrator **reached the result**, graded against
ground truth it cannot fake:

1. **The world's call log** — the right effects happened (`agent.launch` in the
   right worktree, input sent to the right terminal, nothing closed uninvited).
2. **Nonce pass-through** — agent output contains planted tokens
   (`PATCH_ID=9f3a2c`); the final answer must carry them. A value that exists
   only inside the fake world can't be hallucinated: it proves the assistant
   actually read the terminal, end to end. Prompts are phrased so the nonce is
   load-bearing in the user's ask ("give me the patch id it prints") — we grade
   information flow, not summarization taste.
3. **Structural stream facts** — turn ended in a success envelope, no timeout,
   bounded wall-clock where the scenario demands it.

We deliberately do **not** pin the route (which tools, in which order) beyond
the minimum the task defines — the check is "did it get there", not "did it go
the way we imagined".

### Running

```bash
# backend must be live:
#   cd ../assistant-backend && python -m daintree_assistant_server

go run ./benchmarks/orchestration                    # full suite
go run ./benchmarks/orchestration -list              # list scenarios
go run ./benchmarks/orchestration -filter spawn      # subset by id/category substring
go run ./benchmarks/orchestration -trials 3          # pass-rate over N trials
go run ./benchmarks/orchestration -parallel 4        # concurrent trials
go run ./benchmarks/orchestration -bin ./bin/daintree-assistant   # skip rebuild
```

Categories: `status`, `extract`, `spawn`, `interact`, `fault`, `latency`. The fault
scenarios reproduce Daintree quirks that caused real incidents (blank-padded
status tails, rate-limited reads, never-finishing agents, sub-2s finishers).

### The latency suite (response speed)

The `latency` scenarios exist for their **RoundDetail metrics**, not their checks:
cheap, fast turns whose per-round decomposition is the benchmark. Every trial
reconstructs the turn timeline from the debug log and reports, per model round:

- `gapBeforeMs` — prior round's done → this request (tool execution + CLI bookkeeping)
- `rawMetaMs` — request → the SSE meta arriving at the client (selector + backend
  pre-stream work)
- `skillCueMs` — request → the eager, de-duplicated skill-loaded event reaching the
  output sinks (absent when the round loads no new skill). Nothing renders it to the
  user; the mark exists to separate SELECTION latency from generation latency
- `committedMetaMs` — request → retry-safe metadata/state adoption; this normally
  coincides with first content, or with successful completion on a tool-call-only round
- `firstTokenMs` — request → first visible content delta (absent on tool-call-only rounds)
- prompt/cached tokens — the round's prompt-cache hit rate

Run it serially (parallel trials contend and skew latency):

```bash
go run ./benchmarks/orchestration -filter latency -trials 4 -parallel 1
```

The results JSON carries the same fields (`roundDetail`, `firstRawMetaMs`,
`firstSkillCueMs`, `firstContentMs`, `turnMs`) — diff two runs to see what a change did
to response speed. The backend logs the matching server-side split per request (`selector_ms`,
`pre_upstream_ms`, `respond_upstream_open.upstream_first_event_ms`).

**Cache forensics:** set `DAINTREE_DUMP_UPSTREAM_DIR=<dir>` on the backend to
dump every upstream request body as numbered JSON, then diff two dumps to find
the exact divergence byte. This is how the 2026-07-08 cache-busting layout bug
was found (volatile system-role runtime context serialized before the tool
schemas — 36% cache hit on byte-identical turns; 99% after the fix). The rule it
established: **only stable content may ride a system-role message** — the DeepSeek
route serializes [all system messages] → [tools] → [conversation] regardless of array
order. That measurement was taken against `deepseek/deepseek-v4-flash-0731` **through
OpenRouter**; it is a property of that route, not of OpenRouter generally, so re-measure
before assuming it transfers to another model the backend may select. The raw-meta /
skill-cue / committed-meta / first-token split is also the
measurement surface for prior-skill speculation: it shows separately when selection
finishes, when the user sees the capability cue, and when kept generation becomes visible.

Each trial is fully isolated: its own fake world, its own
`DAINTREE_ASSISTANT_STATE_DIR`, its own debug log, its own empty CWD. One-shot
mode never spawns a daemon, so no processes are left behind — the temp workdir
(per-trial state + debug logs, printed at start) and the saved results JSON are
deliberately retained for post-mortems. Scenarios run in real time (settle
graces and poll cadences don't scale down) — a full serial run is ~15–20 min;
use `-parallel`.

### Reading results

Console: per-trial PASS/FAIL + failed-check detail + a summary table
(duration, model rounds, tool calls, tokens). JSON: written to `results/` with
every check outcome and metric — diff two runs to see what a prompt change did.
Each result row links the trial's debug log for post-mortems (`backend.respond.*`,
`tool.call`, `mcp.call` events).

### The improvement loop

1. Run the suite → a scenario fails or a metric regresses.
2. Read the trial's debug log — find where the model misjudged/misused a tool.
3. Fix the **system**: backend prompt/skill (`../assistant-backend`) or local
   tool shape (this repo).
4. Re-run the filter for that scenario, then the full suite.
5. New real-world incident? Add it as a scenario — the log-archaeology loop
   ends in a permanent regression case instead of a hope.

### Adding a scenario

One function in `scenario/scenarios.go` (~20 lines): a prompt, a world setup
(pre-seeded terminals and/or a `SetSpawnScript` for launched agents, fault
flags), and checks. Plant nonces in agent output and make the prompt ask for
them. Register it in `All()`.

### Known-red findings (real product issues, not harness bugs)

- **`hung-agent-no-stall`** (first full run, 2026-07-07): the model's first
  `awaitAll` used a ~240s budget, then re-awaited "with a longer budget" and
  blew the 6-minute scenario bound. Root cause is guidance, not the model: the
  backend prompt/skills cap the re-await **count** (max 2) but never the
  **total in-turn wait**, and `daintree.edits.spawn-visible-agent.md` +
  `daintree.orchestration.*.md` explicitly sanction `maxAttempts: 240` (~480s)
  per await — the letter of the rules allows ~20 min of blocking on a hung
  agent, contradicting the base prompt's own "in-turn waits are for well under
  a minute or two; prefer async beyond that". Fix belongs in
  `../assistant-backend`: keep the FIRST await at the default budget and cap
  total in-turn blocking per terminal (~3–4 min) before taking the defined
  exit (async handoff / proceed-without / blocked publish). Deferred because
  that file was mid-edit on a parallel backend branch at the time.

### Caveats

- **Trust the call log + nonces, not vibes.** If a check keeps failing, first
  ask whether the ask is fair (would a human include that fact?), then whether
  the world is faithful, and only then blame the model.
- Watcher/timer/wake scenarios need the scheduler, which one-shot deliberately
  never starts — background-supervision scenarios grade "spawned + didn't
  block + honest answer" instead. A daemon-mode harness is a future phase.
- Model behaviour is nondeterministic: for decisions, run `-trials 3` and read
  pass-rates; single trials are for smoke checks and debugging.
