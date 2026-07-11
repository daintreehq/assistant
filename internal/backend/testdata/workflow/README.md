# Workflow-intelligence golden wire fixtures

Canonical wire shapes for the three workflow tasks (`workflow_plan`,
`workflow_reconcile`, `workflow_resume_digest`) on `POST /v1/daintree/tasks`.
The backend Pydantic models in
`src/daintree_assistant_server/contracts/tasks.py` are the AUTHORITY; these
files are generated from those models and pinned byte-for-byte by
`tests/wire/test_workflow_wire.py`. The Go CLI copies these exact files into
its testdata and decodes them with its real DTOs — do not hand-edit one side
without the other.

## Which wire layer the fixtures represent

The task endpoint wraps every result in the `TaskResult` envelope:

```json
{
  "id": "task_…",
  "object": "daintree.task.result",
  "task": "workflow_reconcile",
  "model": "…",
  "output": { … },
  "finish_reason": "stop",
  "usage": { … },
  "prompt_version": "workflow_reconcile"
}
```

- `plan_output.json`, `reconcile_output.json`, `resume_output.json` are the
  **value of the `output` field** (the innermost task result object), NOT the
  envelope. Go should unmarshal each file directly into its
  plan / reconcile-patch / resume-digest DTO — the same type it uses for
  `TaskResult.output` after decoding the envelope.
- `reconcile_input.json` is the **value of the `input` field of the task
  REQUEST** (`{"task": "workflow_reconcile", "input": { … }}`) — the canonical
  shape the Go client must SEND. Go should assert that marshalling its request
  DTOs reproduces this shape (snake_case keys throughout; nodes carry
  `id`/`status`/`depends_on`; edges carry `source`/`target`).

## Serialization contract (how the output bytes are produced)

The server serializes task outputs with Pydantic `model_dump()` defaults
(`services/task_runner.py`): **no field aliases, no `exclude_none`, no
`exclude_defaults`** — every field is always present, in model declaration
order, with `null` / `""` / `[]` / `{}` for unset values. The fixture files are
`json.dumps(output, indent=2, ensure_ascii=False)` plus a trailing newline.
Go decoders must therefore tolerate explicit nulls, and Go tests should treat
a *missing* key as a contract failure, not as an implicit zero value.

Key spellings that have drifted before and are load-bearing:

- edges: `source` / `target` (never `from` / `to`)
- nodes: `depends_on` (never `dependsOn`), `requires_confirm`, `tool_name`,
  `tool_args`, `expected_evidence`, `async_policy`
- patches: `node_id`, `resource_id`, `base_revision`, `resolve_blockers`
- blockers: `summary` (required) + `kind` + `node_id` + optional `id`
- resume items: `workflow_id`, `headline`, `blockers`,
  `recommended_action.requires_confirmation`

Note: the input models are `extra="ignore"` and the reconcile snapshot reader
is lenient, so drifted keys in the INPUT are not rejected — they are silently
*not read*, which disables server-side cycle detection and the terminal-status
guard. `test_workflow_wire.py` pins that hazard; the canonical form in
`reconcile_input.json` is required for the safety checks to work at all.

## Regenerating

Change the models first, then regenerate the fixtures from the models (build
the payload, `Model.model_validate(...)`, run it through
`services/workflow_validation.py`, `model_dump()`, and write with
`json.dumps(..., indent=2, ensure_ascii=False) + "\n"`), and copy the new
files to the Go repo in the same change.
