package mcpx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/safety"
	"github.com/daintreehq/assistant/internal/tools"
)

/* ----------------------------- daintree.invoke ---------------------------- */

// daintree.invoke is the third step of search → schema → invoke, and the whole
// difference between it and daintree.call is WHICH action the safety pipeline
// thinks is running.
//
// daintree.call is registered at system risk, so every call it carries — a
// terminal listing as much as a worktree deletion — is tier-gated, previewed,
// typed-confirmed and audited as "daintree.call". That is honest for an unbounded
// escape hatch, and it is why the escape hatch is kept exactly as it is. What it
// cannot do is describe the action it forwarded: the audit row names the invoker,
// a grant can only be written against the invoker (so it is refused outright), and
// the human approving it is shown the invoker's generic consequence text.
//
// This tool resolves the target FIRST and then lets dispatch run its normal
// pipeline against THAT identity (tools.TargetResolver). The action decides the
// risk class, the approval sheet previews the action and its real arguments, a
// grant is written against "daintree.invoke:<action>" and matches nothing else,
// and the audit row carries both halves. Nothing about the pipeline is bypassed —
// the same Decide, the same Confirm, the same ConsumeGrant, the same audit — it is
// simply told the truth about what it is gating.
//
// It is narrower than daintree.call on every axis, and deliberately so:
//
//   - only actions in the LOCAL live MCP catalog (a hidden, restricted, or
//     unoffered action is not there, and no argument can make it appear);
//   - only actions this repo has REVIEWED and classified (policy.go) — everything
//     else is refused and left to the escape hatch's system/typed-confirm path;
//   - only actions with NO typed wrapper (a wrapper is a better contract and
//     redirecting to it is not a downgrade);
//   - only arguments that VALIDATE against the action's own advertised JSON Schema.
//
// So daintree.invoke can never reach anything daintree.call could not, and always
// reaches it under a tighter, better-described policy.

const (
	codeActionUnavailable = "MCP_ACTION_UNAVAILABLE"
	codePolicyUnknown     = "MCP_ACTION_POLICY_UNKNOWN"
	codeArgsSchemaInvalid = "MCP_ARGS_SCHEMA_MISMATCH"
	codePolicyDrift       = "MCP_ACTION_POLICY_DRIFT"
)

// maxInvokeSchemaBytes bounds the raw schema we will compile. The schema is
// attacker-adjacent input in the weak sense that it comes from whatever server
// DAINTREE_MCP_URL points at, and jsonschema.Resolve walks it eagerly. A real
// Daintree action schema is a few KB; 256 KiB is far past any of them and still
// far short of anything that could hurt.
const maxInvokeSchemaBytes = 256 * 1024

// maxValidationErrChars bounds the validator message we hand back. The library
// reports failures as a wrapped-error chain that flattens to one long string
// naming every nested step; the leading portion carries the actual constraint,
// and an unbounded tail would push a genuinely useful message toward the result cap.
const maxValidationErrChars = 600

type invokeArgs struct {
	Action     string         `json:"action"`
	Arguments  map[string]any `json:"arguments,omitempty"`
	RequestKey string         `json:"requestKey,omitempty"`
}

// Validate re-states the bounds the declared schema advertises. StrictDecoder
// rejects unknown fields and type mismatches but runs no schema engine, so
// minLength/maxLength would otherwise be advisory. Padding is rejected rather than
// trimmed: the action confirmed, granted and audited must be byte-for-byte the
// action forwarded, and silent trimming breaks that in one direction.
func (a invokeArgs) Validate() error {
	if strings.TrimSpace(a.Action) == "" {
		return fmt.Errorf("action is required — pass the exact MCP action name, e.g. {\"action\":\"terminal.list\"}")
	}
	if a.Action != strings.TrimSpace(a.Action) {
		return fmt.Errorf("action has leading or trailing whitespace; pass it exactly, e.g. {\"action\":\"terminal.list\"}")
	}
	if utf8.RuneCountInString(a.Action) > maxSchemaNameLen {
		return fmt.Errorf("action is %d characters; MCP action names are at most %d",
			utf8.RuneCountInString(a.Action), maxSchemaNameLen)
	}
	return nil
}

var invokeSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "action": {
      "type": "string",
      "minLength": 1,
      "maxLength": 128,
      "description": "Exact action name from tool.search, never auto-corrected."
    },
    "arguments": {
      "type": "object",
      "additionalProperties": true,
      "description": "Object matching the inputSchema tool.schema returned. Validated against it before anything runs or is confirmed."
    },
    "requestKey": {
      "type": "string",
      "description": "Idempotency key Daintree dedupes on. Set one for every autonomous mutation. Do not repeat it inside the arguments object."
    }
  },
  "required": ["action"]
}`)

func newInvokeTool(deps Deps) tools.Tool {
	return tools.Tool{
		Name: "daintree.invoke",
		Description: "Invoke ONE Daintree MCP action that has no typed wrapper, gated as THAT action: a read runs with no " +
			"confirmation, a mutation confirms at its own risk. Find the action with tool.search, read its shape with " +
			"tool.schema, then call {\"action\":\"terminal.list\",\"arguments\":{...}}. Only actions tool.search reports " +
			"`invocable: true` run here; anything else is refused with the alternative named. An action this build has not " +
			"reviewed stays reachable only via daintree.call.",
		Consequence: "Runs the named Daintree MCP action with the arguments shown.",
		// FAIL-CLOSED CEILING, not the risk this tool usually runs at. Every surface
		// that cannot execute a resolver reads this field: the sub-agent inventory
		// filter admits read-risk tools (so this stays out of an unattended read-only
		// run), the parallel-dispatch adapters gate on it, and the capability
		// reference documents it. Registering low and resolving high would invert all
		// three. ResolveTarget narrows it per call; nothing widens it.
		Risk:          domain.RiskSystem,
		Schema:        invokeSchema,
		Decode:        tools.StrictDecoder(func() any { return &invokeArgs{} }),
		ResolveTarget: invokeResolver(deps),
		Handle: func(ctx context.Context, raw json.RawMessage, tctx *tools.ToolContext) tools.ToolResult {
			// Re-resolve rather than carry state across the gate. The resolver ran over
			// these same immutable bytes, so it reaches the same verdict — but "the
			// action approved is the action forwarded" is worth enforcing structurally
			// instead of trusting that no future refactor moves the two apart.
			target, refusal := resolveInvoke(ctx, deps, raw, tctx)
			if refusal != nil {
				return *refusal
			}
			// Everything derived from the (immutable) arguments necessarily agrees. The
			// live catalog and a future host policy source are NOT immutable, so compare
			// against what dispatch actually gated: a policy that said "read" at the gate
			// and "external" here would otherwise run unconfirmed under a risk nobody
			// approved. Drift is a refusal, never a best-effort.
			if g := tctx.GatedTarget; g != nil {
				if g.Name != domain.DynamicTargetName(target.action) || g.Risk != target.policy.Risk {
					return tools.Fail(codePolicyDrift, fmt.Sprintf(
						"The policy for %s changed between authorization and execution (approved as %s/%s, now %s/%s), so it "+
							"was NOT run. Retry to re-authorize under the current policy.",
						target.action, g.Display, g.Risk, target.action, target.policy.Risk), tools.Unrecoverable())
				}
			}

			args := make(map[string]any, len(target.arguments)+1)
			for k, v := range target.arguments {
				args[k] = v
			}
			// requestKey is forwarded as a normal argument, exactly as daintree.call
			// does. Daintree dedupes on it and strips it before its own validation, so
			// it never trips the schema check above.
			if target.requestKey != "" {
				args["requestKey"] = target.requestKey
			}

			res, err := deps.MCP.CallTool(ctx, target.action, args)
			if err != nil {
				if ctx.Err() != nil {
					return tools.Fail(codeCancelled, fmt.Sprintf("Turn cancelled during %s.", target.action), tools.Unrecoverable())
				}
				return tools.Fail(codeMCPToolError, fmt.Sprintf("Daintree MCP action %s failed: %s", target.action, err.Error()))
			}
			if res.IsError {
				msg := res.Text
				if msg == "" {
					msg = fmt.Sprintf("Daintree MCP action %s returned an error.", target.action)
				}
				return tools.Fail(codeMCPToolError, msg,
					tools.WithDetails(map[string]any{"structuredContent": res.StructuredContent}))
			}
			return tools.Ok(fmt.Sprintf("Ran %s (%s).", target.action, target.policy.Risk), map[string]any{
				"action": target.action, "risk": string(target.policy.Risk),
				"text": res.Text, "structuredContent": res.StructuredContent, "isError": res.IsError,
			})
		},
	}
}

// resolvedInvoke is one fully-checked invocation: the action exists in this
// session's catalog, has a reviewed policy, has no typed wrapper, and its
// arguments validate against its own schema.
type resolvedInvoke struct {
	action     string
	arguments  map[string]any
	requestKey string
	policy     TargetPolicy
}

// invokeResolver adapts resolveInvoke to the dispatch hook. Everything the gate
// needs comes from the same function the handler runs.
func invokeResolver(deps Deps) tools.TargetResolver {
	return func(ctx context.Context, raw json.RawMessage, tctx *tools.ToolContext) (tools.TargetInfo, *tools.ToolResult) {
		target, refusal := resolveInvoke(ctx, deps, raw, tctx)
		if refusal != nil {
			return tools.TargetInfo{}, refusal
		}
		return tools.TargetInfo{
			Name:    domain.DynamicTargetName(target.action),
			Display: target.action,
			Risk:    target.policy.Risk,
			Consequence: fmt.Sprintf("Runs the Daintree MCP action %s (%s risk%s). %s",
				target.action, target.policy.Risk, dangerSuffix(target.policy), target.policy.Summary),
		}, nil
	}
}

func dangerSuffix(p TargetPolicy) string {
	if p.Danger == "" || p.Danger == "safe" {
		return ""
	}
	return ", Daintree danger:" + p.Danger
}

// resolveInvoke runs every gate in the order a refusal is most useful in.
//
// Ordering is load-bearing. The checks that hold regardless of connectivity come
// first, so a disconnected MCP still yields "use the typed wrapper" rather than
// "not connected" for a call that was wrong either way. The file-edit guard sits
// above everything reachable, because it is an invariant of the process, not a
// property of this session's catalog.
func resolveInvoke(ctx context.Context, deps Deps, raw json.RawMessage, _ *tools.ToolContext) (resolvedInvoke, *tools.ToolResult) {
	var a invokeArgs
	_ = json.Unmarshal(raw, &a)
	action := a.Action

	// 1. No-file-edit invariant, re-checked on the raw forwarded name exactly as
	//    daintree.call does. Registration-time enforcement only covers LOCAL names.
	if safety.IsForbiddenToolName(action) {
		return fail(safety.FileEditForbiddenCode, fmt.Sprintf(
			"Refusing to invoke %s — the assistant never edits files directly. Spawn a visible agent "+
				"(agentTask.spawnForEdits) to make changes.", action), true)
	}

	// 2. Typed wrapper wins. A wrapper resolves identifiers, attaches watchers, and
	//    strict-decodes its arguments; routing around it through a generic invoker
	//    loses all three, which is the same bypass the daintree.call denylist exists
	//    to prevent.
	if wrapper := preferredWrapperFor(action); wrapper != "" {
		return fail(codeUseTypedWrapper, fmt.Sprintf(
			"Do not invoke %s dynamically — a typed tool governs it: %s. It takes named, validated parameters, so you "+
				"cannot drop a required argument. Switch tools; do not retry this call.", action, wrapper), false)
	}

	// 3. Policy. Checked BEFORE the catalog read so an unclassified action gets the
	//    same answer whether or not Daintree is reachable — the refusal is about this
	//    repo's review state, which no session can change.
	policy := ResolveTargetPolicy(deps.Policy, action)
	if !policy.Known {
		return fail(codePolicyUnknown, fmt.Sprintf(
			"%s has no reviewed policy in this build, so it cannot be invoked dynamically — its real risk is unknown and "+
				"assuming it is harmless is the one thing that must not happen here. It remains reachable through "+
				"daintree.call, which gates any unclassified action at system tier with a typed confirmation. "+
				"Dynamically invocable actions: %s.",
			action, strings.Join(ClassifiedActionNames(), ", ")), false)
	}

	// 4. Connectivity, then eligibility for THIS session.
	if !deps.MCP.Connected() {
		return fail(codeMCPUnavailable, fmt.Sprintf(
			"Daintree MCP is not connected; cannot invoke %s. Use /reconnect to retry once Daintree is available.", action), false)
	}
	list, err := deps.MCP.ListTools(ctx, false)
	if err != nil {
		if ctx.Err() != nil {
			return fail(codeCancelled, fmt.Sprintf("Turn cancelled while resolving %s.", action), true)
		}
		return fail(codeMCPUnavailable, fmt.Sprintf(
			"Could not read the Daintree MCP catalog to resolve %s: %s Use /reconnect to retry once Daintree is available.",
			action, err.Error()), false)
	}
	match, found, ambiguous := resolveSchemaTool(list, action)
	if ambiguous {
		return fail(codeSchemaInvalid, fmt.Sprintf(
			"The Daintree MCP catalog advertises %q more than once with different schemas, so its argument contract is "+
				"ambiguous. Report this rather than invoking it.", action), true)
	}
	if !found {
		// A hidden, restricted, or simply unoffered action is INDISTINGUISHABLE from a
		// missing one here, and that is the correct hard ceiling: the live catalog is
		// this session's authority on what may be called, so absence is a refusal, never
		// a prompt to try harder. Naming it diagnostically is fine; invoking it is not.
		return fail(codeActionUnavailable, fmt.Sprintf(
			"%s is not available to this Daintree session — it is not in the catalog this session may call (it may be "+
				"hidden, restricted to a higher host tier, or simply absent from this Daintree build). It cannot be "+
				"invoked. Use tool.search to see what this session can actually run.", action), true)
	}

	// 5. requestKey is a TRANSPORT field, not an argument. It is merged into the
	//    forwarded map after validation, so allowing it inside `arguments` too would
	//    mean the object validated is not the object sent: a schema-valid inner value
	//    would be silently overwritten by the outer one.
	if _, clash := a.Arguments["requestKey"]; clash {
		return fail(codeArgsSchemaInvalid, fmt.Sprintf(
			"Do not put requestKey inside `arguments` for %s — pass it as the top-level `requestKey` field. It is a "+
				"transport key Daintree strips before validating, and accepting it in both places would send a value "+
				"that is not the one checked against the schema.", action), false)
	}

	// 6. Arguments against the action's OWN schema, before any human is asked to
	//    approve. Validating after confirmation would spend an approval on a call
	//    that was always going to be rejected.
	if refusal := validateInvokeArgs(action, match.InputSchema, match.InputSchemaProvided, a.Arguments); refusal != nil {
		return resolvedInvoke{}, refusal
	}

	return resolvedInvoke{action: action, arguments: a.Arguments, requestKey: a.RequestKey, policy: policy}, nil
}

func fail(code, msg string, unrecoverable bool) (resolvedInvoke, *tools.ToolResult) {
	opts := []domain.FailOption{}
	if unrecoverable {
		opts = append(opts, tools.Unrecoverable())
	}
	res := tools.Fail(code, msg, opts...)
	return resolvedInvoke{}, &res
}

/* --------------------------- argument validation -------------------------- */

// resolvedSchemaCache memoizes compiled schemas by the hash of their canonical
// JSON. Resolve walks and links the whole schema and is the expensive half;
// Validate is cheap and the *Resolved it returns is immutable and safe to share
// across goroutines. Keying on content rather than action name means a host that
// changes an action's schema mid-session gets a fresh compile rather than a stale
// contract.
var (
	resolvedSchemaMu    sync.Mutex
	resolvedSchemaCache = map[string]*jsonschema.Resolved{}
)

// validateInvokeArgs checks args against the action's advertised input schema.
//
// A schema we cannot compile is NOT a reason to skip validation — that would make
// a broken or hostile schema the easiest way to turn the checked path back into an
// unchecked one. It is a refusal: the action stays reachable through daintree.call,
// where the system-tier confirmation is the human's own check on the arguments.
func validateInvokeArgs(action string, inputSchema map[string]any, provided bool, args map[string]any) *tools.ToolResult {
	// `provided` is the check, not `len(inputSchema)`. The MCP client substitutes
	// {"type":"object","properties":{}} for a tool that advertised none, and that
	// object is non-empty AND accepts every possible argument map — so keying off
	// length alone would turn "this action published no contract" into "this action
	// permits anything", which is the exact fail-OPEN this gate exists to prevent.
	// An empty map means the field never survived the seam, which is a plumbing bug;
	// both answers are the same refusal.
	if !provided || len(inputSchema) == 0 {
		res := tools.Fail(codeSchemaInvalid, fmt.Sprintf(
			"Daintree advertises no input schema for %s, so its arguments cannot be validated and it will not be "+
				"invoked dynamically. Use daintree.call if this action really must be run — its system-tier "+
				"confirmation puts a human in front of the arguments instead.", action), tools.Unrecoverable())
		return &res
	}
	rawSchema, err := json.Marshal(inputSchema)
	if err != nil {
		res := tools.Fail(codeSchemaInvalid, fmt.Sprintf(
			"The input schema Daintree advertises for %s could not be encoded (%v), so arguments cannot be validated.",
			action, err), tools.Unrecoverable())
		return &res
	}
	if len(rawSchema) > maxInvokeSchemaBytes {
		res := tools.Fail(codeSchemaTooLarge, fmt.Sprintf(
			"The input schema for %s is %d bytes, past the %d-byte limit this tool will compile; it will not be invoked "+
				"dynamically.", action, len(rawSchema), maxInvokeSchemaBytes), tools.Unrecoverable())
		return &res
	}

	resolved, err := compileSchema(rawSchema)
	if err != nil {
		res := tools.Fail(codeSchemaInvalid, fmt.Sprintf(
			"The input schema Daintree advertises for %s is not a usable JSON Schema (%v), so arguments cannot be "+
				"validated and it will not be invoked dynamically.", action, err), tools.Unrecoverable())
		return &res
	}

	// A nil map and an absent `arguments` field must validate identically to `{}`;
	// otherwise a no-argument action would fail its own `"type":"object"` check.
	instance := any(args)
	if args == nil {
		instance = map[string]any{}
	}
	if err := resolved.Validate(instance); err != nil {
		// The corrective call is spelled out as a literal object because tool.schema
		// takes `name` while this tool takes `action` — prose naming the wrong key is
		// exactly the failure this family exists to stop.
		res := tools.Fail(codeArgsSchemaInvalid, fmt.Sprintf(
			"Arguments for %s do not match its input schema: %s. Call tool.schema with {\"name\":%q} to see the "+
				"required shape, then retry with corrected arguments.",
			action, clampErr(err.Error()), action))
		return &res
	}
	return nil
}

// compileSchema resolves rawSchema, memoized by content hash.
//
// Resolve is called with nil options ON PURPOSE: with no Loader configured, a
// remote `$ref` fails to resolve instead of being fetched, so a schema served by
// whatever DAINTREE_MCP_URL points at can never make this process issue an
// outbound request. Go's regexp is RE2, so a `pattern` keyword cannot backtrack
// pathologically either.
func compileSchema(rawSchema []byte) (*jsonschema.Resolved, error) {
	sum := sha256.Sum256(rawSchema)
	key := hex.EncodeToString(sum[:])

	resolvedSchemaMu.Lock()
	cached, ok := resolvedSchemaCache[key]
	resolvedSchemaMu.Unlock()
	if ok {
		return cached, nil
	}

	var schema jsonschema.Schema
	if err := json.Unmarshal(rawSchema, &schema); err != nil {
		return nil, err
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		return nil, err
	}

	resolvedSchemaMu.Lock()
	resolvedSchemaCache[key] = resolved
	resolvedSchemaMu.Unlock()
	return resolved, nil
}

// clampErr bounds a validator message to maxValidationErrChars runes INCLUDING the
// elision marker, keeping both ends.
//
// The library wraps one error per recursion step, so a deep schema produces
// "validating X: validating Y: … : required: missing properties: [\"cwd\"]" — the
// outer context leads and the ACTUAL CONSTRAINT is at the tail. A head-only clamp
// would therefore drop precisely the half the model needs to fix its call, on
// exactly the schemas where fixing it is hardest. Keeping a short head (which names
// the failing path) and a longer tail (which names the rule) preserves both.
func clampErr(msg string) string {
	msg = strings.TrimSpace(msg)
	if utf8.RuneCountInString(msg) <= maxValidationErrChars {
		return msg
	}
	const marker = " … "
	r := []rune(msg)
	budget := maxValidationErrChars - len([]rune(marker))
	head := budget / 4
	tail := budget - head
	return string(r[:head]) + marker + string(r[len(r)-tail:])
}

/* ------------------------- discovery policy blocks ------------------------ */

// policyBlock renders the machine-readable policy for one action, shared by
// tool.schema and the discovery tools so the two can never describe the same
// action differently.
//
// `invocable` answers the only question that matters at the call site, and it is
// deliberately NOT just "is the policy known": a wrapped action has a known policy
// and still must not be invoked here. When it is false, `unavailableReason` says
// which gate fired and what to use instead — an unavailable action may be NAMED,
// which is what makes a diagnostic listing useful, but naming it never makes it
// callable.
func policyBlock(deps Deps, action string, schemaProvided bool) map[string]any {
	out := map[string]any{}

	// The wrapper answer comes FIRST and wins outright. A wrapped action is
	// deliberately never classified in the catalog, so asking about its policy first
	// would fall into the unknown branch and tell the model to use daintree.call —
	// which denylists the very same name and refuses it. That is a two-round dead end
	// that reads as a broken tool, and the correct answer was available immediately.
	if wrapper := preferredWrapperToolName(action); wrapper != "" {
		out["preferredTool"] = wrapper
		out["invocable"] = false
		out["unavailableReason"] = "The typed tool " + wrapper + " governs this action — call it directly. Neither " +
			"daintree.invoke nor daintree.call will forward this name."
		return out
	}

	policy := ResolveTargetPolicy(deps.Policy, action)
	if !policy.Known {
		out["policySource"] = "unknown"
		out["invocable"] = false
		out["unavailableReason"] = "No reviewed policy for this action in this build; it is reachable only through " +
			"daintree.call, which gates it at system tier with a typed confirmation."
		return out
	}
	out["policySource"] = policy.Source
	out["risk"] = string(policy.Risk)
	out["requiredTier"] = string(policy.RequiredTier())
	out["confirms"] = policy.Confirms()
	if policy.Danger != "" {
		out["danger"] = policy.Danger
	}
	if !schemaProvided {
		// Reported here, not only at call time, so the model does not spend a round
		// discovering it. An action whose server published no schema cannot be
		// argument-checked, and this path refuses to invoke what it cannot check.
		out["invocable"] = false
		out["unavailableReason"] = "Daintree advertises no input schema for this action, so its arguments cannot be " +
			"validated; it is reachable only through daintree.call."
		return out
	}
	out["invocable"] = true
	return out
}
