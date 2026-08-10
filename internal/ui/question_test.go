package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/tools"
)

// question_test.go locks the multiple-choice question sheet: it parks on request,
// answers by letter or arrow+Enter (debounced), cancels the turn on Esc, and replaces
// the composer while up.

func questionReqOf(q string, opts ...string) tools.AskChoiceRequest {
	cs := make([]tools.ChoiceOption, len(opts))
	for i, o := range opts {
		cs[i] = tools.ChoiceOption{Label: string(rune('A' + i)), Text: o}
	}
	return tools.AskChoiceRequest{Question: q, Options: cs}
}

// liveTurnModel returns a harness model with an in-flight turn — the precondition for a
// question (the tool runs mid-turn), which onQuestionRequested now enforces.
func liveTurnModel() Model {
	m := harnessModel()
	now := domain.NowMS()
	cell := &TurnCell{ID: "turn_q", State: TurnActive, Phase: domain.PhaseToolRunning, Ts: now, StartedAt: now, LastActivityAt: now}
	m.transcript = append(m.transcript, TranscriptCell{Turn: cell})
	m.activeTurn = cell.ID
	m.inFlight = true
	return m
}

func questionPending(t *testing.T, req tools.AskChoiceRequest) (Model, chan questionReply) {
	t.Helper()
	m := liveTurnModel()
	ch := make(chan questionReply, 1)
	next, _ := m.Update(QuestionRequestedMsg{Request: req, Reply: ch})
	mm := asModel(t, next)
	if mm.pendingQuestion == nil {
		t.Fatal("QuestionRequestedMsg did not park the sheet")
	}
	// Past the debounce so a letter/Enter commits (the debounce is exercised separately).
	mm.pendingQuestion.shownAt = domain.NowMS() - approveDebounceMs - 50
	return mm, ch
}

func questionDecision(ch chan questionReply) (questionReply, bool) {
	select {
	case r := <-ch:
		return r, true
	default:
		return questionReply{}, false
	}
}

func TestQuestion_ParksAndDrivesPhase(t *testing.T) {
	m := liveTurnModel()
	next, _ := m.Update(QuestionRequestedMsg{Request: questionReqOf("Pick", "One", "Two"), Reply: make(chan questionReply, 1)})
	mm := asModel(t, next)
	if mm.pendingQuestion == nil {
		t.Fatal("sheet not parked")
	}
	if mm.composerFocus() {
		t.Fatal("composer must NOT have focus while a question is up")
	}
	if got := mm.activeTurnCell(); got == nil || got.Phase != domain.PhaseAwaitingQuestion {
		t.Fatalf("phase = %v, want awaiting_question", got)
	}
}

func TestQuestion_ForcesHomeView(t *testing.T) {
	// A question that arrives while the ops deck is open must force the home view so its
	// sheet is visible (keys would otherwise route to an unseen prompt).
	m := liveTurnModel()
	m.view = viewOperations
	m.opsScroll = 3
	next, _ := m.Update(QuestionRequestedMsg{Request: questionReqOf("Pick", "One", "Two"), Reply: make(chan questionReply, 1)})
	mm := asModel(t, next)
	if mm.view != viewHome {
		t.Fatalf("view = %v, want home so the sheet renders", mm.view)
	}
	if mm.pendingQuestion == nil {
		t.Fatal("sheet not parked")
	}
}

func TestQuestion_RejectsWhenNoLiveTurn(t *testing.T) {
	// No in-flight turn (e.g. cancelled between the tool sending and Update processing):
	// the request must be rejected with a cancel reply, not parked as a ghost sheet.
	m := harnessModel() // not in flight, no active turn
	ch := make(chan questionReply, 1)
	next, _ := m.Update(QuestionRequestedMsg{Request: questionReqOf("Pick", "One", "Two"), Reply: ch})
	mm := asModel(t, next)
	if mm.pendingQuestion != nil {
		t.Fatal("a question without a live turn must not be parked")
	}
	if r, ok := questionDecision(ch); !ok || !r.cancelled {
		t.Fatalf("the orphaned request should be cancelled (ok=%v r=%+v)", ok, r)
	}
}

