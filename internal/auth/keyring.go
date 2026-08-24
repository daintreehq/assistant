package auth

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// keyring.go talks to the operating system's credential service.
//
// It shells out to the platform tool rather than linking a keyring library, and that is
// a deliberate trade. Against: an exec is clumsier than a function call and depends on a
// binary being present. With: this module has SIX direct dependencies and treats adding
// one as a review blocker; every Go keyring library pulls a D-Bus stack for Linux, and
// the macOS half of them shell out to exactly the binary below anyway. The whole surface
// we need is get/set/delete of one string.
//
// The security-relevant constraint is that the secret NEVER appears in argv, because
// argv is world-readable through `ps` and this command runs on every token rotation.
// That constraint is what dictates the unusual command shapes below — in particular the
// macOS path, where the obvious spelling does not work at all:
//
//	security add-generic-password ... -w        # prompts on /dev/tty, ignores stdin
//	security add-generic-password ... -w SECRET # puts the secret in argv
//
// The first is not a stdin reader; `security`'s own usage text says "Specify -w as the
// last option to be prompted", and with no controlling terminal it exits 2 with a usage
// dump. The second is exactly what must not happen. The working form is `security -i`,
// which reads a COMMAND STREAM from stdin: argv is then only ["security", "-i"], and the
// secret rides the pipe. Combined with -X (hex-encoded data) it also removes every
// quoting question, since hex contains no metacharacters.

// keyringTimeout bounds one credential-store call. These are local IPC and answer in
// milliseconds; a call that has not returned by now is a hung agent or a modal prompt
// nobody is going to answer, and a login that blocks forever is worse than one that
// fails.
const keyringTimeout = 20 * time.Second

// runFunc executes a credential-store command.
type runFunc func(ctx context.Context, name string, args []string, stdin []byte) (stdout, stderr []byte, err error)

// KeyringStore is the OS credential-service implementation of Store.
type KeyringStore struct {
	// run is a field so tests drive the whole encode/decode path against a fake without
	// touching the real keychain — which would otherwise mean tests prompting a
	// developer for their password.
	run runFunc

	// toolPath is the RESOLVED absolute path of the credential binary, looked up once.
	//
	// Resolving once and executing that exact path matters: on Linux the tool is found
	// on PATH, and re-resolving at exec time means a writable or repo-controlled
	// directory earlier in PATH can substitute a program that receives the refresh
	// token on stdin. Pinning the resolved path closes the window between the check and
	// the use.
	mu       sync.Mutex
	toolPath string
	probed   bool
	probeErr error
}

// OpenStore returns the best available credential store for this machine, together with
// the reason when it is not the keychain.
//
// The reason is RETURNED rather than logged because the caller must surface it. A
// memory-tier session works normally and then evaporates on exit; a user who was not
// told will experience that as the assistant randomly forgetting them.
func OpenStore(ctx context.Context) (Store, StorageTier, error) {
	ks := NewKeyringStore()
	if err := ks.probe(ctx); err != nil {
		return NewMemoryStore(), TierMemory, err
	}
	return ks, TierKeychain, nil
}

// NewKeyringStore builds a store over the platform credential tool.
func NewKeyringStore() *KeyringStore { return &KeyringStore{run: runCommand} }

// toolName returns the platform credential binary name.
//
// Windows is absent because this binary is Unix-only (flock and Setsid have no port and
// the !unix builds fail loudly); a Credential Manager branch here would imply support
// the rest of the binary does not have.
func toolName() string {
	switch runtime.GOOS {
	case "darwin":
		return "/usr/bin/security"
	case "linux":
		return "secret-tool"
	}
	return ""
}

// probe resolves the tool once and verifies the credential service actually WORKS.
//
// Checking only that the binary exists is not enough on Linux: secret-tool is installed
// on plenty of headless machines with no session bus and no Secret Service behind it,
// where every call fails. Reporting TierKeychain there breaks the memory-plus-reason
// contract in the worst way — the user is told their login persists, and it does not.
// So the probe performs a real lookup of a key that will not exist and accepts only
// not-found or a genuine hit as proof of a working service.
func (k *KeyringStore) probe(ctx context.Context) error {
	k.mu.Lock()
	if k.probed {
		defer k.mu.Unlock()
		return k.probeErr
	}
	k.mu.Unlock()

	err := k.doProbe(ctx)

	k.mu.Lock()
	k.probed, k.probeErr = true, err
	k.mu.Unlock()
	return err
}

