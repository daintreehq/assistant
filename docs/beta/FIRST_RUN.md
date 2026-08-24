# First run

Five minutes from nothing to a useful answer. No `.env` file to edit.

---

## 1. Get the binary onto your PATH

Download the archive for your platform from
[Releases](https://github.com/daintreehq/assistant/releases), verify it, and put the
binary somewhere on your PATH:

```bash
sha256sum -c SHA256SUMS          # macOS: shasum -a 256 -c SHA256SUMS
tar xzf daintree-assistant_<version>_darwin_arm64.tar.gz
sudo mv daintree-assistant_<version>_darwin_arm64/daintree-assistant /opt/homebrew/bin/
```

The builds are **not code-signed**, so macOS will warn about an unidentified developer.
Clear it with `xattr -d com.apple.quarantine /opt/homebrew/bin/daintree-assistant` if you
trust the build — or use the source path below, which Gatekeeper does not question.

**From source** (needs Go 1.25.13+, `git`, and `make`):

```bash
git clone https://github.com/daintreehq/assistant
cd assistant
make install      # → /opt/homebrew/bin (Apple Silicon) or /usr/local/bin
```

**Daintree finds the CLI by name**, so an older copy earlier on your PATH silently wins —
and the symptom is not "wrong version", it is a feature that mysteriously doesn't exist.
`doctor` lists every copy it finds and what each one reports.

No native toolchain and no npm either way: SQLite is the pure-Go driver, so the binary is
self-contained. `make install` forces `GOBIN` so *this* invocation cannot add a second
copy — it cannot remove one that is already there, which is what `doctor` finds.

---

## 2. There is nothing to sign in to

No key, no login, no account. The Daintree Assistant backend holds its own upstream
credential and **Daintree pays for the model calls** — including the background ones
(watcher checks, async completions, summarize/extract/classify) that happen while you are
not looking.

No model-provider credential is stored on your machine, and nothing reaches a model
provider from it directly: the CLI sends its requests to the backend with no
`Authorization` header, and the backend does the rest. The CLI does store project-scoped
conversation and operational state locally (`state.db` under `~/.daintree/assistant-cli/`)
so sessions, memories, supervision, audit, and recovery can work — see
[`PRIVACY_AND_DATA.md`](PRIVACY_AND_DATA.md) for exactly what and for how long. Account
sign-in is being built; when it lands, this page grows a step.

---

## 3. Check the environment

```bash
daintree-assistant doctor
```

Read it top to bottom. Every line is one condition with one next action. You want no
`FAIL` lines. `WARN` lines are worth understanding but will not stop you:

- **`Daintree MCP  not configured`** — expected when you run from a plain terminal. Launch
  from inside Daintree to get the real thing. Without it, the assistant cannot see
  terminals, agents, or worktrees, which is most of what it is for.
- **`binary on PATH  2 copies`** — remove the one you are not using.
- **`state dir … readable by other users`** — run the `chmod` it prints.

---

## 4. Launch it from inside Daintree

Open the Assistant panel. Daintree injects the MCP connection; the assistant picks it up.

The attached session renders **inline**, in your terminal's normal screen buffer. Scrolling,
selection, and copy/paste are your terminal's, not ours.

---

## 5. First useful things to ask

Start read-only. These mutate nothing:

> Give me a concise status of everything running in this project. Don't start or close anything.

> What needs my attention right now?

> Explain this project and its current worktrees.

Then try delegating something small:

> Spawn an explore-mode agent in <worktree> to find where <thing> is handled. Don't edit anything — just report what it finds.

You will see it spawn a **visible** terminal, supervise it, and report back. If the work is
short it waits; if it is long it hands off to background supervision and tells you so.

---

## What to expect the first time

**It asks before mutating.** Terminal, project, external, git, and system actions all
confirm; git and system need a typed phrase, not a keypress. That is the design, not
friction to be switched off.

**Long work goes to the background.** When it says it will tell you when something is
done, that survives closing the *assistant panel*. It does **not** survive quitting
Daintree — supervision reaches your terminals through Daintree. Read
[`INTERNAL_BETA.md`](INTERNAL_BETA.md#known-limitations) before relying on it overnight.

**A fresh session looks clean but is not amnesiac.** The transcript starts empty; adopted
watchers keep running, the attention inbox persists, and a one-time "While you were away"
note summarises what the supervisor did while you were gone.

---

## When something goes wrong

```bash
daintree-assistant doctor          # start here, always
daintree-assistant support-bundle  # then this, to send us something
```

See [`TROUBLESHOOTING.md`](TROUBLESHOOTING.md). Please don't send debug logs — they contain
your whole conversation. The bundle exists so you don't have to.
