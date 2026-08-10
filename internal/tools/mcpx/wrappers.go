package mcpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

// The wrappers below cover Daintree actions previously reachable only through the
// raw daintree.call escape hatch. Each carries the risk class Daintree gates the
// action at, so reads/UI-focus run without the system-tier confirmation the escape
// hatch always forces. Most `arguments` records are opaque and forwarded verbatim
// (we deliberately do NOT model their keys); copyTree's `options` is the
// exception — see copyTreeOptions for why it had to be typed.

/* ------------------------------ terminal.focus ---------------------------- */

// mcpPanelFocus is the raw Daintree MCP action behind the terminal.focus
// wrapper: Daintree has no `terminal.focus` action, because terminals ARE
// panels. Named rather than inlined so the rename is greppable from either side
// (the wrapper takes terminalId; the raw action takes panelId).
const mcpPanelFocus = "panel.focus"

type focusArgs struct {
	TerminalID string `json:"terminalId"`
}

var focusSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "terminalId": { "type": "string", "description": "Daintree terminal id to focus in the UI." }
  },
  "required": ["terminalId"]
}`)

func newTerminalFocusTool(deps Deps) tools.Tool {
	return tools.Tool{
		Name:        "terminal.focus",
		Description: "Bring ONE Daintree terminal to the front in the UI (forwards to Daintree's panel.focus with the terminal id as the panelId). Pure UI: no confirmation, no state change — it does not read, send to, or close anything. Use it when the user asks to see or switch to a terminal, or to point them at a tab you just spawned. Pass the full terminal-<uuid> id.",
		Risk:        domain.RiskUI,
		Schema:      focusSchema,
		Decode:      tools.StrictDecoder(func() any { return &focusArgs{} }),
		Handle: func(ctx context.Context, raw json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a focusArgs
			_ = json.Unmarshal(raw, &a)
			// Daintree has no `terminal.focus` MCP tool — terminals are panels, so the
			// correct call is `panel.focus` with the terminal id as the panelId.
			return passthrough(ctx, deps.MCP, mcpPanelFocus, map[string]any{"panelId": a.TerminalID}, "")
		},
	}
}

/* ------------------------------ terminal.rename --------------------------- */

type renameArgs struct {
	TerminalID string `json:"terminalId"`
	Name       string `json:"name"`
}

var renameSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "terminalId": { "type": "string", "description": "Daintree terminal id to rename (the id field from terminal.list)." },
    "name": { "type": "string", "description": "New tab title. Required: a non-empty name renames programmatically. Keep it short and descriptive (e.g. \"merge pipeline: PRs #10779-84\")." }
  },
  "required": ["terminalId", "name"]
}`)

func newTerminalRenameTool(deps Deps) tools.Tool {
	return tools.Tool{
		Name: "terminal.rename",
		Description: "Set a terminal/agent tab's title. This is how you ORGANISE the workspace: when several agents are all " +
			"named \"Claude\" (or you've lost track of a fleet), rename each to what it is actually doing so the tabs read at " +
			"a glance. Pass terminalId + a short name; rename many in ONE batched round (one call per id, all in the same turn). " +
			"UI-only, no confirmation. Typed wrapper around the Daintree terminal.rename MCP tool — don't reach for it via " +
			"tool.search or daintree.call.",
		Risk:   domain.RiskUI,
		Schema: renameSchema,
		Decode: tools.StrictDecoder(func() any { return &renameArgs{} }),
		Handle: func(ctx context.Context, raw json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a renameArgs
			_ = json.Unmarshal(raw, &a)
			// Daintree's raw terminal.rename treats BOTH args as optional: an omitted
			// terminalId targets the *focused* terminal, and an omitted name opens the
			// interactive rename dialog instead of renaming
			// (terminalLifecycleActions.ts). Both are wrong for an orchestrator — it
			// must rename a SPECIFIC terminal programmatically, never pop a dialog or
			// silently retarget the focused tab — so the wrapper requires both and
			// rejects blanks with a recoverable validation error.
			if strings.TrimSpace(a.TerminalID) == "" || strings.TrimSpace(a.Name) == "" {
				return tools.Fail(domain.CodeValidation, "terminal.rename: terminalId and a non-empty name are both required.")
			}
			return passthrough(ctx, deps.MCP, "terminal.rename",
				map[string]any{"terminalId": a.TerminalID, "name": a.Name}, "")
		},
	}
}

/* ------------------------- copyTree.* shared shapes ----------------------- */

