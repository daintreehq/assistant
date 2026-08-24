> **HISTORICAL — this describes the retired PTY embedding (host protocol v1/v2).**
>
> Daintree used to run this binary as an ordinary terminal panel and render its Bubble Tea
> cockpit through the same xterm pipeline every agent terminal used. That is no longer how
> the assistant is embedded: the binary is **headless**, there is no TUI package, and
> Daintree drives it over `host --stdio` and draws the conversation in React. The current
> contract is [`../DAINTREE_HOST.md`](../DAINTREE_HOST.md).
>
> Kept for archaeology. Parts of it — the env-injection table, the session-binding error
> codes, the worktree-scope reasoning — are still accurate and were carried forward into
> the new document; the display, lifecycle and "structured host is deferred" material is
> not. Do not treat anything here as a live contract.

# Daintree host — how the app embeds this CLI

This is the **host-side companion** to [`DAINTREE_MCP.md`](DAINTREE_MCP.md). That doc
describes the *protocol* the CLI speaks to Daintree (the tool catalog, tiers, call/response
shapes). This doc describes the *embedding* — how the Daintree desktop app (an Electron
IDE) actually launches this binary, wires it to the MCP server, shows its terminal, hides
it, restarts it, and tears it down. If you have ever wondered "who set `DAINTREE_MCP_URL`?",
"why did my session get a `SESSION_BINDING_GONE`?", or "what happens to my process when the
user switches projects?", this is the reference.

> **Source of truth.** This describes Daintree's behavior as observed from the CLI's side of
> the boundary plus a read of Daintree's host code (`daintreehq/daintree`). The env contract
> the CLI actually consumes lives in [`internal/config/config.go`](../internal/config/config.go)
> — that file wins for what the CLI *reads*. The Daintree internals named here can drift; the
> **observable contract** (env var names, error codes, tier semantics) is what to rely on. A
> pointer list for cross-repo maintenance is at the end.

## TL;DR

- The assistant is **one ordinary terminal panel** inside Daintree's "Assistant area" (the
  **HelpPanel**), not a special window. Daintree runs `daintree-assistant` in an interactive
  shell PTY and shows its TUI with the same xterm pipeline every agent terminal uses.
- Daintree gives the CLI the MCP connection **entirely through environment variables** —
  `DAINTREE_MCP_URL`, `DAINTREE_MCP_TOKEN`, `DAINTREE_WINDOW_ID`, `DAINTREE_PROJECT_ID`. No
  `.mcp.json`, no CLI flags (`supports.mcpInjection: "env-only"`).
- There is **one live assistant backend per project**, pinned to the window/view that
  launched it. Launching again for the same project **displaces** the old one.
- The token is **per-session** and is bound to a **worktree/terminal context snapshot taken
  at launch**. Daintree grants the assistant the **`system`** tier — the CLI's own safety
  layer, not Daintree's tier gate, is what stops dangerous calls.
- **Hiding** the panel never kills the process; the panel slides off-canvas and keeps
  running (throttled). **Restart / New session** mints a fresh token and **drops the
  transcript**. **Eviction / window-close / crash** kill the PTY but capture a resume
  handle; **app restart** does not auto-resume.

---

## 1. What the assistant *is*, from the host's side

Daintree is a multi-window, multi-project IDE. Each open project runs in its own renderer
(a `WebContentsView` with its own V8 context). Inside that renderer, a React component
called the **HelpPanel** (the "Daintree Assistant" area, a right-hand slide-in panel) hosts
the assistant.

The assistant is launched as a normal **PTY-backed terminal panel** — internally a panel of
`kind: "terminal", location: "overlay"`. Daintree does **not** `exec` the binary directly;
it starts an interactive login shell, then *types the command into it* (roughly
`daintree-assistant\r`) so that when the CLI exits the shell is still there. Consequences:

- The CLI runs under a **real TTY** in an interactive shell — the attached session's raw-mode /
  alt-screen assumptions hold.
- `os.Getenv("SHELL")`, the user's shell rc, and PATH are the user's normal login
  environment (see the env-hygiene note in §2).
