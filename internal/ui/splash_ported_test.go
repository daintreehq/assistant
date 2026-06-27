package ui

import (
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/ui/theme"
)

// splash_ported_test.go exercises the boot splash: it draws
// in (more ink over frames), sizes naturally with top breathing room + horizontal
// centering, and — crucially — a too-narrow terminal SKIPS the mark but still fires
// its done timer so boot never hangs. The composer-never-gated contract already lives
// in liveness_test (Splash_DoesNotGateComposer).

func splashTheme() theme.Theme { return darkTheme() }

func TestSplash_FrameDataShape(t *testing.T) {
	// The source masks have a fixed shape: exactly 20 frames, each 18 lines of 48
	// columns, using only U+2588 (█) and spaces. The rendered splash timeline has more
	// frames than this; these masks remain the historical coarse art reference.
	if len(splashFrames) != splashSourceFrames {
		t.Fatalf("splashFrames length = %d, want %d", len(splashFrames), splashSourceFrames)
	}
	for fi, frame := range splashFrames {
		lines := strings.Split(frame, "\n")
		if len(lines) != SplashHeight {
			t.Fatalf("frame %d: %d lines, want %d", fi, len(lines), SplashHeight)
		}
		for li, line := range lines {
			if w := len([]rune(line)); w != SplashWidth {
				t.Fatalf("frame %d line %d: %d columns, want %d", fi, li, w, SplashWidth)
			}
			for _, r := range line {
				if r != '█' && r != ' ' {
					t.Fatalf("frame %d line %d: unexpected rune %q", fi, li, r)
				}
			}
		}
	}
}

func TestSplash_RenderTimelineIsHigherFrameRateThanSourceMasks(t *testing.T) {
	if SplashFrames <= splashSourceFrames {
		t.Fatalf("SplashFrames = %d, want more than source masks %d", SplashFrames, splashSourceFrames)
	}
}

func TestSplash_FrameLinesUseXtermCorrectedVisibleHeight(t *testing.T) {
	lines := splashFrameLines(SplashFrames - 1)
	if len(lines) != SplashHeight {
		t.Fatalf("rendered frame has %d lines, want %d", len(lines), SplashHeight)
	}
	minX, minY, maxX, maxY, ok := splashInkBounds(lines)
	if !ok {
		t.Fatal("final rendered splash frame must carry ink")
	}
	if got := maxY - minY + 1; got != splashVisibleHeight {
		t.Fatalf("final rendered splash visible height = %d, want %d", got, splashVisibleHeight)
	}
	if minY != 0 {
		t.Fatalf("final rendered splash should start at row 0, got row %d", minY)
	}
	if got := maxX - minX + 1; got != 44 {
		t.Fatalf("final rendered splash visible width = %d, want 44", got)
	}
	for i := splashVisibleHeight; i < SplashHeight; i++ {
		if strings.TrimSpace(lines[i]) != "" {
			t.Fatalf("rendered splash row %d should be bottom padding, got %q", i, lines[i])
		}
	}
}

func TestSplash_FrameLinesUseSubcellGlyphs(t *testing.T) {
	lines := splashFrameLines(SplashFrames - 1)
	hasSubcell := false
	for _, line := range lines {
		for _, r := range line {
			if r != ' ' && r != '█' {
				hasSubcell = true
			}
		}
	}
	if !hasSubcell {
		t.Fatal("final rendered splash should use subcell block glyphs for curved edges")
	}
}

func TestSplash_FrameLinesDoNotBleedIntoCanopyBeforeCanopyStarts(t *testing.T) {
	lines := splashFrameLines(splashCanopyStartFrame - 1)
	parts := splashFinalPartLines()
	for y, line := range lines {
		lineRunes := []rune(line)
		partRunes := []rune(parts[y])
		for x, r := range lineRunes {
			if partRunes[x] == 'C' && splashIsInk(r) {
				t.Fatalf("canopy pixel revealed before canopy phase at row %d col %d: %q", y, x, line)
			}
		}
	}
}

