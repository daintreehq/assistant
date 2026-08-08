// ignore.go implements the .gitignore / .copytreeignore awareness the fs family's
// recursive walks apply, plus the shared glob matcher fs.find / fs.search use.
//
// Why hand-rolled rather than a library: the semantics we need are CopyTree's, and
// CopyTree's own walker (the `copytree` package's ignoreWalker) layers ignore files
// per directory in a specific order with last-match-wins. Encoding that ordering
// explicitly here is what makes the parity reviewable — and it is the only place
// where the hard security precedence (sensitive-path refusal and skipDirs run BEFORE
// any ignore rule, so a negation can never re-expose a credential store) is visible
// as straight-line code. doublestar supplies only the `**` wildcard evaluation the
// stdlib lacks.
//
// SCOPE: these rules prune DISCOVERY (the fs.list recursion, the fs.search /
// fs.find walk). They never refuse an explicitly-named target — reading a
// gitignored build output or a local config by exact path is a legitimate thing to
// ask for, and the divergence this file exists to fix is about what the assistant
// SEES when it explores, not about what it may name.
package fsx

import (
	"io"
	"os"
	"path"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// ignoreFileNames are read at every directory boundary during descent, in THIS
// order. Order is load-bearing: rules are appended in file order and the LAST
// matching rule wins, so listing .copytreeignore second is what makes CopyTree's
// rules beat .gitignore's on a conflict — the same precedence CopyTree itself
// applies.
var ignoreFileNames = []string{".gitignore", ".copytreeignore"}

// maxIgnoreFileBytes bounds a single ignore file read. A real .gitignore is a few
// KiB; anything past this is pathological (a generated blob, a mislabelled file),
// so we skip it rather than fail the whole tool call — a partial ruleset still
// prunes more accurately than no ruleset, and there is no such thing as a
// gitignore parse error to report (every non-comment line is a valid pattern).
const maxIgnoreFileBytes = 256 * 1024

// readIgnoreFile reads one ignore file through the CONFINED root, and is the only
// way either walker loads one. Two properties matter:
//
//   - It refuses anything that is not a regular file. A symlinked .gitignore would
//     otherwise let a project point the parser outside the project entirely, or at
//     its own .env — and since ignore rules change which paths come back, that is
//     an observable oracle over the linked file's contents, not just a harmless
//     parse. (git declines to follow symlinked ignore files for its own reasons;
//     we land in the same place.) The Lstat and the Open are separate syscalls, so
//     this is not proof against an adversary swapping the file between them — but
//     os.Root still bounds the damage to in-project targets, and an attacker who
//     can write to the project mid-walk has better options than this.
//   - It reads at most maxIgnoreFileBytes+1 bytes. Checking the size AFTER an
//     unbounded ReadFile would mean a multi-gigabyte .gitignore is fully allocated
//     before being "skipped" — the bound has to apply to the read itself. The +1
//     is what lets the caller tell "exactly at the cap" from "over it".
//
// A missing ignore file is the overwhelmingly common case, so absence is not an
// error worth surfacing: every failure path simply yields no rules.
func readIgnoreFile(root *os.Root, rel string) []byte {
	st, err := root.Lstat(rel)
	if err != nil || !st.Mode().IsRegular() {
		return nil
	}
	f, err := root.Open(rel)
	if err != nil {
		return nil
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxIgnoreFileBytes+1))
	if err != nil || len(data) > maxIgnoreFileBytes {
		return nil
	}
	return data
}

// ignoreRule is one parsed pattern line, remembered together with the directory it
// was declared in (base, project-relative slash path, "" for the project root).
// Patterns are matched RELATIVE to that base, which is what makes nested ignore
// files at any depth behave the way git does.
type ignoreRule struct {
	base    string // directory the ignore file lived in, "" for project root
	pattern string // normalized: no leading "!", no trailing "/"
	negate  bool   // "!" prefix — re-includes something an earlier rule excluded
	dirOnly bool   // trailing "/" — applies to directories only
	// anchored records that the pattern carried a "/" at the start or in the
	// middle, which in gitignore semantics pins it to `base` instead of letting it
	// float. A slashless pattern floats and is matched against the BASENAME at any
	// depth below base (`*.log` hides logs everywhere); a slash-bearing one is
	// matched against the whole base-relative path (`build/out` only there).
	anchored bool
}