- Daintree reuses the exact same xterm rendering, resize, focus, and scrollback machinery it
  uses for agent terminals — the assistant is not a bespoke widget, it is "a terminal that
  happens to be pinned in the HelpPanel."

The binary is discovered **by name on `PATH`** (`command: "daintree-assistant"`): Daintree
tries `DAINTREE_CLI_PATH_PREPEND` dirs, then `which daintree-assistant`, then the npm-global
prefix shim (`<npm prefix>/bin/daintree-assistant`). It is installed **independently of
Daintree** (global npm / a PATH shim), which is why Daintree resolves it by name rather than
bundling it.

> The `daintree-assistant` agent id is deliberately excluded from Daintree's normal
> "launchable agents" list — it is **not** offered as a standalone coding agent in the
> toolbar, launcher, or keybindings. The only way to start it is through the Assistant area.

**Not the transport you might expect.** The CLI also ships a `host --stdio` NDJSON transport
(`internal/host`). Daintree has a *contract* for driving the assistant as a
`utilityProcess.fork()` structured host, but that path is **deferred and not wired** in the
shipping app. Today's integration is 100% the **interactive PTY attached session + env-injected MCP
over HTTP** described here. Don't assume Daintree is talking to `host --stdio`.

---

## 2. The launch handshake — how the CLI receives its MCP connection

Everything the CLI needs to reach Daintree arrives as **process environment**, injected at
spawn time. The CLI reads these in [`internal/config/config.go`](../internal/config/config.go)
(the "trusted-env boundary"): `DAINTREE_MCP_URL` / `DAINTREE_MCP_TOKEN` /
`DAINTREE_PROJECT_ID` / `DAINTREE_WINDOW_ID` are `trustedOrOwn` — taken from the real process
env (or the assistant's *own* `.env`), **never** from the bound project's `.env`, so a
checked-out repo can't spoof the link or the identity.

### Environment Daintree injects

| Variable | Value | CLI reads it? |
| --- | --- | --- |
| `DAINTREE_MCP_URL` | `http://127.0.0.1:<port>/mcp` (Streamable HTTP; `<port>` is the *actually bound* MCP port, default 45454) | **Yes** — `cfg.McpURL` |
| `DAINTREE_MCP_TOKEN` | Per-session bearer (32 random bytes, hex). Sent as `Authorization: Bearer <token>` | **Yes** — `cfg.McpToken` |
| `DAINTREE_PROJECT_ID` | The bound project's id. Scopes the CLI's `StateDir` (`~/.daintree/assistant-cli/<project>/state.db`) | **Yes** — `cfg.ProjectID` |
| `DAINTREE_WINDOW_ID` | The launching window's numeric id. **Informational** to the CLI (identity / per-window state); the enforceable binding is server-side, not this value | **Yes** — `cfg.WindowID` |
| `DAINTREE_ASSISTANT_AUTO_APPROVE` | `"1"` when the user enabled "bypass permission prompts" for the session | **Yes** — `cfg.AutoApprove` (trusted-only) |
| `DAINTREE_ASSISTANT_DEBUG_LOG` | `"1"` when the user enabled debug logging in settings | **Yes** — `cfg.DebugLog` (trusted-only) |
| `DAINTREE_ASSISTANT_SCRATCH_DIR` | A per-session scratch path Daintree provisions | **No** — host-set but not currently consumed (reserved) |
| `DAINTREE_PANE_ID` / `DAINTREE_CWD` / `DAINTREE_WORKTREE_ID` | Universal Daintree terminal metadata stamped on *every* PTY | **No** — the CLI ignores these; see §6 on why worktree identity is server-side |

Notes that matter for the CLI:

- **Env-only, always fresh.** Because injection is env-only, there is no config file to read
  and no flag to parse for the connection. Daintree strips all inherited `DAINTREE_*` (and
  known-sensitive vars) from the shell's environment before injecting its own, so a
  `DAINTREE_MCP_TOKEN` sitting in the user's OS environment can't leak in — the values you
  get are the ones Daintree minted for *this* launch.
- **The URL is `/mcp` (Streamable HTTP).** The CLI's client tries Streamable HTTP first and
  falls back to legacy SSE by rewriting `/mcp → /sse`; Daintree serves both, but for the
  assistant it hands you the `/mcp` endpoint directly.
- **The port can move.** Default is `127.0.0.1:45454`, but on a bind conflict Daintree walks
  up to 10 ports. Always use the port embedded in `DAINTREE_MCP_URL`; never hard-code 45454.
- **`SCRATCH_DIR`, `PANE_ID`, `WORKTREE_ID`, `CWD` are not read today.** If the CLI ever
  wants a Daintree-provided scratch dir or its own pane/worktree id from env, the host
  already provides them — but as of now `config.go` doesn't consume them.
- The CLI also accepts `--mcp-url` / `--mcp-token` flags as overrides, but Daintree does
  **not** use them; it always goes through the env.

### The command line Daintree runs

The launch command is the bare `daintree-assistant`, plus any user-configured `--model <id>`
and free-form custom args from the Assistant settings tab. There are **no** connection flags
— all wiring is the env above.

### Working directory

The assistant is launched with **cwd = the project root** (not the active worktree, not a
per-session dir). Daintree's rationale: the assistant is env-only and ships its own runbooks,
so it reads nothing meaningful from cwd — running it at the project root makes its own file
tools and the terminal's file-link resolution operate on the real project tree. (Other
help-panel agents like Claude/Codex run in a per-session dir that owns their `.mcp.json`;
the Daintree Assistant does not.) See §6 for why cwd is *not* how worktree scope is decided.

