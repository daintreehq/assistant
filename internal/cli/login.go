package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/config"
	"github.com/daintreehq/assistant/internal/credentials"
	"github.com/daintreehq/assistant/internal/domain"
	"golang.org/x/term"
)

// The login flow runs OUTSIDE the Bubble Tea program, as plain terminal I/O —
// deliberately, and for the same reason the boot splash does: it must work before the
// cockpit exists (app.Create needs the key to build the backend client), and it must
// work identically in the classic REPL, in a one-shot run, and from the `login`
// subcommand. A cockpit-native modal would serve exactly one of those four.

// loginValidateTimeout bounds the capability probe that confirms a sign-in. Generous
// enough for a cold Cloud Run instance (the deployed backend scales to zero), short
// enough that a wrong URL fails while the user is still watching.
const loginValidateTimeout = 30 * time.Second

// LoginIO is the terminal the login flow talks to. The zero value is unusable; build
// one with StdLoginIO (real terminal) or ScriptedLoginIO (tests, piped stdin).
//
// Both reads are FUNCTIONS over one shared stream rather than an io.Reader the flow
// wraps itself. That is the fix for a real bug: a bufio.Scanner over stdin reads ahead,
// so a separate secret reader on the same fd found the key line already swallowed and
// looped forever prompting for it.
type LoginIO struct {
	Out io.Writer
	// ReadLine reads one trimmed answer line. It MUST return an error at EOF — a
	// cancelled or exhausted stdin that reports "" instead silently takes defaults and,
	// on a required field, spins.
	ReadLine func() (string, error)
	// ReadSecret reads one secret line WITHOUT echoing it, with the same EOF contract.
	ReadSecret func() (string, error)
}

// errLoginEOF ends the flow when input runs out.
var errLoginEOF = errors.New("sign-in cancelled (no more input)")

// StdLoginIO builds the real terminal I/O over stdin. When stdin is a TTY the key is
// read with echo off; otherwise both reads come off the SAME buffered reader so
// `printf '1\n$KEY\n' | daintree-assistant login` works for scripted sign-in.
func StdLoginIO() LoginIO {
	in := bufio.NewReader(os.Stdin)
	readLine := func() (string, error) {
		line, err := in.ReadString('\n')
		if err != nil {
			// A final line without a trailing newline is still an answer; only a truly
			// empty read at EOF ends the flow.
			if errors.Is(err, io.EOF) && strings.TrimSpace(line) != "" {
				return strings.TrimSpace(line), nil
			}
			if errors.Is(err, io.EOF) {
				return "", errLoginEOF
			}
			return "", err
		}
		return strings.TrimSpace(line), nil
	}
	return LoginIO{
		Out:      os.Stdout,
		ReadLine: readLine,
		ReadSecret: func() (string, error) {
			fd := int(os.Stdin.Fd())
			if !term.IsTerminal(fd) {
				return readLine()
			}
			raw, err := term.ReadPassword(fd)
			// ReadPassword swallows the user's Enter, so the cursor is still parked at
			// the end of the prompt line — emit the newline ourselves or the next line
			// of output overwrites the prompt.
			fmt.Fprintln(os.Stdout)
			if err != nil {
				return "", err
			}
			return strings.TrimSpace(string(raw)), nil
		},
	}
}

// ScriptedLoginIO drives the flow from a canned stream — piped input and tests. The
// secret is read from the SAME stream as the answers, in order, exactly as StdLoginIO
// does for a pipe.
func ScriptedLoginIO(in io.Reader, out io.Writer) LoginIO {
	r := bufio.NewReader(in)
	readLine := func() (string, error) {
		line, err := r.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) && strings.TrimSpace(line) != "" {
				return strings.TrimSpace(line), nil
			}
			if errors.Is(err, io.EOF) {
				return "", errLoginEOF
			}
			return "", err
		}
		return strings.TrimSpace(line), nil
	}
	return LoginIO{Out: out, ReadLine: readLine, ReadSecret: readLine}
}

