package ui

import (
	"os"
	"strings"
	"testing"
)

func TestSplash_DumpSelectedFrames(t *testing.T) {
	if os.Getenv("DAINTREE_DUMP_SPLASH") == "" {
		t.Skip("set DAINTREE_DUMP_SPLASH=1 to print selected splash frames")
	}
	frames := []int{0, 4, 8, 12, 16, splashCanopyEndFrame, SplashFrames - 1}
	for _, frame := range frames {
		t.Logf("\n--- splash frame %d ---\n%s", frame, strings.Join(splashFrameLines(frame), "\n"))
	}
}
