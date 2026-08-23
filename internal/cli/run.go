package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/daintreehq/assistant/internal/agent"
	"github.com/daintreehq/assistant/internal/app"
	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/cli/jsonout"
	"github.com/daintreehq/assistant/internal/cli/render"
	"github.com/daintreehq/assistant/internal/config"
	"github.com/daintreehq/assistant/internal/debuglog"
	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/mcp"
	"github.com/daintreehq/assistant/internal/projectinstructions"
	"github.com/daintreehq/assistant/internal/redact"
	"github.com/daintreehq/assistant/internal/storage"
	"github.com/daintreehq/assistant/internal/tools"
	"github.com/mattn/go-isatty"
)

// Options are the parsed CLI flags + the one-shot prompt.
type Options struct {
	McpURL   string
	McpToken string
	// McpTokenFile is a PATH to the Daintree MCP bearer, for a caller that must not put
	// the token in an argument — the MCP server surface, where the argument would be
	// chosen by a model. Read and validated on the same fatal path as --api-key-file.
	McpTokenFile string
	Project      string
	Tier         string
	// Offline/AutoApprove/DebugLog are POINTERS: nil means the flag was not passed and
	// the environment decides, false means it was explicitly turned OFF and must beat
	// the environment. Collapsing those two cases would let DAINTREE_ASSISTANT_AUTO_APPROVE=1
	// survive an explicit --auto-approve=false.
	Offline *bool
	// Classic is a DEPRECATED NO-OP — accepted and ignored. The line REPL it used to
	// select is now the only interactive front end (the Bubble Tean attached session was removed
	// when Daintree took over rendering), so the flag no longer chooses anything.
	Classic   bool
	JSON      bool
	Inline    bool // DEPRECATED NO-OP — accepted and ignored
	Prompt    string
	HasPrompt bool

	// The harness knobs. Every one of these already had a ConfigOverrides field and a
	// trusted env var; only the flag was missing, which forced any scripted caller to
	// rewrite the process environment to say something argv says perfectly well. They
	// carry the SAME trust as the env vars they shadow (argv is as trusted as env) and
	// win over them, per the FirstString order in config.LoadConfig.
	BackendURL string
	// APIKeyFile is a path, never the key itself: argv is world-readable through `ps`,
	// so there is deliberately no --api-key. Read once, in overridesFromOptions.
	APIKeyFile string
	// PromptFile is a path (or "-" for stdin) holding the one-shot prompt. It is a PATH,
	// not the prompt itself, for the same reason parseArgs is I/O-free: the flag layer
	// captures it and RunOneShot reads it, inside the --timeout bound.
	PromptFile string
	// MultiTurn runs a whole CONVERSATION in one process: one prompt per stdin line,
	// each its own turn against the same session, all of it one JSONL transcript. It
	// requires --json (without it the line REPL on piped stdin is already exactly
	// this) and refuses to share a run with either single-prompt source, since two
	// prompt spellings at once is a mistake rather than a precedence question.
	MultiTurn bool
	StateDir  string
	LogDir    string
	// ProjectID scopes StateDir into a per-project subdirectory, so it is how a harness
	// gets project isolation without hand-rolling state directories. WindowID is the
	// other half of looking like a real Daintree window rather than a bare cwd.
	ProjectID string
	WindowID  string
	// ProjectInstructionsFile is a path, but the override it feeds carries CONTENT: the
	// flag does the read, because config.LoadConfig never touches the filesystem for
	// ProjectInstructions.
	ProjectInstructionsFile string
	AutoApprove             *bool
	DebugLog                *bool

	// PinnedSkillIDs are the backend runbook ids `--skill` named (repeatable). Like
	// Timeout this is a session control rather than configuration: it has no env var,
	// and carrying it through config would also pin the supervisor's unattended wake
	// turns, which nobody asked for. Negotiated once per launch by
	// App.PreparePinnedSkills — naming an id here does not yet mean it will be honoured.
	PinnedSkillIDs []string

	// Timeout bounds a one-shot run's wall clock (zero = unbounded). It is NOT a config
	// value: it cancels the run context, so the turn unwinds through the same path as a
	// SIGINT and reports cancelled rather than being killed mid-write.
	Timeout time.Duration

	// RunScheduler opts a one-shot into running the scheduler + async coordinator for
	// the life of the run, matching what `mcp --stdio` and `host --stdio` already do. It
	// is off by default because taking the lease and ticking is a bigger commitment
	// than a scripted query should make by accident; the flag path requires a positive
	// Timeout so an unsettleable job cannot hang a harness forever. Routing-only, like
	// Timeout — never carried into config.
	RunScheduler bool
}

// overridesFromOptions maps routing-irrelevant flags to config overrides. classic/
// inline/json/timeout are routing-only and NOT carried into config.
//
// It returns an error only for --api-key-file: an unreadable key file must be fatal,
// never a silent fall-through to the stored sign-in. Falling back would run the job
// against a DIFFERENT key than the caller named — spending someone else's credit and
// hiding the mistake behind a successful-looking run.
func overridesFromOptions(opts Options) (config.ConfigOverrides, error) {
	var o config.ConfigOverrides
	set := func(dst **string, v string) {
		if v != "" {
			val := v
			*dst = &val
		}
	}
	set(&o.McpURL, opts.McpURL)
	set(&o.McpToken, opts.McpToken)
	set(&o.ProjectPath, opts.Project)
	set(&o.Tier, opts.Tier)
	set(&o.BackendURL, opts.BackendURL)
	set(&o.StateDir, opts.StateDir)
	set(&o.LogDir, opts.LogDir)
	set(&o.ProjectID, opts.ProjectID)
	set(&o.WindowID, opts.WindowID)
	// Pass the booleans through as-is. They already carry the "was the flag passed"
	// distinction as nil-ness, so re-deriving it here would throw it away again.
	o.Offline = opts.Offline
	o.AutoApprove = opts.AutoApprove
	o.DebugLog = opts.DebugLog
	if opts.APIKeyFile != "" {
		key, err := readAPIKeyFile(opts.APIKeyFile)
		if err != nil {
			return config.ConfigOverrides{}, err
		}
		o.APIKey = &key
	}
	if opts.McpTokenFile != "" {
		token, err := readMcpTokenFile(opts.McpTokenFile)
		if err != nil {
			return config.ConfigOverrides{}, err
		}
		o.McpToken = &token
	}
	// A NON-NIL ProjectInstructions is the provenance signal every auto-load path below
	// checks: it means a caller named the file explicitly, so no DAINTREE.md discovery
	// may overwrite it. Setting it here (rather than in buildOverrides) is what makes
	// that true for the host and MCP paths too, since both start from this function.
	if opts.ProjectInstructionsFile != "" {
		content, err := readProjectInstructionsFile(opts.ProjectInstructionsFile)
		if err != nil {
			return config.ConfigOverrides{}, err
		}
		o.ProjectInstructions = &content
	}
	return o, nil
}

// loadConfigFromOptions is overridesFromOptions + LoadConfig, which is what every
// subcommand that needs config but not an App actually wants. Folding the pair here
// keeps the --api-key-file failure on the SAME error path as a bad config, so no call
// site can accidentally drop it.
func loadConfigFromOptions(opts Options) (config.AppConfig, error) {
	o, err := overridesFromOptions(opts)
	if err != nil {
		return config.AppConfig{}, err
	}
	return config.LoadConfig(o)
}

