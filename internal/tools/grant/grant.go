// Package grant holds the automation-grant tools: grant.create, grant.list,
// grant.revoke. A grant is the non-interactive escape valve — it pre-authorizes
// a watcher/timer actor to run otherwise-confirm-required tools unattended,
// bounded by TTL, use-count, and a tool/risk allowlist (union authorization).
package grant

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/safety"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

// maxGrantTTLMs caps a grant's lifetime at 30 days; maxGrantUses caps uses at
// 1000 — a runaway grant should self-expire.
const (
	maxGrantTTLMs = int64(30 * 24 * 60 * 60 * 1000)
	maxGrantUses  = 1000
)

const (
	codeInvalidArgs     = "INVALID_ARGS"
	codeActorForbidden  = "GRANT_ACTOR_FORBIDDEN"
	codeEmptyScope      = "GRANT_EMPTY_SCOPE"
	codeUngrantableTool = "GRANT_UNGRANTABLE_TOOL"
	codeUserDeclined    = "USER_DECLINED"
	codeGrantNotFound   = "GRANT_NOT_FOUND"
)

// mutatingGrantRisks are the risk classes whose presence in a grant's scope makes
// the grant "mutating" (and so requires an interactive confirm to mint).
var mutatingGrantRisks = map[domain.RiskClass]bool{
	domain.RiskTerminal: true,
	domain.RiskProject:  true,
	domain.RiskGit:      true,
	domain.RiskExternal: true,
	domain.RiskSystem:   true,
}

// Store is the slice of storage the grant tools touch.
type Store interface {
	InsertGrant(ctx context.Context, rec domain.AutomationGrantRecord) (string, error)
	ListGrants(ctx context.Context, actorID string) ([]domain.AutomationGrantRecord, error)
	RevokeGrant(ctx context.Context, id string) (bool, error)
}

// Deps is the dependency set for the grant family.
type Deps struct {
	Store Store
}

// Tools returns the grant tool family.
func Tools(deps Deps) []*tools.Tool {
	return []*tools.Tool{
		newCreateTool(deps),
		newListTool(deps),
		newRevokeTool(deps),
	}
}

// --- grant.create ---

type createArgs struct {
	ActorID            string   `json:"actorId"`
	ActorType          string   `json:"actorType"` // watcher | timer | wake
	AllowedRiskClasses []string `json:"allowedRiskClasses,omitempty"`
	AllowedToolNames   []string `json:"allowedToolNames,omitempty"`
	TTLMs              int64    `json:"ttlMs"`
	MaxUses            int      `json:"maxUses"`
}

var createSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["actorId", "actorType", "ttlMs", "maxUses"],
  "properties": {
    "actorId": { "type": "string", "minLength": 1 },
    "actorType": { "type": "string", "enum": ["watcher", "timer", "wake"] },
    "allowedRiskClasses": { "type": "array", "items": { "type": "string", "enum": ["read","local","ui","terminal","project","git","external","system"] } },
    "allowedToolNames": { "type": "array", "items": { "type": "string", "minLength": 1 } },
    "ttlMs": { "type": "integer", "minimum": 1 },
    "maxUses": { "type": "integer", "minimum": 1 }
  }
}`)

func newCreateTool(deps Deps) *tools.Tool {
	return &tools.Tool{
		Name:        "grant.create",
		Description: "Pre-authorize unattended confirm-required tool use, bounded by TTL, use-count and a tool/risk allowlist. actorType watcher/timer authorizes that one watcher/timer (actorId = its wch_/tmr_ id); actorType \"wake\" with actorId \"wake\" authorizes the supervisor daemon's autonomous wake turns while the user is away (create it when the user asks for unattended work that must MUTATE — e.g. sending terminal commands overnight).",
		Risk:        domain.RiskLocal,
		Schema:      createSchema,
		Decode:      tools.StrictDecoder(func() any { return &createArgs{} }),
		Handle: func(ctx context.Context, args json.RawMessage, tctx *tools.ToolContext) tools.ToolResult {
			var a createArgs
			if err := tools.DecodeStrict(args, &a); err != nil {
				return tools.Fail(codeInvalidArgs, "Invalid arguments for grant.create: "+err.Error())
			}
			// 1. main actor only — a non-interactive actor can never mint authority.
			if tctx.Actor != domain.ActorMain {
				return tools.Fail(codeActorForbidden,
					"grant.create can only be run by the interactive main actor.", tools.Unrecoverable())
			}
			if strings.TrimSpace(a.ActorID) == "" {
				return tools.Fail(codeInvalidArgs, "grant.create: actorId is required")
			}
			if a.ActorType != "watcher" && a.ActorType != "timer" && a.ActorType != "wake" {
				return tools.Fail(codeInvalidArgs, "grant.create: actorType must be watcher|timer|wake")
			}
			// Wake grants authorize the supervisor daemon's unattended wake turns;
			// they are keyed to ONE well-known actor id so the user can list and
			// revoke the project's unattended authority as a single grant.
			if a.ActorType == "wake" && a.ActorID != domain.WakeActorID {
				return tools.Fail(codeInvalidArgs,
					`grant.create: a wake grant's actorId must be the literal "wake" (the shared unattended-turn authority)`)
			}
			if a.TTLMs <= 0 || a.TTLMs > maxGrantTTLMs {
				return tools.Fail(codeInvalidArgs, fmt.Sprintf("grant.create: ttlMs must be 1..%d", maxGrantTTLMs))
			}
			if a.MaxUses <= 0 || a.MaxUses > maxGrantUses {
				return tools.Fail(codeInvalidArgs, fmt.Sprintf("grant.create: maxUses must be 1..%d", maxGrantUses))
			}

			risks := a.AllowedRiskClasses
			toolNames := a.AllowedToolNames
			// 2. both lists empty → a grant with no scope authorizes nothing.
			if len(risks) == 0 && len(toolNames) == 0 {
				return tools.Fail(codeEmptyScope,
					"grant.create: provide at least one of allowedRiskClasses or allowedToolNames.", tools.Unrecoverable())
			}
			// 3. ungrantable tools.
			for _, tn := range toolNames {
				if domain.IsUngrantableTool(tn) {
					return tools.Fail(codeUngrantableTool,
						"grant.create: "+tn+" can never be granted.", tools.Unrecoverable())
				}
			}
			// Validate the risk-class values.
			for _, rc := range risks {
				if !domain.RiskClass(rc).IsValid() {
					return tools.Fail(codeInvalidArgs, "grant.create: invalid risk class "+rc)
				}
			}

			// 4. grantScopeMutates → confirm unless autoApprove. local risk never
			//    auto-confirms, so this is the ONLY guard.
			mutates := len(toolNames) > 0
			for _, rc := range risks {
				if mutatingGrantRisks[domain.RiskClass(rc)] {
					mutates = true
				}
			}
			if mutates && !tctx.Config.AutoApprove {
				approved := false
				if tctx.Confirm != nil {
					ok, err := tctx.Confirm(ctx, tools.ConfirmRequest{
						ToolName:    "grant.create",
						Risk:        domain.RiskSystem,
						Consequence: "Pre-authorizes an automation actor to run mutating tools unattended.",
						Summary: fmt.Sprintf("Pre-authorize %s %s to run %s unattended (%d use(s), TTL %dms)?",
							a.ActorType, a.ActorID, scopeDescription(risks, toolNames), a.MaxUses, a.TTLMs),
						Args:              args,
						NeedsTypedConfirm: safety.NeedsTypedConfirm(domain.RiskSystem),
					})
					approved = err == nil && ok
				}
				if !approved {
					return tools.Fail(codeUserDeclined, "User declined grant.create.")
				}
			}

			// 5. mint.
			now := domain.NowMS()
			rec := domain.AutomationGrantRecord{
				ID:            domain.NewID(domain.PrefixGrant),
				ActorID:       a.ActorID,
				ActorType:     domain.AutomationGrantActorType(a.ActorType),
				ExpiresAt:     now + a.TTLMs,
				MaxUses:       a.MaxUses,
				UsesRemaining: a.MaxUses,
				CreatedAt:     now,
				Source:        domain.GrantSourceLocal,
			}
			if len(risks) > 0 {
				rj, _ := json.Marshal(risks)
				s := string(rj)
				rec.AllowedRiskClassesJson = &s
			}
			if len(toolNames) > 0 {
				tj, _ := json.Marshal(toolNames)
				s := string(tj)
				rec.AllowedToolNamesJson = &s
			}
			id := rec.ID
			if deps.Store != nil {
				newID, err := deps.Store.InsertGrant(ctx, rec)
				if err != nil {
					return tools.Fail(domain.CodeInternal, "grant.create: "+err.Error())
				}
				if newID != "" {
					id = newID
				}
			}
			return tools.Ok(
				fmt.Sprintf("Granted %s %s %d use(s) (TTL %dms).", a.ActorType, a.ActorID, a.MaxUses, a.TTLMs),
				map[string]any{
					"id":        id,
					"actorId":   a.ActorID,
					"actorType": a.ActorType,
					"expiresAt": rec.ExpiresAt,
					"maxUses":   a.MaxUses,
				},
			)
		},
	}
}

