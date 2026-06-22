# Port spec: Skills subsystem

Authoritative reference for porting the assistant **skills** subsystem from
TypeScript to Go (Bubble Tea UI). Written so an implementing agent does not need
to re-read the TS source.

## 0. What this subsystem is

An **assistant skill** is a short, procedural runbook (markdown body + metadata)
injected into the main model's context when it is relevant to the user's current
task. Skills are "the behavior layer that replaces fine-tuning": a growing,
validated, on-disk library. The flow:

1. Every skill's **headers** (id + two header strings, never bodies) are rendered
   into a static **skill catalog** appended to the main model's runtime-context
   control message (`message[1]`).
2. The main model calls the `skill.find` tool with a natural-language query.
3. A cheap **small-model selector** reads the query + every skill's headers and
   returns the best **0-3** skill ids.
4. Those skills' **bodies** are merged into the loaded set (capped at 3) and
   injected into `message[2]`, the "loaded skills" control message.
5. While skills are loaded, the per-turn tool list sent to the model is narrowed
   to `CORE_TOOL_NAMES ∪ (loaded skills' requiredTools)`.

> **Do NOT confuse assistant skills with Daintree workspace recipes** (the MCP
> `recipe.list` / `recipe.run` / `worktree.createWithRecipe` actions). Recipes are
> user-facing terminal layouts; skills are hidden prompt instructions. They are
> entirely separate systems — one of the skills (`daintree.recipe.run-or-create`)
> is *about* recipes, but that's coincidence.

Source files (all under `src/skills/` unless noted):

| TS file | Role |
|---|---|
| `types.ts` | Zod schemas: `Skill`, `SkillMetadata`, `SkillSelection`, `SkillFindResult`, `SkillRisk` enum |
| `source.ts` | `SkillSource` interface (the swappable seam: `has`/`get`) |
| `fileSource.ts` | Markdown loader: frontmatter parse + validate + dir discovery |
| `builtin.ts` | `BUILTIN_SKILLS` (loaded at import) + named-handle accessors |
| `registry.ts` | `SkillRegistry` in-memory store |
| `selector.ts` | `selectSkills()` small-model call + system prompt |
| `render.ts` | `renderSkillBundle()` — sort, hash, bundle |
| `../tools/skillRunTools.ts` | The 4 model-facing tools: `skill.find`, `skill.load`, `skill.step.advance`, `skill.run.get` |
| `../models/prompts/skills.ts` | `buildSkillCatalogMessage`, `buildLoadedSkillsMessage` |
| `../agent/loop.ts` | Active-skill merge, message[2] rewrite, tool filter, selection logging |
| `../cli/commandData.ts` / `../cli/commands.ts` | `/skills` manual ops |
| `../schemas.ts` / `../storage/db.ts` | `skill_run_state`, `skill_selection_log` tables |
| `skills/*.md` (repo root) | The 5 actual skill content files |

---

## 1. Types, enums, exact field sets

### 1.1 `SkillRisk` (enum)

Exact value set, in this order (mirrors tool risk classes):

```
read, local, ui, terminal, project, git, external, system
```

Default when omitted from frontmatter: `read`.

> Note ordering differs slightly from the tool-risk doc elsewhere, but the *set*
> is what matters. Port as a Go string type with these 8 constants. Validation:
> reject any other value at load.

### 1.2 `Skill`

The full skill (metadata + body). Zod schema in `types.ts`. Field table:

| Field | Type | Required | Default | Constraints / notes |
|---|---|---|---|---|
| `id` | string | yes | — | `min(1)`. Stable dotted id, e.g. `daintree.edits.spawn-visible-agent`. Unique across registry. Matches filename (sans `.md`). |
| `title` | string | yes | — | `min(1)`. Human label shown in `/skills` + loaded-skills header. |
| `version` | string | yes | — | `min(1)`. Semver-ish string. Used in the bundle hash (`id@version`). Plain integers like `1` would coerce to number in frontmatter — versions have dots so stay strings; a bare integer version would fail the `string` schema. **Bump when body changes** so cache hashes shift. |
| `summary` | string | yes | — | `min(1)`. SHORT header (~8-10 words): "what this skill does". Shown in catalog + given to selector. |
| `whenToUse` | string | yes | — | `min(1)`. LONG header (1-2 sentences): "when to reach for this". **Primary** field the selector matches against. |
| `tags` | string[] | no | `[]` | Free-form bias terms. |
| `priority` | int | no | `0` | `z.number().int()`. Higher wins ties when >3 match (selector-side intent; see §6 caveat). |
| `maxTurns` | int | no | `8` | `z.number().int().positive()`. Soft cap on how long a skill stays loaded. **Currently advisory only** — not enforced anywhere in the active code path. Port the field; don't invent enforcement. |
| `risk` | `SkillRisk` | no | `read` | Riskiest action class the body drives. |
| `requiredTools` | string[] | no | `[]` | Per-turn tool allowlist (unioned with core tools). Empty ⇒ "core tools only". **Under-declaring silently starves the model** — preserve exactly. |
| `body` | string | yes | — | `min(1)`. The runbook markdown injected into the model. NOT in frontmatter — it's the text after the closing `---`. |

### 1.3 `SkillMetadata`

A `.pick()` of `Skill` containing exactly: `id, title, summary, whenToUse, tags,
priority`. **Never includes `body`, `version`, `risk`, `maxTurns`, or
`requiredTools`.** This is the only view the selector ever sees. Keep this subset
exact — it bounds selector input cost and injection surface.

### 1.4 `SkillSelection` (selector model output)

