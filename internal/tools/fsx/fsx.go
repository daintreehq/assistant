// Package fsx is the read-only project-filesystem tool family (fs.list, fs.read,
// fs.search), all risk "read". These tools NEVER write, edit, or delete: they
// list, read, and text-search files under the project root only. Every path is
// resolved with safety.ResolveInsideProject so traversal outside the project is
// impossible, and credential-bearing paths are refused (the read-time secret
// guard) so their contents never leak into the durable audit log / conversation
// history.
package fsx

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/safety"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

// Deps is empty: the fs family reaches only the local filesystem and the project
// root (carried on ToolContext.ProjectPath), so it has no cross-subsystem deps.
// Kept as a struct for symmetry with the other families and future growth.
type Deps struct{}

// fs-family error codes (model-facing). recoverable defaults:
// FS_SENSITIVE / FS_READ / FS_BINARY are recoverable:false (retrying
// can't help), FS_LIST / FS_SEARCH are recoverable (transient walk failures).
const (
	codeFSList      = "FS_LIST"
	codeFSRead      = "FS_READ"
	codeFSSearch    = "FS_SEARCH"
	codeFSSensitive = "FS_SENSITIVE"
	codeFSBinary    = "FS_BINARY"
)

// skipDirs are directory names skipped by every recursive walk.
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "dist": true, "build": true,
	"coverage": true, ".next": true, ".turbo": true, ".cache": true, "vendor": true,
}

const (
	// defaultMaxBytes caps a single file read / per-file search scan.
	defaultMaxBytes = 200_000
	// searchMaxFileBytes: files bigger than this are skipped entirely by fs.search.
	searchMaxFileBytes = 1_000_000
)

// looksBinary is the heuristic binary sniff: a NUL byte in the first chunk, or a
// >30% ratio of non-text control bytes over min(len,4096), means we should not
// treat the buffer as UTF-8 text. Allow tab(9) newline(10) CR(13) and the
// printable range (≥32); count the rest.
func looksBinary(buf []byte) bool {
	n := len(buf)
	if n > 4096 {
		n = 4096
	}
	suspicious := 0
	for i := 0; i < n; i++ {
		b := buf[i]
		if b == 0 {
			return true
		}
		if b < 9 || (b > 13 && b < 32) {
			suspicious++
		}
	}
	return n > 0 && float64(suspicious)/float64(n) > 0.3
}

// rootRel converts a caller's project-relative path into the slash-cleaned form
// os.Root expects. A leading "/" or "." is normalized to "." (the root itself).
// os.Root rejects any ".." escape and any symlink that leaves the root at OPEN
// time, so this is the TOCTOU-safe replacement for resolve-then-open: there is no
// window between the containment check and the open for a symlink to be swapped.
func rootRel(rel string) string {
	cleaned := filepath.ToSlash(filepath.Clean("/" + rel))
	cleaned = strings.TrimPrefix(cleaned, "/")
	if cleaned == "" {
		return "."
	}
	return cleaned
}

// openProjectRoot opens the project directory as a confined os.Root. Every read
// performed through the returned root is guaranteed to stay inside it — a
// project-local symlink cannot point the open outside the root, even if it is
// swapped after a separate Stat (the Stat-then-Open TOCTOU). Caller must Close it.
func openProjectRoot(projectPath string) (*os.Root, error) {
	return os.OpenRoot(projectPath)
}

// readRootDir reads a directory's entries via the confined root (no symlink
// escape on descent). os.Root has no ReadDir, so open + (*os.File).ReadDir.
func readRootDir(root *os.Root, rel string) ([]os.DirEntry, error) {
	f, err := root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.ReadDir(-1)
}

// Tools returns the fs family.
func Tools(_ Deps) []tools.Tool {
	return []tools.Tool{newListTool(), newReadTool(), newSearchTool()}
}

/* -------------------------------- fs.list -------------------------------- */

type listArgs struct {
	Path  string `json:"path,omitempty"`
	Depth *int   `json:"depth,omitempty"`
}

// Validate enforces the Zod bound `depth: int().positive().max(10)` so a negative
// or absurd depth can never drive an unbounded recursive walk.
func (a *listArgs) Validate() error {
	if a.Depth != nil && (*a.Depth < 1 || *a.Depth > 10) {
		return fmt.Errorf("depth must be between 1 and 10")
	}
	return nil
}

var listSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "path": { "type": "string", "description": "Directory relative to project root (default root)." },
    "depth": { "type": "number", "description": "How many directory levels to descend (default 1)." }
  },
  "required": []
}`)

func newListTool() tools.Tool {
	return tools.Tool{
		Name: "fs.list",
		Description: "List directory entries under the project root (read-only). Skips .git, node_modules, and dist. " +
			"PARALLEL: fs.read/fs.list calls batched in ONE reply run concurrently — to survey several directories, emit one fs.list each in one batch.",
		Risk: domain.RiskRead,
		// Independent, bounded filesystem snapshot read with no ordering dependency on its
		// batch siblings, and (unlike a DB read) genuinely concurrent — the single-directory
		// listing is not serialized behind the single-connection store. A batch of these
		// overlaps instead of stacking one syscall round at a time. See terminal.extract.
		// (fs.search is deliberately NOT parallelized — a full recursive project walk is a
		// heavy, ctx-unaware scan; running six at once would redundantly re-walk the tree.)
		Parallelizable: true,
		Schema:         listSchema,
		Decode:         tools.StrictDecoder(func() any { return &listArgs{} }),
		Handle:         handleList,
	}
}

type listEntry struct {
	Name string `json:"name"`
	Type string `json:"type"` // file | dir
}

func handleList(_ context.Context, raw json.RawMessage, tctx *tools.ToolContext) tools.ToolResult {
	var a listArgs
	_ = json.Unmarshal(raw, &a)
	rel := a.Path
	if rel == "" {
		rel = "."
	}
	depth := 1
	if a.Depth != nil {
		depth = *a.Depth
	}

	// Refuse to list a credential store directly, mirroring fs.read. An empty
	// success could be misread as "directory is empty"; a refusal is unambiguous.
	if safety.IsSensitivePath(rel) {
		return tools.Fail(codeFSSensitive,
			fmt.Sprintf("Refusing to list %s: it looks like a sensitive credential directory. Ask the user for only the specific files you need.", rel),
			tools.Unrecoverable())
	}
	abs, err := safety.ResolveInsideProject(tctx.ProjectPath, rel)
	if err != nil {
		// Wrap a traversal escape under FS_LIST: it surfaces under
		// the family's own error code, not the generic "denied".
		return tools.Fail(codeFSList, err.Error(), tools.Unrecoverable())
	}
	// Re-check the symlink-resolved target: a project-local symlink with a benign
	// name (cloud -> .aws) passes the lexical check but still exposes a credential
	// store on descent. Path may not exist yet — the recurse below reports that.
	if real, rerr := filepath.EvalSymlinks(abs); rerr == nil {
		if safety.IsSensitivePath(real) {
			return tools.Fail(codeFSSensitive,
				fmt.Sprintf("Refusing to list %s: it resolves to a sensitive credential directory. Ask the user for only the specific files you need.", rel),
				tools.Unrecoverable())
		}
	}

	// Confined root: every ReadDir is resolved against the root fd, so descent can
	// never escape the project via a swapped/relative symlink (TOCTOU-safe).
	root, rootErr := openProjectRoot(tctx.ProjectPath)
	if rootErr != nil {
		return tools.Fail(codeFSList, rootErr.Error(), tools.Unrecoverable())
	}
	defer root.Close()

	var entries []listEntry
	var recurse func(dirRelRoot, dirRel string, level int)
	recurse = func(dirRelRoot, dirRel string, level int) {
		dirents, derr := readRootDir(root, dirRelRoot)
		if derr != nil {
			return
		}
		for _, de := range dirents {
			isDir := de.IsDir()
			if isDir && skipDirs[de.Name()] {
				continue
			}
			// Omit credential dirs from the listing entirely (not just skip
			// descent): surfacing `.ssh` as an entry still leaks that the store
			// exists. Symlink guard: a symlink named .ssh reports IsDir()==false.
			isSymlink := de.Type()&os.ModeSymlink != 0
			if (isDir || isSymlink) && safety.IsSensitiveSegment(strings.ToLower(de.Name())) {
				continue
			}
			if !isDir && !de.Type().IsRegular() {
				continue
			}
			childRel := de.Name()
			if dirRel != "" {
				childRel = dirRel + "/" + de.Name()
			}
			typ := "file"
			if isDir {
				typ = "dir"
			}
			entries = append(entries, listEntry{Name: childRel, Type: typ})
			if isDir && level+1 < depth {
				childRoot := de.Name()
				if dirRelRoot != "." {
					childRoot = dirRelRoot + "/" + de.Name()
				}
				recurse(childRoot, childRel, level+1)
			}
		}
	}
	recurse(rootRel(rel), "", 0)
	// A plain sort suffices — entry
	// names are project paths, so byte order is the faithful deterministic choice.
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	if entries == nil {
		entries = []listEntry{}
	}
	noun := "entries"
	if len(entries) == 1 {
		noun = "entry"
	}
	return tools.Ok(fmt.Sprintf("Listed %d %s under %s.", len(entries), noun, rel),
		map[string]any{"path": rel, "depth": depth, "entries": entries})
}

/* -------------------------------- fs.read -------------------------------- */

type readArgs struct {
	Path     string `json:"path"`
	MaxBytes *int   `json:"maxBytes,omitempty"`
}

// Validate enforces `maxBytes: int().positive().max(200_000)`. A negative maxBytes
// would otherwise reach make([]byte, toRead) with a negative length and panic; an
// oversized one is rejected rather than silently honored.
func (a *readArgs) Validate() error {
	if a.MaxBytes != nil && (*a.MaxBytes < 1 || *a.MaxBytes > defaultMaxBytes) {
		return fmt.Errorf("maxBytes must be between 1 and %d", defaultMaxBytes)
	}
	return nil
}

var readSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "path": { "type": "string", "description": "Path relative to project root." },
    "maxBytes": { "type": "number", "description": "Max bytes to read." }
  },
  "required": ["path"]
}`)

