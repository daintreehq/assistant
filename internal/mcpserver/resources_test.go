package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/agent"
	"github.com/daintreehq/assistant/internal/redact"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/yosida95/uritemplate/v3"
)

// resources_test.go covers the two references a driving agent follows when a poll digest
// is not enough. They are resources rather than tool results precisely so their cost is
// paid once, when diagnosing — so the tests care most about the bounds and the redaction.

// connectWithResources is connect() plus the resource templates.
func connectWithResources(t *testing.T, factory RuntimeFactory) (*mcp.ClientSession, *Registry) {
	t.Helper()
	ctx := context.Background()
	reg := NewUnconfinedRegistry(ctx, factory)
	srv := mcp.NewServer(&mcp.Implementation{Name: ServerName, Version: "test"}, nil)
	Register(srv, reg, NewBinaryInfo("test"), ctx)
	RegisterResources(srv, reg)

	st, ct := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() {
		_ = cs.Close()
		reg.CloseAll()
	})
	return cs, reg
}

func readResource(t *testing.T, cs *mcp.ClientSession, uri string) (string, error) {
	t.Helper()
	res, err := cs.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: uri})
	if err != nil {
		return "", err
	}
	if len(res.Contents) == 0 {
		t.Fatalf("resource %s returned no contents", uri)
	}
	return res.Contents[0].Text, nil
}

// TestRunTranscriptResourceReturnsTheWholeTimeline: poll deliberately truncates, so the
// transcript is the only way to see the part it dropped.
func TestRunTranscriptResourceReturnsTheWholeTimeline(t *testing.T) {
	fake := newFakeRuntime("ses_test")
	fake.script = func(sink agent.EventSink) {
		for i := 0; i < 120; i++ {
			sink.Info("step")
		}
		sink.AssistantEnd("done", "")
	}
	cs, _ := connectWithResources(t, func(_, _ context.Context, _ OpenParams) (Runtime, error) { return fake, nil })
	sess := openSession(t, cs)
	fake.letFinish()

	var run RunOutput
	if err := call(t, cs, "daintree.ask", AskInput{SessionID: sess.SessionID, Prompt: "x", Wait: true, WaitMs: 5000}, &run); err != nil {
		t.Fatalf("ask: %v", err)
	}
	// The poll window truncated, and said so.
	if run.WithheldEvents == 0 {
		t.Fatalf("expected the default window to truncate 121 events, got %d shown", len(run.Events))
	}

	uri := "daintree://session/" + sess.SessionID + "/run/" + run.RunID
	body, err := readResource(t, cs, uri)
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	var full RunOutput
	if err := json.Unmarshal([]byte(body), &full); err != nil {
		t.Fatalf("transcript is not valid JSON: %v", err)
	}
	if len(full.Events) != 121 {
		t.Errorf("transcript has %d events, want the complete 121", len(full.Events))
	}
	if full.WithheldEvents != 0 {
		t.Errorf("the transcript must withhold nothing, got %d", full.WithheldEvents)
	}
}

