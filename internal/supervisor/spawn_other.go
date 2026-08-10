//go:build !unix

package supervisor

import (
	"errors"

	"github.com/daintreehq/assistant/internal/config"
)

// The supervisor daemon depends on unix process/lock semantics (Setsid, flock).
func spawnDaemon(config.AppConfig, string) error {
	return errors.New("the supervisor daemon is not supported on this platform")
}