// loadProbeConfigFromOptions is loadConfigFromOptions for a read-only probe: same
// resolution, but it does not create the state directory. See config.LoadConfigForProbe
// — a question about the backend must not have a side effect on the user's disk, nor
// fail because a state dir it never wanted is unwritable.
func loadProbeConfigFromOptions(opts Options) (config.AppConfig, error) {
	o, err := overridesFromOptions(opts)
	if err != nil {
		return config.AppConfig{}, err
	}
	return config.LoadConfigForProbe(o)
}

// readAPIKeyFile reads a single-line credential file.
//
// This is the OPTIONAL caller-key path. The CLI needs no credential — the backend funds
// a turn from its own — so a run that names a key file is deliberately opting to spend a
// different account's credit, and every failure below is therefore FATAL rather than a
// fallback: silently reverting to "whatever the environment had" would bill the wrong
// account and hide the mistake behind a successful-looking run.
//
// Trailing newlines are the norm (`printf %s` is not how anyone writes one), so trim; an
// empty file is an error rather than an empty override.
//
// The read is BOUNDED. A path is caller-supplied and need not be a regular file — a
// FIFO or /dev/zero would otherwise block or grow forever, defeating --timeout (whose
// deadline cannot cover a syscall that never returns). One key plus generous slack is
// all a valid file can hold.
func readAPIKeyFile(path string) (string, error) {
	// Through the same guard the other file flags use. The bound was already documented
	// as covering a FIFO, and it did not: os.Open blocks on one before any LimitReader
	// applies. An over-length file is now rejected outright rather than truncated into a
	// shape error, which is the more honest failure for a credential.
	key, err := readBoundedFile(path, backend.MaxKeyLength+1024, "--api-key-file", false)
	if err != nil {
		return "", err
	}
	// The SAME structural check config.LoadConfig applies to DAINTREE_API_KEY, for the
	// same reason: an embedded newline, a smart quote or an over-length paste becomes a
	// readable message here instead of Go's opaque "invalid header field value" on every
	// turn. It also pins "single-line" — ValidateKeyShape rejects every byte below 0x21,
	// newlines included.
	if err := backend.ValidateKeyShape(key); err != nil {
		return "", fmt.Errorf("--api-key-file %s: %w", path, err)
	}
	// Registering here means the key is masked in the debug log from the first line
	// written, including the boot header — app.Create does the same for the env-supplied
	// one, and a key that arrives by file must not be the one that leaks.
	redact.RegisterSecret(key)
	return key, nil
}

// osExit is os.Exit behind a seam so the watchdog can be tested without killing the
// test binary.
var osExit = os.Exit

// startHardTimeoutWatchdog arms the second stage of --timeout and returns a function
// that disarms it. See domain.HardTimeoutGrace for why one stage is not enough.
//
// It writes to stderr, never stdout: --json's whole contract is that stdout carries only
// protocol frames, and a watchdog that violated it to announce itself would corrupt the
// very stream a harness is parsing. The terminal `result` line is already lost in this
// path — that is what makes it a failure worth a distinct exit code rather than a
// cancellation.
func startHardTimeoutWatchdog(timeout time.Duration, diag io.Writer, exit func(int)) func() {
	timer := time.AfterFunc(timeout+domain.HardTimeoutGrace, func() {
		fmt.Fprintf(diag,
			"hard timeout: --timeout (%s) expired and the run did not unwind within %s; killing the process (exit %d). "+
				"Something ignored cancellation — a tool, a wedged read, or a syscall in flight.\n",
			timeout, domain.HardTimeoutGrace, domain.OneShotExitCode.HardTimeout)
		exit(domain.OneShotExitCode.HardTimeout)
	})
	return func() { timer.Stop() }
}

// readMcpTokenFile reads the Daintree MCP bearer from a file.
//
// A path rather than a value, and for a sharper reason than the API key: this bearer
// authorizes system-tier Daintree actions for its whole validity window, and its one
// caller is the MCP server, where an inline argument would be chosen by a MODEL — and so
// could be echoed by a prompt injection, logged by the MCP client, or captured by traces
// outside this repository. The runtime already stopped writing it to its own debug log
// on exactly that reasoning.
//
// Failure is FATAL, never a fall-through to the environment: a caller that named a token
// file meant to bind this session to THAT token, and silently using another one would
// point the assistant at a different Daintree window behind a successful-looking run.
func readMcpTokenFile(path string) (string, error) {
	// Same bounded read as the key file — a caller-supplied path need not be a regular
	// file, and os.Open blocks on a FIFO before any LimitReader could apply.
	token, err := readBoundedFile(path, backend.MaxKeyLength+1024, "--mcp-token-file", false)
	if err != nil {
		return "", err
	}
	// Not ValidateKeyShape: that check is about the BACKEND's key format. What matters
	// here is that the value can be a header — a stray newline or control byte would
	// otherwise surface as Go's opaque "invalid header field value" on every MCP call.
	for _, r := range token {
		if r < 0x21 || r == 0x7f {
			return "", fmt.Errorf("--mcp-token-file %s: the token contains a space or control character; it must be a single line", path)
		}
	}
	// Registered before it is used anywhere, so it is masked in the debug log from the
	// first line written — app.Create does the same for the env-supplied one.
	redact.RegisterSecret(token)
	return token, nil
}

// maxPromptFileBytes bounds --prompt-file. A prompt is prose, not a payload: a megabyte
// is a very long runbook and still far short of anything a turn could carry. The bound
// exists because stdin need not be a finite stream — /dev/zero or a live FIFO would
// otherwise grow the read forever, which --timeout cannot preempt (a deadline does not
// interrupt a syscall already in progress). A NAMED path is additionally required to be
// a regular file; see readBoundedFile.
const maxPromptFileBytes = 1 << 20 // 1 MiB

// promptFileStdin is the literal token that means "read the prompt from stdin". Only the
// exact token — `./-` stays an ordinary filename, which is the whole reason the
// convention spells it as a single character.
const promptFileStdin = "-"

// readBoundedText is the shared shape behind --prompt-file and
// --project-instructions-file: read at most limit+1 bytes so hitting exactly the limit
// is accepted while anything larger is REJECTED rather than silently truncated, then
// trim. A truncated prompt or a truncated brief is worse than a refusal, because the run
// looks successful while the model was asked a different question.
func readBoundedText(r io.Reader, limit int64, flag, path string) (string, error) {
	raw, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return "", fmt.Errorf("%s %s: %w", flag, path, err)
	}
	if int64(len(raw)) > limit {
		return "", fmt.Errorf("%s %s: larger than the %d-byte limit", flag, path, limit)
	}
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return "", fmt.Errorf("%s %s: file is empty", flag, path)
	}
	return text, nil
}

// readPromptFile resolves --prompt-file to the prompt text.
//
// Every failure is fatal: unlike a key file there is no other source to fall back TO —
// the prompt is the caller's actual question, and a run that proceeded without it would
// have nothing to ask. stdin is passed in rather than reached for so the bound is
// testable, and it is NEVER closed: os.Stdin belongs to the process, not to this read.
func readPromptFile(path string, stdin io.Reader) (string, error) {
	if path == promptFileStdin {
		return readBoundedText(stdin, maxPromptFileBytes, "--prompt-file", "-")
	}
	return readBoundedFile(path, maxPromptFileBytes, "--prompt-file", true)
}

