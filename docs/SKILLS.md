# Skills (server-owned)

A **skill** is a procedural runbook for one Daintree operation, written *for the
assistant model* — a checklist a sharp operator follows. Skills are the behavior
layer that replaces fine-tuning: a growing, validated library, selected cheaply.

> **Skills are owned by the Daintree Assistant backend now.** This CLI does **not**
> select, store, embed, or inject skill bodies. The old local `internal/skills`
> package, the `skill.find`/`skill.load` tools, and the `SelectSkills`/`SkillRegistry`
> machinery were **deleted** in the DeepSeek→backend migration. Authoring + selection
> live in `../assistant-backend`. See [`BACKEND.md`](BACKEND.md).

## How it works end to end

Per `respond` request, the backend's selector (a small model) classifies the
conversation against the catalog and picks the runbook(s); the backend then:

1. Renders the **active skill bodies** into a cached system message (in the stable
   prefix, so they ride DeepSeek's prefix cache across a turn's rounds).
2. Appends a synthetic `skill__load` tool-call + result to the upstream transcript so
   the main model *observes* the load — **before** it calls the model for generation,
   so the runbook is in hand for that same response (no extra round trip).
3. Returns a first-class `skills` block (`active` + `newly_loaded` + a `prelude`) plus
   a refreshed opaque **state token** in the first SSE `meta` event.

The CLI is a thin runtime over that: it stores the opaque state token and replays it
on the next request (the entire client-side "keep skills loaded" mechanism — the
backend is stateless and recovers the active set from the token, **not** from the
message history), and surfaces newly-loaded skills as a single info note
(`emitSkillsMeta` → `events.Info("Loaded skill: …")`). Skills **never** narrow the
local toolset — the full registry is offered every turn (`requiredTools` is a backend
focus-hint only, not a capability gate).

### Authoring a skill (in the backend repo)

A skill is a single Markdown file — YAML frontmatter + an opaque Markdown body —
under `../assistant-backend/src/daintree_assistant_server/skills/files/<id>.md`,
auto-globbed by the loader at startup (zero code per skill). Frontmatter is validated
by `SkillMeta` (`id`, `title`, `version` SemVer, `summary`, `whenToUse`, `tags`,
`priority`, `risk`, `requiredTools`); the body is free-form runbook prose. To add or
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
