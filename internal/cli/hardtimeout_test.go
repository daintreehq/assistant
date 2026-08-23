package cli

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daintreehq/assistant/internal/domain"
)

// --timeout cancels a context, and a context only bounds code that WATCHES it. A CI
// runner whose whole purpose is to finish deterministically needs a real wall clock, so
// the second stage kills the process rather than letting a wedged read hang the job.
func TestHardTimeoutWatchdogFiresWhenTheRunWillNotUnwind(t *testing.T) {
	var mu sync.Mutex
	var code int
	var fired bool
	diag := &lockedBuffer{}

	// The watchdog fires at timeout+grace. Waiting the real 30s grace is not a test, so
	// offset the timeout by exactly the grace to land the deadline shortly from now.
	disarm := startHardTimeoutWatchdog(testFireIn(150*time.Millisecond), diag, func(c int) {
		mu.Lock()
		defer mu.Unlock()
		code, fired = c, true
	})
	defer disarm()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		done := fired
		mu.Unlock()
		if done {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if !fired {
		t.Fatal("the watchdog never fired; --timeout would remain merely cooperative")
	}
	if code != domain.OneShotExitCode.HardTimeout {
		t.Errorf("exit code = %d, want %d (a hang must be distinguishable from a clean cancel)",
			code, domain.OneShotExitCode.HardTimeout)
	}
	if !strings.Contains(diag.String(), "hard timeout") {
		t.Errorf("watchdog said %q, want it to name the reason", diag.String())
	}
}

// The normal path must never reach the watchdog: a run that unwinds cleanly disarms it,
// and a killed-mid-flush run would trade a hung job for a corrupted one.
func TestHardTimeoutWatchdogDisarmsOnCleanExit(t *testing.T) {
	var mu sync.Mutex
	fired := false
	disarm := startHardTimeoutWatchdog(testFireIn(100*time.Millisecond), &bytes.Buffer{}, func(int) {
		mu.Lock()
		defer mu.Unlock()
		fired = true
	})
	disarm()

	time.Sleep(400 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if fired {
		t.Error("the watchdog killed a run that had already finished")
	}
}

// It must not write to stdout: --json promises stdout carries only protocol frames, and
// a watchdog that announced itself there would corrupt the stream a harness is parsing.
func TestHardTimeoutWatchdogWritesOnlyToTheGivenDiagnosticWriter(t *testing.T) {
	// The watchdog writes from a timer goroutine, so the buffer needs a lock — polling
	// bytes.Buffer.Len() from the test goroutine while Fprintf grows it is a race in the
	// harness, not in the code under test.
	diag := &lockedBuffer{}
	fired := make(chan struct{})
	disarm := startHardTimeoutWatchdog(testFireIn(150*time.Millisecond), diag, func(int) { close(fired) })
	defer disarm()

	select {
	case <-fired:
	case <-time.After(5 * time.Second):
		t.Fatal("the watchdog never fired")
	}
	got := diag.String()
	if got == "" {
		t.Fatal("the watchdog wrote nothing at all")
	}
	if strings.Contains(got, "{") {
		t.Errorf("the watchdog emitted JSON-looking output: %q", got)
	}
}

// lockedBuffer is a bytes.Buffer safe to read while the watchdog goroutine writes.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// testFireIn returns the --timeout value that makes the watchdog fire after d, given
// that it arms at timeout+grace. Waiting the real grace would make every test here 30s.
func testFireIn(d time.Duration) time.Duration { return d - domain.HardTimeoutGrace }
