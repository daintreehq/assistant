package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/config"
	"github.com/daintreehq/assistant/internal/credentials"
)

// capsServer answers /v1/daintree/capabilities, recording the Authorization header it
// was sent. authRequired mirrors the real backend: no bearer → 401 invalid_api_key.
type capsServer struct {
	*httptest.Server
	gotAuth string
	// verifyHits counts POSTs to the key-verification route, so a test can prove the
	// sign-in took the VERIFIED path rather than the loopback leniency path.
	verifyHits int
}

// newCapsServer serves BOTH halves of the sign-in contract: capabilities and a
// successful key verification.
//
// Serving verify matters even though this server is loopback (and would therefore be
// forgiven for omitting it — see backend.AllowsUnverifiedSignIn). Without it, every
// login test here would pass through the lenient downgrade branch, so the tests would
// prove only that an UNVERIFIED sign-in persists, and a regression that broke real
// verification would not fail a single one of them.
func newCapsServer(t *testing.T) *capsServer {
	t.Helper()
	cs := &capsServer{}
	cs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/daintree/capabilities":
			cs.gotAuth = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			if cs.gotAuth == "" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"error":{"code":"invalid_api_key","message":"missing key"}}`)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"server_version": "test-1.2.3",
				"protocol":       map[string]int{"min": 2, "max": 2},
			})
		case "/v1/daintree/auth/verify":
			cs.verifyHits++
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"valid": true, "label": "test-key"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(cs.Close)
	return cs
}

// loginCfg builds a config whose credentials file is inside an isolated temp dir, so
// no test can read or clobber the developer's real sign-in.
func loginCfg(t *testing.T) config.AppConfig {
	t.Helper()
	return config.AppConfig{CredentialsPath: credentials.Path(t.TempDir())}
}

// scriptedIO drives the flow from a canned script: the menu answers followed by the
// key, all on ONE stream — exactly how a piped `login` feeds it, and the arrangement
// that caught the read-ahead bug where a separate secret reader lost the key line.
func scriptedIO(answers []string, secret string) (LoginIO, *bytes.Buffer) {
	out := &bytes.Buffer{}
	script := strings.Join(append(append([]string{}, answers...), secret), "\n") + "\n"
	return ScriptedLoginIO(strings.NewReader(script), out), out
}

// The whole point of the endpoint menu: picking "custom" and typing a URL must be
// what gets verified AND persisted.
func TestRunLoginCustomEndpointIsVerifiedAndSaved(t *testing.T) {
	srv := newCapsServer(t)
	cfg := loginCfg(t)
	tio, out := scriptedIO([]string{"2", srv.URL}, "sk-or-v1-testkey0123456789")

	got, err := RunLogin(context.Background(), cfg, tio)
	if err != nil {
		t.Fatalf("login: %v (output:\n%s)", err, out.String())
	}
	if got.BaseURL != srv.URL {
		t.Fatalf("saved endpoint = %q, want %q", got.BaseURL, srv.URL)
	}
	// Verification must hit an AUTHENTICATED endpoint with the key attached — a health
	// probe would pass with a bogus key and defer the failure to the first real turn.
	if want := "Bearer sk-or-v1-testkey0123456789"; srv.gotAuth != want {
		t.Fatalf("capabilities Authorization = %q, want %q", srv.gotAuth, want)
	}
	// And it must go on to ask the PROVIDER, which is the only check that catches a
	// well-formed but wrong / revoked / unfunded key. A test that skipped this would
	// pass on the loopback leniency path and prove nothing about verification.
	if srv.verifyHits != 1 {
		t.Fatalf("key verification hit %d times, want exactly 1", srv.verifyHits)
	}
	if strings.Contains(out.String(), "can't check") {
		t.Fatalf("sign-in fell back to the unverified path:\n%s", out.String())
	}

	stored, ok, err := credentials.Load(cfg.CredentialsPath)
	if err != nil || !ok {
		t.Fatalf("credentials not persisted: ok=%v err=%v", ok, err)
	}
	if stored != got {
		t.Fatalf("stored %+v, want %+v", stored, got)
	}
	if strings.Contains(out.String(), "sk-or-v1-testkey0123456789") {
		t.Fatalf("login echoed the raw key:\n%s", out.String())
	}
}

// Choice 3 is the address the CLI used to hardcode; it must stay reachable now that
// the default points at production, since it is the whole local dev loop.
func TestRunLoginLocalChoiceUsesLocalBaseURL(t *testing.T) {
	cfg := loginCfg(t)
	tio, _ := scriptedIO([]string{"3"}, "sk-test-0123456789")

	// No server on the local port → verification fails, but the endpoint it TRIED is
	// what this test is about, and the error names it.
	_, err := RunLogin(context.Background(), cfg, tio)
	if err == nil {
		t.Skip("something is listening on the local backend port; endpoint selection unverifiable here")
	}
	if !strings.Contains(err.Error(), backend.LocalBaseURL) {
		t.Fatalf("local choice should target %s, got error %v", backend.LocalBaseURL, err)
	}
}

// A failed verification must NOT persist anything: writing an unverified sign-in would
// turn a typo into a broken cockpit on the next launch instead of an error here.
func TestRunLoginDoesNotSaveWhenVerificationFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"code":"invalid_api_key","message":"bad key"}}`)
	}))
	defer srv.Close()

	cfg := loginCfg(t)
	tio, _ := scriptedIO([]string{"2", srv.URL}, "sk-bogus-0123456789")

	if _, err := RunLogin(context.Background(), cfg, tio); err == nil {
		t.Fatal("login must fail when the backend rejects the key")
	}
	if _, ok, _ := credentials.Load(cfg.CredentialsPath); ok {
		t.Fatal("a rejected sign-in must not be persisted")
	}
}

