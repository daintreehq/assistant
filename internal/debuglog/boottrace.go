package debuglog

import (
	"os"
	"strconv"
	"sync"
	"time"
)

// Boot trace: an opt-in, file-based timing side-channel for the boot path. When
// DAINTREE_ASSISTANT_BOOT_TRACE names a file, each BootTrace call appends one
// "<msSinceProcessStart>\t<phase>\n" line to it. Unlike the session debug log it
// needs no config (it must cover the window BEFORE config/StartDebugLog exist),
// works in attached mode without touching the terminal, and is a strict no-op when
// the env var is unset. Used by the boot/one-shot latency benchmarks; cheap enough
// to leave compiled in.
//
// processStart is captured at package init — debuglog is imported by main's whole
// dependency tree, so this is within single-digit ms of exec, close enough for
// boot-phase attribution (external wall-clock covers the exec+runtime gap).
var processStart = time.Now()

var (
	bootTraceOnce sync.Once
	bootTracePath string
)

// BootTrace appends one timing line for phase. Best-effort: any failure disables
// nothing and reaches no caller — a benchmark aid must never affect the run it
// measures. Concurrent callers are safe: each line is a single O_APPEND write
// syscall to a local regular file, so the kernel positions and lands it whole.
func BootTrace(phase string) {
	bootTraceOnce.Do(func() {
		bootTracePath = os.Getenv("DAINTREE_ASSISTANT_BOOT_TRACE")
	})
	if bootTracePath == "" {
		return
	}
	ms := time.Since(processStart).Milliseconds()
	f, err := os.OpenFile(bootTracePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(strconv.FormatInt(ms, 10) + "\t" + phase + "\n")
}
