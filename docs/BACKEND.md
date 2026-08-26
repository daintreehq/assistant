# Daintree Assistant backend — CLI integration

The CLI is a **thin local runtime**. It no longer acts as a model client; instead it
talks to the **Daintree Assistant backend** (a Daintree-native HTTP API — *not*
OpenAI-compatible). The backend owns the system prompt, developer instructions, runbook
selection, model choice, prompt assembly, the utility-model prompts, and prompt
caching. The CLI owns the visible conversation, the local tool registry + execution,
permissions, runtime / project context collection, memory + scheduler state, and the
opaque backend state token. It owns **no UI**: the runtime emits structured events, thin
adapters serialize them (host NDJSON, JSONL, the console sink), Daintree renders the
product, and the line REPL is a development convenience. "Terminal UI" and "stream
rendering" were this document's description of a cockpit that no longer exists.

```
User → Daintree CLI ──(structured startup + visible conversation + runtime/turn + tools)──► Daintree backend
        │  stores conversation, exposes & runs local tools,                            owns prompts, runbooks,
        │  streams assistant text, persists backend state                              model routing
        ◄──(named-event SSE: meta / delta / done / error)────────────────────────────┘
                                                                                             │
                                                                        (the BACKEND's own credential)
                                                                                             ▼
                                                                                   the upstream provider
                                                                                             │
                                                                                             ▼
                                                                                   the selected model
```

**The backend owns the upstream credential.** It reaches every model — the main
orchestration model and every utility model behind summarize / extract / classify /
checkpoint / workflow tasks — with a key the SERVER holds, and Daintree pays. Model
*identities* named in this repo are the backend's upstream route ids, not direct provider
integrations, and where a comment names model-specific protocol behaviour it means "that
model's behaviour when reached through the backend's upstream." The CLI carries no
provider key, no provider client, and no pricing table; reintroducing one would let a
handler bypass the backend that owns prompts, runbooks, and credentials.

## Endpoint and credentials

Two constants: `backend.DefaultBaseURL` = `https://assistant.daintree.org` (the deployed
backend, and the default for a fresh install) and `backend.LocalBaseURL` =
`http://127.0.0.1:8473` (a backend you run yourself).

`assistant.daintree.org` is a **staging** deployment being secured in place
(`ENVIRONMENT=staging`, browser account and payment links on `staging.daintree.org`): its
`AUTH_MODE` walks `open` → `observe` → `enforce` by config-only revision, with entitlement
staged behind identity on its own axis. Nothing on this side may encode where that walk has
reached — not in a constant, not in a doc, not in a hostname test.

**Whether a request carries a credential is the deployment's answer.** The backend holds
its own upstream credential and funds every turn from it, so a request needs to carry no
key in order to have one to spend: `auth.authenticate` returns an anonymous principal for a
request with no `Authorization` header, and that stays a first-class path — it is what a
local backend, the e2e fakes, and any deployment short of `enforce` serve. The CLI never
prompts for a provider key, never writes one to disk, and never gates startup on one.

The CLI owns ACCOUNTS (`internal/auth`, and `daintree-assistant auth …`), and when a
deployment configures an identity provider it sends the account's access token as the
bearer on protected paths. Discovery decides: `GET /v1/daintree/auth/config` answers with
`configured` and `required`, and a deployment with neither returns just those two flags —
no issuer, no client id — which the CLI reads as "no accounts here", not as a fault. The
protected/public split matters here: `/healthz`, `/readyz`, `/version` and the discovery
endpoint itself never wait on a credential, because they are exactly what someone probes
when their login is broken.

**`GET /v1/daintree/account` is TWO documents under one version.** Every response carries
`version`, `access` and a valid 16-hex `subject_hash`, and may carry `email`. An
`unverified` response stops there: identity is established, entitlement was never looked
up, so it carries no plan, no source, no stale flag and no `checked_at` — there was
nothing to timestamp. The other three report a completed lookup and must carry
`checked_at`, `entitlement_source` and `entitlement_stale` together; `granted` also
requires a `plan_id`. The stale flag is decoded presence-aware for a reason: absent reads
as "not stale", so a lookup that never said would be rendered as "we checked, and this is
current". It is the only field whose wire presence survives decoding — a `null` string is
indistinguishable from an omission — so the `unverified` rules are checked on the decoded
value.

This decoder is a SUBSET of the server's contract, not a mirror of it. It ignores unknown
fields (the server forbids extras) and does not repeat every cross-field rule the server
already enforces. It is the part that decides what a user is told, and a body that breaks
it is a LOCAL `account_contract_invalid` (`internal/backend/accountstatus.go`) sitting
deliberately outside `accountCodes` — malformed data is a statement about the backend, and
must never reach a user as "you are signed out" or "you are not subscribed". The canonical
bodies live in `internal/backend/accountfixture` and are decoded by every package that
reads this contract.

**One read serves every surface** (`internal/app/accountrefresh.go`). `auth status
--refresh` and `/account` read as the user asking, through the OBSERVING client, so a
revocation clears the credential. The checks after `auth login` and `/login` are a
COURTESY and run unobserving — a plan report must not be able to revoke a session the
token exchange completed seconds earlier. A plain `auth status` makes no account request
at all.

