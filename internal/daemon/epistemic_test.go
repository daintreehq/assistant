package daemon

import (
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// Covers the epistemicKind mapping (classificationEpistemicKind) and the
// DecideOutcome provenance tagging.

func TestClassificationEpistemicKind_Mapping(t *testing.T) {
	// Deterministic terminal exit = observed; a model-claimed one = inferred.
	if got := domain.ClassificationEpistemicKind(domain.ClassTerminalExited, false); got != domain.EpistemicObserved {
		t.Errorf("terminal_exited (deterministic) → observed, got %s", got)
	}
	if got := domain.ClassificationEpistemicKind(domain.ClassTerminalExited, true); got != domain.EpistemicInferred {
		t.Errorf("terminal_exited (model-claimed) → inferred, got %s", got)
	}

	// waiting_for_input disambiguated by whether the model was consulted.
	if got := domain.ClassificationEpistemicKind(domain.ClassWaitingForInput, false); got != domain.EpistemicObserved {
		t.Errorf("waiting_for_input (agentState) → observed, got %s", got)
	}
	if got := domain.ClassificationEpistemicKind(domain.ClassWaitingForInput, true); got != domain.EpistemicInferred {
		t.Errorf("waiting_for_input (model) → inferred, got %s", got)
	}

	// Model-only and completion classes → inferred.
	for _, c := range []domain.WatcherClassification{
		domain.ClassPermissionPrompt, domain.ClassStillWorking, domain.ClassTestsFailed,
		domain.ClassTestsPassed, domain.ClassCommandFailed, domain.ClassMergeConflict,
		domain.ClassCompletedSuccess, domain.ClassCompletedUnverified, domain.ClassCompletedUnknown,
	} {
		if got := domain.ClassificationEpistemicKind(c, false); got != domain.EpistemicInferred {
			t.Errorf("%s → inferred, got %s", c, got)
		}
	}

	// No-signal / unknown classes → unverified.
	for _, c := range []domain.WatcherClassification{
		domain.ClassNoChange, domain.ClassUnknown, domain.ClassNeedsLargeModel,
	} {
		if got := domain.ClassificationEpistemicKind(c, false); got != domain.EpistemicUnverified {
			t.Errorf("%s → unverified, got %s", c, got)
		}
	}
	// An unrecognized classification also falls through to unverified.
	if got := domain.ClassificationEpistemicKind(domain.WatcherClassification("garbage"), false); got != domain.EpistemicUnverified {
		t.Errorf("garbage → unverified, got %s", got)
	}
}

func TestDecideOutcome_EpistemicProvenance(t *testing.T) {
	base := DecideArgs{Summary: "s", Signals: WatcherSignals{}}

	// Deterministic terminal_exited → observed.
	out := DecideOutcome(withClass(base, domain.ClassTerminalExited, 0.95, false))
	if out.EpistemicKind != domain.EpistemicObserved {
		t.Errorf("deterministic terminal_exited → observed, got %s", out.EpistemicKind)
	}

	// waiting_for_input: deterministic observed vs model-derived inferred.
	if k := DecideOutcome(withClass(base, domain.ClassWaitingForInput, 0.9, false)).EpistemicKind; k != domain.EpistemicObserved {
		t.Errorf("deterministic waiting_for_input → observed, got %s", k)
	}
	if k := DecideOutcome(withClass(base, domain.ClassWaitingForInput, 0.85, true)).EpistemicKind; k != domain.EpistemicInferred {
		t.Errorf("model-derived waiting_for_input → inferred, got %s", k)
	}

	// A model classification → inferred; an unknown outcome → unverified.
	if k := DecideOutcome(withClass(base, domain.ClassTestsFailed, 0.85, true)).EpistemicKind; k != domain.EpistemicInferred {
		t.Errorf("model tests_failed → inferred, got %s", k)
	}
	if k := DecideOutcome(withClass(base, domain.ClassUnknown, 0.4, false)).EpistemicKind; k != domain.EpistemicUnverified {
		t.Errorf("unknown → unverified, got %s", k)
	}
}

func withClass(a DecideArgs, c domain.WatcherClassification, conf float64, usedModel bool) DecideArgs {
	a.Classification = c
	a.Confidence = conf
	a.UsedModel = usedModel
	return a
}
