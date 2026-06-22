package host

import (
	"context"
	"errors"
	"io"
	"os"
)

// Serve runs the embedded host over the given streams until teardown exits the
// process. It is the headless protocol core: in = command stdin (NDJSON, first
// line = descriptor), out = event stdout (NDJSON), errw = diagnostics stderr.
// factory builds the App for the booted session (the cockpit/cli wave provides
// the concrete factory). Serve normally never returns — teardown calls os.Exit
// after flushing — but it returns if Run unwinds without exiting (e.g. tests
// injecting a non-exiting exit func).
func Serve(ctx context.Context, factory AppFactory, in io.Reader, out, errw io.Writer) {
	h := NewHost(factory, in, out, errw)
	h.Run(ctx)
}

// Run is the os-wired entry the CLI's `host --stdio` subcommand calls. It checks
// the stdio precondition (stdin must be a usable command stream — the Go analog
// of the TS "must run inside an Electron utility process" gate), then Serves over
// os.Stdin/os.Stdout/os.Stderr. factory is the App builder.
//
// Returns an exit code only when the precondition fails or Serve unwinds without
// exiting; on the normal path teardown calls os.Exit directly.
func Run(ctx context.Context, factory AppFactory) int {
	if factory == nil {
		_, _ = io.WriteString(os.Stderr,
			"host: no App factory wired — the cockpit/cli wave must provide one.\n")
		return 1
	}
	// Precondition: a usable command stream. If stdin is a terminal the host was
	// launched wrong (it is driven by Daintree over a pipe), so refuse with a
	// diagnostic + exit 1 (TS: the parentPort presence check).
	if isTerminal(os.Stdin) {
		_, _ = io.WriteString(os.Stderr,
			"host: --stdio requires a piped command stream on stdin, not a terminal.\n")
		return 1
	}
	Serve(ctx, factory, os.Stdin, os.Stdout, os.Stderr)
	return 0
}

// isTerminal reports whether f is an interactive terminal. A character device
// with no piped input is the "launched wrong" case.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return (info.Mode() & os.ModeCharDevice) != 0
}

// ErrAppFactoryUnset is returned by the placeholder factory until the concrete
// App (cli/app.ts port) is wired. It lets `host --stdio` build green now and
// boot-fail cleanly (host:error bootstrap-error) rather than panic.
var ErrAppFactoryUnset = errors.New("host: App factory not yet wired (cockpit/cli wave)")
