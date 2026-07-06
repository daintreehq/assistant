package ipc

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// SocketDirEnv overrides where control sockets are created. Test/dev only —
// the product path is always the per-user sockets root. It exists because
// hermetic tests must not touch the real ~/.daintree, and because unix socket
// paths cap at ~104 bytes on darwin, so tests with deep TempDirs need a short
// override they control.
const SocketDirEnv = "DAINTREE_ASSISTANT_SOCKET_DIR"

// SocketPathFor derives the deterministic control-socket path for a project
// state dir. The socket does NOT live inside the state dir: state dirs nest
// under ~/.daintree/assistant-cli/<project-slug>-<hash>/ and easily overflow
// darwin's 104-byte sun_path limit. Instead every socket lives in a flat
// per-user 0700 root keyed by a hash of the (absolute) state dir, keeping the
// full path short while staying one-to-one with the project DB.
func SocketPathFor(stateDir string) (string, error) {
	root := os.Getenv(SocketDirEnv)
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home for socket dir: %w", err)
		}
		root = filepath.Join(home, ".daintree", "sockets")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("create socket dir %s: %w", root, err)
	}
	abs, err := filepath.Abs(stateDir)
	if err != nil {
		abs = stateDir
	}
	sum := sha256.Sum256([]byte(abs))
	return filepath.Join(root, "d"+hex.EncodeToString(sum[:])[:12]+".sock"), nil
}

// DaemonDescriptorName is the discovery/debug descriptor the daemon writes
// into the state dir while running. The flocks are the authority on liveness;
// this file just tells humans and `status` where the socket is and which pid
// wrote it.
const DaemonDescriptorName = "daemon.json"

// DaemonDescriptor is the on-disk shape of daemon.json.
type DaemonDescriptor struct {
	Pid         int    `json:"pid"`
	SocketPath  string `json:"socketPath"`
	StartedAtMs int64  `json:"startedAtMs"`
	Version     string `json:"version"`
}

// WriteDaemonDescriptor persists daemon.json (0600) into the state dir.
func WriteDaemonDescriptor(stateDir string, d DaemonDescriptor) error {
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(stateDir, DaemonDescriptorName), b, 0o600)
}

// ReadDaemonDescriptor loads daemon.json. (nil, nil) when absent.
func ReadDaemonDescriptor(stateDir string) (*DaemonDescriptor, error) {
	b, err := os.ReadFile(filepath.Join(stateDir, DaemonDescriptorName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var d DaemonDescriptor
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, fmt.Errorf("parse %s: %w", DaemonDescriptorName, err)
	}
	return &d, nil
}

// RemoveDaemonDescriptor deletes daemon.json, ignoring absence.
func RemoveDaemonDescriptor(stateDir string) {
	_ = os.Remove(filepath.Join(stateDir, DaemonDescriptorName))
}