// readBoundedFile opens a NAMED path and reads it under a byte bound.
//
// It insists on a regular file, which is a stronger check than it looks: os.Open on a
// FIFO blocks until a writer appears, BEFORE any bound this code could apply, and
// --timeout cannot preempt a syscall already in flight. Streaming input has a spelling
// already — "-" — so a named pipe here is a mistake worth naming rather than a hang worth
// waiting out. A directory becomes a clear message instead of an opaque read error.
func readBoundedFile(path string, limit int64, flag string, stdinHint bool) (string, error) {
	advice := ""
	if stdinHint {
		advice = " (use '-' to stream from stdin)"
	}
	notRegular := func(p string) error {
		return fmt.Errorf("%s %s: not a regular file%s", flag, p, advice)
	}
	// Checked BEFORE the open, because that is the check that prevents the hang: os.Open
	// on a FIFO blocks waiting for a writer, and an error we never reach is no bound.
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", flag, err)
	}
	if !info.Mode().IsRegular() {
		return "", notRegular(path)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", flag, err)
	}
	defer f.Close()
	// And again on the DESCRIPTOR, which is a different question: stat-then-open is two
	// syscalls, so whatever we actually opened is not provably what we stat'd. This
	// closes the half that matters — we never READ a non-regular file — while the other
	// half (a swap to a FIFO landing between the two calls, which would still block in
	// Open) stays open, and needs a non-blocking open to shut. The path comes from argv,
	// so an attacker who can win that race already has the caller's own trust.
	if fi, err := f.Stat(); err != nil {
		return "", fmt.Errorf("%s %s: %w", flag, path, err)
	} else if !fi.Mode().IsRegular() {
		return "", notRegular(path)
	}
	return readBoundedText(f, limit, flag, path)
}

// readProjectInstructionsFile resolves --project-instructions-file to DAINTREE.md
// CONTENT (the override carries the text, not a path).
//
// It shares projectinstructions.MaxBytes because both sources land in the same prompt
// field and so deserve the same budget. It does NOT share the implicit loader's symlink
// rejection: that exists because the bound PROJECT is untrusted and could point its
// DAINTREE.md at a secret, whereas this path came from argv, which carries the same
// trust as the environment it shadows.
//
// A named file that cannot be read is fatal. Falling through to the repo's own
// DAINTREE.md would run the job against a DIFFERENT brief than the caller named and hide
// the typo behind a successful-looking run.
func readProjectInstructionsFile(path string) (string, error) {
	// stdinHint false: this flag has no "-" spelling, so advising one would send the
	// caller looking for a file literally named "-".
	return readBoundedFile(path, projectinstructions.MaxBytes, "--project-instructions-file", false)
}

// applyAutoProjectInstructions fills o.ProjectInstructions from an auto-DISCOVERED
// source, and only when nothing explicit is already there. Both discovery paths (this
// one's DAINTREE.md load and the host descriptor's) run after overridesFromOptions, so
// without this guard either would silently clobber --project-instructions-file and
// defeat the flag's whole purpose.
func applyAutoProjectInstructions(o *config.ConfigOverrides, content string) {
	if o.ProjectInstructions != nil || content == "" {
		return
	}
	c := content
	o.ProjectInstructions = &c
}

// buildOverrides resolves the overrides and loads the project DAINTREE.md (best
// effort; a warning is non-fatal). The discovered content only fills a project-
// instructions override that is still nil — an explicitly named
// --project-instructions-file always wins over a file the repo happened to contain.
func buildOverrides(opts Options, r *render.Renderer) (config.ConfigOverrides, error) {
	o, err := overridesFromOptions(opts)
	if err != nil {
		return config.ConfigOverrides{}, err
	}
	projectPath := opts.Project
	if projectPath == "" {
		if cwd, err := os.Getwd(); err == nil {
			projectPath = cwd
		}
	}
	res := projectinstructions.Load(projectPath)
	if res.Warning != "" {
		r.Warn(res.Warning)
	}
	applyAutoProjectInstructions(&o, res.Content)
	return o, nil
}

// Run routes per the top-level dispatch:
//
//	prompt        → RunOneShot
//	--multi-turn  → RunOneShot (prompts come from stdin, one turn per line)
//	--json no prompt → usage error (exit 2, stderr; normally caught by main)
//	else          → RunInteractive
func Run(ctx context.Context, opts Options) int {
	if opts.HasPrompt || opts.MultiTurn {
		return RunOneShot(ctx, opts)
	}
	if opts.JSON {
		fmt.Fprint(os.Stderr, "--json requires a prompt argument (one-shot mode only).\n")
		return 2
	}
	return RunInteractive(ctx, opts)
}