### Version gating and a missing binary

- Daintree probes the installed version by running `daintree-assistant --version` (parsed as
  semver, cached ~12h; "Check again" forces a refresh).
- The `daintree-assistant` agent declares **no minimum version**, so the version gate
  **never blocks it** — the block screen exists for other help agents (Claude/Copilot) that
  do declare a floor. Practically: Daintree will launch whatever `daintree-assistant` is on
  PATH.
- If the binary is **absent**, Daintree shows a "missing CLI" state with a **Run anyway**
  affordance (the launch still attempts, so a shim that resolves late still works).

---

## 3. The MCP server the host stands up

The endpoint in `DAINTREE_MCP_URL` is Daintree's **local MCP server** (in-process in the
Electron main process), not the assistant backend. Key facts the CLI can rely on:

- **Bind + transport.** `127.0.0.1` only (localhost-gated: requests with a non-local `Host`,
  or a mismatched `Origin`, are rejected with 403 before auth). Streamable HTTP at `/mcp`
  plus legacy SSE at `/sse` on the same server.
- **The assistant's token is per-session.** Minted when Daintree *provisions* the session,
  registered with the server's validator, and **rotated on every re-provision**. It is not
  the same as the persistent "API key" shown in the MCP server settings tab — that key is
  for *external* third-party clients (Cursor, Claude Code, scripts) and maps to a separate
  `external` tier. The assistant is never `external`.
- **The assistant runs at the `system` tier.** Daintree forces the Daintree Assistant's
  session tier to `system` ("the workspace's first-class conductor"), the top of the
  workbench ⊂ action ⊂ system ladder. So the host tier gate does **not** block the assistant
  from any tool — `git.commit`, `git.push`, `worktree.delete`, `forge.mergePR`, etc. are all
  reachable. **Confirmation for dangerous operations is therefore the CLI's own
  responsibility** (its `safety` layer + user confirm), exactly as
  [`DAINTREE_MCP.md`](DAINTREE_MCP.md) says: the tier is advisory to the CLI.
- **Two different "tiers" — don't conflate them.** (a) The *server-enforced* tier is bound
  to your token and forced to `system`. (b) The CLI's *own* advisory `cfg.Tier`
  (`DAINTREE_ASSISTANT_TIER`, defaulted locally) drives its local safety gating. Daintree
  does **not** inject `DAINTREE_ASSISTANT_TIER`, so the CLI's local tier is whatever the CLI
  defaults to — independent of the `system` grant on the wire.
