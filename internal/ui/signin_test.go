package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/daintreehq/assistant/internal/app"
	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/config"
	"github.com/daintreehq/assistant/internal/credentials"
	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/tools"
)

// signInModel is the harness model with the sign-in sheet already up, standing in for
// openSignIn (which needs a live App). The sheet's own mechanics are what these tests
// cover; the verify/swap behind it is tested in internal/app.
func signInModel(t *testing.T, current app.SignInStatus) Model {
	t.Helper()
	m := harnessModel()
	m.pendingSignIn = &pendingSignIn{current: current, currentKeySet: current.SignedIn}
	return m
}

func key(r rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: r, Text: string(r)}
}

func code(c rune) tea.KeyPressMsg { return tea.KeyPressMsg{Code: c} }

// The sheet must REPLACE the composer, exactly like the approval and question sheets —
// two visible input surfaces would leave no way to tell which has focus.
func TestSignInSheetReplacesTheComposer(t *testing.T) {
	// Strip ANSI before matching: the composer renders its placeholder with the first
	// character separately styled, so a raw substring check for it silently never
	// matches and the guard below would pass whether or not the composer is showing.
	// The idle placeholder is composer-only chrome (the masthead never says it), so its
	// presence is a reliable "the composer is on screen" probe.
	const composerCue = "Ask Daintree"
	if base := stripAnsi(harnessModel().View().Content); !strings.Contains(base, composerCue) {
		t.Fatalf("harness precondition: composer cue %q not found — this test cannot detect it:\n%s", composerCue, base)
	}

	m := signInModel(t, app.SignInStatus{})
	out := stripAnsi(m.View().Content)

	if !strings.Contains(out, "Daintree sign-in") {
		t.Fatalf("sheet not rendered:\n%s", out)
	}
	if strings.Contains(out, composerCue) {
		t.Fatalf("composer still visible beneath the sheet:\n%s", out)
	}
	// Every offered endpoint must be listed, or the menu silently loses an option.
	for _, c := range backend.EndpointChoices {
		if !strings.Contains(out, c.Label) {
			t.Errorf("endpoint %q missing from the sheet:\n%s", c.Label, out)
		}
	}
}

// SECURITY: the key must never reach the screen. The cockpit runs on the NORMAL screen
// buffer, so anything rendered also lands in the host's scrollback — a leaked key would
// persist in the terminal long after the session.
func TestSignInKeyIsMaskedInTheView(t *testing.T) {
	m := signInModel(t, app.SignInStatus{})
	s := m.pendingSignIn
	s.stage, s.baseURL = signInStageKey, "https://endpoint.test"
	s.keyInput = "sk-or-v1-supersecret"

	out := stripAnsi(m.View().Content)
	if strings.Contains(out, "supersecret") {
		t.Fatalf("the raw key was rendered:\n%s", out)
	}
	if !strings.Contains(out, "•") {
		t.Fatalf("masked field should render bullets:\n%s", out)
	}
	// The count is how a user confirms a paste landed without seeing the key.
	if !strings.Contains(out, "(20)") {
		t.Fatalf("masked field should show the character count:\n%s", out)
	}
	// The key prompt names the endpoint it is about to send the key to.
	if !strings.Contains(out, "https://endpoint.test") {
		t.Fatalf("key stage must name the target endpoint:\n%s", out)
	}
}

// Picking "Custom" opens the URL field; anything else jumps straight to the key.
func TestSignInEndpointSelectionRouting(t *testing.T) {
	m := signInModel(t, app.SignInStatus{})
	next, _ := m.onSignInKey(key('2')) // 2 = Custom
	m = next.(Model)
	if m.pendingSignIn.stage != signInStageCustomURL {
		t.Fatalf("Custom must open the URL field, got stage %d", m.pendingSignIn.stage)
	}

	m = signInModel(t, app.SignInStatus{})
	next, _ = m.onSignInKey(key('3')) // 3 = Local
	m = next.(Model)
	if m.pendingSignIn.stage != signInStageKey {
		t.Fatalf("a fixed endpoint must skip to the key, got stage %d", m.pendingSignIn.stage)
	}
	if m.pendingSignIn.baseURL != backend.LocalBaseURL {
		t.Fatalf("baseURL = %q, want %q", m.pendingSignIn.baseURL, backend.LocalBaseURL)
	}
}

