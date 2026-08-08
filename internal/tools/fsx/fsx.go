// Package fsx is the read-only project-filesystem tool family (fs.list, fs.read,
// fs.search), all risk "read". These tools NEVER write, edit, or delete: they
// list, read, and text-search files under the project root only. Every path is
// resolved with safety.ResolveInsideProject so traversal outside the project is
// impossible, and credential-bearing paths are refused (the read-time secret
// guard) so their contents never leak into the durable audit log / conversation
// history.
package fsx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

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
	codeFSFind      = "FS_FIND"
	codeFSSensitive = "FS_SENSITIVE"
	codeFSBinary    = "FS_BINARY"
)

// skipDirs are directory names skipped by every recursive walk. These survive the
// arrival of .gitignore/.copytreeignore awareness on purpose: they are an
// unconditional RESOURCE guard, not a taste judgement. A repo may carry no ignore
// files at all, or deliberately un-ignore a generated tree, and fs.search still has
// no total-byte walk budget — so a no-match search over an un-ignored node_modules
// would be pathological. An ignore-file negation cannot restore any of these.
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "dist": true, "build": true,
	"coverage": true, ".next": true, ".turbo": true, ".cache": true, "vendor": true,
}

const (
	// defaultMaxBytes caps a single file read / per-file search scan.
	defaultMaxBytes = 200_000
	// searchMaxFileBytes: files bigger than this are skipped entirely by fs.search.
	searchMaxFileBytes = 1_000_000

	// maxLineNumber bounds the 1-based line-window args. Well past any real source
	// file, but small enough that the number itself can't be nonsense.
	maxLineNumber = 1_000_000
	// maxLineWindow caps how many lines one windowed read may return. The point of
	// the window is a cheap peek; a thousand lines is already a generous peek.
	maxLineWindow = 1_000
	// maxLineScanBytes bounds the prefix a line-window read is willing to scan to
	// reach lineStart. A line window is honestly NOT constant-time — line numbers
	// only exist by counting newlines from byte 0 — so we bound that scan and steer
	// past it rather than quietly reading a huge file. byteOffset is the O(1) path.
	maxLineScanBytes = 1_000_000
	// maxByteOffset is the largest byte offset expressible without losing precision
	// once the schema round-trips through JSON's float64 numbers.
	maxByteOffset = int64(9_007_199_254_740_991)

	// fs.list output bounds. depth was already capped at 10, but the RESULT was
	// unbounded, so a deep listing on a large repo could dump tens of thousands of
	// entries into the context window.
	defaultListMaxEntries = 200
	maxListMaxEntries     = 1_000

	// fs.find output bounds.
	defaultFindMaxResults = 200
	maxFindMaxResults     = 1_000

	// maxGlobLength bounds a caller-supplied glob so a pathological pattern can't
	// be handed to the matcher.
	maxGlobLength = 4_096
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
	return []tools.Tool{newListTool(), newReadTool(), newSearchTool(), newFindTool()}
}

// readIgnoreRules loads the ignore files declared in one directory, in
// ignoreFileNames order. base is the directory's project-relative slash path ("" at
// the root), which is both where the rules are anchored and where they are read
// from — BOTH walkers go through the same confined root here, so fs.list and
// fs.search/fs.find can never disagree about whether a path is ignored.
func readIgnoreRules(root *os.Root, base string) []ignoreRule {
	var rules []ignoreRule
	for _, name := range ignoreFileNames {
		rel := name
		if base != "" && base != "." {
			rel = base + "/" + name
		}
		if data := readIgnoreFile(root, rel); len(data) > 0 {
			rules = append(rules, parseIgnoreFile(base, data)...)
		}
	}
	return rules
}

// ancestorIgnoreRules builds the ignore stack that applies INSIDE dirRel by
// walking the root-to-target ancestry and loading each level's ignore files. It is
// what lets fs.list("some/deep/dir") honour rules declared above its target
// without walking anything else.
func ancestorIgnoreRules(root *os.Root, dirRel string) ignoreStack {
	stack := ignoreStack(nil).descend(readIgnoreRules(root, ""))
	if dirRel == "" || dirRel == "." {
		return stack
	}
	base := ""
	for _, seg := range strings.Split(dirRel, "/") {
		if seg == "" || seg == "." {
			continue
		}
		if base == "" {
			base = seg
		} else {
			base = base + "/" + seg
		}
		stack = stack.descend(readIgnoreRules(root, base))
	}
	return stack
}

