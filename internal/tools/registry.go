package tools

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/daintreehq/daintree-assistant/internal/safety"
)

// openAINameRe is the legal OpenAI/Fireworks function-name pattern. The model
// only ever sees wire names; they must match this.
var openAINameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// toWireName maps an internal dotted name to its OpenAI wire form by replacing
// EVERY "." with "__" (fs.read → fs__read). Reversed via the alias map, NOT by
// string substitution (collisions are possible).
func toWireName(name string) string {
	return strings.ReplaceAll(name, ".", "__")
}

// Registry is the tool registry: internal dotted name → Tool, plus the wire-name
// alias maps rebuilt on each OpenAITools projection. Dispatch (dispatch.go) is the
// pipeline; this file holds registration + projection + wire-name resolution.
type Registry struct {
	tools          map[string]*Tool
	order          []string // registration order — the deterministic projection order
	wireToInternal map[string]string
	internalToWire map[string]string
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		tools:          make(map[string]*Tool),
		wireToInternal: make(map[string]string),
		internalToWire: make(map[string]string),
	}
}

// Register inserts a tool. Returns an error (fail-fast) on a duplicate internal
// name — registration happens once at startup, so a duplicate is a wiring bug.
func (r *Registry) Register(t *Tool) error {
	if _, exists := r.tools[t.Name]; exists {
		return fmt.Errorf("Duplicate tool registration: %s", t.Name)
	}
	r.tools[t.Name] = t
	// Track registration order so List/OpenAITools project deterministically across
	// runs — ranging over the map would reorder the tool specs (and the read-only
	// list) per process, churning the prompt cache (a stability invariant).
	r.order = append(r.order, t.Name)
	return nil
}

// RegisterAll registers a batch, stopping at the first duplicate.
func (r *Registry) RegisterAll(tools ...*Tool) error {
	for _, t := range tools {
		if err := r.Register(t); err != nil {
			return err
		}
	}
	return nil
}

// Get returns the tool by internal name, or nil.
func (r *Registry) Get(name string) *Tool { return r.tools[name] }

// Has reports whether an internal name is registered (ports.ToolRegistry).
func (r *Registry) Has(name string) bool { _, ok := r.tools[name]; return ok }

// List returns all registered tools in registration order (deterministic across
// runs — a downstream read-only/filter projection inherits this stable order).
func (r *Registry) List() []*Tool {
	out := make([]*Tool, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, r.tools[name])
	}
	return out
}

// AssertSafe enforces the no-file-edit invariant over all registered names at
// startup. Returns the *safety.FileEditAttemptError if any name is file-mutating.
func (r *Registry) AssertSafe() error {
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	return safety.AssertNoFileEditTools(names)
}

// ChatTool is the OpenAI function-spec projection. Parameters is the raw JSON
// Schema object.
type ChatTool struct {
	Type     string           `json:"type"` // always "function"
	Function ChatToolFunction `json:"function"`
}

// ChatToolFunction is the inner function spec of a ChatTool.
type ChatToolFunction struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters"`
}

// OpenAITools projects the registry to OpenAI function specs and REBUILDS both
// alias maps wholesale (so a narrowed projection never resolves a name from a
// previous wider one). filterNames matches INTERNAL names; nil ⇒ all tools.
// Fail-fast: returns an error on an illegal wire name or a wire-name collision —
// never silently drops.
func (r *Registry) OpenAITools(filterNames []string) ([]ChatTool, error) {
	// Determine the projection order DETERMINISTICALLY (never range over the map):
	//   - nil filter ⇒ registration order (r.order);
	//   - non-nil filter ⇒ filterNames order, de-duped, skipping unregistered names.
	// The filter slice already carries the caller's intended order (coreTools then
	// each skill's requiredTools), so honoring it keeps the per-turn toolset stable
	// across runs for the same loaded-skill set — a prompt-cache-stability invariant.
	var names []string
	if filterNames == nil {
		names = r.order
	} else {
		seen := make(map[string]bool, len(filterNames))
		names = make([]string, 0, len(filterNames))
		for _, n := range filterNames {
			if seen[n] || r.tools[n] == nil {
				continue
			}
			seen[n] = true
			names = append(names, n)
		}
	}

	wireToInternal := make(map[string]string)
	internalToWire := make(map[string]string)
	var out []ChatTool

	for _, name := range names {
		t := r.tools[name]
		wire := toWireName(name)
		if !openAINameRe.MatchString(wire) {
			return nil, fmt.Errorf(
				"Tool '%s' produces wire name '%s', which does not match %s",
				name, wire, openAINameRe.String(),
			)
		}
		if prev, clash := wireToInternal[wire]; clash {
			return nil, fmt.Errorf(
				"Wire-name collision: '%s' and '%s' both map to '%s'",
				prev, name, wire,
			)
		}
		wireToInternal[wire] = name
		internalToWire[name] = wire

		// Decode the raw schema into a generic value for emission; an empty/nil
		// schema becomes a permissive empty object so projection never fails on a
		// no-arg tool.
		var params any
		if len(t.Schema) > 0 {
			if err := json.Unmarshal(t.Schema, &params); err != nil {
				return nil, fmt.Errorf("tool '%s' has invalid JSON Schema: %w", name, err)
			}
		} else {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}

		out = append(out, ChatTool{
			Type: "function",
			Function: ChatToolFunction{
				Name:        wire,
				Description: t.Description,
				Parameters:  params,
			},
		})
	}

	// Replace the alias maps wholesale only after a fully successful projection.
	r.wireToInternal = wireToInternal
	r.internalToWire = internalToWire
	return out, nil
}

// ResolveWireName maps a wire name back to its internal name from the MOST RECENT
// OpenAITools projection. Unknown ⇒ "" (no such resolution).
func (r *Registry) ResolveWireName(wireName string) string {
	return r.wireToInternal[wireName]
}
