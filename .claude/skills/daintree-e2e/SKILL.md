---
name: daintree-e2e
description: Run the Daintree assistant end-to-end against a real query — a freshly-built CLI talking to a locally-run backend and the live Daintree MCP — then read the debug log, decide whether a failure is a CLI or backend problem, fix it in the right repo, and loop. Use for manual Daintree assistant improvement: reproducing a failure from a session log, testing a change against a real run, hunting the orchestration issues we keep hitting (plan-dumps, artifact-read loops, bad terminal ids, stalled turns), or driving skill-creation / self-improvement tests (run a task, have the system create or update a skill, then review what it produced and whether the run matched the intended plan). Accepts freeform input — a prompt to run, optionally a prior debug-log path plus a note on what went wrong. Covers rebuilding the CLI with make, starting/connecting to the backend, resetting the project, running multi-step, and managing state. Not for editing normal project files.
metadata:
  author: greg
  version: "1.0.0"
---

# Daintree assistant — end-to-end test & improve loop

The repeatable loop for **manually improving the Daintree assistant**: send a real query to a freshly-built CLI wired to a locally-run backend and the live Daintree MCP, read the full debug log, decide whether a failure is a **CLI** or **backend** problem, fix it in the right repo, rebuild/restart, and run again. Reach for this whenever we're reproducing a reported failure, testing a change against a real run, or hunting the class of orchestration issues we keep hitting.

All commands assume the standard checkout layout (override with the `DAINTREE_*_DIR` env vars the scripts read):
- CLI: `/Users/gpriday/Projects/Daintree/assistant` (this repo)
- Backend: `/Users/gpriday/Projects/Daintree/assistant-backend`
- Daintree project to run in: `/Users/gpriday/Projects/Daintree/daintree`

Bundled helpers live in `scripts/` — reference them by the path this SKILL.md is in.

## What the Daintree assistant is FOR (judge every run against this)

Daintree's local **operations officer** — a **direct, quick operator** that plans Daintree work, spawns/supervises visible agent terminals, and keeps the human's thread clean. It is **not** a deep reasoner and is **NOT a code editor** (real edits go to spawned agents). The goals to hold it to:

- **Workflow-first.** The ideal turn is a clean, minimal WORKFLOW: check state → spawn the cohort → wait → read → judge → act. Measured, one step at a time driven by *current* state — never a speculative dump of the whole plan.
- **Skills ARE the thinking.** Thinking mode is OFF by default (see the catalog — thinking-on made the model dump its entire plan as one giant batch). The runbook (skill) is the pre-computed plan; the model's job is to EXECUTE it step by step, not re-derive it.
- **Externalize the plan.** Because there is no hidden reasoning, the assistant should state briefly, in prose, what it is about to do — a one-line workflow statement — then do it. Clear, direct, focused on building the ideal workflow. A run where the model narrates a crisp plan and steps through it is *good*; silent flailing or a 60-call dump is *bad*.
- **Precision over cleverness.** The failures are mechanical (bad ids, wrong arg shapes, futile loops), so reliability is an **engineering** problem: make the wrong shape impossible, and make sure the right skill loads.

### Know the plan before you run

A run doesn't just have to *complete* — it has to go **according to plan**. So decide the plan FIRST:
- Before each run, write down the **expected workflow** — the ideal step sequence for this task (e.g. `snapshot → scratch.create → END; next turn: spawn cohort in one batch → END; next: awaitAll → extract.json → score → …`). This is your yardstick.
- The assistant should **externalize its own plan** in prose (thinking is off, so its plan must be visible). A good run states a crisp workflow and steps through it; judge its stated plan AND its actual tool sequence against your expected workflow.
- "Flawless" = it followed the intended workflow (measured, one step per turn, no dump, no bad-id storm, no stall) **and**, for a skill-creation test, produced a good skill/update. Completion alone is not success.
- Most runs happen **in the Daintree project itself** (`/Users/gpriday/Projects/Daintree/daintree`) around a real workflow — spawning agents, supervising, orchestrating. Pick the workflow the test is meant to exercise and hold the run to it.

## Two repos — know where a fix lands

