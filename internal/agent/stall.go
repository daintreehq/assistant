package agent

import (
	"hash/fnv"
	"strconv"
	"strings"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/models"
)

// Turn convergence guard.
//
// The agentic loop in runTurn ends a turn on exactly one condition: a round that
// returns no tool calls. Everything else — the two circuit breakers in runToolBatch —
// keys on FAILURE, so a model whose calls all SUCCEED is invisible to them. That left
// one shape completely unguarded: an open-ended prompt ("do a huge round of performance
// work across the whole app") puts the model in a restate-and-re-explore attractor
// where every round emits a fresh paragraph about how large the job is plus another
// batch of harmless reads. Nothing fails, so nothing trips; nothing is answered, so the
// loop never exits. The turn runs until the process dies.
//
// This file supplies the missing progress measure. Two signals, both computed from what
// the loop already has in hand:
//
//   - NOVELTY. A round that issues no (tool, args, RESULT) triple this turn has not
//     already seen learned nothing: it either asked a question it had asked before and
//     got the same answer, or re-ran work whose outcome had not moved. A short
//     consecutive run of those is a stall.
//   - BUDGET. A total round ceiling for the turn, because a model can churn without
//     repeating itself — a different file every round, forever.
//
// Neither ends the turn silently. Both escalate the same way: nudge the model once with
// a system event, then, if it still hasn't converged, spend the final round with tools
// switched off asking for the plan and status in prose. The user gets an honest report
// and can continue in a new turn; the alternative — an unbounded spend that never
// answers — is the actual defect.
//
// That last part is not a new idea here: internal/subagent already bounds its delegated
// loop the same way (DefaultMaxRounds, then Runner.reportRound with tool_choice "none"),
// on the reasoning that a run which has spent the whole budget getting lost still owes
// the caller a write-up. The main turn loop was simply the one loop that never got it.
//
// THE GUARANTEE the loop relies on: a turn ends within TurnRoundBudget+1 model rounds,
// unconditionally. The budget counts rounds actually streamed since the turn began, and
// nothing rewinds it — not a mid-turn injection, not a lifted stall close. That matters
// because InjectPrompt is reachable from the MCP server (internal/mcpserver/session.go
// Inject), so an external agent, not only the human at the keyboard, can fold messages
// into a running turn; a budget an injection could reset would hand that caller an
// unbounded turn by injecting once before each close.

// convergenceStep is the guard's instruction to the loop at the top of a round.
type convergenceStep int

const (
	// convergenceContinue: normal round, tools on.
	convergenceContinue convergenceStep = iota
	// convergenceNudge: normal round, tools on, but push the accompanying system
	// event first — the model is drifting and gets one chance to correct itself.
	convergenceNudge
	// convergenceClose: last round of this turn. Tools off, and the accompanying
	// system event asks for the closing report.
	convergenceClose
)

// convergenceAction is one step decision plus the two messages that must accompany it:
// instruction steers the MODEL (pushed into history as a system event), warning tells
// the HUMAN (surfaced through the event sink). Both are empty on continue.
type convergenceAction struct {
	step        convergenceStep
	instruction string
	warning     string
}

// turnStall tracks per-turn convergence. One instance per turn.
type turnStall struct {
	// seenCalls holds every (tool, args, result) signature observed this turn.
	seenCalls map[string]struct{}
	// barren counts CONSECUTIVE rounds that added nothing to seenCalls.
	barren int
	// stallNudged / budgetNudged latch the two warnings SEPARATELY. Sharing one latch
	// meant a turn that repeated itself early, got nudged, then corrected and ran long
	// was closed at the budget having never been told the budget was approaching —
	// each escalation has to be nudge-then-close in its own right.
	stallNudged  bool
	budgetNudged bool
	// closing latches once the guard has asked for the closing round, so the loop
	// cannot be told to close twice.
	closing bool
	// budgetClosed records that the close was the ROUND BUDGET, not repetition. A
	// stall close yields to a mid-turn injection (a fresh instruction is genuinely new
	// work, and the budget still bounds whatever follows); a budget close does not,
	// because lifting it is what would make the ceiling negotiable.
	budgetClosed bool
}

func newTurnStall() *turnStall {
	return &turnStall{seenCalls: make(map[string]struct{})}
}