**The backend's account verdicts change local state.** The codes in `account.go` are not
just for display: a protected 2xx confirms the session, `auth_token_expired` gets one
refresh and one replay (never after anything visible has streamed), `auth_session_revoked`
deletes the stored credential, and the 402/503 families preserve it. See
`backend.AccountObserver`.

Three surfaces carry that, and what each one actually does:

| surface | state today | what it does |
|---|---|---|
| `DAINTREE_API_KEY` → `cfg.APIKey` → `Authorization: Bearer` | unset on a normal install | supplies a bearer identifying the CALLER. It does not fund anything: in `open` mode the backend does not read it, and in `observe`/`enforce` it is verified as an account token. Every model call is funded by the server's own credential regardless. It takes precedence over the account manager on this side, so setting both is a configuration mistake rather than a precedence question |
| `App.Backend` is a `backend.Swappable` | `/backend` swaps it | a different deployment is a different account authority too, so the swap rebuilds the client AND the manager. An ordinary token refresh does not come through here — `TokenSource` changes the credential for the same endpoint, one level below. An account read that CROSSES a swap is discarded, not applied: `App.RefreshAccount` fetches outside `cfgMu` and commits under it, and `/backend` needs the write lock, so an answer describing the endpoint a session has just left can never reach the new endpoint's manager. The same `Discarded` outcome also covers a login, logout or revocation moving the generation mid-read |
| `POST /v1/daintree/auth/verify` | `doctor` is the only caller | answers for the PROVIDER credential the request would spend — the backend's own, normally — so it is the one probe that says "this deployment can actually run a turn". It is not a question about the caller's account |

A **malformed** bearer is still a `401 invalid_api_key`, and that asymmetry is deliberate:
absence is a valid choice, but a header that is present and unusable is a mistake, and
silently ignoring it would show a caller a successful authentication while every turn ran
as an anonymous principal. The CLI shape-checks `DAINTREE_API_KEY` in `config.LoadConfig`
for the same reason, one layer earlier — nobody is prompted any more, so a mangled value
arrives from the environment and would otherwise die inside `net/http` as "invalid header
field value" on every turn, naming neither the variable nor the cause.

These two conditions remain distinct by **code**, never by status:

| condition | code | CLI predicate | meaning |
|---|---|---|---|
| malformed bearer (when one is sent at all) | `invalid_api_key` | `Error.IsAuth()` | fix your header |
| well-formed key the provider rejects | `provider_invalid_api_key` | `Error.IsUpstreamAuth()` | fix the account behind the key |

The status is not the contract for either row. `invalid_api_key` is a 401 by nature — it is
this request's own header that is wrong. `provider_invalid_api_key` is not: it reports the
DEPLOYMENT's own upstream credential failing, a server-side fault, and it has been served as
a 401 while moving to a 5xx. **Do not encode either number.** The CLI is indifferent by
construction: `Error.IsAuth()` short-circuits on the provider codes before it ever consults a
status, and `deterministicUpstreamCodes` in the retry layer lists `provider_invalid_api_key`
by code, so both the classification and the no-retry decision hold at any status — including
the `HTTPStatus == 0` a mid-stream SSE error carries. That is what lets the two repos move
this one independently instead of in a flag day.

The COST path used to be the exception, and is not any more. `taskMayHaveBilled` decides
whether a failed call may already have spent money, and it now answers by CODE first.
`billedVerdictCodes` — the codes meaning a generation demonstrably ran,
`task_output_invalid` today — is checked above every status-shaped arm; the provider-refusal
codes (`provider_invalid_api_key`, `provider_key_forbidden`,
`provider_insufficient_credits`, `upstream_no_compliant_provider`) get arms of their own
rather than being caught as "a 401" and "a 403"; and `auth_credential_unavailable` is
decided by code too, since it carries no HTTP status at all and a status arm would miss it
entirely (it is raised before dispatch, so nothing left the machine and nothing can have
billed). The remaining 401/403 arm is orientation for a code this build does not recognise,
and it is LAST on purpose. Ordering is the argument: over-caveating an accurate total is
cosmetic, while reporting a real charge as free is a number the user cannot recover — so a
code that names spend must never be overridable by a status. If a status-shaped arm ever
misclassifies a real code, give that code its own arm above; never add a number below.

`IsAuth()` deliberately excludes the provider codes: telling someone whose credential the
provider revoked to "check you pasted it in full" sends them round a re-entry loop that
cannot work. See the upstream taxonomy below.

### Verifying that a turn can actually be funded

```
POST /v1/daintree/auth/verify
  →  {"valid": true, "usable": true, "reason": "ok", "detail": "...",
      "label": "...", "limit_remaining": 1.23}
```

It asks the provider directly (a model listing — no tokens spent) about the key this
request would spend, which is the BACKEND's own upstream credential on every install: the
CLI ships no provider key, and neither an account sign-in nor a caller bearer gives it one
— both are account tokens, and neither reaches the provider. `valid:false` comes back as
**200**, not 401: "this key is invalid" is a successful answer to the question, and a 401
would tell the client to retry the same header. A provider we cannot reach propagates as 502 `upstream_error`, because
then we do not know — and "could not check" must never be reported as "invalid".