func TestQuestion_RejectsWhenCancelling(t *testing.T) {
	m := liveTurnModel()
	m.activeTurnCell().Phase = domain.PhaseCancelling
	ch := make(chan questionReply, 1)
	next, _ := m.Update(QuestionRequestedMsg{Request: questionReqOf("Pick", "One", "Two"), Reply: ch})
	mm := asModel(t, next)
	if mm.pendingQuestion != nil {
		t.Fatal("a question for a cancelling turn must not be parked")
	}
	if r, ok := questionDecision(ch); !ok || !r.cancelled {
		t.Fatalf("the request should be cancelled (ok=%v r=%+v)", ok, r)
	}
}

func TestQuestion_SecondRequestRejectedNotOrphaned(t *testing.T) {
	m, first := questionPending(t, questionReqOf("Pick", "One", "Two"))
	second := make(chan questionReply, 1)
	next, _ := m.Update(QuestionRequestedMsg{Request: questionReqOf("Other", "X", "Y"), Reply: second})
	mm := asModel(t, next)
	// The first sheet stays; the second request is rejected (its channel replied cancelled).
	if mm.pendingQuestion == nil || mm.pendingQuestion.req.Question != "Pick" {
		t.Fatal("the first pending question must survive a second request")
	}
	if r, ok := questionDecision(second); !ok || !r.cancelled {
		t.Fatalf("the second request should be cancelled, not orphan the first (ok=%v r=%+v)", ok, r)
	}
	if _, ok := questionDecision(first); ok {
		t.Fatal("the first channel must not have been touched")
	}
}

func TestQuestion_LetterAnswersImmediately(t *testing.T) {
	m, ch := questionPending(t, questionReqOf("Env?", "Local", "Staging", "Production"))
	mm := asModel(t, mustModel(m.onQuestionKey(runeKey('b'))))
	r, ok := questionDecision(ch)
	if !ok || r.cancelled || r.index != 1 {
		t.Fatalf("B should answer option index 1 (ok=%v r=%+v)", ok, r)
	}
	if mm.pendingQuestion != nil {
		t.Fatal("sheet not dismissed after answering")
	}
}

func TestQuestion_LowercaseLetterAnswers(t *testing.T) {
	m, ch := questionPending(t, questionReqOf("Env?", "Local", "Staging", "Production"))
	asModel(t, mustModel(m.onQuestionKey(runeKey('c'))))
	if r, ok := questionDecision(ch); !ok || r.index != 2 {
		t.Fatalf("c should answer option index 2 (ok=%v r=%+v)", ok, r)
	}
}

func TestQuestion_OutOfRangeLetterRingsBell(t *testing.T) {
	m, ch := questionPending(t, questionReqOf("Env?", "Local", "Staging"))
	next, cmd := m.onQuestionKey(runeKey('z'))
	mm := asModel(t, next)
	if _, ok := questionDecision(ch); ok {
		t.Fatal("an out-of-range letter must not answer")
	}
	if cmd == nil {
		t.Fatal("an out-of-range letter should ring the bell")
	}
	if mm.pendingQuestion == nil {
		t.Fatal("an unhandled key dismissed the sheet")
	}
}

func TestQuestion_ArrowThenEnterAnswersHighlighted(t *testing.T) {
	m, ch := questionPending(t, questionReqOf("Pick", "One", "Two", "Three"))
	m = asModel(t, mustModel(m.onQuestionKey(tea.KeyPressMsg{Code: tea.KeyDown})))
	m = asModel(t, mustModel(m.onQuestionKey(tea.KeyPressMsg{Code: tea.KeyDown})))
	if m.pendingQuestion.selected != 2 {
		t.Fatalf("selected = %d after two downs, want 2", m.pendingQuestion.selected)
	}
	asModel(t, mustModel(m.onQuestionKey(tea.KeyPressMsg{Code: tea.KeyEnter})))
	if r, ok := questionDecision(ch); !ok || r.index != 2 {
		t.Fatalf("Enter should answer the highlighted option 2 (ok=%v r=%+v)", ok, r)
	}
}

func TestQuestion_DownClampsAtEnd(t *testing.T) {
	m, _ := questionPending(t, questionReqOf("Pick", "One", "Two"))
	m = asModel(t, mustModel(m.onQuestionKey(tea.KeyPressMsg{Code: tea.KeyDown})))
	m = asModel(t, mustModel(m.onQuestionKey(tea.KeyPressMsg{Code: tea.KeyDown})))
	if m.pendingQuestion.selected != 1 {
		t.Fatalf("selected = %d, want clamped to 1", m.pendingQuestion.selected)
	}
}

