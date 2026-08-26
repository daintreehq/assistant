package auth

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// keyref.go remembers WHICH credential this machine last stored, so logout works
// offline.
//
// Without it, Logout has to fetch the auth manifest to derive the credential key — which
// means a laptop on a plane, or one pointed at a backend that is down, cannot sign out.
// That directly contradicts the guarantee the whole logout path is built around: a user
// must always be able to remove access from their own machine, and making that
// conditional on a network call hands the decision to whoever runs the server.
//
// Everything stored here is NON-SECRET: an environment name, an issuer URL, a public
// OAuth client id, a backend origin, a state root. It is the ADDRESS of a credential,
// never the credential. It is 0600 for consistency with the rest of the state root, not
// because its contents are sensitive — the same reason the revision marker is.

// keyRefFileName is the last-credential descriptor inside the auth directory.
const keyRefFileName = "credential.json"

// maxKeyRefBytes bounds the read.
const maxKeyRefBytes = 8 << 10

// keyRefPath is the descriptor path for an auth directory.
func keyRefPath(dir string) string { return filepath.Join(dir, keyRefFileName) }

// saveKeyRef records the credential key a successful login just stored under.
func saveKeyRef(dir string, key CredentialKey) error {
	body, err := json.Marshal(key)
	if err != nil {
		return wrapError(CodeExchangeFailed, "could not record the credential descriptor", err)
	}
	return writeAtomic(keyRefPath(dir), append(body, '\n'))
}

// keyRefState is what this machine can say about a recorded login, and the third case
// is the one that matters.
//
// "There is no descriptor" and "there is a descriptor and I cannot read it" are opposite
// facts, and every caller here used to receive them as the same ok=false. The first is a
// definitive local "no login". The second is a FAULT, and treating it as the first is
// how an unreadable auth directory turns a signed-in session into an anonymous request
// that the backend's open door accepts — see AccessToken.
type keyRefState int

const (
	// keyRefAbsent: no descriptor. Definitive: this machine has no recorded login.
	keyRefAbsent keyRefState = iota
	// keyRefPresent: a descriptor was read.
	keyRefPresent
	// keyRefUnreadable: a descriptor exists and could not be read or parsed. Says
	// nothing either way about whether a login exists.
	keyRefUnreadable
)

// readKeyRef reads the last recorded credential key and reports which of the three
// answers this machine can actually give.
func readKeyRef(dir string) (CredentialKey, keyRefState) {
	f, err := os.Open(keyRefPath(dir))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return CredentialKey{}, keyRefAbsent
		}
		return CredentialKey{}, keyRefUnreadable
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxKeyRefBytes))
	if err != nil {
		return CredentialKey{}, keyRefUnreadable
	}
	var k CredentialKey
	if err := json.Unmarshal(raw, &k); err != nil {
		return CredentialKey{}, keyRefUnreadable
	}
	if k.Issuer == "" || k.ClientID == "" {
		// Present, parseable, and naming nothing. Corrupt rather than absent: a login
		// wrote this file, so concluding "never signed in" from it would be a guess.
		return CredentialKey{}, keyRefUnreadable
	}
	return k, keyRefPresent
}

// loadKeyRef reads the last recorded credential key.
//
// It collapses readKeyRef's three answers to two, for the callers whose only question is
// "can I name a credential right now" — the offline logout path and the rollback's
// does-this-descriptor-name-my-key check. A caller deciding whether a login EXISTS must
// use readKeyRef instead.
func loadKeyRef(dir string) (CredentialKey, bool) {
	k, st := readKeyRef(dir)
	return k, st == keyRefPresent
}

// forgetKeyRef removes the descriptor. Absence is success.
func forgetKeyRef(dir string) error { return removeIfPresent(keyRefPath(dir)) }
