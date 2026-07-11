package backend

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestBufReader wraps a string in the same small-buffer bufio.Reader shape the
// parser uses, forcing readBoundedLine through its ErrBufferFull accumulation path.
func newTestBufReader(s string) *bufio.Reader {
	return bufio.NewReaderSize(strings.NewReader(s), 4096)
}

// flushWrite writes s to w and flushes, so the client sees the bytes immediately
// (SSE liveness tests depend on real write timing, not response buffering).
func flushWrite(w http.ResponseWriter, s string) {
	_, _ = io.WriteString(w, s)
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}
}

// A stream that stays open but goes silent past the idle window must abort with the
// typed stream_idle_timeout error — distinct from a caller cancellation.
func TestRespondStream_IdleTimeoutAbortsSilentStream(t *testing.T) {
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flushWrite(w, "event: meta\ndata: {}\n\n")
		<-release // hold the stream open, silent
	}))
	t.Cleanup(srv.Close)

	// Retries disabled: the idle abort IS retriable, and a retry would just hit the
	// silent handler again and double the test time.
	c := NewClient(ClientConfig{BaseURL: srv.URL, Retry: RetryPolicy{MaxAttempts: 1}})
	c.streamIdleTimeout = 80 * time.Millisecond

	start := time.Now()
	_, err := c.RespondStream(context.Background(), RespondRequest{}, StreamCallbacks{})
	elapsed := time.Since(start)
	close(release)
	released = true

	if err == nil {
		t.Fatal("expected idle-timeout error, got nil")
	}
	var be *Error
	if !errorsAs(err, &be) {
		t.Fatalf("expected *Error, got %T: %v", err, err)
	}
	if be.Code != "stream_idle_timeout" || !be.Stream {
		t.Fatalf("error = %+v, want typed stream_idle_timeout", be)
	}
	// Distinct from caller cancellation: the parent context was never cancelled.
	if context.Background().Err() != nil {
		t.Fatal("parent context unexpectedly cancelled")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("idle abort took %v, want ~the 80ms window", elapsed)
	}
}

// SSE comment lines (the backend's ": hb" heartbeats) carry no events but must reset
// the idle window: a stream whose only activity is heartbeats stays alive well past
// the idle timeout, and the comments themselves are ignored by the parser.
func TestRespondStream_CommentHeartbeatsResetIdleWindow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flushWrite(w, "event: meta\ndata: {}\n\n")
		// Six heartbeats 40ms apart: total elapsed (~240ms) far exceeds the 120ms
		// idle window, but each gap is well inside it — only resets keep this alive.
		for i := 0; i < 6; i++ {
			time.Sleep(40 * time.Millisecond)
			flushWrite(w, ": hb\n\n")
		}
		flushWrite(w, "event: delta\ndata: {\"content\":\"alive\"}\n\n"+
			"event: done\ndata: {\"finish_reason\":\"stop\",\"usage\":{}}\n\n")
	}))
	t.Cleanup(srv.Close)

	c := NewClient(ClientConfig{BaseURL: srv.URL, Retry: RetryPolicy{MaxAttempts: 1}})
	c.streamIdleTimeout = 120 * time.Millisecond

	res, err := c.RespondStream(context.Background(), RespondRequest{}, StreamCallbacks{})
	if err != nil {
		t.Fatalf("heartbeat-kept-alive stream failed: %v", err)
	}
	if res.Message.Content != "alive" {
		t.Fatalf("content = %q, want alive (comments must be ignored, not decoded)", res.Message.Content)
	}
}

// A single SSE line beyond maxSSELineBytes is a protocol error: typed, non-retriable,
// and the stream aborts cleanly instead of buffering without bound.
func TestRespondStream_OversizedLineRejected(t *testing.T) {
	huge := "data: " + strings.Repeat("a", maxSSELineBytes+16) + "\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flushWrite(w, "event: meta\ndata: {}\n\nevent: delta\n"+huge)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(ClientConfig{BaseURL: srv.URL, Retry: RetryPolicy{MaxAttempts: 1}})
	_, err := c.RespondStream(context.Background(), RespondRequest{}, StreamCallbacks{})
	var be *Error
	if !errorsAs(err, &be) || be.Code != "stream_line_too_large" || !be.Stream {
		t.Fatalf("error = %v, want typed stream_line_too_large", err)
	}
	if isRetriable(be) {
		t.Fatal("oversized line must not be retriable (it would replay identically)")
	}
}

// Many bounded lines accumulating into one event beyond maxSSEEventBytes must also
// abort with a typed protocol error.
func TestRespondStream_OversizedEventRejected(t *testing.T) {
	chunk := "data: " + strings.Repeat("b", 512*1024) + "\n" // 512 KiB per line, under the line cap
	var event strings.Builder
	event.WriteString("event: delta\n")
	for i := 0; i < 9; i++ { // ~4.5 MiB accumulated payload > 4 MiB event cap
		event.WriteString(chunk)
	}
	event.WriteString("\n")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flushWrite(w, "event: meta\ndata: {}\n\n"+event.String())
	}))
	t.Cleanup(srv.Close)

	c := NewClient(ClientConfig{BaseURL: srv.URL, Retry: RetryPolicy{MaxAttempts: 1}})
	_, err := c.RespondStream(context.Background(), RespondRequest{}, StreamCallbacks{})
	var be *Error
	if !errorsAs(err, &be) || be.Code != "stream_event_too_large" || !be.Stream {
		t.Fatalf("error = %v, want typed stream_event_too_large", err)
	}
	if isRetriable(be) {
		t.Fatal("oversized event must not be retriable (it would replay identically)")
	}
}

// A normal multi-event stream under all bounds is untouched by the new guards, and a
// line exactly at the boundary still parses (the bound is exclusive).
func TestReadBoundedLineBoundary(t *testing.T) {
	// Exactly maxSSELineBytes (including the newline) passes.
	exact := strings.Repeat("x", maxSSELineBytes-1) + "\n"
	if got, err := readBoundedLine(newTestBufReader(exact)); err != nil || got != exact {
		t.Fatalf("boundary line: err=%v len=%d, want clean read of %d", err, len(got), len(exact))
	}
	// One byte over fails with the sentinel.
	over := strings.Repeat("x", maxSSELineBytes+1) + "\n"
	if _, err := readBoundedLine(newTestBufReader(over)); err != errSSELineTooLong {
		t.Fatalf("over-boundary line err = %v, want errSSELineTooLong", err)
	}
	// A final line without a newline is returned with io.EOF, like ReadString.
	if got, err := readBoundedLine(newTestBufReader("tail")); err != io.EOF || got != "tail" {
		t.Fatalf("unterminated tail: got %q err=%v, want \"tail\" io.EOF", got, err)
	}
}