func (k *KeyringStore) doProbe(ctx context.Context) error {
	name := toolName()
	if name == "" {
		return fmt.Errorf("%w: no credential service on %s", ErrStoreUnavailable, runtime.GOOS)
	}
	resolved, err := exec.LookPath(name)
	if err != nil {
		return fmt.Errorf("%w: %s is not installed", ErrStoreUnavailable, name)
	}
	k.mu.Lock()
	k.toolPath = resolved
	k.mu.Unlock()

	// A lookup for a key that cannot exist. ErrNotFound is the SUCCESS case here: it
	// proves the service answered.
	probeKey := CredentialKey{StateRoot: "probe", BackendOrigin: "probe", Issuer: "probe", Environment: "probe", ClientID: "probe"}
	if _, err := k.get(ctx, probeKey); err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	return nil
}

// tool returns the resolved binary path, resolving lazily if probe has not run.
func (k *KeyringStore) tool() (string, error) {
	k.mu.Lock()
	p := k.toolPath
	k.mu.Unlock()
	if p != "" {
		return p, nil
	}
	name := toolName()
	if name == "" {
		return "", ErrStoreUnavailable
	}
	resolved, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("%w: %s is not installed", ErrStoreUnavailable, name)
	}
	k.mu.Lock()
	k.toolPath = resolved
	k.mu.Unlock()
	return resolved, nil
}

// Tier reports which tier this store is on right now.
func (k *KeyringStore) Tier(ctx context.Context) StorageTier {
	if err := k.probe(ctx); err != nil {
		return TierUnavailable
	}
	return TierKeychain
}

// Load reads and decodes the stored session.
func (k *KeyringStore) Load(ctx context.Context, key CredentialKey) (StoredSession, error) {
	raw, err := k.get(ctx, key)
	if err != nil {
		return StoredSession{}, err
	}
	var s StoredSession
	if err := json.Unmarshal(raw, &s); err != nil {
		// `security find-generic-password -w` prints the stored bytes as PLAIN text when
		// they are printable and as HEX when they are not. Our payload is always
		// printable JSON, so the plain form is the normal path — but decoding hex on the
		// way in makes the round trip total rather than dependent on a formatting rule
		// we do not control.
		if decoded, herr := hex.DecodeString(strings.TrimSpace(string(raw))); herr == nil {
			if jerr := json.Unmarshal(decoded, &s); jerr == nil {
				return validated(s)
			}
		}
		// Something IS stored under our key and this build cannot read it. Distinct from
		// not-found on purpose: not-found means sign in, corrupt means the entry needs
		// replacing, and treating the second as the first produces a login that appears
		// to succeed and then reads the same broken entry next launch.
		return StoredSession{}, fmt.Errorf("%w: %v", ErrStoreCorrupt, err)
	}
	return validated(s)
}

func validated(s StoredSession) (StoredSession, error) {
	if !s.Valid() {
		return StoredSession{}, fmt.Errorf("%w: the stored session is incomplete or from a newer build", ErrStoreCorrupt)
	}
	return s, nil
}

// Save encodes and writes the session.
func (k *KeyringStore) Save(ctx context.Context, key CredentialKey, session StoredSession) error {
	session.Version = storedSessionVersion
	if !session.Valid() {
		return errors.New("auth: refusing to store an incomplete session")
	}
	body, err := json.Marshal(session)
	if err != nil {
		return fmt.Errorf("auth: encode session: %w", err)
	}
	return k.set(ctx, key, body)
}

// --- platform commands -------------------------------------------------------------

// get reads the secret for a key.
func (k *KeyringStore) get(ctx context.Context, key CredentialKey) ([]byte, error) {
	switch runtime.GOOS {
	case "darwin":
		// -w prints ONLY the password. Without it `security` dumps every attribute and
		// the value would have to be parsed back out.
		out, errOut, err := k.exec(ctx, []string{
			"find-generic-password", "-s", ServiceName, "-a", key.Account(), "-w",
		}, nil)
		if err != nil {
			return nil, classifyDarwin(err, errOut)
		}
		return bytes.TrimRight(out, "\r\n"), nil
	case "linux":
		out, errOut, err := k.exec(ctx, []string{
			"lookup", "service", ServiceName, "account", key.Account(),
		}, nil)
		if err != nil {
			return nil, classifyLinux(err, out, errOut)
		}
		if len(bytes.TrimSpace(out)) == 0 {
			return nil, ErrNotFound
		}
		return bytes.TrimRight(out, "\r\n"), nil
	}
	return nil, ErrStoreUnavailable
}