// copyTreeOptions mirrors Daintree's CopyTreeOptionsSchema field for field. The
// authoritative source is
// daintree/src/services/actions/definitions/schemas.ts (CopyTreeOptionsSchema);
// the semantics behind each field live in daintree/shared/types/ipc/copyTree.ts.
//
// This is deliberately NOT an opaque map any more. The schema below sets
// "additionalProperties": false, which makes an untyped field an UNREACHABLE
// field — so the curation surface the model actually needs (includePaths,
// filter, exclude, scopePaths) has to be spelled out here to exist at all. The
// previous shape advertised an opaque bag and told the model not to invent
// keys, which meant there was no path from "these 40 files matter" to a bundle.
// Keep this struct, the schema fragment, and the host schema in lockstep;
// TestCopyTreeOptionsSchemaMatchesStruct is the local half of that guard.
//
// Pointers are used ONLY where the JSON zero value is both legal to send and
// dangerous to drop. `omitempty` erases an explicit 0, and a dropped
// maxFileSize/maxTotalSize/maxFileCount/charLimit reads to the CopyTree SDK as
// "no budget at all" — turning a malformed narrow request into a whole-worktree
// bundle. The two booleans need no such care: Daintree defaults both to false,
// so an explicit false and an omitted field genuinely mean the same thing.
type copyTreeOptions struct {
	Format string `json:"format,omitempty"`

	// Daintree types filter/exclude as `string | string[]`. We accept ONLY the
	// array arm: a lone pattern is a one-element array, which is a perfectly
	// valid instance of the host union, so nothing is lost. Modelling the union
	// itself would need a JSON-Schema combinator (oneOf, or a type array) — and
	// tool schemas are forwarded verbatim to the upstream model, where those are
	// honoured inconsistently at best. Same reasoning as domain.WatchCondition's
	// one-key discriminated union: no $ref, no oneOf.
	Filter []string `json:"filter,omitempty"`

	// Pointers, unlike the three selection lists, because an EMPTY exclude/always
	// list is legal host-side and means something specific: Daintree back-fills
	// the project's configured excludedPaths/alwaysExclude/alwaysInclude only
	// when the field is `undefined` (electron/ipc/handlers/copyTree.ts,
	// mergeCopyTreeOptions), so `[]` is how a caller says "use NO exclusions,
	// ignore my project defaults". A plain slice with omitempty would erase that
	// during StrictDecoder's canonical re-marshal and hand Daintree `undefined`
	// instead — which back-fills the project's alwaysInclude, and since `always`
	// is a force-include that overrides the normal filter, a project pattern like
	// "**/*" would quietly defeat a curated includePaths. A non-nil pointer to an
	// empty slice survives omitempty and marshals as `[]`.
	Exclude *[]string `json:"exclude,omitempty"`
	Always  *[]string `json:"always,omitempty"`

	IncludePaths []string `json:"includePaths,omitempty"`
	ScopePaths   []string `json:"scopePaths,omitempty"`

	Modified bool   `json:"modified,omitempty"`
	Changed  string `json:"changed,omitempty"`

	MaxFileSize  *int `json:"maxFileSize,omitempty"`
	MaxTotalSize *int `json:"maxTotalSize,omitempty"`
	MaxFileCount *int `json:"maxFileCount,omitempty"`

	WithLineNumbers bool `json:"withLineNumbers,omitempty"`
	CharLimit       *int `json:"charLimit,omitempty"`

	Sort string `json:"sort,omitempty"`
}

// The two enums Daintree advertises. Mirrored here because a schema `enum` is
// only advisory to the model — nothing rejects an off-menu value until the host
// does, and by then the user has already confirmed the call.
var (
	copyTreeFormats = []string{"xml", "json", "markdown", "tree", "ndjson", "sarif"}
	copyTreeSorts   = []string{"path", "size", "modified", "name", "extension", "depth"}
)

// validate enforces the bounds Daintree's Zod schema declares but strict JSON
// decoding alone can't express.
//
// The three selection lists are the load-bearing ones. Daintree rejects an empty
// filter/includePaths/scopePaths (and a blank entry inside one) rather than
// normalizing it away, because an empty selection reads as "no filter" to the
// CopyTree SDK — i.e. the whole worktree. We have to catch that HERE rather than
// leaning on the host to do it: Validate runs before StrictDecoder's canonical
// re-marshal, and `omitempty` erases a present-but-empty slice, so left alone
// `{"includePaths":[]}` would reach Daintree as "no selection was given" and
// quietly bundle everything — a narrow request silently widened into a full-repo
// copy, which for generateAndCopyFile lands on the user's clipboard.
func (o *copyTreeOptions) validate() error {
	if o == nil {
		return nil
	}
	// Non-empty at both levels, matching the host's .min(1) on the list and on
	// each item. exclude/always are deliberately absent from this list: neither
	// carries those refinements host-side, and an empty list there is a
	// MEANINGFUL instruction (clear the project's configured defaults) rather
	// than a malformed selection — see the pointer fields above.
	for _, sel := range []struct {
		name string
		vals []string
	}{
		{"filter", o.Filter},
		{"includePaths", o.IncludePaths},
		{"scopePaths", o.ScopePaths},
	} {
		if sel.vals == nil {
			continue
		}
		if len(sel.vals) == 0 {
			return fmt.Errorf("options.%s was supplied as an empty list; omit it to select the whole worktree, or name at least one path/pattern", sel.name)
		}
		for i, v := range sel.vals {
			if strings.TrimSpace(v) == "" {
				return fmt.Errorf("options.%s[%d] is blank; every entry must be a non-empty path or pattern", sel.name, i)
			}
		}
	}
	for _, budget := range []struct {
		name string
		val  *int
	}{
		{"maxFileSize", o.MaxFileSize},
		{"maxTotalSize", o.MaxTotalSize},
		{"maxFileCount", o.MaxFileCount},
		{"charLimit", o.CharLimit},
	} {
		if budget.val != nil && *budget.val < 1 {
			return fmt.Errorf("options.%s must be a positive integer (got %d); omit it for no budget", budget.name, *budget.val)
		}
	}
	if o.Format != "" && !slices.Contains(copyTreeFormats, o.Format) {
		return fmt.Errorf("options.format must be one of: %s", strings.Join(copyTreeFormats, ", "))
	}
	if o.Sort != "" && !slices.Contains(copyTreeSorts, o.Sort) {
		return fmt.Errorf("options.sort must be one of: %s", strings.Join(copyTreeSorts, ", "))
	}
	return nil
}

