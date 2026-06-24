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
  - terminal.awaitAll
  - terminal.read
  - terminal.focus
  - queue.digest
  - queue.resolve
---
Use when: the user wants several agents to collaborate, compete, or cross-check on a single problem, and you are the conductor relaying between them.

The whole pattern is a loop: spawn a cohort → collect each one's output → relay the others' output back to each → ask for the next step → repeat → synthesize one answer. terminal.sendCommand is how you relay. It is always available (every turn, including an autonomous watcher wake) — call it by name, never hunt for it with tool.search.

Procedure:
1. Scope it in one pass. Decide the cohort (which agents — claude, codex, gemini, antigravity, … — resolve any user-spoken name against the roster; a mis-transcribed id launches a dead terminal) and the rounds (e.g. round 1: each answers; round 2: each sees the others and votes). Don't over-plan — the loop is the same regardless of round count.

2. Spawn the cohort in PARALLEL — one agentTask.spawnForEdits per agent in a single turn (batches of ~4), each with a SHORT, self-contained taskPrompt and a distinct `title` so the terminals stay distinguishable. Do NOT attach a watcher (`watch`/`watchGoal`): you are the conductor and you drive the rounds in-turn, so a background watcher here is a SECOND supervisor that fights your driving — the #1 way this task sprawls into minutes of churn and duplicate notifications. (If the user instead wants a fire-and-forget "wake me later" run, stop here, attach a watcher, end your turn, and react on the wake — but then do NOT also wait in-turn.)
   - Pick `mode` by what the agents actually do: `mode:"explore"` for thinking/answering/analysis/opinions that touch NO files (the read-only constraint is appended automatically and is task-neutral — it will not contaminate a non-codebase ask); `mode:"edit"` only when they must change files in a worktree.
   - Keep each prompt MINIMAL. State exactly the one thing you want and stop. Do not pad it with extra constraints, codebase instructions, or boilerplate — a bloated or self-contradictory prompt is a top way these tasks go wrong. If you want a fact off the top of its head, say only that.
   - Keep a terminalId→role map. Terminal titles can collide (two "Claude" tabs), so the terminalId each spawn returns is your only reliable handle for routing in step 3.

3. Wait for the WHOLE cohort with ONE terminal.awaitAll({ terminalIds: [t1, t2, t3] }), then read their outputs together. awaitAll polls every agent CONCURRENTLY and resolves only when each is confirmed finished (a small-model check on the tail, not a bare "waiting" — which an agent also shows parked at its prompt before it starts, paused mid-task, or when its window is backgrounded). A tidy findings-shaped block on screen is NOT proof an agent is done; trust awaitAll's status.
   - Do NOT issue one terminal.extract wait:{} per agent — that is the SINGLE-agent path and the calls run one after another (three 60s timeouts in a row). One awaitAll for the cohort replaces all of them.
   - Once awaitAll reports them finished, read every output in ONE terminal.extract over all the terminalIds with NO wait (pull the fact/vote you need), or a bounded terminal.read for a short verbatim answer. For garbled multi-screen TUI use terminal.summarize. Don't re-summarize or re-extract an extract result (a model read of a model read).
   - Handle the non-finished cases awaitAll reports: an agent ASKING A QUESTION → answer it with sendCommand (step 4) and await again; an agent that FAILED (nonzero exit) → drop it from the cohort and note it in your synthesis, do NOT respawn in a loop; an agent STILL WORKING when the budget ran out → await again (or read the ones that did finish and proceed). Never relay a half-done or dead screen.

4. Relay with terminal.sendCommand({ terminalId, command }). Send each agent ONLY what it needs from the OTHERS — their facts/drafts/votes — plus the one question for this round (e.g. "Here are the other two answers: A: … B: … Which is better and why?"). One sendCommand per agent, in parallel. Then collect again (step 3). Repeat the collect→relay loop for as many rounds as the task needs.

5. Synthesize and report ONE clean answer — the tally, the winner, the merged result — not a transcript of every terminal. Quote real terminalIds where useful.

6. Close out. Driving in-turn you attach no watchers, so there is usually nothing to cancel; just resolve any inbox item you did create with queue.resolve {"id":"<inbox id>"}. The cohort's terminals stay open and idle — leave them unless the user wants them closed; you can send another round to them anytime with terminal.sendCommand. When the user DOES ask you to close/clean up the terminals, retire the whole cohort in one confirmed call with terminal.close({ terminalIds: ["<id>","<id>",…] }) — don't tool.search for a close tool (terminal.close is always callable) and don't loop it once per id.

Anti-patterns to avoid (these are how this task fails):
- Attaching a watcher AND driving the rounds in-turn — pick one mode (this runbook is in-turn, so NO watcher). Two supervisors on the same terminals is the top cause of churn and duplicate notifications.
- Issuing one terminal.extract wait:{} per agent instead of ONE terminal.awaitAll for the cohort — the per-agent waits run one after another and stack their timeouts.
- Padding agent prompts, or appending instructions that contradict the ask. Short and clean wins.
- tool.search-ing for terminal.sendCommand or terminal.awaitAll. They are always callable — just call them.
- A `contains` wait for output the agent already produced (it times out). Wait on the cohort with terminal.awaitAll, which confirms genuine completion.
- Treating a bare "waiting"/idle, or a findings-shaped screen, as "finished" and relaying it mid-run. Idle ≠ done — trust awaitAll's confirmed status. This is a top way a round gets relayed early with half-baked output.
- Relaying raw terminal scrollback. Read a FINISHED agent with a batched extract (or summarize), then relay the gist.

Report back: the synthesized result (winner/tally/merged answer), which agents participated (real terminalIds), and any inbox cleanup you did.
