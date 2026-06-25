---
id: daintree.terminals.rename
title: Rename terminal tabs
version: 0.1.0
summary: Give terminals/agents clear tab titles — the fast, direct route (no tool hunting).
whenToUse: Use whenever the user wants to rename terminals/tabs, retitle agents, or "figure out what each of these terminals is doing and give them a title" — especially when several agents share the same name (e.g. all "Claude") and need to be told apart.
priority: 90
risk: ui
tags:
  - daintree
  - terminal
  - rename
  - title
  - tab
  - agent
requiredTools:
  - context.snapshot
  - terminal.extract
  - terminal.rename
---
Use when: the user wants terminals/agents/tabs renamed or titled — e.g. "figure out what each of these is doing and give them a proper title", "rename these", "title the agents".

The tool is terminal.rename({ terminalId, name }). It is always callable by name — DO NOT use tool.search or daintree.listTools to find it, and DO NOT route it through daintree.call. Hunting for it is the slow path; this is the fast one.

Always pass BOTH terminalId AND a non-empty name: omitting name opens an interactive rename dialog instead of renaming, and omitting terminalId targets the focused tab. Be explicit on every call.

Tool calls in one response are dispatched ONE AFTER ANOTHER, not in parallel — so the way to go fast is FEWER model calls, not "a big batch". Read every terminal in ONE terminal.extract over all the ids (a single small-model call), not one terminal.summarize per terminal.

Procedure:
1. GET THE IDS. If you don't already have the terminal ids + current titles, call context.snapshot (it embeds the terminal list). If the user pointed at a subset ("these three"), keep only those ids.
2. UNDERSTAND — in ONE call. Call terminal.extract over ALL the ids at once (it accepts up to 16 terminalIds) with an instruction that asks for a per-terminal title, e.g. instruction: "For EACH terminal, give one line `<terminalId> — <short tab title of what this agent is doing>` (max ~6 words per title)." That is a single model read of the whole cohort. Skip this entirely for any terminal whose job you already know. The extract result is a LEAF — read it, don't re-extract or summarize it.
3. RENAME. Emit one terminal.rename per id: terminal.rename({ "terminalId": "<id>", "name": "<short title>" }). Keep names short and scannable, leading with the kind of work. terminal.rename is UI-only (no confirmation) and each call is a cheap local UI op, so renaming the whole set is quick even though the calls run in sequence.

Then report a one-line "what it's doing" for each terminal next to its new title.

Fast-path shape (three agents all titled "Claude"):
  context.snapshot()                               // → ids t1, t2, t3 + current titles
  terminal.extract({                               // ONE call reads all three
    terminalIds: ["t1", "t2", "t3"],
    instruction: "For EACH terminal, output `<terminalId> — <≤6-word title of what it is doing>`."
  })
  // then, from that one result:
  terminal.rename({ terminalId: "t1", name: "bug: allowlist gap in WorkspaceHostProcess" })
  terminal.rename({ terminalId: "t2", name: "merge pipeline: PRs #10779-84" })
  terminal.rename({ terminalId: "t3", name: "debug: simple-git editor guard" })

Notes:
- If you already know what a terminal is doing (you just spawned it, or the user told you), skip the extract and rename it directly.
- Renaming is independent per terminal, so one failed rename never blocks the others — report which ones changed.