func newReadTool() tools.Tool {
	return tools.Tool{
		Name: "fs.read",
		Description: "Read a UTF-8 text file from the project (read-only). " +
			"PARALLEL: fs.read/fs.list calls batched in ONE reply run concurrently — to read several files, emit one fs.read each in one batch, not one per turn.",
		Risk: domain.RiskRead,
		// Independent, bounded filesystem snapshot read, genuinely concurrent (not
		// serialized behind the single-connection store like a DB read): a batch of
		// fs.read calls overlaps their disk I/O instead of running one at a time. See
		// terminal.extract.
		Parallelizable: true,
		Schema:         readSchema,
		Decode:         tools.StrictDecoder(func() any { return &readArgs{} }),
		Handle:         handleRead,
	}
}

func handleRead(_ context.Context, raw json.RawMessage, tctx *tools.ToolContext) tools.ToolResult {
	var a readArgs
	_ = json.Unmarshal(raw, &a)

	if safety.IsSensitivePath(a.Path) {
		return tools.Fail(codeFSSensitive,
			fmt.Sprintf("Refusing to read %s: it looks like a secrets file (env file, private key, or credential store). Reading it could persist secrets into the audit log and conversation history. Ask the user to share only the specific values you need.", a.Path),
			tools.Unrecoverable())
	}
	// Defense-in-depth lexical/symlink containment (keeps the FS_READ-on-../ contract
	// the tests assert) BEFORE the confined open. The confined os.Root below is the
	// authoritative TOCTOU-safe guard; this just produces the friendly error code.
	abs, err := safety.ResolveInsideProject(tctx.ProjectPath, a.Path)
	if err != nil {
		// Wrap under FS_READ (a ../
		// traversal returns FS_READ), not the generic "denied".
		return tools.Fail(codeFSRead, err.Error(), tools.Unrecoverable())
	}
	// Re-check the symlink-resolved target: notes.txt -> .env must not slip past.
	if real, rerr := filepath.EvalSymlinks(abs); rerr == nil {
		if safety.IsSensitivePath(real) {
			return tools.Fail(codeFSSensitive,
				fmt.Sprintf("Refusing to read %s: it resolves to a secrets file. Ask the user to share only the specific values you need.", a.Path),
				tools.Unrecoverable())
		}
	}

	limit := defaultMaxBytes
	if a.MaxBytes != nil && *a.MaxBytes < limit {
		limit = *a.MaxBytes
	}

	// Stat + Open through a confined root so resolution is atomic and a symlink
	// swapped between the secret-guard EvalSymlinks above and the open here can NOT
	// escape the project root (os.Root refuses any out-of-root symlink at open).
	root, rerr := openProjectRoot(tctx.ProjectPath)
	if rerr != nil {
		return tools.Fail(codeFSRead, fmt.Sprintf("Could not read %s: %s", a.Path, rerr.Error()))
	}
	defer root.Close()
	relForRoot := rootRel(a.Path)

	info, serr := root.Stat(relForRoot)
	if serr != nil {
		return tools.Fail(codeFSRead, fmt.Sprintf("Could not read %s: %s", a.Path, serr.Error()))
	}
	if !info.Mode().IsRegular() {
		return tools.Fail(codeFSRead, fmt.Sprintf("Not a regular file: %s", a.Path), tools.Unrecoverable())
	}
	size := info.Size()
	toRead := size
	if int64(limit) < toRead {
		toRead = int64(limit)
	}
	f, oerr := root.Open(relForRoot)
	if oerr != nil {
		return tools.Fail(codeFSRead, fmt.Sprintf("Could not read %s: %s", a.Path, oerr.Error()))
	}
	defer f.Close()
	buf := make([]byte, toRead)
	// io.ReadFull tolerates short reads but treats a partial-then-EOF as
	// ErrUnexpectedEOF; both still give us the bytes actually read, which is all
	// we need (toRead is already bounded by the file size).
	n, rerr := io.ReadFull(f, buf)
	if rerr != nil && rerr != io.EOF && rerr != io.ErrUnexpectedEOF {
		return tools.Fail(codeFSRead, fmt.Sprintf("Could not read %s: %s", a.Path, rerr.Error()))
	}
	slice := buf[:n]
	if looksBinary(slice) {
		return tools.Fail(codeFSBinary,
			fmt.Sprintf("Refusing to read %s: it appears to be a binary file.", a.Path),
			tools.Unrecoverable())
	}
	truncated := size > int64(n)
	suffix := ""
	if truncated {
		suffix = fmt.Sprintf(", truncated from %d", size)
	}
	return tools.Ok(fmt.Sprintf("Read %s (%d bytes%s).", a.Path, n, suffix),
		map[string]any{"path": a.Path, "content": string(slice), "bytes": n, "truncated": truncated})
}

