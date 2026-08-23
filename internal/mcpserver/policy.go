package mcpserver

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
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
// Installing one is opting into a ceiling, so the switches DENY by default:
// AllowAutoApprove false refuses auto-approve, and the three Allow*Override fields
// refuse an endpoint or credential a session names for itself.
//
// The ALLOWLISTS are the exception, and it is worth being precise about it: an empty
// AllowedProjectRoots / AllowedStateRoots / AllowedLogRoots / MaxTier confines nothing,
// so a zero ServerPolicy is a real policy that still admits any path at any tier.
// "Installed a policy" and "is confined" are therefore different claims — ServeModelFacing
// checks the second one. A process with no policy installed at all is unconfined outright;
// see Registry.policy for why that must not be the same thing as a zero one.
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
	// AllowDelegatedApprovals permits approvals:"delegate", where the CALLER AGENT
	// settles each confirmation.
	//
	// It is a separate switch from AllowAutoApprove because it is a separate question,
	// and the honest framing matters: delegation is not a weaker form of asking a human,
	// it is asking the same model that is driving the session. Whether that is
	// acceptable depends on something only the operator knows — whether the caller agent
	// is a person's terminal or an unattended loop over a repository that could steer
	// it. Off by default under a policy: a session declines mutating tools and carries
	// on, which is visible and recoverable, rather than approving them via a channel
	// nobody outside the model ever sees.
	//
	// Separate, but not independent in one direction: AllowAutoApprove implies this,
	// because auto is strictly the broader grant. See Check.
	AllowDelegatedApprovals bool
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

	// --- Endpoint authority ---
	//
	// A session argument that names a NETWORK ORIGIN is authority, not configuration.
	// `backendUrl` decides where the whole conversation, the project context and every
	// tool result are posted; `mcpUrl` decides which server advertises the tools the
	// assistant will believe and call. On this surface both are chosen by a model whose
	// context can be steered by repository text, so an unbounded one is simultaneously
	// an SSRF primitive and an exfiltration route — and forfeiting the inherited
	// credential (which the CLI already does) narrows the blast radius without closing
	// either.

	// AllowBackendOverride permits a session to name its own backendUrl at all. Off
	// under a policy: the operator-configured endpoint is the one that was chosen by
	// someone who is not reading the repository.
	AllowBackendOverride bool
	// AllowedBackendOrigins, when non-empty, restricts an override to these exact
	// origins (scheme://host[:port]). It is checked IN ADDITION to
	// AllowBackendOverride, so a policy cannot enable overrides by listing origins
	// alone — the switch and the list are separate decisions.
	AllowedBackendOrigins []string
	// AllowMCPOverride and AllowedMCPOrigins are the same pair for the Daintree MCP
	// endpoint. Kept separate from the backend's because the two carry different
	// authority: redirecting the backend leaks the conversation, redirecting MCP hands
	// the assistant a tool server that can lie to it.
	AllowMCPOverride  bool
	AllowedMCPOrigins []string
	// RequireTLSForRemoteEndpoints refuses a plaintext http:// origin that is not
	// loopback. Loopback is exempt because the local dev backend is http://127.0.0.1
	// and there is no network to intercept.
	RequireTLSForRemoteEndpoints bool

	// --- Credential authority ---
	//
	// Passing a PATH rather than an inline token keeps the secret VALUE out of the
	// model's context. It does not make the operation safe: a model that can name any
	// path can select a credential it was never meant to spend, probe the filesystem
	// through the error behaviour, or pair someone else's key with an endpoint it also
	// chose. So the paths are allowlisted EXACTLY, not by root — a root would let one
	// stolen directory traversal reach every key beneath it.

	// AllowCredentialOverride permits a session to name apiKeyFile / mcpTokenFile at
	// all. Off under a policy.
	AllowCredentialOverride bool
	// AllowedAPIKeyFiles and AllowedMCPTokenFiles are EXACT files (compared after
	// symlink resolution), not roots. Empty with AllowCredentialOverride on means "any
	// path", which is the trusted-harness case and must be chosen deliberately.
	AllowedAPIKeyFiles   []string
	AllowedMCPTokenFiles []string

	// The Default* fields describe what an OMITTED argument actually resolves to in
	// this process. They exist because a ceiling that inspects the raw arguments is not
	// a ceiling: a blank `tier` resolves to the process tier (system, by default) and a
	// blank `approvals` resolves to auto whenever the process was launched with
	// auto-approve — so simply LEAVING A FIELD OUT walked straight past checks that
	// only fired on an explicit request. The policy compares the resolved authority,
	// which is the thing that actually governs the session.
	DefaultTier        domain.Tier
	DefaultAutoApprove bool
	DefaultProject     string
	DefaultStateDir    string
	DefaultLogDir      string

	// pinned records that Canonicalize has already resolved this policy's roots. Check
	// consults it so a policy used directly still answers correctly — the PINNING is a
	// property of the copy the registry holds, not a precondition every caller has to
	// remember, and a Check that silently compared a resolved request against an
	// unresolved root would refuse legitimate paths on any machine where a parent is a
	// symlink (every macOS /var, for one).
	pinned bool
}

