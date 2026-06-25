// Package memory holds the cross-session project-memory tools: memory.recall,
// memory.list, memory.save, memory.forget, memory.pin, memory.unpin. Memories
// are FTS5-backed (recall is BM25-ranked) and scoped to one project (one DB ==
// one project, so there is no projectId). forget is a soft-delete.
package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

const (
	codeInvalidArgs    = "INVALID_ARGS"
	codeMemoryNotFound = "MEMORY_NOT_FOUND"
)

// Practical input caps (rune counts). An
// unbounded recall query becomes an expensive FTS5 MATCH; unbounded saved content
// / category / id bloat the index and the MATCH parser. These are generous —
// far above any legitimate use — and exist to stop pathological/adversarial input,
// not to constrain normal calls.
const (
	maxQueryRunes    = 4_000
	maxContentRunes  = 100_000
	maxCategoryRunes = 200
	maxIDRunes       = 200
)

// overLimit reports whether s exceeds n runes (counted, not bytes, so a
// multibyte string isn't rejected early).
func overLimit(s string, n int) bool { return len([]rune(s)) > n }

// RecallOptions / ListOptions mirror the DB query params.
type RecallOptions struct {
	Category string
	Limit    int
}

type ListOptions struct {
	Category   string
	PinnedOnly bool
	Limit      int
}

// Store is the slice of storage the memory tools touch.
type Store interface {
	RecallMemories(ctx context.Context, query string, opts RecallOptions) ([]domain.MemoryRecord, error)
	ListMemories(ctx context.Context, opts ListOptions) ([]domain.MemoryRecord, error)
	InsertMemory(ctx context.Context, rec domain.MemoryRecord) (string, error)
	ForgetMemory(ctx context.Context, id string) (bool, error)
	PinMemory(ctx context.Context, id string) (*domain.MemoryRecord, error)
	UnpinMemory(ctx context.Context, id string) (*domain.MemoryRecord, error)
}

// Deps is the dependency set for the memory family.
type Deps struct {
	Store Store
	// OnChange, when set, is invoked after a successful pin/unpin/forget so the caller
	// can refresh the injected pinned-memory block in the runtime context (message[1]).
	// Optional + best-effort (nil-safe, panic-guarded) — a refresh failure must never
	// fail the tool call. Not fired for save (a new memory is unpinned, so it does not
	// affect the injected block) or for the read-only recall/list tools.
	OnChange func()
}

// notifyChange fires the optional OnChange hook, swallowing any panic — the pin-state
// mutation already succeeded, so a refresh side-effect must not turn it into a failure.
func (d Deps) notifyChange() {
	if d.OnChange == nil {
		return
	}
	defer func() { _ = recover() }()
	d.OnChange()
}

// Tools returns the memory tool family.
func Tools(deps Deps) []*tools.Tool {
	return []*tools.Tool{
		newRecallTool(deps),
		newListTool(deps),
		newSaveTool(deps),
		newForgetTool(deps),
		newPinTool(deps),
		newUnpinTool(deps),
	}
}

// memoryView is the tool-boundary projection. pinned = pinnedAt != null. kind
// always shows (defaulting to semantic); expiresAt/runId surface only when set.
// sessionId is internal provenance and deliberately omitted.
func memoryView(r *domain.MemoryRecord) map[string]any {
	kind := r.Kind
	if kind == "" {
		kind = domain.MemoryKindSemantic
	}
	view := map[string]any{
		"id":        r.ID,
		"content":   r.Content,
		"source":    r.Source,
		"kind":      kind,
		"pinned":    r.PinnedAt != nil,
		"createdAt": r.CreatedAt,
		"updatedAt": r.UpdatedAt,
	}
	if r.Category != nil {
		view["category"] = *r.Category
	}
	if r.ExpiresAt != nil {
		view["expiresAt"] = *r.ExpiresAt
	}
	if r.RunID != nil {
		view["runId"] = *r.RunID
	}
	return view
}

func views(rows []domain.MemoryRecord) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for i := range rows {
		out = append(out, memoryView(&rows[i]))
	}
	return out
}

// --- memory.recall ---

type recallArgs struct {
	Query    string `json:"query"`
	Category string `json:"category,omitempty"`
	Limit    *int   `json:"limit,omitempty"`
}