/* ------------------------------- fs.search ------------------------------- */

type searchArgs struct {
	Query      string `json:"query"`
	Glob       string `json:"glob,omitempty"`
	MaxResults *int   `json:"maxResults,omitempty"`
}

// Validate enforces `maxResults: int().positive().max(500)` so a negative cap
// (which would make the `len(matches) >= max` guard true immediately, or worse)
// or an unbounded one is rejected.
func (a *searchArgs) Validate() error {
	if a.MaxResults != nil && (*a.MaxResults < 1 || *a.MaxResults > 500) {
		return fmt.Errorf("maxResults must be between 1 and 500")
	}
	return nil
}

var searchSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "query": { "type": "string", "description": "Substring to search for in file contents." },
    "glob": { "type": "string", "description": "Optional filename suffix/extension filter, e.g. \".ts\"." },
    "maxResults": { "type": "number", "description": "Maximum number of matches to return (default 50)." }
  },
  "required": ["query"]
}`)

func newSearchTool() tools.Tool {
	return tools.Tool{
		Name:        "fs.search",
		Description: "Substring search across the project's text files (read-only): a recursive walk skipping .git/node_modules/dist and credential paths, an optional filename-suffix `glob`, capped at maxResults (default 50, max 500). Returns file/line/text per match plus a `capped` flag. LITERAL substring — not a regex, not case-insensitive. Locate code here, then read it with fs.read; it runs serially, so prefer one good needle to a batch of searches.",
		Risk:        domain.RiskRead,
		// NOT Parallelizable — deliberate exclusion. Unlike the bounded point reads
		// fs.read/fs.list, this is a full recursive project walk that reads file contents
		// and ignores ctx (no mid-scan cancellation). Running six concurrently would
		// redundantly re-walk the whole tree six times and spike I/O for no real overlap
		// win; a batch of searches is better served serially (warm OS page cache).
		Schema: searchSchema,
		Decode: tools.StrictDecoder(func() any { return &searchArgs{} }),
		Handle: handleSearch,
	}
}

type searchMatch struct {
	File string `json:"file"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