// TrustedUnconfined is the explicit marker for a server that intends to have NO
// ceiling. It exists so the dangerous configuration takes more code than the safe one:
// a nil policy pointer was previously enough, which meant forgetting a field produced
// the unconfined server rather than a refusal.
//
// The trusted embedding paths pass it deliberately. `mcp --stdio` never does.
type TrustedUnconfined struct{}

// resolve folds an omitted argument onto the default it would actually take.
func resolve(requested, def string) string {
	if strings.TrimSpace(requested) != "" {
		return requested
	}
	return def
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

// Canonicalize resolves the policy's own roots and credential files ONCE, and returns
// the pinned copy. SetPolicy calls it at install time.
//
// Resolving them per request looked equivalent and was not: a root the operator named as
// a symlink is re-followed on every open, so retargeting that link while the server is
// alive MOVES the ceiling. The whole premise is that the policy is fixed when the
// process launches, where the operator decides it — a boundary that a later filesystem
// change can widen is not fixed.
//
// A root that will not resolve is kept in its lexical form rather than dropped. Dropping
// it would empty the allowlist, and an empty allowlist means unconfined — the one
// outcome a failure here must never produce.
//
// The slices are copied, so a caller that keeps its own reference cannot mutate the
// installed ceiling afterwards.
func (p ServerPolicy) Canonicalize() ServerPolicy {
	pin := func(in []string) []string {
		if in == nil {
			return nil
		}
		out := make([]string, 0, len(in))
		for _, v := range in {
			if resolved, err := resolvePath(v); err == nil {
				out = append(out, resolved)
				continue
			}
			out = append(out, v)
		}
		return out
	}
	p.AllowedProjectRoots = pin(p.AllowedProjectRoots)
	p.AllowedStateRoots = pin(p.AllowedStateRoots)
	p.AllowedLogRoots = pin(p.AllowedLogRoots)
	p.AllowedAPIKeyFiles = pin(p.AllowedAPIKeyFiles)
	p.AllowedMCPTokenFiles = pin(p.AllowedMCPTokenFiles)
	p.AllowedBackendOrigins = append([]string(nil), p.AllowedBackendOrigins...)
	p.AllowedMCPOrigins = append([]string(nil), p.AllowedMCPOrigins...)
	p.pinned = true
	return p
}

// Check applies the policy to one open request. openSessions is the count already live.
func (p ServerPolicy) Check(in OpenParams, openSessions int) error {
	// A policy that was never installed through SetPolicy still has lexical roots, and
	// comparing a resolved request against one of those refuses paths that are genuinely
	// inside it. Resolve here for that caller; the registry's copy is already pinned, so
	// this costs nothing on the path that matters and cannot un-pin it.
	if !p.pinned {
		p = p.Canonicalize()
	}
	if p.MaxSessions > 0 && openSessions >= p.MaxSessions {
		return &PolicyError{Field: "session.open", Reason: fmt.Sprintf(
			"this server allows %d concurrent session(s) and %d are open; close one first",
			p.MaxSessions, openSessions)}
	}
	if err := p.checkTier(resolve(in.Tier, string(p.DefaultTier))); err != nil {
		return err
	}
	// The RESOLVED mode, not the requested one: an omitted `approvals` becomes auto in
	// a process launched with auto-approve, so checking only the explicit value let a
	// caller reach unattended approval by saying nothing at all.
	mode := in.Approvals
	if mode == "" && p.DefaultAutoApprove {
		mode = ApprovalAuto
	}
	if mode == ApprovalAuto && !p.AllowAutoApprove {
		return &PolicyError{Field: "approvals:\"auto\"", Reason: "this server does not permit unattended approval of " +
			"mutating tools; use \"decline\", or \"delegate\" if this server allows it"}
	}
	// AllowAutoApprove IMPLIES delegation. Auto runs every tier-permitted mutating call
	// with nothing consulted; delegate runs the subset the caller agent chooses to
	// release. An operator who granted the broader authority cannot coherently be
	// refusing the narrower one, and refusing it would push a caller that wanted to
	// review each call toward the mode that reviews none.
	if mode == ApprovalDelegate && !p.AllowDelegatedApprovals && !p.AllowAutoApprove {
		return &PolicyError{Field: "approvals:\"delegate\"", Reason: "this server does not let the calling agent " +
			"settle its own approval requests; use \"decline\", which skips the mutating call and lets the turn carry on"}
	}
	if err := p.checkEndpoint("backendUrl", in.BackendURL, p.AllowBackendOverride, p.AllowedBackendOrigins); err != nil {
		return err
	}
	if err := p.checkEndpoint("mcpUrl", in.McpURL, p.AllowMCPOverride, p.AllowedMCPOrigins); err != nil {
		return err
	}
	// The RAW caller values, not the resolved ones: a default supplied by the operator
	// is not a model-chosen spelling and has no reason to be second-guessed here.
	for _, sp := range []struct{ field, raw string }{
		{"project", in.Project},
		{"stateDir", in.StateDir},
		{"logDir", in.LogDir},
		{"apiKeyFile", in.APIKeyFile},
		{"mcpTokenFile", in.McpTokenFile},
	} {
		if err := checkPathSpelling(sp.field, sp.raw); err != nil {
			return err
		}
	}
	if err := p.checkCredentialFile("apiKeyFile", in.APIKeyFile, p.AllowedAPIKeyFiles); err != nil {
		return err
	}
	if err := p.checkCredentialFile("mcpTokenFile", in.McpTokenFile, p.AllowedMCPTokenFiles); err != nil {
		return err
	}
	// The project must EXIST to be operated on, so it is resolved strictly. The state
	// and log roots are routinely created by the open itself, so they resolve through
	// their nearest existing ancestor instead — see checkRoot.
	if err := checkRoot("project", resolve(in.Project, p.DefaultProject), p.AllowedProjectRoots); err != nil {
		return err
	}
	if err := checkRoot("stateDir", resolve(in.StateDir, p.DefaultStateDir), p.AllowedStateRoots); err != nil {
		return err
	}
	return checkRoot("logDir", resolve(in.LogDir, p.DefaultLogDir), p.AllowedLogRoots)
}

// checkEndpoint governs one caller-supplied network origin.
//
// An OMITTED endpoint is always fine: the caller did not choose it, so the process
// default answers and there is nothing for the policy to judge. Everything else is a
// deliberate redirection of where this session's data goes, which is the decision the
// operator — not a model reading a repository — is entitled to make.
func (p ServerPolicy) checkEndpoint(field, raw string, allowOverride bool, allowedOrigins []string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if !allowOverride {
		return &PolicyError{Field: field, Reason: "this server pins its endpoints at launch; " +
			"omit the field to use the one the operator configured"}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return &PolicyError{Field: field, Reason: fmt.Sprintf("%q is not a URL", raw)}
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return &PolicyError{Field: field, Reason: fmt.Sprintf(
			"scheme %q is not permitted; use http or https", u.Scheme)}
	}
	if u.Host == "" {
		return &PolicyError{Field: field, Reason: fmt.Sprintf("%q names no host", raw)}
	}
	// Userinfo is refused outright rather than stripped. `https://user:pass@host/` in a
	// model-callable argument is a credential travelling through a channel that exists
	// precisely so credentials do not — and silently dropping it would leave the caller
	// believing it had authenticated.
	if u.User != nil {
		return &PolicyError{Field: field, Reason: "an endpoint must not carry userinfo; " +
			"credentials travel by file, never in a URL"}
	}
	origin := u.Scheme + "://" + u.Host
	if p.RequireTLSForRemoteEndpoints && u.Scheme != "https" && !isLoopbackHost(u.Hostname()) {
		return &PolicyError{Field: field, Reason: fmt.Sprintf(
			"%s is plaintext and not loopback; this server requires https for a remote endpoint", origin)}
	}
	if len(allowedOrigins) == 0 {
		return nil
	}
	for _, allowed := range allowedOrigins {
		if strings.EqualFold(strings.TrimSpace(strings.TrimSuffix(allowed, "/")), origin) {
			return nil
		}
	}
	return &PolicyError{Field: field, Reason: fmt.Sprintf(
		"origin %s is not in this server's allowlist (%s)", origin, strings.Join(allowedOrigins, ", "))}
}

// isLoopbackHost reports whether a URL hostname names this machine. It never resolves a
// name: a DNS lookup on a caller-supplied host would itself be the SSRF request this
// check exists to prevent.
//
// A textual "127." prefix is NOT enough — `127.attacker.example` is a perfectly ordinary
// remote hostname that starts with those four characters, and treating it as loopback
// waved it straight past the TLS requirement. Only a literal IP that the net package
// itself calls loopback counts, plus the one name that is loopback by definition.
func isLoopbackHost(host string) bool {
	host = strings.ToLower(strings.Trim(host, "[]"))
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// checkPathSpelling refuses a model-supplied path whose SPELLING is a problem in itself,
// before anything tries to resolve it. Both rules exist because the policy and the code
// that finally opens the path must agree on which bytes they are talking about.
//
//   - Surrounding whitespace. config trims every value it resolves, and the MCP session
//     layer deliberately does NOT (a trailing space is a legal part of a filename, and
//     silently reading "/keys/account" for a caller that named "/keys/account " would
//     substitute a credential). The two are individually defensible and together they
//     are an escape: "/repo/state " can be a symlink INTO the allowed root while
//     "/repo/state" is an attacker-controlled directory outside it, so the policy
//     follows one path and the factory opens the other. Refusing the padded spelling
//     costs nothing real and makes the two layers agree.
//
//   - A ".." component. filepath.Abs cleans "a/link/../b" to "a/b" LEXICALLY, before any
//     symlink is followed, while the kernel resolves `link` first and lands somewhere
//     else entirely. Every check built on a cleaned path is therefore answering a
//     question about a path that will never be opened. A model has no legitimate need to
//     write ".." into an absolute root or credential path, so the honest fix is to
//     refuse it rather than to try to out-resolve the kernel.
func checkPathSpelling(field, raw string) error {
	if raw == "" || raw == strings.TrimSpace(raw) {
		// Fall through to the ".." scan.
	} else {
		return &PolicyError{Field: field, Reason: fmt.Sprintf(
			"%q begins or ends with whitespace; give the path exactly", raw)}
	}
	for _, part := range strings.Split(filepath.ToSlash(raw), "/") {
		if part == ".." {
			return &PolicyError{Field: field, Reason: fmt.Sprintf(
				"%q contains a \"..\" component; name the directory directly", raw)}
		}
	}
	return nil
}

// checkCredentialFile governs a caller-supplied credential PATH.
//
// The allowlist holds EXACT files, not roots, and the comparison happens after symlink
// resolution on both sides. A root would mean one traversal reaches every key beneath
// it, and an unresolved comparison would mean a symlink named inside the allowlist
// selects a file outside it — the same escape checkRoot closes for directories.
func (p ServerPolicy) checkCredentialFile(field, raw string, allowed []string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if !p.AllowCredentialOverride {
		return &PolicyError{Field: field, Reason: "this server does not let a session choose its own credential file; " +
			"omit the field to use the credential the operator gave this process"}
	}
	if len(allowed) == 0 {
		return nil
	}
	got, err := resolvePath(raw)
	if err != nil {
		return &PolicyError{Field: field, Reason: fmt.Sprintf("%q could not be resolved", raw)}
	}
	// Already resolved at install, for the same reason checkRoot's are.
	for _, cand := range allowed {
		if filepath.Clean(cand) == got {
			return nil
		}
	}
	return &PolicyError{Field: field, Reason: fmt.Sprintf(
		"%q is not one of the credential files this server permits", raw)}
}

func (p ServerPolicy) checkTier(requested string) error {
	if p.MaxTier == "" {
		return nil
	}
	max, ok := tierRank(p.MaxTier)
	// Validate the CEILING before the request. Returning early on a blank request let a
	// policy carrying an unintelligible MaxTier fail OPEN for exactly the caller who
	// said nothing — the case that resolves to the most privileged default.
	if !ok {
		return &PolicyError{Field: "tier", Reason: fmt.Sprintf(
			"this server's policy names an unknown maximum tier %q", p.MaxTier)}
	}
	if strings.TrimSpace(requested) == "" {
		// Nothing requested and nothing to resolve it to: the factory decides, and the
		// policy has no resolved value to judge.
		return nil
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
//
// The comparison is on RESOLVED paths, not lexical ones. A lexical check answers a
// question nobody asked: `/srv/projects/link` is textually inside `/srv/projects` even
// when `link` is a symlink to `/etc`, so "allowed root" would name a set of strings
// rather than a set of directories. Both sides are therefore resolved through
// filepath.EvalSymlinks before the prefix test.
//
// Neither side is required to exist. A state or log root is routinely CREATED by the
// open that names it, so a strict resolve would refuse exactly the first launch. Instead
// the path resolves through its nearest existing ancestor and the not-yet-created tail
// is re-appended — which still closes the escape, because the escape needs an existing
// symlink to travel through.
//
// This is a pre-open check, so a path that is renamed or re-linked between the check and
// the open is not covered; closing that needs openat/O_NOFOLLOW at every use site, which
// is a different change. The check makes the allowlist mean directories instead of
// strings, which is the property the policy claims.
func checkRoot(field, path string, roots []string) error {
	if len(roots) == 0 || strings.TrimSpace(path) == "" {
		return nil
	}
	abs, err := resolvePath(path)
	if err != nil {
		return &PolicyError{Field: field, Reason: fmt.Sprintf("%q could not be resolved to an absolute path", path)}
	}
	// The roots were resolved when the policy was installed (see Canonicalize), so only
	// the requested side is resolved here — re-resolving a root per request would let a
	// symlink retargeted after launch move the ceiling.
	for _, root := range roots {
		if withinRoot(abs, filepath.Clean(root)) {
			return nil
		}
	}
	return &PolicyError{Field: field, Reason: fmt.Sprintf(
		"%q is outside the directories this server permits (%s)", abs, strings.Join(roots, ", "))}
}

// resolvePath makes a path absolute and follows every symlink in it, including in the
// part that does not exist yet.
//
// The walk upward is what makes it usable on a state directory the open is about to
// create: resolve the deepest ancestor that DOES exist, then re-join the tail.
//
// Three rules keep the walk from becoming its own escape:
//
//   - It starts ONLY on ENOENT. Any other error — ENOTDIR, EACCES, ELOOP — means the
//     path exists in some form we could not follow, and stepping past it would resolve
//     against an ancestor while ignoring the component that actually failed.
//
//   - A component that ENOENTs but is present to Lstat is a DANGLING SYMLINK, not a
//     missing name. Skipping it would treat a link the caller controls as a plain
//     directory that does not exist yet — and the link's target decides where the
//     eventual mkdir lands. It is refused.
//
//   - Reaching the filesystem root with nothing resolvable is an ERROR, never the
//     lexical path. Returning the unresolved spelling would be exactly the lexical
//     answer this function exists to replace.
//
// The caller-supplied path has already been refused if it contained a ".." component
// (see checkPathSpelling), so filepath.Abs's lexical cleaning cannot elide a symlink
// here.
func resolvePath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	// The fast path: the whole thing exists.
	resolved, err := filepath.EvalSymlinks(abs)
	if err == nil {
		return filepath.Clean(resolved), nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	// Walk up to the nearest existing ancestor, remembering the tail. Bounded by the
	// filesystem root, so the loop terminates.
	var tail []string
	cur := abs
	for {
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", fmt.Errorf("no existing ancestor of %q could be resolved", abs)
		}
		// A name that does not resolve but DOES exist is a dangling symlink. It is a
		// caller-controlled redirect wearing a missing directory's clothes.
		if fi, lerr := os.Lstat(cur); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("%q is a symlink that does not resolve", cur)
		}
		tail = append([]string{filepath.Base(cur)}, tail...)
		cur = parent
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			return filepath.Clean(filepath.Join(append([]string{resolved}, tail...)...)), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
	}
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