// Re-running login to change ONLY the endpoint must not force the user to re-type a
// key they cannot see.
func TestRunLoginEmptyKeyKeepsTheStoredOne(t *testing.T) {
	srv := newCapsServer(t)
	cfg := loginCfg(t)
	const existing = "sk-or-v1-existing0123456789"
	if err := credentials.Save(cfg.CredentialsPath, credentials.Credentials{
		BaseURL: "https://old.example.test", APIKey: existing,
	}); err != nil {
		t.Fatal(err)
	}

	tio, out := scriptedIO([]string{"2", srv.URL}, "")
	got, err := RunLogin(context.Background(), cfg, tio)
	if err != nil {
		t.Fatalf("login: %v (output:\n%s)", err, out.String())
	}
	if got.APIKey != existing {
		t.Fatalf("key = %q, want the existing key kept", got.APIKey)
	}
	if got.BaseURL != srv.URL {
		t.Fatalf("endpoint = %q, want %q", got.BaseURL, srv.URL)
	}
	// The current sign-in should be shown, redacted, so the user knows what they have.
	if !strings.Contains(out.String(), credentials.Redact(existing)) {
		t.Fatalf("login should show the current key redacted:\n%s", out.String())
	}
	// And the PROMPT itself must advertise the default. Behaviour alone is not enough:
	// the key is invisible, so a user who cannot see that blank Enter keeps it re-pastes
	// a secret — or concludes the sign-in threw the stored one away.
	if !strings.Contains(out.String(), "press Enter to keep "+credentials.Redact(existing)) {
		t.Fatalf("the key prompt should say that Enter keeps the stored key:\n%s", out.String())
	}
	if strings.Contains(out.String(), existing) {
		t.Fatalf("login echoed the raw stored key:\n%s", out.String())
	}
}

// A stored key that cannot ride an HTTP header must not be offered as the blank-Enter
// default: credentials.Load only checks the fields are non-empty, so a hand-edited file
// can hold one. Offering it would send it to the network and fail with a transport-
// shaped error that never mentions the real cause — and blank Enter would keep picking
// the same unusable value.
func TestRunLoginRefusesToKeepAMalformedStoredKey(t *testing.T) {
	srv := newCapsServer(t)
	cfg := loginCfg(t)
	const malformed = "sk-or-v1-with a space in it"
	if err := credentials.Save(cfg.CredentialsPath, credentials.Credentials{
		BaseURL: "https://old.example.test", APIKey: malformed,
	}); err != nil {
		t.Fatal(err)
	}

	// Endpoint, custom URL, then a blank key line the flow must REFUSE, followed by a
	// usable one. A flow that still offered the stored key would take the blank line and
	// never read the replacement.
	const replacement = "sk-or-v1-replacement0123456789"
	tio, out := scriptedIO([]string{"2", srv.URL, ""}, replacement)
	got, err := RunLogin(context.Background(), cfg, tio)
	if err != nil {
		t.Fatalf("login: %v (output:\n%s)", err, out.String())
	}
	if got.APIKey != replacement {
		t.Fatalf("key = %q, want the replacement — the malformed default must not be kept", got.APIKey)
	}
	if !strings.Contains(out.String(), "the stored key is unusable") {
		t.Fatalf("login should explain why the stored key was dropped:\n%s", out.String())
	}
}