The structured JSON the small model returns. Zod-validated:

| Field | Type | Constraints |
|---|---|---|
| `skillIds` | string[] | `.max(3)` — at most 3 |
| `confidence` | number | `.min(0).max(1)` |
| `reason` | string | one-line rationale |
| `taskType` | string | free-form classification label |

### 1.5 `SkillFindResult` (plain interface — `skill.find` engine return)

Returned by `AgentSession.findSkills()`. Not a Zod schema — plain struct.

| Field | Type | Notes |
|---|---|---|
| `ok` | bool | `false` ONLY when the selector model errored (or cancelled); loaded set left unchanged. |
| `matched` | bool | true ⇒ resolved ≥1 skill. |
| `query` | string | echoed back. |
| `reason` | string | selector's one-line rationale (or `"skill selector unavailable"` / `"cancelled"`). |
| `confidence` | number | [0,1]. 0 on failure/cancel. |
| `selected` | `{id, title, summary}[]` | The skills THIS query pulled in (NOT the whole loaded set; hallucinated ids dropped). |
| `activeSkillIds` | string[] | The FULL loaded id set after the find. Always a copy. |

---

## 2. Skill file format (frontmatter + body)

Each skill is one markdown file under repo-root `skills/`. Filename = the dotted
id + `.md` (convention; the loader does not enforce filename==id, but
`builtin.ts` named handles assert specific ids exist).

### 2.1 Overall shape

```
---
<frontmatter>
---
<body markdown>
```

### 2.2 Fence parsing (`parseSkillFile`)

Exact behavior to preserve:

1. Strip a leading UTF-8 BOM (`﻿`, the literal `﻿` char in the regex
   `/^﻿/`) if present.
2. Strip any leading run of blank lines before the opening fence
   (`/^(?:[ \t]*\r?\n)+/`).
3. Match the fence with this regex (CRLF-tolerant, trailing spaces/tabs allowed
   on fence lines):
   ```
   /^---[ \t]*\r?\n([\s\S]*?)\r?\n---[ \t]*\r?\n?([\s\S]*)$/
   ```
   Group 1 = frontmatter block; group 2 = body.
4. No fence ⇒ throw `"<filename>: missing YAML frontmatter (a skill must open with a --- … --- block)."`.
5. Body = group 2 **`.trim()`-ed**.
6. Build `{...meta, body}` and validate through the `Skill` schema; on schema
   error throw `"<filename>: invalid skill — <message>"`.

### 2.3 Frontmatter parser (`parseFrontmatter`) — exact grammar

This is a **hand-rolled, deliberately tiny YAML subset** (no YAML library — the
shape is owned/constrained). Port it faithfully; do NOT substitute a general YAML
parser (it would accept inputs the TS rejects, and vice versa).

Rules, line by line over the frontmatter block:

- Blank line (after `.trim() === ""`) ⇒ skip.
- A line starting with whitespace (`/^\s+/`) that was NOT consumed as a block-list
  item ⇒ throw `"<filename>: unexpected indented line: \"<line>\""`.
- Otherwise must match `/^([A-Za-z0-9_]+):\s?(.*)$/` (key chars: letters, digits,
  underscore). No match ⇒ throw `"<filename>: malformed frontmatter line: \"<line>\""`.
  Note the `\s?` — exactly zero or one space after the colon is consumed; the rest
  is `rest`.
- **Duplicate key** (key already present) ⇒ throw `"<filename>: duplicate frontmatter key: \"<key>\""`.
  (Prevents a second `requiredTools:` silently clobbering the first.)
- Value resolution by `rest`:
  - `rest === ""` ⇒ **block list**: consume following lines matching
    `/^\s*-\s+/`, strip the `^\s*-\s+` prefix, `coerceScalar` each. Empty array
    when no items follow.
  - `rest` starts `[` and ends `]` ⇒ **inline array**: inner = `rest.slice(1,-1).trim()`.
    Empty ⇒ `[]`. Else split on `,`, `coerceScalar` each, then **filter out `""`
    entries** (so `[a, b,]` does not smuggle a `""`).
  - else ⇒ `coerceScalar(rest)`.

### 2.4 `coerceScalar(raw)` — scalar coercion

`raw.trim()` then, in order:

1. If wrapped in matching `"…"` or `'…'` quotes ⇒ strip one layer, return the
   inner string (no further coercion).
2. `"true"` ⇒ boolean `true`; `"false"` ⇒ boolean `false`.
3. Matches `/^-?\d+$/` (plain signed integer, no dots) ⇒ `Number(v)`.
   **Versions like `0.2.0` have dots so stay strings.**
4. else ⇒ the trimmed string.

Resulting value type: `string | number | boolean`. The `Skill` schema then
type-checks (e.g. `priority` must coerce to int, `version` must stay string).

### 2.5 `isSkillFile(name)` — which files load

A file is a skill iff ALL:
- lowercased name ends `.md`
- name does NOT start with `.` or `_`
- lowercased name is not `readme.md`

### 2.6 `loadSkillsFromDir(dir)`

`fs.readdirSync(dir)` → filter `isSkillFile` → **`.sort()` (lexicographic on
filename)** for deterministic order → map each through `parseSkillFile(read, name)`.
**One bad file aborts the entire load** (throws) — fail-loud at boot.

### 2.7 Directory discovery (`resolveSkillsDir` / `findSkillsDir`)

- Env override **`DAINTREE_ASSISTANT_SKILLS_DIR`** (trimmed) wins if set
  (absolute path; mainly for tests).