/* -------------------------------- fs.list -------------------------------- */

type listArgs struct {
	Path       string `json:"path,omitempty"`
	Depth      *int   `json:"depth,omitempty"`
	MaxEntries *int   `json:"maxEntries,omitempty"`
}

// Validate enforces the bounds the schema advertises, so an out-of-range value is
// an INVALID_ARGS the model can fix rather than an unbounded recursive walk (depth)
// or an unbounded result dump (maxEntries).
func (a *listArgs) Validate() error {
	if a.Depth != nil && (*a.Depth < 1 || *a.Depth > 10) {
		return fmt.Errorf("depth must be between 1 and 10")
	}
	if a.MaxEntries != nil && (*a.MaxEntries < 1 || *a.MaxEntries > maxListMaxEntries) {
		return fmt.Errorf("maxEntries must be between 1 and %d", maxListMaxEntries)
	}
	return nil
}

// Bounds are encoded as REAL JSON Schema keywords, not just prose: the backend
// forwards this schema to the model verbatim, so `minimum`/`maximum`/`default`
// actually steer the call instead of merely documenting it after the fact.
var listSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "path": { "type": "string", "minLength": 1, "description": "Directory relative to project root (default root)." },
    "depth": { "type": "integer", "minimum": 1, "maximum": 10, "default": 1, "description": "How many directory levels to descend." },
    "maxEntries": { "type": "integer", "minimum": 1, "maximum": 1000, "default": 200, "description": "Maximum entries returned. Entries are sorted by path, then truncated; the result sets \"capped\": true when more existed." }
  },
  "required": []
}`)

func newListTool() tools.Tool {
	return tools.Tool{
		Name: "fs.list",
		Description: "List directory entries under the project root (read-only), each with its `size` in bytes for files. Sorted by path and capped at maxEntries (default 200, max 1000) — check the `capped` flag and narrow `path`/`depth` if it is set. Entry `name` is relative to the requested `path`. " +
			"Honours .gitignore and .copytreeignore during descent (so the tree matches what CopyTree would bundle) and always skips .git, node_modules, dist, build, coverage, .next, .turbo, .cache, vendor and credential paths. " +
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
	// Size is the file's byte size — the whole point of listing sizes is that
	// include/exclude decisions stop requiring a read of every candidate. A
	// pointer so an empty file reports 0 rather than vanishing under omitempty.
	// Directories carry no size: FileInfo.Size() on a directory is filesystem
	// bookkeeping, not content bytes, and a recursive rollup would mean walking
	// the very subtrees depth/maxEntries exist to avoid.
	Size *int64 `json:"size,omitempty"`
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
	maxEntries := defaultListMaxEntries
	if a.MaxEntries != nil {
		maxEntries = *a.MaxEntries
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

	startRoot := rootRel(rel)
	// Ignore rules declared ABOVE the listing target still bind inside it, so seed
	// the stack from the root-to-target ancestry before descending.
	baseIgnore := ancestorIgnoreRules(root, startRoot)

	var entries []listEntry
	var recurse func(dirRelRoot, dirRel string, level int, ig ignoreStack)
	recurse = func(dirRelRoot, dirRel string, level int, ig ignoreStack) {
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
			childRoot := de.Name()
			if dirRelRoot != "." {
				childRoot = dirRelRoot + "/" + de.Name()
			}
			// Ignore rules run LAST, after the two unconditional prunes above, so a
			// negation in a .gitignore can never re-expose a credential dir or a
			// hard-skipped tree. Paths are matched relative to the LISTING TARGET's
			// position in the project, which is what the ancestry seed encodes.
			ignoreRel := childRel
			if startRoot != "." {
				ignoreRel = startRoot + "/" + childRel
			}
			if ig.ignored(ignoreRel, isDir) {
				continue
			}
			// Second-layer secret guard, matching fs.search/fs.find: the segment
			// check above catches credential DIRS, this catches a sensitive FILE
			// basename (.env, server.key). Without it a `!.env` negation in a
			// project-controlled ignore file could put a secrets file — and now its
			// byte size — back into the listing.
			if safety.IsSensitivePath(ignoreRel) {
				continue
			}
			typ := "file"
			var size *int64
			if isDir {
				typ = "dir"
			} else if info, ierr := de.Info(); ierr == nil {
				n := info.Size()
				size = &n
			}
			entries = append(entries, listEntry{Name: childRel, Type: typ, Size: size})
			if isDir && level+1 < depth {
				recurse(childRoot, childRel, level+1, ig.descend(readIgnoreRules(root, ignoreRel)))
			}
		}
	}
	recurse(startRoot, "", 0, baseIgnore)
	// Sort BEFORE truncating. Truncating during the walk would hand back an
	// arbitrary traversal-order prefix; sorting first makes the cap a deterministic
	// lexical window the caller can reason about (and narrow with path/depth).
	// Entry names are project paths, so byte order is the faithful choice.
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	capped := len(entries) > maxEntries
	if capped {
		entries = entries[:maxEntries]
	}
	if entries == nil {
		entries = []listEntry{}
	}
	noun := "entries"
	if len(entries) == 1 {
		noun = "entry"
	}
	plus := ""
	if capped {
		plus = "+"
	}
	return tools.Ok(fmt.Sprintf("Listed %d%s %s under %s.", len(entries), plus, noun, rel),
		map[string]any{"path": rel, "depth": depth, "maxEntries": maxEntries, "capped": capped, "entries": entries})
}

/* -------------------------------- fs.read -------------------------------- */

type readArgs struct {
	Path     string `json:"path"`
	MaxBytes *int   `json:"maxBytes,omitempty"`
	// ByteOffset is the O(1) mid-file peek: seek straight there, pay only for the
	// window. Mutually exclusive with the line window.
	ByteOffset *int64 `json:"byteOffset,omitempty"`
	// LineStart/LineEnd are the 1-based inclusive line window. Line numbers only
	// exist by counting newlines from byte 0, so this scans a bounded prefix —
	// convenient (it pairs with fs.search's line numbers) but not free.
	LineStart *int `json:"lineStart,omitempty"`
	LineEnd   *int `json:"lineEnd,omitempty"`
}

// Validate enforces every bound the schema advertises. maxBytes especially: a
// negative one would otherwise reach make([]byte, toRead) with a negative length
// and panic. The two window modes are mutually exclusive because they answer the
// same question in different coordinate systems — honouring both at once would
// mean silently picking one.
func (a *readArgs) Validate() error {
	if strings.TrimSpace(a.Path) == "" {
		return fmt.Errorf("path is required")
	}
	if a.MaxBytes != nil && (*a.MaxBytes < 1 || *a.MaxBytes > defaultMaxBytes) {
		return fmt.Errorf("maxBytes must be between 1 and %d", defaultMaxBytes)
	}
	if a.ByteOffset != nil && (*a.ByteOffset < 0 || *a.ByteOffset > maxByteOffset) {
		return fmt.Errorf("byteOffset must be between 0 and %d", maxByteOffset)
	}
	if (a.LineStart == nil) != (a.LineEnd == nil) {
		return fmt.Errorf("lineStart and lineEnd must be provided together")
	}
	if a.LineStart != nil {
		if a.ByteOffset != nil {
			return fmt.Errorf("byteOffset cannot be combined with lineStart/lineEnd — pick one window mode")
		}
		if *a.LineStart < 1 || *a.LineStart > maxLineNumber {
			return fmt.Errorf("lineStart must be between 1 and %d", maxLineNumber)
		}
		if *a.LineEnd < 1 || *a.LineEnd > maxLineNumber {
			return fmt.Errorf("lineEnd must be between 1 and %d", maxLineNumber)
		}
		if *a.LineEnd < *a.LineStart {
			return fmt.Errorf("lineEnd must be greater than or equal to lineStart")
		}
		if *a.LineEnd-*a.LineStart+1 > maxLineWindow {
			return fmt.Errorf("line window must cover at most %d lines", maxLineWindow)
		}
	}
	return nil
}

// Bounds are real JSON Schema keywords (the backend forwards this verbatim to the
// model). Mode exclusivity is expressed in prose rather than allOf/if/then: the
// conditional keywords are weakly honoured by function-calling models, cost tokens
// on every turn, and Validate() is the actual enforcement either way.
var readSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "path": { "type": "string", "minLength": 1, "description": "Path relative to project root." },
    "maxBytes": { "type": "integer", "minimum": 1, "maximum": 200000, "default": 200000, "description": "Max content bytes returned." },
    "byteOffset": { "type": "integer", "minimum": 0, "maximum": 9007199254740991, "description": "Zero-based byte offset to start from — the cheap mid-file peek (seeks straight there). Pass a previous result's \"byteEnd\" to continue. Cannot be combined with lineStart/lineEnd." },
    "lineStart": { "type": "integer", "minimum": 1, "maximum": 1000000, "description": "First line of an inclusive 1-based line window. Requires lineEnd. Pairs with fs.search's line numbers. Cannot be combined with byteOffset." },
    "lineEnd": { "type": "integer", "minimum": 1, "maximum": 1000000, "description": "Last line of the inclusive window; at most 1000 lines past lineStart. Requires lineStart." }
  },
  "required": ["path"]
}`)