// REGRESSION: a piped login whose input runs out before the key must FAIL, not retry.
// The first cut looped forever printing "a key is required" — the read can never
// succeed once stdin is exhausted, so retrying is unbounded output and a hung process.
// (If this regresses, the test hangs until the package timeout rather than failing.)
func TestRunLoginExhaustedInputFailsInsteadOfLooping(t *testing.T) {
	srv := newCapsServer(t)
	cfg := loginCfg(t)
	out := &bytes.Buffer{}
	// Endpoint answers only — no key line follows.
	tio := ScriptedLoginIO(strings.NewReader("2\n"+srv.URL+"\n"), out)

	done := make(chan error, 1)
	go func() {
		_, err := RunLogin(context.Background(), cfg, tio)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a login with no key in the stream must fail")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("login did not terminate on exhausted input — it is looping")
	}
	if n := strings.Count(out.String(), "a key is required"); n > 1 {
		t.Fatalf("prompt repeated %d times on exhausted input; want at most 1", n)
	}
}

// Signing out must clear the file, and doing it twice must not error.
func TestRunLogoutRemovesTheSignIn(t *testing.T) {
	cfg := loginCfg(t)
	if err := credentials.Save(cfg.CredentialsPath, credentials.Credentials{
		BaseURL: "https://example.test", APIKey: "sk-test-0123456789",
	}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := RunLogout(cfg, &out); err != nil {
		t.Fatalf("logout: %v", err)
	}
	if _, ok, _ := credentials.Load(cfg.CredentialsPath); ok {
		t.Fatal("logout left credentials behind")
	}
	if err := RunLogout(cfg, &out); err != nil {
		t.Fatalf("second logout: %v", err)
	}
}

// The gate is what stands between a signed-out launch and a cockpit that 401s on the
// first message. Non-interactive callers must get the instruction, not a prompt.
func TestEnsureSignedInNonInteractiveErrorNamesTheCommand(t *testing.T) {
	t.Setenv("DAINTREE_ASSISTANT_STATE_DIR", t.TempDir())
	t.Setenv("DAINTREE_API_KEY", "")

	err := ensureSignedIn(context.Background(), config.ConfigOverrides{}, false)
	if err == nil {
		t.Fatal("a signed-out non-interactive launch must fail")
	}
	if !strings.Contains(err.Error(), "login") {
		t.Fatalf("error must point at the login command, got %v", err)
	}
}

func TestEnsureSignedInPassesWithAnEnvKey(t *testing.T) {
	t.Setenv("DAINTREE_ASSISTANT_STATE_DIR", t.TempDir())
	t.Setenv("DAINTREE_API_KEY", "sk-env-0123456789")

	if err := ensureSignedIn(context.Background(), config.ConfigOverrides{}, false); err != nil {
		t.Fatalf("an env key must satisfy the gate: %v", err)
	}
}

// The sign-in surfaces render the verdict a user actually reads. `ErrKeyRejected` is
// matched before the per-code branches further down, so a rejection carrying a precise
// reason has to be recognised THERE or it gets re-generalised to "check it is active and
// funded" — advice that covers all three account problems and resolves none of them.
func TestLoginCheckErrorNamesTheSpecificAccountProblem(t *testing.T) {
	cases := []struct {
		name    string
		code    string
		want    string
		notWant string
	}{
		{
			name:    "revoked key",
			code:    backend.CodeProviderInvalidAPIKey,
			want:    "does not recognise this key",
			notWant: "active and funded",
		},
		{
			name: "no credit",
			code: backend.CodeProviderInsufficientCredit,
			want: "no credit left",
		},
		{
			name: "not permitted for this model",
			code: backend.CodeProviderKeyForbidden,
			want: "model permissions",
			// The one case where re-entering the same key is guaranteed not to help.
			notWant: "active and funded",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wrapped := fmt.Errorf("%w: %w", backend.ErrKeyRejected, &backend.Error{
				HTTPStatus: 401, Code: tc.code, Message: "nope",
			})
			got := loginCheckError("https://assistant.example", wrapped, "sk-or-v1-test-key").Error()
			if !strings.Contains(got, tc.want) {
				t.Errorf("message %q does not contain %q", got, tc.want)
			}
			if tc.notWant != "" && strings.Contains(got, tc.notWant) {
				t.Errorf("message %q still carries the generic advice %q", got, tc.notWant)
			}
		})
	}
}