// A bad URL must be caught at the field with a readable reason, keeping the sheet up —
// not sent off to fail as a confusing transport error.
func TestSignInRejectsABadCustomURLInline(t *testing.T) {
	m := signInModel(t, app.SignInStatus{})
	m.pendingSignIn.stage = signInStageCustomURL
	m.pendingSignIn.urlInput = "ftp://nope.test"

	next, _ := m.onSignInKey(code(tea.KeyEnter))
	m = next.(Model)
	if m.pendingSignIn.stage != signInStageCustomURL {
		t.Fatal("a rejected URL must keep the field open")
	}
	if m.pendingSignIn.errMsg == "" {
		t.Fatal("a rejected URL must explain why")
	}
	if !strings.Contains(stripAnsi(m.View().Content), "scheme") {
		t.Fatalf("the reason should be visible in the sheet:\n%s", stripAnsi(m.View().Content))
	}
}

// A typed URL is normalized on the way through, so a bare host or a trailing slash
// cannot produce a subtly wrong base URL.
func TestSignInNormalizesTheTypedURL(t *testing.T) {
	m := signInModel(t, app.SignInStatus{})
	m.pendingSignIn.stage = signInStageCustomURL
	m.pendingSignIn.urlInput = "example.test/"

	next, _ := m.onSignInKey(code(tea.KeyEnter))
	m = next.(Model)
	if got := m.pendingSignIn.baseURL; got != "https://example.test" {
		t.Fatalf("baseURL = %q, want https://example.test", got)
	}
}

// Esc steps BACK rather than discarding the sheet: a mistyped URL should not also cost
// the endpoint choice. Only Esc at the first stage cancels.
func TestSignInEscStepsBackThenCancels(t *testing.T) {
	m := signInModel(t, app.SignInStatus{})
	m.pendingSignIn.stage = signInStageKey

	next, _ := m.onSignInKey(code(tea.KeyEscape))
	m = next.(Model)
	if m.pendingSignIn == nil {
		t.Fatal("Esc from the key stage must not close the sheet")
	}
	if m.pendingSignIn.stage != signInStageEndpoint {
		t.Fatalf("Esc should step back to the endpoint stage, got %d", m.pendingSignIn.stage)
	}

	next, _ = m.onSignInKey(code(tea.KeyEscape))
	m = next.(Model)
	if m.pendingSignIn != nil {
		t.Fatal("Esc at the first stage must cancel the sheet")
	}
}

// Typed text edits the field, and backspace removes exactly one rune.
func TestSignInTextEntryAndBackspace(t *testing.T) {
	m := signInModel(t, app.SignInStatus{})
	m.pendingSignIn.stage = signInStageKey

	for _, r := range "abc" {
		next, _ := m.onSignInKey(key(r))
		m = next.(Model)
	}
	if got := m.pendingSignIn.keyInput; got != "abc" {
		t.Fatalf("keyInput = %q, want abc", got)
	}
	next, _ := m.onSignInKey(code(tea.KeyBackspace))
	m = next.(Model)
	if got := m.pendingSignIn.keyInput; got != "ab" {
		t.Fatalf("after backspace keyInput = %q, want ab", got)
	}
}

// An empty key with nothing stored is an error, not a silent no-op sign-in.
func TestSignInEmptyKeyWithNoStoredKeyIsRejected(t *testing.T) {
	m := signInModel(t, app.SignInStatus{}) // signed out ⇒ currentKeySet false
	m.pendingSignIn.stage = signInStageKey
	m.pendingSignIn.baseURL = "https://endpoint.test"

	next, _ := m.onSignInKey(code(tea.KeyEnter))
	m = next.(Model)
	if m.pendingSignIn.stage == signInStageVerifying {
		t.Fatal("an empty key must not start a verification")
	}
	if m.pendingSignIn.errMsg == "" {
		t.Fatal("an empty key must explain that one is required")
	}
}