// reset clears the repetition tally for a folded-in user instruction: new instruction,
// new work, so what the PREVIOUS instruction had already asked for says nothing about
// whether this one is making progress. It deliberately leaves the budget latches
// (budgetNudged, budgetClosed) alone — see the guarantee above.
func (t *turnStall) reset() {
	t.seenCalls = make(map[string]struct{})
	t.barren = 0
	t.stallNudged = false
	t.closing = false
}

// observe folds one round's settled batch into the tracker. A round is NOVEL when it
// produced at least one (tool, canonical args, result digest) triple this turn had not
// seen before; a novel round clears the barren run. Signatures are recorded whether or
// not the round was novel, so re-running the same call three rounds apart and getting
// the same answer back still reads as repetition.
func (t *turnStall) observe(sigs []string) {
	novel := false
	for _, sig := range sigs {
		if _, seen := t.seenCalls[sig]; !seen {
			novel = true
			t.seenCalls[sig] = struct{}{}
		}
	}
	if novel {
		t.barren = 0
		return
	}
	t.barren++
}

// step reports what the loop should do before streaming `round` (1-based, counted over
// the whole turn — see the guarantee). It is called once per round and mutates the
// latches, so a caller must act on what it returns.
//
// Order is deliberate. The two closes outrank both nudges, so a nudged turn still
// closes; and within each pair the stall signal is checked first, because when a turn is
// both repeating itself and near its ceiling, "you are repeating work" is the more
// actionable thing to say.
func (t *turnStall) step(round int) convergenceAction {
	if t.closing {
		return convergenceAction{step: convergenceContinue}
	}
	switch {
	case t.barren >= domain.TurnStallAbort:
		t.closing = true
		return convergenceAction{
			step: convergenceClose,
			instruction: closingInstruction("You have run " + itoa(t.barren) +
				" rounds that produced nothing you had not already seen this turn."),
			warning: "The assistant repeated the same work for " + itoa(t.barren) +
				" rounds — closing the turn and asking it to report its plan.",
		}
	case round > domain.TurnRoundBudget:
		t.closing = true
		t.budgetClosed = true
		return convergenceAction{
			step: convergenceClose,
			instruction: closingInstruction("This turn has reached its limit of " +
				itoa(domain.TurnRoundBudget) + " model rounds."),
			warning: "Turn reached its " + itoa(domain.TurnRoundBudget) +
				"-round limit without an answer — closing it and asking the assistant to report its plan.",
		}
	case !t.stallNudged && t.barren >= domain.TurnStallWarn:
		t.stallNudged = true
		return convergenceAction{
			step: convergenceNudge,
			instruction: "[system event]\nThe last " + itoa(t.barren) +
				" rounds re-ran tools you had already run this turn and got the same answers back, so they told you nothing new. " +
				"Repeating work you have already done cannot move this forward. Take the next concrete action now — spawn the agent terminals that will do the work, arm the watchers, send the commands — " +
				"or stop and give the user your plan and what you need them to decide. Do not restate the request.",
			warning: "The assistant has spent " + itoa(t.barren) +
				" rounds re-running work it had already done — nudging it to act or report.",
		}
	case !t.budgetNudged && round >= domain.TurnRoundWarn:
		t.budgetNudged = true
		return convergenceAction{
			step: convergenceNudge,
			instruction: "[system event]\nYou are " + itoa(round) + " rounds into this turn and it will be closed automatically at " +
				itoa(domain.TurnRoundBudget) + ". Stop restating the goal and stop gathering more context. " +
				"Either take the action that actually does the work now — spawn the agent terminals, arm the watchers — or stop and give the user the plan, what you already have running, and what you need them to narrow.",
			warning: "Turn is " + itoa(round) + " rounds in with no answer yet — nudging the assistant to converge.",
		}
	}
	return convergenceAction{step: convergenceContinue}
}

// closingInstruction wraps a reason in the final-round brief. The closing round runs
// with tool_choice "none", so this is the last thing the model is told before it must
// produce prose: it asks for the two things a user of an orchestrator actually needs
// after a turn that did not converge — what is already running, and what would happen
// next — and explicitly forbids the restatement that got the turn here.
func closingInstruction(reason string) string {
	return "[system event]\n" + reason + " No further tools will run this turn. " +
		"Reply now, in prose, with: what you have already done or set running this turn (name the terminals, worktrees and watchers), " +
		"the plan you would follow next, and anything you need the user to decide or narrow. " +
		"Do not restate the request and do not describe how large the job is."
}