// set writes the secret for a key.
//
// On macOS this goes through `security -i`, whose argv is only ["-i"]: the command —
// including the secret — is written to the tool's stdin. The payload is hex-encoded via
// -X so no quoting rule of the interactive parser can corrupt or truncate a token, and
// so no metacharacter in a token can alter the command being run.
//
// On Linux `secret-tool store` reads the secret from stdin natively.
func (k *KeyringStore) set(ctx context.Context, key CredentialKey, secret []byte) error {
	switch runtime.GOOS {
	case "darwin":
		// -U updates in place when an entry exists. That is what makes a rotation atomic
		// from a reader's point of view: a delete-then-add sequence leaves a window in
		// which a concurrent process sees no credential at all and concludes the user is
		// signed out.
		cmd := fmt.Sprintf(
			"add-generic-password -U -s %s -a %s -l %s -D %s -X %s\n",
			quoteSecurityArg(ServiceName),
			quoteSecurityArg(key.Account()),
			quoteSecurityArg("Daintree Assistant ("+key.Environment+")"),
			quoteSecurityArg("OAuth refresh token"),
			hex.EncodeToString(secret), // hex: no metacharacters, no quoting question
		)
		// NOT -v: the verbose form echoes the whole command, secret included, to stdout.
		out, errOut, err := k.exec(ctx, []string{"-i"}, []byte(cmd))
		if err != nil {
			return classifyDarwin(err, errOut)
		}
		// `security -i` exits 0 even when an individual command fails, so its own error
		// text is the only signal that the write did not happen.
		if msg := combinedToolError(out, errOut); msg != "" {
			return fmt.Errorf("%w: %s", ErrStoreUnavailable, msg)
		}
		return nil
	case "linux":
		_, errOut, err := k.exec(ctx, []string{
			"store", "--label=Daintree Assistant (" + key.Environment + ")",
			"service", ServiceName, "account", key.Account(),
		}, secret)
		if err != nil {
			return classifyLinux(err, nil, errOut)
		}
		return nil
	}
	return ErrStoreUnavailable
}

// Delete removes the secret. An absent entry is success — a user must always be able to
// reach the signed-out state, and failing here would block that.
func (k *KeyringStore) Delete(ctx context.Context, key CredentialKey) error {
	var err error
	var errOut, out []byte
	switch runtime.GOOS {
	case "darwin":
		_, errOut, err = k.exec(ctx, []string{
			"delete-generic-password", "-s", ServiceName, "-a", key.Account(),
		}, nil)
		if err == nil {
			return nil
		}
		if e := classifyDarwin(err, errOut); errors.Is(e, ErrNotFound) {
			return nil
		} else {
			return e
		}
	case "linux":
		out, errOut, err = k.exec(ctx, []string{
			"clear", "service", ServiceName, "account", key.Account(),
		}, nil)
		if err == nil {
			return nil
		}
		if e := classifyLinux(err, out, errOut); errors.Is(e, ErrNotFound) {
			return nil
		} else {
			return e
		}
	}
	return ErrStoreUnavailable
}

// quoteSecurityArg quotes one argument for `security -i`'s command parser.
//
// Only the service name, account and labels go through here — all values this process
// controls — because the SECRET is hex-encoded and needs no quoting at all. Quoting them
// anyway keeps a future caller from turning a label into an injection.
func quoteSecurityArg(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\n', '\r':
			// A newline would end the command line and start a new one.
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// combinedToolError extracts an error line from a tool that exits 0 on failure.
func combinedToolError(out, errOut []byte) string {
	for _, b := range [][]byte{errOut, out} {
		s := strings.TrimSpace(string(b))
		if s == "" {
			continue
		}
		if strings.Contains(strings.ToLower(s), "error") || strings.Contains(s, "security:") {
			return firstLine(b)
		}
	}
	return ""
}

// exec runs the platform tool with a bounded deadline.
func (k *KeyringStore) exec(ctx context.Context, args []string, stdin []byte) ([]byte, []byte, error) {
	path, err := k.tool()
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, keyringTimeout)
	defer cancel()
	out, errOut, runErr := k.run(ctx, path, args, stdin)
	// A CommandContext kill surfaces as an ExitError ("signal: killed"), not as
	// DeadlineExceeded, so the deadline has to be recovered from the context itself.
	// Without this every timeout falls through to the generic branch and is reported as
	// "no credential service", sending the user to fix an installation that is fine.
	if runErr != nil && ctx.Err() != nil {
		return out, errOut, fmt.Errorf("%w: the credential service did not respond within %s", ErrStoreLocked, keyringTimeout)
	}
	return out, errOut, runErr
}

