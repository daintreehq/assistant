// Model router: maps the small/medium/large abstraction onto concrete Fireworks
// model ids and the FireworksClient. The large model owns the main thread; the
// small model does cheap repeated work (watchers, summaries, classification). For
// v1 the medium tier routes to the large model id (but stays a distinct contract).
package models

import (
	"context"
	"fmt"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// RouterConfig is the narrow config view the router needs (model ids per tier).
// A consumer-defined struct so the package compiles without importing config; the
// app wires it from its AppConfig.
type RouterConfig struct {
	LargeModel  string
	MediumModel string
	SmallModel  string
}

// DebugLogger is the consumer-defined seam for the debug-log subsystem. The router
// traces model.request/response/error/cancelled through it. A nil logger disables
// tracing (the no-op path) — the model layer never depends on debuglog directly.
type DebugLogger interface {
	LogDebug(event string, fields map[string]any)
}

// Router maps tiers onto models and wraps FireworksClient with the image-tier gate
// and debug tracing.
type Router struct {
	FW  *FireworksClient
	cfg RouterConfig
	log DebugLogger
}

// NewRouter builds a router. fw may be nil, in which case the caller is expected to
// supply one via NewRouterWithClient; this constructor requires an explicit client
// because the model-id config and the client's connection config are separate seams.
func NewRouter(cfg RouterConfig, fw *FireworksClient, log DebugLogger) *Router {
	return &Router{FW: fw, cfg: cfg, log: log}
}

// ModelFor resolves a tier to its concrete model id. default falls through to large.
func (r *Router) ModelFor(tier domain.ModelTier) string {
	switch tier {
	case domain.ModelSmall:
		return r.cfg.SmallModel
	case domain.ModelMedium:
		return r.cfg.MediumModel
	case domain.ModelLarge:
		return r.cfg.LargeModel
	default:
		return r.cfg.LargeModel
	}
}

// Describe returns the resolved model ids per tier.
func (r *Router) Describe() map[string]string {
	return map[string]string{
		"large":  r.cfg.LargeModel,
		"medium": r.cfg.MediumModel,
		"small":  r.cfg.SmallModel,
	}
}

// assertImageTier rejects image content on any tier but large. Only the large model
// is vision-capable; small is text-only and medium routes through, so an image
// bound for either would 400 at the provider or be silently dropped. Gate on tier
// semantics (the stable contract), not the resolved model id, and fail before any
// wire call so the error is a clear local one.
func (r *Router) assertImageTier(tier domain.ModelTier, messages []ChatMessage) error {
	if tier != domain.ModelLarge && HasImageContent(messages) {
		return &ImageInputNotSupportedError{
			Message: fmt.Sprintf(
				"Image inputs require the large tier; got %q (only the large model is vision-capable).", tier),
		}
	}
	return nil
}

// Chat runs a non-streaming completion on the given tier. opts.Model is ignored and
// overwritten with the tier's resolved model.
func (r *Router) Chat(ctx context.Context, tier domain.ModelTier, opts ChatOptions) (ChatResult, error) {
	if err := r.assertImageTier(tier, opts.Messages); err != nil {
		return ChatResult{}, err
	}
	model := r.ModelFor(tier)
	opts.Model = model
	r.logRequest("chat", tier, model, opts)
	res, err := r.FW.Chat(ctx, opts)
	if err != nil {
		return ChatResult{}, r.classifyErr("chat", tier, model, err)
	}
	r.logResponse("chat", tier, model, res)
	return res, nil
}

// Stream runs a streaming completion on the given tier with a token callback.
func (r *Router) Stream(ctx context.Context, tier domain.ModelTier, opts ChatOptions, onToken func(string)) (ChatResult, error) {
	if err := r.assertImageTier(tier, opts.Messages); err != nil {
		return ChatResult{}, err
	}
	model := r.ModelFor(tier)
	opts.Model = model
	r.logRequest("stream", tier, model, opts)
	res, err := r.FW.ChatStream(ctx, opts, onToken)
	if err != nil {
		return ChatResult{}, r.classifyErr("stream", tier, model, err)
	}
	r.logResponse("stream", tier, model, res)
	return res, nil
}

// JSON runs a strict json_object completion on the given tier and returns the
// cleaned JSON string (think-stripped + balanced-span-extracted). The caller
// decodes/validates into a concrete struct (see DecodeWatcherVerdict etc.).
func (r *Router) JSON(ctx context.Context, tier domain.ModelTier, opts ChatOptions) (string, error) {
	if err := r.assertImageTier(tier, opts.Messages); err != nil {
		return "", err
	}
	model := r.ModelFor(tier)
	opts.Model = model
	r.logRequest("json", tier, model, opts)
	out, err := r.FW.JSON(ctx, opts)
	if err != nil {
		return "", r.classifyErr("json", tier, model, err)
	}
	r.logDebug("model.response", map[string]any{"kind": "json", "tier": string(tier), "model": model, "result": out})
	return out, nil
}

// classifyErr traces a cancelled turn under model.cancelled (a clean user abort,
// not a failure) and everything else under model.error, then returns err unchanged.
func (r *Router) classifyErr(kind string, tier domain.ModelTier, model string, err error) error {
	if _, ok := err.(*CancelledError); ok {
		r.logDebug("model.cancelled", map[string]any{"kind": kind, "tier": string(tier), "model": model})
		return err
	}
	r.logDebug("model.error", map[string]any{
		"kind": kind, "tier": string(tier), "model": model, "error": err.Error(),
	})
	return err
}

func (r *Router) logRequest(kind string, tier domain.ModelTier, model string, opts ChatOptions) {
	if r.log == nil {
		return
	}
	toolNames := make([]string, 0, len(opts.Tools))
	for _, t := range opts.Tools {
		toolNames = append(toolNames, t.Function.Name)
	}
	r.logDebug("model.request", map[string]any{
		"kind": kind, "tier": string(tier), "model": model,
		"toolChoice": opts.ToolChoice,
		"toolNames":  toolNames,
		"messages":   redactMessages(opts.Messages),
	})
}

func (r *Router) logResponse(kind string, tier domain.ModelTier, model string, res ChatResult) {
	r.logDebug("model.response", map[string]any{
		"kind": kind, "tier": string(tier), "model": model,
		"finishReason": res.FinishReason,
		"usage":        res.Usage,
		"toolCalls":    res.ToolCalls,
		"content":      res.Content,
	})
}

func (r *Router) logDebug(event string, fields map[string]any) {
	if r.log != nil {
		r.log.LogDebug(event, fields)
	}
}