// RunLogin runs the interactive sign-in and persists the result to
// cfg.CredentialsPath. It returns the saved credentials so a caller mid-startup can
// use them without re-reading the file.
//
// Re-running it is the supported way to CHANGE endpoint or key: the current sign-in
// is shown and pre-selected, and an empty key keeps the existing one.
func RunLogin(ctx context.Context, cfg config.AppConfig, tio LoginIO) (credentials.Credentials, error) {
	if tio.Out == nil || tio.ReadLine == nil || tio.ReadSecret == nil {
		return credentials.Credentials{}, errors.New("login: incomplete terminal I/O")
	}

	current, signedIn, err := credentials.Load(cfg.CredentialsPath)
	if err != nil {
		// A corrupt file must not block a fresh login — that is precisely the moment
		// the user is trying to fix it. Say so, then continue as signed out.
		fmt.Fprintf(tio.Out, "warning: %v\n", err)
		current, signedIn = credentials.Credentials{}, false
	}

	fmt.Fprintln(tio.Out)
	fmt.Fprintln(tio.Out, "Daintree Assistant — sign in")
	fmt.Fprintln(tio.Out)
	if signedIn {
		fmt.Fprintf(tio.Out, "  currently: %s  (key %s)\n\n", current.BaseURL, credentials.Redact(current.APIKey))
	}

	baseURL, err := promptEndpoint(tio, current.BaseURL)
	if err != nil {
		return credentials.Credentials{}, err
	}

	// Restate the RESOLVED endpoint before asking for the key, and say what the key is.
	// The endpoint matters because a custom URL is normalized on the way through
	// (scheme added, trailing slash dropped), so what was typed is not necessarily what
	// the key is about to be sent to — and the key is spendable, so "which host am I
	// handing this to" is the question to answer before it is typed, not after.
	fmt.Fprintf(tio.Out, "\nSigning in to %s\n", baseURL)
	fmt.Fprintf(tio.Out, "Use your own OpenRouter API key (sk-or-v1-…). %s\n\n", backend.KeyPurposeNotice)

	apiKey, err := promptAPIKey(tio, current.APIKey)
	if err != nil {
		return credentials.Credentials{}, err
	}

	next := credentials.Credentials{BaseURL: baseURL, APIKey: apiKey}
	if err := verifySignIn(ctx, tio, next); err != nil {
		return credentials.Credentials{}, err
	}
	if err := credentials.Save(cfg.CredentialsPath, next); err != nil {
		return credentials.Credentials{}, err
	}
	fmt.Fprintf(tio.Out, "Signed in to %s\n", next.BaseURL)
	fmt.Fprintf(tio.Out, "Saved to %s\n\n", cfg.CredentialsPath)
	// The env override silently wins over what we just saved, so a user who has one
	// exported would otherwise sign in "successfully" and still hit a different
	// backend on the next turn with no explanation.
	if envURL := strings.TrimSpace(os.Getenv("DAINTREE_BACKEND_URL")); envURL != "" && envURL != next.BaseURL {
		fmt.Fprintf(tio.Out, "note: DAINTREE_BACKEND_URL=%s is set and overrides this endpoint.\n\n", envURL)
	}
	if strings.TrimSpace(os.Getenv("DAINTREE_API_KEY")) != "" {
		fmt.Fprintln(tio.Out, "note: DAINTREE_API_KEY is set and overrides the key you just saved.")
		fmt.Fprintln(tio.Out)
	}
	return next, nil
}

// promptEndpoint renders the endpoint menu and returns the chosen base URL. The
// default is the current endpoint when signed in, else the official one.
func promptEndpoint(tio LoginIO, currentURL string) (string, error) {
	def := 1
	for i, c := range backend.EndpointChoices {
		if c.URL != "" && c.URL == currentURL {
			def = i + 1
		}
	}
	// A custom URL already in use is not in the menu, so default to "Custom" and
	// offer it back as that prompt's default rather than silently dropping it.
	custom := strings.TrimSpace(currentURL)
	for _, c := range backend.EndpointChoices {
		if c.URL == custom {
			custom = ""
		}
	}
	if custom != "" {
		def = 2
	}

	fmt.Fprintln(tio.Out, "Which backend should the assistant use?")
	for i, c := range backend.EndpointChoices {
		target := c.URL
		if target == "" {
			target = c.Note
		}
		fmt.Fprintf(tio.Out, "  %d) %-8s %s\n", i+1, c.Label, target)
	}
	fmt.Fprintln(tio.Out)

	for {
		fmt.Fprintf(tio.Out, "Endpoint [%d]: ", def)
		answer, err := tio.ReadLine()
		if err != nil {
			return "", err
		}
		idx := def
		if answer != "" {
			n, convErr := parseChoice(answer, len(backend.EndpointChoices))
			if convErr != nil {
				fmt.Fprintf(tio.Out, "  %v\n", convErr)
				continue
			}
			idx = n
		}
		chosen := backend.EndpointChoices[idx-1]
		if chosen.URL != "" {
			return chosen.URL, nil
		}
		return promptCustomURL(tio, custom)
	}
}