| Symptom | Repo | What to edit | Gate |
|---|---|---|---|
| Wrong tool **argument shape**, dispatch/turn loop, circuit breaker, cockpit UI, `/clear` & state, MCP client | **CLI** `../assistant` (Go) | `internal/…` | `go test ./...`, `go vet ./...`, `gofmt -l internal/` |
| Model **behavior / wording / plan / which skill loads / thinking / model choice** | **Backend** `../assistant-backend` (Python) | `src/daintree_assistant_server/prompts/**`, `.../skills/files/*.md`, `config.py` | `.venv/bin/python -m pytest tests/ -q` |

Rule of thumb: **behavior/wording/plan/skill-selection = backend; tool argument shape / dispatch / UI / state = CLI.** Prompt/skill fixes land in the backend; local tool-shape fixes land in the CLI. Prefer making the correct shape impossible to get wrong over lenient parsing.

## Inputs (freeform)

You'll get some mix of: a **prompt** to run, a **prior debug-log path** to reproduce/understand, and a **note** on what went wrong.
- Given a **log** → `scripts/analyze-log.sh <log>` FIRST, then read the interesting turns before touching anything.
- Given only a **prompt** → run it fresh (loop below). The recurring stress test is the multi-agent "pair-programmer" orchestration (spawn Grok/Codex/Antigravity, interview, pick a winner, close losers) — it exercises the whole spawn→await→read→judge→close workflow.
- Given only a **complaint** → find the newest log (`ls -t ~/.daintree/logs/*.log | head -1`) and analyze it.

## The loop

Run each step as its own Bash call. Long-running steps (backend, the query) go in a **background** Bash so you can monitor.

**1 — Rebuild the CLI to the newest code.** Always test the change you just made:
```
cd ../assistant && make build      # → ./bin/daintree-assistant, version-stamped
```

**2 — Start the backend (background).** `LEARN=off` (default) freezes the skill catalog so it can't mutate mid-iteration — use it while debugging behavior. For a **skill-creation test**, use `LEARN=propose` (writes proposals, changes nothing) or `LEARN=apply` (writes the run's lessons into the real skill files):
```
scripts/start-backend.sh                 # BACKGROUND; serves on :8473, learning off
LEARN=apply scripts/start-backend.sh     # self-improvement ON — review with skill-diff.sh
```
Poll readiness before running: `curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8473/readyz` must be `200`. To A/B thinking mode, prefix `THINKING=on|off` (default is the config default, currently off).

**3 — Get a fresh MCP token.** Daintree mints per-session tokens that **expire in ~12 minutes**. The scripts read `DAINTREE_MCP_URL` / `DAINTREE_MCP_TOKEN` from the environment and **nowhere else**.

> **They are not in the debug log, and must never be put back there.** The runtime deleted its `mcp.credentials` log line deliberately: the token authorises system-tier Daintree actions for its whole validity window and a log file outlives it. A script that scrapes logs for credentials is broken on every current build *and* teaches the next reader to look for secrets in logs — which is the pressure that gets the unsafe line reinstated. Don't add a fallback.

Take them from the RUNNING assistant's own environment while it still holds them:
```
pgrep -f daintree-assistant                              # find the Daintree-launched process
eval "$(ps eww -o command= -p <pid> | tr ' ' '\n' \
        | grep -E '^DAINTREE_MCP_(URL|TOKEN)=' | sed 's/^/export /')"
python3 scripts/mcp.py terminal.list '{}'                # verify it is live
```
`Unauthorized` / HTTP 401 → the token is stale: ask the user to open a Daintree session, then re-export from that process and retry.

**4 — Reset the project** (close leftover terminals from a prior run):
```
python3 scripts/mcp.py close-all
```

**5 — Run the query (background).** Builds, sets env, runs one-shot `--json` with the full debug trace, cwd = the Daintree project:
```
scripts/run.sh "the prompt"        # or: scripts/run.sh -f /path/to/prompt.txt
```
It spawns agents and runs multiple rounds (minutes). The CLI prints `logging to <path>` early; the log is also just `ls -t ~/.daintree/logs/*.log | head -1`.

