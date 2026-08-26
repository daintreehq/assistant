package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/daintreehq/assistant/internal/agent"
	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/config"
)

// failclosed_test.go pins the property the account layer exists to have: a layer that
// could not be CONSTRUCTED never lets a turn go out as an anonymous principal.
//
// The old behaviour was silent in both directions. NewAccountManager discarded its error
// and returned nil, accountTokenSource turned that nil into "no source", and the client
// substituted NoTokenSource — so RespondStream went out with no Authorization header at
// all. On a deployment whose door is open that SUCCEEDS, and this machine's local fault is
// attributed to whoever the open door resolves to; on one that enforces accounts it comes
// back as a server rejection naming a deployment that is working perfectly. The tests here
// assert the request is never SENT, which is the only version of this that is safe on both
// kinds of deployment.
//
// These drive the REAL backend.Client against an httptest endpoint rather than the
// fakeBackend override, because the fake replaces the very layer under test: it satisfies
// backend.Backend directly and never consults a token source.

// recordingEndpoint is a minimal Daintree-native backend that records what actually
// reached the wire. Counting requests is the assertion that matters — a fail-closed turn
// must produce zero of them, and "the response was an error" proves nothing about whether
// the conversation crossed the network first.
type recordingEndpoint struct {
	srv *httptest.Server

	mu       sync.Mutex
	calls    int
	auth     []string // the Authorization header of every respond request, in order
	capCalls int
	capAuth  []string // …and of every capabilities request
}

func newRecordingEndpoint(t *testing.T) *recordingEndpoint {
	t.Helper()
	e := &recordingEndpoint{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/daintree/respond", e.handleRespond)
	mux.HandleFunc("/v1/daintree/capabilities", e.handleCapabilities)
	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)
	return e
}

