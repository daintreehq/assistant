package backend

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"unicode"
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

// MaxBaseURLLength bounds a backend endpoint. Any real one is far shorter; the cap only
// stops an absurd value being dialed, persisted and rendered.
const MaxBaseURLLength = 2048

// PlaintextRemoteError is the ONE rejection callers have to be able to tell apart from
// the rest, which is why it is a type rather than a message.
//
// Two of them need it. config.LoadConfig separates "the stored preference is plaintext
// remote" (EndpointInsecureRejected) from "the stored preference is malformed"
// (EndpointShapeRejected), because a surface reporting them says different things.
// app.ResolveBackendTarget needs it to substitute its own remedy: startup has an escape
// hatch for this and `/backend` deliberately does not, so the two must be able to phrase
// the same refusal differently without re-deriving it.
//
// Host is the host:port that was refused — never the raw input, which can carry
// userinfo (see NormalizeBaseURL's redaction rule).
type PlaintextRemoteError struct {
	Host string
}

func (e *PlaintextRemoteError) Error() string {
	return fmt.Sprintf(
		"%s is plaintext http to a remote host — every turn would cross that wire in the clear. "+
			"Use https://, a loopback address, or authorize it explicitly (--allow-insecure-backend or DAINTREE_ALLOW_INSECURE_BACKEND=1)",
		e.Host)
}