**6 — Watch + analyze.** While it runs and after it ends:
```
scripts/analyze-log.sh             # newest log, or pass a path
```
Read the specific bad turns raw (grep by `runId`/`round`). See the cheat-sheet below.

**7 — Diagnose: CLI or backend?** (next section.)

**8 — Fix → verify → loop.** Edit the right repo; run its gate (`go test`/`pytest`); **rebuild the CLI (step 1) or restart the backend (kill + step 2)** so the run uses the change; reset terminals (step 4); run again (step 5). Repeat until the workflow is clean.

## Self-improvement & skill creation

Daintree is meant to **self-improve** — the backend's skill-learning harness watches sessions and creates or improves skill files, so the system gets better at its own workflows over time. A major class of E2E test is exactly this: **"work through this task, and create/update a skill from it."** Verifying that loop end-to-end is core to what we do here.

To run one:
1. Start the backend with learning on: `LEARN=propose` (safe dry-run) or `LEARN=apply` (writes real skill files). Keep thinking off.
2. Run the task the skill should capture — a real Daintree workflow in the Daintree project (spawn/supervise/orchestrate). The assistant does the work; the learner distills a skill from it.
3. Inspect what it produced: `scripts/skill-diff.sh` shows the uncommitted skill/prompt changes plus the audit trail (`.daintree/skill-learning` — proposals, gate decisions, confidence).
4. **Review the skill hard**, the same way we review any skill:
   - **Grounded in real Daintree behavior** — the tools/wrappers/MCP it names actually exist, not invented?
   - **A clean workflow runbook** — numbered steps, the right tools, one step per turn, ids-come-back-next-turn discipline — and does it match the intended plan?
   - **Correctly scoped** — a `whenToUse` that loads it at the right time; does it duplicate or contradict an existing skill?
   - Did the RUN itself go according to plan? A skill distilled from a *messy* run bakes the bad workflow in — fix the run first, then re-learn.
5. **Grounding = search both repos.** To check a proposed skill against reality, search the backend for the real skill/prompt surfaces (`grep -r … src/daintree_assistant_server/skills/files` and `.../prompts`) and the CLI for the real tool shapes (`internal/tools/**`). The self-improving system leans on those surfaces; so should your review.
6. **Keep or revert.** A good skill/update → `git add` it in the backend; a bad one → `git checkout -- <file>`. Set `LEARN=off` for the next debugging iteration so nothing mutates while you fix code.

## Diagnose: CLI or backend?

