---
id: daintree.orchestration.multi-agent
title: Orchestrate multiple agents on one problem
version: 0.1.0
summary: Spawn several agents, relay their work between them with terminal.sendCommand, and synthesize one answer — collaborative or competitive multi-agent tasks.
whenToUse: Use when the user wants several agents to work one problem TOGETHER (or against each other) — collaborate, cross-check, debate, vote, or combine outputs — and you must coordinate the back-and-forth between the terminals.
priority: 95
risk: project
tags:
  - daintree
  - orchestration
  - multi-agent
  - agents
  - collaborate
  - relay
  - vote
  - debate
  - fan-out
  - sendCommand
requiredTools:
  - context.snapshot
  - agentTask.spawnForEdits
  - agentTask.status
  - agentTask.list
  - terminal.sendCommand
  - terminal.summarize
  - terminal.extract
  - terminal.read
  - terminal.focus
  - watcher.terminal.create
  - watcher.list
  - watcher.cancel
  - queue.digest
  - queue.resolve
  - timer.schedule
---
Use when: the user wants several agents to collaborate, compete, or cross-check on a single problem, and you are the conductor relaying between them.

The whole pattern is a loop: spawn a cohort → collect each one's output → relay the others' output back to each → ask for the next step → repeat → synthesize one answer. terminal.sendCommand is how you relay. It is always available (every turn, including an autonomous watcher wake) — call it by name, never hunt for it with tool.search.

Procedure:
1. Scope it in one pass. Decide the cohort (which agents — claude, codex, gemini, antigravity, … — resolve any user-spoken name against the roster; a mis-transcribed id launches a dead terminal) and the rounds (e.g. round 1: each answers; round 2: each sees the others and votes). Don't over-plan — the loop is the same regardless of round count.

2. Spawn the cohort in PARALLEL — one agentTask.spawnForEdits per agent in a single turn (batches of ~4), each with a SHORT, self-contained taskPrompt and a distinct `title` so the terminals stay distinguishable. Attach a watcher to each (`watch: true`, `watchGoal: "...wait for the agent to FINISH its turn, then surface the answer"` — not the instant it goes quiet).
   - Pick `mode` by what the agents actually do: `mode:"explore"` for thinking/answering/analysis/opinions that touch NO files (the read-only constraint is appended automatically and is task-neutral — it will not contaminate a non-codebase ask); `mode:"edit"` only when they must change files in a worktree.
   - Keep each prompt MINIMAL. State exactly the one thing you want and stop. Do not pad it with extra constraints, codebase instructions, or boilerplate — a bloated or self-contradictory prompt is the #1 way these tasks go wrong. If you want a fact off the top of its head, say only that.

3. Collect each agent's output once it has actually FINISHED its turn — NOT the instant it looks idle. A bare "waiting"/idle reading is an unreliable proxy: an agent reads "waiting" parked at its prompt before it starts, paused mid-task, or when its window is backgrounded (e.g. you switched to another project — that flips the cohort to "waiting"). A tidy findings-shaped block on the screen mid-run is NOT proof it is done. You do NOT need to end your turn and wait for a watcher wake — drive the rounds in-turn, but gate on CONFIRMED completion:
   - To block until an agent genuinely finishes, run a settle wait-extract PER TERMINAL: terminal.extract({ terminalIds: ["<one id>"], instruction, wait: {} }). `wait: {}` now prefers a real working→waiting transition (or a stable idle past a short grace if one was never seen) AND always confirms with a small-model check on the tail before it resolves, so it will NOT hand you a half-done screen; it resolves a SINGLE terminal, so issue one per agent (in parallel) — do NOT put the whole cohort in one waited call. (An explicit `wait: {"stateIs":"waiting"}` is NOT equivalent — it matches the raw state with no confirmation; prefer `wait: {}`.) If it TIMES OUT, the agent is likely not done — wait for its watcher's completed_* event rather than hammering the same wait-extract; never relay the partial screen. Never use a `contains` wait for text that may already have printed (it just times out and burns ~60s).
   - Once an agent's watcher has published a "completed_*" event (which encodes the confirmed completion), read it immediately — you CAN batch several confirmed-done terminals into one terminal.extract with multiple terminalIds and NO wait. Do NOT short-circuit on a raw "waiting" you read yourself off getStatus.
   - terminal.summarize({ terminalId }) gives a clean gist of a FINISHED agent (DEFAULT — agent scrollback is garbled TUI noise; never relay raw scrollback). Don't re-summarize or re-extract an extract result (a model read of a model read); read the agent's finished state from getStatus or the watcher event, not by re-modeling already-extracted text.

4. Relay with terminal.sendCommand({ terminalId, command }). Send each agent ONLY what it needs from the OTHERS — their facts/drafts/votes — plus the one question for this round (e.g. "Here are the other two answers: A: … B: … Which is better and why?"). One sendCommand per agent, in parallel. Then collect again (step 3). Repeat the collect→relay loop for as many rounds as the task needs.

5. Synthesize and report ONE clean answer — the tally, the winner, the merged result — not a transcript of every terminal. Quote real terminalIds where useful.

6. Close out. Resolve handled inbox items with queue.resolve {"id":"<inbox id>"}; cancel any watcher you no longer need with watcher.cancel {"id":"<watcherId>"} and say so. The cohort's terminals stay open and idle — leave them unless the user wants them closed; you can send another round to them anytime with terminal.sendCommand. When the user DOES ask you to close/clean up the terminals, retire the whole cohort in one confirmed call with terminal.close({ terminalIds: ["<id>","<id>",…] }) — don't tool.search for a close tool (terminal.close is always callable) and don't loop it once per id.

Anti-patterns to avoid (these are how this task fails):
- Padding agent prompts, or appending instructions that contradict the ask. Short and clean wins.
- tool.search-ing for terminal.sendCommand. It is always callable — just call it.
- A `contains` wait for output the agent already produced (it times out). Use a settle `wait: {}` (confirmed-finished), or read immediately only once a watcher completed_* event has confirmed it.
- Treating a bare "waiting"/idle, or a findings-shaped screen, as "finished" and relaying it mid-run. Idle ≠ done — gate on a confirmed `wait: {}` or a watcher completed_* event. This is the #1 way a round gets relayed early with half-baked output.
- Relaying raw terminal scrollback. Summarize a FINISHED agent, then relay the gist.

Report back: the synthesized result (winner/tally/merged answer), which agents participated (real terminalIds), and any watcher/inbox cleanup you did.
