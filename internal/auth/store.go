package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// store.go defines where a refresh token lives, and what "safely" means for it.
//
// Exactly one secret is persisted: the refresh token. The access token stays in process
// memory and is never written anywhere, because it is short-lived and re-derivable —
// persisting it would add a second place to leak from in exchange for saving one
// network round trip an hour.
//
// The rule that shapes everything else here is NO SILENT PLAINTEXT. A credential store
// that quietly degrades to a file on disk when the keychain is unavailable is worse than
// one that refuses, because the user is told they are signed in and never told the
// terms changed. When there is no OS credential service, the session lives in memory for
// this process only and the caller is told so explicitly.

// StorageTier names where a credential is actually kept, so status can say so.
type StorageTier string

const (
	// TierKeychain: an OS credential service holds the refresh token.
	TierKeychain StorageTier = "keychain"
	// TierMemory: no credential service is available, so the session lives only in this
	// process and will not survive exit. Never silently chosen — a caller that lands
	// here must tell the user.
	TierMemory StorageTier = "memory"
	// TierUnavailable: storage could not be determined at all.
	TierUnavailable StorageTier = "unavailable"
)

// Sentinel errors. Distinguished because the four cases need four different responses,
// and collapsing them is how "your keychain is locked" gets reported as "please sign in
// again" — sending someone through a browser flow that will fail at the same write.
var (
	// ErrNotFound: no credential is stored. The ordinary signed-out state, not a fault.
	ErrNotFound = errors.New("auth: no stored session")
	// ErrStoreUnavailable: no credential service exists on this machine.
	ErrStoreUnavailable = errors.New("auth: no OS credential store is available")
	// ErrStoreLocked: a credential service exists but refused access — a locked
	// keychain, a denied prompt. Retryable once the user unlocks it.
	ErrStoreLocked = errors.New("auth: the OS credential store denied access")
	// ErrStoreCorrupt: something is stored under our key that this build cannot read.
	ErrStoreCorrupt = errors.New("auth: the stored session could not be decoded")
)

// storedSessionVersion is the on-disk schema version for a stored session. Bumping it
// makes an older build's entry read as ErrStoreCorrupt rather than being misinterpreted.
const storedSessionVersion = 1

// StoredSession is everything persisted about a login: the refresh token plus the
// minimum needed to know which issuer and client it belongs to.
//
// It deliberately holds NO access token, NO email, and NO user id. Identity display
// comes from the backend's session endpoint, which is authoritative and current; a copy
// cached beside the credential would go stale and would put personal data in the OS
// credential store for no benefit.
type StoredSession struct {
	Version int `json:"v"`
	// RefreshToken is the one secret. Rotating: each use may mint a replacement.
	RefreshToken string `json:"refresh_token"`
	// Issuer and ClientID identify what this credential is FOR. They are stored beside
	// it so a token minted for staging can be recognised — and refused — after the
	// endpoint is pointed somewhere else.
	Issuer   string `json:"issuer"`
	ClientID string `json:"client_id"`
	// Environment is carried for the same reason, and for status display.
	Environment string `json:"environment"`
}

// Valid reports whether a decoded session is usable.
func (s StoredSession) Valid() bool {
	return s.Version == storedSessionVersion &&
		strings.TrimSpace(s.RefreshToken) != "" &&
		strings.TrimSpace(s.Issuer) != "" &&
		strings.TrimSpace(s.ClientID) != ""
}

