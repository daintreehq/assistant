# Privacy and data

What leaves your machine, what stays on it, and how to get rid of any of it.

---

## What leaves your machine

Two separate destinations, not one:

```
model traffic   you → CLI → Daintree Assistant backend → OpenRouter → the selected model
Daintree        you → CLI → the local Daintree MCP (terminals, agents, worktrees)
```

The Daintree MCP is local to your machine. Documentation lookups for questions about
Daintree itself are performed by the backend, not this CLI, so the public docs server
sees the backend's search query rather than any connection from you.

### To the Assistant backend, on every turn

| Sent | Notes |
| --- | --- |
| Your messages and the assistant's replies | The visible conversation, in full |
| A stable project snapshot | Project identity, worktrees, agent roster |
| `DAINTREE.md` | Your project instructions, verbatim, up to 16 KiB |
| Runtime and turn context | Tier, MCP status, open terminals, live async work, pinned memories |
| The tool inventory | Names and JSON schemas — not results |
| Tool **results** you ran | Terminal output, file excerpts, issue and PR bodies — whatever the tool returned |
| Utility-task payloads | Text to summarize, extract from, classify, or distil into a memory |

That last pair is the one worth pausing on. **When the assistant reads a terminal, that
output goes to the model.** When it reads a file, that file's contents go to the model.
That is how it does its job, and it is not avoidable — but it means the boundary is *what
you ask it to look at*.

**This is cumulative.** The conversation is resent each round, so anything a tool read
earlier — terminal output, a file, an issue body — keeps travelling with it until the
conversation is compacted or cleared. Recalled and pinned **memories** ride along too.

### What stays here

- Your API key, beyond the single hop above (bearer token, request-scoped).
- The Daintree MCP token.
- Files no tool read on your behalf.
- The debug log and the audit trail — local, unless you send a bundle.

### What this document cannot promise

Everything above describes **this client**: what it sends, and when. What the backend does
with it — retention, staff access, whether OpenRouter is asked not to train on it — is a
property of the service, not of this code, and cannot be established by reading this
repository. Ask for the backend's own data policy; do not take a client-side README as
one.

Concretely: **a maintainer operates the backend your conversation passes through.** Not
sending a support bundle does not make your conversation private from them.

---

## What is stored on your machine

Under `~/.daintree/assistant-cli/` (per project when Daintree supplies a project id):

Each row keeps the **newer of** its age window or its last-N records — so a busy project
loses old rows sooner than the age suggests, and a quiet one keeps them for the full
window.

| Data | Where | Kept |
| --- | --- | --- |
| Conversation history | `state.db` | 90 days / 1,000 rows |
| Memories | `state.db` | Until you forget them. A forgotten memory is soft-deleted and hard-removed 30 days later; a TTL-expired one stops being recalled but is not hard-deleted |
| Audit trail (every tool call) | `state.db` | 30 days / 5,000 rows |
| Run events (for `/explain`) | `state.db` | 14 days / 500 runs |
| Watchers, timers, workflows | `state.db` | Until finished or cancelled — completed rows are **not** currently garbage-collected |
| Async invocations | `state.db` | 7 days after finishing |
| Artifacts (oversized results, archived transcripts) | `state.db` | 90 days / 1,000 rows — deliberately the same window as the conversation, so a transcript stub can never outlive the payload it points at |
| Attention inbox | `state.db` | Until resolved (7 days once terminal) |

**No credential is stored at all.** There is no sign-in: the backend holds the upstream
key and a request from the CLI carries no `Authorization` header.

And under `~/.daintree/logs/`, only when `DAINTREE_ASSISTANT_DEBUG_LOG=1`:

| Data | Kept |
| --- | --- |
| Full session trace — model requests, tool calls with args and results, watcher lifecycle | Pruned past 7 days **at the next debug-logging launch** — not on a timer, so logs from a machine you stopped using stay until you run it again with tracing on |

A state directory this CLI creates is 0700, and debug logs and support bundles are
written 0600. An **existing** directory's permissions are left as they are —
`doctor` reports both, with the exact `chmod`, rather than silently changing something it
did not create.

---

## Redaction, and its limits

Credentials are stripped before anything is written to the **debug log**, the **audit
rows**, the **run events**, the **console/JSONL output**, and the **attached session's activity
rows**. Two layers: recognisable shapes (bearer tokens, `sk-` keys, PATs, JWTs, PEM
blocks, `export API_KEY=…`, URL userinfo), plus this process's own MCP token by exact
value — and `DAINTREE_API_KEY` too, on the rare install that sets one.

**Redaction covers tool activity, not prose.** Tool call arguments and results are
scrubbed at the event source, which is what feeds the log, the audit rows, run events, the
console and JSONL sinks, and the attached session's activity rows. Your messages, the assistant's
replies, and its reasoning are stored and displayed **verbatim** — so if the model repeats
a credential it read, that is not caught.

**Four things are deliberately NOT scrubbed**, because scrubbing them would break the
product:

- **The conversation.** The model must see exactly what the terminal printed. A redacted
  transcript would make the assistant unable to relay a value you legitimately asked it to
  read, and would corrupt a resumed session.
- **Artifacts** — archived oversized results and pre-compaction transcripts. Same reason:
  they are the conversation's overflow.
- **Timer payloads and async commands.** These are scheduled *work*; the stored form is
  replayed to execute it later, so redacting it would break the job.
- **Prose.** If the model repeats a credential it read, no metadata-level scrubbing can
  prevent that.

These are protected by **scope**, not by scrubbing: they live in owner-only local files,
and no command copies them anywhere by default. That is a weaker guarantee than
"unreadable", and it is the honest one. It is also exactly why `support-bundle` is a
separate, narrow artifact rather than "zip up the state directory".

---

## Deleting things

| To remove | Command |
| --- | --- |
| One memory | `/memory forget <id>` |
| The current conversation (and cancel watchers/async) | `/clear` |
| This project's conversation and supervision state | `daintree-assistant reset project-state` |
| Everything this CLI has written for this project | `daintree-assistant reset all-data` |
| Debug logs | `rm -rf ~/.daintree/logs` — or wherever `DAINTREE_ASSISTANT_LOG_DIR` points |

Every `reset` scope stops the daemon, takes the owner lease, and shows you exactly what it
will remove and what survives. It writes a timestamped backup first unless you pass
`--no-backup`. It refuses to run
against a directory that does not look like an assistant state directory — so a mistyped
`DAINTREE_ASSISTANT_STATE_DIR` cannot turn it on your repository.

---

## Exporting

```bash
daintree-assistant support-bundle --include-audit   # redacted, for support
```

For your own records, the `audit.export` tool writes the full audit trail as JSON or CSV
from inside a session. `/audit export json` does the same from the attached session.

---

## Questions this should have answered

**Does my code get uploaded?** Only the parts a tool read on your behalf — a file you
asked about, terminal output from an agent you launched, a diff you asked it to check.
Never a bulk upload, and never anything it was not asked to look at.

**Can a maintainer read my conversation?** Your conversation passes through a
maintainer-operated backend, so treat it as visible to whoever operates that service — ask
them for their retention and access policy. What is true here: a **support bundle contains
none of it**, and we will not ask for a debug log as a first step.

**Is my key stored anywhere but my machine?** This client stores it in one 0600 file and
sends it as a per-request bearer token. Whether the backend persists it is a property of
the backend; its policy is the authority, not this page.

**What if I think a secret leaked?** Rotate it first, then see
[`../../SECURITY.md`](../../SECURITY.md). Rotating costs a minute; not rotating can cost a
lot more.
