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

// skills.go is the `--list-skills` route: what runbooks can this backend load?
//
// Until now the only way to answer that was to read the backend's source, which makes
// `--skill` unusable — you cannot name a thing you cannot enumerate, and a mistyped id
// produces a run that looks fine. This is deliberately the LIGHTEST route in the binary:
// one capability GET. No owner lease, no database, no MCP, no App, no turn. Listing what
// a backend can load is a question about the backend, and it must be answerable while
// another assistant owns the project.

// listSkillsTimeout bounds the single capability read. A listing that hangs is worse
// than one that fails: the caller is usually a human mid-sentence, or a script about to
// choose an id.
const listSkillsTimeout = 15 * time.Second

// SkillCatalogJSON is the `--list-skills --json` document.
//
// It is ONE indented JSON object on stdout, deliberately NOT the one-shot JSONL event
// stream: there is no run here to narrate, and a consumer of a listing wants a value it
// can pipe into `jq`, not a sequence of events it must reassemble.
type SkillCatalogJSON struct {
	// CatalogRevision is what a caching caller keys this list on. Conservative, not
	// exact: the backend's revision hashes each skill's body too, so it moves on an
	// edit that leaves these ids and titles byte-identical.
	CatalogRevision string             `json:"catalogRevision"`
	Skills          []backend.SkillRef `json:"skills"`
}

// skillCatalogErrorJSON is what a failure looks like under --json. A caller parsing
// stdout must never receive prose on the one path it cannot handle — the same rule
// `doctor --json` follows.
type skillCatalogErrorJSON struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// errCatalogNotAdvertised separates "this backend serves no catalog" from "the fetch
// failed". They need different next actions — upgrade the backend versus check the
// endpoint — and collapsing them sends the reader to fix the wrong thing.
var errCatalogNotAdvertised = errors.New(
	"this backend does not advertise a skill catalog, so there is nothing to list; upgrade the backend to use --list-skills")

// RunListSkills is the `--list-skills` entry point.
//
// Exit codes follow the one-shot contract: 0 on a catalog read (INCLUDING an advertised
// empty one — "this backend loads nothing" is a successful answer to the question), 1 on
// a config or fetch failure or a backend with no catalog, 2 on cancellation.
func RunListSkills(ctx context.Context, opts Options) int {
	return runListSkills(ctx, opts, os.Stdout, os.Stderr)
}

// runListSkills is RunListSkills with the streams injected, so the exact bytes of every
// output shape are testable without a subprocess.
func runListSkills(ctx context.Context, opts Options, stdout, stderr io.Writer) int {
	fail := func(code string, err error) int {
		if errors.Is(ctx.Err(), context.Canceled) {
			return domain.OneShotExitCode.Cancelled
		}
		if opts.JSON {
			var doc skillCatalogErrorJSON
			doc.Error.Code = code
			doc.Error.Message = err.Error()
			writeIndentedJSON(stdout, doc)
		} else {
			render.New(stderr).Error(err.Error())
		}
		return domain.OneShotExitCode.Error
	}

	cfg, err := loadConfigFromOptions(opts)
	if err != nil {
		return fail("config_failed", err)
	}

	cctx, cancel := context.WithTimeout(ctx, listSkillsTimeout)
	defer cancel()
	caps, err := app.NewProbeBackendClient(cfg).Capabilities(cctx)
	if err != nil {
		return fail("capabilities_unavailable",
			fmt.Errorf("could not read the backend's capabilities from %s: %w", cfg.BackendURL, err))
	}
	// nil and empty are DIFFERENT answers and the distinction is the whole reason the
	// contract keeps them apart: nil means the deployment predates the catalog field and
	// cannot answer, empty means it answered "none".
	if caps.Skills.Catalog == nil {
		return fail("skill_catalog_not_advertised", errCatalogNotAdvertised)
	}

	// Sorted defensively. The backend documents sorted-by-id, but this is a listing a
	// human scans and a script may diff — it must not depend on a server-side promise.
	refs := append([]backend.SkillRef(nil), caps.Skills.Catalog...)
	sort.Slice(refs, func(i, j int) bool { return refs[i].ID < refs[j].ID })

	if opts.JSON {
		if err := writeIndentedJSON(stdout, SkillCatalogJSON{
			CatalogRevision: caps.Skills.CatalogRevision,
			Skills:          refs,
		}); err != nil {
			return fail("write_failed", err)
		}
		return domain.OneShotExitCode.Success
	}
	writeSkillCatalogText(stdout, refs)
	return domain.OneShotExitCode.Success
}

// writeSkillCatalogText renders the human listing: two aligned columns, ids first
// because the id is the thing `--skill` takes and the title is only the reminder of
// what it is.
func writeSkillCatalogText(w io.Writer, refs []backend.SkillRef) {
	if len(refs) == 0 {
		fmt.Fprintln(w, "This backend advertises no skills.")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tTITLE")
	for _, ref := range refs {
		fmt.Fprintf(tw, "%s\t%s\n", ref.ID, ref.Title)
	}
	_ = tw.Flush()
	fmt.Fprintf(w, "\nPass one to --skill to load it on every turn (repeat the flag for more than one).\n")
}

func writeIndentedJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
