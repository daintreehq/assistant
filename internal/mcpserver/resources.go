package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/daintreehq/assistant/internal/redact"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// resources.go exposes the two things a driving agent needs when a run goes wrong and
// the poll digest is not enough: the run transcript in PAGES larger than poll's window,
// and the process debug log (the ground truth for what the model and tools actually did).
//
// Neither is unbounded, and the transcript's page is the more interesting of the two. It
// used to return every retained event, which made it the largest single response this
// server could produce — reachable by a caller with no idea how long the run was, and
// built and encoded in full before anyone could decide it was too big. "Larger than a
// poll window" is the useful property; "unbounded" was never one.
//
// They are resources rather than tools deliberately. A tool result is something the
// caller asked to happen; these are references it may follow, and only when it needs
// them — which is the difference between paying for a megabyte of trace on every poll
// and paying for it once, when diagnosing.

// runTranscriptURITemplate is the run-transcript resource template.
//
// The {?fromSeq,limit} expression is REQUIRED, not decoration. The SDK matches a read
// against this template with a regexp, so without it a paged URI matched nothing and the
// whole paging feature was unreachable — the base resource answered, its `remaining`
// pointed at a continuation URI, and that URI reached no handler at all. Pinned by
// TestTranscriptTemplateMatchesBothPagedAndUnpagedURIs.
const runTranscriptURITemplate = "daintree://session/{sessionId}/run/{runId}{?fromSeq,limit}"

const (
	// LogURIScheme namespaces this server's resources.
	LogURIScheme = "daintree"
	// maxLogTail bounds a debug-log read. These files reach tens of megabytes over a
	// long session; a resource read that returned the whole thing would blow up the
	// caller's context for a fact usually found in the last few thousand lines.
	maxLogTail = 256 * 1024
)

// RegisterResources wires the resource templates onto a server.
func RegisterResources(s *mcp.Server, reg *Registry) {
	s.AddResourceTemplate(&mcp.ResourceTemplate{
		Name:  "run-transcript",
		Title: "Run transcript",
		// The {?fromSeq,limit} expression is REQUIRED, not decoration. The SDK matches a
		// read against this template with a regexp, so without it a paged URI matched
		// nothing and the whole paging feature was unreachable — the base resource
		// answered, its `remaining` pointed at a continuation URI, and that URI 404'd.
		URITemplate: runTranscriptURITemplate,
		MIMEType:    "application/json",
		Description: "The retained event timeline of one run, in pages larger than the window daintree.poll returns. " +
			"Read this when a poll's withheldEvents was non-zero and you need the part it dropped. " +
			"Append ?fromSeq=N&limit=M to page; the response reports nextSeq, remaining and complete.",
	}, func(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		sessionID, runID, page, err := parseRunURI(req.Params.URI)
		if err != nil {
			return nil, err
		}
		sess, err := reg.Get(sessionID)
		if err != nil {
			return nil, err
		}
		run, err := sess.Run(runID)
		if err != nil {
			return nil, err
		}
		// PAGED, not "the whole thing". A resource that returned every retained event
		// was the largest single response this server could produce, it was reachable
		// by a caller that had no idea how long the run was, and it had to be built and
		// encoded in full before anyone could decide it was too big. The page is larger
		// than poll's window, which is the useful distinction; unbounded is not.
		out := renderRun(run, page.fromSeq, page.limit, sess.Approvals())
		// Every paging field is DERIVED from the one response, which came from one lock
		// hold. Taking the total separately let a page report complete:true beside a
		// total that had already grown past it, and a caller stopping on `complete`
		// silently missed the tail.
		body, err := json.MarshalIndent(transcriptPage{
			RunOutput: out,
			Remaining: out.WithheldEvents,
			Complete:  out.WithheldEvents == 0,
		}, "", "  ")
		if err != nil {
			return nil, fmt.Errorf("encode transcript: %w", err)
		}
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{
			URI:      req.Params.URI,
			MIMEType: "application/json",
			Text:     string(body),
		}}}, nil
	})

	s.AddResourceTemplate(&mcp.ResourceTemplate{
		Name:        "server-log",
		Title:       "Server debug log",
		URITemplate: "daintree://session/{sessionId}/log",
		MIMEType:    "text/plain",
		Description: "The tail of this SERVER PROCESS's structured debug trace — every backend request, tool call with " +
			"arguments and result, and MCP call, for every session this process is running. The ground truth for what " +
			"actually happened, as opposed to what the answer claims. Requires a session opened with debugLog:true. " +
			"NOT isolated per session: filter by the sessionId, runId or turnId fields on each line.",
	}, func(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		sessionID, err := parseLogURI(req.Params.URI)
		if err != nil {
			return nil, err
		}
		sess, err := reg.Get(sessionID)
		if err != nil {
			return nil, err
		}
		// The URI is addressed by session because that is how a caller reaches it, but
		// the FILE is process-global: debuglog keeps a single active path, so every
		// session in this process writes to it. Saying "this session's log" made the
		// resource sound isolated when it is not — a second session's conversation and
		// tool activity are in the same file, and a caller treating grep as a boundary
		// would be trusting a convention rather than a mechanism.
		//
		// The honest near-term answer is to name the scope and default MaxSessions to 1.
		// Real isolation needs a per-runtime logger rather than a package-global
		// singleton, which is a change to internal/debuglog, not to this file.
		path := sess.Facts().LogPath
		if path == "" {
			return nil, fmt.Errorf(
				"this session has no debug log — it was opened without debugLog:true, so there is no trace to read. " +
					"Open a new session with debugLog:true to capture one")
		}
		text, err := readTail(path, maxLogTail)
		if err != nil {
			return nil, err
		}
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{
			URI:      req.Params.URI,
			MIMEType: "text/plain",
			Text:     text,
		}}}, nil
	})
}