// The "keep the current key" default has to be VISIBLE at the field, not just in the
// dim key legend — otherwise hopping between the local and deployed backend looks like
// it demands a re-paste of a secret the user cannot see, and the sheet reads as though
// it discarded the stored key.
func TestSignInKeyStageOffersToKeepTheCurrentKey(t *testing.T) {
	const stored = "sk-or-v1-storedkey0123456789"
	redacted := credentials.Redact(stored)
	m := signInModel(t, app.SignInStatus{SignedIn: true, Endpoint: backend.DefaultBaseURL, KeyRedacted: redacted})
	m.pendingSignIn.stage = signInStageKey
	m.pendingSignIn.baseURL = backend.LocalBaseURL

	out := stripAnsi(m.View().Content)
	if !strings.Contains(out, "blank") || !strings.Contains(out, "keep your current key") {
		t.Fatalf("key stage must say that an empty field keeps the current key:\n%s", out)
	}
	// The empty field itself must say what Enter would do — a blank row reads as
	// "nothing entered, nothing will happen".
	if !strings.Contains(out, "Enter keeps the current key)") {
		t.Fatalf("empty key field must say what Enter would do:\n%s", out)
	}
	// A signed-out sheet must NOT make the offer — there is nothing to keep, and an
	// empty Enter there is an error, not a shortcut.
	out2 := signInModel(t, app.SignInStatus{})
	out2.pendingSignIn.stage = signInStageKey
	out2.pendingSignIn.baseURL = backend.LocalBaseURL
	plain := stripAnsi(out2.View().Content)
	// Anchor on something the signed-out key stage MUST render, or the absence checks
	// below would also pass on a sheet that stopped drawing entirely.
	if !strings.Contains(plain, "API key for "+backend.LocalBaseURL) {
		t.Fatalf("precondition: the signed-out key stage did not render:\n%s", plain)
	}
	if strings.Contains(plain, "keep your current key") || strings.Contains(plain, "Enter keeps") {
		t.Fatalf("a signed-out sheet must not offer to keep a key:\n%s", plain)
	}
}

// The positive half of the promise the sheet now makes loudly: blank Enter with a key
// in force must RESOLVE that key and start verifying, not stall on "a key is required".
// The two halves are asserted separately because the resolver is what can silently
// break — the sheet would still look correct while Enter did nothing useful.
func TestSignInEmptyKeyResolvesTheCurrentKeyAndVerifies(t *testing.T) {
	const stored = "sk-or-v1-storedkey0123456789"
	m := signInModel(t, app.SignInStatus{SignedIn: true, Endpoint: backend.DefaultBaseURL,
		KeyRedacted: credentials.Redact(stored)})
	m.controller.app = &app.App{Config: config.AppConfig{APIKey: stored}}
	m.pendingSignIn.stage = signInStageKey
	m.pendingSignIn.baseURL = backend.LocalBaseURL

	// The resolver itself: this is the value submitSignIn hands to App.SignIn.
	got, err := m.controller.currentAPIKey()
	if err != nil {
		t.Fatalf("currentAPIKey: %v", err)
	}
	if got != stored {
		t.Fatalf("currentAPIKey = %q, want the key in force", got)
	}

	next, cmd := m.onSignInKey(code(tea.KeyEnter))
	m = next.(Model)
	if m.pendingSignIn.errMsg != "" {
		t.Fatalf("blank Enter with a key in force must not error: %q", m.pendingSignIn.errMsg)
	}
	if m.pendingSignIn.stage != signInStageVerifying {
		t.Fatalf("stage = %d, want verifying — blank Enter should have started the sign-in", m.pendingSignIn.stage)
	}
	if cmd == nil {
		t.Fatal("blank Enter must dispatch the verify command")
	}
}

