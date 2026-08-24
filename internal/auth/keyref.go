package auth

import (
	"encoding/json"
	"io"
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

// loadKeyRef reads the last recorded credential key.
//
// A missing or unreadable descriptor returns ok=false rather than an error: it is a
// convenience for the offline path, and a caller that cannot read it should fall back to
// discovery rather than fail. The only situation where its absence is fatal is an
// offline logout with no prior login on this machine — which is a no-op anyway.
func loadKeyRef(dir string) (CredentialKey, bool) {
	f, err := os.Open(keyRefPath(dir))
	if err != nil {
		return CredentialKey{}, false
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxKeyRefBytes))
	if err != nil {
		return CredentialKey{}, false
	}
	var k CredentialKey
	if err := json.Unmarshal(raw, &k); err != nil {
		return CredentialKey{}, false
	}
	if k.Issuer == "" || k.ClientID == "" {
		return CredentialKey{}, false
	}
	return k, true
}

// forgetKeyRef removes the descriptor. Absence is success.
func forgetKeyRef(dir string) error { return removeIfPresent(keyRefPath(dir)) }