// Validate caps the recall query + category so an unbounded MATCH can't be issued.
func (a *recallArgs) Validate() error {
	if overLimit(a.Query, maxQueryRunes) {
		return fmt.Errorf("query must be at most %d characters", maxQueryRunes)
	}
	if overLimit(a.Category, maxCategoryRunes) {
		return fmt.Errorf("category must be at most %d characters", maxCategoryRunes)
	}
	return nil
}

var recallSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["query"],
  "properties": {
    "query": { "type": "string" },
    "category": { "type": "string" },
    "limit": { "type": "integer", "minimum": 1, "maximum": 50 }
  }
}`)

func newRecallTool(deps Deps) *tools.Tool {
	return &tools.Tool{
		Name:        "memory.recall",
		Description: "Recall project memories most relevant to a query (BM25-ranked full-text search).",
		Risk:        domain.RiskRead,
		Schema:      recallSchema,
		Decode:      tools.StrictDecoder(func() any { return &recallArgs{} }),
		Handle: func(ctx context.Context, args json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a recallArgs
			if err := tools.DecodeStrict(args, &a); err != nil {
				return tools.Fail(codeInvalidArgs, "Invalid arguments for memory.recall: "+err.Error())
			}
			if strings.TrimSpace(a.Query) == "" {
				return tools.Fail(codeInvalidArgs, "memory.recall: query is required")
			}
			limit := clampLimit(a.Limit, 10, 1, 50)
			if deps.Store == nil {
				return tools.Ok("No memories (storage unavailable).", map[string]any{"memories": []any{}})
			}
			rows, err := deps.Store.RecallMemories(ctx, a.Query, RecallOptions{Category: a.Category, Limit: limit})
			if err != nil {
				return tools.Fail(domain.CodeInternal, "memory.recall: "+err.Error())
			}
			return tools.Ok(fmt.Sprintf("Recalled %d memory(ies).", len(rows)), map[string]any{"memories": views(rows)})
		},
	}
}

// --- memory.list ---

type listArgs struct {
	Category   string `json:"category,omitempty"`
	PinnedOnly bool   `json:"pinnedOnly,omitempty"`
	Limit      *int   `json:"limit,omitempty"`
}

// Validate caps the category filter.
func (a *listArgs) Validate() error {
	if overLimit(a.Category, maxCategoryRunes) {
		return fmt.Errorf("category must be at most %d characters", maxCategoryRunes)
	}
	return nil
}

var listSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "category": { "type": "string" },
    "pinnedOnly": { "type": "boolean" },
    "limit": { "type": "integer", "minimum": 1, "maximum": 200 }
  }
}`)

func newListTool(deps Deps) *tools.Tool {
	return &tools.Tool{
		Name:        "memory.list",
		Description: "List project memories (pinned first, then most recent).",
		Risk:        domain.RiskRead,
		Schema:      listSchema,
		Decode:      tools.StrictDecoder(func() any { return &listArgs{} }),
		Handle: func(ctx context.Context, args json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a listArgs
			if err := tools.DecodeStrict(args, &a); err != nil {
				return tools.Fail(codeInvalidArgs, "Invalid arguments for memory.list: "+err.Error())
			}
			limit := clampLimit(a.Limit, 50, 1, 200)
			if deps.Store == nil {
				return tools.Ok("No memories (storage unavailable).", map[string]any{"memories": []any{}})
			}
			rows, err := deps.Store.ListMemories(ctx, ListOptions{Category: a.Category, PinnedOnly: a.PinnedOnly, Limit: limit})
			if err != nil {
				return tools.Fail(domain.CodeInternal, "memory.list: "+err.Error())
			}
			return tools.Ok(fmt.Sprintf("%d memory(ies).", len(rows)), map[string]any{"memories": views(rows)})
		},
	}
}

// --- memory.save ---

type saveArgs struct {
	Content  string `json:"content"`
	Category string `json:"category,omitempty"`
	Source   string `json:"source,omitempty"` // user | assistant (compact excluded)
	Kind     string `json:"kind,omitempty"`   // semantic (default) | episodic
	TtlMs    *int64 `json:"ttlMs,omitempty"`  // optional relative TTL in ms → expiresAt
}

// maxTtlMs bounds the relative TTL to ~100 years. Beyond this a ttlMs is nonsense
// and would risk overflowing the absolute expiresAt (now + ttlMs); rejecting keeps
// the epoch arithmetic in the handler safe. KEEP the literal in saveSchema's
// ttlMs "maximum" in sync with this value (100*365*24*60*60*1000 = 3153600000000).
const maxTtlMs = int64(100*365*24*60*60) * 1000