// NormalizeBaseURL is THE backend base-URL validator: every source of an endpoint goes
// through it, and it returns the ONE canonical spelling of whatever it accepts.
//
// "Every source" is the point. The endpoint arrives from five places — `--backend-url`,
// the trusted DAINTREE_BACKEND_URL, the preference `/backend` stored on disk, the
// compiled-in default, and `/backend <url>` typed mid-session — and for a long time only
// the last of them was checked properly. Startup applied the plaintext rule alone, so a
// value the interactive command flatly refused was accepted at launch and dialed for the
// whole session. Two doors into the same decision is how they drift; this is the door.
//
// Everything rejected here is something that fails silently or dangerously if it is
// allowed through:
//
//   - **userinfo** (`https://user:pass@host`). Go's http.Client turns URL userinfo into a
//     Basic `Authorization` header automatically when no other one is set, so this
//     quietly starts authenticating every request with a credential nothing in this
//     process knows it is sending. It would also be persisted in cleartext and rendered.
//   - **query or fragment**. The client joins the API path onto this base, so
//     `https://host?token=x` becomes `https://host?token=x/v1/daintree/respond` and the
//     request lands on `/`. A fragment is never sent at all. Both produce a baffling
//     404 rather than an obviously wrong endpoint.
//   - **anything but http/https, or no host at all**. `ftp://host` and `127.0.0.1:8473`
//     (which parses as a URL with an EMPTY host) both become an unhelpful transport
//     error much later, against an endpoint that was never real.
//   - **plaintext http:// to a REMOTE host**, unless allowInsecure authorizes it. Every
//     turn carries the whole conversation, the project context, tool arguments and tool
//     results across that wire, and an on-path attacker can also rewrite the streamed
//     response to inject tool calls that then run under the session's tier and grants.
//     Loopback is exempt: there is no network to intercept, and it is the local
//     development loop.
//   - **control characters, whitespace and backslashes**. The first would otherwise reach
//     the terminal through the masthead and command cards before request construction
//     ever rejected them; the others exist only to make one URL read as two different
//     ones to a human and to a parser.
//
// allowInsecure is a PARAMETER rather than an ambient lookup because the answer differs
// by caller and must keep differing: startup honours --allow-insecure-backend /
// DAINTREE_ALLOW_INSECURE_BACKEND, and `/backend` passes false because a session must not
// be able to talk itself onto a plaintext remote endpoint from the inside.
//
// REDACTION RULE, and it applies to every error this function returns: the raw input is
// never echoed. A rejected endpoint is exactly the kind of value that carries
// `user:password@` or `?token=`, and these messages reach the terminal, the startup
// diagnostic, /status and any log capturing them. Only structural facts — a host:port, a
// byte count, the name of the property that failed — cross that boundary.
func NormalizeBaseURL(rawURL string, allowInsecure bool) (string, error) {
	raw := strings.TrimSpace(rawURL)
	if raw == "" {
		return "", errors.New("no endpoint given")
	}
	if len(raw) > MaxBaseURLLength {
		return "", fmt.Errorf("endpoint is too long (%d bytes, max %d)", len(raw), MaxBaseURLLength)
	}
	// Screened BEFORE url.Parse, because the parser is lenient about some of this and
	// because a message about a character is more useful than whatever the parser would
	// have said about the shape it produced. Interior whitespace is included: only the
	// OUTER kind is a paste artifact worth forgiving, and a space in the middle is either
	// a mangled value or a deliberate attempt to make one URL look like another.
	for _, r := range raw {
		switch {
		case r == '\\':
			return "", errors.New("an endpoint must not contain a backslash — it is not a path separator here, and parsers disagree about what it means")
		case unicode.IsSpace(r):
			return "", errors.New("an endpoint must not contain spaces — check for a stray paste or a wrapped line")
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r):
			// Cf as well as Cc: U+202E RIGHT-TO-LEFT OVERRIDE is not a "control
			// character" by unicode.IsControl, and it exists to make one host read as
			// another everywhere this endpoint is printed — the masthead, a command
			// card, the debug log.
			return "", errors.New("endpoint contains control characters — check for a stray paste")
		}
	}
	// The scheme is checked on the literal prefix as well as on the parsed value. A bare
	// `host:port` parses with scheme "host" and no host at all, and saying "that needs
	// http:// or https://" is the only reading of it that helps.
	lower := strings.ToLower(raw)
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		return "", errors.New("an endpoint needs an http:// or https:// scheme")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("that is not a usable URL: %s", safeParseReason(err))
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		// Unreachable through the prefix check above, and kept anyway: this function is
		// the security boundary, and a boundary that only holds because of a check
		// somewhere else above it is one refactor from not holding at all.
		return "", errors.New("an endpoint needs an http:// or https:// scheme")
	}
	if u.Host == "" || u.Hostname() == "" {
		return "", errors.New("an endpoint needs a host")
	}
	// The authority has to be a REAL one, not merely non-empty. net/url is lenient here
	// in ways that surface only as a transport error much later, a whole session after
	// the launch that accepted them: it parses `host:443:443` (Hostname() comes back as
	// "backend.example:443"), a port outside 1–65535, a bare trailing colon, and a host
	// carrying raw bytes that arrived percent-encoded. A validator that let those past
	// would boot cleanly and then fail every single turn.
	if strings.HasSuffix(u.Host, ":") {
		return "", errors.New("an endpoint's port is missing after the colon")
	}
	host := u.Hostname()
	if !strings.HasPrefix(u.Host, "[") && strings.Contains(host, ":") {
		return "", errors.New("an endpoint's host carries more than one port")
	}
	if port := u.Port(); port != "" {
		n, perr := strconv.Atoi(port)
		if perr != nil || n < 1 || n > 65535 {
			return "", errors.New("an endpoint's port must be a number between 1 and 65535")
		}
	}
	for _, r := range host {
		// Letters and digits keep internationalized domains usable; `-`, `.` and `_`
		// are the separators a real hostname uses, and `:` is reachable only inside the
		// brackets of an IPv6 literal, which the check above has already pinned. A byte
		// that arrived as `%FF` decodes to something that is none of these.
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '.' || r == '_' || r == ':' {
			continue
		}
		return "", errors.New("an endpoint's host contains a character that cannot appear in a host name")
	}
	if u.User != nil {
		return "", errors.New("an endpoint must not embed a username or password — Go would send it as an Authorization header on every request")
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" {
		return "", errors.New("an endpoint must not carry a query string or fragment — the API path is joined onto it, so the request would never reach the API")
	}
	if perr := plaintextRemoteCheck(u, allowInsecure); perr != nil {
		return "", perr
	}
	// Rebuild from the parsed form rather than returning the input, so the stored, dialed
	// and displayed value is the canonical one and two spellings of the same endpoint
	// compare equal — the string comparison in App.SetBackendURL is one place that would
	// otherwise treat `https://x/` and `https://x` as different endpoints, and a
	// DAINTREE_BACKEND_URL that disagreed with a `/backend` value by one trailing slash
	// is the shape of bug this exists to make impossible.
	//
	// Trailing slashes come off the ESCAPED path, never the decoded one. `%2F` decodes to
	// '/' as well, so trimming the decoded form would silently rename an endpoint's last
	// path segment instead of tidying a separator off the end of it. Host case is left
	// exactly as typed: it is the one part of a URL nothing here is entitled to rewrite.
	if escaped := u.EscapedPath(); escaped != "" {
		if trimmed := strings.TrimRight(escaped, "/"); trimmed != escaped {
			decoded, derr := url.PathUnescape(trimmed)
			if derr != nil {
				return "", errors.New("an endpoint's path contains an invalid %-escape")
			}
			u.Path, u.RawPath = decoded, trimmed
		}
	}
	return u.String(), nil
}