// promptCustomURL reads and normalizes a user-supplied backend URL.
func promptCustomURL(tio LoginIO, def string) (string, error) {
	for {
		if def != "" {
			fmt.Fprintf(tio.Out, "Backend URL [%s]: ", def)
		} else {
			fmt.Fprint(tio.Out, "Backend URL (e.g. http://127.0.0.1:8473): ")
		}
		answer, err := tio.ReadLine()
		if err != nil {
			return "", err
		}
		if answer == "" {
			if def == "" {
				fmt.Fprintln(tio.Out, "  a URL is required")
				continue
			}
			answer = def
		}
		normalized, err := credentials.NormalizeBaseURL(answer)
		if err != nil {
			fmt.Fprintf(tio.Out, "  %v\n", err)
			continue
		}
		return normalized, nil
	}
}

// promptAPIKey reads the caller's API key without echo. When a key is already stored,
// an empty answer keeps it — so changing only the endpoint never means re-typing it.
func promptAPIKey(tio LoginIO, currentKey string) (string, error) {
	// Validate the DEFAULT before offering it. credentials.Load only checks that the
	// fields are non-empty, so a hand-edited file (or an env-supplied key) can hold a
	// value that cannot ride an HTTP header at all — and blank Enter would then send it
	// to the network and fail with a transport-shaped error that says nothing about the
	// real cause. Dropping the offer here turns that into the shape message, at the
	// prompt, with a working way forward; keeping the offer would let blank Enter loop
	// on a default that can never succeed.
	if currentKey != "" {
		if err := credentials.ValidateKeyShape(currentKey); err != nil {
			fmt.Fprintf(tio.Out, "  the stored key is unusable (%v) — enter a new one\n", err)
			currentKey = ""
		}
	}
	for {
		if currentKey != "" {
			// Spell the default out rather than leaning on the bracket convention:
			// this is the prompt that makes switching endpoints cheap, and a reader who
			// misses it re-pastes a secret they cannot see (or believes the sign-in
			// discarded the stored key).
			fmt.Fprintf(tio.Out, "API key (press Enter to keep %s): ", credentials.Redact(currentKey))
		} else {
			fmt.Fprint(tio.Out, "API key: ")
		}
		key, err := tio.ReadSecret()
		if err != nil {
			// EOF here is a cancelled sign-in, NOT an empty answer to retry. Returning
			// "" and looping is what made a piped `login` spin forever printing
			// "a key is required" — the read can never succeed once input is gone.
			if errors.Is(err, errLoginEOF) {
				return "", err
			}
			return "", fmt.Errorf("read API key: %w", err)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			if currentKey != "" {
				return currentKey, nil
			}
			fmt.Fprintln(tio.Out, "  a key is required")
			continue
		}
		if err := credentials.ValidateKeyShape(key); err != nil {
			fmt.Fprintf(tio.Out, "  %v\n", err)
			continue
		}
		return key, nil
	}
}