// copyTreeStrictDecoder is tools.StrictDecoder plus a raw-JSON pre-scan that
// rejects explicit `null` and blank/whitespace-only strings ANYWHERE in a
// copyTree argument object — with a single carve-out for the top-level
// free-text `name` label, below.
//
// It exists because Go's decoder quietly collapses both into "absent", and
// StrictDecoder's canonical re-marshal then erases the evidence with omitempty —
// the same fail-open the empty-list guard closes, reached through two other
// doors:
//
//	{"options":{"includePaths":null}}  → nil slice  → no selection → whole worktree
//	{"worktreeId":""}                  → ""         → no target    → whatever worktree is active
//	{"options":{"changed":"  "}}       → dropped    → no git filter → whole worktree
//	{"options":{"sort":""}}            → dropped    → back-fills the project's sort strategy
//
// Rejecting both is fidelity, not an invented rule: Daintree's fields are
// `.optional()` (undefined-able, never nullable) and its worktree selectors are
// `.min(1)` precisely so an empty selector can't silently become an
// active-worktree fallback (locationArgs.ts). Almost every string in this
// argument family is a path, pattern, git ref, id or enum member, where blank is
// never meaningful. The ONE exception is the top-level `name` — a free-text
// cosmetic label where the host itself treats blank as "absent, derive a label
// from the selection" — so the blank rule carves that single key out (the
// forward paths then drop a blank name rather than sending "" as a label).
// Null stays rejected even for name: optional means undefined-able, not
// nullable, there as everywhere else.
func copyTreeStrictDecoder(newArgs func() any) tools.DecodeFunc {
	inner := tools.StrictDecoder(newArgs)
	return func(raw json.RawMessage) (json.RawMessage, error) {
		if err := scanCopyTreeArgs(raw); err != nil {
			return nil, &tools.DecodeError{Message: err.Error(), Issues: []string{err.Error()}}
		}
		return inner(raw)
	}
}

// scanCopyTreeArgs walks the RAW TOKEN STREAM rather than a decoded map, and that
// distinction is load-bearing: a map has already discarded a repeated key
// (last-wins collapses it), while encoding/json MERGES repeated object members
// into the same destination struct. So
//
//	{"options":{"includePaths":null},"options":{}}
//
// decodes to a map holding no null at all — a map-based scan sees nothing wrong —
// yet leaves IncludePaths nil in the struct, which is exactly the "no selection"
// state that makes Daintree bundle the whole worktree. Rejecting duplicate keys
// outright also closes a real strictness gap, since DisallowUnknownFields does
// not consider a repeated key unknown.
//
// A malformed body is left to the strict decoder, which reports it far better
// than this scan could — every error path here simply stops scanning.
func scanCopyTreeArgs(raw json.RawMessage) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	return scanCopyTreeValue(dec, "")
}

// scanCopyTreeValue consumes exactly one JSON value, naming the offending field
// so the model can fix the one key it got wrong instead of re-sending blind.
func scanCopyTreeValue(dec *json.Decoder, path string) error {
	tok, err := dec.Token()
	if err != nil {
		return nil
	}
	where := path
	if where == "" {
		where = "the arguments object"
	}
	switch t := tok.(type) {
	case nil:
		return fmt.Errorf("%s is null; omit the field entirely instead of sending null, which Daintree rejects and which would silently read here as 'no value given'", where)
	case string:
		if strings.TrimSpace(t) == "" {
			// Top-level `name` is the one free-text field in the family: the host
			// reads a blank one as "absent" and derives a label, so blank is not an
			// error there. The check is exact — a `name` nested anywhere else (e.g.
			// options.name, or inside an array) gets no such carve-out.
			if path == "name" {
				return nil
			}
			return fmt.Errorf("%s is blank; every path, pattern, git ref, id and enum value here must be non-empty (omit the field if you have no value for it)", where)
		}
	case json.Delim:
		switch t {
		case '{':
			seen := map[string]bool{}
			for dec.More() {
				keyTok, err := dec.Token()
				if err != nil {
					return nil
				}
				key, ok := keyTok.(string)
				if !ok {
					return nil
				}
				if seen[key] {
					return fmt.Errorf("%s names %q more than once; send each field exactly once (a repeated key silently overwrites the earlier one)", where, key)
				}
				seen[key] = true
				child := key
				if path != "" {
					child = path + "." + key
				} else if key == "" {
					// An empty top-level key must not leave the child path empty:
					// its children would then look top-level themselves, and a
					// nested "name" would wrongly inherit the blank carve-out.
					// The quoted spelling keeps the path honest (the strict
					// decoder rejects the unknown "" field regardless).
					child = `""`
				}
				if err := scanCopyTreeValue(dec, child); err != nil {
					return err
				}
			}
			if _, err := dec.Token(); err != nil { // consume '}'
				return nil
			}
		case '[':
			for i := 0; dec.More(); i++ {
				if err := scanCopyTreeValue(dec, fmt.Sprintf("%s[%d]", where, i)); err != nil {
					return err
				}
			}
			if _, err := dec.Token(); err != nil { // consume ']'
				return nil
			}
		}
	}
	return nil
}