// parseIgnoreFile turns one ignore file's bytes into rules declared at base.
// Blank lines and comments are dropped; a leading "\" escapes a literal "#" or
// "!" as git specifies.
func parseIgnoreFile(base string, data []byte) []ignoreRule {
	var out []ignoreRule
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimRight(raw, " \r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		r := ignoreRule{base: base}
		if strings.HasPrefix(line, "!") {
			r.negate = true
			line = line[1:]
		} else if strings.HasPrefix(line, `\#`) || strings.HasPrefix(line, `\!`) {
			// `\#foo` / `\!foo` mean the literal character, not a comment/negation.
			// ONLY these two: stripping a leading backslash unconditionally would
			// turn `\*.txt` (git's way of writing the literal name "*.txt") into a
			// live glob that hides every text file.
			line = line[1:]
		}
		if line == "" {
			continue
		}
		if strings.HasSuffix(line, "/") {
			r.dirOnly = true
			line = strings.TrimSuffix(line, "/")
		}
		if line == "" {
			continue
		}
		// A LEADING slash anchors without being part of the pattern; an interior
		// slash anchors and is matched literally. Trailing slashes are already
		// stripped above, so they never make a pattern anchored (matching git).
		if strings.HasPrefix(line, "/") {
			r.anchored = true
			line = strings.TrimPrefix(line, "/")
		} else if strings.Contains(line, "/") {
			r.anchored = true
		}
		if line == "" {
			continue
		}
		r.pattern = line
		out = append(out, r)
	}
	return out
}

// ignoreStack is the accumulated rule list from the project root down to the
// directory currently being walked, ancestors first. Last match wins across the
// whole stack, so a deeper file's rules override a shallower one's.
type ignoreStack []ignoreRule

// descend returns the stack for a child directory, with that directory's own
// ignore-file rules appended. The three-index slice expression forces append to
// COPY rather than share the backing array — without it, two sibling directories
// would write their rules over each other's.
func (s ignoreStack) descend(rules []ignoreRule) ignoreStack {
	if len(rules) == 0 {
		return s
	}
	return append(s[:len(s):len(s)], rules...)
}

// ignored reports whether relSlash (a project-relative, slash-separated path) is
// excluded by the accumulated rules. Every rule is evaluated in order and the LAST
// one that matches decides, so a later "!keep.log" genuinely re-includes a file an
// earlier "*.log" hid.
//
// This answers ONLY the ignore question. Callers apply it after the sensitive-path
// and skipDirs prunes, which are unconditional — a negation here can never put a
// credential store or a hard-skipped tree back on the table.
func (s ignoreStack) ignored(relSlash string, isDir bool) bool {
	result := false
	for _, r := range s {
		if r.dirOnly && !isDir {
			continue
		}
		rel, ok := relativeTo(r.base, relSlash)
		if !ok {
			continue
		}
		var target string
		if r.anchored {
			target = rel
		} else {
			// A floating pattern matches the entry's own name at any depth. We
			// evaluate every entry as we descend, so testing the basename is
			// enough to prune a directory and everything under it.
			target = path.Base(rel)
		}
		if matched, err := doublestar.Match(r.pattern, target); err == nil && matched {
			result = !r.negate
		}
	}
	return result
}

// relativeTo re-expresses relSlash against base, reporting false when the path is
// not under base at all (that rule simply does not apply to it).
func relativeTo(base, relSlash string) (string, bool) {
	if base == "" {
		return relSlash, true
	}
	if relSlash == base {
		return "", false
	}
	if !strings.HasPrefix(relSlash, base+"/") {
		return "", false
	}
	return relSlash[len(base)+1:], true
}

// matchGlob reports whether a project-relative slash path matches pattern, using
// the same floating/anchored split gitignore uses: a pattern with NO "/" is
// matched against the basename at any depth, and one containing "/" against the
// whole relative path.
//
// This is deliberate, not a convenience. Git, ripgrep and fd all behave this way,
// and it is what a caller means by "*.go" — under bare doublestar semantics `*`
// never crosses "/", so "*.go" would silently match only root-level files while
// appearing to work. Callers wanting an explicitly rooted match write "**/*.go" or
// "internal/**/*.go", both of which contain "/" and so match the full path.
func matchGlob(pattern, relSlash string) (bool, error) {
	target := relSlash
	if !strings.Contains(pattern, "/") {
		target = path.Base(relSlash)
	}
	return doublestar.Match(pattern, target)
}

// validGlob reports whether a pattern is well-formed, so a malformed glob is
// rejected at decode (INVALID_ARGS the model can fix) instead of silently matching
// nothing at handle time.
func validGlob(pattern string) error {
	_, err := doublestar.Match(pattern, "probe")
	return err
}