// verifySignIn proves the endpoint is reachable AND accepts the key, by calling an
// AUTHENTICATED endpoint (/v1/daintree/capabilities). A health probe would not do:
// /healthz is unauthenticated, so it stays green with a completely bogus key and the
// failure would surface later, mid-turn.
//
// Note what each half proves. Capabilities only checks the key's SHAPE — the backend
// holds no upstream credential of its own — so on its own a pass means just "endpoint
// reachable, key well-formed". `backend.CheckSignIn` therefore follows it with
// /v1/daintree/auth/verify, which asks the PROVIDER and is the only check that catches
// a key that is well-formed but wrong, revoked, or unfunded. A remote backend that does
// not serve that route fails sign-in (backend.ErrBackendIncompatible); loopback keeps
// the lenient warning path for local development. A key the provider rejects only after
// sign-in still surfaces mid-turn as 502 upstream_error (backend.Error.IsUpstreamAuth),
// which is why that code carries its own message.
func verifySignIn(ctx context.Context, tio LoginIO, c credentials.Credentials) error {
	fmt.Fprintf(tio.Out, "\nChecking %s … ", c.BaseURL)

	client := backend.NewClient(backend.ClientConfig{
		BaseURL: c.BaseURL,
		APIKey:  c.APIKey,
		ClientInfo: backend.ClientInfo{
			Name:     "daintree-cli",
			Platform: runtime.GOOS,
		},
		// One attempt: this is an interactive check and the user is watching. The
		// default policy settles into a 10–15s poll across 10 attempts, which would
		// leave a wrong URL spinning for minutes before saying so.
		Retry: backend.RetryPolicy{MaxAttempts: 1},
	})

	cctx, cancel := context.WithTimeout(ctx, loginValidateTimeout)
	defer cancel()

	v, warning, err := backend.CheckSignIn(cctx, client)
	if err != nil {
		fmt.Fprintln(tio.Out, "failed")
		return loginCheckError(c.BaseURL, err, c.APIKey)
	}
	warning = backend.ScrubKey(warning, c.APIKey)
	fmt.Fprintln(tio.Out, "ok")
	if warning != "" {
		fmt.Fprintf(tio.Out, "  note: %s\n", warning)
	} else {
		fmt.Fprintf(tio.Out, "  key accepted by the provider%s\n", keyLabelSuffix(v, c.APIKey))
	}
	fmt.Fprintln(tio.Out)
	return nil
}

// keyLabelSuffix renders the provider's own metadata about the key, when it offers any.
// The label is what tells a user they pasted the RIGHT key rather than merely a working
// one; remaining credit turns "signed in but every turn fails" into a warning up front.
func keyLabelSuffix(v backend.KeyVerification, key string) string {
	var parts []string
	if label := safeKeyLabel(v.Label, key); label != "" {
		parts = append(parts, label)
	}
	if v.LimitRemaining != nil {
		parts = append(parts, fmt.Sprintf("%.2f remaining", *v.LimitRemaining))
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, " · ") + ")"
}

// safeKeyLabel returns the provider's own name for the key, or "" when that "name" is
// really a piece of the key.
//
// OpenRouter defaults a key's label to a TRUNCATED form of the key itself
// ("sk-or-v1-abc…c99"). ScrubKey only catches the complete bearer, so without this the
// login and doctor output would print a partial secret — and leave a durable fingerprint
// of it in the host's native scrollback, which the cockpit never clears.
//
// The test is deliberately blunt rather than clever: if any run of the label long enough
// to matter also appears in the key, it is treated as key material and dropped. A
// genuinely user-chosen label ("work laptop") shares no such run, and the cost of a false
// positive is one missing confirmation line — against the cost of a false negative, which
// is a secret on screen.
func safeKeyLabel(label, key string) string {
	label = strings.TrimSpace(label)
	key = strings.TrimSpace(key)
	if label == "" || key == "" {
		return label
	}
	// Split on the ellipsis the provider uses, and on nothing else: each side is then a
	// literal prefix/suffix of the key if this is a truncated rendering of it.
	for _, part := range strings.FieldsFunc(label, func(r rune) bool {
		return r == '…' || r == '.' || r == '*' || r == ' '
	}) {
		if len(part) >= minKeyFragmentRunes && strings.Contains(key, part) {
			return ""
		}
	}
	return label
}

// minKeyFragmentRunes is how much overlap with the key makes a label suspect. Low enough
// to catch a provider's truncated rendering, high enough that a short common word in a
// hand-written label cannot coincidentally appear inside a key.
const minKeyFragmentRunes = 6