// transcriptPage is one page of a run's timeline. It embeds the ordinary run shape so a
// caller that already parses a poll response can parse this too, and adds the three
// fields that make paging usable: where to continue, how much is left, and whether this
// page is the end.
type transcriptPage struct {
	RunOutput
	// Remaining is events past this page. It duplicates withheldEvents deliberately —
	// the name a paging caller looks for is "remaining", and the name a polling caller
	// looks for is "withheld".
	Remaining int `json:"remaining"`
	// Complete says this page reached the end of the retained timeline AS OF the read.
	// RETAINED, not produced: a run pruned from the session's history is gone
	// regardless, and a live run can grow after the page was taken — which is why a
	// caller polling a running run should re-read from nextSeq rather than stopping on
	// complete once.
	Complete bool `json:"complete"`
}

// runPage is the paging request parsed off a transcript URI.
type runPage struct {
	fromSeq int
	limit   int
}

// parseRunURI extracts the ids and any paging query from
// daintree://session/{s}/run/{r}[?fromSeq=N&limit=M].
func parseRunURI(uri string) (sessionID, runID string, page runPage, err error) {
	page = runPage{limit: MaxPollEvents}
	if raw, query, found := strings.Cut(uri, "?"); found {
		uri = raw
		values, perr := url.ParseQuery(query)
		if perr != nil {
			return "", "", page, fmt.Errorf("transcript URI has an unparseable query: %w", perr)
		}
		if v := values.Get("fromSeq"); v != "" {
			n, cerr := strconv.Atoi(v)
			if cerr != nil || n < 0 {
				return "", "", page, fmt.Errorf("fromSeq must be a non-negative integer, got %q", v)
			}
			page.fromSeq = n
		}
		if v := values.Get("limit"); v != "" {
			n, cerr := strconv.Atoi(v)
			if cerr != nil || n < 0 {
				return "", "", page, fmt.Errorf("limit must be a non-negative integer, got %q", v)
			}
			page.limit = clampPageSize(n, MaxPollEvents, MaxPollEvents)
		}
	}
	rest, ok := strings.CutPrefix(uri, "daintree://session/")
	if !ok {
		return "", "", page, fmt.Errorf("not a daintree run URI: %q", uri)
	}
	sessionID, rest, ok = strings.Cut(rest, "/run/")
	if !ok || sessionID == "" || rest == "" {
		return "", "", page, fmt.Errorf("expected daintree://session/{sessionId}/run/{runId}, got %q", uri)
	}
	return sessionID, rest, page, nil
}

// parseLogURI extracts the id from daintree://session/{s}/log.
func parseLogURI(uri string) (string, error) {
	rest, ok := strings.CutPrefix(uri, "daintree://session/")
	if !ok {
		return "", fmt.Errorf("not a daintree log URI: %q", uri)
	}
	sessionID, ok := strings.CutSuffix(rest, "/log")
	if !ok || sessionID == "" {
		return "", fmt.Errorf("expected daintree://session/{sessionId}/log, got %q", uri)
	}
	return sessionID, nil
}

// readTail returns at most max bytes from the END of a file, prefixed with a marker
// when it truncated. The tail, not the head: a trace is read to find what went wrong,
// which is at the end.
//
// The result passes through the redactor. The debug log is already written redacted, so
// this is belt-and-braces — but it is cheap, and this content crosses a process boundary
// into another agent's context, which is one hop further than the file ever was.
func readTail(path string, max int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("read debug log: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("stat debug log: %w", err)
	}
	size := fi.Size()
	var prefix string
	offset := int64(0)
	if size > int64(max) {
		offset = size - int64(max)
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return "", fmt.Errorf("seek debug log: %w", err)
		}
		prefix = fmt.Sprintf("[… %d earlier bytes omitted; this is the tail of %s …]\n", offset, path)
	}
	// LimitReader, not ReadAll: the log is being APPENDED TO while we read it (this is a
	// live session's trace), so reading to EOF would return however much arrived in the
	// meantime — unbounded in exactly the case the bound exists for.
	body, err := io.ReadAll(io.LimitReader(f, int64(max)))
	if err != nil {
		return "", fmt.Errorf("read debug log: %w", err)
	}
	text := string(body)
	// Drop a partial first line — confusing to read, useless to grep — but only when the
	// offset actually landed mid-line. Checking the preceding byte avoids discarding a
	// perfectly good line when the cut happened to fall on a boundary.
	if offset > 0 && !startsOnLineBoundary(f, offset) {
		if _, after, ok := strings.Cut(text, "\n"); ok {
			text = after
		}
	}
	return prefix + redact.String(text), nil
}

// startsOnLineBoundary reports whether offset is immediately after a newline. A read
// error is treated as "not a boundary": dropping one line is cheaper than emitting a
// confusing partial one.
func startsOnLineBoundary(f *os.File, offset int64) bool {
	var b [1]byte
	if _, err := f.ReadAt(b[:], offset-1); err != nil {
		return false
	}
	return b[0] == '\n'
}