// RunOneShot is the scriptable path. In JSON mode stdout carries
// ONLY the JSONL stream; every human line goes to stderr.
func RunOneShot(ctx context.Context, opts Options) int {
	stderrR := render.New(os.Stderr)
	var sink *jsonout.Sink
	if opts.JSON {
		// The multi-turn sink differs ONLY in that it brackets turns and latches the
		// run's outcome across them; a single-prompt run keeps the plain constructor, so
		// its stream is what it has always been down to the byte.
		if opts.MultiTurn {
			sink = jsonout.NewMultiTurn(os.Stdout, domain.NowMS)
		} else {
			sink = jsonout.New(os.Stdout, domain.NowMS)
		}
	}

	// A --timeout bounds the run by CANCELLING it, so a turn already under way unwinds
	// through the same path SIGINT uses and the sink reports cancelled (exit 2) rather
	// than the process being killed mid-JSONL-line. It is installed FIRST so setup —
	// reading the key file, loading DAINTREE.md, the sign-in gate, waiting for the owner
	// lease — is inside the bound too; a deadline that only starts once the turn does is
	// not a wall-clock bound, and waiting for a lease is exactly where a scripted run
	// hangs. Individual syscalls that ignore context (os.ReadFile, app.Create) are
	// bounded by their own means; see readAPIKeyFile.
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
		// STAGE TWO: a hard wall-clock bound. The context deadline above is
		// cooperative, and a context only bounds code that watches it — a syscall
		// already in flight, a tool that ignores cancellation, a wedged `-` stdin read.
		// For a CI runner whose whole job is to finish deterministically, "usually
		// stops" is not a bound. So if the process is still alive a grace period after
		// its own deadline, kill it with a distinct exit code instead of hanging the
		// job. The normal path never reaches this: a clean cancel has to flush the
		// terminal result, release the lease and close the store, and the grace is sized
		// so that killing mid-flush is not the outcome we trade a hang for.
		defer startHardTimeoutWatchdog(opts.Timeout, os.Stderr, osExit)()
	}

	// reportError routes a setup failure to the active output contract. A failure that
	// is really OUR deadline expiring is reported as cancelled, not as an error: the
	// layers below translate a dead context into their own vocabulary (ownership turns
	// context.DeadlineExceeded into ErrProjectBusy), and reporting "another assistant
	// owns this project" for a self-inflicted timeout sends the reader hunting a
	// process that does not exist.
	timedOut := func() bool { return opts.Timeout > 0 && errors.Is(ctx.Err(), context.DeadlineExceeded) }
	reportError := func(err error) {
		if timedOut() {
			msg := fmt.Sprintf("timed out after %s", opts.Timeout)
			switch {
			case sink != nil && opts.MultiTurn:
				// A multi-turn stream promises that every assistant event sits inside a
				// turn bracket, and a setup timeout has no turn to sit in. CancelRun moves
				// the run's outcome without inventing an event, which is exactly the
				// distinction it was built for.
				sink.CancelRun()
				sink.Warn(msg)
			case sink != nil:
				sink.AssistantCancelled("")
				sink.Warn(msg)
			default:
				stderrR.Warn(msg)
			}
			return
		}
		msg := err.Error()
		if sink != nil {
			sink.Error(msg)
		} else {
			stderrR.Error(msg)
		}
	}
	// exitFor collapses the repeated "report, finish the sink, pick a code" tail every
	// setup failure below shares.
	exitFor := func() int {
		if sink != nil {
			return sink.Finish()
		}
		if timedOut() {
			return domain.OneShotExitCode.Cancelled
		}
		return domain.OneShotExitCode.Error
	}

	// The argument boundary already refuses these combinations; this is the same rule
	// restated where the damage would happen, because Options is also assembled by
	// callers that never went through parseArgs (the MCP server derives one per
	// session). Without --json there is no sink for the loop to write to, and two
	// prompt sources at once means one of them is silently ignored.
	if opts.MultiTurn {
		var bad error
		switch {
		case !opts.JSON:
			bad = errors.New("--multi-turn requires --json")
		// opts.Prompt is checked as well as HasPrompt: they are independent fields, and a
		// programmatic caller that set the text but not the flag would otherwise slip
		// past the very check that exists to stop a prompt being silently ignored.
		case opts.HasPrompt || opts.Prompt != "" || opts.PromptFile != "":
			bad = errors.New("--multi-turn reads its prompts from stdin and cannot be combined with a prompt argument or --prompt-file")
		}
		if bad != nil {
			reportError(bad)
			return exitFor()
		}
	}

	// The prompt file is read HERE, not in parseArgs: parsing stays I/O-free and
	// table-tested, and the read lands inside the --timeout bound with every other piece
	// of setup. HasPrompt was already true at the argument boundary, so this only fills
	// in the text the flag promised.
	if opts.PromptFile != "" {
		prompt, err := readPromptFile(opts.PromptFile, os.Stdin)
		if err != nil {
			reportError(err)
			return exitFor()
		}
		opts.Prompt = prompt
	}

	overrides, err := buildOverrides(opts, stderrR)
	if err != nil {
		reportError(err)
		return exitFor()
	}
	debuglog.BootTrace("oneshot.overrides.loaded")
	// One-shot takes the owner lease briefly (never spawning a daemon — a script
	// probe must not litter the machine with supervisors). A held lease means a
	// live assistant owns the project: fail loudly instead of double-opening.
	own, err := acquireOwnership(ctx, overrides, false, 10*time.Second,
		func(m string) { fmt.Fprintln(os.Stderr, m) })
	if err != nil {
		reportError(err)
		return exitFor()
	}
	defer own.Release()
	debuglog.BootTrace("oneshot.ownership.acquired")
	a, err := app.Create(app.CreateOptions{Overrides: overrides, PinnedSkillIDs: opts.PinnedSkillIDs})
	if err != nil {
		reportError(err)
		return exitFor()
	}

	// A debug-log path is diagnostic metadata, never answer content. Keep it on
	// stderr for every one-shot mode so stdout remains empty on a failed human run.
	logPath := debuglog.StartDebugLog(debuglog.Config{DebugLog: a.Config.DebugLog, LogDir: a.Config.LogDir},
		map[string]any{"sessionId": a.SessionID, "project": a.Config.ProjectPath})
	if logPath != "" {
		stderrR.Line(stderrR.Gray("logging to " + logPath))
	}

	// No model-key preflight: the CLI no longer holds model credentials (the backend
	// owns them). If the backend is unreachable the turn fails with a clear
	// "could not reach assistant backend" error from the backend client.

	// The ONE preflight that remains, and only when `--skill` named something. It costs
	// a capability GET, which is why it is conditional: an ordinary scripted run must
	// not grow a network round trip. A failure here aborts BEFORE the turn, because a
	// pin that cannot be negotiated produces a normal-looking run that silently did not
	// load the runbook — exactly what --skill exists to rule out.
	pinNotice, perr := a.PreparePinnedSkills(ctx)
	if perr != nil {
		if serr := a.Shutdown(); serr != nil {
			fmt.Fprintf(os.Stderr, "shutdown error: %v\n", serr)
		}
		reportError(perr)
		return exitFor()
	}

	// AUTO_APPROVE reaches HERE too, and that is easy to miss. One-shot is
	// non-interactive, so it installs an auto-DECLINE confirm hook below — but dispatch
	// skips the hook entirely when AutoApprove is set, because a one-shot run is still
	// the `main` actor. The net effect is that an inherited
	// DAINTREE_ASSISTANT_AUTO_APPROVE=1 makes a scripted run perform tier-allowed
	// mutations with nothing on screen to say so. The attached session has a persistent badge for
	// exactly this; a scripted run has no footer, so it gets a loud line instead —
	// on stderr, and as a structured event in JSON mode, so neither output contract
	// breaks.
	//
	// It is a WARNING, not an error, and the distinction is load-bearing: Sink.Error
	// flips the terminal status and stamps errorMessage, which AssistantEnd clears but
	// AssistantCancelled does NOT — so routing this through Error made a cancelled run
	// report its failure as "AUTO-APPROVE is ON". An event-driven consumer watching for
	// `error` lines would also abort a perfectly healthy run.
	warnAutoApprove := func() {
		if !a.Config.AutoApprove {
			return
		}
		// Name the source that actually turned it on. Telling someone to unset an env var
		// they never set — because they passed --auto-approve — is a dead end.
		source := "DAINTREE_ASSISTANT_AUTO_APPROVE=1 is set"
		if opts.AutoApprove != nil && *opts.AutoApprove {
			source = "--auto-approve was passed"
		}
		warn := fmt.Sprintf(
			"AUTO-APPROVE is ON (%s): mutating actions will run WITHOUT confirmation (tier '%s'). "+
				"Turn it off unless this is an automated harness.", source, a.Tier())
		if sink != nil {
			sink.Warn(warn)
		} else {
			stderrR.Warn(warn)
		}
	}

	confirm := func(_ context.Context, req tools.ConfirmRequest) (bool, error) {
		// One-shot is non-interactive → auto-decline. Reached ONLY when AutoApprove is
		// off; see the warning above for why that distinction matters.
		msg := fmt.Sprintf("Skipping %s (%s) — confirmation needed; run interactively to approve.", req.ToolName, req.Risk)
		if sink != nil {
			fmt.Fprint(os.Stderr, "  "+msg+"\n")
		} else {
			stderrR.Warn(msg)
		}
		return false, nil
	}
	logHook := func(m string) {
		if sink != nil {
			fmt.Fprintf(os.Stderr, "  · %s\n", m)
		} else {
			stderrR.Line(stderrR.Gray("  · " + m))
		}
	}

	cs := newOneShotConsoleSink(render.Stdout(), stderrR)
	var events agent.EventSink = cs
	if sink != nil {
		events = sink
	}
	a.SetHooks(app.AppHooks{AgentEvents: events, Confirm: confirm, Log: logHook})

	debuglog.BootTrace("oneshot.app.created")
	runErr := func() error {
		st := a.ConnectMcp(ctx)
		debuglog.BootTrace("oneshot.mcp.connect.done")
		// The scheduler starts BEFORE the first backend round, and that ordering is the
		// whole feature. PromptContext derives scheduler_active from whether it is
		// running, so starting later would tell the model background work is unavailable
		// on the very round where it decides whether to start any; and asyncPreflight
		// rejects the async tools outright without a live coordinator, so a late start
		// leaves them failing for the turn that needed them.
		//
		// Skipped when the bound already expired: Coordinator.Start adopts rows and flips
		// its started flag synchronously, so on a dead context it would advertise a poll
		// loop whose goroutine exits on its first select.
		if opts.RunScheduler && ctx.Err() == nil {
			// nil attention callback, for the same reason mcp --stdio passes nil: a non-nil
			// one enables the scheduler's notifier, which marks attention-or-higher events
			// delivered as it invokes them — so a callback with nowhere to render would
			// consume exactly the async completions the durable inbox exists to hand to
			// the next session.
			a.StartScheduler(ctx, nil)
			debuglog.BootTrace("oneshot.scheduler.started")
		}
		// The session header goes out AFTER the MCP connect and BEFORE the first round.
		// After, because mcpConnected is the field that separates a real answer from one
		// produced in degraded local mode, and a header that guessed would be worse than
		// none. Before, because its whole job is to let a consumer reach the trace for a
		// run that is about to fail.
		if sink != nil {
			sink.Session(jsonout.SessionInfo{
				SessionID: a.SessionID,
				Project:   a.Config.ProjectPath,
				Tier:      string(a.Tier()),
				// SanitizeURL, not the raw config value: DAINTREE_BACKEND_URL and
				// --backend-url are never normalized, so an endpoint carrying userinfo
				// or a query token would otherwise be published verbatim on stdout and
				// straight into a CI log. It fails closed (an unparseable endpoint
				// becomes ""), which is the right trade for a diagnostic field.
				BackendURL:   mcp.SanitizeURL(a.Config.BackendURL),
				LogPath:      logPath,
				Version:      buildVersion,
				AutoApprove:  a.Config.AutoApprove,
				MCPConnected: st.Connected,
				MCPTransport: st.Transport,
			})
		}
		// After the header, so a JSONL consumer can rely on `session` being the FIRST
		// line whenever one is emitted at all.
		warnAutoApprove()
		// The non-fatal half of the pin preflight: the backend accepts pins but serves no
		// catalog, so the ids could not be checked locally. Same channel and the same
		// reason as the auto-approve notice — a condition that will quietly change what
		// the run means.
		if pinNotice != "" {
			if sink != nil {
				sink.Warn(pinNotice)
			} else {
				stderrR.Warn(pinNotice)
			}
		}
		// One prompt or a whole scripted conversation — the difference is entirely in
		// this statement. Everything above (lease, app, MCP, pins, scheduler, session
		// header) is process-scoped and already right for a run of any number of turns,
		// and everything below (the async barrier, Shutdown, the terminal line) closes
		// the PROCESS, not a turn.
		if opts.MultiTurn {
			err := runJSONTurns(ctx, a, sink, os.Stdin)
			debuglog.BootTrace("oneshot.multiturn.done")
			return err
		}
		_, err := a.Session.Send(ctx, opts.Prompt, agent.SendOptions{})
		debuglog.BootTrace("oneshot.send.done")
		return err
	}()
	if runErr != nil {
		reportError(runErr)
	}

	// waitCancelled is the console path's equivalent of Sink.CancelRun: the turn itself
	// succeeded, only the async barrier ran out of time, and the exit code below has to
	// say cancelled without a turn-level cancellation event to read it from.
	waitCancelled := false
	// Hold the run open until the async work THIS session started has settled and
	// published. Without it Shutdown — whose first act is cancelling the coordinator —
	// would tear down a scheduler that never polled, and the handles the turn just
	// handed out would be abandoned by the process that created them.
	//
	// Runs even when the turn reported a failure: a tool that already accepted work and
	// returned a handle deserves supervision regardless of how the round ended. Skipped
	// entirely when unflagged, so the default path adds no call at all.
	if opts.RunScheduler {
		if werr := a.WaitForSessionAsync(ctx); werr != nil {
			// The bound expired with work still live. Say so, and mark the RUN cancelled —
			// but at the run level only, so an answer that did complete survives into the
			// terminal line. The work itself stays durably live for the next owner.
			// Never claim a duration was SPENT waiting: the same --timeout bounds the whole
			// run, so a deadline that expired during the turn reaches the barrier already
			// dead and it returns without polling once. Name the bound, not an elapsed
			// time we did not measure.
			// "unsettled" rather than "still running": an entry stays counted until its
			// completion is PUBLISHED, so the work may well have finished and be waiting
			// only on the event that carries the outcome.
			msg := fmt.Sprintf("--timeout (%s) expired with async work unsettled; it stays live for the next session", opts.Timeout)
			if !timedOut() {
				msg = "stopped waiting for async work to settle; it stays live for the next session"
			}
			if sink != nil {
				sink.Warn(msg)
				sink.CancelRun()
			} else {
				stderrR.Warn(msg)
				waitCancelled = true
			}
		}
	}

	// Shutdown BEFORE the terminal result line; route any shutdown error off stdout.
	if serr := a.Shutdown(); serr != nil {
		if sink != nil {
			fmt.Fprintf(os.Stderr, "shutdown error: %v\n", serr)
		} else {
			stderrR.Error("shutdown error: " + serr.Error())
		}
	}

	debuglog.BootTrace("oneshot.shutdown.done")
	if sink != nil {
		return sink.Finish()
	}
	// Session.Send returns turn FAILURES as sentinel replies (not an error — the error
	// return is reserved for the single-flight guard), so a backend-down / model-error
	// turn surfaces as an Error event, not runErr. Gate the exit code on both so a failed
	// one-shot exits non-zero for scripts/CI (the JSON sink does the same via Finish()).
	if runErr != nil || cs.Failed() {
		return domain.OneShotExitCode.Error
	}
	if cs.Cancelled() || waitCancelled {
		return domain.OneShotExitCode.Cancelled
	}
	return domain.OneShotExitCode.Success
}