// wire renders the typed options as the plain JSON object Daintree expects.
// Going through encoding/json keeps ONE source of truth for the wire key names —
// the struct tags — because hand-building a second 14-key map is exactly how the
// two spellings would drift apart. Returns nil when there is nothing to send, so
// an absent (or entirely default) options object stays absent on the wire rather
// than arriving as a meaningless empty object.
func (o *copyTreeOptions) wire() map[string]any {
	if o == nil {
		return nil
	}
	raw, err := json.Marshal(o)
	if err != nil {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil || len(m) == 0 {
		return nil
	}
	return m
}

// copyTreeOptionsSchemaJSON is the nested "options" schema, shared verbatim by
// all three copyTree wrappers so the three local copies can't drift from each
// other. Expanded inline rather than referenced: tool schemas are forwarded to
// the model as-is, and $ref would have to be resolved by something that isn't
// there.
const copyTreeOptionsSchemaJSON = `{
    "type": "object",
    "additionalProperties": false,
    "description": "Curation and budget settings for the bundle. Once you have worked out WHICH files matter, name them here in ONE call — e.g. {\"includePaths\":[\"internal/a.go\",\"internal/a_test.go\",\"internal/b.go\"]} — rather than bundling the whole worktree and hoping. Omit this object to apply no explicit selection; the project's own exclusions, ignore rules and size budgets still apply.",
    "properties": {
      "includePaths": { "type": "array", "items": { "type": "string", "minLength": 1 }, "minItems": 1, "description": "THE curation field: worktree-relative exact paths (or glob patterns) to include. Use it to assemble a bundle of scattered but related files — the sources, the code they lean on, and their tests — in a single call. Unlike scopePaths it does not restrict traversal, so a pattern may match anywhere in the worktree. Combined with filter when both are given." },
      "filter": { "type": "array", "items": { "type": "string", "minLength": 1 }, "minItems": 1, "description": "Worktree-relative glob patterns or exact paths to include. Combined with includePaths when both are given; omit both to include the whole worktree. Always an ARRAY here — pass a single pattern as a one-element array." },
      "exclude": { "type": "array", "items": { "type": "string", "minLength": 1 }, "description": "Worktree-relative glob patterns to drop from the selection. Always an ARRAY here — pass a single pattern as a one-element array. Omit it to inherit the project's configured exclusions; pass an empty array to clear those for this call." },
      "always": { "type": "array", "items": { "type": "string", "minLength": 1 }, "description": "Force-include glob patterns: they survive the exclusions and the per-file maxFileSize gate. Omit to inherit the project's configured always-include patterns; pass an empty array to clear those for this call, which is what you want when the bundle should stay close to just what you listed. Note a worktree's own .copytree.yml may still force files in." },
      "scopePaths": { "type": "array", "items": { "type": "string", "minLength": 1 }, "minItems": 1, "description": "LITERAL worktree-relative file or directory paths that restrict traversal to those subtrees — these are NOT glob patterns; nothing is globbed or escaped. Composes with filter/includePaths rather than replacing them. Omit to traverse the whole worktree; an empty list is rejected rather than read as 'no scoping', which would silently copy everything." },
      "format": { "type": "string", "enum": ["xml", "json", "markdown", "tree", "ndjson", "sarif"], "description": "Output format for the bundle. Omit for Daintree's default." },
      "modified": { "type": "boolean", "description": "Include only tracked files modified in the working directory (staged and unstaged changes; untracked files are excluded)." },
      "changed": { "type": "string", "minLength": 1, "description": "Include only files changed since this commit or branch. Give a ref that definitely exists: Daintree falls back to NO git filter when the diff can't be run, so a bad ref quietly widens the bundle instead of failing." },
      "maxFileSize": { "type": "integer", "minimum": 1, "description": "Per-file size gate in bytes; larger files are never opened. Patterns listed in 'always' override it. CopyTree's own 10MB memory-safety ceiling applies regardless and cannot be lifted." },
      "maxTotalSize": { "type": "integer", "minimum": 1, "description": "Total size budget in bytes across every retained file." },
      "maxFileCount": { "type": "integer", "minimum": 1, "description": "Maximum number of files retained after selection and sorting." },
      "charLimit": { "type": "integer", "minimum": 1, "description": "Character budget across ALL included file content, not per file." },
      "withLineNumbers": { "type": "boolean", "description": "Prefix included file content with line numbers." },
      "sort": { "type": "string", "enum": ["path", "size", "modified", "name", "extension", "depth"], "description": "Decides WHICH files survive when maxFileCount/maxTotalSize/charLimit force a trim: the selection is sorted this way and the head is kept." }
    },
    "required": []
  }`

// copyTreeNameSchemaJSON is the top-level "name" property, shared verbatim by
// all three copyTree wrappers (same drift rationale as the options schema
// above). Top-level on purpose: the host keeps name out of CopyTreeOptions so
// the label stays out of the run-history dedupe key. Deliberately no minLength —
// a blank name means "no label given" and must not fail validation (the host
// derives a label instead); the forward paths simply drop a blank one.
const copyTreeNameSchemaJSON = `{ "type": "string", "description": "Short human-readable label for this copy tree, shown in the user's copy-tree run history and in the completion notification. 2 to 4 words describing what the context is for, e.g. 'auth flow context'. Omit it to have Daintree derive a label from the selection." }`

/* --------------------------- copyTree.generate ---------------------------- */

type copyTreeGenerateArgs struct {
	WorktreeID     string           `json:"worktreeId,omitempty"`
	WorktreePath   string           `json:"worktreePath,omitempty"`
	Name           string           `json:"name,omitempty"`
	Options        *copyTreeOptions `json:"options,omitempty"`
	IncludeContent bool             `json:"includeContent,omitempty"`
}

// Validate rejects an unsafe options bag at DECODE time. copyTree.generate keeps
// Daintree's active-worktree fallback (unlike generateAndCopyFile below), so
// there is no target to require here.
func (a copyTreeGenerateArgs) Validate() error { return a.Options.validate() }

func (a copyTreeGenerateArgs) forwardMap() map[string]any {
	m := map[string]any{}
	if a.WorktreeID != "" {
		m["worktreeId"] = a.WorktreeID
	}
	if a.WorktreePath != "" {
		m["worktreePath"] = a.WorktreePath
	}
	// A blank name means "no label given" — leave the key off the wire so the
	// host derives a label from the selection, rather than sending "" as one.
	if strings.TrimSpace(a.Name) != "" {
		m["name"] = a.Name
	}
	if opts := a.Options.wire(); opts != nil {
		m["options"] = opts
	}
	if a.IncludeContent {
		m["includeContent"] = true
	}
	return m
}

var copyTreeGenerateSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "worktreeId": { "type": "string", "minLength": 1, "description": "Worktree to bundle, by id. Omit this and worktreePath to use the active worktree; the id wins when both are given." },
    "worktreePath": { "type": "string", "minLength": 1, "description": "Absolute worktree root path, as an alternative to worktreeId." },
    "name": ` + copyTreeNameSchemaJSON + `,
    "options": ` + copyTreeOptionsSchemaJSON + `,
    "includeContent": { "type": "boolean", "description": "Also return a bounded HEAD of the bundle inline as 'content', with 'contentTruncated' reporting whether it was cut. The file at 'filePath' always holds the whole bundle; set this only when you need to eyeball what was captured." }
  },
  "required": []
}`)

// copyTreeHandlerGuard re-runs the FULL decode contract inside a handler, so the
// "defense in depth for a path that skipped Decode" claim is actually true rather
// than a hand-copied subset that drifts. Cheap: these payloads are tiny and a
// copyTree call is user-scale, not hot-path. In normal dispatch the raw is
// already Decode's canonical output, so this can only ever agree with it.
func copyTreeHandlerGuard[T any](decode tools.DecodeFunc, toolName string, raw json.RawMessage) (T, tools.ToolResult) {
	var a T
	canonical, err := decode(raw)
	if err != nil {
		return a, tools.Fail(domain.CodeValidation, toolName+": "+err.Error())
	}
	if err := json.Unmarshal(canonical, &a); err != nil {
		return a, tools.Fail(domain.CodeValidation, toolName+": could not read arguments: "+err.Error())
	}
	return a, tools.ToolResult{Ok: true}
}

func newCopyTreeGenerateTool(deps Deps) tools.Tool {
	decode := copyTreeStrictDecoder(func() any { return &copyTreeGenerateArgs{} })
	return tools.Tool{
		Name: "copyTree.generate",
		Description: "Generate a Daintree 'copy tree' — a curated bundle of a worktree's files — written to a temporary file, and return that " +
			"file's PATH plus its file count, byte size and budget stats. The bundle is NOT returned inline (it routinely runs to megabytes); " +
			"pass includeContent:true for a bounded head of it. This is the endpoint that touches neither the clipboard nor a terminal: end a " +
			"curation loop HERE when you just need the reference file, and reach for copyTree.generateAndCopyFile only when the user asked for " +
			"it on their clipboard. Curate with " +
			"options.includePaths — the explicit list of files that matter — instead of bundling the whole worktree. The returned path is a system temp file OUTSIDE " +
			"the project, so fs.read and artifact.read CANNOT open it: use includeContent:true to eyeball the bundle yourself, or hand the path to an agent " +
			"terminal that can read it. It is pruned by age either way, so use it promptly.",
		Risk:   domain.RiskRead,
		Schema: copyTreeGenerateSchema,
		Decode: decode,
		Handle: func(ctx context.Context, raw json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			a, bad := copyTreeHandlerGuard[copyTreeGenerateArgs](decode, "copyTree.generate", raw)
			if !bad.Ok {
				return bad
			}
			return passthrough(ctx, deps.MCP, "copyTree.generate", a.forwardMap(), "")
		},
	}
}

/* ---------------------- copyTree.generateAndCopyFile ---------------------- */

type copyTreeCopyFileArgs struct {
	WorktreeID   string           `json:"worktreeId,omitempty"`
	WorktreePath string           `json:"worktreePath,omitempty"`
	Name         string           `json:"name,omitempty"`
	Options      *copyTreeOptions `json:"options,omitempty"`
}

// Validate mirrors Daintree's requireExplicitWorktreeForAgentDispatch: every call
// we make reaches the host as dispatchSource "agent", and an agent caller gets NO
// active-worktree fallback there — it has to name the worktree it curated the
// file list against rather than inherit whichever tab happens to be focused when
// the call lands. Enforcing it here, rather than letting the host reject it,
// means the user is never asked to confirm a system-tier clipboard overwrite
// that was always going to fail: StrictDecoder runs Validate before dispatch
// reaches its confirmation prompt.
func (a copyTreeCopyFileArgs) Validate() error {
	if strings.TrimSpace(a.WorktreeID) == "" && strings.TrimSpace(a.WorktreePath) == "" {
		return errors.New("copyTree.generateAndCopyFile requires an explicit worktreeId (preferred) or worktreePath — assistant calls have no active-worktree fallback; take the id or path from the worktree-listing tool")
	}
	return a.Options.validate()
}

func (a copyTreeCopyFileArgs) forwardMap() map[string]any {
	m := map[string]any{}
	if a.WorktreeID != "" {
		m["worktreeId"] = a.WorktreeID
	}
	if a.WorktreePath != "" {
		m["worktreePath"] = a.WorktreePath
	}
	// Blank name = "no label given": omit it so the host derives a label.
	if strings.TrimSpace(a.Name) != "" {
		m["name"] = a.Name
	}
	if opts := a.Options.wire(); opts != nil {
		m["options"] = opts
	}
	return m
}

// Neither selector is `required` in the schema: "exactly one of these two" is a
// combinator (oneOf/anyOf) that the model's tool-calling surface honours
// inconsistently, so the constraint is carried by the descriptions and enforced
// authoritatively by Validate above.
var copyTreeCopyFileSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "worktreeId": { "type": "string", "minLength": 1, "description": "REQUIRED (this or worktreePath): the worktree to bundle, by id. There is no active-worktree fallback for assistant calls — name the worktree you curated the file list against. The id wins when both are given." },
    "worktreePath": { "type": "string", "minLength": 1, "description": "REQUIRED (this or worktreeId): absolute worktree root path, as an alternative to worktreeId." },
    "name": ` + copyTreeNameSchemaJSON + `,
    "options": ` + copyTreeOptionsSchemaJSON + `
  },
  "required": []
}`)

