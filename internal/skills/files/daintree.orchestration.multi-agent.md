---
id: daintree.orchestration.multi-agent
title: Orchestrate multiple agents on one problem
version: 0.1.1
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
  - terminal.extract.json
  - terminal.awaitAll
  - terminal.read
  - terminal.focus
  - queue.digest
  - queue.resolve
  - scratch.create
  - scratch.set
  - scratch.get
---
Use when: the user wants several agents to collaborate, compete, or cross-check on a single problem, and you are the conductor relaying between them.

The whole pattern is a loop: spawn a cohort → collect each one's output → relay the others' output back to each → ask for the next step → repeat → synthesize one answer. terminal.sendCommand is how you relay. It is always available (every turn, including an autonomous watcher wake) — call it by name, never hunt for it with tool.search.

Procedure:
1. Scope it in one pass. Decide the cohort (which agents — claude, codex, gemini, antigravity, … — resolve any user-spoken name against the roster; a mis-transcribed id launches a dead terminal) and the rounds (e.g. round 1: each answers; round 2: each sees the others and votes). Don't over-plan — the loop is the same regardless of round count.
   - Create ONE scratch store up front as your compaction-safe state carrier: `scratch.create({"name":"multi-agent-orchestration"})`, and hold the returned `storeId` (an `scr_…` handle — there is NO way to list stores, so this handle is the only way back to it). Across the whole run you keep the evolving state — the terminalId→role map, the running scores, the round counter, which agent is chronically slow — in ONE key (`orchestration_state`) that you OVERWRITE each round (never a key per round). WHY: a long enough run crosses the auto-compact threshold, and compaction replaces the transcript with a lossy summary that silently drops state living only in prose — leaving the Director quietly unsure who is who. The scratch store lives OUTSIDE the transcript, so it survives compaction. (It is session-only: it does NOT survive a process restart — if the assistant restarts mid-run, re-scope from scratch.)

2. Spawn the cohort in ONE BATCH — one agentTask.spawnForEdits per agent, all emitted together in a single model turn (groups of ~4 per turn), each with a SHORT, self-contained taskPrompt and a distinct `title` so the terminals stay distinguishable. (Batched calls dispatch one after another in emission order, not concurrently — the win is the single model turn, not simultaneous execution.) Do NOT attach a watcher (`watch`/`watchGoal`): you are the conductor and you drive the rounds in-turn, so a background watcher here is a SECOND supervisor that fights your driving — the #1 way this task sprawls into minutes of churn and duplicate notifications. (If the user instead wants a fire-and-forget "wake me later" run, stop here, attach a watcher, end your turn, and react on the wake — but then do NOT also wait in-turn.)
   - Pick `mode` by what the agents actually do: `mode:"explore"` for thinking/answering/analysis/opinions that touch NO files (the read-only constraint is appended automatically and is task-neutral — it will not contaminate a non-codebase ask); `mode:"edit"` only when they must change files in a worktree.
   - Keep each prompt MINIMAL. State exactly the one thing you want and stop. Do not pad it with extra constraints, codebase instructions, or boilerplate — a bloated or self-contradictory prompt is a top way these tasks go wrong. If you want a fact off the top of its head, say only that.
   - Keep a terminalId→role map. Terminal titles can collide (two "Claude" tabs), so the terminalId each spawn returns is your only reliable handle for routing in step 3.
   - Once every spawn has returned, write the initial state to scratch BEFORE the first round, so a compaction can never strand it: `scratch.set({"storeId":"scr_…","key":"orchestration_state","value":{"round":1,"terminalRoles":{"term_x":"claude/affirmative","term_y":"codex/negative"},"activeTerminalIds":["term_x","term_y"],"droppedTerminalIds":[],"scores":{"affirmative":0,"negative":0},"slowCounts":{}}})`. The `value` is a raw JSON object — pass it directly, do NOT wrap it in a string.

