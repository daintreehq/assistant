# Skills (server-owned)

A **skill** is a procedural runbook for one Daintree operation, written *for the
assistant model* — a checklist a sharp operator follows. Skills are the behavior
layer that replaces fine-tuning: a growing, validated library, selected cheaply.

> **Skills are owned by the Daintree Assistant backend now.** This CLI does **not**
> select, store, embed, or inject skill bodies. The old local `internal/skills`
> package, the `skill.find`/`skill.load` tools, and the `SelectSkills`/`SkillRegistry`
> machinery were **deleted** in the backend migration. Authoring + selection live in
> `../assistant-backend`. See [`BACKEND.md`](BACKEND.md).

## How it works end to end

Per `respond` request, the backend's selector (a small model) classifies the
conversation against the catalog and picks the runbook(s); the backend then:

1. Renders the **active skill bodies** into a cached system message (in the stable
   prefix, so they ride the model's prefix cache across a turn's rounds). Cache
   semantics are a property of the model route the backend picked through OpenRouter,
   not a universal guarantee — treat a measured cache result as valid for the route it
   was measured on.
2. Opens the response stream with a first-class `skills` block (`active` +
   `newly_loaded` + a vestigial `prelude`) plus
   a refreshed opaque **state token** in the first SSE `meta` event. That event is
   flushed as soon as skill selection finishes, before the upstream model connection
   or first generated token.
3. Starts upstream generation with the selected bodies already present in the prompt,
   so the runbook is in hand for that same response (no extra main-model round trip).

The CLI is a thin runtime over that: it stores the opaque state token and replays it
on the next request (the entire client-side "keep skills loaded" mechanism — the
backend is stateless and recovers the active set from the token, **not** from the
message history). `StreamCallbacks.OnSkillLoaded` emits each newly-loaded skill
immediately as a `SkillLoaded` event, while the state-bearing `OnMeta` callback
remains deferred until the stream attempt commits so retries cannot advance state from
a failed attempt. If a full POST retry is needed after meta, the client adopts that
attempt's signed `state` into the next request so the backend reuses the same
selection instead of selecting again; repeated refs are also de-duplicated defensively.

**A backend skill load never enters the conversation.** `SkillLoaded` is a diagnostic
signal — the durable run log, the `--json` stream and the debug trace consume it; the
attached session, console and host sinks deliberately drop it (pinned by
`internal/ui/render_skill_test.go` and `TestConsoleSinkSkillLoadedIsSilent`). The one
place it reaches a human is the explicit **`/explain <run>`** timeline, where it prints as
`✦ skill loaded: …` beside that run's tool calls and errors — a retrospective diagnostic
view the user asked for, not the live transcript. Which
runbook the backend picked is prompt assembly, not a decision the operator takes or can
reverse, and the `newly_loaded` delta the old inline card rendered was actively
misleading: it never showed what was retained across rounds, dropped by the max-active
cap, or paired in automatically as a domain foundation.

There is **no `/skills` command** either (pinned by `TestUISkillsCommandDoesNotExist`) — a
standing "what is active?" reveal is the same information with the same missing
affordance, which is what separates it from `/explain`'s per-run history. Selector tuning
reads the debug trace, where `backend.respond.meta` logs the `active` and `newly_loaded`
sets per round. The whole "skill" **vocabulary** — a visible "Skill loaded" event, the
`/skills` name — is deliberately held in reserve for future user-authored *assistant*
skills, which ARE intent-driven and will want it for list/create/edit.
If the terminal retry fails before receiving its own meta, the client still forwards the
last adopted meta once so that selection state is persisted. Skills **never** narrow the
local toolset — the full registry is offered every turn (`requiredTools` is a backend
focus-hint only, not a capability gate).

### Authoring a skill (in the backend repo)

A skill is a single Markdown file — YAML frontmatter + an opaque Markdown body —
under `../assistant-backend/src/daintree_assistant_server/skills/files/<id>.md`,
auto-globbed by the loader at startup (zero code per skill). Frontmatter is validated
by `SkillMeta` (`id`, `title`, `summary`, `whenToUse`, `foundation`, `tags`,
`priority`, `risk`, `requiredTools`); the body is free-form runbook prose. Skills
are **unversioned** — there is no `version` key, and because `SkillMeta` is
`extra="forbid"` adding one fails the skill load outright. Change-busting rides
the catalog content hash/revision instead. To add or
change a workflow: edit the `.md`, run the backend locally
(`cd ../assistant-backend && python -m daintree_assistant_server`, serves on
`127.0.0.1:8473`), and retest through this CLI. Authoritative schema + selector
behavior: see the backend repo (`skills/models.py`, `skills/selector.py`) and its
`docs/`.

## What the CLI still owns: stepwise run-tracking

The CLI keeps two local tools for multi-step skills — they track progress, they do
NOT select or load:

- **`skill.run.get`** — read the saved stepwise checkpoint for `(sessionId, skillId)`.
  Absence is a normal OK answer (a run-state row is created lazily on first advance).
- **`skill.step.advance`** — record a completed step (`done` | `skipped`); omitting
  `nextStep` finishes the run. Session-scoped only.

When a backend runbook has numbered steps, its body tells the model to call these
after each step (and `skill.run.get` to resume). Both are core tools — always present,
never listed in a skill's `requiredTools`.