- **Idempotency + confirmation** behave as documented in `DAINTREE_MCP.md` (`requestKey`
  dedup; `danger:"confirm"` may elicit). Because the assistant is `system`-tier, the host's
  tier-mismatch / grant-elevation machinery is largely dormant for it — that flow mainly
  serves lower-tier help agents and external clients.

---

## 4. Identity, window & session binding

Daintree pins each assistant session to the exact renderer that launched it, so tool calls
can never be routed to the wrong window or worktree.

- **The bearer is pinned to a WebContents + an `ActionContext` snapshot** taken at launch.
  Every tool dispatch replays that snapshot as the acting context. `DAINTREE_WINDOW_ID` is
  handed to the CLI for identity/telemetry, but the *enforceable* binding is this server-side
  pin, not the env value — so don't build anything security-sensitive on `WINDOW_ID`.
- **One backend per project.** Daintree keys the live assistant by `projectId`. Provisioning
  a new session for a project **displaces** any prior one (revokes its token, kills its PTY).
  Across N windows you can have up to one assistant per distinct project; a second window
  opening the assistant for an already-active project takes it over (last-writer-wins).

### The two "stop retrying" errors

These are the host telling the CLI its binding is permanently gone. Both are non-retriable
"business" errors and their messages literally end with **"Do not retry."** The CLI should
stop using that session and tell the user (this is the behavior `DAINTREE_MCP.md` prescribes).

| Error code | Raised when | Meaning for the CLI |
| --- | --- | --- |
| `SESSION_BINDING_GONE` | The pinned WebContents is gone/destroyed (window closed, view evicted, renderer crashed) | The Daintree window this session was bound to no longer exists. Dead session. |
| `BINDING_STALE` | A pinned dispatch targets a `projectId` that is no longer the active project in that view (user switched projects in the same window) | The session was bound to a project that isn't active anymore. Dead session. |

What invalidates a binding (all revoke the token; some also capture a resume handle — see §7):

- Window closed → revoked (capture).
- Project-view LRU-evicted / under memory pressure → revoked (capture).
- Renderer/view crash → revoked (capture).
- Project switched away inside the same view → `BINDING_STALE` on next call.
- A sibling re-provision for the same project (the one-backend rule) → prior session revoked,
  in-flight calls 401, PTY killed.

Repeated auth failures or tier-mismatches trip an **abuse policy** that revokes the session
outright — another reason to treat a `401`/binding error as terminal rather than hammering.

---

## 5. How the CLI's activity is displayed

The HelpPanel renders the CLI's TUI in the terminal pane, and adds a thin **status/activity
row** underneath it fed by the MCP server (targeted IPC to the pinned renderer, never
broadcast). None of this changes what the CLI does — but it's why your tool calls become
visible chrome:

- **MCP activity strip.** Every tool call the CLI makes emits `tool-call-started` /
  `tool-call-settled` events (args redacted host-side). The strip coalesces same-turn bursts
  into "N calls · <tool>", holds sub-400ms calls to avoid flashing, decays a success after
  ~5s, and keeps errors sticky. Clicking it opens a **recent calls** popover backed by the
  MCP **audit log**, filtered to this help session.
- **Turn-outcome pip.** The host can surface a small warning dot for `agent-stuck`
  ("Stopped early") or `reasoning-loop` ("Repeating steps"), derived from turn-outcome
  telemetry.
- **Images / figures.** If the CLI calls the host's image-display tool, Daintree shows the
  figure in a **figure rail** beneath the terminal (`help.displayImage` → a targeted
  `help-display-image` event). This is the host-side render path for CLI-produced figures.
- **Tier / grant banners.** For lower-tier help agents the host shows tier-mismatch and
  grant ("Approve once" / "Always allow", with a live countdown) banners. For the
  `system`-tier assistant these rarely fire.

Everything here is faithful surfacing of what the CLI *did* — Daintree reads the audit trail
and the tool-call events, it does not re-interpret the conversation.

---

## 6. Worktree & project scope