// Typing a replacement must retire the keep-offer everywhere it appears: the sheet
// stays up while the key is entered, so a standing "Enter keeps the current key" would
// be describing the opposite of what Enter is about to do — with a secret the user
// cannot read back to catch the mistake.
func TestSignInTypedKeyRetiresTheKeepOffer(t *testing.T) {
	const stored = "sk-or-v1-storedkey0123456789"
	m := signInModel(t, app.SignInStatus{SignedIn: true, Endpoint: backend.DefaultBaseURL,
		KeyRedacted: credentials.Redact(stored)})
	m.pendingSignIn.stage = signInStageKey
	m.pendingSignIn.baseURL = backend.LocalBaseURL
	m.pendingSignIn.keyInput = "sk-or-v1-replacement"

	out := stripAnsi(m.View().Content)
	if strings.Contains(out, "Enter keeps the current key)") {
		t.Errorf("the field placeholder must give way to the typed key:\n%s", out)
	}
	if !strings.Contains(out, "Enter sign in") {
		t.Errorf("the legend must say Enter signs in once a replacement is typed:\n%s", out)
	}
	// Neither key may reach the screen — the cockpit paints into host scrollback.
	if strings.Contains(out, stored) || strings.Contains(out, "sk-or-v1-replacement") {
		t.Errorf("a raw key was rendered:\n%s", out)
	}
}

// submitSignIn TRIMS before deciding, so a field holding only spaces still keeps the
// current key. The legend has to agree with the code that acts, or it tells the user
// Enter will send a replacement that does not exist.
func TestSignInWhitespaceOnlyKeyStillReadsAsKeep(t *testing.T) {
	m := signInModel(t, app.SignInStatus{SignedIn: true, Endpoint: backend.DefaultBaseURL,
		KeyRedacted: credentials.Redact("sk-or-v1-storedkey0123456789")})
	m.pendingSignIn.stage = signInStageKey
	m.pendingSignIn.baseURL = backend.LocalBaseURL
	m.pendingSignIn.keyInput = " "

	if got := signInHint(m.pendingSignIn); !strings.Contains(got, "keeps the current key") {
		t.Fatalf("legend = %q, want the keep wording for a whitespace-only field", got)
	}
}

// Both halves of the keep-offer must survive a realistically narrow sheet. The accent
// line is capWrapped (it ELLIPSIZES past its row budget) and the field placeholder is a
// single unwrappable row — an earlier, longer placeholder lost its tail at 40 columns,
// which is precisely where a truncated instruction is least recoverable.
func TestSignInKeepOfferSurvivesNarrowWidths(t *testing.T) {
	redacted := credentials.Redact("sk-or-v1-storedkey0123456789")
	for _, width := range []int{40, 50, 60, 80} {
		m := testModel(width)
		m.pendingSignIn = &pendingSignIn{
			current:       app.SignInStatus{SignedIn: true, Endpoint: backend.DefaultBaseURL, KeyRedacted: redacted},
			currentKeySet: true,
			stage:         signInStageKey,
			baseURL:       backend.LocalBaseURL,
		}
		// Wrapping inserts newlines, so compare on a whitespace-collapsed form.
		flat := strings.Join(strings.Fields(stripAnsi(m.View().Content)), " ")
		if want := "Leave this blank and press Enter to keep your current key (" + redacted + ")."; !strings.Contains(flat, want) {
			t.Errorf("width %d: the keep offer is ellipsized:\n%s", width, flat)
		}
		if !strings.Contains(flat, "(Enter keeps the current key)") {
			t.Errorf("width %d: the field placeholder is truncated:\n%s", width, flat)
		}
	}
}

// While verifying, the request is in flight and cannot be steered — keystrokes must be
// swallowed rather than queued onto whatever stage comes next.
func TestSignInIgnoresInputWhileVerifying(t *testing.T) {
	m := signInModel(t, app.SignInStatus{})
	m.pendingSignIn.stage = signInStageVerifying
	m.pendingSignIn.keyInput = "abc"

	next, _ := m.onSignInKey(key('z'))
	m = next.(Model)
	if m.pendingSignIn.keyInput != "abc" {
		t.Fatalf("input during verification leaked into the field: %q", m.pendingSignIn.keyInput)
	}
	if m.pendingSignIn.stage != signInStageVerifying {
		t.Fatal("a keystroke must not move the sheet off the verifying stage")
	}
}

