# Authoring recipes

A **recipe** is a procedural runbook for one Daintree operation, written *for the
assistant model*, not for a human. The main model never sees recipe bodies up
front — it sees only their headers (the catalog) and pulls the full body into
context on demand with `recipe.find`. Recipes are the behavior layer that replaces
fine-tuning: a growing, validated library, selected cheaply by the small model.

> These are **assistant recipes** (hidden prompt runbooks). Do not confuse them
> with **Daintree workspace recipes** (the MCP `recipe.list` / `recipe.run` /
> `worktree.createWithRecipe` actions), which are user-facing terminal layouts.

## TL;DR — add a recipe

1. Create `recipes/<id>.md` (filename = the dotted id, e.g.
   `daintree.edits.spawn-visible-agent.md`).
2. Write the frontmatter (metadata) and the body (the runbook).
3. `npm run typecheck && npm test`. A malformed recipe fails at boot with its
   filename — there is nothing else to wire up.

No code change, no registration, no DB reset. The loader
(`src/recipes/fileSource.ts`) reads every `*.md` in `recipes/`, validates each
through the `Recipe` Zod schema, and seeds the registry.

## How fetching works (why headers matter)

```
model: "I need to figure out how to X"
  └─ calls recipe.find({ query: "X" })
       └─ small model reads the query + every recipe's two headers (NOT bodies)
            └─ returns the best 0-3 recipe ids
                 └─ their full bodies are injected into the model's context
```

So a recipe is only ever chosen on the strength of its **headers**. Bodies are
loaded *after* selection. Write headers for the selector; write the body for the
executor.

## File format

A recipe file is YAML-ish frontmatter between `---` fences, then the body:

```markdown
---
id: daintree.example.do-a-thing
title: Do a thing
version: 0.1.0
summary: How to do the thing safely with Daintree tools.
whenToUse: Use when the user asks to do the thing, kick off a thing, or wrap up a thing for review.
priority: 150
risk: project
tags:
  - thing
  - example
requiredTools:
  - context.snapshot
  - thing.start
---
Use when: the user wants to do the thing.
Procedure:
1. Read state first with context.snapshot — never guess.
2. Start the thing with thing.start, forwarding the id.
Confirmation: thing.start mutates state — confirm per the active tier.
Report back: the ids created and the next checkpoint.
```

### Frontmatter fields

| Field | Type | Required | Notes |
|---|---|---|---|
| `id` | string | ✅ | Stable dotted id, unique. Matches the filename. Never reused. |
| `title` | string | ✅ | Human label for `/recipes` and the loaded-recipe header. |
| `version` | string | ✅ | Semver-ish. **Bump when the body changes** so cache hashes shift. |
| `summary` | string | ✅ | **Short header** — see below. |
| `whenToUse` | string | ✅ | **Long header** — see below. |
| `tags` | string[] | — | Free-form bias terms. Block list or inline `[a, b]`. Default `[]`. |
| `priority` | int | — | Tie-breaker when >3 recipes match. Higher wins. Default `0`. |
| `risk` | enum | — | `read \| local \| ui \| terminal \| project \| git \| external \| system`. The riskiest class the body drives. Default `read`. |
| `requiredTools` | string[] | — | Per-turn tool allowlist — **see the gotcha**. Default `[]`. |
| `maxTurns` | int | — | Soft cap on how long the recipe stays loaded. Default `8`. |

The frontmatter parser accepts: `key: scalar` (string / int / bool, quotes
optional), inline arrays `key: [a, b, c]`, and block lists (`key:` then `  - item`
lines). Use block lists for long `requiredTools`. Nothing fancier — a typo throws
at load with the filename, by design.

### The two headers

These are the *only* thing the selector sees. Get them right.

- **`summary` — the SHORT header (~8–10 words).** A scannable one-liner of what
  the recipe does. It sits next to every other recipe in the catalog; keep it
  terse and distinct. Start with "How to …".
  - ✅ `How to spawn a visible agent for edits or read-only exploration.`
  - ❌ `This recipe explains the process for handling user requests that involve
    modifying files in the project by delegating to a separate agent process.`

- **`whenToUse` — the LONG header (1–2 sentences).** The detailed *trigger*: the
  situations, user phrasings, and verbs that should pull this recipe in. This is
  the primary match signal — spell out the synonyms.
  - ✅ `Use whenever the user asks to implement, refactor, fix, add tests, update
    docs, or otherwise change project files — OR to spawn a visible agent to
    explore/investigate a project read-only.`

## Body style

The body — everything after the closing `---` fence, trimmed of surrounding
blank lines — is injected into the main model. Write it the way you'd write a
checklist for a sharp operator who is in a hurry.

