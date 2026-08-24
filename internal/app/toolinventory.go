package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/daintreehq/assistant/internal/agent"
	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/config"
)

// toolinventory.go exports the tool projection this CLI sends the backend — the same
// JSON value it puts in `input.tools` every turn, indented (see RenderToolInventory for
// exactly how far "same" goes).
//
// It exists because the backend pins a captured copy of that payload (it is the largest
// single region of a request, and its runbooks name the tools in it), and refreshing that
// pin previously required EDITING THIS REPO: write a throwaway test that guesses how to
// build the registry, run it, delete it, hope `git status` came back clean. The guess
// went stale exactly as you would expect — the backend's recipe still named the
// pre-rename module path and claimed a booted App was required — and the drift it hid
// was real: six tools had left the projection while five backend runbooks still instructed
// the model to call them. A runbook naming a tool that is not offered, while the base
// prompt forbids inventing one, stalls the turn on a contradiction the model cannot
// report usefully.
//
// So the construction lives here, committed, next to the wiring it depends on. When
// DefaultToolBuilder's requirements change, this file changes with it and the consumer's
// command line does not.
//
// The projection is deliberately taken from a REAL App rather than a hand-assembled
// registry: boot is the only thing that can promise the output equals what a turn sends.
// The App is offline and scoped to a throwaway state directory, so generating the
// inventory neither touches the user's own state nor reaches the network.

// ToolInventoryOptions selects WHICH projection to export.
type ToolInventoryOptions struct {
	// WorkflowIntelligence includes the seven execution-graph tools that register only
	// under DAINTREE_WORKFLOW_INTELLIGENCE=1. Default OFF, which is what a normal launch
	// sends — a fixture pinned with the flag on would tell the backend that
	// workflow.plan is always offered, and a runbook written against that promise would
	// name an unoffered tool for every user who has not opted in.
	WorkflowIntelligence bool
}

// BuildToolInventory returns the wire tool inventory for the given options: the
// registry's OpenAITools(nil) projection, put through the SAME conversion and
// name/description validation a real turn applies (agent.ToBackendTools), so the export
// cannot pass while the live request would be rejected.
// Cleanup errors are REPORTED, not swallowed: a Shutdown that cannot close the database,
// or a state directory that cannot be removed, both mean this run left something behind,
// and a generator that prints a perfect fixture while quietly leaking a SQLite tree into
// $TMPDIR on every invocation is how a small leak becomes a large one. The inventory is
// still returned alongside such an error — the bytes are correct, only the tidying up
// failed — so a caller can decide whether to use them.
func BuildToolInventory(opts ToolInventoryOptions) (inv []backend.Tool, err error) {
	dir, err := os.MkdirTemp("", "daintree-toolinventory-")
	if err != nil {
		return nil, fmt.Errorf("temp state dir: %w", err)
	}
	// LIFO order is load-bearing: Shutdown must release the SQLite handle before the
	// directory holding it is removed. Deferred second, so it runs first.
	defer func() {
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			err = errors.Join(err, fmt.Errorf("remove temp state dir: %w", rmErr))
		}
	}()

	offline := true
	tier := "operator"
	flag := opts.WorkflowIntelligence
	a, err := Create(CreateOptions{
		Overrides: config.ConfigOverrides{
			Offline:              &offline,
			StateDir:             &dir,
			ProjectPath:          &dir,
			Tier:                 &tier,
			WorkflowIntelligence: &flag,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("build app: %w", err)
	}
	defer func() {
		if sdErr := a.Shutdown(); sdErr != nil {
			err = errors.Join(err, fmt.Errorf("shutdown: %w", sdErr))
		}
	}()

	// Project through the SAME seam a turn uses (the App's toolRunner), with a nil
	// filter — the full registry, which is what every turn offers, since runbook selection
	// is server-owned and a loaded runbook never narrows the toolset.
	projected, err := newToolRunner(a).OpenAITools(nil)
	if err != nil {
		return nil, fmt.Errorf("project tools: %w", err)
	}
	return agent.ToBackendTools(projected)
}

// RenderToolInventory serializes the inventory as the same JSON VALUE the wire carries,
// with two byte-level deviations that exist purely for the consumer: two-space
// indentation and a trailing newline. Compacting the result reproduces the wire bytes
// exactly; the raw output does not, and nothing here should claim otherwise.
//
// Indented output is the point of the format choice. The fixture is refreshed by
// regenerating and committing the diff, so a one-line 100 KB blob would render every
// refresh as a single changed line — indistinguishable between "one description was
// shortened" and "half the registry was removed". Indented, a refresh is an additive,
// reviewable diff, which is the difference between a gate someone reads and a gate
// someone rubber-stamps.
//
// Determinism is not decoration here: registration order is already stable, and Go's
// encoder emits struct fields in declaration order, so the same registry serializes to
// the same bytes on every run and on every platform.
//
// HTML escaping is left ON — the encoder default — which looks wrong and is not. The
// real request is built with json.Marshal (internal/backend/client.go), which escapes
// the three HTML-significant characters inside every string it writes. Turning escaping
// off here would produce a prettier fixture that is no longer the payload, and the point
// of this export is fidelity to the bytes sent, not readability of the strings in them.
// Compacting this output therefore reproduces json.Marshal exactly, which is what the
// paired test asserts.
func RenderToolInventory(inv []backend.Tool) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	// A nil inventory would encode as `null`; an empty registry is still an empty LIST.
	// (The real request omits `input.tools` entirely in that case — `omitempty` — so this
	// is a fixture-consumer courtesy, not a claim about the wire. It should never happen:
	// an empty registry fails AssertRegistered at boot long before reaching here.)
	if inv == nil {
		inv = []backend.Tool{}
	}
	if err := enc.Encode(inv); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
