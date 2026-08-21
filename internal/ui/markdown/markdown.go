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
//     OUTPUT too. The OSC-8 hyperlinks we then GENERATE are restricted to
//     http/https targets (filterHyperlinkSchemes) — glamour would otherwise make
//     mailto:/file:///javascript: destinations genuinely clickable in the host.
//   - Bounded LRU cache keyed (contentHash, width, expanded); each Renderer owns
//     one immutable theme, so theme is implicit in the cache instance.
//   - Plain fallback on unknown lexer / render failure / empty prose.
package markdown

import (
	"hash/fnv"
	"strings"
	"sync"

	"charm.land/glamour/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/daintreehq/assistant/internal/ui/theme"
)

// defaultCacheSize bounds the LRU. The working set is the handful of blocks
// currently near the live footer plus those being (re)committed on a resize; a
// few hundred entries comfortably covers that without unbounded growth.
const defaultCacheSize = 512

// maxTermRenderers bounds the secondary per-width glamour pipeline cache. The
// rendered-output LRU handles normal resize reuse; retaining every width ever seen
// would otherwise grow for the lifetime of a heavily resized cockpit.
const maxTermRenderers = 16

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
	// trWidths is oldest-created first and capped. The rendered-output LRU above
	// handles normal resize reuse, so this secondary cache only needs a simple bound.
	trWidths []int
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
	// Security: strip anything the model could use to drive the terminal itself
	// BEFORE hashing/parsing, so it can never reach the output and the cache key
	// is computed over the sanitized text.
	clean := sanitizeInput(content)

	key := cacheKey{
		contentHash: hashContent(clean),
		width:       width,
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

	// Tables render natively as aligned grids: glamour width-wraps them
	// (WithTableWrap) so a SIMPLE table fits the narrow cockpit, and the base
	// prompt keeps the model to small tables. (We used to flatten every table to a
	// record list because table-wrap was off and unwrapped grids shredded; with
	// wrap on, a simple scoreboard reads as a real table.)
	styled, err := r.glamourRender(clean, width)
	if err != nil || strings.TrimSpace(ansi.Strip(styled)) == "" {
		// Plain fallback: unknown lexer, render failure, or a render that produced
		// no visible text. Word-wrap the sanitized source by cells so the fallback
		// still respects the cockpit width.
		plain := wrapCells(clean, width)
		return Rendered{ANSI: plain, Plain: plain}
	}

	// Trim trailing blank lines glamour appends (the spec trims trailing blanks).
	styled = strings.TrimRight(styled, "\n")
	// Restrict the hyperlinks glamour generated to http/https BEFORE either
	// representation is derived or cached, so the allowlist holds for every
	// consumer. (The no-color branch below strips all ANSI anyway, but the
	// ordering is what makes the invariant obvious to a reader.)
	styled = filterHyperlinkSchemes(styled)
	// …then drop every control we did NOT generate. glamour un-escapes HTML
	// entities in text nodes after we hand it the source, so "&#27;[2J" arrives
	// here as a live clear-screen no matter how clean the input was.
	styled = sanitizeOutput(styled)
	// stripHyperlinks first: ansi.Strip mis-frames an OSC payload whose bytes
	// include 0x9C (the continuation byte of runes like 'Ü'), which would spill the
	// tail of a legitimate URI — and its BEL — into the plain text.
	plain := ansi.Strip(stripHyperlinks(styled))

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
// glamour at the live width. TableWrap is ON so glamour wraps table cells to the
// live width instead of letting a wide row shred past the right edge — the base
// prompt keeps tables simple enough that a wrapped cell stays readable.
func (r *Renderer) glamourRender(src string, width int) (string, error) {
	r.trMu.Lock()
	defer r.trMu.Unlock()
	tr := r.trs[width]
	if tr == nil {
		var err error
		tr, err = glamour.NewTermRenderer(
			glamour.WithStyles(r.theme.GlamourStyles()),
			glamour.WithWordWrap(width),
			glamour.WithTableWrap(true),
		)
		if err != nil {
			return "", err
		}
		if r.trs == nil {
			r.trs = map[int]*glamour.TermRenderer{}
		}
		if len(r.trWidths) >= maxTermRenderers {
			delete(r.trs, r.trWidths[0])
			r.trWidths = r.trWidths[1:]
		}
		r.trs[width] = tr
		r.trWidths = append(r.trWidths, width)
	}
	return tr.Render(src)
}

// hashContent is a fast non-cryptographic content hash for the cache key. FNV-1a
// over the sanitized text — collisions are astronomically unlikely for the block
// sizes we render.
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
