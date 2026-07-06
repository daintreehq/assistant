package tools

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/daintreehq/daintree-assistant/internal/safety"
)

// openAINameRe is the legal OpenAI/DeepSeek function-name pattern. The model
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
	// observer, when set, receives every completed dispatch (observer.go). A
	// pure after-the-fact side-channel — never consulted before/while a tool runs.
	observer DispatchObserver
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		tools:          make(map[string]*Tool),
		wireToInternal: make(map[string]string),
		internalToWire: make(map[string]string),
	}
}

// noArgsParams is the canonical compact params object emitted for a tool with an
// empty/nil Schema — a permissive empty object so projection never fails on a
// no-arg tool. It is the byte-for-byte output of json.Marshal'ing
// map{"type":"object","properties":{}} (keys sort: "properties" < "type"),
// matching the value the projection previously built inline per call.
var noArgsParams = json.RawMessage(`{"properties":{},"type":"object"}`)

// Register inserts a tool. Returns an error (fail-fast) on a duplicate internal
// name or a malformed JSON Schema — registration happens once at startup, so
// either is a wiring bug.
func (r *Registry) Register(t *Tool) error {
	if _, exists := r.tools[t.Name]; exists {
		return fmt.Errorf("Duplicate tool registration: %s", t.Name)
	}
	// Canonicalize the schema ONCE here (the cold path) into the exact compact bytes
	// the projection emits, so OpenAITools never re-unmarshals it on the hot path
	// (rebuilt once per model round, of which a turn may run many). Unmarshal→marshal yields the same
	// sorted-key compact form the per-call json.Marshal produced before, keeping the
	// wire bytes (and the prompt cache) byte-stable. A bad schema fails fast here.
	if len(t.Schema) > 0 {
		var v any
		if err := json.Unmarshal(t.Schema, &v); err != nil {
			return fmt.Errorf("tool '%s' has invalid JSON Schema: %w", t.Name, err)
		}
		canon, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("tool '%s' schema re-encode failed: %w", t.Name, err)
		}
		t.projectionParams = canon
	} else {
		t.projectionParams = noArgsParams
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

// AssertRegistered checks that every name in names is registered, returning an
// error listing the missing ones. It is the drift guard for hand-maintained name
// lists (e.g. agent.coreToolNames) that must stay in lockstep with the registry:
// a rename or removal that drops a name from the registry would otherwise boot
// clean and then silently starve the model of a tool it was promised. label names
// the offending list for the error message. The check preserves the input order
// so the diagnostic is deterministic.
func (r *Registry) AssertRegistered(label string, names []string) error {
	var missing []string
	for _, name := range names {
		if !r.Has(name) {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%s not registered in the tool registry: %s", label, strings.Join(missing, ", "))
	}
	return nil
}

// ChatTool is the OpenAI function-spec projection. Parameters is the canonical
// raw JSON Schema object (pre-decoded at registration).
type ChatTool struct {
	Type     string           `json:"type"` // always "function"
	Function ChatToolFunction `json:"function"`
}

// ChatToolFunction is the inner function spec of a ChatTool. Parameters carries
// the canonical schema bytes verbatim (json.RawMessage) so the adapter can forward
// them to the model spec without a re-marshal round-trip.
type ChatToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
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

		// Emit the canonical params pre-decoded at Register — no per-call unmarshal.
		out = append(out, ChatTool{
			Type: "function",
			Function: ChatToolFunction{
				Name:        wire,
				Description: t.Description,
				Parameters:  t.projectionParams,
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

// closestToolName returns the registered internal tool name within a small edit
// distance of name — a "did you mean?" hint when the model emits a hallucinated or
// misremembered tool name. Returns "" when nothing is close enough. The wire form
// (fs__read) is normalized to the internal form (fs.read) first so a wire-name miss
// still finds its neighbor. Dotted tool names are longer than slash commands, so
// the threshold is a touch looser (≤3 edits).
func (r *Registry) closestToolName(name string) string {
	name = strings.ReplaceAll(strings.TrimSpace(name), "__", ".")
	if name == "" {
		return ""
	}
	best, bestD := "", 4 // strictly < 4, i.e. at most 3 edits
	for _, n := range r.order {
		if d := toolLevenshtein(name, n); d < bestD {
			best, bestD = n, d
		}
	}
	return best
}

// toolLevenshtein is the classic rune-aware edit distance (insert/delete/substitute).
func toolLevenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur := make([]int, len(rb)+1)
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(rb)]
}
