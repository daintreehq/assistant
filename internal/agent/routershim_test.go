package agent

import (
	"context"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/models"
)

// Router is a TEST-ONLY shim. It used to be a production seam on SessionDeps,
// satisfied by the legacy *models.Router (a direct DeepSeek client) — but the loop
// stopped calling it when the backend became the CLI's only model gateway, and the
// whole transport was deleted along with the field.
//
// The agent suite's fakes are still written against this shape and reach the loop
// through backendFromRouter, which adapts them to the real AssistantBackend seam.
// Keeping the interface here (rather than resurrecting it in deps.go) means the
// production build carries no model-access seam at all: there is no field a future
// handler could reach through to bypass the backend that owns prompt assembly,
// skill selection, and the provider credentials.
//
// FlushMeter is deliberately absent — it drained the deleted Router's per-tier
// usage meter, and UsageEvent.Tiers (its only consumer) was never populated.
type Router interface {
	Stream(ctx context.Context, tier domain.ModelTier, opts models.ChatOptions, onToken func(string)) (models.ChatResult, error)
	Chat(ctx context.Context, tier domain.ModelTier, opts models.ChatOptions) (models.ChatResult, error)
	ModelFor(tier domain.ModelTier) string
}