// RunInteractive runs the line REPL — the only interactive front end. Daintree
// renders the assistant natively over `host --stdio`; this path exists for headless
// operators (a shell, an SSH session, a piped script), not as a product surface.
func RunInteractive(ctx context.Context, opts Options) int {
	return runInteractive(ctx, opts, stdinIsTTY() && stdoutIsTTY())
}

// runInteractive is the testable core of RunInteractive. ttyOK is measured at the
// process boundary by RunInteractive; keeping it explicit here lets the TTY-only
// behaviors (schema auto-reset) be exercised without a pseudoterminal in unit tests.
func runInteractive(ctx context.Context, opts Options, ttyOK bool) int {
	r := render.Stdout()
	overrides, err := buildOverrides(opts, r)
	if err != nil {
		r.Error(err.Error())
		return domain.OneShotExitCode.Error
	}
	debuglog.BootTrace("boot.overrides.loaded")
	// Interactive launch: ensure the project's supervisor daemon exists, attach
	// (it yields ownership + receives our fresh MCP credentials), and take the
	// owner lease. Closing this assistant later hands supervision straight back
	// — the daemon keeps watchers/async/timers running and integrates results
	// with autonomous wake turns until the next attach.
	own, err := acquireOwnership(ctx, overrides, true, 60*time.Second,
		func(m string) { r.Line(r.Gray(m)) })
	if err != nil {
		r.Error(err.Error())
		return domain.OneShotExitCode.Error
	}
	defer own.Release()
	debuglog.BootTrace("boot.ownership.acquired")
	createOpts := app.CreateOptions{Overrides: overrides, PinnedSkillIDs: opts.PinnedSkillIDs}
	// A stale on-disk schema has exactly one sensible recovery for this pre-release,
	// single-baseline DB: hard-reset it. On an interactive terminal (Daintree's xterm)
	// take that automatically instead of prompting — the answer is always "yes" here, so
	// the y/N only added friction to every fresh-folder launch. A piped/non-TTY launch
	// still keeps the loud, actionable error: we never silently wipe local state in an
	// automated context.
	if ttyOK {
		createOpts.OnSchemaStale = schemaAutoReset(r)
		createOpts.OnSchemaReset = schemaResetNotice(r)
	}
	a, err := app.Create(createOpts)
	if err != nil {
		r.Error(err.Error())
		return domain.OneShotExitCode.Error
	}
	debuglog.BootTrace("boot.app.created")
	// Negotiate `--skill` BEFORE adopting, and before either front end opens. A no-op
	// without pins; with them, a failure aborts the launch rather than dropping the
	// operator into an attached session whose every turn silently ignores the runbook they named.
	//
	// Ordered ahead of AdoptAsCurrentSession deliberately. Adoption writes the project's
	// durable current-session pointer and shutdown does not put back what was there, so
	// adopting first would let a launch that never ran a turn — a mistyped `--skill` —
	// permanently displace the real conversation: the supervisor's detached wake turns
	// would resume the empty session instead of the one the user was actually having.
	pinNotice, perr := a.PreparePinnedSkills(ctx)
	if perr != nil {
		_ = a.Shutdown()
		r.Error(perr.Error())
		return domain.OneShotExitCode.Error
	}
	if pinNotice != "" {
		r.Warn(pinNotice)
	}

	// This conversation is now the project's current session — the one the
	// daemon's detached wake turns continue after we exit.
	a.AdoptAsCurrentSession()

	announceDebugLog(a)
	return startRepl(ctx, a)
}

