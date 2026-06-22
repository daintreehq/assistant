// Package safety holds the tool-dispatch safety policy: tier gating, the
// always-confirm matrix, the HARD no-file-edit invariant, the read-only secret
// guards, and project-root path containment. It is the choke point every
// mutating tool call passes through (via internal/tools' dispatch pipeline).
//
// These strings (risk classes, tier names, error codes, forbidden fragments)
// are model/schema/audit contracts — preserve their exact spelling.
package safety

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// tierAllowed maps each tier to the risk classes it may perform AT ALL.
// operator deliberately lacks git/system; only system tier has them.
var tierAllowed = map[domain.Tier]map[domain.RiskClass]bool{
	domain.TierSupervisor: setOf(domain.RiskRead, domain.RiskLocal, domain.RiskUI),
	domain.TierOperator: setOf(
		domain.RiskRead, domain.RiskLocal, domain.RiskUI,
		domain.RiskTerminal, domain.RiskProject, domain.RiskExternal,
	),
	domain.TierSystem: setOf(
		domain.RiskRead, domain.RiskLocal, domain.RiskUI,
		domain.RiskTerminal, domain.RiskProject, domain.RiskExternal,
		domain.RiskGit, domain.RiskSystem,
	),
}

// alwaysConfirm is the set of risk classes that always require an explicit
// confirmation (or a scoped grant for non-interactive actors) before running.
// read/local/ui never confirm.
var alwaysConfirm = setOf(
	domain.RiskTerminal, domain.RiskProject,
	domain.RiskGit, domain.RiskExternal, domain.RiskSystem,
)

// TierAllowsRisk reports whether tier may perform risk at all.
func TierAllowsRisk(tier domain.Tier, risk domain.RiskClass) bool {
	allowed, ok := tierAllowed[tier]
	if !ok {
		return false
	}
	return allowed[risk]
}

// AlwaysConfirm reports whether the risk class is in the always-confirm set.
func AlwaysConfirm(risk domain.RiskClass) bool { return alwaysConfirm[risk] }

// PolicyDecision is the result of evaluating a (risk, tier) pair.
type PolicyDecision struct {
	Allowed           bool
	NeedsConfirmation bool
	// Reason is set only when Allowed is false (the TIER_DENIED message text).
	Reason string
}

// DecideOptions carries the optional pre-resolved-approval flag. The dispatch
// path does NOT pass it (the grant check happens after Decide in the registry);
// it exists for callers that pre-resolve approval.
type DecideOptions struct {
	HasScopedApproval bool
}

// Decide applies tier gating and the confirmation matrix.
//
//  1. If the tier may not perform the risk class at all → not allowed, with the
//     /permissions steer message.
//  2. Otherwise allowed; needsConfirmation = risk in ALWAYS_CONFIRM and no
//     pre-resolved scoped approval.
func Decide(risk domain.RiskClass, tier domain.Tier, opts ...DecideOptions) PolicyDecision {
	if !TierAllowsRisk(tier, risk) {
		return PolicyDecision{
			Allowed:           false,
			NeedsConfirmation: false,
			Reason: fmt.Sprintf(
				"'%s' actions require a higher tier than '%s'. Switch tier with /permissions.",
				risk, tier,
			),
		}
	}
	var hasScopedApproval bool
	if len(opts) > 0 {
		hasScopedApproval = opts[0].HasScopedApproval
	}
	return PolicyDecision{
		Allowed:           true,
		NeedsConfirmation: AlwaysConfirm(risk) && !hasScopedApproval,
	}
}

func setOf[T comparable](vals ...T) map[T]bool {
	m := make(map[T]bool, len(vals))
	for _, v := range vals {
		m[v] = true
	}
	return m
}

// --- No-file-edit guard (HARD INVARIANT) ---

// FileEditAttemptError is raised when a file-mutating tool name is detected at
// registration OR when daintree.call forwards a forbidden raw MCP name. Its code
// FILE_EDIT_FORBIDDEN is model-facing.
type FileEditAttemptError struct {
	Message string
}

// Code is the stable machine code carried by every FileEditAttemptError.
const FileEditForbiddenCode = "FILE_EDIT_FORBIDDEN"

func (e *FileEditAttemptError) Error() string { return e.Message }

// Code returns the machine code (always FILE_EDIT_FORBIDDEN).
func (e *FileEditAttemptError) Code() string { return FileEditForbiddenCode }

// forbiddenToolFragments are case-insensitive substrings that mark a file-editing
// tool. The assistant never edits files; edits go through a spawned agent.
// The first block is the verbatim TS guard list (FORBIDDEN_TOOL_FRAGMENTS); the
// second block hardens it with additional common mutating verbs/separators (the
// fs__write OpenAI wire form, delete/remove/rename/save variants, and more patch
// spellings) so a write tool can't be wired in under a near-miss name. Matching is
// a case-insensitive substring test, so each entry catches its name variants.
var forbiddenToolFragments = []string{
	"write_file",
	"writefile",
	"apply_patch",
	"applypatch",
	"edit_file",
	"editfile",
	"fs.write",
	"fs.edit",
	"file.write",
	"file.edit",
	"patch.apply",

	// Hardening additions.
	"fs__write", // OpenAI wire form of fs.write (dots → "__")
	"savefile",  // saveFile / save_file
	"save_file",
	"rename_file", // renameFile
	"renamefile",
	"remove_file", // removeFile
	"removefile",
	"delete_file", // deleteFile
	"deletefile",
	"file.delete",
	"file.remove",
	"file.rename",
	"file.save",
	"workspace.patch",
	"patchfile", // patchFile / patch_file
	"patch_file",
}

