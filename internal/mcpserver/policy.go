package mcpserver

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/daintreehq/assistant/internal/domain"
)

// policy.go is the process-level ceiling on what a session may ask for.
//
// The problem it solves is authority, not ergonomics. `daintree.session.open` lets a
// caller choose the project path, the permission tier, the state and log roots, and
// whether mutating tools are auto-approved. On the MCP surface that caller is a MODEL,
// and a model's arguments can be steered by repository text, tool output, or anything
// else that reaches its context. Without a ceiling, a content-level instruction —
// "open a session on /etc with tier system and approvals auto" — becomes a new process
// authority boundary, which is the one thing a tool argument must never be able to do.
//
// The rule is one-directional: a session may NARROW the policy and can never widen it.
// The policy itself is fixed when the process launches, where the operator (not the
// model) decides it.

// ServerPolicy bounds every session this process will open.
//
// Installing one is opting into a ceiling, so its defaults DENY: an unset
// AllowedProjectRoots is the only permissive zero value here, and AllowAutoApprove false
// means auto-approve is refused. A process with no policy installed at all is
// unconfined — see Registry.policy for why those two must not be the same thing.
type ServerPolicy struct {
	// MaxSessions caps concurrent open sessions. Each holds a project lease and starts
	// real runtime machinery, so "unbounded" is a resource decision, not a default.
	// Zero means no cap.
	MaxSessions int
	// MaxTier is the highest permission tier a session may request. Empty means no
	// ceiling. A request ABOVE it is refused rather than silently downgraded: a caller
	// that believes it has system tier and is quietly given supervisor would read every
	// subsequent refusal as a bug.
	MaxTier domain.Tier
	// AllowAutoApprove permits approvals:"auto". Off by default under a policy, because
	// auto-approve is the setting that turns a read-mostly session into one that can
	// push, run commands, and mutate a repository with nothing watching.
	AllowAutoApprove bool
	// AllowedProjectRoots, when non-empty, confines a session's project path to these
	// directories. Paths are cleaned and compared after symlink-free lexical
	// normalization, and a prefix match must land on a path SEPARATOR so /srv/appfoo
	// cannot pass as being inside /srv/app.
	AllowedProjectRoots []string
	// AllowedStateRoots and AllowedLogRoots do the same for the directories a session
	// writes to. They are separate from the project roots because the honest answers
	// differ: a caller may legitimately read a project it may not scribble beside.
	AllowedStateRoots []string
	AllowedLogRoots   []string
}

// tierRank orders the tiers so a ceiling can be compared. It is deliberately a closed
// switch rather than a map lookup with a default: an unknown tier must not sort as
// "lowest" and slip under every ceiling.
func tierRank(t domain.Tier) (int, bool) {
	switch t {
	case domain.TierSupervisor:
		return 1, true
	case domain.TierOperator:
		return 2, true
	case domain.TierSystem:
		return 3, true
	}
	return 0, false
}

// PolicyError is a refusal by the process policy. It is distinct from an argument
// validation error because the remedy is different: the caller cannot fix it by asking
// differently, only the operator who launched the process can change it.
type PolicyError struct {
	Field  string
	Reason string
}

func (e *PolicyError) Error() string {
	return fmt.Sprintf("%s is not permitted by this server's policy: %s. "+
		"This is a process-level ceiling set when the server was launched, not something a session argument can raise.",
		e.Field, e.Reason)
}

// Check applies the policy to one open request. openSessions is the count already live.
func (p ServerPolicy) Check(in OpenParams, openSessions int) error {
	if p.MaxSessions > 0 && openSessions >= p.MaxSessions {
		return &PolicyError{Field: "session.open", Reason: fmt.Sprintf(
			"this server allows %d concurrent session(s) and %d are open; close one first",
			p.MaxSessions, openSessions)}
	}
	if err := p.checkTier(in.Tier); err != nil {
		return err
	}
	if in.Approvals == ApprovalAuto && !p.AllowAutoApprove {
		return &PolicyError{Field: "approvals:\"auto\"", Reason: "this server does not permit unattended approval of " +
			"mutating tools; use \"ask\" and answer them, or \"decline\""}
	}
	if err := checkRoot("project", in.Project, p.AllowedProjectRoots); err != nil {
		return err
	}
	if err := checkRoot("stateDir", in.StateDir, p.AllowedStateRoots); err != nil {
		return err
	}
	return checkRoot("logDir", in.LogDir, p.AllowedLogRoots)
}

func (p ServerPolicy) checkTier(requested string) error {
	if p.MaxTier == "" || strings.TrimSpace(requested) == "" {
		return nil
	}
	max, ok := tierRank(p.MaxTier)
	if !ok {
		// A policy carrying a tier this build does not know is a misconfiguration, and
		// the safe reading of an unintelligible ceiling is that nothing clears it.
		return &PolicyError{Field: "tier", Reason: fmt.Sprintf(
			"this server's policy names an unknown maximum tier %q", p.MaxTier)}
	}
	want, ok := tierRank(domain.Tier(strings.TrimSpace(requested)))
	if !ok {
		return &PolicyError{Field: "tier", Reason: fmt.Sprintf("%q is not a known tier", requested)}
	}
	if want > max {
		return &PolicyError{Field: "tier", Reason: fmt.Sprintf(
			"this server allows at most %q and the session asked for %q", p.MaxTier, requested)}
	}
	return nil
}

// checkRoot confines a caller-supplied path to an allowlist. An empty allowlist means
// unconfined; an empty path means "use the default", which the policy has nothing to say
// about because the caller did not choose it.
func checkRoot(field, path string, roots []string) error {
	if len(roots) == 0 || strings.TrimSpace(path) == "" {
		return nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return &PolicyError{Field: field, Reason: fmt.Sprintf("%q could not be resolved to an absolute path", path)}
	}
	abs = filepath.Clean(abs)
	for _, root := range roots {
		rabs, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		if withinRoot(abs, filepath.Clean(rabs)) {
			return nil
		}
	}
	return &PolicyError{Field: field, Reason: fmt.Sprintf(
		"%q is outside the directories this server permits (%s)", abs, strings.Join(roots, ", "))}
}

// withinRoot reports whether abs is root or beneath it. The separator check is the whole
// point: a bare strings.HasPrefix would let /srv/appliance pass as being inside /srv/app.
func withinRoot(abs, root string) bool {
	if abs == root {
		return true
	}
	if !strings.HasSuffix(root, string(filepath.Separator)) {
		root += string(filepath.Separator)
	}
	return strings.HasPrefix(abs, root)
}