// RunDoctor is the `doctor` subcommand: the environment gate.
//
// It builds a structured DoctorReport and renders it as either the human banner or
// `--json`. Every condition is a typed check with an id, a status, and a next action —
// see doctorreport.go for why that replaced prose.
//
// The exit code is the contract: non-zero iff something FAILED. Warnings and unknowns
// never gate, because a gate that fires on "could not check" is a gate people learn to
// ignore.
func RunDoctor(ctx context.Context, opts Options) int {
	report, err := buildDoctorReport(ctx, opts)
	if err != nil {
		if opts.JSON {
			// Even a fatal setup error answers in JSON when JSON was asked for: a caller
			// parsing stdout must never receive prose on the one path it cannot handle.
			fatal := &DoctorReport{Version: buildVersion, Platform: runtime.GOOS + "/" + runtime.GOARCH}
			fatal.Add(DoctorCheck{ID: "doctor.setup", Label: "doctor", Status: StatusFail, Detail: err.Error()})
			fatal.Finalize()
			_ = fatal.WriteJSON(os.Stdout)
		} else {
			render.Stdout().Error(err.Error())
		}
		return domain.OneShotExitCode.Error
	}

	if opts.JSON {
		if werr := report.WriteJSON(os.Stdout); werr != nil {
			render.Stdout().Error(werr.Error())
			return domain.OneShotExitCode.Error
		}
	} else {
		renderDoctorHuman(os.Stdout, report)
	}
	if !report.Summary.Healthy {
		return domain.OneShotExitCode.Error
	}
	return domain.OneShotExitCode.Success
}

// buildDoctorReport runs every check. It returns an error only when the diagnosis itself
// cannot start (config or App construction) — every other condition is a check.
func buildDoctorReport(ctx context.Context, opts Options) (*DoctorReport, error) {
	// STDERR in JSON mode. buildOverrides prints a warning when DAINTREE.md cannot be
	// read, and `doctor --json` promises stdout is a single JSON document — one
	// unreadable project file would otherwise emit prose ahead of it and break every
	// parser downstream.
	r := render.Stdout()
	if opts.JSON {
		r = render.New(os.Stderr)
	}
	overrides, err := buildOverrides(opts, r)
	if err != nil {
		return nil, err
	}

	report := &DoctorReport{
		Version:  buildVersion,
		Platform: runtime.GOOS + "/" + runtime.GOARCH,
	}

	// Environment checks first, and BEFORE the lease: they need no App, they are the ones
	// that explain a broken install, and they must still report when the DB cannot open.
	report.Add(CheckPlatform())
	report.Add(CheckBinaryOnPath(buildVersion))

	// Doctor opens the DB, so it needs the lease too (briefly, never spawning). A failure
	// here is itself a finding — "something else owns this project" is exactly what a
	// stuck user needs told — so it becomes a check rather than an abort.
	own, oerr := acquireOwnership(ctx, overrides, false, 10*time.Second, func(string) {})
	if oerr != nil {
		// Deliberately does NOT assert "another process owns it": acquiring the lease can
		// also fail on a read-only mount, a bad state path, or a permissions problem, and
		// naming the wrong cause sends the user to fix something that is not broken.
		report.Add(DoctorCheck{
			ID: "state.owner", Label: "state ownership", Status: StatusFail,
			Detail: "could not take the project's owner lease: " + oerr.Error(),
			Hint:   "Usually another assistant is open — close it, or run `daintree-assistant daemon stop`. If not, check that the state dir is writable.",
		})
		report.Finalize()
		return report, nil
	}
	defer own.Release()
	report.Add(DoctorCheck{
		ID: "state.owner", Label: "state ownership", Status: StatusOK,
		Detail: "acquired (no other assistant is using this project)",
	})

	a, err := app.Create(app.CreateOptions{Overrides: overrides})
	if err != nil {
		// state.schema, not a second id: a stale schema is the overwhelmingly common
		// reason App.Create fails, and a caller keying off the documented id must find it
		// here rather than having to know about a sibling.
		report.Add(DoctorCheck{
			ID: "state.schema", Label: "state database", Status: StatusFail,
			Detail: err.Error(),
			Hint:   "If the schema is stale, run `daintree-assistant reset project-state` (it keeps your sign-in).",
		})
		report.Finalize()
		return report, nil
	}
	defer a.Shutdown()

	report.Add(CheckStateDir(a.Config.StateDir))
	report.Add(DoctorCheck{
		ID: "state.schema", Label: "state schema", Status: StatusOK,
		Detail: fmt.Sprintf("version %d at %s", storage.SchemaVersion(), a.Config.DBPath),
		Data:   map[string]any{"version": storage.SchemaVersion(), "path": a.Config.DBPath},
	})

	a.ConnectMcp(ctx)

	// One-shot probes: a diagnostic reports the hop's CURRENT state. The patient turn-time
	// retry budget would make a plainly-dead backend take seconds per row to report
	// exactly the same thing.
	ctx = backend.WithoutRetry(ctx)
	for _, c := range backendDoctorChecks(ctx, a) {
		report.Add(c)
	}
	for _, c := range daintreeDoctorChecks(a) {
		report.Add(c)
	}
	report.Add(CheckAutoApprove(a.Config.AutoApprove, string(a.Tier())))
	report.Add(DoctorCheck{
		ID: "tools.registered", Label: "tools", Status: StatusOK,
		Detail: fmt.Sprintf("%d registered, tier '%s'", len(a.Registry.List()), a.Tier()),
		Data:   map[string]any{"count": len(a.Registry.List()), "tier": string(a.Tier())},
	})

	report.Extra = map[string]any{
		"project":     a.Config.ProjectPath,
		"stateDir":    a.Config.StateDir,
		"sessionId":   a.SessionID,
		"debugLog":    a.Config.DebugLog,
		"workflowInt": a.Config.WorkflowIntelligence,
	}
	report.Finalize()
	return report, nil
}