- **Dense and terse.** No preamble, no rationale essays. Every line earns its place.
- **Numbered procedure.** Imperative steps, in order. One action per step.
- **Name exact tool ids** (`agentTask.spawnForEdits`, not "the spawn tool").
- **State the safety posture inline**: a `Confirmation:` line for mutating recipes,
  a `Report back:` line for what to tell the user at the end.
- **Read before you act.** Tell the model to establish state with `context.snapshot`
  rather than assume worktree/terminal/git state.
- **No identity / hard-rule restatement.** Those live in the base prompt
  (`src/models/prompts/base.ts`); recipes never override them.

Canonical body skeleton:

```
Use when: <one sentence>.
Procedure:
1. <read state>
2. <do the typed action, naming the tool>
3. <report the real ids back; never claim a clean result you didn't verify>
Confirmation: <which action mutates state and needs confirmation>.
Report back: <the ids / checkpoint to surface to the user>.
```

### The `requiredTools` gotcha

When a recipe is loaded, the per-turn tool projection is **core ∪ the loaded
recipes' `requiredTools`** (see `CORE_TOOL_NAMES` in `src/agent/loop.ts`). Any tool
your body tells the model to call **must** be listed in `requiredTools`, or the
model never sees it — it is silently starved, with no runtime error. Under-declare
and the recipe quietly fails. (Core tools — `context.snapshot`, `tool.search`,
`recipe.find`, `recipe.load`, `recipe.step.advance`, `recipe.run.get`, … — are
always present; you don't list those, but listing them is harmless.) The
`recipeRegistry` test cross-checks every `requiredTools` name against the real tool
registry, so a typo fails CI.

### Multi-step recipes: checkpointing

If the body has numbered steps the model should checkpoint, it will call
`recipe.step.advance` after each step (and `recipe.run.get` to resume). You don't
declare those — they're core. Just write clear, numbered steps.

## Worked example

`recipes/daintree.workflow.start-work-on-issue.md`:

```markdown
---
id: daintree.workflow.start-work-on-issue
title: Start work on a forge issue
version: 0.1.0
summary: How to pick a forge issue and start work on it through Daintree workflow actions.
whenToUse: Use when the user asks to start work on an issue, pick up a ticket, begin an issue/PR, or set up a worktree/branch for a forge issue.
priority: 190
risk: external
tags:
  - issue
  - forge
  - workflow
  - start-work
requiredTools:
  - context.snapshot
  - forge.listIssues
  - forge.getIssue
  - workflow.startWorkOnIssue
---
Use when: the user wants to start work on a forge issue.
Procedure:
1. Inspect current Daintree context first if the project/worktree is ambiguous.
2. If the issue is not identified, list candidates with forge.listIssues and confirm which one with the user.
3. Read the chosen issue with forge.getIssue to ground the task before mutating anything.
4. Start work with workflow.startWorkOnIssue, forwarding the issue identifier in arguments.
5. workflow.startWorkOnIssue attaches a supervising watcher automatically — report the returned watcherId; do not poll.
Confirmation: workflow.startWorkOnIssue mutates real state and touches the forge — confirm before launch per the active tier.
Report back: which issue was started, the worktree/branch/terminal ids created, the watcher id attached, and the next checkpoint.
```

## Authoring checklist

- [ ] Filename is the dotted `id`, ending `.md`.
- [ ] `summary` is ~8–10 words, starts with "How to …".
- [ ] `whenToUse` names the trigger verbs/phrasings the selector should match.
- [ ] `risk` reflects the riskiest action the body drives.
- [ ] **Every tool the body names is in `requiredTools`.**
- [ ] Body is numbered, terse, tool-id-exact, with `Confirmation:` / `Report back:`.
- [ ] Bumped `version` if you edited an existing recipe's body.
- [ ] `npm run typecheck && npm test` is green.

## Loading internals (reference)

- Loader: `src/recipes/fileSource.ts` — sync, validated, fails loud with the
  filename. Resolves `recipes/` by walking up to the dir holding `package.json`,
  so it works from source (bun/tsx/vitest) and from the bundle (node). Override
  with `DAINTREE_ASSISTANT_RECIPES_DIR` (used by tests).
- Registry: `src/recipes/registry.ts` — seeded from the loaded set; exposes
  `metadataForSelection()` (headers only) to the catalog + selector.
- Catalog: `buildRecipeCatalogMessage()` in `src/models/prompts/recipes.ts` —
  every recipe's headers, appended to the runtime-context system message.
- Selection: `selectRecipes()` in `src/recipes/selector.ts` — small model, query
  in, ids out. Driven by the `recipe.find` tool (`src/tools/recipeRunTools.ts`).
- The seam is swappable: a future hosted recipe service replaces the file loader
  behind the `RecipeSource` interface (`src/recipes/source.ts`) without touching
  any caller.