- **`SkillLoaded = 0`** → the runbook never loaded; the model has no plan. Almost always **backend** (skill selector) or the CLI `/clear` state-token bug (a `/clear` before the query that leaves a stale backend `state` token — that was a real CLI bug). Check: did the user `/clear`? Did the selector run?
- **Round-0 (or any round) with a huge `toolCallCount`** (dozens of calls: spawn+read+await+close at once, against ids that don't exist yet) → **plan-dump**. Backend/model behavior — thinking mode on, or a skill/prompt that invites one-shot planning. Fix: keep thinking off; strengthen the "one step per turn, ids come back NEXT turn" rule in the base prompt / orchestration skill.
- **A tool failing the same way many times** (`artifact.read ARTIFACT_NOT_FOUND`, `Terminal not found`) → a futile loop or a bad-id storm. The CLI circuit breaker should abort it; if it didn't, that's a **CLI** breaker gap. The *cause* (why the model does it) is usually **backend** prompt/skill guidance.
- **Truncated / wrong terminal ids** ("still working forever", "Terminal not found") → model shortened a `terminal-<uuid>`. **Backend** id-discipline guidance + the CLI terminal-id resolver.
- **`backend.respond.error` 400 / connect** → CLI↔backend contract mismatch (e.g. an over-long runtime field) or the backend is down. **CLI** (respect the contract / clamp) or just start the backend.
- **Model stops with no tool call, brief content** → under-acting: no skill (see above), or the prompt let it end early. **Backend**.

## Log analysis cheat-sheet

`~/.daintree/logs/<date>-<sessionId>.log` is structured text — grep it, don't eyeball it. Every turn line carries `runId`/`turnId`/`round`.
- `turn.start` / `turn.end` — per-turn bracket; `status=failed|cancelled` + `rounds` + `replyPreview` is the fastest "which turn broke".
- `backend.respond.request` / `.meta` / `.done` / `.error` — what the backend was SHOWN, its report (model + **skill-selection outcome**), what it PRODUCED (`toolCallCount`, `finishReason`, content preview), and errors. `.meta` is the surface that tells you whether a fix belongs in the **backend selector**.
- `tool.call … ok=false` — every rejected/failed tool call with post-decode `args:` + `error:` (code + message). Highest-signal for mechanical mistakes.
- `mcp.call` — the raw MCP layer (throttles, transport blips) where many "tool failures" actually originate.
- Skill loads surface as `SkillLoaded` events (also the cockpit "Skill loaded" line). `0` is a red flag.
- Full-fidelity replay: `scripts/mcp.py` hits the SAME MCP by hand, using the url+token you exported from the live process (step 3). The log deliberately does **not** contain them.

## Known failure catalog (recognize these)

Patterns we've hit and where each was fixed — check whether a run is repeating one before diagnosing fresh:
- **Plan-dump** — model emits its whole multi-round plan as one 60+-call batch → most fail. Cause: **thinking mode ON**. Fix: `MAIN_THINKING_ENABLED=false` (backend default) + "one step per turn" rule.
- **Artifact-read loop** — model hunts a pruned artifact, paging `offset` 0/3500/7000… (`ARTIFACT_NOT_FOUND`, unrecoverable). Fixes: CLI **coarse, pagination-insensitive, mid-batch** circuit breaker; backend guidance "never retry an unrecoverable error / read a cohort via terminal.extract, not artifact.read / the extract result you got IS the answer".
- **`/clear` didn't reset the backend state token** → skill never re-loads after clear → model stalls. Fix: CLI `clearLocked` drops `s.backendState`.
- **Over-long runtime field 400** — a disconnected MCP sent a >64-char status → backend 400 killed the turn. Fix: CLI clamps `rc.MCP.Status` to 64.
- **Truncated terminal ids / worktreeId-is-a-path** — model shortens a `terminal-<uuid>` or passes a branch name where an id is wanted → "not found" storms. Fixes: CLI resolvers + backend id-discipline.
- **MCP getOutput throttle** — bursty reads get rate-limited; parallel client-side dispatch bursts it (a NON-STARTER — the win is fewer calls / server-side fan-out, not client goroutines).

## State & hygiene

- **Kill the test backend when done** so it can't conflict with the user's own: `pkill -f daintree_assistant_server`. The user usually stops theirs so you can run yours — don't leave a stray on `:8473`.
- **Reset terminals between runs** (`mcp.py close-all`) so each run starts from a clean project.
- **Token expiry** is the #1 friction — it dies ~12 min after minting. Re-export it from the live assistant process right before a run; on 401, ask the user to open a fresh Daintree session and export again. Never cache it to a file.
- **Rebuild/restart before you trust a change**: `make build` for CLI edits; kill+restart the backend for prompt/config edits. The version stamp (`daintree-assistant --version`) confirms which binary you're running.
- Never edit normal project files here — this skill spawns/reads, and fixes land as targeted CLI/backend edits.

## Scripts

- `scripts/mcp.py` — stdlib MCP client, env-only credentials. `mcp.py <tool> '<json>'` calls any tool; `mcp.py close-all` resets; `mcp.py creds` echoes back the url+token it resolved from the environment (it never discovers them).
- `scripts/start-backend.sh` — start the backend. `LEARN=off|propose|apply` (skill-learning), `THINKING=on|off` (A/B). Run in background; kill with `pkill -f daintree_assistant_server`.
- `scripts/run.sh` — build the CLI + run ONE query end-to-end (`"prompt"` or `-f file`). Run in background; prints the new log path.
- `scripts/analyze-log.sh` — diagnostic snapshot of a debug log (newest by default).
- `scripts/skill-diff.sh` — after a `LEARN=propose|apply` run, review what skill-learning created/updated (skill diff + audit trail).
