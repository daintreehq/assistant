package auth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/daintreehq/assistant/internal/ipc"
)

// lock.go serializes credential mutations ACROSS PROCESSES.
//
// An in-process mutex is not enough here, and the reason is specific to how this binary
// is used. A machine routinely has several of these running at once against the same
// account: a terminal session, an embedded host Daintree drives, and one supervisor
// daemon per project. Supabase refresh tokens ROTATE and are one-time use, so two of
// them refreshing concurrently means one wins, the other presents a token that has just
// been consumed, and — depending on how the provider treats reuse — either that process
// is signed out or the whole session is revoked as a suspected theft.
//
// So the lock is a file lock, and it is deliberately the SAME primitive the supervisor
// already uses for its ownership lease (internal/ipc.FileLock): flock, released by the
// kernel when the holder dies. That last property is what makes it safe here. A crashed
// process mid-refresh must not leave every other process unable to refresh forever, and
// no cleanup code of ours runs reliably enough to guarantee that — the kernel's does.
//
// The lock is per-CREDENTIAL, not global: two different accounts (a staging login and a
// production one) have no reason to block each other.

// authDirName is the subdirectory of the per-user state ROOT holding auth coordination
// files. Deliberately not the per-project state dir: an account is a property of the
// person, and a lock scoped per project would let two projects rotate concurrently,
// which is the exact failure this file exists to prevent.
const authDirName = "auth"

// lockWait bounds how long we wait for another process's refresh.
//
// Sized against the operation it waits on: a token refresh is one HTTPS round trip plus
// a credential-store write, which is a second or two. Waiting 30 covers a slow network
// and a keychain prompt; waiting forever would let one wedged process hang every other
// one on the machine, including a daemon that is only trying to decide whether to stop.
const lockWait = 30 * time.Second

// AuthDir returns the auth coordination directory under a state root, creating it 0700.
func AuthDir(stateRoot string) (string, error) {
	dir := filepath.Join(stateRoot, authDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", wrapError(CodeExchangeFailed, "could not create the auth state directory", err)
	}
	return dir, nil
}

// lockPath is the lock file for one credential key.
func lockPath(dir string, key CredentialKey) string {
	return filepath.Join(dir, key.Account()+".lock")
}

// credentialLock is a held cross-process lock for one credential.
type credentialLock struct{ fl *ipc.FileLock }

// acquireCredentialLock blocks until this process holds the credential lock, the
// context is done, or lockWait elapses.
func acquireCredentialLock(ctx context.Context, dir string, key CredentialKey) (*credentialLock, error) {
	fl := ipc.NewFileLock(lockPath(dir, key))
	ctx, cancel := context.WithTimeout(ctx, lockWait)
	defer cancel()
	if err := fl.Acquire(ctx, 50*time.Millisecond); err != nil {
		// Only a DEADLINE means contention. Acquire also returns on caller cancellation
		// and on a real filesystem error (an unwritable auth directory, a full disk),
		// and telling someone to "close the other process" when the actual fault is
		// permissions sends them after something that will not help.
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return nil, wrapError(CodeExchangeFailed,
				"another Daintree process is holding the sign-in credential", err).
				withHint("Wait for it to finish, or close it and try again.")
		case errors.Is(err, context.Canceled):
			return nil, wrapError(CodeCancelled, "cancelled while waiting for the sign-in credential", err)
		default:
			return nil, wrapError(CodeExchangeFailed,
				"could not lock the sign-in credential", err).
				withHint("Check that " + dir + " is writable.")
		}
	}
	return &credentialLock{fl: fl}, nil
}

// release drops the lock. Idempotent.
func (l *credentialLock) release() {
	if l != nil && l.fl != nil {
		l.fl.Release()
	}
}
