# Daintree Assistant

> ❗ **Internal testing only, for now.** Pre-release software under active development.
> Anyone is welcome to use it, but expect **breaking changes** — to the backend wire
> protocol, the on-disk schema, and how authentication works. An update may mean
> re-running install and losing local state.

A single native Go binary: the **orchestration engine for
[Daintree](https://github.com/daintreehq/daintree)**. It plans Daintree operations, spawns
and supervises visible agent terminals, watches them with cheap models, schedules timers,
and keeps your main conversation clean.

**It is headless.** Daintree embeds this binary over `host --stdio` and renders the whole
conversation natively in its own UI — there is no terminal interface to speak of here. The
line REPL that remains is an operator convenience for a shell or an SSH session, not a
product surface.

**It is not a code editor and never edits your files.** When a change is needed it spawns
a *visible* agent in a worktree and supervises it.

Every model call takes one path — CLI → Daintree Assistant backend → the upstream
provider → the selected model. The CLI holds no credential at all: the backend supplies
the upstream key and owns the system prompt, skill selection, and model choice. See
[`docs/BACKEND.md`](docs/BACKEND.md).

## Supported platforms

macOS (arm64, amd64) and Linux (amd64, arm64).

**Windows does not run** — and is not merely untested. Exactly one process at a time may
own a project's `state.db`, and that lease is an `flock`, which has no Windows port. Every
stateful mode takes the lease before doing anything, so all of them fail there.

## Installing

Two steps, and nothing else — no `npm`, no `node`, no native toolchain. The result is one
self-contained ~24 MB binary; nothing is fetched at run time.

**1. Install Go.** You need **1.21 or newer** — *not* 1.25.13. Go 1.21+ reads this module's
`go 1.25.13` directive and downloads the matching toolchain itself. Go is a build-time
requirement only; the binary it produces does not depend on it.

```bash
brew install go                      # macOS
sudo snap install go --classic       # Linux (distro packages are often too old)
go version                           # want go1.21 or newer
```

No Homebrew or snap? Take the official package from <https://go.dev/dl/>.

**2. Install and verify.**

```bash
go install github.com/daintreehq/assistant/cmd/daintree-assistant@latest
daintree-assistant doctor     # read it top to bottom; you want no FAIL lines
```

**There is no sign-in and no API key to paste.** The backend holds the upstream
credential and Daintree pays for the model calls, so the binary works as soon as it is on
your `PATH`. Account sign-in is being built; when it arrives, this section grows a step.

One thing that trips people up: `go install` writes to `$(go env GOPATH)/bin` (usually
`~/go/bin`), **which is not on `PATH` by default on macOS** — so the install succeeds and
the command is then not found. Fix it with
`echo 'export PATH="$(go env GOPATH)/bin:$PATH"' >> ~/.zshrc && exec zsh`.

## Updating

Updating is a full reinstall — the same one command:

```bash
go install github.com/daintreehq/assistant/cmd/daintree-assistant@latest
```

**You never update Go.** Any 1.21+ downloads whatever toolchain this project requires by
itself, so a Go install you set up once keeps working.

There is no self-update command and nothing tells you when you are behind, which matters
because the backend deploys independently and the skew failures are hard rather than
graceful: a protocol mismatch answers HTTP 426 and the turn cannot run, and an outdated
on-disk schema is refused. So re-run the install command whenever a backend or schema
change is announced, then run `doctor`.

## Running it

```bash
daintree-assistant host --stdio             # THE embedding path — Daintree drives this
daintree-assistant "which worktrees are ready?"   # one-shot, prints, exits
daintree-assistant --json "…"               # one-shot, JSONL events to stdout
daintree-assistant --json --multi-turn      # a whole conversation, one JSONL transcript
daintree-assistant mcp --stdio              # the assistant AS an MCP server, for another agent
daintree-assistant                          # line REPL (operator convenience)
daintree-assistant doctor                   # environment check
daintree-assistant status                   # supervisor health and live work
daintree-assistant support-bundle           # redacted diagnostics to send a maintainer
```

The line REPL writes plain lines to the normal screen buffer — no alternate screen, no
raw mode, no mouse capture — so the host terminal keeps native scrolling, selection, and
copy-paste. `/help` lists the slash commands. `--classic` is accepted as a deprecated
no-op; the REPL is the only interactive mode.

**`doctor` is the gate.** It diagnoses the install, the backend (including whether its
upstream credential can actually fund a turn — "no credit left" is its own verdict), and
both MCP connections, and exits non-zero only on a real failure. Start there when
anything is wrong.

## Inside Daintree

Daintree launches the CLI and injects the MCP connection via `DAINTREE_MCP_URL`,
`DAINTREE_MCP_TOKEN`, and `DAINTREE_PROJECT_ID`. Without them it runs in **degraded local
mode**, where its whole orchestration role is offline: file reads, memory, timers and the
audit trail still work, but nothing that reaches a terminal, agent, or worktree does.

## Contributing

```bash
git clone https://github.com/daintreehq/assistant && cd assistant
make build          # → ./bin/daintree-assistant
make install        # → /opt/homebrew/bin or /usr/local/bin
go test ./...       # no network — fakes for MCP and the backend
go vet ./... && gofmt -l .
```

`make install` forces `GOBIN` so it cannot leave a second copy behind. Daintree finds the
CLI by a `PATH` lookup, so a stale copy earlier on `PATH` silently wins — and the symptom
is not "wrong version" but a feature that mysteriously does not exist. `doctor` lists
every copy it finds.

## Documentation

| Testing it | |
| --- | --- |
| [`docs/beta/FIRST_RUN.md`](docs/beta/FIRST_RUN.md) | Install → sign in → first result |
| [`docs/beta/TROUBLESHOOTING.md`](docs/beta/TROUBLESHOOTING.md) | A decision tree keyed to `doctor`'s check ids |
| [`docs/beta/PRIVACY_AND_DATA.md`](docs/beta/PRIVACY_AND_DATA.md) | What leaves your machine |
| [`docs/beta/INTERNAL_BETA.md`](docs/beta/INTERNAL_BETA.md) | Scope, limitations, what it costs you |

| Working on it | |
| --- | --- |
| [`docs/BACKEND.md`](docs/BACKEND.md) | The model / skill / prompt story — **start here** |
| [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) | How the pieces fit |
| [`docs/SUPERVISOR.md`](docs/SUPERVISOR.md) | The persistent daemon |
| [`docs/TOOLS.md`](docs/TOOLS.md) | Adding a tool |
| [`docs/LOGGING.md`](docs/LOGGING.md) | The debug-log event reference |
| [`docs/generated/`](docs/generated/) | Generated from the live registry: tools, commands, compatibility |

## License

Apache 2.0. See [LICENSE](LICENSE).