func TestSplash_FrameLinesStaggerTrunksBeforeCanopy(t *testing.T) {
	parts := splashFinalPartLines()

	beforeSide := splashFrameLines(splashLeftBranchStartFrame - 1)
	if !splashPartHasInk(beforeSide, parts, 'T') {
		t.Fatal("center trunk should start before the side trunks")
	}
	for _, part := range []rune{'L', 'R', 'C'} {
		if splashPartHasInk(beforeSide, parts, part) {
			t.Fatalf("part %c appeared before the side-trunk phase", part)
		}
	}

	overlap := splashFrameLines(splashRightBranchStartFrame)
	if !splashPartHasInk(overlap, parts, 'L') || !splashPartHasInk(overlap, parts, 'R') {
		t.Fatal("side trunks should begin while the center trunk is still drawing")
	}
	if splashPartRowHasInk(overlap, parts, 'T', 3) {
		t.Fatal("center trunk should not be complete when side trunks begin")
	}
	if splashPartHasInk(overlap, parts, 'C') {
		t.Fatal("canopy should not appear during the trunk overlap")
	}

	canopyStart := splashFrameLines(splashCanopyStartFrame)
	if !splashPartHasInk(canopyStart, parts, 'C') {
		t.Fatal("canopy should begin after the overlapping trunk reveal")
	}
}

func TestSplash_FrameLinesKeepCanopyApexLate(t *testing.T) {
	for frame := splashCanopyStartFrame; frame < splashCanopyEndFrame-1; frame++ {
		line := []rune(splashFrameLines(frame)[0])
		for x := 22; x <= 25; x++ {
			if splashIsInk(line[x]) {
				t.Fatalf("canopy apex leaked at frame %d col %d: %q", frame, x, string(line))
			}
		}
	}
}

func TestSplash_FrameLinesHoldCompletedLogo(t *testing.T) {
	want := strings.Join(splashFrameLines(splashCanopyEndFrame), "\n")
	for frame := splashCanopyEndFrame + 1; frame < SplashFrames; frame++ {
		if got := strings.Join(splashFrameLines(frame), "\n"); got != want {
			t.Fatalf("frame %d changed after completed reveal", frame)
		}
	}
}

func TestSplash_FrameLinesRevealBranchSlicesAtFinalWidth(t *testing.T) {
	finalLines := splashFinalFrameLines()
	parts := splashFinalPartLines()
	for frame := 0; frame < SplashFrames; frame++ {
		lines := splashFrameLines(frame)
		for y, line := range lines {
			lineRunes := []rune(line)
			finalRunes := []rune(finalLines[y])
			partRunes := []rune(parts[y])
			for _, part := range []rune{'L', 'R', 'T'} {
				seen := false
				for x, p := range partRunes {
					if p == part && splashIsInk(lineRunes[x]) {
						seen = true
						break
					}
				}
				if !seen {
					continue
				}
				for x, p := range partRunes {
					if p == part && splashIsInk(finalRunes[x]) && lineRunes[x] != finalRunes[x] {
						t.Fatalf("frame %d row %d part %c widened late at col %d: got %q want %q",
							frame, y, part, x, lineRunes[x], finalRunes[x])
					}
				}
			}
		}
	}
}

func TestSplash_FinalBranchStemsMatchTrunkWidth(t *testing.T) {
	lines := splashFinalFrameLines()
	parts := splashFinalPartLines()
	for _, row := range []int{9, 10, 11, 12} {
		trunk := splashPartHorizontalCoverage(lines[row], parts[row], 'T')
		left := splashPartHorizontalCoverage(lines[row], parts[row], 'L')
		right := splashPartHorizontalCoverage(lines[row], parts[row], 'R')
		if trunk != 4 || left != trunk || right != trunk {
			t.Fatalf("row %d stem widths: left %.1f trunk %.1f right %.1f, want all 4.0", row, left, trunk, right)
		}
	}
}

func TestSplash_FinalBranchBottomEdgesAreSolid(t *testing.T) {
	line := []rune(splashFinalFrameLines()[12])
	parts := []rune(splashFinalPartLines()[12])
	for x, part := range parts {
		if part != 'L' && part != 'R' {
			continue
		}
		switch line[x] {
		case '▀', '▝', '▘', '▗', '▖':
			t.Fatalf("branch bottom col %d uses anti-aliased cap glyph %q", x, line[x])
		}
	}
}