- Else: start at the dir of the current module, walk **up** to the nearest
  `package.json`, resolve `skills/` beside it. Stop at the FIRST `package.json`
  (never climb past the package root to adopt an ancestor's `skills/`). If that
  package root has a `skills/` dir ⇒ return it; if not ⇒ return undefined (an
  error, not a reason to keep walking). Reaching filesystem root ⇒ undefined.
- `resolveSkillsDir` throws if nothing found:
  `"Could not locate the skills/ directory (searched up from <here>). Set DAINTREE_ASSISTANT_SKILLS_DIR to override."`.

### 2.8 The 5 shipped skill files (exact metadata)

These must port byte-identically (the bodies are model behavior contracts). They
live at repo-root `skills/`. Filename = `<id>.md`.

| id | version | priority | risk | maxTurns | requiredTools (count) |
|---|---|---|---|---|---|
| `daintree.orchestration.basic` | 0.1.1 | 100 | read | 8 (default) | 18 |
| `daintree.edits.spawn-visible-agent` | 0.2.0 | 200 | project | 8 | 4 |
| `daintree.recipe.run-or-create` | 0.1.0 | 180 | project | 8 | 6 |
| `daintree.workflow.start-work-on-issue` | 0.1.0 | 190 | external | 8 | 4 |
| `daintree.workflow.prep-branch-for-review` | 0.1.0 | 185 | external | 8 | 4 |

The `requiredTools` lists are the per-turn allowlists and MUST be carried exactly
(they gate which tools the model can see while the skill is loaded). The full
files are the source of truth — copy them verbatim into the Go repo's `skills/`
dir (embed plan in §9). Do not paraphrase bodies.

---

## 3. `SkillSource` seam (`source.ts`)

```
interface SkillSource {
  has(id string) bool
  get(id string) (*Skill, ok)   // undefined/nil when unknown
}
```

The narrow contract callers (the `skill.load` tool) may rely on, so the backing
store can later become a hosted service. `SkillRegistry` satisfies it
structurally. Port as a Go interface.

---

## 4. `SkillRegistry` (`registry.ts`)

In-memory `map[string]Skill`, insertion via constructor from an initial set
(default `BUILTIN_SKILLS`). Each raw entry is re-validated through the `Skill`
schema on construction. **Duplicate id throws** `"Duplicate skill id: <id>"`.

Methods (all O(1)/O(n) over the map):

| Method | Signature | Behavior |
|---|---|---|
| `list()` | → `[]Skill` | All skills (map value order; see ordering caveat below). |
| `has(id)` | → bool | Membership. |
| `get(id)` | → `*Skill` | nil when unknown. |
| `getMany(ids)` | `[]string → []Skill` | Resolve ids → skills, **silently dropping unknown ids**, preserving input order. |
| `metadataForSelection()` | → `[]SkillMetadata` | Maps each skill to the 6-field metadata subset. **Never includes bodies.** |

**Ordering caveat:** in TS, `Map` preserves insertion order, and `BUILTIN_SKILLS`
is filename-sorted, so `list()`/`metadataForSelection()` are effectively
filename-sorted. In Go, `map` iteration is random — **back the registry with an
insertion-ordered structure** (e.g. a `[]Skill` plus a `map[string]int` index, or
keep a sorted `ids []string`) to preserve deterministic catalog ordering, since
catalog text stability matters for prompt caching.

---

## 5. `BUILTIN_SKILLS` & named handles (`builtin.ts`)

- `BUILTIN_SKILLS = loadSkillsFromDir()` — evaluated once at module import
  (eager). In Go: load at package init / first use; one bad file panics/errors at
  startup (preserve fail-loud).
- `byId(id)` throws `"Built-in skill '<id>' is missing from skills/"` if absent.
- Named exported handles (assert-at-load that these specific ids exist):
  - `BASIC_DAINTREE_ORCHESTRATION_SKILL` = `daintree.orchestration.basic`
  - `SPAWN_AGENT_FOR_EDITS_SKILL` = `daintree.edits.spawn-visible-agent`
  - `DAINTREE_RECIPE_RUNNER_SKILL` = `daintree.recipe.run-or-create`
  - `WORKFLOW_START_WORK_SKILL` = `daintree.workflow.start-work-on-issue`
  - `WORKFLOW_PREP_BRANCH_SKILL` = `daintree.workflow.prep-branch-for-review`

These named handles exist so code/tests referencing a skill fail fast on a
rename/removal. Port as package-level vars/accessors that error if the id is missing.

---

## 6. Selector (`selector.ts`)

Small-model call that turns a query + candidate headers into a `SkillSelection`.

### 6.1 Constants

- `MAX_QUERY_CHARS = 2000` — the query is `.slice(0, MAX_QUERY_CHARS)` before
  use (bounds cost + prompt-injection surface).
- Model tier: **`"small"`** (the cheap Flash model).
- `temperature: 0`
- `maxTokens: 500`

### 6.2 System prompt (byte-stable — this is a wire/behavior contract)

```
You are the Daintree Assistant skill selector.
Return only JSON.
The main assistant has hit a point where it wants a procedural runbook ("skill") and has given you a query describing what it needs to figure out. Choose the 0-3 skills whose full instructions best answer that query.
Rules:
- Match the query against each candidate's "whenToUse" (the detailed signal) and "summary".
- Return 0 skills if none genuinely fit — do not force a match.
- Return 1 skill for a focused need; 2-3 only when the task clearly spans multiple skills.
- Order skillIds best-match first.
- Never invent skill ids. Choose only from the candidate list.
Return this JSON shape:
{
  "skillIds": ["string"],
  "confidence": 0.0,
  "reason": "string",
  "taskType": "string"
}
```

### 6.3 User message (exact template)

```
JSON selection task.
Query (what the assistant needs to figure out):
<query>

Candidate skills (headers only):
<JSON.stringify(candidates, null, 2)>

Return the JSON object now.
```

`candidates` is `metadataForSelection()` serialized as 2-space-indented JSON
(`SkillMetadata[]`). In Go: `json.MarshalIndent(candidates, "", "  ")`.

### 6.4 Call shape

`router.json("small", {messages, temperature:0, maxTokens:500, signal}, SkillSelection)`.
`router.json` posts to Fireworks (OpenAI-compatible) requesting structured JSON
and validates the result against the schema. The selector passes the abort
`signal` through so a user-cancel during selection tears the request down (small
model rejects with a `CancelledError`).

> **`priority` and the "tie-break when >3 match" intent are NOT enforced in
> code.** The selector model is *told* to return ≤3 and order best-first; the
> hard cap-to-3 and dedup happen on the consumer side (`resolveKnownIds`, §7.4).
> `priority` is only metadata fed to the selector. Don't invent a priority sort.

---

## 7. Active-skill merge & message rewriting (`agent/loop.ts`)

The `AgentSession` owns the live loaded-skill state. Control messages live at
**fixed indices** and this invariant must be preserved exactly:

- `messages[0]` = base system prompt (cached prefix, byte-stable, never changes
  mid-session).
- `messages[1]` = runtime context **+ the skill catalog appended** (see §8).
- `messages[2]` = loaded skills message (rewritten in place on every skill change).

### 7.1 State fields

- `activeSkillIds []string` (init `[]`)
- `skillBundle RenderedSkillBundle` (init `renderSkillBundle([])`)
- `skillCatalog string` — built ONCE in the constructor from
  `metadataForSelection()`; static for the session (skills don't change
  mid-session). Re-appended whenever `message[1]` is rewritten.

### 7.2 `applySkillBundle(skills)` (private)

The single mutation point:
```
this.skillBundle = renderSkillBundle(skills)
this.activeSkillIds = this.skillBundle.ids        // sorted ids from the bundle
this.messages[2] = {role:"system", content: buildLoadedSkillsMessage(this.skillBundle)}
```
**Only `messages[2]` is touched.** `messages[0]`/`[1]` are never disturbed, so the
cached prefix stays stable.

### 7.3 `findSkills(query, signal)` → `SkillFindResult` (the `skill.find` engine)

1. `selection = selectSkills({router, candidates: metadataForSelection(), query, signal})`.
   On throw ⇒ return `{ok:false, matched:false, query, reason:"skill selector unavailable", confidence:0, selected:[], activeSkillIds: copy(active)}`.
2. If `signal.aborted` after the call ⇒ return `{ok:false, matched:false, query, reason:"cancelled", confidence:0, selected:[], activeSkillIds: copy(active)}` (don't mutate the live set with an abandoned result).
3. `newlyKnown = resolveKnownIds(selection.skillIds)` — the skills this query
   actually resolved (hallucinated ids dropped).
4. `merged = resolveKnownIds([...selection.skillIds, ...activeSkillIds])` — **new
   ids FIRST** so they survive the cap-of-3.
5. `applySkillBundle(registry.getMany(merged))`.
6. `logSelection(query, selection, newlyKnown)` — logs what the query resolved,
   **NOT the post-merge active set** (so a no-match isn't recorded as if it loaded
   the existing set).
7. `selected = registry.getMany(newlyKnown).map(r => {id,title,summary})`.
8. return `{ok:true, matched: len(selected)>0, query, reason:selection.reason, confidence:selection.confidence, selected, activeSkillIds: copy(active)}`.

### 7.4 `resolveKnownIds(ids)` (private) — the cap/dedup gate

```
[...new Set(ids)]                  // dedup, preserving first-seen order
  .filter(id => registry.has(id))  // drop unknown/hallucinated BEFORE the cap
  .slice(0, 3)                     // cap at 3
```
**Order is load-bearing:** dedup → drop-unknown → cap. Dropping unknowns *before*
slicing means a hallucinated id can never push a valid skill out of the top 3.

### 7.5 `loadAdditionalSkills(ids)` → `[]string` (the `skill.load` engine)

```
merged = resolveKnownIds([...ids, ...activeSkillIds])   // new ids first
applySkillBundle(registry.getMany(merged))
return copy(activeSkillIds)
```
New ids go first so an explicit load evicts the lowest-priority prior skill rather
than being dropped when 3 are already loaded.

### 7.6 `setSkills(ids)` — manual set (the `/skills load` / `/skills clear` path)

`applySkillBundle(registry.getMany(resolveKnownIds(ids)))`. `setSkills([])` clears.

### 7.7 `getActiveSkillIds()` → read-only `[]string`.

### 7.8 `describeSkills()` → string (the `/skills loaded` text)

- Empty bundle ⇒ `"No skills are currently loaded."`
- Else: header `"Loaded skills (<n>, bundle <hash>):"` then one line per item:
  `"  <id>  [<risk>]  <title> — <summary>"`.

### 7.9 `buildToolFilter()` → `[]string | nil` (per-turn tool projection)

- `len(activeSkillIds)==0` ⇒ return **nil/undefined** ("unconstrained, send the
  full registry" — an unconstrained turn must not be tool-starved).
- Else ⇒ `unique(CORE_TOOL_NAMES ∪ flatMap(loaded skills → requiredTools))`.

This is recomputed each turn. On a read-only (autonomous wake) turn the session
uses `readOnlyToolNames()` instead (§7.11).

### 7.10 `CORE_TOOL_NAMES` (always-available tools, even with skills loaded)

Exact list (order doesn't matter — deduped into a set):
```
context.snapshot, fs.read, fs.list, fs.search, queue.digest, daintree.status,
tool.search, terminal.read, terminal.extract, skill.step.advance, skill.run.get,
skill.find, skill.load, memory.recall, memory.list, … (artifact.read + others
beyond the skill cut — see the full loop.ts list for the non-skill tail)
```
The skill-relevant guarantees: `skill.find`, `skill.load`, `skill.step.advance`,
`skill.run.get`, and `tool.search` are ALWAYS in core so the model can always
discover and pull skills even when an active skill's allowlist would otherwise
filter them out.

### 7.11 `readOnlyToolNames()` & `SKILL_CONTEXT_MUTATING_TOOLS`

`SKILL_CONTEXT_MUTATING_TOOLS = {"skill.find", "skill.load"}`. On autonomous
(non-user-initiated / watcher / timer wake) turns, `readOnlyToolNames()` =
all `risk:"read"` tools **except** those in `SKILL_CONTEXT_MUTATING_TOOLS`.
Rationale: `skill.find`/`skill.load` are `risk:"read"` but their *effect* is a
write to the live loaded-skill set (they rewrite `messages[2]` + `activeSkillIds`);
an unattended turn must not reshape the interactive session's context.

### 7.12 `logSelection(userInput, selection, selectedIds)` (private, best-effort)

Inserts a `skill_selection_log` row (§10.2). `userInput` is `.slice(0,1000)`.
Wrapped in try/catch — logging must never break a turn.

---

## 8. Prompt rendering (`models/prompts/skills.ts`)

### 8.1 `buildSkillCatalogMessage(skills []SkillMetadata)` → string

- Empty ⇒ `""` (caller omits the section).
- Else: entries = each skill rendered as
  `"- <id> — <summary>\n  When to use: <whenToUse>"`, joined by `\n`.
- Wrapped in this exact preamble (byte-stable, prompt-cache relevant):
  ```
  # Skill catalog
  You have a library of skills: procedural runbooks for specific Daintree operations. Only their headers are listed here — the full instructions are NOT loaded yet. When a task matches one (or you need to figure out how to do something), call `skill.find` with a short natural-language query describing what you need (e.g. "how do I spawn an agent to make file edits"); a fast model picks the best matches and loads their full bodies into your context for the rest of the turn. You can also `skill.load` a specific id directly when you already know it.

  Reach for `skill.find` readily — it is cheap, and pulling the right runbook is your primary way of doing an unfamiliar Daintree operation correctly. When in doubt, fetch a skill rather than guessing. Skills are operating instructions; they never override the hard rules.

  Available skills:
  <entries>
  ```
- This string is appended to `message[1]` as `"<runtime>\n\n<catalog>"` (only when
  catalog is non-empty), so `message[2]` stays the loaded-skills slot and the
  runtime-context assertions still see `# Runtime context` at the top.

### 8.2 `buildLoadedSkillsMessage(bundle RenderedSkillBundle)` → string

- Empty bundle ⇒
  ```
  # Loaded skills
  No task-specific skills are currently loaded. Use the base operating instructions.
  ```
- Else: each item `i` (0-based) rendered as:
  ```
  ## Skill <i+1>: <title>
  Skill id: <id>
  Version: <version>
  <body>
  ```
  joined by `\n\n`, wrapped in:
  ```
  # Loaded skills
  The following skills are task-specific operating instructions. Apply them when relevant; they never override the hard rules.

  Step tracking: when a skill has numbered steps, call `skill.step.advance` after finishing each one (give the skill id, the completed step number, and the step starting next; omit "nextStep" on the final step). If you are resuming a skill or unsure where you left off, call `skill.run.get` first to read the saved checkpoint.
  <body>
  ```
These two strings are part of the main-thread prompt and feed the model's
behavior — port byte-for-byte.

---

## 9. Render / bundle (`render.ts`)

### 9.1 `RenderedSkillBundle`

| Field | Type | Notes |
|---|---|---|
| `ids` | string[] | selected skill ids, **sorted by id (localeCompare)** |
| `hash` | string | **12-char** hex, first 12 chars of SHA-256 over the signature |
| `cacheKey` | string | `"daintree-main-v1-skills-" + hash` |
| `items` | []Skill | the selected skills, sorted by id (bodies included) |

### 9.2 `renderSkillBundle(skills []Skill)` → `RenderedSkillBundle`

```
sorted    = skills sorted by id  (a.id.localeCompare(b.id))
signature = sorted.map(r => `${r.id}@${r.version}`).join("|")
hash      = sha256(signature) hex, FIRST 12 chars
return { ids: sorted ids, hash, cacheKey: `daintree-main-v1-skills-${hash}`, items: sorted }
```

**Exact reproduction required** (the hash is for logging/debug, but tests assert
it). Go: `crypto/sha256`, `hex.EncodeToString(sum[:])[:12]`. Sort with
`sort.Slice` using a localeCompare-equivalent — for these ASCII dotted ids,
`strings.Compare` (byte order) matches `localeCompare`; if any non-ASCII id ever
appears, use `golang.org/x/text/collate` to match JS. For the current 5 ids,
plain byte sort is identical.

> The `cacheKey` here (`daintree-main-v1-skills-<hash>`) is a **debug/log** key,
> NOT the live Fireworks `prompt_cache_key`. The real prompt-cache key is the
> separate constant `MAIN_PROMPT_CACHE_KEY = "daintree-main"` (in `agent/loop.ts`),
> which is plain and unversioned. Changing the loaded skills does NOT churn the
> live cache prefix. Keep these two distinct — do not wire `cacheKey` into the
> Fireworks request.

### 9.3 go:embed plan

- Mirror the repo-root `skills/` directory in the Go module.
- Embed with `//go:embed skills/*.md` into an `embed.FS`, and have the loader read
  from the embedded FS by default (so the binary is self-contained), while still
  honoring `DAINTREE_ASSISTANT_SKILLS_DIR` as an on-disk override (reads from real
  FS when set). This replaces the TS `findSkillsDir` package-root walk (which only
  exists because Node resolves files at runtime). The override path is needed for
  tests and remains a string env var.
- Keep filename sorting deterministic over the embedded entries
  (`fs.ReadDir` on `embed.FS` already returns sorted names; preserve it).

---

## 10. Persistence (SQLite — schema/wire contract)

Two tables. Both names, column names, and id prefixes are a compatibility
contract if any cross-tool reader exists; at minimum keep them stable within the
Go port.

### 10.1 `skill_run_state` — step-level progress checkpoints

DDL (preserve verbatim):
```sql
CREATE TABLE IF NOT EXISTS skill_run_state (
  id TEXT PRIMARY KEY,
  sessionId TEXT NOT NULL,
  skillId TEXT NOT NULL,
  currentStep INTEGER NOT NULL DEFAULT 0,
  stepsJson TEXT NOT NULL DEFAULT '[]',
  status TEXT NOT NULL DEFAULT 'active',
  startedAt INTEGER NOT NULL,
  updatedAt INTEGER NOT NULL,
  completedAt INTEGER
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_skill_run_state_key ON skill_run_state (sessionId, skillId);
```

Record (`SkillRunStateRecord`):

| Column | Type | Notes |
|---|---|---|
| `id` | string | prefix **`rrs_`** + first 8 of a UUID (`rrs_<uuid8>`). |
| `sessionId` | string | — |
| `skillId` | string | — |
| `currentStep` | int | step now active. 0 = not started; rests on final step once done. |
| `stepsJson` | string | JSON array of `SkillStepProgress` (`{index:int, status:"done"|"skipped", notes?:string, ts:int}`). |
| `status` | `SkillRunStatus` | `"active" | "completed" | "abandoned"`. |
| `startedAt` | int (ms epoch) | fixed once. |
| `updatedAt` | int (ms epoch) | advanced on every transition; **always set by the store**, never from a caller patch. |
| `completedAt` | int? | stamped once when status first reaches a terminal state; caller-settable. |

- Natural key `(sessionId, skillId)` is UNIQUE — upsert by it. (Selector caps a
  session at 3 mutually-exclusive skills, so one run per skill suffices.)
- Updatable columns (`SKILL_RUN_UPDATE_COLS`): exactly
  `{currentStep, stepsJson, status, updatedAt, completedAt}`. `id`/`sessionId`/
  `skillId`/`startedAt` are immutable.
- `updateSkillRunState(id, patch)`: forces `updatedAt = now()` (overrides any
  caller value); `completedAt` IS caller-settable.
- `rowToSkillRunState`: SQL NULL `completedAt` → undefined/nil; `stepsJson` stays
  raw JSON.

DB methods to port: `insertSkillRunState`, `getSkillRunState(sessionId, skillId)`,
`listSkillRunStates(sessionId?)` (most-recent-updated first), `updateSkillRunState`.

### 10.2 `skill_selection_log` — selector decision dataset

DDL:
```sql
CREATE TABLE IF NOT EXISTS skill_selection_log (
  id TEXT PRIMARY KEY,
  ts INTEGER NOT NULL,
  sessionId TEXT NOT NULL,
  userInput TEXT NOT NULL,
  selectedSkillIdsJson TEXT NOT NULL,
  confidence REAL NOT NULL,
  taskType TEXT,
  reason TEXT
);
CREATE INDEX IF NOT EXISTS idx_skill_sel_ts ON skill_selection_log (ts);
```

Record (`SkillSelectionLogRecord`):

| Column | Type | Notes |
|---|---|---|
| `id` | string | prefix **`rsl_`** + first 8 of a UUID (`rsl_<uuid8>`). NOTE: prefix is `rsl_`, distinct from run-state's `rrs_`. |
| `ts` | int (ms epoch) | defaults to `Date.now()`. |
| `sessionId` | string | — |
| `userInput` | string | the query, sliced to ≤1000 chars by the caller. |
| `selectedSkillIdsJson` | string | JSON `string[]` of what the query RESOLVED (`newlyKnown`), not the merged set. |
| `confidence` | number (REAL) | from selection. |
| `taskType` | string? | from selection. |
| `reason` | string? | from selection. |

Methods: `insertSkillSelection`, `listSkillSelections(limit=50)` (ts DESC).
Insertion is best-effort (try/catch in the caller).

---

## 11. Model-facing tools (`tools/skillRunTools.ts`)

Four `ToolDef`s. Each has `name`, `description` (model-visible — port verbatim,
they shape model behavior), `risk`, a Zod `schema`, a raw JSON-Schema `parameters`
object (sent to Fireworks; `additionalProperties:false`), and a `handler(args,
ctx)` returning a `ToolResult` envelope (`ok(summary, result?)` / `fail(code,
message, {recoverable})`). Handlers NEVER throw to the caller.

### 11.1 `skill.find` (risk `read`)

- args: `{ query: string (trim, min 1) }`.
- Requires `ctx.findSkills` (wired only for the interactive `main` actor). Absent
  ⇒ `fail("SKILL_FIND_UNAVAILABLE", …, {recoverable:false})`.
- `result = ctx.findSkills(query, ctx.signal)`.
  - `!result.ok` ⇒ `fail("SKILL_FIND_FAILED", "The skill selector was unavailable; no skills were loaded.", {recoverable:true})`.
  - `!result.matched` ⇒ `ok("No skill matched \"<query>\". Use your base operating instructions.", {query, selected:[], activeSkillIds})`.
  - else ⇒ `ok("Loaded <n> skill(s) for \"<query>\": <id (title)>, …. Their full instructions are now in your context.", {query, selected, reason, activeSkillIds})`.

### 11.2 `skill.load` (risk `read`)

- args: `{ skillId: string (trim, min 1) }`.
- Requires `ctx.skillSource`; absent ⇒ `fail("SKILL_SOURCE_UNAVAILABLE", …, {recoverable:false})`.
- `skill = ctx.skillSource.get(skillId)`; nil ⇒ `fail("SKILL_NOT_FOUND", "No skill with id '<id>' is registered. Use tool.search to find a valid skill id.", {recoverable:true})`.
- Requires `ctx.loadSkills`; absent ⇒ `fail("SKILL_LOAD_UNAVAILABLE", …, {recoverable:false})`.
- `activeSkillIds = ctx.loadSkills([skill.id])`; ⇒ `ok("Skill <id> loaded.", {id, title, summary, activeSkillIds})`.

### 11.3 `skill.step.advance` (risk `local`)

- args: `{ skillId(min1), completedStep(int ≥1), nextStep?(int ≥1), status: "done"|"skipped" default "done", notes?:string }`. Required: `skillId`, `completedStep`.
- Needs a session id (`ctx.sessionId` non-blank); absent ⇒ `fail("SKILL_RUN_NO_SESSION", …, {recoverable:false})`.
- Logic:
  - `finished = (nextStep === undefined)`.
  - load existing run by `(sessionId, skillId)`.
  - `steps = upsertStep(parseSteps(existing.stepsJson), {index:completedStep, status, notes, ts:now})`.
  - `currentStep = finished ? completedStep : max(nextStep, existing.currentStep ?? 0)`
    — **clamp so a stale lower-numbered replay can't regress the pointer.**
  - if existing: build patch `{currentStep, stepsJson, status: finished?"completed":"active"}`;
    **only set `completedAt` when `finished`** (= `existing.completedAt ?? now` —
    stamp first finish, preserve original on re-touch). Critically, do NOT include
    a `completedAt:undefined` key on a non-final replay — an enumerable undefined
    would write SQL NULL and wipe the stamp. (In Go: only add the column to the
    update when finishing.)
  - else: insert new `{sessionId, skillId, currentStep, stepsJson, status: finished?"completed":"active", startedAt:now, completedAt: finished?now:undefined}`.
  - return `ok("Skill <id>: step <completedStep> <done|skipped> → <tail> (<n> step(s) recorded).", {state: view})` where tail = `"skill complete"` (finished) or `"step <currentStep> active"`.
  - any error ⇒ `fail("SKILL_STEP_ADVANCE", "Could not record skill step: <msg>")`.
- `upsertStep`: remove any entry with the same `index`, push the new one, re-sort
  ascending by `index`.
- `parseSteps`: parse JSON; non-array ⇒ `[]`; keep only objects with numeric
  `index` and `status ∈ {done, skipped}`; coerce `notes` to string|undefined and
  `ts` to number (default 0). Garbage tolerated → `[]`.
- `toView(rec)`: `{id, sessionId, skillId, currentStep, status, steps: parseSteps(stepsJson), startedAt, updatedAt, completedAt}`.

### 11.4 `skill.run.get` (risk `read`)

- args: `{ skillId(min1) }`.
- Needs session id; absent ⇒ `fail("SKILL_RUN_NO_SESSION", …, {recoverable:false})`.
- No record ⇒ `ok("No checkpoint for skill <id> in this session.", {state:null})` (absence is a normal answer, not an error).
- Else ⇒ `ok("Skill <id>: <status> (<where>).", {state:view})` where `where = status==="completed" ? "complete" : "at step <currentStep>"`.

### 11.5 `ToolContext` skill hooks (wiring)

In `tools/types.ts` / wired in `cli/app.ts`:
- `skillSource?: SkillSource` — the registry (the narrow seam).
- `loadSkills?: (ids[]) => string[]` — `session.loadAdditionalSkills`, **main actor only**.
- `findSkills?: (query, signal?) => Promise<SkillFindResult>` — `session.findSkills`, **main actor only**.
All three are absent for watcher/timer (non-main) actors → tools fail gracefully.
Read `this.session` lazily (buildContext("main") runs during session construction).

---

## 12. `/skills` manual command (CLI)

Two parallel implementations exist: `cli/commandData.ts` (structured, used by the
cockpit) and `cli/commands.ts` (classic REPL render). Port ONE shared handler in
Go (Bubble Tea) producing the same text; the classic-REPL variant can be dropped
(see §14).

Subcommands of `/skills`:

| Invocation | Behavior |
|---|---|
| `/skills` (no sub) | Title `Skills (<n>)`; body = each registry skill `"<id>  [<risk>]  <title> — <summary>"` joined by `\n`, then footer `"\n\n/skills loaded | find <query> | load <id…> | clear"`. |
| `/skills loaded` | `session.describeSkills()` (§7.8). |
| `/skills clear` | `session.setSkills([])`; text `"Cleared loaded skills."`. |
| `/skills load <id…>` | No ids ⇒ usage `"Usage: /skills load <id> [<id>…]"`. Split known/unknown via `registry.has`. No known ⇒ `"No known skill ids given; loaded skills unchanged."` (+ `" Unknown: <…>"` if any). Else `session.setSkills(known)`, prepend `"Unknown id(s) ignored: <…>\n"` if any, then `describeSkills()`. (classic REPL also warns `"More than 3 skills given; loading the first 3."` — the cap is enforced by `resolveKnownIds`.) |
| `/skills find <query>` | Empty ⇒ `"Usage: /skills find <query>"`. Else `res = session.findSkills(query)`; head = `!res.ok ? "Selector unavailable; loaded skills unchanged." : res.matched ? "Loaded for \"<query>\": <ids joined ,>" : "No skill matched \"<query>\"."`; body = `<head>\n<describeSkills()>`. Errors caught → `"Skill find failed: <msg>"`. |
| anything else | usage `"Usage: /skills [loaded|find <query>|load <id…>|clear]"`. |

---

## 13. Concrete Go mapping proposal

Suggested package: `internal/skills` (loader, registry, selector, render, types,
source). The 4 model-facing tools live with the rest of the tool registry
(`internal/tools`), and the session-side merge/message logic stays in the agent
loop package (`internal/agent`).

| TS construct | Go target |
|---|---|
| `Skill`, `SkillMetadata`, `SkillSelection`, `SkillFindResult`, `SkillRisk`, `RenderedSkillBundle` | structs/typed-string in `internal/skills` |
| Zod validation | hand validation func `(*Skill).validate()` returning error; mirror `min(1)`, int/positive, enum checks |
| `SkillSource` | Go interface `{ Has(id) bool; Get(id) (Skill, bool) }` |
| `SkillRegistry` | struct with `map[string]Skill` + insertion-ordered `[]string` for deterministic listing |
| frontmatter parser | hand-rolled parser (NOT `gopkg.in/yaml.v3`) reproducing §2.3-2.4 exactly |
| `loadSkillsFromDir` / embed | `//go:embed skills/*.md` → `embed.FS`; override via env to real FS |
| `selectSkills` | calls the model router's structured-JSON method (`router.JSON("small", req, &SkillSelection{})`) |
| `renderSkillBundle` | `crypto/sha256` + `encoding/hex`, `sort.Slice` by id |
| selection log / run state | the project's SQLite layer (`database/sql` + `modernc.org/sqlite` or `mattn/go-sqlite3`), same DDL |
| UUID id prefixes (`rrs_`, `rsl_`) | `github.com/google/uuid` → `"rrs_" + uuid.NewString()[:8]` |
| JSON marshaling of candidates / steps | `encoding/json` (`MarshalIndent(_, "", "  ")` for candidates) |

Notes:
- Preserve `int64` millisecond epoch timestamps (`time.Now().UnixMilli()`),
  matching TS `Date.now()`.
- Keep `confidence` a float64 (REAL column).
- `notes`/`taskType`/`reason`/`completedAt` are nullable → use `*string`/`*int64`
  or `sql.Null*`.

---

## 14. DELETE / do-not-port list (Node/Bun/React/OpenTUI-specific)

- **`findSkillsDir` package.json walk** — replaced by `go:embed` (keep only the
  `DAINTREE_ASSISTANT_SKILLS_DIR` override + embedded default). `fileURLToPath`/
  `import.meta.url` are Node-only.
- **`node:fs` / `node:path` / `node:crypto` / `node:url` imports** — use Go stdlib.
- **Zod schemas as runtime objects** — port to plain validation functions; do NOT
  pull in a reflection/validation lib to mimic Zod.
- **The `parameters` raw JSON-Schema blobs** are still needed (they go on the wire
  to Fireworks for tool definitions) — KEEP these, but generate/hold them however
  the Go tool registry already does tool schemas; don't hand-duplicate the Zod
  `schema` AND a JSON schema if the Go registry derives one.
- **The duplicate classic-REPL `/skills` handler** (`cli/commands.ts`) — port only
  ONE handler. The Bubble Tea UI replaces both the OpenTUI cockpit and the classic
  readline REPL; keep the structured-result shape from `commandData.ts`.
- **`crypto.randomUUID().slice(0,8)` / `Buffer.byteLength`** — Go equivalents.

---

## 15. Critical invariants checklist (do not regress)

1. Selector sees **headers only** (`SkillMetadata`: 6 fields, no body).
2. Loaded set capped at **3**; cap applied AFTER dropping unknown ids; new ids
   merged FIRST.
3. `messages[2]` is the only message rewritten on skill change; `[0]`/`[1]`
   untouched (prompt-cache stability).
4. Bundle hash = first 12 chars of SHA-256 over `id@version|…` of id-sorted skills.
5. Live Fireworks `prompt_cache_key` is `"daintree-main"` (unversioned), NOT the
   bundle `cacheKey`.
6. `skill.find`/`skill.load` are `risk:"read"` but withheld on autonomous turns
   (in `SKILL_CONTEXT_MUTATING_TOOLS`).
7. Frontmatter parser is a fixed tiny subset — duplicate keys, indented strays,
   and malformed lines all throw with the filename; one bad file aborts the load.
8. `version` stays a string (dotted), `priority`/`maxTurns` coerce to int.
9. `skill.step.advance` never wipes `completedAt` on a non-final replay; clamps
   `currentStep` so stale replays don't regress.
10. All logging/persistence in this subsystem is best-effort (wrapped) and must
    never break a tool call or turn.