func TestQuestion_DebounceIgnoresTypedAhead(t *testing.T) {
	m := liveTurnModel()
	ch := make(chan questionReply, 1)
	next, _ := m.Update(QuestionRequestedMsg{Request: questionReqOf("Env?", "Local", "Staging"), Reply: ch})
	mm := asModel(t, next) // shownAt = now → inside the debounce window
	if mm.pendingQuestion == nil {
		t.Fatal("sheet not parked")
	}
	nx, cmd := mm.onQuestionKey(runeKey('a'))
	mmm := asModel(t, nx)
	if _, ok := questionDecision(ch); ok {
		t.Fatal("a typed-ahead letter inside the debounce window answered the question")
	}
	if mmm.pendingQuestion == nil {
		t.Fatal("the sheet was dismissed during the debounce window")
	}
	if cmd == nil {
		t.Fatal("a debounced answer should ring the bell")
	}
}

func TestQuestion_EscCancelsTurn(t *testing.T) {
	m, ch := questionPending(t, questionReqOf("Env?", "Local", "Staging"))
	m.inFlight = true // exercise the onCancel path (cancelTurn is a no-op with a nil cancel)
	mm := asModel(t, mustModel(m.onQuestionKey(tea.KeyPressMsg{Code: tea.KeyEscape})))
	r, ok := questionDecision(ch)
	if !ok || !r.cancelled {
		t.Fatalf("Esc should cancel the question (ok=%v r=%+v)", ok, r)
	}
	if mm.pendingQuestion != nil {
		t.Fatal("Esc did not dismiss the sheet")
	}
}

func TestQuestion_ClearCancels(t *testing.T) {
	m, ch := questionPending(t, questionReqOf("Env?", "Local", "Staging"))
	mm := asModel(t, mustModel(m.onClear("Cleared", "conversation cleared")))
	if mm.pendingQuestion != nil {
		t.Fatal("/clear must dismiss a pending question sheet")
	}
	if r, ok := questionDecision(ch); !ok || !r.cancelled {
		t.Fatalf("/clear should cancel the pending question (ok=%v r=%+v)", ok, r)
	}
}

func TestQuestion_CtrlAltLettersDoNotAnswer(t *testing.T) {
	for _, mod := range []tea.KeyMod{tea.ModCtrl, tea.ModAlt} {
		m, ch := questionPending(t, questionReqOf("Env?", "Local", "Staging"))
		next, cmd := m.onQuestionKey(tea.KeyPressMsg{Code: 'b', Mod: mod})
		mm := asModel(t, next)
		if _, ok := questionDecision(ch); ok {
			t.Fatalf("a modified letter (mod=%v) must not answer", mod)
		}
		if mm.pendingQuestion == nil {
			t.Fatalf("a modified letter (mod=%v) dismissed the sheet", mod)
		}
		if cmd == nil {
			t.Fatalf("a modified letter (mod=%v) should ring the bell", mod)
		}
	}
}

func TestQuestion_HomeEndMoveHighlight(t *testing.T) {
	m, _ := questionPending(t, questionReqOf("Pick", "One", "Two", "Three"))
	m = asModel(t, mustModel(m.onQuestionKey(tea.KeyPressMsg{Code: tea.KeyEnd})))
	if m.pendingQuestion.selected != 2 {
		t.Fatalf("End should select the last option, got %d", m.pendingQuestion.selected)
	}
	m = asModel(t, mustModel(m.onQuestionKey(tea.KeyPressMsg{Code: tea.KeyHome})))
	if m.pendingQuestion.selected != 0 {
		t.Fatalf("Home should select the first option, got %d", m.pendingQuestion.selected)
	}
}

func TestQuestion_PhaseLabels(t *testing.T) {
	if got := liveStatusLabel(domain.PhaseAwaitingQuestion); got == "" {
		t.Error("PhaseAwaitingQuestion should have a live-status label")
	}
	if got := runStageLabel(domain.PhaseAwaitingQuestion); got == "" {
		t.Error("PhaseAwaitingQuestion should have a composer stage label")
	}
}