// IsForbiddenToolName reports whether name contains any forbidden fragment
// (case-insensitive substring match). Applied at registration to local names AND
// at runtime inside daintree.call to a raw forwarded MCP name.
func IsForbiddenToolName(name string) bool {
	n := strings.ToLower(name)
	for _, frag := range forbiddenToolFragments {
		if strings.Contains(n, frag) {
			return true
		}
	}
	return false
}

// AssertNoFileEditTools throws (returns *FileEditAttemptError) if any name is a
// file-mutating tool name. Called at registry startup over all registered names.
func AssertNoFileEditTools(toolNames []string) error {
	var offenders []string
	for _, n := range toolNames {
		if IsForbiddenToolName(n) {
			offenders = append(offenders, n)
		}
	}
	if len(offenders) > 0 {
		return &FileEditAttemptError{Message: fmt.Sprintf(
			"Refusing to register file-mutating tools: %s. The CLI must delegate edits to a spawned agent.",
			strings.Join(offenders, ", "),
		)}
	}
	return nil
}

// --- Secret-file guards (read-only leak prevention) ---
//
// The read-only fs tools refuse to read credential-bearing files: their contents
// would otherwise leak into the durable audit log / conversation history.
// Matching is case-insensitive.

var secretBasenames = setOf(
	".env", ".envrc", ".npmrc", ".netrc", ".pgpass", ".htpasswd",
	"credentials", "credentials.json",
	"id_rsa", "id_dsa", "id_ecdsa", "id_ed25519",
	".dockercfg", ".git-credentials",
	"secrets.json", "secrets.yaml", "secrets.yml",
	"service-account.json", "serviceaccount.json",
)

var secretSuffixes = []string{
	".pem", ".key", ".p12", ".pfx", ".keystore", ".jks", ".asc", ".gpg", ".ppk",
}

var secretDirSegments = setOf(
	".ssh", ".aws", ".gnupg", ".kube", ".azure", ".gcloud", ".docker",
)

// IsSensitiveSegment reports whether a single (already lowercased) path segment
// is a credential dir or a .env-family file. Caller passes a LOWERCASED segment.
func IsSensitiveSegment(seg string) bool {
	if secretDirSegments[seg] {
		return true
	}
	// .env, *.env (e.g. prod.env), and .env.* (e.g. .env.local) all leak.
	if seg == ".env" || strings.HasSuffix(seg, ".env") || strings.HasPrefix(seg, ".env.") {
		return true
	}
	return false
}

// IsSensitivePath reports whether relOrPath names a credential-bearing file or
// lives under a credential dir anywhere in the path. EVERY segment is checked so
// a sensitive file/dir nested anywhere (nested/.env/x, home/.aws/credentials) is
// caught.
func IsSensitivePath(relOrPath string) bool {
	lower := strings.ToLower(relOrPath)
	base := filepath.Base(lower)
	if secretBasenames[base] {
		return true
	}
	for _, s := range secretSuffixes {
		if strings.HasSuffix(base, s) {
			return true
		}
	}
	// Split on either separator (so a Windows-style path is covered too) and check
	// every segment.
	for _, seg := range strings.FieldsFunc(lower, func(r rune) bool {
		return r == '/' || r == '\\'
	}) {
		if IsSensitiveSegment(seg) {
			return true
		}
	}
	return false
}

// --- Project-root path containment ---

// ResolveInsideProject resolves rel against projectPath and returns the absolute
// target, guaranteeing it stays inside the project root. It runs two passes:
// (1) lexical (catches ../ traversal even for not-yet-existing paths) and
// (2) symlink-resolved (a repo-local symlink can't point outside the project).
// On escape it returns a *FileEditAttemptError.
func ResolveInsideProject(projectPath, rel string) (string, error) {
	lexicalRoot, err := filepath.Abs(projectPath)
	if err != nil {
		return "", &FileEditAttemptError{Message: fmt.Sprintf("Path escapes the project root: %s", rel)}
	}
	target := filepath.Clean(filepath.Join(lexicalRoot, rel))

	// (1) Lexical containment.
	if err := assertInside(lexicalRoot, target, rel); err != nil {
		return "", err
	}

	// (2) Symlink-resolved containment: realpath the nearest existing ancestor of
	// both root and target so benign system symlinks (/tmp → /private/tmp) on both
	// sides cancel out while a repo-local symlink escape is caught.
	realRoot := realpathOfExisting(lexicalRoot)
	realTarget := realpathOfExisting(target)
	if err := assertInside(realRoot, realTarget, rel); err != nil {
		return "", err
	}

	return target, nil
}

// assertInside throws unless p is root itself or lives under root + separator.
func assertInside(root, p, rel string) error {
	sep := string(filepath.Separator)
	normRoot := strings.TrimRight(root, sep) + sep
	if p == strings.TrimRight(root, sep) || strings.HasPrefix(p+sep, normRoot) {
		return nil
	}
	return &FileEditAttemptError{Message: fmt.Sprintf("Path escapes the project root: %s", rel)}
}

// realpathOfExisting resolves symlinks on the nearest existing ancestor of p and
// re-appends the missing remainder, so a not-yet-existing path still gets its
// existing prefix symlink-resolved.
func realpathOfExisting(p string) string {
	p = filepath.Clean(p)
	missing := ""
	cur := p
	for {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			if missing == "" {
				return resolved
			}
			return filepath.Join(resolved, missing)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			// Reached the filesystem root without finding an existing ancestor.
			return p
		}
		missing = filepath.Join(filepath.Base(cur), missing)
		cur = parent
	}
}
