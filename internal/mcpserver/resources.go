package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/daintreehq/assistant/internal/redact"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// resources.go exposes the two things a driving agent needs when a run goes wrong and
// the poll digest is not enough: the FULL run transcript (poll returns a bounded
// window) and the session's debug log (the ground truth for what the model and tools
// actually did).
//
// They are resources rather than tools deliberately. A tool result is something the
// caller asked to happen; these are references it may follow, and only when it needs
// them — which is the difference between paying for a megabyte of trace on every poll
// and paying for it once, when diagnosing.

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
		Name:        "run-transcript",
		Title:       "Run transcript",
		URITemplate: "daintree://session/{sessionId}/run/{runId}",
		MIMEType:    "application/json",
		Description: "The COMPLETE event timeline of one run, unbounded by the window daintree.poll returns. " +
			"Read this when a poll's withheldEvents was non-zero and you need the part it dropped.",
	}, func(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		sessionID, runID, err := parseRunURI(req.Params.URI)
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
		// maxEvents 0 = the whole timeline; that is the entire point of this resource.
		out := renderRun(run, 0, 0, sess.Approvals())
		body, err := json.MarshalIndent(out, "", "  ")
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
		Name:        "session-log",
		Title:       "Session debug log",
		URITemplate: "daintree://session/{sessionId}/log",
		MIMEType:    "text/plain",
		Description: "The tail of this session's structured debug trace — every backend request, tool call with arguments " +
			"and result, and MCP call. The ground truth for what actually happened, as opposed to what the answer claims. " +
			"Requires the session to have been opened with debugLog:true. Grep it by runId, turnId or round.",
	}, func(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		sessionID, err := parseLogURI(req.Params.URI)
		if err != nil {
			return nil, err
		}
		sess, err := reg.Get(sessionID)
		if err != nil {
			return nil, err
		}
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

// parseRunURI extracts the ids from daintree://session/{s}/run/{r}.
func parseRunURI(uri string) (sessionID, runID string, err error) {
	rest, ok := strings.CutPrefix(uri, "daintree://session/")
	if !ok {
		return "", "", fmt.Errorf("not a daintree run URI: %q", uri)
	}
	sessionID, rest, ok = strings.Cut(rest, "/run/")
	if !ok || sessionID == "" || rest == "" {
		return "", "", fmt.Errorf("expected daintree://session/{sessionId}/run/{runId}, got %q", uri)
	}
	return sessionID, rest, nil
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