// backendDoctorChecks diagnoses the backend hop.
func backendDoctorChecks(ctx context.Context, a *app.App) []DoctorCheck {
	var out []DoctorCheck

	// There is deliberately no "signed in" row. The backend holds its own upstream
	// credential and serves a request that carries no Authorization header, so a CLI
	// with no key is a healthy CLI — a red row there would fire on every install.
	// A key is reported only when one is actually being sent, since it then changes
	// which account funds the turn. The value is never printed; naming the source is
	// what makes an inherited or stale value actionable.
	if a.Config.APIKey != "" {
		out = append(out, DoctorCheck{
			ID: "auth.bearer", Label: "bearer token", Status: StatusOK,
			Detail: "sent from DAINTREE_API_KEY — this key funds the turn, not the backend's",
		})
	}

	hctx, hcancel := context.WithTimeout(ctx, 3*time.Second)
	herr := a.Backend.Health(hctx)
	hcancel()
	// Sanitized for the same reason as the MCP URL: a custom backend URL arrives from the
	// trusted env or the stored sign-in, neither of which passes through
	// credentials.NormalizeBaseURL, so it can carry userinfo.
	base := mcp.SanitizeURL(a.Backend.BaseURL())
	if herr != nil {
		out = append(out, DoctorCheck{
			ID: "backend.reachable", Label: "backend", Status: StatusFail,
			Detail: base + " — UNREACHABLE: " + herr.Error(),
			Hint:   "Check your network. For a local backend, start it: cd ../assistant-backend && python -m daintree_assistant_server",
			Data:   map[string]any{"url": base},
		})
		// Everything below needs the backend; reporting each as its own failure would
		// bury the one cause under four symptoms.
		return out
	}
	out = append(out, DoctorCheck{
		ID: "backend.reachable", Label: "backend", Status: StatusOK,
		Detail: base, Data: map[string]any{"url": base},
	})

	out = append(out, verifyCredentialDoctorCheck(ctx, a, base))
	out = append(out, taskManifestDoctorCheck(ctx, a))
	return out
}

// verifyCredentialDoctorCheck asks whether the backend can actually spend a credential —
// which nothing else here can tell you, since /health and /readyz answer for the process
// and every other row stays green while the upstream account is dead.
//
// It reports on whichever key THIS request would spend: ours if DAINTREE_API_KEY named
// one, the backend's own otherwise, which is every normal install. That makes it a
// backend-health row far more often than a "your key" row, and the wording below has to
// hold for both readings.
func verifyCredentialDoctorCheck(ctx context.Context, a *app.App, base string) DoctorCheck {
	c := DoctorCheck{ID: "auth.credentialUsable", Label: "upstream credential"}
	vctx, vcancel := context.WithTimeout(ctx, 3*time.Second)
	ver, verr := a.Backend.VerifyKey(vctx)
	vcancel()

	switch {
	case errors.Is(verr, backend.ErrVerifyUnsupported):
		// A remote endpoint that cannot answer this is out of date or intercepted; a
		// loopback one is routinely a work-in-progress backend, so the gap is benign.
		if !backend.AllowsUnverifiedSignIn(base) {
			c.Status = StatusFail
			c.Detail = "this backend does not serve /v1/daintree/auth/verify"
			c.Hint = "The endpoint is out of date or a proxy is intercepting it. Retry off any proxy, or use a local backend."
			return c
		}
		c.Status = StatusUnknown
		c.Detail = "this local backend can't check"
		return c
	case isBackendAuthError(verr):
		// A 401 at OUR door is a definite answer, not a failed check: this deployment
		// will not serve this CLI, so every turn fails. Reporting it as `unknown` would
		// leave doctor concluding "no blocking problems" for an install that cannot run
		// at all — and the whole point of this row is to say so before a turn does.
		c.Status = StatusFail
		c.Detail = "this backend rejected the request outright — " + verr.Error()
		if a.Config.APIKey != "" {
			c.Hint = "DAINTREE_API_KEY is set and this backend refused it. Unset it, or correct it."
		} else {
			c.Hint = "This CLI sends no API key. The endpoint still requires one, so it is older than this build — point DAINTREE_BACKEND_URL at a current backend."
		}
		return c
	case verr != nil:
		c.Status = StatusUnknown
		c.Detail = "could not check — " + verr.Error()
		return c
	case !ver.Valid:
		c.Status = StatusFail
		c.Detail = "the provider rejected this credential: " + ver.Detail
		c.Hint = credentialFixHint(a.Config.APIKey)
		return c
	case !ver.IsUsable():
		// Recognised but empty fails every turn just as surely as a wrong credential —
		// but the fix is topping up, not replacing it, so it is its own state.
		// IsUsable, not a bare LimitRemaining test: it honours the backend's own
		// `usable` verdict and treats "not reported" as fine, which is what an
		// unlimited or pay-as-you-go account looks like.
		c.Status = StatusFail
		c.Detail = "the credential is valid but has NO CREDIT remaining"
		c.Hint = "Top up the account — every turn will fail until you do."
		c.Data = map[string]any{"limitRemaining": *ver.LimitRemaining}
		return c
	}
	c.Status = StatusOK
	c.Detail = "usable" + credentialOwnerSuffix(a.Config.APIKey)
	if ver.Label != "" {
		// The provider's own safe label, when it offers one. Cerebras does not — its
		// probe answers with a model listing — so this is normally absent rather than
		// empty-looking, and the row must read correctly without it.
		c.Detail += " · " + ver.Label
	}
	if ver.LimitRemaining != nil {
		c.Data = map[string]any{"limitRemaining": *ver.LimitRemaining}
	}
	return c
}

// isBackendAuthError reports a 401/403 raised at the BACKEND's own door (not the
// provider's). Distinct from every other transport failure because it is a verdict
// rather than a gap: the request was understood and refused.
func isBackendAuthError(err error) bool {
	var berr *backend.Error
	return errors.As(err, &berr) && berr.IsAuth()
}

// credentialOwnerSuffix names WHOSE credential the row just reported on. Without it the
// same word means two very different things — "the backend's account is funded" versus
// "the key you exported is funded" — and the reader cannot tell which from the endpoint.
func credentialOwnerSuffix(callerKey string) string {
	if callerKey != "" {
		return " (yours, from DAINTREE_API_KEY)"
	}
	return " (the backend's own)"
}