type walkEntry struct {
	rel string
	abs string
}

// walkFiles is a recursive walk of regular files under root, skipping skipDirs
// and pruning credential dirs at walk time (so their paths never enter memory —
// the post-hoc isSensitivePath filter in handleSearch is a second layer). The
// symlink guard catches a POSIX symlink named .ssh (IsDir()==false).
func walkFiles(root string) []walkEntry {
	var out []walkEntry
	var recurse func(dirAbs, dirRel string)
	recurse = func(dirAbs, dirRel string) {
		dirents, err := os.ReadDir(dirAbs)
		if err != nil {
			return
		}
		for _, de := range dirents {
			isDir := de.IsDir()
			if isDir && skipDirs[de.Name()] {
				continue
			}
			isSymlink := de.Type()&os.ModeSymlink != 0
			if (isDir || isSymlink) && safety.IsSensitiveSegment(strings.ToLower(de.Name())) {
				continue
			}
			childRel := de.Name()
			if dirRel != "" {
				childRel = filepath.Join(dirRel, de.Name())
			}
			childAbs := filepath.Join(dirAbs, de.Name())
			if isDir {
				recurse(childAbs, childRel)
			} else if de.Type().IsRegular() {
				out = append(out, walkEntry{rel: childRel, abs: childAbs})
			}
		}
	}
	recurse(root, "")
	return out
}

func handleSearch(_ context.Context, raw json.RawMessage, tctx *tools.ToolContext) tools.ToolResult {
	var a searchArgs
	_ = json.Unmarshal(raw, &a)
	max := 50
	if a.MaxResults != nil {
		max = *a.MaxResults
	}

	rootPath, err := safety.ResolveInsideProject(tctx.ProjectPath, ".")
	if err != nil {
		// Wrap under FS_SEARCH, not the generic "denied".
		return tools.Fail(codeFSSearch, err.Error(), tools.Unrecoverable())
	}
	// Confined root for the per-file content reads: a walked file's path is
	// re-resolved against the root at open time, so a symlink swapped between the
	// walk and the read cannot point the read outside the project (TOCTOU-safe).
	confined, rerr := openProjectRoot(tctx.ProjectPath)
	if rerr != nil {
		return tools.Fail(codeFSSearch, rerr.Error(), tools.Unrecoverable())
	}
	defer confined.Close()

	files := walkFiles(rootPath)
	matches := make([]searchMatch, 0)
	needle := a.Query
	suffix := a.Glob

	for _, file := range files {
		if len(matches) >= max {
			break
		}
		if suffix != "" && !strings.HasSuffix(file.rel, suffix) {
			continue
		}
		// Never scan secrets, and never load very large or binary files.
		if safety.IsSensitivePath(file.rel) {
			continue
		}
		relForRoot := rootRel(file.rel)
		info, serr := confined.Stat(relForRoot)
		if serr != nil || !info.Mode().IsRegular() || info.Size() > searchMaxFileBytes {
			continue
		}
		data, derr := confined.ReadFile(relForRoot)
		if derr != nil || looksBinary(data) {
			continue
		}
		lines := strings.Split(string(data), "\n")
		for i, line := range lines {
			if strings.Contains(line, needle) {
				matches = append(matches, searchMatch{
					File: file.rel,
					Line: i + 1,
					Text: sliceRunes(strings.TrimSpace(line), 300),
				})
				if len(matches) >= max {
					break
				}
			}
		}
	}
	capped := len(matches) >= max
	plus := ""
	if capped {
		plus = "+"
	}
	noun := "es"
	if len(matches) == 1 {
		noun = ""
	}
	return tools.Ok(fmt.Sprintf("Found %d%s match%s for %q.", len(matches), plus, noun, a.Query),
		map[string]any{"query": a.Query, "glob": suffix, "capped": capped, "matches": matches})
}

// sliceRunes truncates s to at most n runes (rune-count is the faithful
// single-unit choice for what the model sees).
func sliceRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