// CredentialKey identifies one account credential.
//
// Keying by backend origin AND issuer AND environment AND client id is what stops a
// token minted for one deployment being sent to another. That matters more than it
// looks: `/backend` can repoint the CLI at a different endpoint mid-session, and a
// bearer that followed it would hand a staging credential to a production server, or a
// production one to whatever a developer is running on loopback.
type CredentialKey struct {
	// StateRoot namespaces the credential by the directory this process coordinates in.
	//
	// It is here because the lock and the revision marker live under the state root
	// while the OS credential entry does not, and `--state-dir` moves the former without
	// moving the latter. Two processes with different state dirs would then share one
	// refresh token but coordinate through different locks and different markers: they
	// could rotate the SAME rotating token concurrently, and a logout in one would be
	// invisible to the other. Binding the credential to its coordination root makes the
	// two move together — which also gives tests and harnesses real isolation instead of
	// a shared real credential.
	StateRoot     string
	BackendOrigin string
	Issuer        string
	Environment   string
	ClientID      string
}

// KeyFor builds the credential key for a coordination root, a backend origin and a
// validated manifest.
func KeyFor(stateRoot, backendOrigin string, m *Manifest) CredentialKey {
	return CredentialKey{
		StateRoot:     strings.TrimRight(strings.TrimSpace(stateRoot), "/"),
		BackendOrigin: strings.TrimRight(strings.TrimSpace(backendOrigin), "/"),
		Issuer:        m.Issuer,
		Environment:   m.Environment,
		ClientID:      m.ClientID,
	}
}

// String renders the key for humans and logs. It contains no secret — every component
// is public configuration.
func (k CredentialKey) String() string {
	return fmt.Sprintf("%s|%s|%s|%s|%s", k.Environment, k.Issuer, k.ClientID, k.BackendOrigin, k.StateRoot)
}

// Account is the credential-store account name for this key.
//
// A hash rather than the raw tuple, for two reasons. It bounds the length (Secret
// Service and Keychain both have limits, and a backend origin is arbitrary), and it
// keeps a URL out of a field that shows up in GUI keychain browsers. It is not a
// security measure: the components are public.
func (k CredentialKey) Account() string {
	sum := sha256.Sum256([]byte(k.String()))
	return hex.EncodeToString(sum[:16])
}

// ServiceName is the credential-store service under which every Daintree Assistant
// account credential is filed.
const ServiceName = "org.daintree.assistant.oauth"

// Store persists exactly one secret per credential key.
type Store interface {
	// Load returns the stored session, or ErrNotFound when there is none.
	Load(ctx context.Context, key CredentialKey) (StoredSession, error)
	// Save writes the session, replacing any previous one atomically from the caller's
	// point of view. A partially-written credential must never be observable: a reader
	// that saw one would treat a live login as corrupt and sign the user out.
	Save(ctx context.Context, key CredentialKey, session StoredSession) error
	// Delete removes the session. Deleting a session that is not there is NOT an error —
	// a user must always be able to reach the signed-out state, and reporting failure
	// for an already-absent credential would block that.
	Delete(ctx context.Context, key CredentialKey) error
	// Tier reports where this store actually keeps things, for status.
	Tier(ctx context.Context) StorageTier
}

// MemoryStore keeps sessions in process memory only.
//
// It is the explicit fallback when no OS credential service exists, and the default in
// tests. It is never chosen silently: OpenStore returns it together with the reason, and
// the caller is responsible for telling the user their login will not survive exit.
type MemoryStore struct {
	mu       sync.Mutex
	sessions map[string]StoredSession
}

// NewMemoryStore builds an empty in-process store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{sessions: make(map[string]StoredSession)}
}

// Load returns the in-memory session for a key.
func (m *MemoryStore) Load(_ context.Context, key CredentialKey) (StoredSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[key.Account()]
	if !ok {
		return StoredSession{}, ErrNotFound
	}
	return s, nil
}

// Save records the session in memory.
func (m *MemoryStore) Save(_ context.Context, key CredentialKey, session StoredSession) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[key.Account()] = session
	return nil
}

// Delete removes the session. Absent is success.
func (m *MemoryStore) Delete(_ context.Context, key CredentialKey) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, key.Account())
	return nil
}

// Tier reports memory storage.
func (m *MemoryStore) Tier(context.Context) StorageTier { return TierMemory }