**Project scope** is carried by `DAINTREE_PROJECT_ID` (env) and, more importantly, by the
per-session bearer bound to that project. The CLI uses `DAINTREE_PROJECT_ID` to scope its own
`StateDir` (so each project gets its own `state.db`, timers, watchers, memory).

**Worktree scope is server-side, not env, and is pinned at launch:**

- At launch Daintree snapshots the *focused context* (active worktree id/path/branch,
  focused terminal) into the `ActionContext` bound to the token. Tool dispatches replay that
  snapshot, so a focus change between the model deciding to call a tool and the call landing
  can't retarget it.
- The **worktree is frozen** at the launch snapshot; the **target terminal** is re-resolved
  live at dispatch. Changing the active worktree in the UI does **not** relaunch the
  assistant, does **not** re-point it, and does **not** notify it — the assistant stays
  pinned to the worktree it was launched against. Daintree's UI shows a "pinned worktree
  diverged" chip that lets the *user* jump back to the pinned worktree; it never moves the
  assistant.
- **The CLI can't read its worktree from env.** `DAINTREE_WORKTREE_ID` is stamped on the PTY
  but `config.go` doesn't consume it, and the assistant's cwd is the *project root*, not the
  worktree. So to know "which worktree am I acting in," the CLI must **ask over MCP** —
  `actions.getContext` (returns the active project/worktree/focused-terminal snapshot) or
  `worktree.getCurrent` — not inspect its environment or cwd.
- To act in a *different* worktree, the model uses the worktree-targeting tool arguments
  (`worktreeId` on the relevant tools), not a process relaunch.

---

## 7. Lifecycle — hiding, restarting, hibernation, crash

This is the part with the most surprising behavior, so here it is in full.

### Hiding (the panel toggle / focus mode)

Hiding the Assistant area **never touches the process**. Daintree keeps the panel mounted and
slides it off-canvas with a CSS transform (so the terminal box keeps a constant size and the
CLI doesn't get a resize storm on show/hide). While hidden the terminal's render rate drops
to a background tier, but the PTY keeps running and buffering. Re-showing repaints and
re-focuses. The CLI cannot distinguish hidden from visible except via normal TTY signals —
there is no "you are hidden" message.

### Restart / New session

The "**+ New session**" and "**Run anyway**" affordances are a **hard restart**:

- Daintree removes the panel, revokes the old session, and **provisions a fresh session** —
  new `sessionId`, **new bearer token**, new scratch dir, new `DAINTREE_MCP_URL` — then
  spawns a **brand-new PTY**. Env is re-injected fresh; nothing is reused from the old
  process.
- **The transcript is intentionally dropped.** "New session" means the user wants a clean
  slate; Daintree does not carry the old conversation forward at the host level. (The CLI's
  own `state.db` still exists under the same `DAINTREE_PROJECT_ID`, so any continuity is up
  to the CLI — Daintree passes no resume handle on this path.)
- The version gate is skipped on this path; "Run anyway" additionally forces past the
  missing-CLI guard.

There is also a generic "restart the terminal backend" path (restarting Daintree's whole
PTY host process); it is not assistant-specific and just respawns PTYs.

### Hibernation, eviction & the resume story

Daintree treats the assistant asymmetrically from grid terminals. Grid agent PTYs live in a
separate PTY-host process and **survive** view eviction; the **assistant PTY is deliberately
killed** when its owning renderer goes away. But this is *not* a silent death — Daintree
`gracefulKill`s to capture a resume handle and persists it. The full matrix:

| Event | Assistant PTY | Resume handle captured? |
| --- | --- | --- |
| Panel hidden / focus mode | **Survives** (off-canvas, throttled) | n/a |
| Project switch, view stays cached | **Survives** (renderer throttled/frozen, PTY runs) | n/a |
| Window minimize / app background / system sleep | **Survives** (PTY pause/resume) | n/a |
| Project-view **LRU / memory-pressure eviction** | **Killed** (graceful) | **Yes** — per-project pending-hibernation store |
| **Window close** | **Killed** (graceful) | **Yes** |
| Renderer / **view crash** | **Killed** (graceful) | **Yes** |
| **App shutdown** | **Killed** | **No** (no time to capture) |
| **Main-process (host) crash** | Dies with the app | Only if a capture was already on disk |
| PTY-host crash | Dies like any PTY; host shows a crash banner | Generic PTY recovery |

Resume behavior after a capture:

- **Same-session** eviction/crash where the panel was open: Daintree auto-reopens and resumes
  (for agents that support resume).
- **Otherwise / after a full app restart:** the "panel was open" flag is in-memory only, so a
  cold restart never auto-resumes — Daintree offers a manual **"Resume assistant"**
  affordance instead.
- **Important for this CLI specifically:** `daintree-assistant` declares **no resume
  command**, so Daintree's resume affordance is **suppressed for it** — a "resume" of the
  Daintree Assistant is really a **fresh launch** in the same project (fresh token/session).
  Daintree does not pass a resume handle to `daintree-assistant`. **Any conversation
  continuity must come from the CLI's own per-project `state.db`** (keyed by the stable
  `DAINTREE_PROJECT_ID` → `StateDir`). If seamless resume matters, that's a CLI-side feature
  to build on top of persisted state, not something Daintree drives.

