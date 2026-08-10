package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/assistant/internal/ui/theme"
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

func TestSplash_FrameRowsUseLimitedCoverageLevels(t *testing.T) {
	allowed := map[float64]bool{
		0:    true,
		0.72: true,
		1:    true,
	}
	for frame := 0; frame < SplashFrames; frame++ {
		for row, cells := range splashFrameRows(frame) {
			for col, cell := range cells {
				if !allowed[cell.coverage] {
					t.Fatalf("frame %d row %d col %d coverage = %.3f, want background, one AA level, or solid",
						frame, row, col, cell.coverage)
				}
			}
		}
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
	if splashPartHasInk(overlap, parts, 'C') {
		t.Fatal("canopy should not appear during the trunk overlap")
	}

	canopyStart := splashFrameLines(splashCanopyStartFrame)
	if !splashPartHasInk(canopyStart, parts, 'C') {
		t.Fatal("canopy should begin after the overlapping trunk reveal")
	}
}

func TestSplash_FrameLinesStemRevealStartsPromptly(t *testing.T) {
	parts := splashFinalPartLines()
	lines := splashFrameLines(splashTrunkStartFrame + 1)
	if got := splashPartInkRows(lines, parts, 'T'); got < 2 {
		t.Fatalf("center trunk reveal should show visible movement on the first tick, got %d ink rows", got)
	}
}

func TestSplash_FrameLinesBranchesOverlapCanopyRelay(t *testing.T) {
	parts := splashFinalPartLines()
	beforeCanopy := splashFrameLines(splashCanopyStartFrame - 1)
	if !splashPartHasInk(beforeCanopy, parts, 'L') || !splashPartHasInk(beforeCanopy, parts, 'R') {
		t.Fatal("side branches should already be growing when the canopy relay starts")
	}

	relay := splashFrameLines(splashCanopyStartFrame + 9)
	if !splashPartHasInk(relay, parts, 'C') {
		t.Fatal("canopy should overlap the side-branch reveal")
	}
	for _, part := range []rune{'L', 'R'} {
		if !splashPartRowHasInk(relay, parts, part, 6) {
			t.Fatalf("side branch %c should keep travelling through its top curve during the canopy relay", part)
		}
	}
}

func TestSplash_FrameLinesCanopyApexArrivesBeforeFinalHold(t *testing.T) {
	early := []rune(splashFrameLines(splashCanopyStartFrame + 3)[0])
	for x := 22; x <= 25; x++ {
		if splashIsInk(early[x]) {
			t.Fatalf("canopy apex should not jump in immediately at col %d: %q", x, string(early))
		}
	}

	late := []rune(splashFrameLines(splashCanopyEndFrame - 2)[0])
	seenApex := false
	for x := 22; x <= 25; x++ {
		if splashIsInk(late[x]) {
			seenApex = true
			break
		}
	}
	if !seenApex {
		t.Fatalf("canopy apex should begin arriving before the final hold: %q", string(late))
	}

	complete := []rune(splashFrameLines(splashCanopyEndFrame)[0])
	for x := 22; x <= 25; x++ {
		if !splashIsInk(complete[x]) {
			t.Fatalf("canopy apex should be complete at the final reveal frame col %d: %q", x, string(complete))
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

func TestSplash_FinalStraightStemRunsSnapToFourCells(t *testing.T) {
	lines := splashFinalFrameLines()
	for _, row := range []int{9, 10, 11} {
		line := []rune(lines[row])
		for _, run := range []struct {
			name       string
			start, end int
		}{
			{"left", 15, 19},
			{"center", 23, 27},
			{"right", 31, 35},
		} {
			if got := splashHorizontalCoverage(line, run.start, run.end); got != 4 {
				t.Fatalf("row %d %s trunk width = %.1f, want 4.0", row, run.name, got)
			}
		}
		assertSplashRun(t, lines[row], 15, "████")
		assertSplashRun(t, lines[row], 23, "████")
		assertSplashRun(t, lines[row], 31, "████")
		assertSplashRun(t, lines[row], 14, " ")
		assertSplashRun(t, lines[row], 19, " ")
		assertSplashRun(t, lines[row], 30, " ")
		assertSplashRun(t, lines[row], 35, " ")
	}

	assertSplashRun(t, lines[12], 15, "████")
	assertSplashRun(t, lines[12], 23, "████")
	assertSplashRun(t, lines[12], 31, "████")
	assertSplashRun(t, lines[12], 14, " ")
	assertSplashRun(t, lines[12], 19, " ")
	assertSplashRun(t, lines[12], 30, " ")
	assertSplashRun(t, lines[12], 35, " ")
}

func TestSplash_FinalSilhouetteIsSymmetric(t *testing.T) {
	lines := splashFinalFrameLines()
	for row := 0; row < splashVisibleHeight; row++ {
		line := []rune(lines[row])
		for offset := 0; offset < SplashWidth/2; offset++ {
			left := 24 - offset
			right := 25 + offset
			if left < 0 || right >= len(line) {
				continue
			}
			if splashIsInk(line[left]) != splashIsInk(line[right]) {
				t.Fatalf("row %d silhouette differs at mirrored cols %d/%d: %q", row, left, right, lines[row])
			}
		}
	}
}

func TestSplash_FinalCanopyStraightEdgesMatchTrunkWidth(t *testing.T) {
	lines := splashFinalFrameLines()
	for _, row := range []int{7, 8} {
		line := []rune(lines[row])
		left := splashHorizontalCoverage(line, 3, 7)
		right := splashHorizontalCoverage(line, 43, 47)
		if left != 4 || right != 4 {
			t.Fatalf("row %d canopy edge widths: left %.1f right %.1f, want 4.0", row, left, right)
		}
		assertSplashRun(t, lines[row], 3, "████")
		assertSplashRun(t, lines[row], 7, " ")
		assertSplashRun(t, lines[row], 42, " ")
		assertSplashRun(t, lines[row], 43, "████")
	}

	line := []rune(lines[9])
	if left, right := splashHorizontalCoverage(line, 3, 7), splashHorizontalCoverage(line, 43, 47); left != 4 || right != 4 {
		t.Fatalf("canopy bottom widths: left %.1f right %.1f, want 4.0", left, right)
	}
	assertSplashRun(t, lines[9], 3, "████")
	assertSplashRun(t, lines[9], 7, " ")
	assertSplashRun(t, lines[9], 42, " ")
	assertSplashRun(t, lines[9], 43, "████")
}

func TestSplash_FrameLinesKeepFirstFrameVisible(t *testing.T) {
	lines := splashFrameLines(0)
	_, minY, _, maxY, ok := splashInkBounds(lines)
	if !ok {
		t.Fatal("first rendered splash frame must still carry ink")
	}
	if maxY != splashVisibleHeight-1 || minY < splashVisibleHeight-3 {
		t.Fatalf("first frame should stay anchored near corrected trunk base row %d, got rows %d..%d",
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
	// still mark itself tooSmall so the boot lifecycle takes its done path (never hangs).
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

func TestBootHandoffFramePrepaintsCompleteLoadingCockpit(t *testing.T) {
	m := testModel(72)
	m.rows = 30
	m.masthead.Logging = true
	m.masthead.LogFile = "/tmp/daintree-session.log"
	m.syncComposer()

	header := m.headerBlock().Rendered
	frame := m.bootHandoffFrame()
	plain := stripAnsi(frame)

	if !strings.Contains(frame, "\x1b[?2026h") || !strings.Contains(frame, "\x1b[?2026l") {
		t.Fatal("boot handoff frame must use synchronized output")
	}
	if !strings.Contains(frame, splashViewportReset) {
		t.Fatal("boot handoff frame must clear and home before replacing the final logo frame")
	}
	if strings.Contains(frame, "\x1b[3J") {
		t.Fatal("boot handoff frame must preserve native terminal scrollback")
	}
	for _, want := range []string{"Daintree Assistant", "logging", "Ask Daintree", "MCP"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("boot handoff frame missing %q: %q", want, plain)
		}
	}
	if m.mcpResolved {
		t.Fatal("test precondition: handoff must render before MCP resolution reaches the model")
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

func TestHandoffFrameRows(t *testing.T) {
	if got := handoffFrameRows("one\r\ntwo\r\nthree"); got != 3 {
		t.Fatalf("handoff frame rows = %d, want 3", got)
	}
}

func TestSplashAbortCleanupRestoresCursorWithoutErasingScrollback(t *testing.T) {
	for _, want := range []string{"\x1b[?25h", "\x1b[2J", "\x1b[H"} {
		if !strings.Contains(splashAbortCleanup, want) {
			t.Fatalf("splash cleanup missing %q: %q", want, splashAbortCleanup)
		}
	}
	if strings.Contains(splashAbortCleanup, "\x1b[3J") {
		t.Fatal("splash cleanup must preserve native terminal scrollback")
	}
}

func TestBootSplashDurationNeverWaitsForHostStartup(t *testing.T) {
	wantNormal := time.Duration(SplashFrames)*(time.Second/time.Duration(splashFPS)) +
		time.Duration(lingerMs)*time.Millisecond
	for _, windowID := range []string{"", "window-1"} {
		t.Run(windowID, func(t *testing.T) {
			t.Setenv("DAINTREE_WINDOW_ID", windowID)
			if got := bootSplashDuration(); got != wantNormal {
				t.Fatalf("splash duration = %s, want fixed visual budget %s", got, wantNormal)
			}
		})
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

func splashPartInkRows(lines, parts []string, part rune) int {
	rows := 0
	for row := range lines {
		if splashPartRowHasInk(lines, parts, part, row) {
			rows++
		}
	}
	return rows
}

func assertSplashRun(t *testing.T, line string, start int, want string) {
	t.Helper()
	gotRunes := []rune(line)
	wantRunes := []rune(want)
	for i, wantRune := range wantRunes {
		col := start + i
		if col >= len(gotRunes) {
			t.Fatalf("line too short for run at col %d", col)
		}
		if gotRunes[col] != wantRune {
			t.Fatalf("col %d = %q, want %q in line %q", col, gotRunes[col], wantRune, line)
		}
	}
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
