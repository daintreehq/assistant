package backend

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// The `preamble` event is the fast visible preview. Every property pinned here is
// one the backend's own contract calls load-bearing, and each one fails silently
// if it regresses: a preamble that commits on error leaves a sentence in history
// the user never earned, one that appends on replay shows the same intent twice,
// and one that counts as visible content makes every turn that showed one
// un-retryable.

// preambleStream is meta → preamble → delta → done: the ordinary shape.
const preambleStream = "event: meta\ndata: {}\n\n" +
	"event: preamble\ndata: {\"id\":\"pre_1\",\"content\":\"I'll check the failing test.\"," +
	"\"provisional\":true,\"commit_on\":\"done\"}\n\n" +
	"event: delta\ndata: {\"content\":\"The loader was wrong.\"}\n\n" +
	"event: done\ndata: {\"finish_reason\":\"stop\"}\n\n"

func TestPreambleJoinsTheAssistantMessageOnDone(t *testing.T) {
	srv := sseServer(t, preambleStream, nil)
	c := NewClient(ClientConfig{BaseURL: srv.URL})

	var shown []string
	res, err := c.RespondStream(context.Background(), RespondRequest{}, StreamCallbacks{
		OnPreamble: func(p StreamPreamble) { shown = append(shown, p.Content) },
	})
	if err != nil {
		t.Fatalf("stream failed: %v", err)
	}
	if len(shown) != 1 || shown[0] != "I'll check the failing test." {
		t.Fatalf("OnPreamble got %q, want one preview", shown)
	}
	// ONE message, blank-line joined. Two messages would claim a turn boundary the
	// executor never saw — the backend handed it the preamble as its own prior turn.
	want := "I'll check the failing test.\n\nThe loader was wrong."
	if res.Message.Content != want {
		t.Fatalf("committed content = %q, want %q", res.Message.Content, want)
	}
	if res.Preamble != "I'll check the failing test." {
		t.Fatalf("Preamble = %q, want the preview kept beside the joined content", res.Preamble)
	}
}

func TestPreambleIsNotDecodedAsVisibleContent(t *testing.T) {
	// The preamble must not reach OnContent. OnContent is the retry boundary AND
	// the display's executor-token path; routing the preamble through it would
	// both freeze replay and double-render the text.
	srv := sseServer(t, preambleStream, nil)
	c := NewClient(ClientConfig{BaseURL: srv.URL})

	var content strings.Builder
	if _, err := c.RespondStream(context.Background(), RespondRequest{}, StreamCallbacks{
		OnContent: func(s string) { content.WriteString(s) },
	}); err != nil {
		t.Fatalf("stream failed: %v", err)
	}
	if got := content.String(); got != "The loader was wrong." {
		t.Fatalf("OnContent saw %q, want the executor's content alone", got)
	}
}

func TestPreambleCommitsNothingWhenTheStreamErrors(t *testing.T) {
	// `commit_on` is "done" and nothing else. A turn that dies after showing a
	// preview must leave no trace of it: the text was provisional, and the work it
	// described never happened.
	const body = "event: meta\ndata: {}\n\n" +
		"event: preamble\ndata: {\"id\":\"pre_1\",\"content\":\"I'll look into that.\",\"provisional\":true,\"commit_on\":\"done\"}\n\n" +
		"event: error\ndata: {\"error\":{\"code\":\"upstream_error\",\"message\":\"boom\"}}\n\n"

	srv, _ := countingServer(t, func(int) (int, string) { return http.StatusOK, body })
	defer srv.Close()
	// No retries: this asserts the terminal-failure shape, not the replay path.
	c := NewClient(ClientConfig{BaseURL: srv.URL, Retry: RetryPolicy{MaxAttempts: 1}})

	shown := 0
	res, err := c.RespondStream(context.Background(), RespondRequest{}, StreamCallbacks{
		OnPreamble: func(StreamPreamble) { shown++ },
	})
	if err == nil {
		t.Fatal("expected the terminal error to surface")
	}
	if shown != 1 {
		t.Fatalf("OnPreamble fired %d times, want 1 — it is still SHOWN, just not committed", shown)
	}
	if res.Preamble != "" || res.Message.Content != "" {
		t.Fatalf("failed stream committed preamble=%q content=%q, want neither",
			res.Preamble, res.Message.Content)
	}
}

func TestPreambleDoesNotCommitOnAnInterruptedStream(t *testing.T) {
	// The other way a stream fails: it just STOPS — no terminal error event, no
	// `done`. This is the path the commit barrier actually guards. A terminal error
	// returns before the message is even assembled, but an interrupted stream runs
	// the whole finish block, so without `doneSeen` the preview would be handed back
	// to a caller whose turn never completed.
	const truncated = "event: meta\ndata: {}\n\n" +
		"event: preamble\ndata: {\"id\":\"pre_1\",\"content\":\"I'll look into that.\",\"provisional\":true,\"commit_on\":\"done\"}\n\n" +
		"event: delta\ndata: {\"content\":\"partial\"}\n\n"

	srv, _ := countingServer(t, func(int) (int, string) { return http.StatusOK, truncated })
	defer srv.Close()
	c := NewClient(ClientConfig{BaseURL: srv.URL, Retry: RetryPolicy{MaxAttempts: 1}})

	res, err := c.RespondStream(context.Background(), RespondRequest{}, StreamCallbacks{})
	if err == nil {
		t.Fatal("a stream that ended before `done` must be an error, never a silent success")
	}
	if res.Preamble != "" {
		t.Fatalf("Preamble = %q on an interrupted stream, want nothing to commit", res.Preamble)
	}
}