func (e *recordingEndpoint) handleRespond(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	e.calls++
	e.auth = append(e.auth, r.Header.Get("Authorization"))
	e.mu.Unlock()

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	write := func(event, data string) {
		_, _ = io.WriteString(w, "event: "+event+"\ndata: "+data+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}
	// meta ALWAYS first — the client refuses a stream that never sends one.
	write("meta", `{"protocol_version":3,"request_id":"req_1","model":"daintree-assistant",`+
		`"runbooks":{"active":[],"newly_loaded":[],"prelude":{"tool_executions":[]},`+
		`"selector":{"ran":false,"degraded":false}},"state":"dst1.test",`+
		`"catalog_revision":"sha256:test","prompt_version":"test","warnings":[]}`)
	write("delta", `{"content":"ok"}`)
	write("done", `{"finish_reason":"stop","usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0,"cached_tokens":0}}`)
}

// handleCapabilities is the endpoint the `--list-runbooks` probe actually reads. It is a
// PROTECTED path — it describes the deployment rather than the user, but it is not one of
// the client's public paths, so it does consult the token source.
func (e *recordingEndpoint) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	e.capCalls++
	e.capAuth = append(e.capAuth, r.Header.Get("Authorization"))
	e.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{}`)
}

func (e *recordingEndpoint) observed() (int, []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls, append([]string(nil), e.auth...)
}

func (e *recordingEndpoint) capabilitiesObserved() (int, []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.capCalls, append([]string(nil), e.capAuth...)
}

// newTurnApp builds a real App — real backend.Client, real token source — pointed at the
// recording endpoint, with stateRoot as its state root so a caller can break the account
// layer before boot.
func newTurnApp(t *testing.T, endpoint *recordingEndpoint, stateRoot string, apiKey string) *App {
	t.Helper()
	base := endpoint.srv.URL
	a, err := Create(CreateOptions{
		Overrides: config.ConfigOverrides{
			Offline:              boolPtr(true),
			StateDir:             &stateRoot,
			ProjectPath:          &stateRoot,
			Tier:                 strPtr("operator"),
			BackendURL:           &base,
			APIKey:               &apiKey,
			WorkflowIntelligence: boolPtr(false),
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = a.Shutdown() })
	return a
}

// THE test. A state root the auth directory cannot be created under, no caller key, and a
// normal turn: nothing reaches the backend at all.
func TestBrokenAccountLayerSendsNoBackendRequestOnATurn(t *testing.T) {
	endpoint := newRecordingEndpoint(t)
	root := brokenStateRoot(t)
	a := newTurnApp(t, endpoint, root, "")

	if a.Auth != nil {
		t.Fatal("a broken state root produced an account manager, so this test proves nothing")
	}
	reply, err := a.Session.Send(context.Background(), "hello", agent.SendOptions{})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if calls, _ := endpoint.observed(); calls != 0 {
		t.Fatalf("%d request(s) reached the backend; a fail-closed turn sends none", calls)
	}

	// The turn does not error — a recognised account failure is rendered as a reply, which
	// is what accountFailureAdvice exists for. What matters is WHICH reply: the local one.
	if !strings.HasPrefix(reply, "Account problem:") {
		t.Fatalf("the turn did not report an account problem at all:\n%s", reply)
	}
	// "The backend never rejected anything" is the credential_unavailable copy, and it is
	// the half that has to survive: the auth_required copy tells the user to sign in, over
	// a fault that sits on their own disk and would fail the sign-in at the same write.
	if !strings.Contains(reply, "never rejected") {
		t.Errorf("the reply reads as a backend rejection rather than a local failure:\n%s", reply)
	}
	// Neither the state root nor the misleading local code may reach a turn's prose.
	if strings.Contains(reply, root) {
		t.Errorf("the state-root path reached a turn's prose:\n%s", reply)
	}
	if strings.Contains(reply, "auth_exchange_failed") {
		t.Errorf("the raw auth code reached a turn's prose:\n%s", reply)
	}
}

// The same turn, one layer down, where the failure is still typed.
//
// credential_unavailable means nothing was sent; auth_required means the backend refused
// us. The distinction is what keeps doctor from reporting a rejection of a request that
// was never made, and it is decided by Client.credential aborting on a source error — so
// this asserts the source really is the erroring one and not NoTokenSource.
func TestBrokenAccountLayerRaisesALocalCredentialFailure(t *testing.T) {
	endpoint := newRecordingEndpoint(t)
	root := brokenStateRoot(t)
	a := newTurnApp(t, endpoint, root, "")

	_, err := a.Backend.RespondStream(context.Background(), backend.RespondRequest{
		Input: backend.RespondInput{Messages: []backend.Message{{Role: "user", Content: json.RawMessage(`"hello"`)}}},
	}, backend.StreamCallbacks{})
	if err == nil {
		t.Fatal("RespondStream succeeded with an unbuildable account layer")
	}
	if calls, _ := endpoint.observed(); calls != 0 {
		t.Fatalf("%d request(s) reached the backend; the credential aborts before the send", calls)
	}
	var be *backend.Error
	if !errors.As(err, &be) {
		t.Fatalf("not a typed backend error: %#v", err)
	}
	if be.Code != backend.CodeCredentialUnavailable {
		t.Fatalf("code = %q, want %q — an auth_required here would blame the deployment", be.Code, backend.CodeCredentialUnavailable)
	}

	// The CLASS survives the client's own envelope, so a consumer branches on the type
	// rather than on message text.
	if !IsAccountLayerFault(err) {
		t.Errorf("the failure is not recognisable as a construction fault: %v", err)
	}
	// …and the SENTENCE is the shared one. accountLayerFault/AccountFaultMessage is what
	// cli's account doctor row and commands' no-manager card both render, and each of
	// those suites pins its own copy; what could still drift is this path growing wording
	// of its own, which is what this pins.
	fault := a.AccountLayerFault()
	if fault == nil {
		t.Fatal("the App reports no account-layer fault, so there is nothing to agree with")
	}
	sentence := AccountFaultMessage(fault)
	if sentence == "" {
		t.Fatal("AccountFaultMessage rendered nothing for a real fault")
	}
	if !strings.Contains(err.Error(), sentence) {
		t.Errorf("the credential error says %q, which does not carry the shared diagnosis %q", err.Error(), sentence)
	}
	if strings.Contains(err.Error(), root) {
		t.Errorf("the state-root path reached the credential error: %q", err.Error())
	}
	if strings.Contains(err.Error(), "auth_exchange_failed") {
		t.Errorf("the raw auth code reached the credential error: %q", err.Error())
	}
}

// Healthy-signed-out is NOT a fault, and conflating the two would break every install
// there is: an account-optional deployment serves anonymous turns, and that is what every
// deployment does today.
func TestHealthySignedOutAccountLayerStillSendsTheTurn(t *testing.T) {
	endpoint := newRecordingEndpoint(t)
	a := newTurnApp(t, endpoint, t.TempDir(), "")

	if a.Auth == nil {
		t.Fatal("a writable state root produced no account manager")
	}
	if _, err := a.Session.Send(context.Background(), "hello", agent.SendOptions{}); err != nil {
		t.Fatalf("a healthy signed-out session could not run a turn: %v", err)
	}
	calls, auth := endpoint.observed()
	if calls != 1 {
		t.Fatalf("backend saw %d requests, want 1", calls)
	}
	if auth[0] != "" {
		t.Errorf("a signed-out session sent an Authorization header: %q", auth[0])
	}
}

// A caller key is a CHOICE. It replaces account identity for every request, so no manager
// is wanted and no auth directory is needed — and the fail-closed branch must not fire on
// a state root that would otherwise fault.
func TestCallerKeyStillAuthenticatesOnABrokenStateRoot(t *testing.T) {
	endpoint := newRecordingEndpoint(t)
	a := newTurnApp(t, endpoint, brokenStateRoot(t), "fake-caller-key-for-tests")

	if a.Auth != nil {
		t.Fatal("a caller key must leave App.Auth nil — two credentials, one winner")
	}
	if _, err := a.Session.Send(context.Background(), "hello", agent.SendOptions{}); err != nil {
		t.Fatalf("a caller-key session could not run a turn: %v", err)
	}
	calls, auth := endpoint.observed()
	if calls != 1 {
		t.Fatalf("backend saw %d requests, want 1", calls)
	}
	if auth[0] != "Bearer fake-caller-key-for-tests" {
		t.Errorf("Authorization = %q — the caller's key stopped reaching the wire", auth[0])
	}
}

// The discriminator itself, isolated from the turn. Three inputs, three outcomes, and the
// two nil returns are the pair that used to be one.
func TestCredentialSourceDiscriminatesTheTwoNilManagers(t *testing.T) {
	// A construction fault fails closed, carrying the shared diagnosis.
	broken := config.AppConfig{StateRoot: brokenStateRoot(t), BackendURL: "https://assistant.daintree.org"}
	src := credentialSource(broken, nil)
	unavailable, ok := src.(backend.UnavailableTokenSource)
	if !ok {
		t.Fatalf("a broken account layer resolved to %T, want an UnavailableTokenSource", src)
	}
	tok, err := unavailable.AccessToken(context.Background())
	if err == nil {
		t.Fatal("the fail-closed source handed out a credential")
	}
	if tok != "" {
		t.Errorf("the fail-closed source returned a token %q", tok)
	}
	if !IsAccountLayerFault(err) {
		t.Errorf("the fail-closed source's error is not a typed construction fault: %v", err)
	}
	if strings.Contains(err.Error(), broken.StateRoot) {
		t.Errorf("the state root reached the credential error: %q", err.Error())
	}

	// A caller key returns nil UNCHANGED, so backend.NewClient's own APIKey fallback
	// takes it. Anything typed here would win the client's TokenSource-beats-APIKey
	// preference and silently disable the key the operator exported.
	withKey := broken
	withKey.APIKey = "fake-caller-key-for-tests"
	if got := credentialSource(withKey, nil); got != nil {
		t.Errorf("a caller key resolved to %T, want nil so the key survives", got)
	}

	// A real manager is passed straight through — it is the observer too, and swapping in
	// anything else would leave backend verdicts landing nowhere.
	healthy := config.AppConfig{StateRoot: t.TempDir(), BackendURL: "https://assistant.daintree.org"}
	mgr := NewAccountManager(healthy)
	if mgr == nil {
		t.Fatal("a writable state root failed to produce a manager")
	}
	if got := credentialSource(healthy, accountTokenSource(mgr)); got != backend.TokenSource(mgr) {
		t.Errorf("a healthy manager resolved to %T, want the manager itself", got)
	}
}

// The `--list-runbooks` probe keeps working on a broken state root, which is the whole
// reason it names its own anonymous source. A catalog read that failed closed on a local
// auth fault would take a diagnostic offline exactly when someone is diagnosing.
//
// It reads /v1/daintree/capabilities, which is PROTECTED — not one of the client's public
// paths — so this is a real exercise of the token source rather than a path that skips it.
func TestProbeClientStaysAnonymousOnABrokenStateRoot(t *testing.T) {
	endpoint := newRecordingEndpoint(t)
	cfg := config.AppConfig{StateRoot: brokenStateRoot(t), BackendURL: endpoint.srv.URL}

	if _, err := NewProbeBackendClient(cfg).Capabilities(context.Background()); err != nil {
		t.Fatalf("the probe refused its own request over an account-layer fault: %v", err)
	}
	calls, auth := endpoint.capabilitiesObserved()
	if calls != 1 {
		t.Fatalf("the probe made %d capability requests, want 1", calls)
	}
	if auth[0] != "" {
		t.Errorf("an anonymous probe sent an Authorization header: %q", auth[0])
	}
}

// …and it must still carry a caller key when one is set. The probe passes a nil source in
// that case precisely so NewClient's APIKey fallback takes it: naming NoTokenSource
// unconditionally would win the client's TokenSource-beats-APIKey preference and silently
// run the probe as a different principal than a turn would.
func TestProbeClientStillCarriesACallerKey(t *testing.T) {
	endpoint := newRecordingEndpoint(t)
	cfg := config.AppConfig{
		StateRoot:  brokenStateRoot(t),
		BackendURL: endpoint.srv.URL,
		APIKey:     "fake-caller-key-for-tests",
	}

	if _, err := NewProbeBackendClient(cfg).Capabilities(context.Background()); err != nil {
		t.Fatalf("the probe failed with a caller key set: %v", err)
	}
	_, auth := endpoint.capabilitiesObserved()
	if auth[0] != "Bearer fake-caller-key-for-tests" {
		t.Errorf("Authorization = %q — the probe stopped sending the caller's key", auth[0])
	}
}
