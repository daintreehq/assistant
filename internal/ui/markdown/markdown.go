// Package markdown is the cockpit's SYNCHRONOUS markdown renderer. Assistant prose
// is markdown; the cockpit shows it styled (bold, `code`, headings, lists) rather
// than printing raw markers. A render takes (markdown, width, theme, expanded) and
// returns a Rendered{ANSI, Plain}: the styled, width-wrapped ANSI string and a
// plain-text fallback that is never lost if the styling path fails.
//
// HARD CONTRACTS:
//   - SYNCHRONOUS only. A render call NEVER does I/O and NEVER spawns a goroutine
//     (glamour renders synchronously; chroma highlighting is in-process). This is
//     load-bearing: renders happen inside the Bubble Tea Update/View loop.
//   - Width-wrap by terminal CELLS, owned by glamour (WithWordWrap(width)) — never
//     a hard 80-col wrap, never rune/byte counts.
//   - Security: strip pre-existing ANSI from the INPUT before parsing (untrusted
//     model output could inject SGR / OSC-8 links); when color is off, strip the
//     OUTPUT too.
//   - Bounded LRU cache keyed (contentHash, width, theme, expanded).
//   - Plain fallback on unknown lexer / render failure / empty prose.
package markdown

import (
	"hash/fnv"
	"strings"
	"sync"

	"charm.land/glamour/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/daintreehq/daintree-assistant/internal/ui/theme"
)

// defaultCacheSize bounds the LRU. The working set is the handful of blocks
// currently near the live footer plus those being (re)committed on a resize; a
// few hundred entries comfortably covers that without unbounded growth.
const defaultCacheSize = 512

// Rendered is the output of a render: the styled ANSI string and the plain-text
// fallback. Plain is ALWAYS populated (even when ANSI is) so a downstream commit
// can fall back without re-rendering.
type Rendered struct {
	ANSI  string // styled, width-wrapped; equals Plain when color is off
	Plain string // ANSI-stripped fallback, never empty for non-empty input
}

// Renderer renders markdown for one theme. It owns the bounded cache. Construct
// one per theme (cheap) and reuse it; it is safe for concurrent Render calls
// (the cache is mutex-guarded), though the cockpit calls it from one loop.
type Renderer struct {
	theme theme.Theme
	cache *renderCache

	// trMu/trs memoize one glamour TermRenderer PER WIDTH. Building a TermRenderer compiles
	// the whole goldmark + ANSI style pipeline, which dominated a cache-miss render and was
	// repeated on every settled paragraph / resize. Reuse it per width instead (the theme is
	// fixed per Renderer). tr.Render is serialized under trMu — cache misses are rare (the
	// LRU above absorbs hits) and the cockpit renders from a single loop.
	trMu sync.Mutex
	trs  map[int]*glamour.TermRenderer
}

// New builds a Renderer for the given theme with the default cache bound.
func New(th theme.Theme) *Renderer {
	return &Renderer{theme: th, cache: newRenderCache(defaultCacheSize)}
}

// NewWithCacheSize is New with an explicit cache bound (used by tests).
func NewWithCacheSize(th theme.Theme, size int) *Renderer {
	return &Renderer{theme: th, cache: newRenderCache(size)}
}

// Render renders markdown at the given cell width. `expanded` is part of the key
// (the cockpit's ^X expanded mode can change how blocks render) even though the
// body styling itself is currently expansion-agnostic — keeping it in the key
// means a future expanded-only variation can't serve a stale non-expanded hit.
//
// width <= 0 is clamped to 1 so glamour never divides by zero; callers pass the
// already-computed contentWidth.
func (r *Renderer) Render(content string, width int, expanded bool) Rendered {
	if width < 1 {
		width = 1
	}
	// Security: strip any ANSI the model may have injected into its own prose
	// BEFORE hashing/parsing, so an injected SGR/OSC-8 can never reach the output
	// and the cache key is computed over the sanitized text.
	clean := ansi.Strip(content)

	key := cacheKey{
		contentHash: hashContent(clean),
		width:       width,
		theme:       r.theme.Mode.String(),
		expanded:    expanded,
	}
	if hit, ok := r.cache.get(key); ok {
		return hit
	}

	out := r.render(clean, width)
	r.cache.put(key, out)
	return out
}

// render is the cache-miss path: pure, synchronous, no I/O, no goroutines.
func (r *Renderer) render(clean string, width int) Rendered {
	// Empty/whitespace-only prose: a bare glamour render produces nothing, and we
	// never want an empty hole. Return an empty Rendered so the caller's layout
	// decides (the transcript renderer drops empty steps).
	if strings.TrimSpace(clean) == "" {
		return Rendered{ANSI: "", Plain: ""}
	}

	// Rewrite tables to a width-agnostic record list (glamour grids shred narrow).
	src := tablesToRecordLists(clean)

	styled, err := r.glamourRender(src, width)
	if err != nil || strings.TrimSpace(ansi.Strip(styled)) == "" {
		// Plain fallback: unknown lexer, render failure, or a render that produced
		// no visible text. Word-wrap the sanitized source by cells so the fallback
		// still respects the cockpit width.
		plain := wrapCells(clean, width)
		return Rendered{ANSI: plain, Plain: plain}
	}

	// Trim trailing blank lines glamour appends (the spec trims trailing blanks).
	styled = strings.TrimRight(styled, "\n")
	plain := ansi.Strip(styled)

	if !r.theme.Mode.Colorize() {
		// Color off: the output must carry no SGR. glamour may still emit reset
		// sequences via attribute-only styles, so strip the output entirely and
		// serve the plain text as the ANSI string too.
		return Rendered{ANSI: plain, Plain: plain}
	}
	return Rendered{ANSI: styled, Plain: plain}
}

// glamourRender invokes glamour v2 with the theme's StyleConfig and cell-based
// word wrap. WithStyles drives the semantic map; WithWordWrap hands wrapping to
// glamour at the live width. TableWrap is disabled (we already converted tables).
func (r *Renderer) glamourRender(src string, width int) (string, error) {
	r.trMu.Lock()
	defer r.trMu.Unlock()
	tr := r.trs[width]
	if tr == nil {
		var err error
		tr, err = glamour.NewTermRenderer(
			glamour.WithStyles(r.theme.GlamourStyles()),
			glamour.WithWordWrap(width),
			glamour.WithTableWrap(false),
		)
		if err != nil {
			return "", err
		}
		if r.trs == nil {
			r.trs = map[int]*glamour.TermRenderer{}
		}
		r.trs[width] = tr
	}
	return tr.Render(src)
}

// hashContent is a fast non-cryptographic content hash for the cache key. FNV-1a
// over the sanitized text — collisions are astronomically unlikely for the block
// sizes we render, and the key also pins width/theme/expanded.
func hashContent(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}

// wrapCells hard-wraps text to `width` terminal cells (not runes/bytes), used by
// the plain fallback. ansi.Wrap word-wraps on spaces, falling back to a hard cut
// for over-long tokens; the input is already ANSI-free here.
func wrapCells(s string, width int) string {
	if width < 1 {
		width = 1
	}
	// Wrap each source line independently so existing newlines (paragraphs, list
	// items) are preserved; ansi.Wrap measures by display cells.
	var b strings.Builder
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(ansi.Wrap(line, width, ""))
	}
	return strings.TrimRight(b.String(), "\n")
}
