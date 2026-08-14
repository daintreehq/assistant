// Package debuglog writes a per-session, append-only, human-readable trace. It is a
// no-op when disabled and NEVER panics into the caller: a write failure warns ONCE on
// stderr, then is swallowed.
//
// # Redaction
//
// Every value is passed through internal/redact before it is written, and every block
// value is capped. That happens HERE, at the write boundary, rather than at the ~30 call
// sites, for two reasons: a call site that forgets is invisible until the day it matters,
// and new call sites get the protection without knowing the rule exists.
//
// What redaction does and does not buy: credential SHAPES and this process's own
// registered secrets are removed, so a bearer token, an `export API_KEY=…`, or a PEM
// block will not survive into the file. Everything else does. The trace still contains
// the conversation, terminal output, file excerpts, issue bodies, and memory contents,
// which is exactly what makes it useful for archaeology and exactly why it is an
// owner-only local artifact (dir 0700, file 0600, pruned after 7 days) and NOT a support
// artifact. `daintree-assistant support-bundle` is the thing to hand to someone else.
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

	"github.com/daintreehq/assistant/internal/redact"
)

const (
	// maxLogAgeMs: logs older than this (by mtime) are deleted at boot.
	maxLogAgeMs = int64(7 * 24 * 60 * 60 * 1000)
	// inlineMax: a string ≤120 chars with no newline renders inline as key=value;
	// otherwise it renders as an indented block.
	inlineMax = 120
	// blockValueMax caps a single block value (a tool's args, a result payload, a
	// terminal excerpt). Generous — the whole point of this trace is that you can read
	// what the model actually saw, and a stingy cap would cut off the interesting part
	// of exactly the payload you opened the log for.
	//
	// But unbounded was worse. One terminal dump can be megabytes; a turn that reads
	// several of them wrote a log nobody could open, and the value that mattered was
	// buried under a screenful of build output. The overflow is replaced by a size and a
	// content hash, so two occurrences of the same payload are still recognisably the
	// same without either being stored.
	blockValueMax = 64 * 1024
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
// JSON, strings as-is.
//
// EVERY rendered string — inline and block alike — passes through redact.String before
// it is written, and block values are additionally capped at blockValueMax. Numbers and
// booleans skip redaction: they cannot carry a credential, and running a dozen regexes
// over "durationMs=38" on every line of a chatty trace is pure cost.
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
		// blockValue redacts as it renders (see there), so only the cap is applied here.
		// Redacting again would run 15 regexes a second time over the largest values in
		// the trace — precisely the payloads where that costs the most.
		blocks = append(blocks, block{key: k, val: redact.Cap(blockValue(v), blockValueMax)})
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
		// Redact BEFORE the length test, not after: masking can change the length, and a
		// 130-char string that redacts to 110 should render inline like any other short
		// value rather than becoming an indented block for no visible reason.
		r := redact.String(t)
		if len(r) <= inlineMax && !strings.ContainsRune(r, '\n') {
			return r, true
		}
		return "", false
	default:
		return "", false
	}
}

// blockValue renders a non-inline value, REDACTED: a string through the free-text
// patterns, everything else walked structurally and re-marshaled as indented JSON.
//
// Structured values are walked rather than regexed for the same reason the audit path is:
// running the patterns over serialized JSON corrupted it (an env-assignment value ran to
// the next whitespace, and minified JSON has none). The caller applies the size cap.
func blockValue(v any) string {
	if s, ok := v.(string); ok {
		return redact.String(s)
	}
	data, err := json.MarshalIndent(redact.Value(v), "", "  ")
	if err != nil {
		// %v on an unmarshalable value can still print struct fields verbatim, so the
		// fallback gets the free-text pass rather than escaping redaction entirely.
		return redact.String(fmt.Sprintf("%v", v))
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

// randomID returns an 8-char lowercase base36 id. The produced filename must
// satisfy sessionLogRe.
func randomID() string {
	const alphabet = "0123456789abcdefghijklmnopqrstuvwxyz"
	b := make([]byte, 8)
	for i := range b {
		b[i] = alphabet[rand.Intn(len(alphabet))]
	}
	return string(b)
}