func newReadTool() tools.Tool {
	return tools.Tool{
		Name: "fs.read",
		Description: "Read a UTF-8 text file from the project (read-only). Defaults to a head read; for a mid-file peek pass EITHER byteOffset (cheap — seeks straight there) OR lineStart+lineEnd (1-based inclusive, ≤1000 lines, pairs with the line numbers fs.search returns). " +
			"The result carries byteStart/byteEnd/size, so pass the previous byteEnd back as byteOffset to page forward. " +
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
	f, oerr := root.Open(relForRoot)
	if oerr != nil {
		return tools.Fail(codeFSRead, fmt.Sprintf("Could not read %s: %s", a.Path, oerr.Error()))
	}
	defer f.Close()

	// byteStart/byteEnd bracket the window actually returned, in whole-file byte
	// coordinates, so the caller can page forward by feeding byteEnd back in.
	var slice []byte
	var byteStart int64
	var lineWindow bool
	var firstLine, lastLine int

	switch {
	case a.LineStart != nil:
		lineWindow = true
		win, werr := readLineWindow(f, size, *a.LineStart, *a.LineEnd, limit)
		if werr != nil {
			return tools.Fail(codeFSRead, fmt.Sprintf("Could not read %s: %s", a.Path, werr.Error()))
		}
		slice, byteStart, firstLine, lastLine = win.data, win.start, win.firstLine, win.lastLine

	case a.ByteOffset != nil:
		byteStart = *a.ByteOffset
		// An empty file has exactly one valid offset (0). For a non-empty file the
		// end itself is out of bounds — there is nothing there to return, and a
		// silent empty success would read as "this file is empty".
		if (size == 0 && byteStart > 0) || (size > 0 && byteStart >= size) {
			return tools.Fail(codeFSRead,
				fmt.Sprintf("byteOffset %d is at or past the end of %s (%d bytes).", byteStart, a.Path, size))
		}
		if _, serr := f.Seek(byteStart, io.SeekStart); serr != nil {
			return tools.Fail(codeFSRead, fmt.Sprintf("Could not read %s: %s", a.Path, serr.Error()))
		}
		var rerr error
		if slice, rerr = readUpTo(f, minInt64(int64(limit), size-byteStart)); rerr != nil {
			return tools.Fail(codeFSRead, fmt.Sprintf("Could not read %s: %s", a.Path, rerr.Error()))
		}

	default:
		var rerr error
		if slice, rerr = readUpTo(f, minInt64(int64(limit), size)); rerr != nil {
			return tools.Fail(codeFSRead, fmt.Sprintf("Could not read %s: %s", a.Path, rerr.Error()))
		}
	}

	// Sniff the file's own PREFIX, not just the returned window. A window can sit
	// in a run of readable text inside an otherwise binary file (a NUL-prefixed
	// blob with an ASCII stretch at offset 8), and judging only the window would
	// let byteOffset/lineStart walk straight past the guard that a plain head read
	// and fs.search both enforce.
	if binary, berr := fileLooksBinary(root, relForRoot); berr != nil {
		return tools.Fail(codeFSRead, fmt.Sprintf("Could not read %s: %s", a.Path, berr.Error()))
	} else if binary || looksBinary(slice) {
		return tools.Fail(codeFSBinary,
			fmt.Sprintf("Refusing to read %s: it appears to be a binary file.", a.Path),
			tools.Unrecoverable())
	}
	byteEnd := byteStart + int64(len(slice))
	// `truncated` keeps its original meaning — content remains AFTER what we
	// returned — which is what decides whether the caller should page again.
	truncated := byteEnd < size
	suffix := ""
	if truncated {
		suffix = fmt.Sprintf(", truncated from %d", size)
	}
	where := ""
	if lineWindow {
		where = fmt.Sprintf(" lines %d-%d of", firstLine, lastLine)
	} else if byteStart > 0 {
		where = fmt.Sprintf(" bytes %d-%d of", byteStart, byteEnd)
	}
	result := map[string]any{
		"path": a.Path, "content": string(slice), "bytes": len(slice),
		"size": size, "byteStart": byteStart, "byteEnd": byteEnd, "truncated": truncated,
	}
	if lineWindow {
		result["lineStart"] = firstLine
		result["lineEnd"] = lastLine
	}
	return tools.Ok(fmt.Sprintf("Read%s %s (%d bytes%s).", where, a.Path, len(slice), suffix), result)
}

// readUpTo reads at most n bytes from f, tolerating a short read. io.ReadFull
// reports a partial-then-EOF as ErrUnexpectedEOF; either way the bytes it did read
// are exactly what we want, and n is already bounded by the file size. A GENUINE
// I/O error is returned rather than swallowed — silently yielding an empty slice
// would reach the model as a successful read of an empty file, which is a lie it
// would then act on.
func readUpTo(f io.Reader, n int64) ([]byte, error) {
	if n <= 0 {
		return nil, nil
	}
	buf := make([]byte, n)
	read, err := io.ReadFull(f, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, err
	}
	return buf[:read], nil
}

// fileLooksBinary re-opens the file and sniffs its first 4 KiB. Cheap (one bounded
// read regardless of file size) and independent of whichever window the caller
// asked for, so the binary refusal cannot be dodged by seeking past the giveaway
// bytes.
func fileLooksBinary(root *os.Root, rel string) (bool, error) {
	f, err := root.Open(rel)
	if err != nil {
		return false, err
	}
	defer f.Close()
	head, rerr := readUpTo(f, 4096)
	if rerr != nil {
		return false, rerr
	}
	return looksBinary(head), nil
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// lineWindowResult is the byte slice covering an inclusive 1-based line range,
// together with where it sat in the file.
type lineWindowResult struct {
	data      []byte
	start     int64
	firstLine int
	lastLine  int
}

// readLineWindow returns lines [from,to] (1-based, inclusive) of f.
//
// Line numbers are not addressable — they only exist by counting newlines from
// byte 0 — so this scans forward, bounded by maxLineScanBytes. That bound is the
// honest part of the design: a line window is CONTEXT-cheap (it returns only the
// window) but not I/O-free, and past ~1 MB of prefix we refuse and steer to
// byteOffset rather than quietly reading a huge file.
func readLineWindow(f io.Reader, size int64, from, to, limit int) (lineWindowResult, error) {
	budget := minInt64(maxLineScanBytes, size)
	buf, rerr := readUpTo(f, budget)
	if rerr != nil {
		return lineWindowResult{}, rerr
	}

	// scannedAll distinguishes "reached the real end of the file" from "ran out of
	// scan budget". They look identical at the end of buf, but they are not: an
	// unterminated remainder is a genuine final line only in the first case. In the
	// second it is a line CUT IN HALF by the 1 MB budget, and serving it as though
	// it were complete would hand the model a silently truncated line.
	scannedAll := int64(len(buf)) == size

	// Walk newline to newline, tracking each line's start offset, until we have
	// passed `to` or run out of scanned bytes.
	line := 1
	lineStartOff := 0
	var winStart, winEnd = -1, -1
	last, total := 0, 0
	for i := 0; i <= len(buf); i++ {
		atEnd := i == len(buf)
		if !atEnd && buf[i] != '\n' {
			continue
		}
		// A trailing newline leaves an empty final segment. That is not a line, and
		// counting it would make "line 4 of a 3-line file" quietly return an empty
		// window instead of saying the file ends first.
		if atEnd && lineStartOff == i && line > 1 {
			break
		}
		// Past the budget with bytes still unread: whatever trails is a fragment,
		// not a line. Stop without claiming it.
		if atEnd && !scannedAll {
			break
		}
		total = line
		if line == from {
			winStart = lineStartOff
		}
		if line >= from && line <= to {
			// Include the newline so consecutive windows concatenate cleanly.
			winEnd = i
			if !atEnd {
				winEnd = i + 1
			}
			last = line
		}
		if atEnd {
			break
		}
		line++
		lineStartOff = i + 1
		if line > to {
			break
		}
	}

	if winStart < 0 {
		if !scannedAll {
			return lineWindowResult{}, fmt.Errorf(
				"line %d is beyond the first %d bytes; use byteOffset to seek further into this file", from, len(buf))
		}
		return lineWindowResult{}, fmt.Errorf("file has only %d lines; lineStart %d is past the end", total, from)
	}
	data := buf[winStart:winEnd]
	if len(data) > limit {
		// maxBytes cut the window short, so the line range computed above no longer
		// describes what we are actually returning. Recount, or the result would
		// claim lineEnd=N while the content stops at some earlier line — a lie the
		// model would then act on when deciding where to read next.
		data = data[:limit]
		if present := countLines(data); present > 0 {
			last = from + present - 1
		} else {
			last = from
		}
	}
	return lineWindowResult{data: data, start: int64(winStart), firstLine: from, lastLine: last}, nil
}

// countLines counts the lines present in a window, counting a final unterminated
// remainder as a (partial) line — it IS content the caller received.
func countLines(data []byte) int {
	n := bytes.Count(data, []byte{'\n'})
	if len(data) > 0 && data[len(data)-1] != '\n' {
		n++
	}
	return n
}

/* ------------------------------- fs.search ------------------------------- */

type searchArgs struct {
	Query      string `json:"query"`
	Glob       string `json:"glob,omitempty"`
	MaxResults *int   `json:"maxResults,omitempty"`
}

// Validate enforces `maxResults` (a negative cap would make the `len(matches) >=
// max` guard true immediately) and rejects a malformed glob at DECODE, so the model
// gets an INVALID_ARGS it can fix instead of a successful search that silently
// matched nothing.
func (a *searchArgs) Validate() error {
	// StrictDecoder does not execute the JSON Schema, so `required` and
	// `minLength` are advisory to the model and nothing more. An empty query would
	// otherwise reach strings.Contains(line, "") — which is true for EVERY line —
	// and dump maxResults worth of unrelated content after a full project walk.
	if a.Query == "" {
		return fmt.Errorf("query is required and must not be empty")
	}
	if a.MaxResults != nil && (*a.MaxResults < 1 || *a.MaxResults > 500) {
		return fmt.Errorf("maxResults must be between 1 and 500")
	}
	if a.Glob != "" {
		if utf8.RuneCountInString(a.Glob) > maxGlobLength {
			return fmt.Errorf("glob must be at most %d characters", maxGlobLength)
		}
		if err := validGlob(a.Glob); err != nil {
			return fmt.Errorf("glob is not a valid pattern: %v", err)
		}
	}
	return nil
}

var searchSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "query": { "type": "string", "minLength": 1, "description": "Literal substring to search for in file contents (not a regex, case-sensitive)." },
    "glob": { "type": "string", "minLength": 1, "maxLength": 4096, "description": "Optional filename filter, e.g. \"*.ts\" (any depth) or \"internal/**/*.go\" (path-anchored). A pattern with no \"/\" matches the file NAME at any depth; one containing \"/\" matches the whole project-relative path. A bare \".ts\" matches nothing — write \"*.ts\"." },
    "maxResults": { "type": "integer", "minimum": 1, "maximum": 500, "default": 50, "description": "Maximum number of matches to return." }
  },
  "required": ["query"]
}`)

func newSearchTool() tools.Tool {
	return tools.Tool{
		Name: "fs.search",
		Description: "Substring search across the project's text files (read-only), capped at maxResults (default 50, max 500). Optional filename `glob`: \"*.ts\" matches that name at any depth, \"internal/**/*.go\" anchors to a path. Returns file/line/text per match plus a `capped` flag. LITERAL substring — not a regex, not case-insensitive. " +
			"Locate code here, then peek at it with fs.read passing lineStart AND lineEnd around the returned line number (e.g. line 120 → lineStart:110, lineEnd:140) — lineStart alone is rejected. " +
			"The walk honours .gitignore/.copytreeignore and additionally always skips .git, node_modules, dist, build, coverage, .next, .turbo, .cache, vendor and credential paths. Runs serially, so prefer one good needle to a batch of searches. To find files by NAME rather than content, use fs.find.",
		Risk: domain.RiskRead,
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

// walkFiles is a recursive walk of regular files under root, applying the family's
// exclusions in a fixed precedence: credential dirs are pruned at walk time (so
// their paths never enter memory — the post-hoc isSensitivePath filter in
// handleSearch is a second layer), then the unconditional skipDirs, and only THEN
// the accumulated .gitignore/.copytreeignore rules. That ordering is the security
// invariant: ignore files are project-controlled input, so a negation like
// "!.ssh/" is evaluated far too late to re-expose anything the first two prunes
// already dropped. The symlink guard catches a POSIX symlink named .ssh
// (IsDir()==false), and the walk never follows symlinked directories at all —
// os.ReadDir reports them as non-dir entries, so descent stays inside the root.
func walkFiles(root string) []walkEntry {
	// Ignore files are loaded through a CONFINED root, exactly as fs.list does, so
	// a symlinked .gitignore can neither escape the project nor be aimed at an
	// in-project secret — and so the two walkers can never disagree about what is
	// ignored. Traversal itself stays on os.ReadDir (it never follows symlinked
	// directories: ReadDir reports them as non-dir entries, so descent is already
	// confined) — the white-box security test pins that behaviour.
	confined, cerr := openProjectRoot(root)
	if cerr != nil {
		return nil
	}
	defer confined.Close()

	var out []walkEntry
	var recurse func(dirAbs, dirRel string, ig ignoreStack)
	recurse = func(dirAbs, dirRel string, ig ignoreStack) {
		dirents, err := os.ReadDir(dirAbs)
		if err != nil {
			return
		}
		ig = ig.descend(readIgnoreRules(confined, filepath.ToSlash(dirRel)))
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
			if ig.ignored(filepath.ToSlash(childRel), isDir) {
				continue
			}
			childAbs := filepath.Join(dirAbs, de.Name())
			if isDir {
				recurse(childAbs, childRel, ig)
			} else if de.Type().IsRegular() {
				out = append(out, walkEntry{rel: childRel, abs: childAbs})
			}
		}
	}
	recurse(root, "", nil)
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
	glob := a.Glob

	for _, file := range files {
		if len(matches) >= max {
			break
		}
		// Glob first: a rejected path then costs no stat and no content read.
		if glob != "" {
			ok, merr := matchGlob(glob, filepath.ToSlash(file.rel))
			if merr != nil || !ok {
				continue
			}
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
		map[string]any{"query": a.Query, "glob": glob, "capped": capped, "matches": matches})
}

/* -------------------------------- fs.find -------------------------------- */

type findArgs struct {
	Glob       string `json:"glob"`
	MaxResults *int   `json:"maxResults,omitempty"`
}

// Validate rejects a malformed or unbounded pattern at DECODE. An empty glob would
// match nothing while looking like a working call, so it is an error, not a
// "list everything" shorthand — fs.list already does that.
func (a *findArgs) Validate() error {
	if strings.TrimSpace(a.Glob) == "" {
		return fmt.Errorf("glob is required")
	}
	if utf8.RuneCountInString(a.Glob) > maxGlobLength {
		return fmt.Errorf("glob must be at most %d characters", maxGlobLength)
	}
	if err := validGlob(a.Glob); err != nil {
		return fmt.Errorf("glob is not a valid pattern: %v", err)
	}
	if a.MaxResults != nil && (*a.MaxResults < 1 || *a.MaxResults > maxFindMaxResults) {
		return fmt.Errorf("maxResults must be between 1 and %d", maxFindMaxResults)
	}
	return nil
}

var findSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "glob": { "type": "string", "minLength": 1, "maxLength": 4096, "description": "Filename pattern. A pattern with no \"/\" matches the file NAME at any depth (\"*.ts\", \"Dockerfile\"); one containing \"/\" matches the whole project-relative path (\"internal/**/*.go\"). \"**\" spans directories. A bare \".ts\" matches nothing — write \"*.ts\"." },
    "maxResults": { "type": "integer", "minimum": 1, "maximum": 1000, "default": 200, "description": "Maximum files returned. Results are sorted by path, then truncated; the result sets \"capped\": true when more matched." }
  },
  "required": ["glob"]
}`)

