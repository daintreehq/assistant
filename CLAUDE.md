# CLAUDE.md

Guidance for working in this repository.

## What this is

> **Active development, pre-release.** Nothing is shipped yet. Don't preserve
> backward compatibility or version stable surfaces for their own sake — prefer
> the simplest thing. We deliberately do NOT version the system prompt (the
> cache key is a plain, unversioned identifier); just edit the prompt directly.
> The SQLite schema is a single clean baseline (`schemaUserVersion`, currently 12) — on a
> schema change, hard-reset the DB (`make db-reset`, which wipes the resolved
> state dir, honouring `DAINTREE_ASSISTANT_STATE_DIR`) rather than accumulate a
> migration chain.

`github.com/daintreehq/assistant` — a single native **Go** binary, a local
CLI **orchestration assistant for Daintree** ("Daintree's local operations officer").
It plans Daintree operations, spawns and supervises visible agent terminals, watches
them with cheap models, schedules timers, and keeps the human's main conversation clean.

**It is NOT a code editor and never edits project files.** When a change is needed
it spawns a *visible* Daintree agent in a worktree (`agentTask.spawnForEdits`) and
supervises it. This is a hard, enforced invariant (see below) — don't add a tool
that writes/edits files.

The **Daintree project itself** lives at `../daintree` (`~/Projects/Daintree/daintree`)
and on GitHub at <https://github.com/daintreehq/daintree>.