The assistant is also **excluded from Daintree's crash/session snapshot restore** (it's an
ephemeral panel), which is why the pending-hibernation store — not the normal panel-restore
path — is its only recovery channel.

### State survival — the CLI's own answer

The table above is what happens to the **process**. This is what happens to the **state**,
which is the question a tester actually asks ("if I close this, does my agent keep being
watched?"). Four distinct things survive independently, so keep the words apart — do not use
"session", "conversation", "project state", and "supervision state" interchangeably:

| | **Terminal transcript** (host scrollback) | **Conversation** (`state.db` history) | **Project state** (memory, workflows, audit, inbox) | **Background supervision** (watchers, async, timers) |
| --- | --- | --- | --- | --- |
| Panel hidden / project switch | survives | survives | survives | runs (attached session owns the lease) |
| Cockpit exits normally (`^C`, `/quit`) | cleared by the host | survives | survives | **continues** — the daemon re-acquires the lease and adopts the live rows |
| Cockpit crashes / PTY killed | cleared by the host | survives | survives | **continues** — flock is kernel-released, so handover needs no cleanup |
| Host **"+ New session"** | dropped deliberately | new conversation; the old one stays in `state.db` | survives | **continues**, and completions land in the attention inbox |
| Daintree app quits | gone | survives | survives | **stops** — the daemon loses the MCP token, so supervision *pauses* with a blocked inbox item rather than fabricating outcomes; it resumes on the next launch |
| Machine sleeps | survives | survives | survives | pauses, then does timer catch-up on wake |
| Machine restarts | gone | survives | survives | **stops**; the next launch adopts the persisted rows |
| `/clear` | wiped (the only scrollback wipe path) | cleared | survives | **cancelled** — `/clear` is the one wholesale teardown |
| `reset project-state` | untouched | cleared | cleared | cancelled |
| CLI upgrade with a schema bump | untouched | moved aside to a timestamped backup, then recreated | same | cancelled with the old DB |
| **Windows** | as above | as above | as above | **never survives attached session exit** — no supervisor on this platform |

The one-time **"While you were away"** notice (`App.AttachSummaryLines`, consumed on read)
is how the second and third rows become visible: a fresh attached session starts with a clean
transcript, but it tells you what the supervisor did while you were detached. It never
repeats.

So the honest promise to a tester is: **"this survives closing the Assistant panel"** — not
"this survives closing Daintree", and never "this runs overnight" unless Daintree stays up
on a Unix machine.

---

## 8. What the user can configure

Two settings surfaces touch the assistant; both change what Daintree injects/enforces:

- **Assistant settings tab** — chooses the agent (including `daintree-assistant`), custom
  args and `--model`, **debug logging** (`DAINTREE_ASSISTANT_DEBUG_LOG=1`), a "Daintree
  control" master switch for the MCP wiring, hibernation timeout, a **capability tier**
  picker (honored for other help agents; the Daintree Assistant is force-promoted to
  `system` regardless), a "bypass permission prompts" toggle
  (`DAINTREE_ASSISTANT_AUTO_APPROVE=1`), and audit-log capture/retention.
- **MCP server settings tab** — enable/disable the server, port, rotate the *external* API
  key, disconnect external clients, and view the audit log. This governs the server the CLI
  connects to (port/enabled), but the assistant's own bearer is managed automatically, not
  here.

---

## 9. What the CLI can rely on (contract summary)

- The MCP connection arrives **only** via `DAINTREE_MCP_URL` + `DAINTREE_MCP_TOKEN` env,
  fresh per launch, `/mcp` Streamable HTTP, localhost-only. Use the URL's port verbatim.
- `DAINTREE_PROJECT_ID` is a **stable identity** across launches/resumes for a given project
  — safe to key `StateDir` and per-project memory on.
- `DAINTREE_WINDOW_ID` is informational; the real binding is server-side.
- The session tier on the wire is **`system`**; the host will not tier-block the assistant,
  so **the CLI owns confirmation of dangerous operations.**
- `SESSION_BINDING_GONE` / `BINDING_STALE` are **terminal** — stop retrying that session and
  surface it to the user; they mean the bound window/project is gone.
- Worktree identity is **not** in env or cwd — query it over MCP (`actions.getContext` /
  `worktree.getCurrent`); it's pinned at launch.
- Hiding keeps the CLI alive; **New session** kills it and drops the host-side transcript;
  eviction/close/crash kill it (with a resume capture that, for this CLI, currently
  translates to a fresh launch because there's no resume command).

### Known gaps / rough edges

- **No host-driven resume for `daintree-assistant`.** Continuity across
  eviction/close/restart is CLI-side only (`state.db`); Daintree provides the stable
  `DAINTREE_PROJECT_ID` but no resume handle. Worth building CLI-side conversation restore
  on it.
- **`SCRATCH_DIR` / `PANE_ID` / `WORKTREE_ID` / `CWD` are injected but unread.** If the CLI
  wants a host-provided scratch dir or its own worktree id from env instead of an MCP call,
  the plumbing already exists on the host — it just needs `config.go` to read it.
- **`DAINTREE_ASSISTANT_TIER` is not injected**, so the CLI's local advisory tier is its own
  default and doesn't mirror the `system` grant on the wire. That's fine today (the CLI adds
  its own safety layer) but is a place the two "tiers" can be confusing.

---

## Daintree-side source pointers (drift warning)

For cross-repo maintainers only — these are `daintreehq/daintree` internals and **will
drift**; trust the observable contract above, not these paths.

- **Launch + env injection:** `electron/ipc/handlers/terminal/lifecycle.ts` (the help-launch
  branch), `electron/services/HelpSessionService.ts` (provision, token mint, one-per-project,
  revoke/capture), `electron/services/pty/EnvironmentFilter.ts` (metadata injection + env
  hygiene).
- **Agent definition:** `shared/config/agents/daintree-assistant.ts` (command, `env-only`
  MCP, no min version), `shared/config/agentIds.ts` (excluded from launchable agents).
- **MCP server + tiers + binding errors:** `electron/services/mcp-server/*`
  (`httpLifecycle.ts`, `sessionServer.ts`, `tierAuth.ts`, `rendererBridge.ts`),
  `shared/config/helpAssistantTierAllowlists.ts`, `src/services/ActionService.ts`
  (`BINDING_STALE`).
- **UI + lifecycle:** `src/components/HelpPanel/*` (panel, activity strip, banners),
  `src/controllers/HelpSessionController.ts` (launch/resume/hibernate FSM),
  `electron/window/ProjectViewManager.ts` (eviction), `electron/services/PendingHelpHibernationStore.ts`.

Keep this doc and [`DAINTREE_MCP.md`](DAINTREE_MCP.md) in sync when the Daintree side changes
the env contract, the tier the assistant gets, or the binding-error semantics — those three
are the load-bearing parts of the contract.
