package daemon

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// runVerificationPass is a read-only post-completion reconciliation: it
// deterministically checks the worktree's git cleanliness via git.getProjectPulse
// (a read tool — no confirmation, safe from watcher context). Never throws: any
// failure yields verdict "unknown" (treated as not-yet-verified, never clean).
func runVerificationPass(ctx *CheckContext, scope *verificationScope) domain.VerificationResult {
	unverifiable := func(summary string) domain.VerificationResult {
		return domain.VerificationResult{
			Verdict:            domain.VerdictUnknown,
			HasGitChanges:      false,
			ChangedFiles:       0,
			ChangedFileList:    []string{},
			GitSummary:         summary,
			UnresolvedWarnings: []string{},
		}
	}
	if !ctx.MCP.Connected() {
		return unverifiable("Daintree MCP not connected; could not verify git state")
	}
	args := map[string]any{}
	if scope != nil && scope.WorktreeID != "" {
		args["worktreeId"] = scope.WorktreeID
	}
	res, err := ctx.MCP.CallRead(ctx.Ctx, "git.getProjectPulse", args)
	if err != nil {
		return unverifiable("git.getProjectPulse call failed")
	}
	if res.IsError {
		return unverifiable("git.getProjectPulse reported an error")
	}
	return DeriveVerification(res.StructuredContent, res.Text)
}

// gatedCompletion is a completion classification resolved through the gate.
type gatedCompletion struct {
	classification domain.WatcherClassification
	confidence     float64
	summary        string
	evidence       []string
}

// gateInput carries the per-tick gate inputs (the contract + the evidence tail).
type gateInput struct {
	rec                domain.WatcherRecord
	signals            WatcherSignals
	acceptanceCriteria string
}

// gateCompletion resolves a tentative "the agent is done" signal into a
// trustworthy outcome (issue #83). Called from BOTH the FSM `completed` path and
// the small-model `completed_success` path so the gate cannot be bypassed.
func gateCompletion(ctx *CheckContext, scope *verificationScope, baseEvidence []string, gate *gateInput) gatedCompletion {
	verification := runVerificationPass(ctx, scope)
	isClean := verification.Verdict == domain.VerdictVerified && !verification.HasGitChanges
	var criteria string
	if gate != nil {
		criteria = strings.TrimSpace(gate.acceptanceCriteria)
	}

	// No acceptance contract → legacy git-cleanliness gate.
	if criteria == "" {
		if isClean {
			return finalizeGate(verification, baseEvidence, domain.ClassCompletedSuccess, 0.85,
				"Agent completed; worktree clean and verified.")
		}
		summary := fmt.Sprintf("Agent completed but git state is unverified (%s).", verification.GitSummary)
		if verification.HasGitChanges {
			summary = fmt.Sprintf("Agent completed but %s — review before commit/push.", verification.GitSummary)
		}
		return finalizeGate(verification, baseEvidence, domain.ClassCompletedUnverified, 0.8, summary)
	}

	// Acceptance contract present → judge the agent's output against it.
	verification.AcceptanceCriteria = criteria
	var answer *domain.ModelJudgeAnswer
	// Only judge when there is actual terminal evidence to judge. An empty tail =
	// no evidence; judging nothing could harden a transport hiccup into a false
	// "failed" → leave "unknown".
	if gate != nil && ctx.MCP.Connected() && strings.TrimSpace(gate.signals.Tail) != "" {
		question := "Did the agent satisfy this acceptance contract for the task? Contract: " + criteria
		results := runModelJudges(ctx, []string{question}, gate.rec, gate.signals)
		if a, ok := results[question]; ok {
			answer = &a
		}
	}
	confident := answer != nil && answer.Confidence >= judgeConfidenceFloor
	if answer != nil {
		verification.CriteriaMetSummary = answer.Reason
	}

	// Confident YES on a clean tree → verified success.
	if confident && answer.Matched && isClean {
		verification.Verdict = domain.VerdictVerified
		return finalizeGate(verification, baseEvidence, domain.ClassCompletedSuccess,
			math.Max(0.85, answer.Confidence),
			strings.TrimSpace("Agent completed; acceptance contract met and worktree clean. "+answer.Reason))
	}
	// Confident NO → contract not satisfied (non-terminal: a later attempt can satisfy it).
	if confident && !answer.Matched {
		verification.Verdict = domain.VerdictFailed
		return finalizeGate(verification, baseEvidence, domain.ClassCompletedUnverified, answer.Confidence,
			strings.TrimSpace("Agent stopped but the acceptance contract was not met — review. "+answer.Reason))
	}
	// Inconclusive: met but dirty/unreadable, or not confident.
	verification.Verdict = domain.VerdictUnknown
	var why string
	if confident && answer.Matched {
		why = fmt.Sprintf("acceptance contract met but %s — review before commit/push", verification.GitSummary)
	} else {
		why = fmt.Sprintf("acceptance contract could not be confidently verified (%s)", verification.GitSummary)
	}
	return finalizeGate(verification, baseEvidence, domain.ClassCompletedUnverified, 0.8,
		fmt.Sprintf("Agent completed but %s.", why))
}

// finalizeGate attaches the (possibly mutated) VerificationResult bundle as
// serialized evidence so every branch emits the final verdict, never a stale
// snapshot.
func finalizeGate(v domain.VerificationResult, baseEvidence []string, class domain.WatcherClassification, conf float64, summary string) gatedCompletion {
	blob, _ := json.Marshal(v)
	evidence := make([]string, 0, len(baseEvidence)+1)
	evidence = append(evidence, baseEvidence...)
	evidence = append(evidence, domain.VerificationEvidencePrefix+string(blob))
	return gatedCompletion{classification: class, confidence: conf, summary: summary, evidence: evidence}
}

// recommendedActionsFor builds the recommended actions attached to a published
// watcher event. terminal.focus is the real UI tool (open_review is display-only).
func recommendedActionsFor(class domain.WatcherClassification, terminalID string) []domain.RecommendedAction {
	switch class {
	case domain.ClassWaitingForInput, domain.ClassPermissionPrompt:
		return []domain.RecommendedAction{{
			Label:                "Focus terminal",
			ToolName:             "terminal.focus",
			Args:                 map[string]any{"terminalId": terminalID},
			Risk:                 domain.RiskUI,
			RequiresConfirmation: false,
		}}
	case domain.ClassCompletedUnverified:
		return []domain.RecommendedAction{{
			Label:                "Review completion",
			ToolName:             "terminal.focus",
			Args:                 map[string]any{"terminalId": terminalID},
			Risk:                 domain.RiskUI,
			RequiresConfirmation: false,
		}}
	}
	return nil
}