// loginCheckError turns a failed verification into a message that names the likely
// cause and the next action, rather than surfacing the raw transport error.
func loginCheckError(baseURL string, err error, key string) error {
	// A definite provider verdict is the actionable case: say so plainly rather than
	// wrapping it in "could not verify <url>", which reads as a connectivity problem.
	// When the verdict names WHICH account problem it was, that wins — "check it is
	// active and funded" covers all three and helps with none, and a rejection carrying
	// a precise reason must not be re-generalised on the way to the user.
	if errors.Is(err, backend.ErrKeyRejected) {
		var berr *backend.Error
		if errors.As(err, &berr) && berr.ProviderAccountReason() != "" {
			return fmt.Errorf("%s did not accept this key: the provider %s", baseURL, berr.ProviderAccountReason())
		}
		return fmt.Errorf("%s did not accept this key: %v — check it is active and funded", baseURL, err)
	}
	// Nothing wrong with the key: the endpoint is missing a capability the release
	// contract requires. Re-pasting will not help, so say so and point at the fix.
	if errors.Is(err, backend.ErrBackendIncompatible) {
		// Deliberately NOT "your key is fine" — we never got a verdict on the key, and
		// asserting one we do not have is exactly the failure this gate exists to stop.
		// What IS certain is that re-pasting cannot fix an endpoint problem.
		return fmt.Errorf("%s: %v\n  This is an endpoint problem, not a verdict about your key — re-pasting it will\n  not help. Retry off any proxy/VPN, or point at a backend you run yourself\n  (`login` → Local) while this is fixed.",
			baseURL, backend.ErrBackendIncompatible)
	}
	var berr *backend.Error
	if errors.As(err, &berr) {
		switch {
		case berr.IsAuth():
			return fmt.Errorf("%s rejected the key (%s) — check you pasted it in full", baseURL, berr.Code)
		// Named account problems first: the generic line below covers all three and
		// helps with none, and the three fixes are mutually exclusive — telling someone
		// whose balance ran out that their key was rejected gets a good key replaced.
		case berr.ProviderAccountReason() != "":
			return fmt.Errorf("%s accepted the key, but the provider %s", baseURL, berr.ProviderAccountReason())
		case berr.IsUpstreamAuth():
			return fmt.Errorf("%s accepted the key but the upstream provider rejected it — check the key is active and funded", baseURL)
		case berr.IsConnect():
			return fmt.Errorf("could not reach %s — is it running, and is the URL right?\n  (%v)", baseURL, err)
		case berr.HTTPStatus == 404:
			return fmt.Errorf("%s answered, but not as a Daintree backend (404) — check the URL", baseURL)
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s did not answer within %s", baseURL, loginValidateTimeout)
	}
	// Scrub the key from any backend-controlled text before it reaches the terminal.
	return fmt.Errorf("could not verify %s: %s", baseURL, backend.ScrubKey(err.Error(), key))
}

// RunLoginCommand is the `daintree-assistant login` entry point: resolve config,
// run the flow, report. Deliberately does NOT take the owner lease or build an App —
// signing in must work while a cockpit or the supervisor daemon holds the project.
func RunLoginCommand(ctx context.Context, opts Options) int {
	cfg, err := config.LoadConfig(overridesFromOptions(opts))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return domain.OneShotExitCode.Error
	}
	if _, err := RunLogin(ctx, cfg, StdLoginIO()); err != nil {
		fmt.Fprintf(os.Stderr, "\nSign-in failed: %v\n", err)
		return domain.OneShotExitCode.Error
	}
	return domain.OneShotExitCode.Success
}

// RunLogoutCommand is the `daintree-assistant logout` entry point.
func RunLogoutCommand(_ context.Context, opts Options) int {
	cfg, err := config.LoadConfig(overridesFromOptions(opts))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return domain.OneShotExitCode.Error
	}
	if err := RunLogout(cfg, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return domain.OneShotExitCode.Error
	}
	return domain.OneShotExitCode.Success
}

// RunLogout deletes the stored sign-in.
func RunLogout(cfg config.AppConfig, out io.Writer) error {
	_, signedIn, _ := credentials.Load(cfg.CredentialsPath)
	if err := credentials.Delete(cfg.CredentialsPath); err != nil {
		return err
	}
	if !signedIn {
		fmt.Fprintln(out, "Not signed in.")
		return nil
	}
	fmt.Fprintf(out, "Signed out (removed %s).\n", cfg.CredentialsPath)
	return nil
}

// parseChoice accepts a 1-based menu index.
func parseChoice(answer string, max int) (int, error) {
	n := 0
	for _, r := range answer {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("enter a number from 1 to %d", max)
		}
		n = n*10 + int(r-'0')
		if n > max {
			break
		}
	}
	if n < 1 || n > max {
		return 0, fmt.Errorf("enter a number from 1 to %d", max)
	}
	return n, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
