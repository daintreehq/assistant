//go:build pty

package e2e

import (
	"strings"
	"sync"

	"github.com/charmbracelet/x/ansi"
)

// vtscreen_test.go is a MINIMAL in-repo VT interpreter — just enough terminal
// emulation to reconstruct what the host shows after the cockpit drives a real
// turn through a PTY. It is deliberately NOT a full emulator (no SGR colours, no
// alt screen, no DEC private modes): it tracks the cursor, the visible grid, and
// the scrollback that LineFeed pushes off the top, which is all the PTY harness
// needs to assert the three render invariants (pty_test.go).
//
// The byte choreography it must follow was captured from the real binary:
//   - tea.Println (a scrollback commit) = CSI nA (cursor up to the top of the live
//     region) + CSI nL (Insert Line, make room) + the committed text.
//   - In-place footer repaint = CSI nA + per-line CSI K (Erase Line) + text. The n
//     of that cursor-up is the live View height — peakCursorUp tracks its max so the
//     footer-height invariant can bound it.
//   - A settled resize wipes everything with CSI 2J (erase all) + CSI 3J (erase
//     scrollback) + CSI H (home) before recommitting fresh at the new width.
//
// Everything is guarded by a mutex because the PTY drain goroutine feeds bytes
// while the test goroutine reads screen state for assertions.

type vtScreen struct {
	mu         sync.Mutex
	rows, cols int
	grid       [][]rune // rows x cols visible cell buffer
	history    []string // plain-text lines scrolled off the top
	row, col   int      // 0-based cursor
	savedR     int
	savedC     int
	peakCUU    int // largest CSI nA distance seen since the last resetPeakCursorUp
	parser     *ansi.Parser
}

// newVTScreen builds a blank rows×cols screen wired to an x/ansi parser whose
// handler callbacks mutate this screen. Feed() drives bytes through it.
func newVTScreen(rows, cols int) *vtScreen {
	s := &vtScreen{rows: rows, cols: cols}
	s.grid = blankGrid(rows, cols)
	p := ansi.NewParser()
	p.SetHandler(ansi.Handler{
		Print:     s.onPrint,
		Execute:   s.onExecute,
		HandleCsi: s.onCSI,
		HandleEsc: s.onESC,
	})
	s.parser = p
	return s
}

func blankGrid(rows, cols int) [][]rune {
	g := make([][]rune, rows)
	for r := range g {
		g[r] = blankRow(cols)
	}
	return g
}

func blankRow(cols int) []rune {
	row := make([]rune, cols)
	for i := range row {
		row[i] = ' '
	}
	return row
}

// Feed advances the parser over a chunk of raw PTY bytes. The handler callbacks
// run synchronously inside Parse, on this goroutine, under the lock.
func (s *vtScreen) Feed(b []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.parser.Parse(b)
}

// --- parser callbacks (all run under s.mu via Feed) ---

func (s *vtScreen) onPrint(r rune) {
	if s.col >= s.cols {
		// DECAWM autowrap: the pending column folds to the next line before placing.
		s.col = 0
		s.lineFeed()
	}
	if s.row >= 0 && s.row < s.rows && s.col >= 0 && s.col < s.cols {
		s.grid[s.row][s.col] = r
	}
	s.col++
}

func (s *vtScreen) onExecute(b byte) {
	switch b {
	case '\n': // LF
		s.lineFeed()
	case '\r': // CR
		s.col = 0
	case '\b': // BS
		if s.col > 0 {
			s.col--
		}
	case '\t': // HT — advance to the next 8-col tab stop
		s.col = ((s.col / 8) + 1) * 8
		if s.col >= s.cols {
			s.col = s.cols - 1
		}
	}
}

// lineFeed moves down one row, scrolling the top line into history when the
// cursor would leave the bottom of the screen (the only path that grows
// scrollback in this model).
func (s *vtScreen) lineFeed() {
	if s.row >= s.rows-1 {
		s.scrollUp(1)
		s.row = s.rows - 1
		return
	}
	s.row++
}