func TestQuestion_CtrlCCancels(t *testing.T) {
	m, ch := questionPending(t, questionReqOf("Env?", "Local", "Staging"))
	mm := asModel(t, mustModel(m.onCtrlC()))
	if r, ok := questionDecision(ch); !ok || !r.cancelled {
		t.Fatalf("Ctrl+C should cancel the question (ok=%v r=%+v)", ok, r)
	}
	if mm.pendingQuestion != nil {
		t.Fatal("Ctrl+C did not dismiss the sheet")
	}
}

func TestQuestion_RenderShowsOptionsAndHighlight(t *testing.T) {
	m, _ := questionPending(t, questionReqOf("Which environment?", "Local", "Staging", "Production"))
	m.pendingQuestion.selected = 1
	out := stripAnsi(renderQuestion(m.theme, m.pendingQuestion, 56))
	for _, want := range []string{"Which environment?", "A. Local", "B. Staging", "C. Production", "Esc cancel"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered sheet missing %q:\n%s", want, out)
		}
	}
	// The selected row carries the "›" caret marker.
	if !strings.Contains(out, "› B. Staging") {
		t.Errorf("selected option not marked:\n%s", out)
	}
	// Layout: a blank line after the title sets the question off; a full-width rule
	// frames the option list above and below. Rules are matched by LINE equality (not
	// substring count) so an over-wide rule can't false-pass.
	lines := strings.Split(out, "\n")
	if len(lines) < 2 || strings.TrimSpace(lines[1]) != "" {
		t.Errorf("expected a blank line after the title:\n%s", out)
	}
	rule := strings.Repeat(m.theme.Glyphs.Rule, 56)
	var ruleIdx []int
	for i, l := range lines {
		if l == rule {
			ruleIdx = append(ruleIdx, i)
		}
	}
	if len(ruleIdx) != 2 {
		t.Fatalf("expected exactly 2 rule lines framing the options, got %d:\n%s", len(ruleIdx), out)
	}
	between := strings.Join(lines[ruleIdx[0]+1:ruleIdx[1]], "\n")
	for _, want := range []string{"A. Local", "› B. Staging", "C. Production"} {
		if !strings.Contains(between, want) {
			t.Errorf("option %q should sit inside the rule frame:\n%s", want, out)
		}
	}
}

func TestQuestion_BottomBandReplacesComposer(t *testing.T) {
	m, _ := questionPending(t, questionReqOf("Which?", "Yes", "No"))
	band := stripAnsi(m.bottomBand(m.contentW()))
	if !strings.Contains(band, "A. Yes") {
		t.Fatalf("bottom band should show the question sheet:\n%s", band)
	}
	if strings.Contains(band, "Ask Daintree") {
		t.Fatalf("composer placeholder must be hidden while a question is up:\n%s", band)
	}
}

func TestQuestion_ManyOptionsWindowed(t *testing.T) {
	opts := make([]string, 20)
	for i := range opts {
		opts[i] = "option-" + string(rune('a'+i))
	}
	// Worst-case height: a question long enough to fill maxQuestionRows AND a
	// mid-list selection so BOTH scroll cues render inside the frame.
	long := strings.Repeat("which of these many candidate options should we pick ", 4)
	m, _ := questionPending(t, questionReqOf(long, opts...))
	m.pendingQuestion.selected = 10
	out := renderQuestion(m.theme, m.pendingQuestion, 56)
	rows := strings.Count(out, "\n") + 1
	// title + blank + question(≤maxQuestionRows) + rule + ↑cue + (≤maxQuestionOptionRows)
	// + ↓cue + rule + hint — bounded.
	if rows > maxQuestionOptionRows+maxQuestionRows+7 {
		t.Fatalf("windowed sheet is %d rows — should stay bounded", rows)
	}
	// Match the cues by their indented prefix so the hint row's "↑/↓ select" can't
	// false-satisfy the check.
	plain := stripAnsi(out)
	for _, cue := range []string{"  ↑ ", "  ↓ "} {
		if !strings.Contains(plain, cue) {
			t.Errorf("a mid-list selection should show the %q scroll cue:\n%s", cue, plain)
		}
	}
}
