package ui

import (
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/ui/theme"
)

// splash_ported_test.go ports tests/ui/StartupSplash.test.tsx: the boot splash draws
// in (more ink over frames), sizes naturally with top breathing room + horizontal
// centering, and — crucially — a too-narrow terminal SKIPS the mark but still fires
// its done timer so boot never hangs. The composer-never-gated contract already lives
// in liveness_test (Splash_DoesNotGateComposer).

func splashTheme() theme.Theme { return darkTheme() }

func TestSplash_FramesAreNonEmptyAndDrawIn(t *testing.T) {
	th := splashTheme()
	// First frame (frame 0) reveals fewer rows than the final frame.
	first := splashModel{frame: 0}
	last := splashModel{frame: SplashFrames - 1}
	ink := func(s string) int {
		return len(strings.ReplaceAll(strings.ReplaceAll(stripAnsi(s), " ", ""), "\n", ""))
	}
	firstInk := ink(first.view(th, 80))
	lastInk := ink(last.view(th, 80))
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
	lines := strings.Split(s.view(th, 60), "\n")
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
	if nonBlank > len(splashArt)+2 {
		t.Errorf("splash should be natural height, drew %d ink rows", nonBlank)
	}
}

func TestSplash_CentersHorizontally(t *testing.T) {
	th := splashTheme()
	s := splashModel{frame: SplashFrames - 1}
	minPad := func(columns int) int {
		min := 1 << 30
		for _, l := range strings.Split(stripAnsi(s.view(th, columns)), "\n") {
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
	if got := s.view(splashTheme(), SplashWidth); strings.TrimSpace(got) != "" {
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