`valid` and `usable` answer different questions — "does the provider recognise this
credential" versus "can the account behind it fund a turn" — and `reason` (`ok` /
`provider_rejected` / `credits_exhausted`) is the stable outcome to branch on. A
recognised key with a spent balance is `valid: true, usable: false`; a client reading
only `valid` reports health and then fails on the first real request.
`KeyVerification.Usable` is a `*bool` so a backend that predates the field decodes as
"not reported" rather than as `false`, with `IsUsable()` falling back to
`limit_remaining`. Cerebras reports neither a label nor a budget — its probe answers with
a model listing — so both are normally absent, and the doctor row is written to read
correctly without them.

`doctor`'s `upstream credential` row is the whole consumer. It always reports on the
backend's OWN key — that is the credential `/auth/verify` answers for — so it attributes
the credential to the deployment and routes every rejection there. When
`DAINTREE_API_KEY` is set the row says so, as context: an account bearer in play is worth
knowing about, and worth explicitly ruling out as the cause.

**A backend that does not serve the route at all** is scoped by
`backend.AllowsUnverifiedSignIn`:

| endpoint | `/v1/daintree/auth/verify` absent (404 / 405 / 501) | rationale |
|---|---|---|
| any **remote** host | **doctor failure** | the deployed backend has served the route since 2026-08, so its absence means an obsolete deployment or an intercepting proxy |
| **loopback** (`127.0.0.0/8`, `::1`, `localhost`) | reported as unknown, not a failure | the `python -m daintree_assistant_server` development loop, which is routinely mid-change |

The predicate is deliberately **"is this local?"**, never "is this the official
endpoint?". The latter fails **open**: its alias surface is unbounded — `:443`, an empty
port, a trailing DNS root dot, an IDNA spelling, userinfo — and every spelling the check
failed to anticipate would silently take the lenient path against a *remote* host. The
loopback test fails **closed**: no `evil.example` spelling parses to `127.0.0.1`, and an
unparseable URL is treated as remote. Both are pinned by `TestAllowsUnverifiedSignIn`.

404, 405, and 501 all map to `ErrVerifyUnsupported`: the client issues the contractually
correct `POST`, so any of the three is evidence the route is absent or intercepted.
Transport failures and other 5xx do **not** — those mean "could not check", which stays
`unknown` and must never be reported as a verdict about the credential.

### Key hygiene: the client scrubs on the way out

On the normal path there is no caller key for anyone to echo, so this costs nothing. When
`DAINTREE_API_KEY` IS set the bearer identifies an account, and a backend or an
intermediary we do not control can echo the `Authorization` header back at us — enough to
impersonate that account, whether or not it can spend anything. Every path where that could happen
is scrubbed **inside the client**, not at the display sites — there are many sinks (turn
error rendering, doctor rows, the retry hook, the debug-log writer) and exactly one place
they all get their values from:

| path | where | why it needs its own scrub |
|---|---|---|
| `verify` **200** body (`detail`, `label`) | `VerifyKey` | the *success* path — no error wrapper covers it, and it feeds the `doctor` credential row |
| any JSON endpoint's error body | `readErrorResponse` → `scrubBackendError` | scrubbing here also cleans the error handed to the retry-observability hook |
| marshal / decode errors | `doJSON` → `scrubError` | a decoder's message can echo the payload |
| terminal SSE `error` event | `respondStreamOnce` → `scrubError` | `parseRespondStream` is a free function with no access to the key |

`Message`, `Param`, `Code`, and `Type` are all scrubbed: the last two are nominally
stable machine identifiers, but they are still backend-controlled strings. This is
defense-in-depth, not a licence for the backend to leak — but custom endpoints and
proxies are outside our control, so the client cannot assume good behaviour upstream.

### Loopback URLs never take a proxy

The default transports use `proxyExceptLoopback`, not `http.ProxyFromEnvironment`. Go's
stock bypass fires only for the exact lowercase host `localhost` and parseable loopback
IP literals, while `AllowsUnverifiedSignIn` also accepts `LOCALHOST`, `localhost.`,
`dev.localhost`, and `127.0.0.1.` — all of which genuinely address this machine. Left
unaligned, those four spellings would be treated as trusted-local by one predicate while
being *routed* through `HTTP_PROXY` by the other: over plain http the proxy would see
whatever bearer the request carries in clear text, and would sit in the middle of a turn
it was never meant to see. Deriving both from the same predicate makes them agree by
construction, so widening the loopback definition later cannot silently reopen the gap.
Pinned by `TestProxyIsBypassedForEverySpellingClassifiedAsLocal`.

**The CLI never probes a provider itself.** It holds no provider client by design, and
the credential the account system will issue is one only the backend can resolve. Adding
a direct provider call here would break both properties.

### Overrides

`DAINTREE_BACKEND_URL` and `DAINTREE_API_KEY` are **trusted-env only** — a bound project's
`.env` can supply neither. That is a security boundary, not tidiness: the URL decides
where a turn is sent, and the key, when present, decides which account it is sent AS, so a
cloned repo must not be able to inject either.