// TestLogResourceTailsAndBoundsTheFile: these files reach tens of megabytes, and the
// answer is almost always at the END.
func TestLogResourceTailsAndBoundsTheFile(t *testing.T) {
	redact.ResetSecretsForTest()
	t.Cleanup(redact.ResetSecretsForTest)

	dir := t.TempDir()
	logPath := filepath.Join(dir, "session.log")
	var sb strings.Builder
	for i := 0; i < 60_000; i++ {
		sb.WriteString("line of trace output that is long enough to matter\n")
	}
	sb.WriteString("THE-LAST-LINE-MATTERS\n")
	if err := os.WriteFile(logPath, []byte(sb.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	fake := newFakeRuntime("ses_test")
	fake.facts.LogPath = logPath
	cs, _ := connectWithResources(t, func(_, _ context.Context, _ OpenParams) (Runtime, error) { return fake, nil })
	sess := openSession(t, cs)
	fake.letFinish()

	body, err := readResource(t, cs, "daintree://session/"+sess.SessionID+"/log")
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if len(body) > maxLogTail+1024 {
		t.Errorf("log read returned %d bytes, want it bounded near %d", len(body), maxLogTail)
	}
	if !strings.Contains(body, "THE-LAST-LINE-MATTERS") {
		t.Error("the read must be the TAIL — that is where a failure is")
	}
	if !strings.Contains(body, "earlier bytes omitted") {
		t.Error("a truncated read must say so, or it reads as the whole log")
	}
}

// TestLogResourceRedactsCredentials: the trace crosses a process boundary into another
// agent's context, one hop further than the file on disk ever went.
func TestLogResourceRedactsCredentials(t *testing.T) {
	redact.ResetSecretsForTest()
	t.Cleanup(redact.ResetSecretsForTest)

	logPath := filepath.Join(t.TempDir(), "session.log")
	if err := os.WriteFile(logPath,
		[]byte("mcp.call url=https://example.test auth=Bearer sk-or-v1-fake-test-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := newFakeRuntime("ses_test")
	fake.facts.LogPath = logPath
	cs, _ := connectWithResources(t, func(_, _ context.Context, _ OpenParams) (Runtime, error) { return fake, nil })
	sess := openSession(t, cs)
	fake.letFinish()

	body, err := readResource(t, cs, "daintree://session/"+sess.SessionID+"/log")
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if strings.Contains(body, "sk-or-v1-fake-test-secret") {
		t.Errorf("the log resource leaked a credential: %q", body)
	}
}

// TestLogResourceWithoutDebugLoggingExplainsItself: "not found" would send a caller
// hunting for a file; the real answer is that the session was opened without it.
func TestLogResourceWithoutDebugLoggingExplainsItself(t *testing.T) {
	fake := newFakeRuntime("ses_test")
	fake.facts.LogPath = ""
	cs, _ := connectWithResources(t, func(_, _ context.Context, _ OpenParams) (Runtime, error) { return fake, nil })
	sess := openSession(t, cs)
	fake.letFinish()

	_, err := readResource(t, cs, "daintree://session/"+sess.SessionID+"/log")
	if err == nil {
		t.Fatal("expected an error when there is no log")
	}
	if !strings.Contains(err.Error(), "debugLog") {
		t.Errorf("the error must name the remedy, got: %v", err)
	}
}

// TestResourceURIsAreValidated: a malformed or unknown URI must fail cleanly.
func TestResourceURIsAreValidated(t *testing.T) {
	fake := newFakeRuntime("ses_test")
	cs, _ := connectWithResources(t, func(_, _ context.Context, _ OpenParams) (Runtime, error) { return fake, nil })
	openSession(t, cs)
	fake.letFinish()

	for _, uri := range []string{
		"daintree://session//log",
		"daintree://session/ses_nope/log",
		"daintree://session/ses_test/run/",
		"daintree://session/ses_test/run/mrun_nope",
		"http://example.test/log",
	} {
		if _, err := readResource(t, cs, uri); err == nil {
			t.Errorf("%q must be rejected", uri)
		}
	}
}

// TestResourceTemplatesAreDiscoverable: a resource nobody can find is a resource nobody
// reads.
func TestResourceTemplatesAreDiscoverable(t *testing.T) {
	fake := newFakeRuntime("ses_test")
	cs, _ := connectWithResources(t, func(_, _ context.Context, _ OpenParams) (Runtime, error) { return fake, nil })
	fake.letFinish()

	res, err := cs.ListResourceTemplates(context.Background(), nil)
	if err != nil {
		t.Fatalf("list resource templates: %v", err)
	}
	found := map[string]bool{}
	for _, tmpl := range res.ResourceTemplates {
		found[tmpl.URITemplate] = true
		if strings.TrimSpace(tmpl.Description) == "" {
			t.Errorf("template %q has no description", tmpl.URITemplate)
		}
	}
	for _, want := range []string{
		runTranscriptURITemplate,
		"daintree://session/{sessionId}/log",
	} {
		if !found[want] {
			t.Errorf("template %q is not advertised", want)
		}
	}
}

// The paging query has to be in the TEMPLATE, or the SDK's regexp match rejects a paged
// URI and the whole feature is unreachable: the base resource answers, its `remaining`
// points at a continuation URI, and that URI matches no resource at all.
func TestTranscriptTemplateMatchesBothPagedAndUnpagedURIs(t *testing.T) {
	tmpl, err := uritemplate.New(runTranscriptURITemplate)
	if err != nil {
		t.Fatalf("the transcript template is not a valid URI template: %v", err)
	}
	for _, uri := range []string{
		"daintree://session/ses_1/run/mrun_1",
		"daintree://session/ses_1/run/mrun_1?fromSeq=500",
		"daintree://session/ses_1/run/mrun_1?limit=100",
		"daintree://session/ses_1/run/mrun_1?fromSeq=500&limit=100",
	} {
		if !tmpl.Regexp().MatchString(uri) {
			t.Errorf("the template does not match %q, so that read reaches no handler", uri)
		}
	}
}