func newCopyTreeGenerateAndCopyFileTool(deps Deps) tools.Tool {
	decode := copyTreeStrictDecoder(func() any { return &copyTreeCopyFileArgs{} })
	return tools.Tool{
		Name: "copyTree.generateAndCopyFile",
		Description: "Generate a worktree's copy tree and put it on the user's OS clipboard, replacing what they had copied (macOS and Linux " +
			"copy the file itself, Windows its path). System tier — always confirms. Requires an EXPLICIT worktreeId (or worktreePath): there is no active-worktree fallback for " +
			"assistant calls, so name the worktree you curated against. Curate with options.includePaths — the explicit list of files that " +
			"matter — instead of bundling the whole worktree. Daintree shows the user a 'Context copied' notification once the copy lands. " +
			"When you only need the reference file and not the user's clipboard, use copyTree.generate instead.",
		Consequence: "Replaces the operating-system clipboard's current contents with the generated bundle (the file itself on macOS/Linux, its path on Windows), and notifies the user that the context was copied.",
		Risk:        domain.RiskSystem,
		Schema:      copyTreeCopyFileSchema,
		Decode:      decode,
		Handle: func(ctx context.Context, raw json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			// Defense in depth for any path that skips Decode — the host would
			// reject a targetless call too, but only after the user has already
			// confirmed a clipboard overwrite.
			a, bad := copyTreeHandlerGuard[copyTreeCopyFileArgs](decode, "copyTree.generateAndCopyFile", raw)
			if !bad.Ok {
				return bad
			}
			return passthrough(ctx, deps.MCP, "copyTree.generateAndCopyFile", a.forwardMap(), "")
		},
	}
}