**The URL override is not the whole endpoint mechanism** — an interactive `/backend`
choice is persisted too. Four sources resolve, highest first:

1. `--backend-url <url>` on the command line.
2. `DAINTREE_BACKEND_URL` (trusted env).
3. The endpoint stored by `/backend` — a 0600 `endpoint.json` at the per-user state root
   holding **only** `{backend_url}` (`internal/config/endpoint.go`). It is a preference,
   never a credential.
4. `backend.DefaultBaseURL`.

Env outranks the stored preference deliberately: a harness, an e2e run or CI must never be
silently redirected by a choice someone made in an interactive session months ago. Because
that ordering otherwise reads as a broken `/backend`, `cfg.BackendURLPinnedByEnv` makes the
command say which layer won. `/backend` with no argument reports the resolved endpoint,
`/backend <target>` (`local`, `official`, a number, or a URL) swaps the `Swappable` in place
**and** persists, and `/backend default` forgets.

A stored preference that fails validation degrades to the default rather than bricking the
launch — the same contract as an unreadable one — and the reason is surfaced so the CLI can
name it instead of silently dialling somewhere else. Two diagnostics, deliberately not one:
`cfg.EndpointInsecureRejected` for plaintext to a remote host, which is a security refusal a
user may have meant and can authorize, and `cfg.EndpointShapeRejected` for everything else
(userinfo, a query string, a bad scheme, an unparseable URL), which is a repair job.
Collapsing them would report a malformed endpoint as a security decision.

All four sources go through `backend.NormalizeBaseURL`, which is the single door: one
validator, so a value the interactive command flatly refuses cannot be quietly accepted at
launch instead.

The env override is still the local dev loop in its entirety:

```bash
cd ../assistant-backend
python -m daintree_assistant_server            # serves on 127.0.0.1:8473 (its .env pins the port)

DAINTREE_BACKEND_URL=http://127.0.0.1:8473 daintree-assistant
```

e2e tests use the same override to point at a fake backend. They need nothing else: a
fake that serves no discovery route leaves account state UNKNOWN rather than "no accounts
here" — but a machine with no stored credential short-circuits to an anonymous request
either way, so nothing gates them.

A remote endpoint's plaintext `http://` is refused by default, and `config.LoadConfig` gets
that from the same single door as everything else: `backend.NormalizeBaseURL` applies the
plaintext rule (`backend.ValidatePlaintextRemote`) alongside the shape rules, so there is one
decision rather than two that can drift. A request may carry no bearer, but a turn's prose,
tool arguments and results all cross that wire, and plaintext to anything but this machine is
a confidentiality failure regardless. Loopback (the local dev
loop above) stays permitted unconditionally. The escape hatch for a deliberately plaintext
remote endpoint is `--allow-insecure-backend` / `DAINTREE_ALLOW_INSECURE_BACKEND=1`
(trusted-env only).

## Wire contract

Authoritative spec: `../assistant-backend/docs/DAINTREE_API.md`. Exact field types:
`../assistant-backend/src/daintree_assistant_server/contracts/daintree_api.py`. Request
validation: `.../services/validation.py`.

The Go client mirrors it in `internal/backend`:

- `contracts.go` — the strict request envelope (`RespondRequest`: `protocol_version`,
  `session{id,turn_id,instruction_revision,round}`, `state`, `startup`,
  `input{messages,tools,tool_choice}`,
  `runtime`, `turn`, `selection`, `generation`, `client`), the response / stream payloads,
  the tasks envelope, and capabilities.
- `sse.go` — the **named-event** SSE parser (`meta` → `delta` → `done` / `error`). `meta`
  is always first (carries the refreshed `state` token + the first-class `runbooks` block)
  and is flushed as soon as selection finishes, before the upstream model connects. The
  client immediately emits de-duplicated `newly_loaded` refs through `OnRunbookLoaded`, while
  deferring stateful `OnMeta` until an attempt commits. A retry after meta adopts its signed
  `state` in the next POST so the backend reuses the same selection. If a terminal retry
  then dies before its own meta, the last received/adopted meta is forwarded once so that
  state is still persisted. Terminal error events may carry a top-level `retry_after`
  string, which feeds the bounded retry delay; provider timeouts are transient. Tool-call
  deltas accumulate by index (OpenAI-style fragments). EOF before `done` is an error — the
  parser never fabricates a successful finish.