// scrollUp pushes n top lines into history and opens n blank lines at the bottom.
func (s *vtScreen) scrollUp(n int) {
	for i := 0; i < n; i++ {
		s.history = append(s.history, strings.TrimRight(string(s.grid[0]), " "))
		s.grid = append(s.grid[1:], blankRow(s.cols))
	}
}

func (s *vtScreen) onCSI(cmd ansi.Cmd, params ansi.Params) {
	// Ignore prefixed sequences (DEC private modes like CSI ?25l, CSI >4;2m, kitty
	// CSI =1;1u) — none affect the cell/cursor state this model tracks.
	if cmd.Prefix() != 0 {
		return
	}
	p0, _, _ := params.Param(0, 1) // first param, default 1
	switch cmd.Final() {
	case 'A': // CUU — cursor up; its distance is the live-View height being repainted
		if p0 > s.peakCUU {
			s.peakCUU = p0
		}
		s.row = clamp(s.row-p0, 0, s.rows-1)
	case 'B', 'e': // CUD / VPR — cursor down
		s.row = clamp(s.row+p0, 0, s.rows-1)
	case 'C', 'a': // CUF / HPR — cursor forward
		s.col = clamp(s.col+p0, 0, s.cols-1)
	case 'D': // CUB — cursor back
		s.col = clamp(s.col-p0, 0, s.cols-1)
	case 'E': // CNL — next line
		s.row = clamp(s.row+p0, 0, s.rows-1)
		s.col = 0
	case 'F': // CPL — previous line
		s.row = clamp(s.row-p0, 0, s.rows-1)
		s.col = 0
	case 'G', '`': // CHA / HPA — cursor to column
		s.col = clamp(p0-1, 0, s.cols-1)
	case 'd': // VPA — cursor to row
		s.row = clamp(p0-1, 0, s.rows-1)
	case 'H', 'f': // CUP / HVP — cursor position (row;col, 1-based)
		r, _, _ := params.Param(0, 1)
		c, _, _ := params.Param(1, 1)
		s.row = clamp(r-1, 0, s.rows-1)
		s.col = clamp(c-1, 0, s.cols-1)
	case 'J': // ED — erase in display
		s.eraseDisplay(firstParam(params, 0))
	case 'K': // EL — erase in line
		s.eraseLine(firstParam(params, 0))
	case 'L': // IL — insert blank lines at the cursor row (the Println commit path)
		s.insertLines(p0)
	case 'M': // DL — delete lines at the cursor row
		s.deleteLines(p0)
	case 'S': // SU — scroll up
		s.scrollUp(p0)
	case 'T': // SD — scroll down
		s.scrollDown(p0)
	}
}

// onESC handles the few non-CSI escapes the renderer can emit. ESC [ (CSI) and
// strings (OSC/DCS) are routed elsewhere by the parser.
func (s *vtScreen) onESC(cmd ansi.Cmd) {
	switch cmd.Final() {
	case '7': // DECSC — save cursor
		s.savedR, s.savedC = s.row, s.col
	case '8': // DECRC — restore cursor
		s.row, s.col = s.savedR, s.savedC
	case 'D': // IND — index (down, scroll at bottom)
		s.lineFeed()
	case 'M': // RI — reverse index (up, scroll-down at top)
		if s.row <= 0 {
			s.scrollDown(1)
		} else {
			s.row--
		}
	case 'E': // NEL — next line
		s.col = 0
		s.lineFeed()
	}
}