/* ----------------------- copyTree.injectToTerminal ------------------------ */

type copyTreeInjectArgs struct {
	TerminalID string           `json:"terminalId"`
	WorktreeID string           `json:"worktreeId,omitempty"`
	Name       string           `json:"name,omitempty"`
	Options    *copyTreeOptions `json:"options,omitempty"`
}

// Validate runs at decode time, so a blank terminal or an unsafe selection is
// rejected before dispatch prompts the user to confirm an injection that would
// fail (or, worse, succeed at pasting the whole worktree). The handler keeps the
// terminalId guard as defense-in-depth for any path that skips Decode.
func (a copyTreeInjectArgs) Validate() error {
	if strings.TrimSpace(a.TerminalID) == "" {
		return errors.New("terminalId must be a non-empty Daintree terminal id")
	}
	return a.Options.validate()
}

// Daintree's injectToTerminal takes only worktreeId — no worktreePath — and
// keeps its active-worktree fallback, so this schema stays narrower than
// generate's on purpose rather than by omission.
var copyTreeInjectSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "terminalId": { "type": "string", "minLength": 1, "description": "Terminal to inject the generated bundle into. Target an IDLE terminal — the payload can be enormous." },
    "worktreeId": { "type": "string", "minLength": 1, "description": "Worktree to bundle, by id; Daintree uses the active worktree when omitted. This action does not accept worktreePath." },
    "name": ` + copyTreeNameSchemaJSON + `,
    "options": ` + copyTreeOptionsSchemaJSON + `
  },
  "required": ["terminalId"]
}`)

func newCopyTreeInjectTool(deps Deps) tools.Tool {
	decode := copyTreeStrictDecoder(func() any { return &copyTreeInjectArgs{} })
	return tools.Tool{
		Name: "copyTree.injectToTerminal",
		Description: "Generate a worktree's copy tree and inject it into a Daintree terminal's input — this is how you hand a large codebase " +
			"context to an agent. Mutating (writes into a terminal), so it always confirms. Curate with options.includePaths — the explicit " +
			"list of files that matter — instead of injecting the whole worktree into a live pane. Target an IDLE terminal: the payload can be " +
			"enormous. Omit worktreeId to use the active worktree (this action takes no worktreePath).",
		Consequence: "Pastes a generated file digest into the named terminal's input. May be large; review the target terminal before approving.",
		Risk:        domain.RiskTerminal,
		Schema:      copyTreeInjectSchema,
		Decode:      decode,
		Handle: func(ctx context.Context, raw json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			// Mirror terminal.sendCommand's guard: a blank terminalId would inject
			// the digest into an unnamed target, so reject it before the MCP call
			// AND before the observer mark below (a mark for a call that never
			// happened would make a settled agent look busy).
			a, bad := copyTreeHandlerGuard[copyTreeInjectArgs](decode, "copyTree.injectToTerminal", raw)
			if !bad.Ok {
				return bad
			}
			m := map[string]any{"terminalId": a.TerminalID}
			if a.WorktreeID != "" {
				m["worktreeId"] = a.WorktreeID
			}
			// Blank name = "no label given": omit it so the host derives a label.
			if strings.TrimSpace(a.Name) != "" {
				m["name"] = a.Name
			}
			if opts := a.Options.wire(); opts != nil {
				m["options"] = opts
			}
			// Attempted input injection invalidates cross-call settle evidence BEFORE
			// the call, same as terminal.sendCommand (see there for why).
			if deps.Observer != nil {
				deps.Observer.MarkCommandSent(strings.TrimSpace(a.TerminalID), domain.NowMS())
			}
			return copyTreeInjectPassthrough(ctx, deps.MCP, a.TerminalID, m)
		},
	}
}

/* --------------------------- terminal.sendCommand ------------------------- */

type sendCommandArgs struct {
	TerminalID string `json:"terminalId"`
	Command    string `json:"command"`
}

var sendCommandSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "terminalId": { "type": "string", "description": "Terminal to send the command to." },
    "command": { "type": "string", "description": "Shell command text to type into the terminal and run." }
  },
  "required": ["terminalId", "command"]
}`)