// A failed attempt returns to the key stage with the reason inline, so the user can fix
// it without losing the sheet and retyping the endpoint.
func TestSignInResultFailureKeepsTheSheetWithTheReason(t *testing.T) {
	m := signInModel(t, app.SignInStatus{})
	m.pendingSignIn.stage = signInStageVerifying
	m.pendingSignIn.baseURL = "https://endpoint.test"

	next, _ := m.onSignInResult(SignInResultMsg{Endpoint: "https://endpoint.test", Err: errTest})
	m = next.(Model)
	if m.pendingSignIn == nil {
		t.Fatal("a failed sign-in must keep the sheet open")
	}
	if m.pendingSignIn.stage != signInStageKey {
		t.Fatalf("a failure should return to the key stage, got %d", m.pendingSignIn.stage)
	}
	if !strings.Contains(stripAnsi(m.View().Content), errTest.Error()) {
		t.Fatalf("the failure reason must be visible:\n%s", stripAnsi(m.View().Content))
	}
}

// Success pops the sheet and says when the change takes effect — an in-flight turn
// finishes on the old client, so "from your next message" is the honest promise.
func TestSignInResultSuccessClosesTheSheet(t *testing.T) {
	m := signInModel(t, app.SignInStatus{})
	m.pendingSignIn.stage = signInStageVerifying

	next, _ := m.onSignInResult(SignInResultMsg{Endpoint: "https://endpoint.test"})
	m = next.(Model)
	if m.pendingSignIn != nil {
		t.Fatal("a successful sign-in must close the sheet")
	}
}

var errTest = testError("endpoint.test rejected the key")

type testError string

func (e testError) Error() string { return string(e) }

// REGRESSION: pasting is how an API key is actually entered. tea.PasteMsg was routed
// only to the composer, which the sheet hides — so a pasted key vanished with no
// feedback and the field stayed empty.
func TestSignInAcceptsAPastedKey(t *testing.T) {
	m := signInModel(t, app.SignInStatus{})
	m.pendingSignIn.stage = signInStageKey

	next, _ := m.Update(tea.PasteMsg{Content: "sk-or-v1-pastedkey0123456789"})
	m = next.(Model)
	if got := m.pendingSignIn.keyInput; got != "sk-or-v1-pastedkey0123456789" {
		t.Fatalf("pasted key = %q, want it in the field", got)
	}
}

// A key copied from a browser or a wrapped terminal routinely carries a trailing
// newline or an embedded break. Stripping it here keeps the shape check from reporting
// a perfectly good key as malformed.
func TestSignInPasteStripsWhitespace(t *testing.T) {
	m := signInModel(t, app.SignInStatus{})
	m.pendingSignIn.stage = signInStageKey

	next, _ := m.Update(tea.PasteMsg{Content: "sk-or-v1-split\nkey  \n"})
	m = next.(Model)
	if got := m.pendingSignIn.keyInput; got != "sk-or-v1-splitkey" {
		t.Fatalf("pasted key = %q, want whitespace stripped", got)
	}
	if err := credentials.ValidateKeyShape(m.pendingSignIn.keyInput); err != nil {
		t.Fatalf("a cleaned paste must pass the shape check: %v", err)
	}
}

// A paste into the custom-URL field works the same way.
func TestSignInAcceptsAPastedURL(t *testing.T) {
	m := signInModel(t, app.SignInStatus{})
	m.pendingSignIn.stage = signInStageCustomURL

	next, _ := m.Update(tea.PasteMsg{Content: "https://backend.test\n"})
	m = next.(Model)
	if got := m.pendingSignIn.urlInput; got != "https://backend.test" {
		t.Fatalf("pasted URL = %q", got)
	}
}

// A paste must not leak into the composer's buffer while the sheet owns input.
func TestSignInPasteDoesNotReachTheComposer(t *testing.T) {
	m := signInModel(t, app.SignInStatus{})
	m.pendingSignIn.stage = signInStageKey

	next, _ := m.Update(tea.PasteMsg{Content: "sk-or-v1-secretpaste"})
	m = next.(Model)
	if strings.Contains(m.composer.Value(), "secretpaste") {
		t.Fatalf("the pasted key reached the composer: %q", m.composer.Value())
	}
}