func newFindTool() tools.Tool {
	return tools.Tool{
		Name: "fs.find",
		Description: "Find project files by NAME or path pattern (read-only), each with its byte `size`. Sorted by path, capped at maxResults (default 200, max 1000) — check the `capped` flag. Use \"*.ts\" to match a name at any depth, or \"internal/**/*.go\" to anchor to a path. " +
			"This is the filename counterpart to fs.search, which matches file CONTENTS. " +
			"Applies the project's .gitignore/.copytreeignore rules, so results track what CopyTree would bundle; on top of those it ALWAYS skips .git, node_modules, dist, build, coverage, .next, .turbo, .cache, vendor and credential paths, which CopyTree itself might include.",
		Risk: domain.RiskRead,
		// NOT Parallelizable, for the same reason as fs.search: this is a full
		// recursive project walk that ignores ctx, and a pattern matching nothing
		// still traverses everything. Six concurrent copies would re-walk the tree
		// six times for no overlap win.
		Schema: findSchema,
		Decode: tools.StrictDecoder(func() any { return &findArgs{} }),
		Handle: handleFind,
	}
}

type fileMatch struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

func handleFind(_ context.Context, raw json.RawMessage, tctx *tools.ToolContext) tools.ToolResult {
	var a findArgs
	_ = json.Unmarshal(raw, &a)
	max := defaultFindMaxResults
	if a.MaxResults != nil {
		max = *a.MaxResults
	}

	rootPath, err := safety.ResolveInsideProject(tctx.ProjectPath, ".")
	if err != nil {
		return tools.Fail(codeFSFind, err.Error(), tools.Unrecoverable())
	}
	// Confined root for the size stats: a walked path is re-resolved against the
	// root fd, so a symlink swapped between walk and stat can't point outside.
	confined, rerr := openProjectRoot(tctx.ProjectPath)
	if rerr != nil {
		return tools.Fail(codeFSFind, rerr.Error(), tools.Unrecoverable())
	}
	defer confined.Close()

	files := make([]fileMatch, 0)
	for _, file := range walkFiles(rootPath) {
		rel := filepath.ToSlash(file.rel)
		ok, merr := matchGlob(a.Glob, rel)
		if merr != nil || !ok {
			continue
		}
		// Second-layer secret guard, mirroring fs.search: the walk already pruned
		// credential DIRS, this catches a sensitive FILE basename.
		if safety.IsSensitivePath(rel) {
			continue
		}
		info, serr := confined.Stat(rootRel(rel))
		if serr != nil || !info.Mode().IsRegular() {
			continue
		}
		files = append(files, fileMatch{Path: rel, Size: info.Size()})
	}
	// Sort before truncating so the cap is a deterministic lexical window rather
	// than an arbitrary traversal-order prefix.
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	capped := len(files) > max
	if capped {
		files = files[:max]
	}
	plus := ""
	if capped {
		plus = "+"
	}
	noun := "files"
	if len(files) == 1 {
		noun = "file"
	}
	return tools.Ok(fmt.Sprintf("Found %d%s %s matching %q.", len(files), plus, noun, a.Glob),
		map[string]any{"glob": a.Glob, "maxResults": max, "capped": capped, "files": files})
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