// safeParseReason renders WHY a URL failed to parse without ever echoing the value.
//
// url.Error embeds the raw input in its Error() string, and several of the causes
// underneath it quote a fragment of the input too — the offending host character
// (url.InvalidHostError), the text it read as a port, a bad %-escape (url.EscapeError).
// Unwrapping alone is therefore not enough, so each of those is replaced by a
// description of the SHAPE that failed.
//
// The causes are matched on their message because net/url does not export them as
// distinguishable values. That is deliberately allowed to rot: an unrecognised cause
// falls through to the generic line, so a Go release that rewords one costs a little
// actionability and leaks nothing.
func safeParseReason(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		err = ue.Err
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "after host"):
		return "its port is not a number"
	case strings.Contains(msg, "host name"):
		return "its host contains a character that cannot appear in a host name"
	case strings.Contains(msg, "missing ']'"):
		return "its bracketed IPv6 host is not closed"
	case strings.Contains(msg, "invalid URL escape"):
		return "it contains an invalid %-escape"
	}
	return "it is not a valid URL"
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
// This is the plaintext HALF of NormalizeBaseURL, exported for a caller that holds a URL
// it has no business re-canonicalizing and wants only this property answered. Anything
// CHOOSING an endpoint wants NormalizeBaseURL instead — this check alone accepts
// userinfo, query strings and hostless values, which is precisely how startup and
// `/backend` came to disagree about the same URL.
func ValidatePlaintextRemote(rawURL string, allowInsecure bool) error {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		// NOT %q of the input: see NormalizeBaseURL's redaction rule. This path used to
		// print the raw value, which is how a password in a URL that failed to parse
		// ended up on the terminal and in the startup log.
		return fmt.Errorf("that is not a usable URL: %s", safeParseReason(err))
	}
	if perr := plaintextRemoteCheck(u, allowInsecure); perr != nil {
		return perr
	}
	return nil
}

// plaintextRemoteCheck is the rule itself, over an already-parsed URL, so the two
// exported entry points cannot answer it differently. It returns the concrete type
// rather than error so a nil result is an untyped nil at every call site.
func plaintextRemoteCheck(u *url.URL, allowInsecure bool) *PlaintextRemoteError {
	if u.Scheme != "http" || allowInsecure || isLoopbackHost(u.Hostname()) {
		return nil
	}
	return &PlaintextRemoteError{Host: hostWithoutZone(u)}
}

// hostWithoutZone renders the refused authority with any IPv6 ZONE ID removed.
//
// Naming the host is the useful half of the refusal, and a hostname is not a secret. A
// zone id is different: it is the arbitrary text after `%` inside the brackets of an
// IPv6 literal, it is not part of the address, and nothing constrains what it contains —
// `http://[fe80::1%25user:pw@x]` parses, and u.Host comes back carrying that text
// verbatim. NormalizeBaseURL rejects the shape before this is reached, but
// ValidatePlaintextRemote is exported and parses on its own, so the redaction rule has
// to hold here rather than at one of the two doors onto it.
func hostWithoutZone(u *url.URL) string {
	host := u.Host
	if !strings.HasPrefix(host, "[") {
		return host
	}
	close := strings.Index(host, "]")
	if close < 0 {
		return host
	}
	inner := host[1:close]
	pct := strings.Index(inner, "%")
	if pct < 0 {
		return host
	}
	return "[" + inner[:pct] + "%<zone>]" + host[close+1:]
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