- `client.go` — `RespondStream`, `RunTask`, `Capabilities`, `Health`, `Ready`, `Version`.
- `retry.go` — the transient-failure retry policy applied to **every** call above (10
  attempts, exponential from 500ms, settling into a 10–15s poll ≈ 50–75s of backoff,
  the whole call capped by a 2-minute elapsed window — sized to ride out a backend
  restart). Never replays a deterministic failure (auth 401/403, contract 400, protocol
  426), an application **verdict** (`task_output_invalid`, `upstream_error`,
  `internal_error` — keyed on `error.code`, since the backend reuses 502 for both a
  verdict and a real gateway failure; a replay would re-run the model to reach the same
  answer), or a respond turn that has already streamed visible tokens. `WithoutRetry(ctx)`
  makes a call one-shot, for `/doctor`'s probes. See
  [RUNTIME.md](RUNTIME.md#model-errors-rate-limit-backend-down-unavailable) for how it
  layers with the backend's own provider retries and the MCP read retries.

### Endpoint routing

The backend picks which upstream endpoint serves a request. Two of those decisions are
still legitimately the CALLER's — which compliant endpoint sees their source, and how the
pool is ranked — so `/v1/daintree/respond` accepts an optional `routing` block:

```jsonc
"routing": {
  "privacy": "no_training",   // "no_training" (default) | "zdr"
  "sort":    "throughput",    // "throughput" (default) | "price" | "latency"
  "only":    ["deepinfra"],   // optional endpoint allowlist (≤24 slugs)
  "ignore":  ["some-slow-one"]
}
```

A **closed set, not a pass-through** — an arbitrary provider block would let a client drop
the no-training floor or pin an endpoint that ignores `response_format`, each failing as
an inscrutable upstream error rather than a validation one. The CLI validates the same
closed set locally (`internal/backend/routing.go`), so a mistyped `DAINTREE_ROUTING_SORT`
fails at startup naming the valid choices instead of 400-ing mid-turn.

Two things a caller cannot do, by the backend's design: **weaken privacy** (the
no-training filter is sent unconditionally and is not derived from this block; `zdr` is
additive), and **guarantee a route** (a strict mode plus a narrow allowlist can empty the
pool, which fails closed as `upstream_no_compliant_provider` rather than quietly relaxing
the filter).

Config is **trusted-env only** — never a project `.env`, the same boundary the endpoint
and the optional bearer sit behind. A bound repository cannot drop the no-training floor (the backend sends
that unconditionally), but it could pin every request to an endpoint of its choosing, or
quietly cancel a user's zero-retention choice. Which compliant endpoint sees someone's
source is not a decision a checked-in file should make.

The preference is stamped by the CLIENT (`internal/backend/client.go`), so it rides
**both** `/respond` and `/tasks`. A task ships the caller's content upstream exactly as a
turn does — terminal tails, transcripts, memories — so a privacy choice honoured only on
the visible path would be the most misleading kind of half-measure.

The CLI cannot observe the upstream request, so it never claims a mode is *in force*: the
masthead says "requested", and `/routing` and `/doctor` report whether the backend accepts
the field at all. A configured non-default policy against a backend that does not accept
one is a **failing** `/doctor` row, not a silent downgrade.

The privacy **wording** is served, never composed locally: `capabilities.routing.
privacy_description`. OpenRouter models "does not collect or train on" and "does not
retain" as two separate filters, and only the first holds under the default — a client
that summarised in its own words would eventually write "does not store", which would be
false. `/routing` renders the served sentence; the masthead announces only a
NON-DEFAULT policy, since the default is what every install runs.

### Cost reporting

Daintree funds every upstream call now, so a reported cost is no longer the caller's bill
— but it is still the only honest measure of what a turn actually spent, and the number
that says whether a change made the assistant cheaper or more expensive. The backend
reports the provider's own figures — never a token-price estimate, because the router
knows which endpoint served the call and what cache discount applied, and anything derived
client-side would be a guess presented as a bill.

| where | field | meaning |
|---|---|---|
| `/respond` body, and the terminal SSE `done` event | `cost: {total, main, selector, complete}` | the whole request, across every upstream call it made |
| `/respond` `usage.cost` | float | the MAIN completion only |
| `/tasks` `usage.cost` | float | that task's total for the attempt that answered (a repair pass is included; an HTTP attempt this one replaced is not) |
| `/v1/daintree/capabilities` | `respond.cost_reporting` | the contract, advertised so a client can degrade |

Two rules the CLI **implements** rather than infers, because getting either wrong
under-reports what a turn spent while looking like a receipt:

1. **Absent means unknown, never free.** The block is omitted rather than zero-filled.
   Test key PRESENCE, not `!= null`.
2. **`complete: false` means `total` is a floor.** A call ran whose cost could not be
   measured — an unreported selector, or a speculative generation cancelled mid-flight
   (OpenRouter reports usage only in a stream's final chunk, which a cancelled stream
   never sends). A turn that *skipped* selection stays `complete: true`: no call
   happened, so nothing is missing.

Collapsed to one client rule: **a session total is a lower bound if any request in it was
incomplete or carried no cost block at all.**

Two gaps `complete` does not currently close, both of which the CLI handles on its own
side. A **retried** call can have billed on more than one HTTP attempt, and each attempt's
`usage.cost` covers only its own request — the backend aggregates re-rolls *within* a
request, never across HTTP attempts. For `/respond` the client forces `Complete=false` when
an earlier attempt got as far as `meta` (i.e. the selector already ran) and then failed. For
`/tasks` the same rule is enforced in the transport: before replaying a transient failure,
`doJSONRetry` asks `taskMayHaveBilled` whether the attempt it is about to REPLACE could have
billed and remembers the answer as `spendAbandoned`; `RunTask` then reports the task's
figure with `Complete: !spendAbandoned`. So a task that succeeded after a billed-looking
retry reports a **floor**, not the bill — and the flag has to be carried on the SUCCESS
path, because a call that eventually succeeds hides exactly the same money as one that
eventually fails. (The one-shot credential-refresh replay is not asked: it only ever
replaces an identity error, which is refused at the door and provably free.) The two
questions are also asked separately and in order: `spendAbandoned` answers for replaced
attempts, `taskMayHaveBilled(err)` only for the error in hand, which is the last one.

