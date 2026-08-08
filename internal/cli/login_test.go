package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/app"
	"github.com/daintreehq/daintree-assistant/internal/backend"
	"github.com/daintreehq/daintree-assistant/internal/cli/render"
	"github.com/daintreehq/daintree-assistant/internal/config"
	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// login_test.go locks the two-question login flow: default vs custom endpoint,
// re-prompt on garbage, EOF aborts without touching an existing file, and the
// non-interactive gate never fires with the env escape hatch set.

// scripted returns a read func that pops the given answers in order, then
// reports EOF (ok=false) — exactly how lineReader.Read behaves on closed stdin.
func scripted(answers ...string) func(context.Context) (string, bool) {
	i := 0
	return func(context.Context) (string, bool) {
		if i >= len(answers) {
			return "", false
		}
		a := answers[i]
		i++
		return a, true
	}
}

func loginSink() *render.Renderer { return render.New(&bytes.Buffer{}) }

func TestRunLoginFlowDefaultEndpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	// Enter (default endpoint), then the key.
	if !runLoginFlow(context.Background(), loginSink(), scripted("", "sk-or-v1-abc"), path) {
		t.Fatal("login flow must succeed")
	}
	creds, ok, err := config.LoadCredentials(path)
	if err != nil || !ok {
		t.Fatalf("saved credentials unreadable: (%v, %v)", ok, err)
	}
	if creds.Endpoint != backend.DefaultBaseURL || creds.APIKey != "sk-or-v1-abc" {
		t.Fatalf("saved %+v, want default endpoint + key", creds)
	}
}

func TestRunLoginFlowCustomEndpointRepromptsInvalid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	answers := scripted(
		"2",                                // custom
		"not a url",                        // invalid → re-prompt
		"ftp://x.example.com",              // wrong scheme → re-prompt
		"https://backend.example.com/api/", // valid, trailing slash normalized away
		"has spaces",                       // invalid key → re-prompt
		"",                                 // empty key → re-prompt
		"tok-123",                          // valid key
	)
	if !runLoginFlow(context.Background(), loginSink(), answers, path) {
		t.Fatal("login flow must succeed after re-prompts")
	}
	creds, _, _ := config.LoadCredentials(path)
	if creds.Endpoint != "https://backend.example.com/api" || creds.APIKey != "tok-123" {
		t.Fatalf("saved %+v", creds)
	}
}

func TestRunLoginFlowAcceptsPastedURLAtFirstQuestion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	if !runLoginFlow(context.Background(), loginSink(), scripted("https://pasted.example.com", "k1"), path) {
		t.Fatal("pasting the URL at the choice prompt must work")
	}
	creds, _, _ := config.LoadCredentials(path)
	if creds.Endpoint != "https://pasted.example.com" {
		t.Fatalf("saved endpoint %q", creds.Endpoint)
	}
}

func TestRunLoginFlowEOFLeavesExistingFileUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	existing := config.Credentials{Endpoint: "https://keep.example.com", APIKey: "keep-key"}
	if err := config.SaveCredentials(path, existing); err != nil {
		t.Fatal(err)
	}
	// EOF at the endpoint question, and EOF at the key question — both abort.
	for _, answers := range [][]string{{}, {"2"}, {""}} {
		if runLoginFlow(context.Background(), loginSink(), scripted(answers...), path) {
			t.Fatalf("EOF after %v must abort the flow", answers)
		}
		creds, ok, err := config.LoadCredentials(path)
		if err != nil || !ok || creds != existing {
			t.Fatalf("aborted flow touched the file: (%+v, %v, %v)", creds, ok, err)
		}
	}
}

func TestRunLoginFlowNeverEchoesTheKey(t *testing.T) {
	var buf bytes.Buffer
	r := render.New(&buf)
	path := filepath.Join(t.TempDir(), "credentials.json")
	if !runLoginFlow(context.Background(), r, scripted("", "sk-or-v1-supersecret"), path) {
		t.Fatal("login flow must succeed")
	}
	if strings.Contains(buf.String(), "sk-or-v1-supersecret") {
		t.Fatal("the flow's own output must never contain the key")
	}
}

// The /login restart loop end-to-end: a cockpit surface that requests login
// (domain.ErrLoginRequested) hands the terminal back, the flow reads the new
// endpoint + key from stdin, and the SECOND app in the same runInteractive call
// is built against the freshly-saved credentials.
func TestRunInteractive_LoginRestartRebuildsApp(t *testing.T) {
	t.Setenv("DAINTREE_ASSISTANT_STATE_DIR", t.TempDir())
	t.Setenv(NoDaemonEnv, "1")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("DAINTREE_BACKEND_URL", "")
	// The isolated HOME pushes the derived control-socket path past the unix
	// socket limit — pin it to a short dir instead.
	sock, err := os.MkdirTemp("/tmp", "dts")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sock) })
	t.Setenv("DAINTREE_ASSISTANT_SOCKET_DIR", sock)
	// Seed a complete login so the first-run gate does not consume the script.
	if err := config.SaveCredentials(config.DefaultCredentialsPath(),
		config.Credentials{Endpoint: "https://old.example.com", APIKey: "old-key"}); err != nil {
		t.Fatal(err)
	}

	// Scripted stdin for the /login flow: custom endpoint, URL, key.
	stdin, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString("2\nhttps://new.example.com\nnew-key\n"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	originalStdin := os.Stdin
	os.Stdin = stdin
	t.Cleanup(func() {
		os.Stdin = originalStdin
		_ = stdin.Close()
	})

	var urls []string
	opts := Options{
		Offline: true,
		Project: t.TempDir(),
		Cockpit: func(_ context.Context, a *app.App) error {
			urls = append(urls, a.Backend.BaseURL())
			if len(urls) == 1 {
				return domain.ErrLoginRequested
			}
			return nil
		},
	}
	if code := runInteractive(context.Background(), opts, true); code != 0 {
		t.Fatalf("login-restart run exit = %d, want 0", code)
	}
	if len(urls) != 2 || urls[0] != "https://old.example.com" || urls[1] != "https://new.example.com" {
		t.Fatalf("surface saw backend URLs %v, want [old, new]", urls)
	}
	creds, ok, err := config.LoadCredentials(config.DefaultCredentialsPath())
	if err != nil || !ok || creds.APIKey != "new-key" {
		t.Fatalf("persisted credentials after /login = (%+v, %v, %v)", creds, ok, err)
	}
}

func TestBackendLoginNeededRespectsEnvEscapeHatch(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // no credential file
	t.Setenv("DAINTREE_BACKEND_URL", "http://127.0.0.1:9999")
	if backendLoginNeeded() {
		t.Fatal("DAINTREE_BACKEND_URL set → the gate must never fire (e2e would deadlock)")
	}
	t.Setenv("DAINTREE_BACKEND_URL", "")
	if !backendLoginNeeded() {
		t.Fatal("no env, no credentials → the gate must fire")
	}
	if err := config.SaveCredentials(config.DefaultCredentialsPath(),
		config.Credentials{Endpoint: "https://x.example.com", APIKey: "k"}); err != nil {
		t.Fatal(err)
	}
	if backendLoginNeeded() {
		t.Fatal("complete persisted login → the gate must not fire")
	}
}