func TestPreambleIsNotTheRetryBoundary(t *testing.T) {
	// A failure arriving after a preamble is still replayable. Preamble text is
	// server-generated, idempotent intent; treating it as visible content would
	// strand every turn that showed one on its first transient error.
	const failAfterPreamble = "event: meta\ndata: {}\n\n" +
		"event: preamble\ndata: {\"id\":\"pre_1\",\"content\":\"I'll start on that.\",\"provisional\":true,\"commit_on\":\"done\"}\n\n" +
		"event: error\ndata: {\"error\":{\"code\":\"upstream_error\",\"message\":\"boom\"}}\n\n"

	srv, hits := countingServer(t, func(n int) (int, string) {
		if n == 0 {
			return http.StatusOK, failAfterPreamble
		}
		return http.StatusOK, preambleStream
	})
	defer srv.Close()

	c := NewClient(ClientConfig{BaseURL: srv.URL, Retry: fastRetry(3)})
	res, err := c.RespondStream(context.Background(), RespondRequest{}, StreamCallbacks{})
	if err != nil {
		t.Fatalf("a pre-content failure must still be replayed, got %v", err)
	}
	if got := hits(); got != 2 {
		t.Fatalf("server hit %d times, want 2 (the failure was retried)", got)
	}
	if !strings.HasSuffix(res.Message.Content, "The loader was wrong.") {
		t.Fatalf("content = %q, want the replayed answer", res.Message.Content)
	}
}

func TestPreambleIsFirstWinsAcrossAttempts(t *testing.T) {
	// The screen and the committed history must be the SAME text. A replayed
	// attempt asks the fast model again and gets its own wording, so last-wins
	// would commit a sentence the user never read — nothing on screen is rewritten
	// by the second attempt arriving.
	const firstAttempt = "event: meta\ndata: {}\n\n" +
		"event: preamble\ndata: {\"id\":\"pre_1\",\"content\":\"FIRST wording.\",\"provisional\":true,\"commit_on\":\"done\"}\n\n" +
		"event: error\ndata: {\"error\":{\"code\":\"upstream_error\",\"message\":\"boom\"}}\n\n"
	const secondAttempt = "event: meta\ndata: {}\n\n" +
		"event: preamble\ndata: {\"id\":\"pre_2\",\"content\":\"SECOND wording.\",\"provisional\":true,\"commit_on\":\"done\"}\n\n" +
		"event: delta\ndata: {\"content\":\"Answer.\"}\n\n" +
		"event: done\ndata: {\"finish_reason\":\"stop\"}\n\n"

	srv, _ := countingServer(t, func(n int) (int, string) {
		if n == 0 {
			return http.StatusOK, firstAttempt
		}
		return http.StatusOK, secondAttempt
	})
	defer srv.Close()

	c := NewClient(ClientConfig{BaseURL: srv.URL, Retry: fastRetry(3)})
	var shown []string
	res, err := c.RespondStream(context.Background(), RespondRequest{}, StreamCallbacks{
		OnPreamble: func(p StreamPreamble) { shown = append(shown, p.Content) },
	})
	if err != nil {
		t.Fatalf("stream failed: %v", err)
	}
	if len(shown) != 1 || shown[0] != "FIRST wording." {
		t.Fatalf("OnPreamble got %q, want only the one already on screen", shown)
	}
	want := "FIRST wording.\n\nAnswer."
	if res.Message.Content != want {
		t.Fatalf("committed %q, want %q — history must match the screen", res.Message.Content, want)
	}
}

func TestPreambleAloneBecomesTheContentOnAToolCallRound(t *testing.T) {
	// A tool-call round can produce no prose at all. The preview is still what the
	// user saw and still belongs to this turn.
	const body = "event: meta\ndata: {}\n\n" +
		"event: preamble\ndata: {\"id\":\"pre_1\",\"content\":\"I'll read the file.\",\"provisional\":true,\"commit_on\":\"done\"}\n\n" +
		"event: delta\ndata: {\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"type\":\"function\"," +
		"\"function\":{\"name\":\"fs__read\",\"arguments\":\"{}\"}}]}\n\n" +
		"event: done\ndata: {\"finish_reason\":\"tool_calls\"}\n\n"

	srv := sseServer(t, body, nil)
	c := NewClient(ClientConfig{BaseURL: srv.URL})
	res, err := c.RespondStream(context.Background(), RespondRequest{}, StreamCallbacks{})
	if err != nil {
		t.Fatalf("stream failed: %v", err)
	}
	if res.Message.Content != "I'll read the file." {
		t.Fatalf("content = %q, want the preamble with no trailing blank line", res.Message.Content)
	}
	if len(res.Message.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(res.Message.ToolCalls))
	}
}

