package cli

import (
	"bufio"
	"context"
	"os"
	"strings"

	"github.com/daintreehq/daintree-assistant/internal/backend"
	"github.com/daintreehq/daintree-assistant/internal/cli/render"
	"github.com/daintreehq/daintree-assistant/internal/config"
)

// lineReader is the process-wide cooked-mode stdin line source shared by the
// classic REPL and the login prompts. It is DEMAND-DRIVEN: the goroutine reads
// os.Stdin only while a request is outstanding, so between requests (e.g. while
// the Bubble Tea cockpit owns the terminal in raw mode) nothing competes for
// stdin bytes — a continuously-reading goroutine would steal cockpit keystrokes,
// and two bufio readers racing the same fd would steal each other's lines across
// a /login restart. Not safe for concurrent use; the REPL loop and the login
// flow are strictly sequential surfaces.
type lineReader struct {
	req  chan struct{}
	resp chan lineResult
	// outstanding tracks a request whose response was abandoned by a context
	// cancellation; the next Read collects that response instead of issuing a
	// second request (the goroutine holds at most one read at a time).
	outstanding bool
}

type lineResult struct {
	line string
	err  error
}

func newLineReader() *lineReader {
	lr := &lineReader{req: make(chan struct{}), resp: make(chan lineResult, 1)}
	// Capture os.Stdin synchronously: the goroutine reading the package-level
	// var races with tests that swap it back in t.Cleanup (caught by -race), and
	// bufio.NewReader performs no I/O, so eager construction keeps the reads
	// themselves demand-driven.
	reader := bufio.NewReader(os.Stdin)
	go func() {
		for range lr.req {
			// EOF is deliberately NOT latched: on a TTY, Ctrl-D at an empty prompt
			// yields io.EOF for that one read without closing the terminal — a user
			// who Ctrl-D's past the first-run gate must still be able to run /login
			// later. A genuinely closed pipe simply keeps answering EOF per read.
			line, err := reader.ReadString('\n')
			lr.resp <- lineResult{line: line, err: err}
		}
	}()
	return lr
}

// Read blocks for the next stdin line, trimmed. ok=false on EOF / closed stdin
// or context cancellation; an intentionally empty submission is ok=true, "".
func (lr *lineReader) Read(ctx context.Context) (string, bool) {
	if !lr.outstanding {
		select {
		case lr.req <- struct{}{}:
			lr.outstanding = true
		case <-ctx.Done():
			return "", false
		}
	}
	select {
	case <-ctx.Done():
		return "", false
	case result := <-lr.resp:
		lr.outstanding = false
		trimmed := strings.TrimSpace(result.line)
		if result.err != nil && trimmed == "" {
			return "", false
		}
		return trimmed, true
	}
}

// backendLoginNeeded reports whether the interactive first-run gate should ask
// the user to log in: no DAINTREE_BACKEND_URL escape hatch (the dev/test/e2e
// override, which must never hit a prompt) and no complete persisted login. A
// malformed credential file counts as "needed" — completing the flow repairs it.
func backendLoginNeeded() bool {
	if strings.TrimSpace(os.Getenv("DAINTREE_BACKEND_URL")) != "" {
		return false
	}
	_, ok, _ := config.LoadCredentials(config.DefaultCredentialsPath())
	return !ok
}

// runLoginFlow runs the two-question login — endpoint (default or custom), then
// API key — and persists both to path. read is the blocking line source
// (lineReader.Read in production, a script in tests). Returns true when
// credentials were saved; EOF or cancellation at any prompt aborts and leaves
// any existing file untouched. Answers are never logged: the key is a secret,
// and this flow deliberately stays off the debug/session log surfaces.
func runLoginFlow(ctx context.Context, r *render.Renderer, read func(context.Context) (string, bool), path string) bool {
	cancelled := func() bool {
		r.Warn("login cancelled — nothing saved")
		return false
	}
	r.Line("")
	r.Line(r.Bold("Daintree Assistant — login"))
	if _, _, err := config.LoadCredentials(path); err != nil {
		// A corrupt file is repaired by completing the flow (the save replaces it).
		r.Warn("existing credentials could not be read (" + err.Error() + ") — completing login will replace them")
	}
	r.Line("Backend endpoint:")
	r.Line("     " + r.Bold("1.") + " default — " + backend.DefaultBaseURL + r.Gray("  (Enter)"))
	r.Line("     " + r.Bold("2.") + " custom URL")

	var endpoint string
	for endpoint == "" {
		r.Out(r.Yellow("   endpoint [1/2] (Enter for default): "))
		answer, ok := read(ctx)
		if !ok {
			return cancelled()
		}
		switch strings.ToLower(answer) {
		case "", "1", "default":
			endpoint = backend.DefaultBaseURL
		case "2", "custom":
			for endpoint == "" {
				r.Out(r.Yellow("   backend URL: "))
				answer, ok := read(ctx)
				if !ok {
					return cancelled()
				}
				norm, err := config.NormalizeEndpoint(answer)
				if err != nil {
					r.Warn("   " + err.Error())
					continue
				}
				endpoint = norm
			}
		default:
			// Pasting the URL straight at the first question also works.
			norm, err := config.NormalizeEndpoint(answer)
			if err != nil {
				r.Warn("   Please answer 1, 2, or paste a full http(s) URL.")
				continue
			}
			endpoint = norm
		}
	}

	// The key is opaque to the CLI (today an OpenRouter key, later a subscription
	// key) — only obvious paste accidents are rejected, never a format.
	var key string
	for key == "" {
		r.Out(r.Yellow("   API key: "))
		answer, ok := read(ctx)
		if !ok {
			return cancelled()
		}
		if answer == "" || strings.ContainsAny(answer, " \t") {
			r.Warn("   The API key must be a single non-empty token — paste it exactly.")
			continue
		}
		key = answer
	}

	if err := config.SaveCredentials(path, config.Credentials{Endpoint: endpoint, APIKey: key}); err != nil {
		r.Error("could not save credentials: " + err.Error())
		return false
	}
	r.Success("logged in — endpoint " + endpoint + " (credentials saved to " + path + ")")
	if strings.TrimSpace(os.Getenv("DAINTREE_BACKEND_URL")) != "" {
		r.Warn("DAINTREE_BACKEND_URL is set in this environment and still overrides the saved endpoint.")
	}
	return true
}
