package backend

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// MaxKeyLength mirrors the backend's own bearer-token ceiling.
const MaxKeyLength = 4096

// ValidateKeyShape mirrors the backend's structural check (printable non-space ASCII,
// bounded length). The CLI no longer asks anyone for a key, so the only way one arrives
// is DAINTREE_API_KEY; applying the check where the value is resolved turns an obvious
// mis-paste — a shell-mangled value, smart quotes from a chat client — into a readable
// startup error instead of Go's opaque "invalid header field value" on every turn.
//
// Deliberately NO prefix check: the key's issuer is not our business, only that it can
// ride an HTTP header safely.
func ValidateKeyShape(key string) error {
	if strings.TrimSpace(key) == "" {
		return errors.New("a key is required")
	}
	if len(key) > MaxKeyLength {
		return fmt.Errorf("key is too long (%d bytes, max %d)", len(key), MaxKeyLength)
	}
	for _, r := range key {
		if r < 0x21 || r > 0x7E {
			return errors.New("key contains spaces or non-ASCII characters — check for a stray paste")
		}
	}
	return nil
}

// isLoopbackHost reports whether a hostname addresses this machine. Covers the literal
// names as well as any address in 127.0.0.0/8 and ::1, so 127.0.0.2 and friends work.
//
// A trailing DNS root dot is stripped first: "localhost." resolves identically to
// "localhost", and a check that missed it would classify the same destination two
// different ways depending on spelling.
func isLoopbackHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	h = strings.TrimSuffix(h, ".")
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// ValidatePlaintextRemote checks the one specifically security-relevant property a
// backend endpoint URL must satisfy: a plaintext http:// scheme is acceptable only
// for a loopback address, unless explicitly authorized. A normal request carries
// conversation history, terminal output, file excerpts, and tool results — even
// with no bearer credential, sending that in the clear to anything but this
// machine is a confidentiality failure, and an on-path attacker on a plaintext hop
// can also rewrite the response to inject tool calls that then run under the
// session's tier and grants.
//
// Shared by config.LoadConfig (the startup path: override / trusted env / a
// stored `/backend` preference) and app.ResolveBackendTarget (the interactive
// `/backend <url>` switch) so the two never drift on this specific property, even
// though each layers its own additional hygiene checks around it (userinfo,
// query/fragment, control characters — see ResolveBackendTarget's doc comment).
func ValidatePlaintextRemote(rawURL string, allowInsecure bool) error {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return fmt.Errorf("invalid backend URL %q: %w", rawURL, err)
	}
	if u.Scheme != "http" || allowInsecure || isLoopbackHost(u.Hostname()) {
		return nil
	}
	return fmt.Errorf(
		"%s is plaintext http to a remote host — every turn would cross that wire in the clear. "+
			"Use https://, a loopback address, or authorize it explicitly (--allow-insecure-backend or DAINTREE_ALLOW_INSECURE_BACKEND=1)",
		u.Host)
}

// IsLoopbackURL reports whether a base URL addresses THIS machine, whatever its
// spelling — scheme, port, case, bracketed IPv6, and a trailing DNS root dot are all
// normalised away by url.Parse + isLoopbackHost.
//
// Callers use it to decide how much to trust an endpoint. "Loopback" is the one
// property that is decidable from a URL alone and cannot be spoofed by a similar-looking
// hostname: there is no `evil.com` spelling that parses to 127.0.0.1. Contrast an
// "is this OUR host" test, which has an unbounded alias surface (default ports, IDNA,
// trailing dots, userinfo) and fails OPEN on every spelling it hasn't thought of — so
// security decisions here are phrased as "is this local?", never "is this official?".
//
// A URL that does not parse is NOT loopback: an unparseable endpoint gets the strict
// treatment, not the lenient one.
func IsLoopbackURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return false
	}
	return isLoopbackHost(u.Hostname())
}