func TestSplash_FinalCanopyStraightEdgesMatchTrunkWidth(t *testing.T) {
	lines := splashFinalFrameLines()
	parts := splashFinalPartLines()
	stemWidth := splashPartHorizontalCoverage(lines[12], parts[12], 'T')
	for _, row := range []int{7, 8, 9} {
		line := []rune(lines[row])
		left := splashHorizontalCoverage(line, 3, 7)
		right := splashHorizontalCoverage(line, 43, 47)
		if left != stemWidth || right != stemWidth {
			t.Fatalf("row %d canopy edge widths: left %.1f right %.1f, want %.1f", row, left, right, stemWidth)
		}
		for _, x := range []int{3, 4, 5, 6, 43, 44, 45, 46} {
			if line[x] != '█' {
				t.Fatalf("row %d canopy edge col %d = %q, want full block", row, x, line[x])
			}
		}
	}
	line := []rune(lines[9])
	for _, x := range []int{7, 42} {
		if line[x] != ' ' {
			t.Fatalf("canopy base col %d = %q, want the single rounding pixel omitted", x, line[x])
		}
	}
}

func TestSplash_FrameLinesKeepFirstFrameVisible(t *testing.T) {
	lines := splashFrameLines(0)
	_, minY, _, maxY, ok := splashInkBounds(lines)
	if !ok {
		t.Fatal("first rendered splash frame must still carry ink")
	}
	if maxY != splashVisibleHeight-1 || minY < splashVisibleHeight-2 {
		t.Fatalf("first frame should stay anchored at corrected trunk base row %d, got rows %d..%d",
			splashVisibleHeight-1, minY, maxY)
	}
}

func TestSplash_FramesAreNonEmptyAndDrawIn(t *testing.T) {
	th := splashTheme()
	// First frame (frame 0) reveals fewer rows than the final frame.
	first := splashModel{frame: 0}
	last := splashModel{frame: SplashFrames - 1}
	ink := func(s string) int {
		return len(strings.ReplaceAll(strings.ReplaceAll(stripAnsi(s), " ", ""), "\n", ""))
	}
	firstInk := ink(first.view(th, 80, 40))
	lastInk := ink(last.view(th, 80, 40))
	if lastInk <= firstInk {
		t.Errorf("splash must draw in: last frame ink %d should exceed first %d", lastInk, firstInk)
	}
	if lastInk == 0 {
		t.Error("the final splash frame must carry ink")
	}
}

func TestSplash_TopBreathingRoomAndNaturalSize(t *testing.T) {
	th := splashTheme()
	s := splashModel{frame: SplashFrames - 1}
	lines := strings.Split(s.view(th, 60, 40), "\n")
	if strings.TrimSpace(lines[0]) != "" {
		t.Errorf("first splash line must be blank (top breathing room): %q", lines[0])
	}
	// The mark is its natural height — not inflated to a 24-row viewport.
	nonBlank := 0
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			nonBlank++
		}
	}
	if nonBlank > SplashHeight+2 {
		t.Errorf("splash should be natural height, drew %d ink rows", nonBlank)
	}
}

func TestSplash_CentersHorizontally(t *testing.T) {
	th := splashTheme()
	s := splashModel{frame: SplashFrames - 1}
	minPad := func(columns int) int {
		min := 1 << 30
		for _, l := range strings.Split(stripAnsi(s.view(th, columns, 40)), "\n") {
			if strings.TrimSpace(l) == "" {
				continue
			}
			pad := len(l) - len(strings.TrimLeft(l, " "))
			if pad < min {
				min = pad
			}
		}
		return min
	}
	// Centering widens the inset with the terminal; left-alignment would hold it fixed.
	if !(minPad(80) > minPad(60)) {
		t.Errorf("splash inset must grow with width (centered): pad(80)=%d pad(60)=%d", minPad(80), minPad(60))
	}
}

func TestSplash_NarrowSkipsButStillCompletes(t *testing.T) {
	// A terminal too narrow to hold the mark renders nothing — but the overlay must
	// still mark itself tooSmall so the boot handoff fires its done path (never hangs).
	s := newSplash(SplashWidth) // columns <= SplashWidth → too small
	if !s.tooSmall {
		t.Fatal("a too-narrow splash must flag tooSmall")
	}
	if got := s.view(splashTheme(), SplashWidth, 40); strings.TrimSpace(got) != "" {
		t.Errorf("too-narrow splash must render nothing, got %q", got)
	}
	// advance() returns no further tick (done immediately) for a too-small splash.
	if cmd := s.advance(); cmd != nil {
		t.Error("too-small splash advance() must not schedule more frames (unblocks boot)")
	}
}