// SECURITY REGRESSION: onKey routes to a pending APPROVAL before the sign-in sheet, so
// rendering sign-in on top of a live approval would send the user's keystrokes into an
// invisible sheet — `y` blind-approving a mutating tool while the screen shows a key
// prompt. Render priority must mirror input priority.
func TestApprovalPreemptsTheSignInSheetInTheView(t *testing.T) {
	m := signInModel(t, app.SignInStatus{})
	m.pendingSignIn.stage = signInStageKey
	m.pending = &pendingConfirm{
		req:     tools.ConfirmRequest{ToolName: "terminal.run", Risk: domain.RiskTerminal},
		resolve: make(chan bool, 1),
		shownAt: domain.NowMS(),
	}

	out := stripAnsi(m.View().Content)
	if strings.Contains(out, "Daintree sign-in") {
		t.Fatalf("the sign-in sheet must yield to a live approval:\n%s", out)
	}
	if !strings.Contains(out, "terminal.run") {
		t.Fatalf("the approval must be visible — it is what owns the keyboard:\n%s", out)
	}
	// The sheet state survives underneath so it returns once the approval is answered.
	if m.pendingSignIn == nil {
		t.Fatal("the sign-in sheet state must be preserved, not discarded")
	}
}

// A turn is MULTI-ROUND: Session calls RespondStream again after each tool round. A swap
// between rounds would send the next round to a different endpoint carrying a state
// token the previous one signed. Refuse rather than corrupt the turn.
func TestSignInRefusedWhileATurnIsInFlight(t *testing.T) {
	m := harnessModel()
	m.inFlight = true

	next, _ := m.openSignIn()
	m = next.(Model)
	if m.pendingSignIn != nil {
		t.Fatal("/login must not open a sheet while a turn is running")
	}
}

// The cockpit sheet must make the same claim the CLI flow does, at the same moment: this
// is an OpenRouter key and OpenRouter bills it. Both surfaces read the wording from one
// constant, so this pins that the sheet actually renders it — the failure mode is a
// sheet that quietly drops the sentence and asks for a spendable credential unlabelled.
func TestSignInSheetNamesTheKeyAndItsBilling(t *testing.T) {
	m := signInModel(t, app.SignInStatus{})
	s := m.pendingSignIn
	s.stage, s.baseURL = signInStageKey, "https://endpoint.test"

	out := stripAnsi(m.View().Content)
	// The exact shared constant, not a paraphrase — both surfaces render it, and
	// asserting fragments would let the wording drift into something that no longer says
	// who pays.
	if !strings.Contains(out, backend.KeyPurposeNotice) {
		t.Errorf("the key stage never renders the billing notice:\n%s", out)
	}

	// It is stage-specific: the endpoint chooser has no key to explain, and repeating
	// the billing sentence on every stage would train the reader to skip it.
	s.stage = signInStageEndpoint
	if endpointView := stripAnsi(m.View().Content); strings.Contains(endpointView, "bills this key") {
		t.Errorf("the billing notice appears on the endpoint stage too:\n%s", endpointView)
	}
}

// The notice must land WHOLE at a realistically narrow width. capWrap ELLIPSIZES past
// its row budget, and the clause it would drop first — "including background
// supervision" — is the one a reader most needs, since that is the spend that happens
// while nobody is looking.
func TestSignInSheetNoticeSurvivesNarrowWidths(t *testing.T) {
	for _, width := range []int{40, 50, 60, 80} {
		m := testModel(width)
		m.pendingSignIn = &pendingSignIn{}
		s := m.pendingSignIn
		s.stage, s.baseURL = signInStageKey, "https://endpoint.test"

		out := stripAnsi(m.View().Content)
		// Wrapping inserts newlines, so compare on a whitespace-collapsed form.
		flat := strings.Join(strings.Fields(out), " ")
		want := strings.Join(strings.Fields(backend.KeyPurposeNotice), " ")
		if !strings.Contains(flat, want) {
			t.Errorf("width %d truncated the billing notice:\n%s", width, out)
		}
	}
}
