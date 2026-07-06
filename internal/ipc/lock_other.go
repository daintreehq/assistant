//go:build !unix

package ipc

import "errors"

// The supervisor's ownership model is built on flock semantics (kernel-released
// on process death). There is no Windows port yet; failing loudly beats a lock
// that silently doesn't exclude.
var errFlockUnsupported = errors.New("ipc: file locks are not supported on this platform")

func flockExclusive(fd int) (bool, error) { return false, errFlockUnsupported }

func flockRelease(fd int) error { return errFlockUnsupported }