func TestAStreamWithoutAPreambleIsUnchanged(t *testing.T) {
	// FAST_RESPONSE_MODE is off in every deployment today, so this is the shape
	// almost every real turn takes: no event, no callback, no join.
	srv := sseServer(t, streamOK, nil)
	c := NewClient(ClientConfig{BaseURL: srv.URL})

	fired := false
	res, err := c.RespondStream(context.Background(), RespondRequest{}, StreamCallbacks{
		OnPreamble: func(StreamPreamble) { fired = true },
	})
	if err != nil {
		t.Fatalf("stream failed: %v", err)
	}
	if fired {
		t.Fatal("OnPreamble fired on a stream that carried no preamble event")
	}
	if res.Preamble != "" || res.Message.Content != "hi" {
		t.Fatalf("preamble=%q content=%q, want an untouched turn", res.Preamble, res.Message.Content)
	}
}

func TestAnUnusablePreambleIsDroppedNotFatal(t *testing.T) {
	// Best-effort by contract, exactly like `compaction`: a preview this client
	// cannot use is a turn that answers without one, never a failed answer. The
	// reply the user is waiting for has already been generated.
	cases := []struct {
		name  string
		event string
	}{
		{"malformed json", "event: preamble\ndata: {not json}\n\n"},
		{"empty content", "event: preamble\ndata: {\"id\":\"p\",\"content\":\"\",\"provisional\":true,\"commit_on\":\"done\"}\n\n"},
		{"whitespace only", "event: preamble\ndata: {\"id\":\"p\",\"content\":\"   \\n \",\"provisional\":true,\"commit_on\":\"done\"}\n\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := "event: meta\ndata: {}\n\n" + tc.event +
				"event: delta\ndata: {\"content\":\"hi\"}\n\n" +
				"event: done\ndata: {\"finish_reason\":\"stop\"}\n\n"
			srv := sseServer(t, body, nil)
			c := NewClient(ClientConfig{BaseURL: srv.URL})

			fired := false
			res, err := c.RespondStream(context.Background(), RespondRequest{}, StreamCallbacks{
				OnPreamble: func(StreamPreamble) { fired = true },
			})
			if err != nil {
				t.Fatalf("an unusable preamble sank the turn: %v", err)
			}
			if fired {
				t.Fatal("OnPreamble fired for an unusable preview")
			}
			if res.Message.Content != "hi" {
				t.Fatalf("content = %q, want the answer delivered without a preview", res.Message.Content)
			}
		})
	}
}

func TestAPreambleStatingDifferentTermsIsRefused(t *testing.T) {
	// The client implements exactly one policy: hold it provisional, commit it on
	// `done`. An event stating anything else describes a contract we do not
	// implement, and showing the text anyway would render a preview under rules its
	// sender never agreed to — invisibly, since the turn still looks fine.
	cases := []struct {
		name  string
		event string
	}{
		{"not provisional", "event: preamble\ndata: {\"id\":\"p\",\"content\":\"hi there\"," +
			"\"provisional\":false,\"commit_on\":\"done\"}\n\n"},
		{"commits somewhere else", "event: preamble\ndata: {\"id\":\"p\",\"content\":\"hi there\"," +
			"\"provisional\":true,\"commit_on\":\"meta\"}\n\n"},
		{"fields absent entirely", "event: preamble\ndata: {\"id\":\"p\",\"content\":\"hi there\"}\n\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := "event: meta\ndata: {}\n\n" + tc.event +
				"event: delta\ndata: {\"content\":\"hi\"}\n\n" +
				"event: done\ndata: {\"finish_reason\":\"stop\"}\n\n"
			srv := sseServer(t, body, nil)
			c := NewClient(ClientConfig{BaseURL: srv.URL})

			fired := false
			res, err := c.RespondStream(context.Background(), RespondRequest{}, StreamCallbacks{
				OnPreamble: func(StreamPreamble) { fired = true },
			})
			if err != nil {
				t.Fatalf("a refused preamble sank the turn: %v", err)
			}
			if fired {
				t.Fatal("a preamble stating different terms was shown anyway")
			}
			if res.Message.Content != "hi" {
				t.Fatalf("content = %q, want the answer delivered without a preview", res.Message.Content)
			}
		})
	}
}

func TestPreambleFieldsAreDecoded(t *testing.T) {
	// Decoded for symmetry with the backend contract. The client implements the
	// only values the contract allows rather than branching on them, but a backend
	// that changed its mind should show up here rather than as history quietly
	// gaining a message it should not have.
	srv := sseServer(t, preambleStream, nil)
	c := NewClient(ClientConfig{BaseURL: srv.URL})

	var got StreamPreamble
	if _, err := c.RespondStream(context.Background(), RespondRequest{}, StreamCallbacks{
		OnPreamble: func(p StreamPreamble) { got = p },
	}); err != nil {
		t.Fatalf("stream failed: %v", err)
	}
	if got.ID != "pre_1" || !got.Provisional || got.CommitOn != "done" {
		t.Fatalf("decoded %+v, want id pre_1, provisional true, commit_on done", got)
	}
}