func scopeDescription(risks, toolNames []string) string {
	var parts []string
	if len(toolNames) > 0 {
		parts = append(parts, "tools ["+strings.Join(toolNames, ", ")+"]")
	}
	if len(risks) > 0 {
		parts = append(parts, "risks ["+strings.Join(risks, ", ")+"]")
	}
	return strings.Join(parts, " and ")
}

// --- grant.list ---

type listArgs struct {
	ActorID string `json:"actorId,omitempty"`
}

var listSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": { "actorId": { "type": "string" } }
}`)

func newListTool(deps Deps) *tools.Tool {
	return &tools.Tool{
		Name:        "grant.list",
		Description: "List live automation grants (non-revoked, non-expired, with uses remaining).",
		Risk:        domain.RiskRead,
		Schema:      listSchema,
		Decode:      tools.StrictDecoder(func() any { return &listArgs{} }),
		Handle: func(ctx context.Context, args json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a listArgs
			if err := tools.DecodeStrict(args, &a); err != nil {
				return tools.Fail(codeInvalidArgs, "Invalid arguments for grant.list: "+err.Error())
			}
			if deps.Store == nil {
				return tools.Ok("No grants (storage unavailable).", map[string]any{"grants": []any{}})
			}
			rows, err := deps.Store.ListGrants(ctx, a.ActorID)
			if err != nil {
				return tools.Fail(domain.CodeInternal, "grant.list: "+err.Error())
			}
			now := domain.NowMS()
			out := make([]map[string]any, 0, len(rows))
			for i := range rows {
				g := &rows[i]
				// Live = non-revoked, non-expired, uses remaining.
				if g.RevokedAt != nil || g.ExpiresAt <= now || g.UsesRemaining <= 0 {
					continue
				}
				out = append(out, grantView(g))
			}
			return tools.Ok(fmt.Sprintf("%d live grant(s).", len(out)), map[string]any{"grants": out})
		},
	}
}

func grantView(g *domain.AutomationGrantRecord) map[string]any {
	var risks, toolNames []string
	if g.AllowedRiskClassesJson != nil {
		_ = json.Unmarshal([]byte(*g.AllowedRiskClassesJson), &risks)
	}
	if g.AllowedToolNamesJson != nil {
		_ = json.Unmarshal([]byte(*g.AllowedToolNamesJson), &toolNames)
	}
	if risks == nil {
		risks = []string{}
	}
	if toolNames == nil {
		toolNames = []string{}
	}
	return map[string]any{
		"id":                 g.ID,
		"actorId":            g.ActorID,
		"actorType":          g.ActorType,
		"allowedRiskClasses": risks,
		"allowedToolNames":   toolNames,
		"expiresAt":          time.UnixMilli(g.ExpiresAt).UTC().Format(time.RFC3339),
		"usesRemaining":      g.UsesRemaining,
		"maxUses":            g.MaxUses,
		"source":             g.Source,
	}
}

// --- grant.revoke ---

type revokeArgs struct {
	ID string `json:"id"`
}

var revokeSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["id"],
  "properties": { "id": { "type": "string" } }
}`)

func newRevokeTool(deps Deps) *tools.Tool {
	return &tools.Tool{
		Name:        "grant.revoke",
		Description: "Revoke an automation grant by id.",
		Risk:        domain.RiskLocal,
		Schema:      revokeSchema,
		Decode:      tools.StrictDecoder(func() any { return &revokeArgs{} }),
		Handle: func(ctx context.Context, args json.RawMessage, tctx *tools.ToolContext) tools.ToolResult {
			var a revokeArgs
			if err := tools.DecodeStrict(args, &a); err != nil {
				return tools.Fail(codeInvalidArgs, "Invalid arguments for grant.revoke: "+err.Error())
			}
			if tctx.Actor != domain.ActorMain {
				return tools.Fail(codeActorForbidden,
					"grant.revoke can only be run by the interactive main actor.", tools.Unrecoverable())
			}
			if a.ID == "" {
				return tools.Fail(codeInvalidArgs, "grant.revoke: id is required")
			}
			if deps.Store == nil {
				return tools.Fail(codeGrantNotFound, "grant.revoke: no such grant: "+a.ID, tools.Unrecoverable())
			}
			ok, err := deps.Store.RevokeGrant(ctx, a.ID)
			if err != nil {
				return tools.Fail(domain.CodeInternal, "grant.revoke: "+err.Error())
			}
			if !ok {
				return tools.Fail(codeGrantNotFound, "grant.revoke: no such grant: "+a.ID, tools.Unrecoverable())
			}
			return tools.Ok("Revoked grant "+a.ID+".", map[string]any{"id": a.ID})
		},
	}
}
