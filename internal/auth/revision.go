package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/daintreehq/assistant/internal/ipc"
)

// revision.go is a non-secret marker that tells every other process "the credential
// changed — drop what you cached".
//
// It exists because of the supervisor daemon, the consumer that makes this problem
// interesting: it can make PAID requests hours after the visible UI closed. When someone
// logs out in a terminal, that daemon still holds an access token that stays
// cryptographically valid until its expiry. Without a signal it keeps spending on an
// account the user believes they signed out of, for up to an hour.
//
// The obvious alternative — push the new credential to every process over IPC — is
// exactly what must NOT happen. It would put a rotating secret into daemon credential
// frames, into project-scoped process memory, and into whatever logs that path touches.
// Every process can already reach the credential store; all any of them needs is to know
// that what it cached is stale. This file says that and discloses nothing.
//
// The marker is a NONCE plus a counter, not a bare counter, and that is not decoration.
// A bare counter is vulnerable to ABA: delete the file — a `reset`, a tmp cleaner, a
// fresh container layer — and every reader sees 0 again. A daemon that had observed 1
// would then see a recreated file bumped back to 1 and conclude nothing had changed,
// silently keeping a logged-out session alive. The nonce is regenerated whenever the
// file is created, so a recreated file can never collide with what anyone observed.

const (
	// revisionFileName is the marker file inside the auth directory.
	revisionFileName = "revision"
	// revisionLockName serializes the read-modify-write in Bump.
	revisionLockName = "revision.lock"
	// maxRevisionFileBytes bounds the read. The file holds "<nonce> <counter>"; anything
	// larger is a symlink to something else or a corrupted write.
	maxRevisionFileBytes = 128
	// revisionNonceBytes is the per-file-creation identity.
	revisionNonceBytes = 8
)

// Marker is one observation of the shared credential generation.
//
// Nonce identifies the FILE; Counter identifies the mutation within it. Comparing both
// is what makes the marker reset-resistant.
type Marker struct {
	Nonce   string
	Counter uint64
}

// Zero reports the absent/unreadable marker.
func (m Marker) Zero() bool { return m.Nonce == "" && m.Counter == 0 }

// String renders the marker for comparison, logs and status.
func (m Marker) String() string {
	if m.Zero() {
		return "0"
	}
	return m.Nonce + ":" + strconv.FormatUint(m.Counter, 10)
}

// Revision tracks the credential generation this process has observed.
type Revision struct {
	dir  string
	path string

	mu       sync.Mutex
	observed Marker
}

// NewRevision builds a revision tracker over an auth directory.
func NewRevision(dir string) *Revision {
	return &Revision{dir: dir, path: filepath.Join(dir, revisionFileName)}
}

// Path returns the marker file path (for diagnostics).
func (r *Revision) Path() string { return r.path }

// Current reads the shared marker.
//
// A missing or unparseable file reads as the zero Marker rather than failing. That is
// the safe direction: this marker's only job is to invalidate a cache, and an unreadable
// file must not be able to take the assistant offline over a value that is neither
// secret nor authoritative — the credential store remains the authority on whether a
// login exists. A zero marker still compares UNEQUAL to any real observation, so a
// process that had observed something correctly sees a change.
func (r *Revision) Current() Marker {
	f, err := os.Open(r.path)
	if err != nil {
		return Marker{}
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxRevisionFileBytes))
	if err != nil {
		return Marker{}
	}
	nonce, countStr, ok := strings.Cut(strings.TrimSpace(string(raw)), " ")
	if !ok {
		return Marker{}
	}
	n, err := strconv.ParseUint(strings.TrimSpace(countStr), 10, 64)
	if err != nil || strings.TrimSpace(nonce) == "" {
		return Marker{}
	}
	return Marker{Nonce: nonce, Counter: n}
}

// Observed returns the marker this process last adopted.
func (r *Revision) Observed() Marker {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.observed
}

// MarkObserved records that this process has adopted a marker.
func (r *Revision) MarkObserved(m Marker) {
	r.mu.Lock()
	r.observed = m
	r.mu.Unlock()
}

// Changed reports whether the shared marker differs from what this process adopted. A
// caller seeing true must drop its in-memory access token and reload under the
// credential lock.
//
// It does NOT auto-adopt. Adopting is the caller's decision, made after it has actually
// re-read the credential; marking it here would let a process observe the change, fail
// to reload, and then never notice again.
func (r *Revision) Changed() bool { return r.Current() != r.Observed() }

// Bump advances the shared marker and adopts the new value in this process.
//
// It takes its OWN lock rather than relying on the caller's. The credential lock is
// deliberately per-credential, but this file is global, so two correctly-locked
// mutations of DIFFERENT credentials can still race here — and a lost update is not
// cosmetic: a bump that read 5 and is then descheduled can overwrite 7 with 6, and once
// a later mutation restores 7, a daemon that had observed 7 never learns that anything
// happened. A documented "call this under the lock" cannot prevent that, because the
// lock it names is the wrong one.
//
// It is called AFTER the credential write succeeds, never before. Bumping first would
// tell every other process to discard a working token in favour of one that then failed
// to be written, signing the machine out on a transient keychain error.
func (r *Revision) Bump(ctx context.Context) error {
	fl := ipc.NewFileLock(filepath.Join(r.dir, revisionLockName))
	if err := fl.Acquire(ctx, 25*time.Millisecond); err != nil {
		return wrapError(CodeExchangeFailed, "could not serialize the auth revision update", err)
	}
	defer fl.Release()

	cur := r.Current()
	next := Marker{Nonce: cur.Nonce, Counter: cur.Counter + 1}
	if next.Nonce == "" {
		// The file is absent or unreadable: mint a fresh identity so a recreated file can
		// never be confused with the one anybody observed before it.
		nonce, err := newNonce()
		if err != nil {
			return err
		}
		next = Marker{Nonce: nonce, Counter: 1}
	}
	if err := writeAtomic(r.path, []byte(next.Nonce+" "+strconv.FormatUint(next.Counter, 10)+"\n")); err != nil {
		return err
	}
	r.MarkObserved(next)
	return nil
}

// newNonce mints a file identity.
func newNonce() (string, error) {
	buf := make([]byte, revisionNonceBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", wrapError(CodeExchangeFailed, "could not generate secure random data", err)
	}
	return hex.EncodeToString(buf), nil
}

// writeAtomic writes a file through a temp-and-rename.
//
// Atomic rename rather than a truncating write, for the same reason the endpoint
// preference uses one: a crash mid-write would otherwise leave a half-file that parses
// as the zero marker, which is the one value that must never appear by accident.
func writeAtomic(path string, body []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return wrapError(CodeExchangeFailed, "could not create the auth state directory", err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*")
	if err != nil {
		return wrapError(CodeExchangeFailed, "could not write auth state", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return wrapError(CodeExchangeFailed, "could not write auth state", err)
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return wrapError(CodeExchangeFailed, "could not write auth state", err)
	}
	if err := tmp.Close(); err != nil {
		return wrapError(CodeExchangeFailed, "could not write auth state", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return wrapError(CodeExchangeFailed, "could not write auth state", err)
	}
	return nil
}

// removeIfPresent deletes a file, treating absence as success.
func removeIfPresent(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
