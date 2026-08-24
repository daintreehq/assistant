package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/daintreehq/assistant/internal/app"
	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/cli/render"
	"github.com/daintreehq/assistant/internal/domain"
)

// runbooks.go is the `--list-runbooks` route: what runbooks can this backend load?
//
// Until now the only way to answer that was to read the backend's source, which makes
// `--runbook` unusable — you cannot name a thing you cannot enumerate, and a mistyped id
// produces a run that looks fine. This is deliberately the LIGHTEST route in the binary:
// one capability GET. No owner lease, no database, no MCP, no App, no turn. Listing what
// a backend can load is a question about the backend, and it must be answerable while
// another assistant owns the project — and, for the same reason, without creating a
// state directory the caller never asked for (config.LoadConfigForProbe).

// listRunbooksTimeout bounds the capability read END TO END, retries included. A listing
// that hangs is worse than one that fails: the caller is usually a human mid-sentence,
// or a script about to choose an id. The shared client retries a transient failure, so
// this is ONE logical read rather than literally one request — which is the right
// trade, since a catalog that vanishes on a blip would send someone hunting a backend
// problem that healed a second later.
const listRunbooksTimeout = 15 * time.Second

// RunbookCatalogJSON is the `--list-runbooks --json` document.
//
// It is ONE indented JSON object on stdout, deliberately NOT the one-shot JSONL event
// stream: there is no run here to narrate, and a consumer of a listing wants a value it
// can pipe into `jq`, not a sequence of events it must reassemble.
type RunbookCatalogJSON struct {
	// CatalogRevision is what a caching caller keys this list on. Conservative, not
	// exact: the backend's revision hashes each runbook's body too, so it moves on an
	// edit that leaves these ids and titles byte-identical.
	CatalogRevision string             `json:"catalogRevision"`
	Runbooks          []backend.RunbookRef `json:"runbooks"`
}

// runbookCatalogErrorJSON is what a failure looks like under --json. A caller parsing
// stdout must never receive prose on the one path it cannot handle — the same rule
// `doctor --json` follows.
type runbookCatalogErrorJSON struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// errCatalogNotAdvertised separates "this backend serves no catalog" from "the fetch
// failed". They need different next actions — upgrade the backend versus check the
// endpoint — and collapsing them sends the reader to fix the wrong thing.
var errCatalogNotAdvertised = errors.New(
	"this backend does not advertise a runbook catalog, so there is nothing to list; upgrade the backend to use --list-runbooks")

// RunListRunbooks is the `--list-runbooks` entry point.
//
// Exit codes follow the one-shot contract: 0 on a catalog read (INCLUDING an advertised
// empty one — "this backend loads nothing" is a successful answer to the question), 1 on
// a config or fetch failure or a backend with no catalog, 2 on cancellation.
func RunListRunbooks(ctx context.Context, opts Options) int {
	return runListRunbooks(ctx, opts, os.Stdout, os.Stderr)
}

// runListRunbooks is RunListRunbooks with the streams injected, so the exact bytes of every
// output shape are testable without a subprocess.
func runListRunbooks(ctx context.Context, opts Options, stdout, stderr io.Writer) int {
	// fail routes every failure to the active output contract and picks the exit code.
	//
	// Cancellation is decided from the PARENT context, never the child: the internal
	// listRunbooksTimeout below expires only cctx, so a listing that outran its own bound
	// is an error (1), while a SIGINT or a caller-owned deadline is a cancellation (2).
	// Any non-nil parent error counts — a caller's DeadlineExceeded is no less a
	// "you stopped us" than Canceled is.
	//
	// It still writes a document in JSON mode, including when cancelled. "Exactly one
	// JSON document on stdout" has to hold on every path or it is not a contract a
	// script can rely on, and an interrupted run that emitted nothing is the case a
	// parser handles worst.
	fail := func(code string, err error) int {
		cancelled := ctx.Err() != nil
		if cancelled {
			code = "cancelled"
		}
		switch {
		case opts.JSON:
			// A document even when cancelled: "exactly one JSON document on stdout" has to
			// hold on every path or it is not a contract a script can rely on, and an
			// interrupted run that emitted nothing is the case a parser handles worst.
			var doc runbookCatalogErrorJSON
			doc.Error.Code = code
			doc.Error.Message = err.Error()
			_ = writeIndentedJSON(stdout, doc)
		case cancelled:
			// SILENT for a human. They pressed Ctrl-C; a red "✗ context canceled" tells
			// them what they already know while claiming the listing failed, which it did
			// not — it was stopped.
		default:
			render.New(stderr).Error(err.Error())
		}
		if cancelled {
			return domain.OneShotExitCode.Cancelled
		}
		return domain.OneShotExitCode.Error
	}

	cfg, err := loadProbeConfigFromOptions(opts)
	if err != nil {
		return fail("config_failed", err)
	}

	cctx, cancel := context.WithTimeout(ctx, listRunbooksTimeout)
	defer cancel()
	caps, err := app.NewProbeBackendClient(cfg).Capabilities(cctx)
	if err != nil {
		return fail("capabilities_unavailable",
			fmt.Errorf("could not read the backend's capabilities from %s: %w", cfg.BackendURL, err))
	}
	// nil and empty are DIFFERENT answers and the distinction is the whole reason the
	// contract keeps them apart: nil means the deployment predates the catalog field and
	// cannot answer, empty means it answered "none".
	if caps.Runbooks.Catalog == nil {
		return fail("runbook_catalog_not_advertised", errCatalogNotAdvertised)
	}

	// Sorted defensively. The backend documents sorted-by-id, but this is a listing a
	// human scans and a script may diff — it must not depend on a server-side promise.
	//
	// Copied into a NON-NIL slice, which matters only in the empty case and matters a
	// lot there: `append([]backend.RunbookRef(nil), empty...)` stays nil and marshals to
	// `"runbooks": null`, undoing the nil-versus-empty distinction three lines above and
	// breaking `jq '.runbooks[]'` on exactly the backend that answered honestly.
	refs := make([]backend.RunbookRef, len(caps.Runbooks.Catalog))
	copy(refs, caps.Runbooks.Catalog)
	sort.Slice(refs, func(i, j int) bool { return refs[i].ID < refs[j].ID })

	if opts.JSON {
		if err := writeIndentedJSON(stdout, RunbookCatalogJSON{
			CatalogRevision: caps.Runbooks.CatalogRevision,
			Runbooks:          refs,
		}); err != nil {
			return fail("write_failed", err)
		}
		return domain.OneShotExitCode.Success
	}
	writeRunbookCatalogText(stdout, refs)
	return domain.OneShotExitCode.Success
}

// writeRunbookCatalogText renders the human listing: two aligned columns, ids first
// because the id is the thing `--runbook` takes and the title is only the reminder of
// what it is.
func writeRunbookCatalogText(w io.Writer, refs []backend.RunbookRef) {
	if len(refs) == 0 {
		fmt.Fprintln(w, "This backend advertises no runbooks.")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tTITLE")
	for _, ref := range refs {
		fmt.Fprintf(tw, "%s\t%s\n", ref.ID, ref.Title)
	}
	_ = tw.Flush()
	fmt.Fprintf(w, "\nPass one to --runbook to load it on every turn (repeat the flag for more than one).\n")
}

func writeIndentedJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