Powered by the **Daintree Assistant backend** (`../assistant-backend`,
`~/Projects/Daintree/assistant-backend`, on GitHub at
<https://github.com/daintreehq/assistant-backend>), a Daintree-native
HTTP API — **not** OpenAI-compatible. The CLI is a thin local runtime: it sends only a
structured stable startup snapshot + visible conversation + structured runtime/turn context + its tool inventory, and the
backend owns the system prompt, developer instructions, **runbook selection**, model
choice, prompt assembly, and the utility-model prompts. The CLI executes the local
tool calls the backend asks for and streams the assistant's text. See `docs/BACKEND.md`.

**The backend owns the upstream PROVIDER credential, and the CLI has none.** Every model
call — main and utility alike — is funded by a key the SERVER holds; the CLI ships no
provider credential and asks the user for none. That is separate from the ACCOUNT
credential below, which says who is calling and not what pays. Model identities that appear in this repo's
comments are the backend's upstream route ids, not direct provider integrations; where a
comment names model-specific protocol behaviour, read it as "that model's behaviour when
reached through the backend's upstream". There is no provider API key anywhere in this
process.

> **You have standing permission to edit the backend at `../assistant-backend`.** Many
> fixes here are really backend changes — the base/system prompt, developer instructions,
> runbook bodies, and the utility-model prompts all live in that repo
> (`src/daintree_assistant_server/prompts/` and `.../runbooks/files/*.md`). When a model
> behaviour, formatting, or runbook bug traces to the prompt/runbook rather than a local tool
> shape, fix it directly in `../assistant-backend` (prompt/runbook changes land there; local
> tool-shape changes land here) — no need to ask first.

**Endpoint.** The default endpoint is the deployed backend,
`https://assistant.daintree.org` (`backend.DefaultBaseURL`); `backend.LocalBaseURL` is
the local one (`http://127.0.0.1:8473`) you get by running `../assistant-backend`
(`python -m daintree_assistant_server`). Three ways to choose, highest first:
`--backend-url` → `DAINTREE_BACKEND_URL` (trusted env) → the endpoint **stored by
`/backend`** (`internal/config/endpoint.go`, a 0600 `endpoint.json` at the per-user state
root, holding ONLY `{backend_url}` — it is a preference, never a credential) → the
default. `/backend` with no argument reports the resolved endpoint; choosing between
candidates is a HOST concern now (Daintree renders the picker), reusing the same
question channel the model's `user.askMultipleChoice` uses — `pendingQuestion.local`
marks it user-opened so Esc
dismisses instead of cancelling the turn, and nothing blocks on a reply channel. With a
target (`local`, `official`, a number, or a URL) it swaps the `Swappable` in place AND
persists; `/backend default` forgets. The classic REPL, which has no sheet, prints the
list instead. Env deliberately outranks the stored choice so a harness or
CI is never silently redirected — and because that would otherwise look like a broken
feature, `cfg.BackendURLPinnedByEnv` makes `/backend` say so.

**A remote endpoint's plaintext HTTP is refused by default.** `LoadConfig` refuses a non-loopback `http://`
backend URL by default — a normal turn carries the whole conversation, terminal
output, file excerpts, and tool results, and that crossing the wire in the clear
is a confidentiality failure whether or not a bearer is set. Loopback stays
permitted unconditionally (there is no network to intercept). The escape hatch is
`--allow-insecure-backend` / `DAINTREE_ALLOW_INSECURE_BACKEND=1` (trusted-env only,
propagated to a spawned daemon); a STORED `/backend` preference that fails this
check degrades to the default instead of bricking the launch (same contract as an
unreadable stored preference), surfaced via `cfg.EndpointInsecureRejected`. See
`backend.ValidatePlaintextRemote` / `internal/app/backendswitch.go`'s own
independent check on the interactive `/backend <url>` path (no escape hatch there).

**Accounts exist, and whether one is REQUIRED is the deployment's answer, not this
build's** — the deployed backend's configuration answers no today (`AUTH_MODE` defaults
to `open` and the deployment sets no Supabase values). `internal/auth` is the CLI's account authority: OAuth Authorization Code +
PKCE against Supabase, the refresh token in the OS credential store and nowhere else, the
access token in process memory and nowhere else, refresh under a cross-process lock, and
one typed state machine every surface renders (`auth login` / `status` / `logout` /
`disconnect`). Auth state is per USER, at the state ROOT — so one login covers every
project sharing that root, which an explicit `--state-dir` deliberately does not.

The deployment decides whether any of it applies. A backend answers
`GET /v1/daintree/auth/config` with a manifest carrying `configured` and `required`, and
a deployment with no identity provider returns exactly four fields — `version`,
`environment`, `configured`, `required` — and no issuer, client id, endpoints or links: a
CORRECT response that fails manifest validation by design. Requests then go out with no
`Authorization` header at all and the backend serves them, which is what every install
does today. `auth.Availability` carries that answer through to status, and it is the one
thing that must never be rendered as a session or as an outage: see
`Status.WithAvailability`, where a known `configured:false` overrides the credential
state, because the credential store knows nothing about the deployment.

**The backend's answer about the account reaches the state machine.** A protected 2xx
confirms the session; an expired credential is refreshed and the request replayed once,
and only before anything visible has been streamed (a preamble counts); a revocation
deletes the stored credential and bumps the shared revision so another process notices;
plan and dependency failures preserve it. (Cleanup is best-effort: the delete, the
descriptor removal and the revision bump are each attempted and their errors ignored, so
the local state is authoritative and the stored copy may briefly outlive it.) The seam is
`backend.AccountObserver`,
implemented by `*auth.Manager` and held on `App.Auth` — ONE per process, shared with the
supervisor's spend gate, because a verdict landing on an object the gate never reads is
the bug that shape exists to prevent. Verdicts arrive late, so every write rechecks the
identity generation inside the section that performs it.

**The account answer reaches every CLI surface — the `auth` commands, the embedded
`/login` `/logout` `/account`, and a turn's prose — but not yet the typed host protocol.**
`backend.Account(ctx)` reads `GET /v1/daintree/account` under a validated version-1
contract (`internal/backend/accountstatus.go`); a malformed body raises a LOCAL
`account_contract_invalid` that is deliberately outside `accountCodes`, so it can never
read as signed-out or unsubscribed. `auth.Manager` holds the answer in a memory-only
snapshot stamped with the generation AND the shared revision marker
(`internal/auth/accountsnapshot.go`) — never on disk, because a plan on disk is a plan
that can be wrong. A turn renders twelve of the thirteen account codes through
`agent/session.go` `accountFailureAdvice`, whose replies open with the registered
`Account problem:` wake prefix — `account_rate_limited` is the exception and keeps the
ordinary rate-limit reply.

**Version 1 is TWO documents, not one with optional parts** — the fork in
`AccountStatus.validate` is the shape of the contract, not a convenience. Every response
carries `version`, `access` and a valid `subject_hash` (the route is protected, so there is
always a subject to hash), and may carry `email`. Beyond that they diverge:

- **`unverified`** says identity is established and entitlement was never looked up, so it
  carries none of the lookup's results. Demanding a `checked_at` from it — as this decoder
  once did — turns the backend's correct answer into "could not verify your plan".
- **The three checked verdicts** report a completed lookup and must carry `checked_at`
  (RFC 3339 with an offset, and not the zero time, which no consumer can tell apart from
  no check at all), `entitlement_source` AND `entitlement_stale`. `granted` additionally
  requires a `plan_id`; the two subscription verdicts may omit plan and status.

`entitlement_stale` is decoded through a POINTER because an absent flag reads as `false`
through `Stale()` and would render "we checked, and this is current" for a response that
claimed nothing. It is the ONLY field whose wire presence is preserved: a `null` or `""`
string decodes the same as an omission, so the `unverified` rules are enforced on the
decoded value, not on literal absence. The decoder is deliberately NOT a mirror of the
server's — it ignores unknown fields where the server forbids extras, and it does not
re-check every cross-field rule the server already enforces (`source=cache` with
`stale=false`, for one). It is the subset that decides what a user is told. Bounds are
WIDER than the server's where the two count in different units: the backend bounds `email`
in code points and this counts bytes, so a 320-byte cap would refuse a server-valid
accented address. The canonical wire bodies live once, in
`internal/backend/accountfixture`, and are decoded by the `backend`, `auth`, `cli` and
`commands` suites alike — four hand-written copies of one response is how both ends of a
contract come to be green against documents that do not match.

**One account read serves every surface** (`internal/app/accountrefresh.go`):
`App.RefreshAccount` for a session, `RefreshAccountWith` for the standalone `auth`
subcommands, which run in a process with no App at all. `auth status --refresh` and
`/account` read as the user asking — OBSERVING, so a revocation clears the credential;
the checks after `auth login` and `/login` are a COURTESY and run unobserving, because a
plan report has no business revoking a session minted seconds ago. A plain `auth status`
never asks. The App path fetches outside `cfgMu` and commits under it, so an answer for an
endpoint a `/backend` switch has left is never applied — and `ApplyAccountStatus` REPORTS
whether it committed, because it declines silently whenever the identity moved and "no
error" was being read as "applied". `AccountRefresh.Discarded` covers BOTH causes — the
endpoint moved, or a login/logout/revocation moved the generation — and the surfaces
render it as "the account changed while this was being checked", never as a plan verdict.

Three rules in that layer are load-bearing and easy to undo:

- The account endpoint's own 200 must not confirm the session
  (`accountAttempt.deferSuccess`). It answers 200 for a caller with NO plan, so the
  transport's "a protected request succeeded, therefore this session is active"
  inference is exactly wrong there.
- `MarkActive` must not overwrite a subscription verdict. Most protected endpoints
  answer 2xx regardless of entitlement — capabilities does, and it runs at boot.
- The post-login plan check runs through a NON-observing client
  (`app.NewUnobservingAccountBackendClient`). The observing one acts on what it hears,
  and a revoked verdict reaches `RemedyClear`, which would delete the refresh token
  seconds after login persisted it.
- **`Login` publishes its new identity BEFORE releasing the credential lock, and a
  clearer consults the SHARED REVISION after taking it.** Both halves guard the same
  failure — a late revocation deleting the credential a login has just stored, while the
  login reports success and `Persisted: true`. The first closes a window inside one
  process; the second closes the wider one across processes, where a clearer's own
  generation cannot see a login that happened elsewhere and stays wrong until its next
  `AccessToken`. Declining a genuine revocation is the safe error here: the unobserved
  marker makes the next `AccessToken` drop the cached access token and re-resolve the
  current credential anyway, whereas deleting the wrong one has no recovery. `internal/auth/relogin_test.go` pins both, through the
  `loginAfterCredentialUnlock` seam — the windows are too narrow for a stress test to
  reach, which was verified rather than assumed.

**What is still missing:** the host protocol still emits a generic `turn-error` for
account failures rather than a typed account event. Account STATE now reaches an attached
session through the `/account` card rather than a typed event, which is a command result
a host renders as text — a native panel still has no structured account state to bind to.

Two further seams are load-bearing and must stay:

- **`DAINTREE_API_KEY`** still resolves into `cfg.APIKey` (trusted env ONLY — a project
  `.env` may supply neither it nor the URL, since one steals a spendable credential and
  the other redirects where it is sent), as does **`--api-key-file PATH`** for a headless
  caller (a path, never `--api-key`: argv is world-readable through `ps`). When set, the
  client sends it as the bearer. What the backend does with it is NOT "spend it instead":
  in `open` mode it is not read at all, and in `observe`/`enforce` it is interpreted as an
  ACCOUNT token, never a provider key. Every model call is funded by the server's own
  credential either way (`serving_api_key`), so this variable changes who is CALLING, not
  who pays. Nothing sets either on a normal install; they stay live, with the header, the
  shape check and `backend.ScrubKey`, so a per-account credential later becomes a VALUE
  flowing through existing plumbing rather than new plumbing. A NAMED key that cannot be
  read is fatal, never a fallback — falling through would run the turn as a DIFFERENT
  principal (anonymous, or the managed sign-in) behind a successful-looking run, and on a
  deployment that meters by account it would consume the wrong one's entitlement.
- **`App.Backend` is always a `backend.Swappable`.** Every consumer holds the wrapper, so
  a client rebuild reaches Session, watchers, asyncwork and the workflow layer without
  re-wiring. `/backend` swaps it (`internal/app/backendswitch.go`), rebuilding the client
  and the account manager together — a credential minted for one deployment must never be
  presented to another. An ordinary token refresh does NOT come through here; TokenSource
  changes the credential for the same endpoint, one level below.

`POST /v1/daintree/auth/verify` answers for whichever key the request WOULD spend — the
backend's own, on every install — so it is the one probe that can say "this deployment
can actually run a turn" before a turn is spent finding out. `doctor` is its only caller
(`upstream credential` row), and it branches on the stable `reason`, never on the prose
`detail`. The CLI must never probe a provider itself. That row always attributes the
credential to the DEPLOYMENT: a caller-supplied bearer is an account token that is never
sent upstream, so describing the verified credential as the user's — which the copy used
to do whenever one was set — sends them to replace a value that could not be the cause.

**The OAuth shape is fixed and pinned.** The callback is exactly
`http://127.0.0.1:42813/oauth/callback` — Supabase matches redirect URIs exactly, so
binding a different port would produce a `redirect_uri` the provider has never seen. A
REMOTE issuer must sit under `supabase.co` or `daintree.org`, and remote browser links
under `daintree.org` (literal loopback is exempt, which is the local-development shape),
both matched on a LABEL boundary (`notdaintree.org` and
`daintree.org.evil.example` are refused). Manifest validation takes the expected redirect
and NOTHING else — no backend, no origin — which is what lets `assistant.daintree.org`
serve a manifest saying `environment: staging` with links on `staging.daintree.org`
without `staging.daintree.org` ever being hardcoded as a backend.

**The refresh token is the only account secret that persists, and the OS keychain is
the only place it goes.** macOS Keychain, Linux Secret Service. The access token is never
persisted at all. Neither is written to `state.db` or any file, passed in argv, put in an
environment variable, or sent over the daemon's control socket — the state root holds only
`auth/credential.json` (a non-secret descriptor naming WHICH credential), the revision
marker and lock files. Where no credential service is reachable — a headless box with no
session bus, a locked store — there is no persistence at all: `auth status` reports
`credentials  this process only`, and since `auth login` is itself a short-lived process,
a login on that tier is gone the moment the command exits.

**`logout` and `disconnect` are different operations, and neither revokes anything
remotely.** `logout` deletes the credential from THIS machine and bumps the shared
revision so a running daemon stops spending; there is no server-side sign-out call, which
is why `Manager.Logout` returns `revokedRemotely` and it is always false. `disconnect`
does even less: it validates and PRINTS the account-page URL (after confirming, since the
action it points at affects every device) and opens no browser. Whether anything is
revoked depends on what the user does on that page, and the CLI cannot confirm it.

## Commands

Go ≥ 1.25.13. No npm / bun / node — this is a single static binary.

```bash
# Compile & install
go build -o bin/daintree-assistant ./cmd/daintree-assistant   # local binary → ./bin
go install ./cmd/daintree-assistant                           # → $(go env GOBIN) or $(go env GOPATH)/bin
make build                                                    # trimpath + version ldflags → ./bin
make install                                                  # go install with the same ldflags

# Run
./bin/daintree-assistant host --stdio        # THE embedding path — Daintree drives this
./bin/daintree-assistant --classic           # classic line REPL (also used for non-TTY)
./bin/daintree-assistant "which worktrees are ready?"   # one-shot, prints, exits
./bin/daintree-assistant --json "…"          # one-shot, JSONL events to stdout
./bin/daintree-assistant doctor              # environment gate; `doctor --json` for the structured form
./bin/daintree-assistant support-bundle      # redacted diagnostics archive to send to a maintainer
./bin/daintree-assistant reset <scope>       # project-state | all-data (lease-aware, backs up)
./bin/daintree-assistant host --stdio        # embedded host: stdio NDJSON, PROTOCOL_VERSION 3

# Gates (run before considering work done)
go test ./...                # the whole suite, no network — fakes for MCP + backend
go vet ./...                 # static checks
make test-race               # go test -race ./...
gofmt -l .                   # must print nothing (CI fails on unformatted files)

# If you touched the tool registry, COMMAND_REGISTRY, or a protocol/schema constant,
# regenerate the capability reference or CI fails on the drift:
go test ./internal/app -run TestGeneratedDocsAreCurrent -update
go test ./internal/commands -run TestGeneratedCommandRefIsCurrent -update
```

The backend pins a captured copy of our tool projection (its runbooks name the tools in
it), so after a tool add/remove/rename, export the refreshed inventory for it — the same
JSON value we send as `input.tools` (indented; compacting it reproduces the wire bytes),
taken from a real boot rather than re-derived, and needing no source edits:

```bash
go run ./cmd/tooldump                      # the projection a normal launch sends → stdout
go run ./cmd/tooldump -o tools.json        # …to a file
go run ./cmd/tooldump -workflow-intelligence=false  # …WITHOUT the graph tools (DAINTREE_WORKFLOW_INTELLIGENCE=0)
```

CI additionally runs on **macOS and Linux** (PTY harness on macOS, race detector on
Linux), diffs the generated docs, runs `govulncheck`, and scans for literal credentials
with TWO scanners that read different things. `gitleaks` reads git HISTORY (hence
`fetch-depth: 0`): a credential committed and "removed" a week later is still in the
history and still needs rotating, and only a history scan finds it. The project's own
`redact.FindLiteralSecrets`, pinned by `TestRepositoryContainsNoCredentials`, reads the
WORKING TREE with a scan-grade SUBSET of the redaction patterns — narrower on purpose,
because redaction errs toward masking and a scanner that fires on this repo's own prose
about credential shapes gets switched off. A credential-shaped string must announce itself
as a fixture ("fake", "test", "example") — so a real key, which never does, still trips it.
The one escape from that rule is `.gitleaksignore`, a per-finding fingerprint for a
fixture already committed before the rule existed: history is immutable, so such a value
can never grow a marker. Each entry records why it is not a leak. A tagged push builds macOS/Linux archives with
checksums and an SBOM (`.github/workflows/release.yml`); it does not sign or notarize,
because that needs an Apple certificate this repo cannot provision.

`make` targets: `build` · `install` · `test` · `test-race` · `vet` · `fmt` ·
`generate` (`go generate ./...`) · `run` · `clean` · `db-reset` (delegates to
`reset project-state`). `rtk` can mask the real `go` output, so verify a clean build/test
with the real binary if a result looks off.

There is **no ESLint/Prettier/Biome equivalent**: the only gates are `go build`,
`go vet`, `go test`, and a `gofmt` check. The host-embedding contract is in
`docs/DAINTREE_HOST.md`.

## Layout & architecture

Module `github.com/daintreehq/assistant`, `go 1.25.13`. **Import with full
module paths.** SQLite is `modernc.org/sqlite` (pure Go, **no CGO** — `CGO_ENABLED=0`
builds work). MCP is `github.com/modelcontextprotocol/go-sdk`. There is **no UI stack** —
the dependency tree is deliberately tiny (6 direct modules) and adding a rendering library
to it is a review blocker.

```
cmd/daintree-assistant/   main.go — entrypoint: flags → one-shot | doctor | host | mcp | daemon | repl; injects main.version
internal/
  domain/        pure vocabulary (imports only uuid + stdlib): RiskClass, Tier, ModelTier,
                 RunPhase, ToolResult (Ok/Fail), AgentEvent union, DB-row records, constants
                 (MainPromptCacheKey, MaxToolIterations), WatchCondition DSL, IDs
  config/        LoadConfig(ConfigOverrides) → AppConfig; trusted-env boundary; DEFAULTS;
                 resolves the endpoint (BackendURL) + the optional DAINTREE_API_KEY bearer
  ports/         interface seams: EventSink, Store, ToolRegistry, MCPClient, Queue
  projectinstructions/  Load(projectPath) → DAINTREE.md (16 KiB cap)
  debuglog/      StartDebugLog / LogDebug / CurrentDebugLogPath (0700/0600, 7-day prune)
  storage/       Store (store.go) over modernc.org/sqlite — timers, watchers, events, audit,
                 conversation, grants, memory; watchers/async/the attention inbox are
                 PROJECT-scoped and adopted (never cancelled) on Open — see BeginOwnership
  auth/          the account authority: OAuth discovery + PKCE login, the refresh token in
                 the OS credential store, refresh under a cross-process lock, logout, and
                 the typed state machine + redacted Status every surface renders. Also the
                 backend.AccountObserver: it is what turns a backend account verdict into
                 local state. Per USER, at the state ROOT — one login covers every project
  backend/       Daintree backend client — the CLI's ONLY model gateway. client.go (Respond/
                 RunTask/Health), contracts.go (strict wire envelope), sse.go (named-event
                 meta/delta/done/error parser), tasks.go (server-owned utility tasks). See docs/BACKEND.md
  models/        conversation WIRE VOCABULARY only (ChatMessage/ChatTool/ToolCallRequest/
                 ChatResult/Usage) — NOT a model client. The direct provider transport, Router,
                 SSE parser, retry layer and pricing table were deleted with the backend
                 migration; do not add a provider client back here (it would let a handler
                 bypass the backend that owns prompts, runbooks, and credentials)
  prompts/       MainPromptContext — the structured runtime facts the CLI collects
  mcp/           Daintree MCP client over the go-sdk (Streamable HTTP, SSE fallback)
  queue/         Queue — attention queue (Publish / Digest / Resolve)
  safety/        policy.go — Decide(risk, tier), tier gating, AlwaysConfirm, no-file-edit guard
  tools/         Registry (registry.go) + Dispatch (dispatch.go) + AssertSafe; tool families in
                 fsx/ mcpx/ mcpwrap/ contextx/ extractionx/ timer/ watcher/ queue/ grant/
                 workflow/ runbook/ auditx/ memory/ artifactx/ agenttaskx/ asyncx/
                 questionx/ scratchx/ subagentx/ (subagent.run — the delegation tool)
  agent/         Session (session.go) main turn loop + EventSink (events.go) + wake.go (autonomous wake)
  subagent/      the bounded READ-ONLY delegation loop: one brief → its own isolated
                 conversation (own backend session id, profile "subagent", its own narrow
                 tool inventory, hard round/time/size budgets) → ONE compact report back.
                 Everything it read goes to a transcript artifact and is dropped, so the
                 main thread learns the answer without paying the search. See docs/SUBAGENTS.md
  daemon/        scheduler.go (3s tick) + watcher.go (terminal watcher state machine)
  asyncwork/     AsyncCoordinator — runtime owner of async tool futures (terminal.run.async /
                 terminal.await.async): 1s pure-FSM polls, sibling coalescing, completion →
                 attention queue → autonomous wake. PROJECT-scoped: Start ADOPTS persisted live
                 rows and retries unconfirmed publishes (exactly-once via the group dedupe key)
  workflowgraph/ the workflow-intelligence layer (ON by default; DAINTREE_WORKFLOW_INTELLIGENCE=0 disables it):
                 typed durable execution graphs (DAG of nodes/edges/resources/blockers/evidence)
                 with local validation, patch application under optimistic revisions, prompt
                 digests, the dispatch observer, and adapters for the backend's stateless
                 workflow_plan/reconcile/resume_digest tasks. See docs/WORKFLOW_INTELLIGENCE.md
  ipc/           flock ownership leases (owner.lock / daemon.lock), control-socket path
                 derivation, NDJSON request/response protocol (status/attach/credentials/shutdown)
  supervisor/    the persistent per-project daemon: lease contention loop, headless App spans,
                 autonomous wake reactor, client-side AcquireOwnership/spawn. See docs/SUPERVISOR.md
  app/           App.Create(CreateOptions) — wires every dependency once, exposes the ToolContext factory
  commands/      slash-command catalog + handlers (structured results for the host + REPL)
  cli/           Run(Options) entry, line REPL (repl.go), host.go, mcpserve.go, render/, jsonout/
  host/          embedded host (host.go) — stdio NDJSON transport, PROTOCOL_VERSION 3
  mcpserver/     the assistant AS an MCP server (`mcp --stdio`) so another agent can drive
                 it as a sub-agent: per-session config (no server-held binding, because an
                 MCP client cannot restart us), async-first ask/poll because a turn takes
                 minutes, per-session approval brokering (decline/ask/auto, always
                 bounded so a parked dispatch cannot wedge a turn), run-transcript and
                 debug-log resources, and stale-binary reporting for the one thing a
                 session argument cannot fix. See docs/HEADLESS.md
  terminal/      TTY-gated raw escapes (clear.go) — the ONLY host-scrollback wipe path
  deps/          build-time blank-import anchor (deps.go) — pins go.mod modules; NO runtime effect
  e2e/           end-to-end tests only: built-binary, fake backend/MCP, inline-contract, turn/race
```

**Data flow:** `app.App.Create()` builds every dependency once (Store, MCP, Queue,
Backend client, Registry, Session) and exposes a `ToolContext` factory. `agent.Session.Send()`
runs a turn: optional auto-compact → push user message → `Backend.RespondStream(req, …)`
(sends structured `request.startup`, `request.runtime`, and `request.turn` context alongside
the visible conversation, the local tool inventory, and the opaque
backend `state` token) with a
token callback → the FIRST
SSE `meta` event carries the refreshed state token + the server's `runbooks` block and is
flushed as soon as selection finishes, before the upstream model connects. `OnRunbookLoaded`
carries the newly-loaded refs eagerly to the diagnostic sinks, while committed state
handling stays on the retry-safe deferred `OnMeta` callback; a full-request retry adopts
the eager meta's signed state so the backend reuses that selection. **Runbook selection is
server-owned**: the backend's selector picks/injects runbook bodies before it calls the
upstream model, so the runbook is in hand for that same generation; the CLI just stores the
state token. **Backend runbook loads never enter the conversation** — no card, no cue, in the
host stream or the line REPL. They are prompt
assembly, not a step the operator takes, and the delta the old card showed was misleading
besides (never what was retained, capped, or auto-paired as a foundation). There is **no
`/runbooks` command** either — a standing "what's active?" reveal is the same information with
the same missing affordance. The one place a load reaches a human is the explicit
`/explain <run>` timeline, beside that run's tool calls; the debug trace, run log and
`--json` stream keep the full signal, and `backend.respond.meta` is where selector tuning
reads it. The "skill" VOCABULARY — a visible "Skill loaded" event, the `/skills` name — is
held in reserve for future user-authored, project-level ASSISTANT skills, which are
intent-driven; protocol 3 naming the backend's concept "runbook" is what keeps it free. On tool calls, announce the whole batch
(`ToolBatch`) then `registry.Dispatch()` each in the safe sequence, feed results back and
re-`RespondStream` (replaying the state token).
`Dispatch` = validate args → tier gate (`safety.Decide`) → confirmation/grant → run handler →
audit. The daemon `Scheduler` ticks every 3s, firing due timers and watcher checks. All
sub-threads publish to the **attention queue** instead of interrupting the main thread.

## Invariants & conventions

- **No file edits.** `tools.Registry.AssertSafe()` (via `safety.AssertNoFileEditTools`)
  rejects, at startup, any tool whose name contains a forbidden fragment (`write_file`,
  `edit_file`, `fs.write`, `apply_patch`, `file.edit`, …). Edits go through
  `agentTask.spawnForEdits` (mode `edit` | `explore`) — the primary agent-spawn path.
  (`workflow.startWorkOnIssue` also spawns, via Daintree's recipe path: it creates the
  worktree AND launches an agent into it. Both are visible-terminal delegation; neither
  writes a file from this process.)
- **Delegation is READ-ONLY, and structurally so.** `subagent.run` dispatches a
  bounded sub-agent (`internal/subagent`) whose inventory is filtered to
  `domain.RiskRead` tools (`app.subagentToolNames`, 36 of 78 today) minus a small
  denylist, and whose dispatches run under `domain.ActorSubagent` with NO Confirm
  and NO AskChoice hook — so a mutating call that somehow reached dispatch fails
  closed instead of prompting a human who is not watching. That is the whole
  argument for running sub-agents unattended, in parallel, and without an approval
  of their own, and it is why `subagent.run` is itself a read-risk tool. Do NOT
  widen the inventory past read-risk — edits still go through
  `agentTask.spawnForEdits`. A sub-agent never recurses either (`subagent.run` is
  denylisted from its own inventory); fan out peers from the main thread instead.
  Every failure resolves to a `Report` with a `Status`, never an error that could
  sink the calling turn, and a bound hit spends one final tools-withheld round so
  the run still reports what it found, marked `partial`. See docs/SUBAGENTS.md.
- **Tool results use the `ToolResult` envelope** via `domain.Ok(summary, result)` /
  `domain.Fail(code, message, opts…)`. Handlers never throw to the caller — `Dispatch`
  recovers panics and returns a `Fail`. Side-channels (audit, debug log) must never
  break a tool call (guard them).
- **Risk classes & tiers** (`safety/policy.go`): risk ∈ read, local, ui, terminal,
  project, external, git, system. Tiers: `supervisor` (read/local/ui), `operator`
  (+terminal/project/external), `system` (+git/system). Mutating classes (`AlwaysConfirm`:
  terminal/project/external/git/system) need confirmation for the interactive `main`
  actor; non-interactive actors (watcher/timer/workflow/**wake**) need a scoped
  **automation grant** — the daemon's unattended wake turns dispatch as `ActorWake`
  and consume grants keyed to the well-known actor id `wake` (else the call becomes a
  blocked pending-approval inbox item).
- **Prompt assembly + caching are the BACKEND's job now.** The CLI sends NO system/developer
  prompt; the backend owns the base prompt, runbook bodies, prompt assembly, and the upstream
  `prompt_cache_key`. The CLI's only contribution to cache stability is keeping the
  conversation prefix stable: no client-side control prefix
  (`domain.ControlMessageCount == 0`), only `user`/`assistant`/`tool` roles reach the wire,
  project/agent facts ride the dedicated cache-friendly `request.startup` value,
  while volatile per-turn facts ride `request.runtime` / `request.turn` (inert data the
  backend renders). See `docs/BACKEND.md`. The effective order is stable backend system
  layers → tools → stable startup block → append-only conversation → fresh runtime/
  turn user block LAST. Only STABLE content may be system-role: the DeepSeek route
  serializes [all system messages] → [tools] → [conversation], so a volatile system message
  anywhere in the array lands before the ~18k-token tool schemas and busts their cache —
  measured 2026-07-08 by the latency benchmark against `deepseek/deepseek-v4-flash-0731`
  THROUGH OpenRouter, 36% → 99% prompt-cache hit after the fix. That is a property of that
  route; re-measure before assuming it transfers to another model the backend may select.
- **Single-owner, durable supervision (the persistent supervisor).** Exactly ONE
  process at a time owns a project's `state.db` — an open assistant or the
  `daintree-assistant daemon` — serialized by the flock owner lease (`internal/ipc`).
  Watchers, async futures, timers, and the attention inbox are **project-scoped**:
  `storage.Open` never tears them down; the owner-boot reconciliation is the explicit
  `Store.BeginOwnership` (adopt-not-abandon), and `/clear` is the only wholesale
  teardown. When the assistant closes, the daemon re-acquires the lease, adopts the
  live rows, keeps the 3s scheduler + 1s coordinator ticking, and runs autonomous
  wake turns that continue the SAME conversation (runtime_state session pointer +
  persisted backend state token). Background supervision is REAL now — with two honest
  caveats. (1) Credentials: supervision pauses (blocked inbox item, never abandoned)
  when Daintree closes or revokes the per-session MCP token, and resumes on the next
  launch. (2) **Platform: Unix only.** flock + Setsid have no Windows port, so the
  `!unix` builds fail loudly rather than run without exclusion, and background
  supervision simply does not exist there. Never promise overnight work on Windows.
  See docs/SUPERVISOR.md.
- **Async tool futures are runtime-owned, queue-delivered.** `terminal.run.async` /
  `terminal.await.async` return an IMMEDIATE "accepted" result carrying a typed
  `ToolResult.Async` handle (`asy_…`); the `asyncwork.Coordinator` (1s tick, started
  with the scheduler) then polls the terminals with the SAME pure-FSM settle policy as
  `terminal.awaitAll` (`domain.SettleAgentFSM`, no model calls, no output reads),
  coalesces same-turn siblings over a short settle grace, and publishes ONE
  `SourceAsyncTool` attention event that autonomously wakes the model
  (`agent.IsActionableWake` / `BuildWakePrompt`). A completion is NEVER delivered as a
  late tool result for the original call — the transcript stays structurally valid.
  Async invocations are project-scoped like watchers (adopted by the next owner's
  coordinator at Start; cancelled only by `/clear`); every coordinator write goes
  through the `ClaimLiveAsyncInvocation` guard so a concurrent `async.cancel` always
  wins cleanly, and a finalized-but-unpublished row (NULL queueEventId) is retried
  publish-only under the same dedupe key — exactly-once across crashes.
  The live ledger rides every round's `request.turn.async_operations` block so the
  model can't forget or re-issue in-flight work.
- **THE BINARY IS HEADLESS. There is no terminal UI package, and none may be added.**
  The Bubble Tea cockpit (`internal/ui`) was deleted when Daintree took over rendering;
  the whole charm stack (bubbletea / bubbles / lipgloss / glamour, and chroma / goldmark
  behind glamour) is out of `go.mod` and must stay out. Rendering belongs to Daintree,
  which drives this binary over `host --stdio` and draws the conversation in React. If a
  turn needs to *say* something new, that is a new **event on the host protocol**
  (`internal/host`), never a new thing drawn here.
- **Structured events are the only output.** The runtime emits `agent.EventSink` events,
  consumed by exactly three sinks: the host bridge (`internal/host/bridge.go`), the JSONL
  sink (`internal/cli/jsonout`), and the plain console sink (`internal/cli/consolesink.go`)
  behind the line REPL. Tools never render; the model loop never writes to stdout. Adding
  a fourth sink is fine; adding a *renderer* is not.
- **The line REPL is an operator convenience, not a product surface.** `internal/cli/repl.go`
  writes plain lines to the normal screen buffer — no raw mode, no alternate screen, no
  mouse capture — so a shell or SSH session keeps native scrolling and copy/paste. Do not
  grow it. `--classic` is a deprecated no-op kept only so existing invocations don't break.
  `/clear` remains the ONLY scrollback wipe (`internal/terminal/clear.go`,
  `\x1b[2J\x1b[3J\x1b[H`, TTY-gated).
- **Explicit liveness, ordered turn model.** The active turn is driven by a first-class
  `domain.RunPhase` (Received → Analyzing → Generating → ToolQueued/Running →
  Integrating → Complete/Failed/Cancelled), NOT inferred from "is the assistant text
  empty". A turn is an ordered `[]TurnStep` (prose / tool / status / note), not a flat
  string + a separate activities slice — so `preamble → tools → conclusion` reaches a
  consumer in true chronological order.
- **Watcher engine is a state machine, not a poller** (`daemon/watcher.go`):
  deterministic signals (agent state, exit code, tail regex, timeout) first, the small
  model only when needed, dedupe, publish only meaningful changes; completion is gated
  on a read-only git-cleanliness check before any irreversible action is suggested.
- **Fresh starts are honest, not amnesiac.** A new session starts with a
  clean transcript (no old failed turns), but PROJECT state deliberately carries
  over: adopted watchers/async keep running, the attention inbox persists, and the
  one-time "While you were away" notice (App.AttachSummaryLines, consumed on read)
  summarizes what the supervisor did while detached. What must NOT appear is
  anything stale-but-dead: rows for work that no longer exists self-correct on the
  first check (watchers reconcile against the live terminal state), and the
  detached-activity notice never repeats.
- **Comment style:** dense, "why"-focused block comments on non-obvious logic. Match it.
  Tests use Go's `testing` package; the host protocol is tested by driving NDJSON frames
  through `internal/host` and asserting on the emitted event stream. `:memory:` SQLite and
  fakes for MCP/models — never the network.

## Debug logging

Set `DAINTREE_ASSISTANT_DEBUG_LOG=1` to append a trace (every model request/response,
every tool call with args+result, the watcher lifecycle) to a **global** dir (default
`~/.daintree/logs`, override `DAINTREE_ASSISTANT_LOG_DIR`).

**Secrets are redacted at the write boundary** — inside `debuglog.formatLine`, not at the
~30 call sites, so a new call site inherits the protection without knowing the rule
exists. `internal/redact` does the work: credential SHAPES (bearer, `sk-`, PATs, JWTs,
`NAME=value` env assignments, URL userinfo, PEM blocks) plus EXACT values registered via
`redact.RegisterSecret` — the MCP token at boot and on every daemon credential refresh,
plus `DAINTREE_API_KEY` at boot on the rare install that sets one. Registration is additive: a rotated key stays
registered, because a log line written under it is still on disk. Block values are capped
at 64 KiB with a size + sha256 prefix. The same redactor also guards the durable audit
rows (`tools.safeJSON`) and the `approval:requested` payloads the host protocol emits.
The flag is read from the process env or the assistant's own `.env` fallback — NEVER the
bound project's `.env`, which is arbitrary repo content and must not be able to switch
tracing on for a session that did not ask for it (`trustedOrOwnGet`, `internal/config`). `debuglog.StartDebugLog(cfg, sessionId)` runs once per process at
boot: it deletes logs older than 7 days, opens a **per-session** `<date>-<sessionId>.log`
(never clobbering a prior run, dir 0700 / file 0600), writes a `session.start` header,
and returns the path so the caller can print `logging to <file>`. The `--json` session
header and `daintree.session.open` both report it. The logger is a no-op when disabled and never
throws. Tests pin `DAINTREE_ASSISTANT_DEBUG_LOG=0` / pass an explicit `logDir`.

### Replaying MCP calls by hand (live debugging)

Hitting the **same** MCP the assistant uses shows you the exact responses a tool got —
invaluable when a tool (e.g. `terminal.extract` / `terminal.awaitAll`) behaves wrong but
the model loop only shows the post-parse result.

Take the URL + token from the **running process's environment** (`DAINTREE_MCP_URL` /
`DAINTREE_MCP_TOKEN`, injected by Daintree) while it still owns them. They are **NOT** in
the session log: the old `mcp.credentials` line and `App.logMcpCredentials` were removed,
because a bound MCP token stays valid until Daintree revokes it (no fixed TTL) and still
authorises system-tier Daintree actions for that whole window, while a log file outlives it.
Do not add credential material back to the log.

Connecting (Streamable HTTP, `github.com/modelcontextprotocol/go-sdk`): POST JSON-RPC to
the URL with `Authorization: Bearer <token>`, `Content-Type: application/json`, and
`Accept: application/json, text/event-stream`. Handshake = `initialize` (capture the
`Mcp-Session-Id` response header) → `notifications/initialized` → then `tools/call`
(`terminal.list`, `terminal.getStatus`, `terminal.getOutput`, …) passing
`Mcp-Session-Id` on every follow-up. Responses come back as SSE `data:` lines.

To understand what a finish-wait actually *fetches* and how it decides "done", read
`internal/app/toolterminal.go` (`ReadStatuses` → `terminal.getStatus` with
`includeOutput`; `ReadOutput` → `terminal.getOutput`) alongside
`internal/tools/extractionx/awaitall.go` (the cohort poll loop) and
`internal/domain/finish.go` (`FinishPreFilter` — note an empty/whitespace tail is never
judged, so a status read that returns blank `recentOutput` strands a finished agent).

### Reading logs to improve the system (the core dev loop)

The session logs in `~/.daintree/logs/<date>-<sessionId>.log` are the **ground truth**
for how the model and tools actually behaved — not how we assume they behave. A
recurring, first-class development activity here is: the user hands you a real session
log (or a complaint about a session), you read it to find where the model misjudged,
misused a tool, or got confused, and you fix the **system** so the same mistake can't
recur. Treat every "the assistant did X wrong" report as a log-archaeology task, then a
prompt/schema/tool fix — not a one-off patch.

How to read a log fast (it is structured text — grep it, don't eyeball megabytes).
Every turn-scoped line carries `runId`/`turnId`/`round` so you can grep one turn and
reconstruct its timeline; see `docs/LOGGING.md` for the full event reference.
- `turn.start` / `turn.end` — the per-turn bracket. `turn.end status=failed|cancelled`
  is the fastest "which turn went wrong" filter (with `durationMs` + `rounds`).
- `tool.call … ok=false` / `outcome=error` — every rejected or failed tool call that
  reached dispatch, with the (post-decode) `args:` block and the `error:` envelope (code +
  message). Highest-signal: it shows the arguments the handler received, now tagged with
  `toolCallId`/`runId`/`risk`. (Rejections that never reach dispatch — bad-JSON args,
  not-offered — log as `tool.args.invalid` / `tool.not_offered` and carry the model's RAW
  args; a stuck loop logs `tool.repeat.warning` / `tool.repeat.abort`.)
- `backend.respond.request` / `backend.respond.raw_meta` / `backend.respond.runbook_cue` /
  `backend.respond.meta` / `backend.respond.done` / `backend.respond.error` — the
  backend-era successor to `model.request`/`model.response`
  (which no longer exist for the main loop — that path is `Backend.RespondStream`, not the
  vestigial Router). `request` summarizes what the backend was SHOWN (message count +
  role sequence + history hash + newest-message preview + tool inventory + runtime/turn
  context — bounded, not the full prompt); `raw_meta` timestamps actual SSE arrival,
  `runbook_cue` timestamps the optional eager user cue, `meta` is the retry-safe committed
  backend report (model, prompt/catalog version, and runbook-selection outcome — the surface
  that says whether a fix belongs in the backend selector); `done` is what it PRODUCED
  (content preview, tool calls, finish reason, usage).
- `mcp.call` — every MCP tool call with `callKind`, `attempts`, `durationMs`, and a
  bounded preview/hash of the result (or the normalized error). The layer where many
  tool failures actually originate (throttle results, transport blips).
- `watcher.created` / `spawn.launched` / `watcher.*` — the watcher/agent lifecycle.

The fix philosophy — **fix the guidance, not just the symptom.** When the model misuses
a tool, the root cause is almost always ambiguous or misleading instruction, NOT a dumb
model. The model can only act on what the base prompt + runbooks (now **backend-owned**, in
`../assistant-backend/src/daintree_assistant_server/prompts/` and `.../runbooks/files/*.md`)
and the local tool `Description`/`Schema` told it. So a model mistake is usually a
*documentation* bug in one of those surfaces — and the durable fix updates them in lockstep
so the model can't repeat it. **A prompt/runbook fix lands in the `../assistant-backend` repo;
a tool-shape fix lands here.** Prefer making the correct shape impossible to get wrong (show
literal argument shapes, not prose abstractions) over adding lenient parsing.

Worked example (2026-06-23): the model called `agentTask.spawnForEdits` with a flattened
key `"watcher<arg_key>create": true` and the strict decoder rejected it
(`json: unknown field`). Root cause: the prompt + runbook described the arg in prose as the
dotted path `watcher.create: true`, but the schema is a **nested object**
`watcher: {create, goal, cadenceMs}`. The model encoded the dotted prose literally. Fix:
the playbook and runbook now show `watcher: {"create": true, "goal": "..."}` explicitly and
warn against a dotted/flattened key — no code change, a prose fix at the source of the
confusion. (Editing the base prompt is free here: it just cache-misses on the changed
tokens, never goes stale — see the prompt-cache invariant above.)

## Key environment variables

`DAINTREE_BACKEND_URL` (the backend endpoint — trusted-env ONLY, never a project `.env`;
defaults to the deployed backend, and pointing it at `http://127.0.0.1:8473` IS the local
dev loop) · `DAINTREE_ALLOW_INSECURE_BACKEND` (trusted-env ONLY; authorizes a non-loopback
plaintext `http://` endpoint, which is otherwise refused — see `backend.ValidatePlaintextRemote`)
· `DAINTREE_API_KEY` (OPTIONAL bearer, trusted-env ONLY, unset on a normal
install — when set it overrides the managed account sign-in as the CALLER's bearer; it
never reaches the provider and the backend still funds the turn from its own upstream
credential) ·
`DAINTREE_MCP_URL` / `DAINTREE_MCP_TOKEN` /
`DAINTREE_PROJECT_ID` / `DAINTREE_WINDOW_ID` (injected by Daintree) ·
`DAINTREE_ROUTING_PRIVACY` / `DAINTREE_ROUTING_SORT` / `DAINTREE_ROUTING_ONLY` /
`DAINTREE_ROUTING_IGNORE` (endpoint routing — trusted-env ONLY, never a project `.env`:
they decide how strictly the privacy filter is applied and which endpoints see the
user's source. Closed set, validated at startup so a typo fails there rather than as a
mid-turn 400; `/routing` shows the active posture using the BACKEND's own privacy
wording) ·
`DAINTREE_ASSISTANT_TIER` (default `system`) · `DAINTREE_ASSISTANT_AUTO_APPROVE` ·
`DAINTREE_ASSISTANT_OFFLINE` · `DAINTREE_ASSISTANT_STATE_DIR` · `DAINTREE_ASSISTANT_DEBUG_LOG` /
`DAINTREE_ASSISTANT_LOG_DIR` · `DAINTREE_WORKFLOW_INTELLIGENCE` (the workflow execution-graph
layer, ON by default now that the backend carries the matching `workflow_state` turn-context
contract + workflow tasks; `=0` disables it; see docs/WORKFLOW_INTELLIGENCE.md).
(Model/provider variables — every `*_API_KEY` and the `DAINTREE_{LARGE,MEDIUM,SMALL}_MODEL`
trio — are **backend-only**. The CLI reads none of them and its `AppConfig` carries no
model or provider fields at all. On a normal install it holds no credential whatsoever;
the one it CAN hold, `DAINTREE_API_KEY`, it forwards verbatim and never stores.)
Resolution order: CLI overrides → real process env (snapshotted **before** `.env` loads,
the trusted-env boundary) → project `.env` → assistant's own `.env` → `DEFAULTS`. All in
`internal/config`. State lives under `~/.daintree/assistant-cli/` (`state.db`; per-project
subdir when a project id is set).

## More docs

`docs/BACKEND.md` (**the backend integration — read this for the model / runbook / prompt
story**), `docs/SUPERVISOR.md` (the persistent supervisor daemon: leases, adoption,
autonomous wake turns, credential lifecycle), `docs/SUBAGENTS.md` (**delegated research — the sub-agent loop, its bounds, the read-only
guarantee, and sub-agent runbook selection**),
`docs/RUNBOOKS.md` (how server-owned runbooks
work + the local run-tracking tools), `docs/WORKFLOW_INTELLIGENCE.md` (the flag-gated
workflow execution-graph layer: graph model, tools, observer, async linking, and the
backend contract it expects),
`docs/HEADLESS.md` (**driving the CLI from a script or another agent — the `mcp --stdio`
server, the flags, the `--json` event schema, exit codes, isolation**),
`README.md` (full overview), `docs/DAINTREE_HOST.md` (host embedding),
`docs/ARCHITECTURE.md`, `docs/DAINTREE_MCP.md` (Daintree's MCP protocol),
`docs/DAINTREE_HOST.md` (how Daintree launches / displays / hides / restarts this CLI),
`docs/LOGGING.md` (the debug-log event reference), `docs/RUNTIME.md` (auto-compaction +
model error behavior), `docs/TOOLS.md` (adding a tool). Runbook authoring + the model live in `../assistant-backend`
(its `runbooks/files/*.md` + `docs/DAINTREE_API.md`).