3. Re-anchor from scratch, then wait for the WHOLE cohort. At the TOP of every round, FIRST `scratch.get({"storeId":"scr_…","key":"orchestration_state"})` and restore `terminalRoles`, `activeTerminalIds`, `scores`, `slowCounts`, and `round` from it — after a compaction the transcript may no longer hold them, so the scratch store is your source of truth for who is who and what the score is. Then wait with ONE terminal.awaitAll({ terminalIds: [t1, t2, t3] }), then read their outputs together. awaitAll polls every agent's agentState CONCURRENTLY — no model call, no output read — and resolves when each has gone working→idle (or completed/exited). It is fast and light, but agentState is an IMPERFECT signal: an agent can momentarily read "waiting"/idle while still mid-work (parked before it starts, paused mid-task, or backgrounded). So awaitAll's "finished" is a strong hint, NOT proof.
   - Do NOT issue one terminal.extract wait:{} per agent — that is the SINGLE-agent path and the calls run one after another (three 60s timeouts in a row). One awaitAll for the cohort replaces all of them.
   - VERIFY + SELF-HEAL after awaitAll reports finished: read every output together in ONE terminal.extract over all the terminalIds with NO wait (pull the fact/vote you need as plain text), or a bounded terminal.read for a short verbatim answer. As you read, sanity-check each — if a terminal reported "finished" but its last few lines show it is still working (a half-finished message, a live spinner, no real answer yet), do NOT relay it: re-await just that one, or set a watcher on it and poll, or send a visible agent to look closer. Heal the misread before you synthesize. For garbled multi-screen TUI use terminal.summarize. Don't re-summarize or re-extract an extract result (a model read of a model read).
   - Reading a vote/answer to RELAY or eyeball is a plain-text job — use terminal.extract (no schema, no "format" arg). Only when you want the whole cohort's votes as ONE structured object to tally programmatically, use terminal.extract.json with an `instruction` AND a `jsonSchema` (e.g. jsonSchema `{ "votes": [ { "player": "string", "vote": "yes|no" } ] }`). Don't reach for the json tool just to read one value.
   - Handle the non-finished cases awaitAll reports: the result names them at the top level as `stillWorking` and `askingQuestion` ID arrays, so you can target the stragglers without scanning perTerminal. An agent ASKING A QUESTION → answer the `askingQuestion` ids with sendCommand (step 4) and await again; an agent that FAILED (nonzero exit) → drop it from the cohort and note it in your synthesis, do NOT respawn in a loop; agents STILL WORKING when the budget ran out → re-await exactly the `stillWorking` ids (not the whole cohort), or read the ones that did finish and proceed. Never relay a half-done or dead screen. Size the await budget UP FRONT, not reactively after a cap: start round 1 of a multi-round cohort with `maxAttempts:60` (≈ 120s) — the default 30 (≈ 60s) only fits a fast, single-shot cohort. For a cohort with a known-slow agent, raise `maxAttempts` (max 240 ≈ 480s) on the await call rather than re-awaiting in a loop.
   - Remember the straggler ACROSS rounds. Track which terminalId is the lone `stillWorking` entry each round; if the SAME terminal is the only straggler in two consecutive rounds, stop awaiting it inside the cohort. Read the fast agents immediately with a no-wait terminal.extract (they are already idle), then await ONLY the chronic straggler in its own call with a dedicated higher budget (`maxAttempts:120`–`240`). Mechanical reason: a batch dispatches in emission order, so a cohort awaitAll that still includes the slow terminal blocks every read queued behind it — isolating the slow wait lets the fast output flow now instead of at the slow agent's pace.

