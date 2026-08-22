// Package render is the dependency-free ANSI console helper. Color is enabled
// only on a stdout TTY and disabled when NO_COLOR is set (any value). All
// human/diagnostic output for the console and classic-REPL paths flows through
// here.
package render

import (
	"fmt"
	"io"
	"os"

	"github.com/mattn/go-isatty"
)

// Color codes.
const (
	codeDim     = "2"
	codeBold    = "1"
	codeRed     = "31"
	codeGreen   = "32"
	codeYellow  = "33"
	codeBlue    = "34"
	codeMagenta = "35"
	codeCyan    = "36"
	codeGray    = "90"
)

// toolResultTruncate caps tool-result summaries in the console.
const toolResultTruncate = 200

// Renderer writes ANSI-decorated lines to an output stream. The zero value is not
// usable; build with New.
type Renderer struct {
	out      io.Writer
	useColor bool
}

// Stdout builds a Renderer over os.Stdout, honoring NO_COLOR + isatty.
func Stdout() *Renderer { return New(os.Stdout) }

// New builds a Renderer over w. Color is enabled only when w is a TTY *os.File and NO_COLOR
// is unset. NO_COLOR is honored by PRESENCE (any value, even empty), via LookupEnv, so the
// console agrees with the attached session theme (theme.resolveMode) — previously an empty NO_COLOR
// was ignored here (os.Getenv can't tell unset from empty) while the attached session honored it.
func New(w io.Writer) *Renderer {
	useColor := false
	if f, ok := w.(*os.File); ok {
		if _, noColor := os.LookupEnv("NO_COLOR"); !noColor && isatty.IsTerminal(f.Fd()) {
			useColor = true
		}
	}
	return &Renderer{out: w, useColor: useColor}
}

// wrap decorates s with an SGR code (bare s when color is off).
func (r *Renderer) wrap(code, s string) string {
	if !r.useColor {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

// --- color helpers (return strings, no write) ---

func (r *Renderer) Dim(s string) string     { return r.wrap(codeDim, s) }
func (r *Renderer) Bold(s string) string    { return r.wrap(codeBold, s) }
func (r *Renderer) Red(s string) string     { return r.wrap(codeRed, s) }
func (r *Renderer) Green(s string) string   { return r.wrap(codeGreen, s) }
func (r *Renderer) Yellow(s string) string  { return r.wrap(codeYellow, s) }
func (r *Renderer) Blue(s string) string    { return r.wrap(codeBlue, s) }
func (r *Renderer) Magenta(s string) string { return r.wrap(codeMagenta, s) }
func (r *Renderer) Cyan(s string) string    { return r.wrap(codeCyan, s) }
func (r *Renderer) Gray(s string) string    { return r.wrap(codeGray, s) }

// --- write helpers ---

// Out writes raw (no newline).
func (r *Renderer) Out(s string) { fmt.Fprint(r.out, s) }

// Line writes s followed by a newline.
func (r *Renderer) Line(s string) { fmt.Fprint(r.out, s+"\n") }

// StreamToken writes a streamed token raw (no newline).
func (r *Renderer) StreamToken(s string) { fmt.Fprint(r.out, s) }

// Info / Warn / Error / Success are the four status lines.
func (r *Renderer) Info(s string)    { r.Line(r.Cyan("ℹ ") + s) }
func (r *Renderer) Warn(s string)    { r.Line(r.Yellow("⚠ ") + s) }
func (r *Renderer) Error(s string)   { r.Line(r.Red("✗ ") + s) }
func (r *Renderer) Success(s string) { r.Line(r.Green("✓ ") + s) }

// ToolCall renders a gray "  ⚙ name(args)" line (args already compacted).
func (r *Renderer) ToolCall(name, args string) {
	r.Line(r.Gray("  ⚙ " + name + "(" + args + ")"))
}

// ToolResult renders a "  ↳" prefix (green ok / red fail) + dimmed truncated summary.
func (r *Renderer) ToolResult(ok bool, summary string) {
	arrow := "  ↳"
	if ok {
		arrow = r.Green(arrow)
	} else {
		arrow = r.Red(arrow)
	}
	r.Line(arrow + " " + r.Dim(Truncate(summary, toolResultTruncate)))
}

// Banner writes a blank line, each line, then a blank line.
func (r *Renderer) Banner(lines []string) {
	r.Line("")
	for _, l := range lines {
		r.Line(l)
	}
	r.Line("")
}

// AssistantStart writes a blank line before a streamed answer.
func (r *Renderer) AssistantStart() { r.Line("") }

// AssistantEnd terminates a streamed answer with a newline.
func (r *Renderer) AssistantEnd() { r.Line("") }

// Truncate caps s to limit runes, appending "…" when cut. Exported for reuse.
func Truncate(s string, limit int) string {
	rs := []rune(s)
	if len(rs) <= limit {
		return s
	}
	if limit <= 1 {
		return string(rs[:limit])
	}
	return string(rs[:limit-1]) + "…"
}