func newTerminalSendCommandTool(deps Deps) tools.Tool {
	return tools.Tool{
		Name: "terminal.sendCommand",
		Description: "Send a command line to a Daintree terminal — types it into the terminal's input and runs it. Mutating, so it " +
			"always confirms. Typed wrapper around the Daintree terminal.sendCommand MCP tool.",
		Consequence: "Runs a shell command in the named terminal as if you typed it. Effects depend on the command and may not be reversible.",
		Risk:        domain.RiskTerminal,
		Schema:      sendCommandSchema,
		Decode:      tools.StrictDecoder(func() any { return &sendCommandArgs{} }),
		Handle: func(ctx context.Context, raw json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a sendCommandArgs
			_ = json.Unmarshal(raw, &a)
			// The schema trims+requires non-empty; reject whitespace-only here so a
			// blank command never types a bare newline into the terminal.
			if strings.TrimSpace(a.TerminalID) == "" || strings.TrimSpace(a.Command) == "" {
				return tools.Fail(domain.CodeValidation, "terminal.sendCommand: terminalId and command must be non-empty.")
			}
			// Invalidate the terminal's cross-call "seen working" settle evidence
			// BEFORE the send: an ambiguous transport failure may still have delivered
			// the input (the command may start a new task), so waiting for a confirmed
			// success would leave stale evidence standing exactly when it matters. A
			// definitively rejected send injects nothing — the spurious invalidation
			// only routes the next wait to the safe slow path. Keyed by the id as
			// given: Daintree matches ids exactly, so an id it accepts is the same
			// canonical id the waits resolve to.
			if deps.Observer != nil {
				deps.Observer.MarkCommandSent(strings.TrimSpace(a.TerminalID), domain.NowMS())
			}
			return terminalSendCommandPassthrough(ctx, deps.MCP, a.TerminalID, a.Command,
				map[string]any{"terminalId": a.TerminalID, "command": a.Command})
		},
	}
}

/* ------------------------------- terminal.close --------------------------- */

type terminalCloseArgs struct {
	TerminalID  string   `json:"terminalId,omitempty"`
	TerminalIDs []string `json:"terminalIds,omitempty"`
}

// ids merges the singular terminalId and the plural terminalIds into one
// whitespace-trimmed, de-duplicated, order-preserving list. Accepting BOTH shapes
// keeps the tool forgiving: the model reaches for `terminalId` by analogy with
// every other terminal.* wrapper, but `terminalIds` is the whole point of this one
// — retiring a spawned cohort in ONE confirmed call rather than N.
func (a terminalCloseArgs) ids() []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	add(a.TerminalID)
	for _, id := range a.TerminalIDs {
		add(id)
	}
	return out
}

// Validate runs at DECODE time (StrictDecoder honours the Validator seam) — i.e.
// BEFORE the confirmation prompt in dispatch — so an empty/blank call (`{}`,
// `{"terminalIds":[]}`, whitespace-only ids) is rejected as invalid args up front
// instead of prompting the user to confirm a close that then fails. The handler
// keeps the same guard as defense-in-depth for any path that skips Decode.
func (a terminalCloseArgs) Validate() error {
	if len(a.ids()) == 0 {
		return errors.New("provide terminalId (a single id) or terminalIds (a list); at least one non-empty terminal id is required")
	}
	return nil
}

var terminalCloseSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "terminalId": { "type": "string", "description": "A single Daintree terminal id to close." },
    "terminalIds": { "type": "array", "items": { "type": "string" }, "description": "Several terminal ids to close in ONE confirmed call (e.g. retiring a spawned cohort) — prefer this over calling terminal.close once per id." }
  },
  "required": []
}`)

func newTerminalCloseTool(deps Deps) tools.Tool {
	return tools.Tool{
		Name: "terminal.close",
		Description: "Close Daintree terminal(s) — moves each to the trash and ends the agent or process running in it. ONLY call this when the USER has " +
			"explicitly asked you to close a terminal or terminals; closing is never your own decision. Do NOT call it to tidy up, to retire agents you " +
			"spawned, or to recover from a failed/ambiguous/'no terminalId' spawn — a spawn that returned no terminalId is retried or reported, never " +
			"'fixed' by closing terminals. A terminal you did not just spawn yourself may be the user's own live work or another agent mid-task " +
			"(agentState 'working'/'waiting'), so closing it ends that work irreversibly. Pass a single terminalId, or terminalIds:[...] to close a whole " +
			"cohort the user asked you to retire in ONE call. Typed wrapper around the Daintree terminal.close MCP tool. (terminal.kill, reachable via " +
			"daintree.call, deletes PERMANENTLY instead of trashing — prefer close.)",
		Consequence: "Closes the named Daintree terminal(s) — only ever at the user's explicit request — moving them to the trash and ending whatever agent or process is running in each (irreversible for in-flight work).",
		Risk:        domain.RiskTerminal,
		Schema:      terminalCloseSchema,
		Decode:      tools.StrictDecoder(func() any { return &terminalCloseArgs{} }),
		Handle: func(ctx context.Context, raw json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a terminalCloseArgs
			_ = json.Unmarshal(raw, &a)
			ids := a.ids()
			if len(ids) == 0 {
				return tools.Fail(domain.CodeValidation,
					"terminal.close: provide terminalId (a single id) or terminalIds (a list); at least one non-empty terminal id is required.")
			}
			return terminalClosePassthrough(ctx, deps.MCP, ids)
		},
	}
}

/* --------------------- terminal.arm / disarm / disarmAll ------------------ */

type terminalArmingArgs struct {
	TerminalID string `json:"terminalId"`
}

var terminalArmingSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "terminalId": { "type": "string", "description": "Terminal to add to / remove from the fleet arming set." }
  },
  "required": ["terminalId"]
}`)

func newTerminalArmTool(deps Deps) tools.Tool {
	return tools.Tool{
		Name: "terminal.arm",
		Description: "Add a terminal to Daintree's fleet arming set so the human's next broadcast keystrokes are ALSO routed to it. " +
			"Mutating (reroutes the human's input), so it always confirms. Typed wrapper around the Daintree terminal.arm MCP tool. " +
			"Reports the resulting armed set so arming is never silent.",
		Consequence: "Arms a terminal: the human's next broadcast input is also typed into it, and keeps being routed to every armed terminal until disarmed.",
		Risk:        domain.RiskTerminal,
		Schema:      terminalArmingSchema,
		Decode:      tools.StrictDecoder(func() any { return &terminalArmingArgs{} }),
		Handle: func(ctx context.Context, raw json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a terminalArmingArgs
			_ = json.Unmarshal(raw, &a)
			return terminalArmingPassthrough(ctx, deps.MCP, "terminal.arm",
				map[string]any{"terminalId": a.TerminalID}, "Armed terminal "+a.TerminalID+".")
		},
	}
}

func newTerminalDisarmTool(deps Deps) tools.Tool {
	return tools.Tool{
		Name: "terminal.disarm",
		Description: "Remove a terminal from Daintree's fleet arming set so it no longer receives the human's broadcast input. " +
			"Mutating (reroutes the human's input), so it always confirms. Typed wrapper around the Daintree terminal.disarm MCP tool. " +
			"Reports the resulting armed set so the change is never silent.",
		Consequence: "Disarms a terminal: it stops receiving the human's broadcast input. Changes where keystrokes are routed.",
		Risk:        domain.RiskTerminal,
		Schema:      terminalArmingSchema,
		Decode:      tools.StrictDecoder(func() any { return &terminalArmingArgs{} }),
		Handle: func(ctx context.Context, raw json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a terminalArmingArgs
			_ = json.Unmarshal(raw, &a)
			return terminalArmingPassthrough(ctx, deps.MCP, "terminal.disarm",
				map[string]any{"terminalId": a.TerminalID}, "Disarmed terminal "+a.TerminalID+".")
		},
	}
}

func newTerminalDisarmAllTool(deps Deps) tools.Tool {
	return tools.Tool{
		Name: "terminal.disarmAll",
		Description: "Clear Daintree's entire fleet arming set so no terminal receives the human's broadcast input. Mutating " +
			"(reroutes the human's input), so it always confirms. Typed wrapper around the Daintree terminal.disarmAll MCP tool. " +
			"Reports the resulting (empty) armed set so the change is never silent.",
		Consequence: "Disarms every terminal at once: the human's broadcast input stops being routed anywhere. Changes where keystrokes are routed.",
		Risk:        domain.RiskTerminal,
		Schema:      noArgs,
		Decode:      tools.StrictDecoder(func() any { return &struct{}{} }),
		Handle: func(ctx context.Context, _ json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			return terminalArmingPassthrough(ctx, deps.MCP, "terminal.disarmAll",
				map[string]any{}, "Cleared the fleet arming set.")
		},
	}
}
