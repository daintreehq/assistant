// Package debuglog writes a per-session, full-fidelity, append-only human-readable
// trace. It is a no-op when disabled and NEVER panics into the caller: a write
// failure warns ONCE on stderr, then is swallowed. Spec: docs/port/domain-config.md §4.
//
// Full-fidelity logs contain model messages, tool args, and terminal output, so
// the dir is 0700 and files are 0600 (owner-only).
package debuglog

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// maxLogAgeMs: logs older than this (by mtime) are deleted at boot.
	maxLogAgeMs = int64(7 * 24 * 60 * 60 * 1000)
	// inlineMax: a string ≤120 chars with no newline renders inline as key=value;
	// otherwise it renders as an indented block.
	inlineMax = 120
)

// sessionLogRe matches the only filenames eligible for pruning.
var sessionLogRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}-.+\.log$`)

// safeIDRe strips characters not allowed in a filename id.
var safeIDRe = regexp.MustCompile(`[^\w.-]`)

// Config is the minimal slice of AppConfig this package needs.
type Config struct {
	DebugLog bool
	LogDir   string
}

var (
	mu            sync.Mutex
	activeLogPath string
	warnedOnce    bool
)

// enabled reports whether logging is on for the given config.
func enabled(cfg Config) bool {
	return cfg.DebugLog && cfg.LogDir != ""
}

// warnOnce prints a single disabled-for-this-run notice to stderr. Caller holds mu.
func warnOnce(err error) {
	if warnedOnce {
		return
	}
	warnedOnce = true
	fmt.Fprintf(os.Stderr, "[debugLog] write failed (logging disabled for this run): %v\n", err)
}

// StartDebugLog primes per-session logging: prune old logs, choose the session
// file path, emit the session.start header, and return the path. No-op (returns
// "") when disabled. Call once per process after config load.
func StartDebugLog(cfg Config, header map[string]any) string {
	if !enabled(cfg) {
		return ""
	}
	mu.Lock()
	pruneOldLogs(cfg.LogDir)
	sessionID, _ := header["sessionId"].(string)
	if sessionID == "" {
		sessionID = randomID()
	}
	date := time.Now().UTC().Format("2006-01-02")
	safe := safeIDRe.ReplaceAllString(sessionID, "")
	if safe == "" {
		safe = "session"
	}
	activeLogPath = filepath.Join(cfg.LogDir, fmt.Sprintf("%s-%s.log", date, safe))
	path := activeLogPath
	mu.Unlock()

	LogDebug(cfg, "session.start", header)
	return path
}

// CurrentDebugLogPath returns the active log path once logging has started, or "".
func CurrentDebugLogPath() string {
	mu.Lock()
	defer mu.Unlock()
	return activeLogPath
}

// LogDebug appends one event line (plus any block values) to the session log.
// No-op unless logging is enabled. Catches all errors → warnOnce; never panics.
func LogDebug(cfg Config, event string, fields map[string]any) {
	if !enabled(cfg) {
		return
	}
	mu.Lock()
	defer mu.Unlock()

	if err := os.MkdirAll(cfg.LogDir, 0o700); err != nil {
		warnOnce(err)
		return
	}
	target := resolveTarget(cfg.LogDir)

	line := formatLine(event, fields)

	f, err := os.OpenFile(target, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		warnOnce(err)
		return
	}
	defer f.Close()
	if _, err := f.WriteString(line); err != nil {
		warnOnce(err)
	}
}

// resolveTarget reuses activeLogPath iff its dir matches logDir; else lazily opens
// a fresh <date>-<randomId>.log so stray writes still coalesce. Caller holds mu.
func resolveTarget(logDir string) string {
	if activeLogPath != "" && filepath.Dir(activeLogPath) == logDir {
		return activeLogPath
	}
	date := time.Now().UTC().Format("2006-01-02")
	activeLogPath = filepath.Join(logDir, fmt.Sprintf("%s-%s.log", date, randomID()))
	return activeLogPath
}

// formatLine renders one trace line:
//
//	<ISO-timestamp>  <event>[  k1=v1 ...]\n
//	[  <blockKey>:\n    <indented value>\n ...]
//
// Inline scalars: null/number/bool, or a string ≤120 chars without newline.
// Nil-valued fields are omitted (noise). Block values: objects/arrays via indented
// JSON, strings as-is. Values are NEVER truncated.
func formatLine(event string, fields map[string]any) string {
	ts := time.Now().UTC().Format("2006-01-02T15:04:05.000Z07:00")

	// Deterministic field order for stable output.
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var inline strings.Builder
	type block struct {
		key, val string
	}
	var blocks []block

	for _, k := range keys {
		v := fields[k]
		if v == nil {
			// Distinguish a literal nil interface (omit) — but in Go an explicit
			// JSON null isn't representable as a nil here, so we render literal
			// "null" only for explicit Null sentinel; a nil value is omitted.
			continue
		}
		if scalar, ok := inlineScalar(v); ok {
			fmt.Fprintf(&inline, "  %s=%s", k, scalar)
			continue
		}
		blocks = append(blocks, block{key: k, val: blockValue(v)})
	}

	var b strings.Builder
	b.WriteString(ts)
	b.WriteString("  ")
	b.WriteString(event)
	b.WriteString(inline.String())
	b.WriteByte('\n')
	for _, bl := range blocks {
		fmt.Fprintf(&b, "  %s:\n", bl.key)
		for _, l := range strings.Split(bl.val, "\n") {
			fmt.Fprintf(&b, "    %s\n", l)
		}
	}
	return b.String()
}

// Null is a sentinel rendering as the literal "null" inline (distinct from an
// omitted nil field).
var Null = nullValue{}

type nullValue struct{}

// inlineScalar reports whether v renders inline, returning its rendering.
func inlineScalar(v any) (string, bool) {
	switch t := v.(type) {
	case nullValue:
		return "null", true
	case bool:
		return strconv.FormatBool(t), true
	case int:
		return strconv.Itoa(t), true
	case int64:
		return strconv.FormatInt(t, 10), true
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64), true
	case string:
		if len(t) <= inlineMax && !strings.ContainsRune(t, '\n') {
			return t, true
		}
		return "", false
	default:
		return "", false
	}
}

// blockValue renders a non-inline value: strings as-is, everything else as
// indented JSON.
func blockValue(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(data)
}

// pruneOldLogs deletes matching logs older than maxLogAgeMs by mtime. Best-effort;
// a missing dir is a no-op; never panics.
func pruneOldLogs(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().UnixMilli() - maxLogAgeMs
	for _, e := range entries {
		if e.IsDir() || !sessionLogRe.MatchString(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().UnixMilli() < cutoff {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// randomID returns an 8-char lowercase base36 id. The exact bytes need not match
// the TS output, but the produced filename must satisfy sessionLogRe.
func randomID() string {
	const alphabet = "0123456789abcdefghijklmnopqrstuvwxyz"
	b := make([]byte, 8)
	for i := range b {
		b[i] = alphabet[rand.Intn(len(alphabet))]
	}
	return string(b)
}