// credentialFixHint routes a rejection to whoever can actually fix it. A caller-supplied
// key is the user's to correct; the backend's own is not, and telling them to re-paste
// something they never pasted sends them looking for a setting that does not exist.
func credentialFixHint(callerKey string) string {
	if callerKey != "" {
		return "DAINTREE_API_KEY names a credential the provider will not accept — unset it to fall back to the backend's own."
	}
	return "The backend's own upstream credential is rejected — this is a backend-side problem, not yours."
}

// taskManifestDoctorCheck compares the task ids this CLI sends against what the backend
// advertises. Drift is GATING: every id is one the CLI will actually send, so a missing
// one is a guaranteed 404 mid-turn (the 2026-07-07 de-versioning incident, which a
// count-only check could not see).
func taskManifestDoctorCheck(ctx context.Context, a *app.App) DoctorCheck {
	c := DoctorCheck{ID: "backend.tasks", Label: "backend tasks"}
	cctx, ccancel := context.WithTimeout(ctx, 3*time.Second)
	caps, cerr := a.Backend.Capabilities(cctx)
	ccancel()

	if cerr != nil {
		// A capabilities FETCH error is not drift: /v1/daintree/capabilities sits behind
		// require_ready, so a warming backend legitimately yields nothing.
		c.Status = StatusUnknown
		c.Detail = "cannot verify — " + cerr.Error()
		return c
	}
	av := backend.CheckTasks(caps, a.Config.WorkflowIntelligence)
	switch {
	case !av.Reported:
		c.Status = StatusFail
		c.Detail = "the backend advertises NO tasks — every utility task call will fail"
		c.Hint = "The backend is misconfigured or mid-deploy. Check its /v1/daintree/capabilities."
		return c
	case av.OK():
		c.Status = StatusOK
		c.Detail = fmt.Sprintf("all %d required tasks present", av.Required)
		c.Data = map[string]any{"required": av.Required}
		return c
	}
	c.Status = StatusFail
	c.Detail = fmt.Sprintf("DRIFT — %d of %d missing: %s", len(av.Missing), av.Required, strings.Join(av.Missing, ", "))
	c.Hint = "This CLI and the backend disagree about task ids. Update whichever is older."
	c.Data = map[string]any{"missing": av.Missing, "required": av.Required}
	return c
}

// daintreeDoctorChecks diagnoses the Daintree MCP connection and the project binding.
func daintreeDoctorChecks(a *app.App) []DoctorCheck {
	var out []DoctorCheck

	st := a.MCP.Status()
	// SANITIZED, always. Daintree's per-session MCP URL carries its bearer as
	// ?session=<token> (see mcp.SanitizeURL), and this value goes into `doctor --json`
	// and straight into a support bundle — i.e. into a file a tester is being encouraged
	// to send to someone else. The generic redactor cannot save us here: an opaque query
	// token matches no shape, and the field name "url" is not credential-marked, so it
	// would sail through both passes. Endpoints get stripped at the source, never trusted
	// to a downstream scrubber.
	mcpURL := mcp.SanitizeURL(a.Config.McpURL)
	c := DoctorCheck{ID: "mcp.daintree", Label: "Daintree MCP", Data: map[string]any{"url": mcpURL, "connected": st.Connected}}
	switch {
	case a.Config.Offline:
		// Explicitly asked for. Reporting a deliberate choice as a failure would make
		// every offline test run look broken.
		c.Status = StatusSkip
		c.Detail = "offline mode — Daintree MCP not attempted"
	case a.Config.McpURL == "":
		// Not a failure — degraded local mode is a supported way to run — but the
		// assistant cannot do its actual job, so it must not read as healthy either.
		c.Status = StatusWarn
		c.Detail = "not configured — DEGRADED LOCAL MODE"
		c.Hint = "Launch from inside Daintree, or pass --mcp-url/--mcp-token. Without it there are no terminals, agents or worktrees."
	case st.Connected:
		count := 0
		if st.ToolCount != nil {
			count = *st.ToolCount
		}
		c.Status = StatusOK
		c.Detail = fmt.Sprintf("connected (%s, %d tools)", st.Transport, count)
		c.Data["transport"], c.Data["toolCount"] = st.Transport, count
	default:
		c.Status = StatusFail
		c.Detail = "configured but NOT connected: " + st.Error
		c.Hint = "Daintree may have closed or revoked this session. Reopen the assistant from Daintree, or use /reconnect."
	}
	out = append(out, c)

	p := DoctorCheck{ID: "project.instructions", Label: "project", Data: map[string]any{"path": a.Config.ProjectPath}}
	p.Status = StatusOK
	if a.Config.ProjectInstructions != "" {
		p.Detail = fmt.Sprintf("%s (DAINTREE.md, %d bytes)", a.Config.ProjectPath, len(a.Config.ProjectInstructions))
	} else {
		p.Detail = a.Config.ProjectPath + " (no DAINTREE.md)"
	}
	out = append(out, p)
	return out
}

// announceDebugLog opens the log and prints a gray notice when active.
func announceDebugLog(a *app.App) {
	path := debuglog.StartDebugLog(debuglog.Config{DebugLog: a.Config.DebugLog, LogDir: a.Config.LogDir},
		map[string]any{"sessionId": a.SessionID, "project": a.Config.ProjectPath})
	if path != "" {
		r := render.Stdout()
		r.Line(r.Gray("logging to " + path))
	}
}

func stdinIsTTY() bool  { return isatty.IsTerminal(os.Stdin.Fd()) }
func stdoutIsTTY() bool { return isatty.IsTerminal(os.Stdout.Fd()) }

// schemaAutoReset returns the app.Create OnSchemaStale handler for the interactive
// terminal. Given what the Daintree Assistant is — a local operations officer whose
// SQLite state is a single clean pre-release baseline that we hard-reset (never migrate)
// on a schema bump — a stale on-disk DB has exactly one sensible recovery, so we take it
// automatically rather than block every fresh-folder launch on a y/N whose answer is
// always "yes". It prints one concise notice (local state reset, previous state kept as
// a backup; code + Daintree untouched) and authorises the rebuild — app.Create then
// moves the old DB aside (never deletes it) and reports the backup path through the
// OnSchemaReset handler (schemaResetNotice). Wired only when stdin/stdout are TTYs
// (see RunInteractive), so a piped/non-TTY launch still keeps the loud, actionable
// stale-schema error rather than silently destroying local state in an automated context.
func schemaAutoReset(r *render.Renderer) func(have, want int) (bool, error) {
	return func(have, want int) (bool, error) {
		r.Line(r.Gray(fmt.Sprintf(
			"Local assistant database was from an older version (schema %d → %d) — resetting local state; your code and Daintree are untouched.",
			have, want)))
		return true, nil
	}
}

// schemaResetNotice returns the app.Create OnSchemaReset handler paired with
// schemaAutoReset: once the stale database has been safely moved aside it names
// the backup path, so the reset never reads as silent data loss — the previous
// state (timers, watchers, memories, history) is right there on disk.
func schemaResetNotice(r *render.Renderer) func(backupPath string) {
	return func(backupPath string) {
		if backupPath == "" {
			return
		}
		r.Line(r.Gray("Previous state backed up to " + backupPath))
	}
}