And a **failed** call may still have billed: `task_output_invalid` is raised only after a
billed completion, often after a second billed repair, so a failed task reports unknown
spend unless the failure proves no generation can have run — a refused socket, a contract
error, an account verdict at the backend's own door, an unfunded or unroutable request, or a
credential this process could not produce at all. Note that the proof is the CODE, never the
status: `task_output_invalid` wearing a 400 is still billed, which is exactly why
`billedVerdictCodes` is consulted first.

One gap remains on the backend side and is tracked as
[assistant-backend#31](https://github.com/daintreehq/assistant-backend/issues/31): a
re-rolled generation where only one attempt reported its cost yields a partial `total`
still flagged `complete: true`, and `/tasks` has no completeness field at all. Small in
magnitude, but it means `complete` is a floor on how conservative to be, not a guarantee.

`backend.ClientConfig.OnCost` is the single seam — it fires for every respond turn AND
every utility task, because the client is the only layer they both pass through. A day of
orchestration spends real money on summarize/extract/classify tasks fired from tools and
watchers that never appear as a turn. `internal/costledger` accumulates; `/cost` and
`/doctor` render, hedging an incomplete total as `≥ $x` (truncated, never rounded up) and
naming why. The ledger is deliberately unpersisted and counts from process launch or the
last `/clear`; it deliberately outlives the client it measures, so anything that rebuilds
that client mid-session cannot silently zero the bill.

Not counted anywhere: **runbook learning**, which runs fire-and-forget on a stronger model
after the response and can cost more than the turn that triggered it. It is off by
default and forbidden in production, so no beta tester is exposed — but a local developer
who enables it will see dashboard spend that no field here accounts for.

### Phase timings

Alongside `cost`, the `/respond` body and the terminal SSE `done` event carry
`timings` — where the request's wall clock went, measured server-side around real
awaits: `selection_ms`, `docs_ms`, `preparation_ms`, `upstream_open_ms`, `thinking_ms`,
`first_output_ms`, `generation_ms`, `total_ms`. It rides the terminal event for the same
reason cost does: `meta` is emitted *before* the model is opened — which is what makes
meta useful — so it cannot know generation or total.

The CLI decodes it as `backend.TurnTimings` (all fields `*int`) and writes it to the
debug log on `backend.respond.done` as flat `server*Ms` keys; nothing renders it to the
user. Three rules it implements, mirroring the cost block's:

1. **Absent means the phase did not happen, never 0.** The backend serializes with
   `exclude_none`, so a skipped selector is a *missing* key — and decoding it as zero
   would merge it with a selector that answered instantly. Hence pointers, and hence
   `TurnTimings.Any()` for "did this backend report anything at all".
2. **They do not sum to `total_ms`.** Phases overlap by design: a speculative stream
   opens while the selector is still running, so `selection_ms` and `upstream_open_ms`
   can cover the same wall clock.
3. **`total_ms` is the winning attempt only.** A retried respond call measures per
   attempt, exactly as it bills per attempt — which is why the log gates its derived
   `clientOverheadMs` on a round having made no retry.

The CLI measures the other half itself. The server's clock starts when the request lands
and stops when the response completes, so the dial, the TLS handshake, our upload and
the flight home are invisible to it — on the first real turn against the deployed
backend that was 934 ms of a 7.9 s round, and it was *constant* across the round (the
gap at first token and at completion matched to 2 ms), which is the signature of a fixed
pre-request cost rather than a slow stream. `internal/backend/transport.go` captures it
with `net/http/httptrace` (`TransportMarks` on `RespondResult`, populated per attempt so
a retried call reports the winning one), and the same log line carries both halves.

See [`docs/LOGGING.md`](LOGGING.md#where-a-slow-turn-went) for the log keys and how to
read them, and the backend's `docs/DAINTREE_API.md` § Phase timings for the contract.

### The upstream-failure taxonomy

The backend used to collapse every upstream 401/402/403/404 and every 5xx into one
`502 upstream_error`. It now names each condition, because they have different fixes and
only three of them are transient. **`retryable` is server-side and is NOT serialised** —
the CLI classifies from the code itself, so this table is the contract:

| code | status today | retried? | whose problem |
|---|---|---|---|
| `provider_invalid_api_key` | 401 → 5xx (in flight) | no | the credential funding the turn — always the backend's own; a caller bearer never reaches the provider |
| `provider_insufficient_credits` | 402 | no | that account's balance. It is the DEPLOYMENT's account, so it cannot be topped up from the CLI — report it |
| `provider_key_forbidden` | 403 | no | that key's model permissions / spend limit / guardrails, again the deployment's |
| `upstream_no_compliant_provider` | 503 | no | your routing policy matched no endpoint |
| `upstream_rate_limited` | 429 | **yes** | transient (honours `Retry-After`) |
| `upstream_timeout` | 504 | **yes** | transient |
| `upstream_unavailable` | 503 | **yes** | transient provider outage |
| `upstream_request_rejected` | 502 | no | **ours** — report it with the request id |
| `upstream_protocol_error` | 502 | no | **ours** — report it with the request id |
| `upstream_error` | 502 | stream only | the pre-split catch-all — still emitted for a stream error the backend could not classify, and by the key-verification path when the provider could not be reached; a genuine "we don't know", which is why the stream form is worth one more attempt while the pre-stream form (an application verdict) is not |

Two rules make this work, and both are easy to get wrong:

The `status today` column is orientation, not contract, and `provider_invalid_api_key` is the
live example of why: it reports the deployment's own upstream credential failing, a
server-side fault wearing a client-error number, and it is moving from 401 to a 5xx. Nothing
on this side changes when it lands, precisely because of the first rule below — a CLI that had
keyed on the number would have needed a synchronised release of both repos.

1. **Classify on the code, never the status.** The backend emits `meta` before it opens
   the upstream stream, so most of this taxonomy arrives as a terminal SSE `error` event
   with `HTTPStatus == 0`. A status-based rule reverses its answer depending only on how
   far the request got — and a routing dead end (a 503) reads as a transient gateway,
   burning the entire retry budget to re-derive the same empty endpoint pool.
2. **Deterministic ≠ our verdict.** `nonRetriableAppCodes` is pre-stream only (it means
   "the work ran, this is its answer"); `deterministicUpstreamCodes` applies on **both**
   transports (it means "this will hold identically on every replay").

`internal/agent/session.go`'s `upstreamFailureAdvice` renders a distinct reply for each
of these codes except `upstream_rate_limited` (caught earlier by `IsRateLimited()`, which
also raises the health badge) and `upstream_timeout` (which falls through to the generic
`Model error:` with its detail intact). Every reply starts with a registered
wake-failure prefix, so an unattended supervisor wake cannot mistake a failed turn for an
answer. `Error.RequestID` carries the backend's `X-Request-Id` — validated against a
conservative id alphabet and scrubbed like any other field we did not author, since it is
rendered into terminal scrollback — stamped from the HTTP error response and from the
streamed 200's headers for a mid-stream failure, so the two reportable codes produce a
report someone can act on.

The two reportable codes are grouped by their next step, NOT by their culprit, and the
copy says so: `upstream_request_rejected` means the provider judged the request Daintree
built to be malformed (our bug); `upstream_protocol_error` means the provider answered
with something unparseable (usually a provider or compatibility problem).
- `tasks.go` — typed helpers for the server-owned utility tasks.

## Invariants the CLI upholds

- **No `system` / `developer` messages.** Only `user` / `assistant` / `tool` reach the
  backend (the converter `internal/agent/backendconv.go` rejects anything else up front).
  `domain.ControlMessageCount == 0`: no synthetic row is persisted in visible history.
  `input.messages` contains visible history only; startup data never masquerades as a
  user-authored message.
- **Context uses dedicated structured channels.** Required `request.startup` carries
  the cacheable curated project identity, the effective agent catalog (including
  availability and toolbar state), and normalized bounded `DAINTREE.md` instructions.
  The catalog has a 16 KiB encoded-row budget; any row that does not fit is omitted whole
  while `total_count` retains the discovered total, rather than emitting a truncated,
  unusable agent id. `request.runtime` carries tier, MCP, scheduler, a freshly read typed
  worktree snapshot, open terminals, and the terminal geometry the reply renders at. Stable project
  and agent fields are not duplicated in this fresh tail. `request.turn` carries the goal,
  wake, workflow runs, async operations, memories, and `resumed_watchers`.
- **The integration surface names its endpoints.** `runtime.mcp_servers` lists every MCP
  server this process is wired to — the primary Daintree control plane, and nothing else
  since the docs client was removed (issue #332) — each as `name` + a `description` leading
  with its endpoint URL, so the model can
  say WHICH Daintree it is driving instead of guessing (ses_8cb40b4e). The backend renders
  it as a session-**stable** system block ahead of the tool schemas, so the list carries
  endpoints ONLY: connected/transport/tool-count fluctuate mid-session and stay on
  `runtime.mcp`, which rides the volatile tail. Endpoints are sanitized
  (`mcp.SanitizeURL`: no userinfo/query/fragment) because Daintree's per-session URL can
  carry its bearer as a query parameter. The same endpoints are reachable on demand
  through `daintree.status` (MCP) and `context.snapshot` (MCP + the assistant backend URL).
- **Splash-time discovery is bounded and parallel.** A successful MCP connect starts
  `project.getCurrent`, canonical `agent.listAvailable`, `worktree.getCurrent`, and the
  open-terminal warm-up while the logo is animating. The agent action returns the current
  effective built-in/user/plugin launch registry with display names, source, coarse CLI
  availability, and built-in tri-state pin/resolved toolbar visibility. A fast first submit
  joins the whole connect+prefetch gate rather than racing an empty snapshot; the post-logo
  bootstrap awaits that same single attempt instead of reconnecting. The primary lifecycle
  has an 8-second cancellation budget. A completed degraded attempt fails open for later
  turns (manual `/reconnect` owns retries), while an externally cancelled launch remains
  retryable once by bootstrap/the first turn.
- **The cache boundary is intentional.** The backend places the structured stable startup
  block immediately before the append-only conversation and keeps its fresh
  runtime/turn user message at the end. Worktree is re-read on every backend round, so a
  switch only changes the tail. A project or pin change changes the startup block and
  following conversation, but leaves the backend's system prompts and large tool schemas
  cached. Raw `project.getSettings` is never injected because its open-ended values may
  include environment or other sensitive configuration.
- **The reply's own width is reported.** `runtime.display` carries `{columns,
  content_width}` in terminal cells: `columns` is the window, `content_width` the narrower
  measure the assistant's markdown is actually wrapped at (after the attached session's left inset,
  the autowrap gutter, and the `ui.ContentMax` prose cap) — the one the backend's response
  contract is written against. The attached session republishes on every resize
  (`App.SetDisplaySize`), so a dragged window lands on the next round. An omitted block
  means "unmeasured" — a piped one-shot, the stdio host, the headless daemon — and the
  backend applies its own default width rather than being handed a fabricated 80×24.
  **Gated on `capabilities.respond.display_context`:** `runtime` is validated with
  `extra="forbid"`, so a backend that predates the field would 422 the whole turn; the CLI
  fails closed and withholds the geometry until a handshake advertises support (the
  descriptor is cached by `App.BackendCapabilities` and re-fetched when the endpoint
  changes). Delete the gate once no such deployment is reachable.
- **Worktree read state is explicit.** An omitted `runtime.worktree` means the live read was
  unavailable, `{current:null}` means Daintree definitively reports no current worktree,
  and a current object carries id/path/branch/issue/PR/status/last-commit fields.
- **Runbooks are server-owned.** No `runbook.find` / `runbook.load` (reserved + rejected). The
  backend's selector picks and injects runbooks and returns a `runbooks` block. The CLI
  folds NONE of it into the conversation, and there is no `/runbooks` command. Backend runbook
  selection is prompt-assembly machinery the user neither approves nor steers, so
  `newly_loaded` feeds the debug trace, the durable run log and the `--json` stream — and
  surfaces to a human only in an explicit `/explain <run>` replay. The CLI keeps only the
  local run-tracking tools
  `runbook.run.get` / `runbook.step.advance` (the backend prompt drives them).
- **Opaque state token.** `meta.state` is stored verbatim and replayed on the next request;
  the CLI never inspects, signs, or mutates it. A missing token is valid for a new session.
- **One `turn_id` per user request** across the whole tool-call loop; `round` increments
  per continuation; `instruction_revision` bumps when a mid-turn injection is folded in.
- **Utility work is server-owned tasks** (`/v1/daintree/tasks`): `checkpoint`,
  `memory_distill`, `watcher_classify`, `terminal_judge`, `terminal_summarize`,
  `terminal_extract_text`, `terminal_extract_json`, `extraction_verdict`,
  `runbook_step_consistency`, plus the flag-gated `workflow_plan`,
  `workflow_reconcile`, `workflow_resume_digest`. The CLI sends task *data* only —
  never prompts.

### Task ids are a frozen wire contract

The ids above are hardcoded on BOTH sides and must change in lockstep:

| Side | Manifest | Guard |
| --- | --- | --- |
| CLI | `internal/backend/tasks.go` (`coreTaskIDs`) + `workflowtasks.go` (`workflowTaskIDs`) | `taskcheck_test.go` parses the AST and fails if a `Task*` constant is not in the manifest |
| Backend | `task_runner` profiles | `EXPECTED_TASK_IDS` in `tests/unit/test_task_runner.py`, asserted as an exact SET |

`backend.CheckTasks(caps, workflowIntelligence)` diffs the CLI's manifest against the
live `Capabilities.Tasks`. It surfaces in three places: the `backend tasks` row in
`/doctor`, the `doctor` subcommand's banner (**and its exit code** — drift is a gating
failure), and a `backend.tasks` debug-log line at boot.

Three rules the checker encodes:

- **Extra server tasks are fine** — forward compatibility, not drift.
- **An unreported inventory is "cannot verify", never a failure.**
  `/v1/daintree/capabilities` sits behind `require_auth` *and* `require_ready`, so a
  warming backend legitimately advertises nothing.
- **Workflow ids are required unless `DAINTREE_WORKFLOW_INTELLIGENCE=0`** (ON by default).

> Why the machinery: on **2026-07-07** the backend dropped a `.v1` suffix from every
> task id. The count was unchanged, and *both* sides asserted only a count — so every
> test stayed green while every task call 404'd mid-turn. Never assert `len(tasks)`.

## When changing protocol behavior

Inspect both sides:
`../assistant-backend/docs/DAINTREE_API.md`,
`../assistant-backend/src/daintree_assistant_server/contracts/daintree_api.py`,
`../assistant-backend/src/daintree_assistant_server/services/validation.py`.