func TestSplash_AdvanceTicksThenLingers(t *testing.T) {
	// A normal splash steps frames until the last, then holds (linger), then done.
	s := splashModel{}
	for i := 0; i < SplashFrames-1; i++ {
		if cmd := s.advance(); cmd == nil {
			t.Fatalf("frame %d advance should keep ticking", i)
		}
	}
	if s.frame != SplashFrames-1 {
		t.Fatalf("frame = %d, want last frame %d", s.frame, SplashFrames-1)
	}
	// At the last frame advance() returns the linger cmd (non-nil), not nil.
	if cmd := s.advance(); cmd == nil {
		t.Error("last frame advance() must return the linger hold cmd")
	}
}

func TestBootHandoffFramePrepaintsCockpitAndParksAtFooter(t *testing.T) {
	m := testModel(72)
	m.rows = 30
	m.syncComposer()

	header := m.headerBlock().Rendered
	frame := m.bootHandoffFrame()
	plain := stripAnsi(frame)

	if !strings.Contains(frame, "\x1b[?2026h") || !strings.Contains(frame, "\x1b[?2026l") {
		t.Fatal("boot handoff frame must use synchronized output")
	}
	if !strings.Contains(frame, "\x1b[2J\x1b[3J\x1b[H") {
		t.Fatal("boot handoff frame must clear screen and scrollback before painting")
	}
	if !strings.Contains(plain, "Daintree Assistant") {
		t.Fatalf("boot handoff frame missing masthead: %q", plain)
	}
	if !strings.Contains(plain, "Ask Daintree") {
		t.Fatalf("boot handoff frame missing initial composer footer: %q", plain)
	}
	lines := strings.Split(strings.ReplaceAll(plain, "\r\n", "\n"), "\n")
	headerLines := strings.Split(strings.TrimSuffix(stripAnsi(header), "\n"), "\n")
	if len(lines) <= len(headerLines) || strings.TrimSpace(lines[len(headerLines)]) != "" {
		t.Fatalf("boot handoff frame must reserve a blank line below the masthead, got %q", plain)
	}

	cursorAtFooter := "\x1b[" + itoa(lineCount(header)+1) + ";1H"
	if !strings.Contains(frame, cursorAtFooter) {
		t.Fatalf("boot handoff frame must park cursor at footer origin %q, frame %q", cursorAtFooter, frame)
	}
}

func TestBootHandoffFrameEmpty(t *testing.T) {
	if got := renderBootHandoffFrame("", ""); got != "" {
		t.Fatalf("empty boot handoff frame = %q, want empty", got)
	}
}

func splashInkBounds(lines []string) (minX, minY, maxX, maxY int, ok bool) {
	minX, minY = 1<<30, 1<<30
	for y, line := range lines {
		for x, r := range []rune(line) {
			if !splashIsInk(r) {
				continue
			}
			if x < minX {
				minX = x
			}
			if x > maxX {
				maxX = x
			}
			if y < minY {
				minY = y
			}
			if y > maxY {
				maxY = y
			}
			ok = true
		}
	}
	return minX, minY, maxX, maxY, ok
}

func splashPartHasInk(lines, parts []string, part rune) bool {
	for row := range lines {
		if splashPartRowHasInk(lines, parts, part, row) {
			return true
		}
	}
	return false
}

func splashPartRowHasInk(lines, parts []string, part rune, row int) bool {
	lineRunes := []rune(lines[row])
	for x, p := range []rune(parts[row]) {
		if p == part && splashIsInk(lineRunes[x]) {
			return true
		}
	}
	return false
}

func splashPartHorizontalCoverage(line, parts string, part rune) float64 {
	lineRunes := []rune(line)
	var width float64
	for x, p := range []rune(parts) {
		if p == part {
			width += splashGlyphHorizontalCoverage(lineRunes[x])
		}
	}
	return width
}

func splashHorizontalCoverage(line []rune, start, end int) float64 {
	var width float64
	for x := start; x < end; x++ {
		width += splashGlyphHorizontalCoverage(line[x])
	}
	return width
}

func splashGlyphHorizontalCoverage(r rune) float64 {
	switch r {
	case '█', '▀', '▄':
		return 1
	case '▌', '▐', '▘', '▝', '▖', '▗':
		return 0.5
	case '▛', '▜', '▙', '▟':
		return 1
	default:
		return 0
	}
}
