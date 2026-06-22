# Phase A scaffold — module foundation

Records what the Phase-A scaffold established so Phase-C agents implement against it.

## Module

- Path: `github.com/daintreehq/daintree-assistant`
- `go` directive: **`go 1.25.8`** (raised from `1.25` — `charm.land/glamour/v2` forces it;
  see `_deps.md`).
- Dep versions: see `docs/port/_deps.md`. `go build ./...` and `go vet ./...` both pass;
  `go mod verify` → all modules verified.

## Package layout

```
cmd/daintree-assistant/      main.go — entrypoint: --help/--version/--tier, doctor stub
internal/domain/             pure vocabulary (imports nothing internal)
  enums.go                   RiskClass, Tier, ModelTier, AgentState, Severity (+rank),
                             EventSource, EpistemicKind, WatcherClassification,
                             VerificationVerdict, JsonOutputStatus, JsonlEventType,
                             RecommendedActionVerb, all string-union types, ToolActor
  runphase.go                RunPhase enum (+String/IsTerminal) — shared agent+ui
  ids.go                     ID prefixes + NewID(prefix) → prefix + 8 hex of uuid v4
  time.go                    EpochMS, NowMS/ToTime/FromTime
  toolresult.go              ToolResult/ToolError + Ok()/Fail() (+ FailOption, codes)
  constants.go               VerificationEvidencePrefix, JSONOutputSchemaVersion,
                             OneShotExitCode, agent-loop magic constants
  watchcondition.go          WatchCondition recursive DSL + UnmarshalJSON guards
  contracts.go               WatcherVerdict, ModelJudgeAnswer (field order preserved),
                             VerificationResult, JsonlEvent, JsonResultEnvelope,
                             QueuePublishArgs, EventTarget, RecommendedAction,
                             ClassificationEpistemicKind()
  records.go                 all DB-row structs + QueueEvent + QueueDigestOptions
  events.go                  AgentEvent tagged-union vocabulary (what EventSink carries)
internal/config/             LoadConfig + trusted-env boundary, ProjectIDToDir,
                             FirstString, DescribeConfig, DEFAULTS, AppConfig, ConfigOverrides
internal/debuglog/           StartDebugLog/LogDebug/CurrentDebugLogPath, prune, 0700/0600
internal/projectinstructions/ Load(projectPath) → Result (DAINTREE.md, 16KiB cap)
internal/ports/              interface seams: EventSink (+NoopSink, MultiSink),
                             Store, Router, ToolRegistry, MCPClient, Queue
internal/deps/               blank-import anchor (delete as Phase C imports for real)
```

`internal/domain` imports only `github.com/google/uuid` and stdlib — no internal package,
no UI/network/storage. Subsystem packages (storage/models/mcp/tools/agent/daemon/ui) are
deliberately ABSENT; Phase C creates them.

## Interface seams (implement to these signatures)

All in `internal/ports`. Imports `internal/domain` and `context`.

```go
// Event fan-out
type EventSink interface { Emit(ev domain.AgentEvent) }
// NoopSink{}.Emit discards. NewMultiSink(sinks ...EventSink) *MultiSink fans out
// with per-sink panic recovery (a UI sink can never crash the loop).

type Store interface {
    AppendRunEvent(ctx context.Context, ev domain.RunEventRecord) error
    AppendAudit(ctx context.Context, rec domain.AuditRecord) (string, error) // returns audit id
    Close() error
}

type ChatMessage struct { Role, Content, ToolCallID string }
type ModelChunk struct { Content string; ToolCalls []domain.ToolCallInfo; Done bool; Usage *domain.Usage }
type Router interface {
    Stream(ctx context.Context, tier domain.ModelTier, messages []ChatMessage) (<-chan ModelChunk, error)
}

type ToolRegistry interface {
    Dispatch(ctx context.Context, actor domain.ToolActor, name, argsJson string) (domain.ToolResult, error)
    Has(name string) bool
}

type MCPClient interface {
    CallTool(ctx context.Context, name, argsJson string) (domain.ToolResult, error)
    Connected() bool
}

type Queue interface {
    Publish(ctx context.Context, args domain.QueuePublishArgs) (domain.QueueEvent, error)
    Digest(ctx context.Context, opts domain.QueueDigestOptions) ([]domain.QueueEvent, error)
    Resolve(ctx context.Context, id string) (bool, error)
}
```

These are intentionally MINIMAL — the core methods only. Each interface's doc comment
points at the docs/port spec describing its full surface; Phase C grows the concrete
package's own type and adds methods as needed (the seam keeps the agent loop compiling
without import cycles).

## Notable fidelity points carried from the spec

- Severity **rank** (`domain.SeverityRank`, `RankOf`) ≠ enum declaration order; unknown ⇒
  rank 1 (info), mirroring the SQL `CASE ELSE 1`.
- Tier **fails closed**: unset ⇒ `system`; explicit-but-invalid ⇒ `supervisor`.
- Config **trusted-env boundary**: `os.Environ()` snapshotted BEFORE `godotenv.Load`;
  tier/autoApprove/offline/stateDir/logDir read only from the snapshot or an override.
- `WatchCondition.UnmarshalJSON` enforces every degenerate-condition guard (exactly one
  variant key; non-empty contains/modelJudge; compilable regex; positive noOutputForMs;
  non-empty all/any) — a degenerate condition would create a watcher that can never fire.
- `ModelJudgeAnswer` fields declared `Reason, Confidence, Matched` (encoding/json emits in
  declaration order — load-bearing implicit chain-of-thought).
- `ProjectIDToDir` reproduces the exact slug + sha256[:8] algorithm (path wire-compat).
- IDs are `<prefix><8 hex of uuid v4>`; prefixes in `domain` (`PrefixTimer` … `PrefixAgentLaunch`).
```

## Phase-C note: host wave

`internal/host` (`host --stdio`) bumped **PROTOCOL_VERSION 1 → 2** for the
Electron-MessagePort → stdio-NDJSON transport swap. Daintree's
`ASSISTANT_HOST_PROTOCOL_VERSION` must follow. See `docs/port/_newdeps.md`. The
host drives the runtime through the `host.App` seam (not the concrete
`internal/app.App`); the cockpit/cli wave fills `hostAppFactory`.