// A rejection with no per-code reason — an older backend, or a verdict from the 200
// {"valid": false} path — must still get the generic advice rather than an empty clause.
func TestLoginCheckErrorKeepsGenericAdviceWithoutACode(t *testing.T) {
	got := loginCheckError("https://assistant.example", backend.ErrKeyRejected, "sk-or-v1-test-key").Error()
	if !strings.Contains(got, "active and funded") {
		t.Errorf("message %q lost the generic fallback advice", got)
	}
}

// The disclosure must be on screen BEFORE the key is typed, not merely present in the
// final transcript. Asserted at the moment `ReadSecret` is called — a flow that read the
// key first and printed the notice after would satisfy any check on the finished output
// while giving the user nothing to act on.
//
// It also pins the RESOLVED endpoint, exactly. A custom URL is normalized on the way
// through (trailing slash dropped), so what was typed is not necessarily what the
// spendable key is about to be sent to.
func TestRunLoginNamesTheKeyAndItsBillingBeforeAsking(t *testing.T) {
	srv := newCapsServer(t)
	cfg := loginCfg(t)

	out := &bytes.Buffer{}
	answers := []string{"2", srv.URL + "/"} // trailing slash: normalization has work to do
	var atSecret string
	i := 0
	tio := LoginIO{
		Out: out,
		ReadLine: func() (string, error) {
			if i >= len(answers) {
				return "", io.EOF
			}
			v := answers[i]
			i++
			return v, nil
		},
		ReadSecret: func() (string, error) {
			atSecret = out.String() // everything the user could see at this instant
			return "sk-or-v1-testkey0123456789", nil
		},
	}

	if _, err := RunLogin(context.Background(), cfg, tio); err != nil {
		t.Fatalf("login: %v (output:\n%s)", err, out.String())
	}
	if atSecret == "" {
		t.Fatal("ReadSecret was never called")
	}

	// The SHARED sentence, not an invented paraphrase — both surfaces render this exact
	// constant, and asserting fragments would let the wording drift into something that
	// no longer says who pays.
	if !strings.Contains(atSecret, backend.KeyPurposeNotice) {
		t.Errorf("the billing notice was not on screen when the key was requested:\n%s", atSecret)
	}
	if !strings.Contains(atSecret, "OpenRouter API key") {
		t.Errorf("the prompt never says which kind of key it wants:\n%s", atSecret)
	}
	// Exact line: srv.URL is a PREFIX of srv.URL+"/", so a Contains on the bare URL
	// would pass even if the trailing slash had survived normalization.
	if !strings.Contains(atSecret, "\nSigning in to "+srv.URL+"\n") {
		t.Errorf("the resolved endpoint line %q is missing:\n%s", "Signing in to "+srv.URL, atSecret)
	}
}

// A provider label that is really a truncated copy of the key must not be printed.
// OpenRouter defaults a key's label to exactly that, and ScrubKey only catches the
// complete bearer — so without suppression a partial secret lands in the host's native
// scrollback, which the cockpit never clears.
func TestKeyLabelSuffixSuppressesAKeyShapedLabel(t *testing.T) {
	const key = "sk-or-v1-abcdef0123456789abcdef0123456789c99"
	limit := 15.5

	leaky := keyLabelSuffix(backend.KeyVerification{Label: "sk-or-v1-abcdef0123…89c99"}, key)
	if strings.Contains(leaky, "abcdef0123") {
		t.Errorf("a truncated copy of the key was printed: %q", leaky)
	}

	// A human-chosen label is useful — it confirms WHICH key was pasted — and must
	// survive.
	kept := keyLabelSuffix(backend.KeyVerification{Label: "work laptop", LimitRemaining: &limit}, key)
	if !strings.Contains(kept, "work laptop") {
		t.Errorf("a genuine label was suppressed: %q", kept)
	}
	if !strings.Contains(kept, "15.50 remaining") {
		t.Errorf("the remaining limit was dropped: %q", kept)
	}

	// With the label gone, the remaining-limit half still renders on its own rather than
	// leaving an empty "()".
	onlyLimit := keyLabelSuffix(backend.KeyVerification{Label: "sk-or-v1-abcdef0123…89c99", LimitRemaining: &limit}, key)
	if strings.Contains(onlyLimit, "()") || strings.Contains(onlyLimit, "abcdef") {
		t.Errorf("malformed or leaky suffix: %q", onlyLimit)
	}
	if !strings.Contains(onlyLimit, "15.50 remaining") {
		t.Errorf("the remaining limit was dropped with the label: %q", onlyLimit)
	}
}