// Validate caps the saved content + category so a single memory can't bloat the
// FTS index, and bounds ttlMs so the computed expiresAt can't overflow. (The
// required/min-length + source/kind-enum checks stay in the handler.)
func (a *saveArgs) Validate() error {
	if overLimit(a.Content, maxContentRunes) {
		return fmt.Errorf("content must be at most %d characters", maxContentRunes)
	}
	if overLimit(a.Category, maxCategoryRunes) {
		return fmt.Errorf("category must be at most %d characters", maxCategoryRunes)
	}
	if a.TtlMs != nil && (*a.TtlMs < 1 || *a.TtlMs > maxTtlMs) {
		return fmt.Errorf("ttlMs must be between 1 and %d", maxTtlMs)
	}
	return nil
}

var saveSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["content"],
  "properties": {
    "content": { "type": "string", "minLength": 1 },
    "category": { "type": "string" },
    "source": { "type": "string", "enum": ["user", "assistant"] },
    "kind": { "type": "string", "enum": ["semantic", "episodic"] },
    "ttlMs": { "type": "integer", "minimum": 1, "maximum": 3153600000000 }
  }
}`)

func newSaveTool(deps Deps) *tools.Tool {
	return &tools.Tool{
		Name:        "memory.save",
		Description: "Save a durable project memory (persists across sessions).",
		Risk:        domain.RiskLocal,
		Schema:      saveSchema,
		Decode:      tools.StrictDecoder(func() any { return &saveArgs{} }),
		Handle: func(ctx context.Context, args json.RawMessage, tctx *tools.ToolContext) tools.ToolResult {
			var a saveArgs
			if err := tools.DecodeStrict(args, &a); err != nil {
				return tools.Fail(codeInvalidArgs, "Invalid arguments for memory.save: "+err.Error())
			}
			if strings.TrimSpace(a.Content) == "" {
				return tools.Fail(codeInvalidArgs, "memory.save: content is required")
			}
			// source defaults to assistant; "compact" is reserved internal and rejected.
			source := domain.MemoryAssistant
			switch a.Source {
			case "", "assistant":
				source = domain.MemoryAssistant
			case "user":
				source = domain.MemoryUser
			default:
				return tools.Fail(codeInvalidArgs, "memory.save: source must be user|assistant")
			}
			// kind defaults to semantic; episodic rows get a sessionId stamped below.
			kind := domain.MemoryKindSemantic
			switch a.Kind {
			case "", "semantic":
				kind = domain.MemoryKindSemantic
			case "episodic":
				kind = domain.MemoryKindEpisodic
			default:
				return tools.Fail(codeInvalidArgs, "memory.save: kind must be semantic|episodic")
			}
			now := domain.NowMS()
			rec := domain.MemoryRecord{
				ID:        domain.NewID(domain.PrefixMemory),
				Content:   a.Content,
				Source:    source,
				Kind:      kind,
				CreatedAt: now,
				UpdatedAt: now,
			}
			if a.Category != "" {
				rec.Category = &a.Category
			}
			// Relative TTL → absolute deadline (Validate bounded ttlMs, so no overflow).
			if a.TtlMs != nil {
				exp := now + *a.TtlMs
				rec.ExpiresAt = &exp
			}
			// Provenance + episodic namespacing are stamped from the live turn context,
			// never caller-supplied: runId records which turn saved the fact; sessionId
			// scopes an episodic row to this session. tctx may be nil in bare tests.
			if tctx != nil {
				if tctx.RunID != "" {
					runID := tctx.RunID
					rec.RunID = &runID
				}
				if kind == domain.MemoryKindEpisodic && tctx.SessionID != "" {
					sessID := tctx.SessionID
					rec.SessionID = &sessID
				}
			}
			id := rec.ID
			if deps.Store != nil {
				newID, err := deps.Store.InsertMemory(ctx, rec)
				if err != nil {
					return tools.Fail(domain.CodeInternal, "memory.save: "+err.Error())
				}
				if newID != "" {
					id = newID
					rec.ID = newID
				}
			}
			return tools.Ok("Saved memory "+id+".", map[string]any{"id": id, "memory": memoryView(&rec)})
		},
	}
}

// --- memory.forget / pin / unpin (id-only) ---

type idArgs struct {
	ID string `json:"id"`
}

// Validate caps the id length (ids are short prefixed handles; anything longer is
// junk that would never match a row).
func (a *idArgs) Validate() error {
	if overLimit(a.ID, maxIDRunes) {
		return fmt.Errorf("id must be at most %d characters", maxIDRunes)
	}
	return nil
}

var idSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["id"],
  "properties": { "id": { "type": "string" } }
}`)