// eraseDisplay: 0=cursor→end, 1=start→cursor, 2=whole screen, 3=scrollback.
func (s *vtScreen) eraseDisplay(mode int) {
	switch mode {
	case 0:
		s.clearLineRange(s.row, s.col, s.cols)
		for r := s.row + 1; r < s.rows; r++ {
			s.grid[r] = blankRow(s.cols)
		}
	case 1:
		s.clearLineRange(s.row, 0, s.col+1)
		for r := 0; r < s.row; r++ {
			s.grid[r] = blankRow(s.cols)
		}
	case 2:
		s.grid = blankGrid(s.rows, s.cols)
	case 3:
		s.history = nil
	}
}

// eraseLine: 0=cursor→eol, 1=bol→cursor, 2=whole line.
func (s *vtScreen) eraseLine(mode int) {
	if s.row < 0 || s.row >= s.rows {
		return
	}
	switch mode {
	case 0:
		s.clearLineRange(s.row, s.col, s.cols)
	case 1:
		s.clearLineRange(s.row, 0, s.col+1)
	case 2:
		s.grid[s.row] = blankRow(s.cols)
	}
}

func (s *vtScreen) clearLineRange(row, from, to int) {
	if row < 0 || row >= s.rows {
		return
	}
	for c := from; c < to && c < s.cols; c++ {
		if c >= 0 {
			s.grid[row][c] = ' '
		}
	}
}

// insertLines shifts rows [cursor..bottom] down by n, opening n blank rows at the
// cursor; rows pushed off the bottom are discarded (standard IL — they are
// transient footer, never scrollback).
func (s *vtScreen) insertLines(n int) {
	if s.row < 0 || s.row >= s.rows || n <= 0 {
		return
	}
	for i := 0; i < n; i++ {
		// drop the last row, open a blank at s.row
		copy(s.grid[s.row+1:], s.grid[s.row:s.rows-1])
		s.grid[s.row] = blankRow(s.cols)
	}
}

// deleteLines shifts rows below the cursor up by n, opening blanks at the bottom.
func (s *vtScreen) deleteLines(n int) {
	if s.row < 0 || s.row >= s.rows || n <= 0 {
		return
	}
	for i := 0; i < n; i++ {
		copy(s.grid[s.row:], s.grid[s.row+1:])
		s.grid[s.rows-1] = blankRow(s.cols)
	}
}

func (s *vtScreen) scrollDown(n int) {
	for i := 0; i < n; i++ {
		s.grid = append([][]rune{blankRow(s.cols)}, s.grid[:s.rows-1]...)
	}
}

// --- read accessors (lock internally; safe to call from the test goroutine) ---

// PlainLines returns the full composed text: scrollback history followed by the
// visible grid, trailing blank rows trimmed.
func (s *vtScreen) PlainLines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	lines := make([]string, 0, len(s.history)+s.rows)
	lines = append(lines, s.history...)
	for _, row := range s.grid {
		lines = append(lines, strings.TrimRight(string(row), " "))
	}
	// trim trailing blank lines
	end := len(lines)
	for end > 0 && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	return lines[:end]
}

// Plain joins PlainLines with newlines.
func (s *vtScreen) Plain() string { return strings.Join(s.PlainLines(), "\n") }

// CountLineSubstr counts how many composed lines contain sub. Counting per-line
// (not raw occurrences) is the robust signal for the no-duplicate-prose invariant:
// a unique marker placed once should land on exactly one composed line.
func (s *vtScreen) CountLineSubstr(sub string) int {
	n := 0
	for _, l := range s.PlainLines() {
		if strings.Contains(l, sub) {
			n++
		}
	}
	return n
}

// PeakCursorUp returns the largest CSI nA distance seen since the last reset — the
// peak live-View height the renderer repainted.
func (s *vtScreen) PeakCursorUp() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.peakCUU
}

// ResetPeakCursorUp zeroes the peak so a later read measures only one phase
// (e.g. the streamed turn, isolated from the taller pre-commit boot footer).
func (s *vtScreen) ResetPeakCursorUp() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.peakCUU = 0
}

func firstParam(p ansi.Params, def int) int {
	v, _, _ := p.Param(0, def)
	return v
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
