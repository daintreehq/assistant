package ipc

import "encoding/json"

// ProtocolVersion versions the daemon control protocol. Separate from
// host.ProtocolVersion (that one is byte-coupled to Daintree's embedded-host
// peer); this one only has to agree between two builds of this binary, so a
// bump is cheap and mismatches simply tell the client to restart the daemon.
const ProtocolVersion = 1

// maxFrameBytes caps one NDJSON control frame in either direction. Control
// payloads are tiny (status snapshots, credential strings); anything bigger is
// a protocol violation, not a legitimate message.
const maxFrameBytes = 1 << 20 // 1 MiB

// Request types. The vocabulary is deliberately small: the socket carries
// coordination and introspection, never conversation traffic — the attached
// process owns the DB directly while it holds the owner lock.
const (
	// ReqStatus returns a StatusReply snapshot. Never mutates.
	ReqStatus = "status"
	// ReqAttach asks the daemon to release the owner lock so the caller can
	// take it. The daemon suspends supervision (finishing any in-flight wake
	// turn first) and stays suspended until THIS CONNECTION CLOSES — the open
	// attach connection is the client's lease on the daemon's restraint, so a
	// crashed attached session (connection drops) resumes the daemon automatically.
	ReqAttach = "attach"
	// ReqCredentials refreshes the daemon's MCP credentials (and optionally the
	// backend URL) so a later detached run uses the newest token the last
	// attached session saw. Carried separately from attach so an attached session can push a
	// mid-session token refresh without re-attaching.
	ReqCredentials = "credentials"
	// ReqShutdown asks the daemon process to exit cleanly.
	ReqShutdown = "shutdown"
	// ReqAuthChanged tells the daemon the account credential changed, carrying only the
	// new revision marker — NEVER a token.
	//
	// Adding it did NOT bump ProtocolVersion, deliberately. The server rejects any
	// version mismatch outright, so a bump would strand a freshly-upgraded CLI behind a
	// still-running old daemon: attach fails, `daemon stop` fails, and there is no
	// supported way out. Since this request is best-effort — an old daemon answers
	// "unknown request type" and stops on its own at the next marker poll — the cost of
	// bumping is real and the benefit is nil.
	//
	// It is an optimisation, not the mechanism: the daemon polls the shared marker before
	// every wake anyway, so a daemon that was unreachable at logout still stops on its
	// own. This just removes the delay for the daemon that IS reachable, which is the
	// common case and the one where a user watching their terminal expects the effect to
	// be immediate.
	ReqAuthChanged = "auth_changed"
)

// Request is one inbound control frame.
type Request struct {
	V       int             `json:"v"`
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Response is one outbound control frame, correlated by Request.ID.
type Response struct {
	V       int             `json:"v"`
	ID      string          `json:"id"`
	OK      bool            `json:"ok"`
	Error   string          `json:"error,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// DaemonState is the supervision lifecycle the daemon reports over ReqStatus.
type DaemonState string

const (
	// StateSupervising: the daemon holds the owner lock and its engines run.
	StateSupervising DaemonState = "supervising"
	// StateStandby: the daemon wants the owner lock but another process (an
	// attached session) holds it.
	StateStandby DaemonState = "standby"
	// StateYielded: an attach connection is open; the daemon will not contend
	// for the owner lock until that connection closes.
	StateYielded DaemonState = "yielded"
	// StateStarting: process is up but the runtime loop hasn't settled yet.
	StateStarting DaemonState = "starting"
)

// StatusReply is the ReqStatus payload.
type StatusReply struct {
	State           DaemonState `json:"state"`
	Pid             int         `json:"pid"`
	Version         string      `json:"version"`
	StartedAtMs     int64       `json:"startedAtMs"`
	ProjectID       string      `json:"projectId,omitempty"`
	StateDir        string      `json:"stateDir"`
	DBPath          string      `json:"dbPath"`
	McpConfigured   bool        `json:"mcpConfigured"`
	McpConnected    bool        `json:"mcpConnected"`
	McpBlocked      bool        `json:"mcpBlocked"`
	BackendHealthy  bool        `json:"backendHealthy"`
	LiveWatchers    int         `json:"liveWatchers"`
	LiveAsync       int         `json:"liveAsync"`
	ScheduledTimers int         `json:"scheduledTimers"`
	OpenAttention   int         `json:"openAttention"`
	PendingApproval int         `json:"pendingApproval"`
	WakeTurnsRun    int         `json:"wakeTurnsRun"`
	// AuthState is the daemon's view of the account, so `status` can explain a daemon
	// that is alive and deliberately doing nothing.
	AuthState string `json:"authState,omitempty"`
	// AuthRequired reports that unattended work is PAUSED pending a sign-in. Distinct
	// from a plain state string because it is the one bit a caller acts on.
	AuthRequired bool `json:"authRequired"`
	// AuthRevision is the observed marker, for correlating what this daemon believes
	// with what a terminal believes. Non-secret by construction.
	AuthRevision string `json:"authRevision,omitempty"`
	LastWakeAtMs int64  `json:"lastWakeAtMs,omitempty"`
	LastError    string `json:"lastError,omitempty"`
}

// AttachRequest is the ReqAttach payload. Credentials ride along so every
// attach doubles as a freshness push (the attached session always has the newest
// Daintree-injected MCP token).
type AttachRequest struct {
	ClientPid   int          `json:"clientPid"`
	Credentials *Credentials `json:"credentials,omitempty"`
}

// AttachReply acknowledges that the daemon released (or never held) the owner
// lock and will stand down until the connection closes.
type AttachReply struct {
	// OwnerReleased is true when the daemon held the lock and released it; false
	// when it wasn't holding it (some OTHER process owns the DB — most likely a
	// second attached session).
	OwnerReleased bool `json:"ownerReleased"`
	// OwnerBusy is set when the daemon believes a non-daemon process holds the
	// owner lock, with the stamped pid when known.
	OwnerBusy    bool `json:"ownerBusy"`
	OwnerPid     int  `json:"ownerPid,omitempty"`
	WakeInFlight bool `json:"wakeInFlight"`
}

// Credentials is the ReqCredentials payload (also embedded in AttachRequest).
// Empty fields mean "leave unchanged".
//
// ACCOUNT TOKENS DO NOT BELONG HERE, and no field for one may be added. Every process
// can already reach the same OS credential store, so copying a rotating one-time-use
// refresh token into a project-scoped daemon's memory buys nothing and costs a great
// deal: it puts a live secret into an IPC payload, into whatever logs that path touches,
// and into a second place that can go stale. The daemon learns about credential changes
// from the non-secret revision marker instead — see AuthChangedRequest.
type Credentials struct {
	McpURL     string `json:"mcpUrl,omitempty"`
	McpToken   string `json:"mcpToken,omitempty"`
	BackendURL string `json:"backendUrl,omitempty"`
}

// AuthChangedRequest is the ReqAuthChanged payload. It carries the new revision marker
// and nothing else — the marker is a nonce and a counter, never a credential.
type AuthChangedRequest struct {
	Revision string `json:"revision"`
}