// runCommand is the real exec. Arguments are passed as an argv slice and never through a
// shell.
func runCommand(ctx context.Context, name string, args []string, stdin []byte) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

// Darwin `security` exit codes, which are the OSStatus values offset by 100000 and
// truncated to a byte. Only the ones that need distinct handling are named; everything
// else falls through to ErrStoreUnavailable, which degrades to an explicit memory-tier
// session rather than to a silent plaintext write.
const (
	dsItemNotFound        = 44 // errSecItemNotFound (-25300)
	dsAuthFailed          = 51 // errSecAuthFailed (-25293)
	dsInteractionNotAllow = 36 // errSecInteractionNotAllowed (-25308)
	dsNoSuchKeychain      = 25 // errSecNoSuchKeychain (-25294) — a keychain that is gone
	dsKeychainLocked      = 29 // errSecKeychainLocked-family
)

// classifyDarwin maps a `security` failure onto a sentinel.
func classifyDarwin(err error, stderr []byte) error {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		switch ee.ExitCode() {
		case dsItemNotFound:
			// The ordinary signed-out state. Must not read as a fault, or logout and
			// first-launch both report an error.
			return ErrNotFound
		case dsInteractionNotAllow, dsKeychainLocked, dsAuthFailed:
			// A locked keychain, a non-interactive session, or a denied prompt. The user
			// can fix this by unlocking — which is a completely different instruction
			// from "sign in again", and the reason these are not folded together.
			return fmt.Errorf("%w: %s", ErrStoreLocked, firstLine(stderr))
		case dsNoSuchKeychain:
			return fmt.Errorf("%w: %s", ErrStoreUnavailable, firstLine(stderr))
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: the keychain did not respond", ErrStoreLocked)
	}
	return fmt.Errorf("%w: %v: %s", ErrStoreUnavailable, err, firstLine(stderr))
}

// classifyLinux maps a `secret-tool` failure onto a sentinel.
//
// secret-tool is far less expressive than `security`: a missing item is exit 1 with NO
// output at all, which is indistinguishable by exit code from a real failure. So the
// rule is: exit 1 with nothing on either stream is the ordinary not-found, and anything
// that actually said something is classified from what it said.
//
// Matching on message text is unpleasant, and the fallback is deliberately the SAFE
// one: anything unrecognised is ErrStoreUnavailable, which degrades to an explicit
// memory-tier session, never to a silent plaintext write.
func classifyLinux(err error, stdout, stderr []byte) error {
	quiet := len(bytes.TrimSpace(stderr)) == 0 && len(bytes.TrimSpace(stdout)) == 0
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 && quiet {
		// The normal absent case: `lookup` with no match and `clear` with nothing to
		// remove both land here. Reporting it as a failure would make first launch look
		// broken and would block logout.
		return ErrNotFound
	}

	msg := strings.ToLower(string(stderr))
	switch {
	case strings.Contains(msg, "no such secret"), strings.Contains(msg, "no matching"):
		return ErrNotFound
	case strings.Contains(msg, "dismissed"), strings.Contains(msg, "denied"), strings.Contains(msg, "locked"):
		return fmt.Errorf("%w: %s", ErrStoreLocked, firstLine(stderr))
	case strings.Contains(msg, "cannot autolaunch"), strings.Contains(msg, "not provided by any .service"),
		strings.Contains(msg, "no such interface"), strings.Contains(msg, "connection refused"):
		return fmt.Errorf("%w: no Secret Service is running", ErrStoreUnavailable)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: the credential service did not respond", ErrStoreLocked)
	}
	return fmt.Errorf("%w: %v: %s", ErrStoreUnavailable, err, firstLine(stderr))
}

// firstLine returns a bounded, control-stripped first line of tool output. The tool's
// stderr is not attacker-controlled, but it does reach terminal scrollback and a debug
// log, and it has no business carrying escapes there.
func firstLine(b []byte) string {
	s := string(b)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	return safeEcho(strings.TrimSpace(s))
}