func newForgetTool(deps Deps) *tools.Tool {
	return &tools.Tool{
		Name:        "memory.forget",
		Description: "Soft-delete a project memory by id.",
		Risk:        domain.RiskLocal,
		Schema:      idSchema,
		Decode:      tools.StrictDecoder(func() any { return &idArgs{} }),
		Handle: func(ctx context.Context, args json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			a, res, ok := decodeID(args, "memory.forget")
			if !ok {
				return res
			}
			if deps.Store == nil {
				return tools.Fail(codeMemoryNotFound, "memory.forget: no such memory: "+a.ID, tools.Unrecoverable())
			}
			found, err := deps.Store.ForgetMemory(ctx, a.ID)
			if err != nil {
				return tools.Fail(domain.CodeInternal, "memory.forget: "+err.Error())
			}
			if !found {
				return tools.Fail(codeMemoryNotFound, "memory.forget: no such memory: "+a.ID, tools.Unrecoverable())
			}
			// A forgotten memory might have been pinned — refresh the injected block.
			deps.notifyChange()
			return tools.Ok("Forgot memory "+a.ID+".", map[string]any{"id": a.ID})
		},
	}
}

func newPinTool(deps Deps) *tools.Tool {
	return &tools.Tool{
		Name:        "memory.pin",
		Description: "Pin a project memory so it sorts first and resists pruning (idempotent).",
		Risk:        domain.RiskLocal,
		Schema:      idSchema,
		Decode:      tools.StrictDecoder(func() any { return &idArgs{} }),
		Handle: func(ctx context.Context, args json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			a, res, ok := decodeID(args, "memory.pin")
			if !ok {
				return res
			}
			if deps.Store == nil {
				return tools.Fail(codeMemoryNotFound, "memory.pin: no such memory: "+a.ID, tools.Unrecoverable())
			}
			rec, err := deps.Store.PinMemory(ctx, a.ID)
			if err != nil {
				return tools.Fail(domain.CodeInternal, "memory.pin: "+err.Error())
			}
			if rec == nil {
				return tools.Fail(codeMemoryNotFound, "memory.pin: no such memory: "+a.ID, tools.Unrecoverable())
			}
			deps.notifyChange()
			return tools.Ok("Pinned memory "+a.ID+".", map[string]any{"id": a.ID, "memory": memoryView(rec)})
		},
	}
}

func newUnpinTool(deps Deps) *tools.Tool {
	return &tools.Tool{
		Name:        "memory.unpin",
		Description: "Unpin a project memory (idempotent).",
		Risk:        domain.RiskLocal,
		Schema:      idSchema,
		Decode:      tools.StrictDecoder(func() any { return &idArgs{} }),
		Handle: func(ctx context.Context, args json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			a, res, ok := decodeID(args, "memory.unpin")
			if !ok {
				return res
			}
			if deps.Store == nil {
				return tools.Fail(codeMemoryNotFound, "memory.unpin: no such memory: "+a.ID, tools.Unrecoverable())
			}
			rec, err := deps.Store.UnpinMemory(ctx, a.ID)
			if err != nil {
				return tools.Fail(domain.CodeInternal, "memory.unpin: "+err.Error())
			}
			if rec == nil {
				return tools.Fail(codeMemoryNotFound, "memory.unpin: no such memory: "+a.ID, tools.Unrecoverable())
			}
			deps.notifyChange()
			return tools.Ok("Unpinned memory "+a.ID+".", map[string]any{"id": a.ID, "memory": memoryView(rec)})
		},
	}
}

func decodeID(args json.RawMessage, name string) (idArgs, tools.ToolResult, bool) {
	var a idArgs
	if err := tools.DecodeStrict(args, &a); err != nil {
		return a, tools.Fail(codeInvalidArgs, "Invalid arguments for "+name+": "+err.Error()), false
	}
	if a.ID == "" {
		return a, tools.Fail(codeInvalidArgs, name+": id is required"), false
	}
	return a, tools.ToolResult{}, true
}

// clampLimit applies the default then clamps into [min,max].
func clampLimit(p *int, def, min, max int) int {
	v := def
	if p != nil {
		v = *p
	}
	if v < min {
		v = min
	}
	if v > max {
		v = max
	}
	return v
}
