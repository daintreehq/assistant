//go:build unix

package ipc

import (
	"errors"
	"syscall"
)

// flockExclusive takes a non-blocking exclusive flock on fd. Returns
// (false, nil) when another live process holds it — EWOULDBLOCK is the one
// "expected" outcome and must not surface as an error to callers that poll.
func flockExclusive(fd int) (bool, error) {
	err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return false, nil
	}
	return false, err
}

// flockRelease drops the lock. The fd close that follows would release it
// anyway (flock is fd-scoped); the explicit unlock just makes the handoff
// window deterministic instead of GC/close-ordering dependent.
func flockRelease(fd int) error {
	return syscall.Flock(fd, syscall.LOCK_UN)
}
