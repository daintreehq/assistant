# Daintree Assistant — internal beta

What this is, what it can and cannot do, and what you are agreeing to by running it.

Read [`FIRST_RUN.md`](FIRST_RUN.md) next; it takes about five minutes.

---

## What it is

Daintree's **local operations officer**. It watches your project, launches and supervises
*visible* coding agents, waits on them intelligently, keeps working in the background, and
reports what actually happened.

**It never edits your files.** That is enforced, not promised: any tool whose name looks
file-mutating is rejected at startup, before the process finishes booting. When a change is
needed it spawns an agent into a Daintree terminal you can watch, and supervises that.

Its natural jobs:

- what's running, what needs attention, what's blocked
- find the worktree, branch, terminal, issue, or PR
- launch an agent to investigate or to change something
- run several agents at once and combine their results
- wait briefly for a short result; hand long work to background supervision
- answer an agent's question and continue
- extract exact values from terminal output
- watch terminals and PRs, schedule reminders
- remember things about the project across sessions
- explain what it did, from the audit trail

## What it is not

- Not a code editor, and not a general shell agent.
- Not a replacement for the agents it coordinates.
- Not an autonomous deployment bot. It asks before mutating, and unattended work needs
  an explicit grant.
- Not useful outside Daintree. Without the Daintree MCP connection it runs in a degraded
  local mode where most of the above simply isn't available.

---

## Supported platforms

| Platform | Interactive use | Background supervision |
| --- | --- | --- |
| macOS (arm64, amd64) | supported | supported |
| Linux (amd64, arm64) | supported | supported |
| Windows | **unsupported** | **unsupported** |

Not "background work stops" — **it does not start at all.** Exactly one process at a time
may own a project's state, and that lease is an `flock`, which has no Windows port. Every
stateful mode takes the lease before doing anything: the attached session, the line REPL,
one-shot, `--json`, `doctor`, and the host. On Windows all of them fail at that step.
Windows is not built or tested in CI, so "it compiles" is not a claim this project makes
either.

---

## What it costs you

**Nothing.** There is no key to supply and no account to fund: the backend holds the
upstream credential and Daintree pays for every model call — turns you type, and also:

- watcher checks (a small model classifying terminal output, on a cadence)
- async supervision waking the assistant when work completes
- utility tasks: summarize, extract, classify, memory distillation, compaction

That list is worth reading anyway, because it is where the assistant spends time as well
as money, and it explains activity you did not ask for. `doctor`'s `upstream credential`
row reports whether the backend's own account can actually fund a turn, and treats "valid
but no credit" as a FAILURE — that state fails every turn, so it should not read as
healthy. If you see it, it is ours to fix, not yours.

---

## Known limitations

**Background work survives closing the *assistant panel*, not closing Daintree** — and
only when the supervisor daemon actually started. Spawning it is deliberately non-fatal,
so a launch where it failed runs solo and silently loses background work on exit;
`daintree-assistant status` tells you which you have. When it is running, it keeps
watching after you close the attached session and hands everything back on the next launch. But it reaches your terminals through Daintree's MCP, so when Daintree
quits, supervision *pauses* — it does not fabricate outcomes, and it does not lose the
work; it publishes a blocked item and resumes on the next launch. Nobody should be told
"I'll have that done overnight" unless Daintree is staying up.

**An OLDER on-disk schema resets local state.** This is pre-release: the SQLite schema is a
single clean baseline rather than a migration chain. On an **interactive** launch a database
stamped at an older baseline is moved aside to a timestamped backup and recreated, and the
path is printed. A non-TTY launch (one-shot, `--json`, host, daemon) fails loudly instead, so
a script never destroys state silently. Conversation history and memories for that project do
not survive. A NEWER on-disk schema (this binary is the one behind — often a stale duplicate
elsewhere on PATH) is the opposite case and never resets anything — see the `state.schema`
entry in [`TROUBLESHOOTING.md`](TROUBLESHOOTING.md): the fix there is updating the binary,
not touching the database.

**Workflow Intelligence is on.** The execution-graph layer is enabled by default now that
the backend advertises the three workflow tasks it needs. Set
`DAINTREE_WORKFLOW_INTELLIGENCE=0` to run without it — worth doing if you are chasing an
ordinary failure and want the smaller surface.

**The model is not always right.** It can misread a terminal, pick the wrong worktree, or
stop early. That is what you are testing — see [`TROUBLESHOOTING.md`](TROUBLESHOOTING.md).
(`DAINTREE_ASSISTANT_DEBUG_LOG=1` records what actually happened, but only for sessions
where it was already on, and its values are redacted and size-capped.)

---

## What we ask of you

1. **Run `doctor` before reporting anything.** Most problems are environment problems, and
   it names them precisely. `doctor --json` if you want to paste something structured.
2. **Report bad behaviour with a `support-bundle`**, not a screenshot and not a debug log.
   See [`SUPPORT_BUNDLE.md`](SUPPORT_BUNDLE.md) for exactly what it contains.
3. **Tell us when it was confidently wrong.** A tool that fails loudly is a small bug. A
   tool that reports success for work it did not do is the one we most need to hear about.
4. **Do not leave `AUTO_APPROVE` on.** It is off by default. When on, mutating actions run
   without asking — and that applies to one-shot, `--json`, and host runs too, not just the
   attached session, because all of them are the same `main` actor. Every surface says so: a badge
   on the attached session's status line and masthead, a line in the line REPL banner, a warning
   on stderr for scripted runs, and a `doctor` check. It does **not** widen the tier gate,
   and unattended actors (watchers, timers, wake turns) still need a scoped grant.

---

## Reference

- [`FIRST_RUN.md`](FIRST_RUN.md) — install, verify, first useful result
- [`PRIVACY_AND_DATA.md`](PRIVACY_AND_DATA.md) — what leaves your machine, what is stored, how to delete it
- [`TROUBLESHOOTING.md`](TROUBLESHOOTING.md) — a doctor-driven decision tree
- [`SUPPORT_BUNDLE.md`](SUPPORT_BUNDLE.md) — what a bundle contains and what it deliberately omits
- [`../generated/TOOLS.md`](../generated/TOOLS.md) — every tool, with its risk and confirmation behaviour
- [`../generated/COMMANDS.md`](../generated/COMMANDS.md) — every slash command
- [`../../SECURITY.md`](../../SECURITY.md) — reporting a leaked secret or an unsafe action