4. Relay with terminal.sendCommand({ terminalId, command }). Send each agent ONLY what it needs from the OTHERS — their facts/drafts/votes — plus the one question for this round (e.g. "Here are the other two answers: A: … B: … Which is better and why?"). One sendCommand per agent, all in ONE BATCH. Emit the round's terminal.awaitAll as the LAST call in that SAME batch (right after the sendCommands) — dispatch is sequential in emission order, so every send completes and then the await begins, all in one model turn. Issuing the awaitAll in a SEPARATE turn after the sends wastes a full model round-trip every relay cycle. (Exception: once you are isolating a chronic straggler (step 3), the fast agents are already idle — read them straight away with a no-wait extract and give the straggler its OWN dedicated await; don't bundle a cohort awaitAll into the relay batch for agents that already finished.) Then collect again (step 3). Repeat the collect→relay loop for as many rounds as the task needs.
   - Before you close each round, OVERWRITE the checkpoint with the updated state: `scratch.set` the SAME `orchestration_state` key with the new `round`, `scores`, `slowCounts`, `droppedTerminalIds`, and any gist you'll need next round. Overwriting one key (not a key per round) keeps you under the 100-key / 10 000-rune-per-value caps and makes the read-back in step 3 a single get. Store concise gists, never raw scrollback.

5. Synthesize and report ONE clean answer — the tally, the winner, the merged result — not a transcript of every terminal. Quote real terminalIds where useful.

6. Close out. Driving in-turn you attach no watchers, so there is usually nothing to cancel; just resolve any inbox item you did create with queue.resolve {"id":"<inbox id>"}. The cohort's terminals stay open and idle — leave them unless the user wants them closed; you can send another round to them anytime with terminal.sendCommand. When the user DOES ask you to close/clean up the terminals, retire the whole cohort in one confirmed call with terminal.close({ terminalIds: ["<id>","<id>",…] }) — don't tool.search for a close tool (terminal.close is always callable) and don't loop it once per id.

Anti-patterns to avoid (these are how this task fails):
- Attaching a watcher AND driving the rounds in-turn — pick one mode (this runbook is in-turn, so NO watcher). Two supervisors on the same terminals is the top cause of churn and duplicate notifications.
- Issuing one terminal.extract wait:{} per agent instead of ONE terminal.awaitAll for the cohort — the per-agent waits run one after another and stack their timeouts.
- Padding agent prompts, or appending instructions that contradict the ask. Short and clean wins.
- tool.search-ing for terminal.sendCommand or terminal.awaitAll. They are always callable — just call them.
- A `contains` wait for output the agent already produced (it times out). Wait on the cohort with terminal.awaitAll instead.
- Relaying on awaitAll's status ALONE without reading the tail. awaitAll is state-based and fast, but idle ≠ guaranteed-done — always read each output and self-heal a terminal that reported finished yet still looks busy (re-await/watch it) before relaying. This is a top way a round gets relayed early with half-baked output.
- Relaying raw terminal scrollback. Read a FINISHED agent with a batched extract (or summarize), then relay the gist.
<<<<<<< HEAD
- Issuing the round's awaitAll in a SEPARATE turn after the sendCommands — emit it as the LAST call in the SAME batch as the sends so polling starts the moment the sends complete (saves a model round-trip per relay cycle).
- Keeping a chronic straggler inside the cohort awaitAll round after round — once the SAME terminal is the lone straggler in two consecutive rounds, isolate it into its own dedicated, higher-budget await; a cohort awaitAll blocks behind the slow terminal and stalls reading the agents that already finished.
=======
- Keeping the terminalId→role map, scores, or round counter ONLY in the transcript on a long run — auto-compaction silently discards them and the Director loses track of who is who. Checkpoint them to the scratch store each round (step 4) and read them back at the top of the next (step 3).
- One scratch key per round (`round_1`, `round_2`, …) — use a single `orchestration_state` key and OVERWRITE it every round, so the read-back is one get and the key count stays flat under the cap.
- Storing raw terminal scrollback or string-wrapped JSON in the scratch value — store concise gists, and pass the `value` as a real JSON object, not a string.
>>>>>>> 4119aad (feat(skills): checkpoint multi-round orchestration state to scratch)

Report back: the synthesized result (winner/tally/merged answer), which agents participated (real terminalIds), and any inbox cleanup you did.