// The deterministic fallback for a closing round that produced no prose at all. A turn
// must never end on an empty bubble, and this path is a genuine non-result.
//
// Split into prefix + suffix so isStalledReply can recognize THIS string rather than
// anything starting "Stopped after ". A bare prefix match would classify a real model
// answer opening with those words as a wake failure — and "Stopped after N rounds…" is
// a plausible opening for the very convergence report the closing round asks for.
const (
	stalledReplyPrefix = "Stopped after "
	stalledReplySuffix = " rounds without converging on an answer. Nothing further was run. " +
		"Try narrowing this into a smaller, more specific piece of work and sending it again."
)

func stalledReply(rounds int) string {
	return stalledReplyPrefix + itoa(rounds) + stalledReplySuffix
}

// isStalledReply reports whether a reply is the stalled-turn sentinel above. Both ends
// must match, so only the string this engine emits qualifies.
func isStalledReply(reply string) bool {
	return strings.HasPrefix(reply, stalledReplyPrefix) && strings.HasSuffix(reply, stalledReplySuffix)
}

// toolChoiceFor maps the closing latch to the wire value. "none" is the only value
// that actually prevents a batch; "auto" is the normal round.
func toolChoiceFor(closing bool) string {
	if closing {
		return "none"
	}
	return "auto"
}

// callSignature is the novelty key: the tool, its canonicalized arguments, and a digest
// of what the call actually returned. Including the RESULT is what keeps the guard
// honest about mutable reads — watcher.list, agentTask.status, a re-polled
// forge.getChecks all take identical (often empty) arguments and legitimately return
// DIFFERENT data each time, and keying on the call alone would have forced those turns
// closed after a handful of polls. A poll whose answer changes is progress; a poll whose
// answer is byte-identical is not.
func callSignature(name, rawArgs, resultDigest string) string {
	return name + "\x00" + canonicalJSON(rawArgs) + "\x00" + resultDigest
}

// resultDigest hashes one serialized tool reply down to a short, stable key. Only
// equality matters, so a non-cryptographic 64-bit hash is the right cost: the
// alternative is holding every full tool result of the turn in the tracker.
func resultDigest(body string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(body))
	return strconv.FormatUint(h.Sum64(), 36)
}

// roundSignatures builds the novelty keys for one SETTLED round — call it after the
// batch has run, so each call's reply is already in the transcript.
func (s *Session) roundSignatures(calls []models.ToolCallRequest) []string {
	if len(calls) == 0 {
		return nil
	}
	digests := s.toolReplyDigests(calls)
	sigs := make([]string, 0, len(calls))
	for _, call := range calls {
		sigs = append(sigs, callSignature(s.resolveInternal(call.Function.Name), call.Function.Arguments, digests[call.ID]))
	}
	return sigs
}

// toolReplyDigests reads each call's settled reply back off the transcript tail.
// runToolBatch pushes exactly one tool message per call (dispatched, stubbed or
// skipped) as it goes, so this round's replies are the last messages in history and the
// backwards scan is bounded to a small window rather than the whole conversation — the
// O(turns²) trap this package avoids everywhere else. A call whose reply is not in the
// window digests as "", which reads as "unknown", never as "same as last time".
func (s *Session) toolReplyDigests(calls []models.ToolCallRequest) map[string]string {
	want := make(map[string]struct{}, len(calls))
	for _, c := range calls {
		want[c.ID] = struct{}{}
	}
	out := make(map[string]string, len(calls))

	s.mu.Lock()
	defer s.mu.Unlock()
	// The batch's own replies plus the handful of messages runToolBatch can append
	// around them (the end-of-batch stuck nudge).
	window := len(calls) + 8
	for i := len(s.messages) - 1; i >= 0 && window > 0 && len(out) < len(want); i-- {
		window--
		m := s.messages[i]
		if m.Role != "tool" {
			continue
		}
		if _, ok := want[m.ToolCallID]; !ok {
			continue
		}
		if _, dup := out[m.ToolCallID]; dup {
			continue
		}
		out[m.ToolCallID] = resultDigest(m.StringContent)
	}
	return out
}
