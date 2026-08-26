package auth

import (
	"context"
	"os/exec"
	"runtime"
	"time"
)

// browser.go opens the system browser for the authorization request.
//
// It is an interface rather than a direct call for one reason that matters more than
// testability: the URL passed through here carries a live authorization request bound
// to this attempt's PKCE state. A test that accidentally launched a real browser would
// be handing that to whatever session the developer happens to have open, so the
// default in tests is a no-op recorder, never the real thing.

// Opener launches a URL in the user's browser.
type Opener interface {
	// Open launches url. It returns once the launcher has been handed the URL, NOT
	// when the page has loaded — there is no way to observe the latter, and the flow
	// does not need it: the loopback listener is already accepting.
	Open(ctx context.Context, url string) error
}

// openerFunc adapts a function to Opener.
type openerFunc func(context.Context, string) error

func (f openerFunc) Open(ctx context.Context, url string) error { return f(ctx, url) }

// browserLaunchTimeout bounds the launcher process. `open`/`xdg-open` hand off and exit
// immediately; one that has not returned in this long is wedged, and waiting on it
// would hang a login whose listener is already up and perfectly able to succeed.
const browserLaunchTimeout = 15 * time.Second

// noOpenHint is the remedy for every browser-launch failure: the WHOLE command, in a
// terminal.
//
// It used to read "Re-run with --no-open", which is unambiguous only where the reader is
// already looking at a shell prompt they typed a command into. Sign-in is now also a
// slash command inside an embedding host, and there "re-run with a flag" names neither a
// command nor a place to type it — the host's composer does not take flags, and the
// binary the flag belongs to is not the thing the reader is looking at. Spelling the
// whole invocation costs a few characters and is correct on both surfaces.
//
// The authorization URL is deliberately absent. --no-open prints it on the terminal path,
// to a human-controlled stream; a hint travels into host events, logs and support
// bundles, and a sign-in URL carries the state and challenge that make it worth stealing.
const noOpenHint = "Run `daintree-assistant auth login --no-open` in a terminal."

// SystemOpener returns the platform browser launcher.
//
// Deliberately a small exec rather than a dependency. The whole surface is one command
// name per platform, and this repo keeps a six-module direct dependency tree on
// purpose.
func SystemOpener() Opener {
	return openerFunc(func(ctx context.Context, url string) error {
		name, args := browserCommand(url)
		if name == "" {
			return newError(CodeInteractiveRequired,
				"no way to open a browser on this platform").
				withHint(noOpenHint)
		}
		ctx, cancel := context.WithTimeout(ctx, browserLaunchTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, name, args...)
		// No inherited stdio. A launcher that writes to stdout would corrupt the --json
		// event stream, which a caller is parsing line by line.
		cmd.Stdout, cmd.Stderr, cmd.Stdin = nil, nil, nil
		if err := cmd.Run(); err != nil {
			return wrapError(CodeBrowserFailed, "could not open a browser", err).
				withHint(noOpenHint)
		}
		return nil
	})
}

// browserCommand returns the launcher for this platform.
//
// The URL is passed as a separate argv element and never through a shell, so a URL
// containing shell metacharacters is inert. Windows is absent because this binary is
// Unix-only (flock and Setsid have no port; the !unix builds fail loudly) — adding a
// rundll32 branch here would imply a platform the rest of the binary does not support.
func browserCommand(url string) (string, []string) {
	switch runtime.GOOS {
	case "darwin":
		return "/usr/bin/open", []string{url}
	case "linux":
		return "xdg-open", []string{url}
	default:
		return "", nil
	}
}

// NoOpener never opens anything. It backs --no-open, where the URL is printed for the
// user to carry to a browser themselves, and it is the default in tests.
type NoOpener struct{}

// Open does nothing and succeeds.
func (NoOpener) Open(context.Context, string) error { return nil }
