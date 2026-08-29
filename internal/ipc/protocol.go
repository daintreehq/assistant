package ipc

import (
	"encoding/json"

	"github.com/daintreehq/assistant/internal/timers"
)

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
	// ReqTimers lists the project's scheduled timers and what recently-fired ones
	// did. Never mutates.
	//
	// It is on this socket because a timer OUTLIVES the assistant: once the panel is
	// gone the daemon is the only process holding the project lease, and it is the
	// only thing that can answer "what is still going to happen". The alternative —
	// a caller opening state.db behind the lock-holder's back — duplicates the schema
	// across a process boundary and races the writes the daemon is making.
	ReqTimers = "timers"
	// ReqTimerCancel retires one timer on a human's behalf, revoking the automation
	// grants scoped to it. The one MUTATION on this socket, and it is here for the
	// same reason: the daemon owns the store, so it has to be the one to write.
	//
	// Neither request bumps ProtocolVersion, for the reason spelled out on
	// ReqAuthChanged: the server rejects a version mismatch outright, so a bump would
	// strand an upgraded CLI behind a still-running old daemon with no way out. An
	// old daemon answers "unknown request type", which a caller reports as the
	// feature being unavailable — the honest answer, and a recoverable one.
	ReqTimerCancel = "timer_cancel"
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

// TimersReply is the ReqTimers payload: the same rows the embedded host serves, so
// a caller that talks to both cannot be told two different things.
type TimersReply struct {
	Timers   []timers.View  `json:"timers"`
	Outcomes []TimerOutcome `json:"outcomes"`
	TakenAt  int64          `json:"takenAtMs"`
}

// TimerOutcome is what one fired timer did, read off the attention queue.
type TimerOutcome struct {
	EventID   string `json:"eventId"`
	TimerID   string `json:"timerId"`
	Severity  string `json:"severity"`
	Title     string `json:"title"`
	Summary   string `json:"summary"`
	CreatedAt int64  `json:"createdAtMs"`
	UpdatedAt int64  `json:"updatedAtMs"`
	Count     int    `json:"count"`
}

// TimerCancelRequest is the ReqTimerCancel payload.
type TimerCancelRequest struct {
	TimerID string `json:"timerId"`
}

// TimerCancelReply reports what the cancel actually did. Mirrors the embedded host's
// outcome field for field, because a caller should not have to care which transport
// answered it.
type TimerCancelReply struct {
	TimerID           string `json:"timerId"`
	Cancelled         bool   `json:"cancelled"`
	AlreadyInactive   bool   `json:"alreadyInactive"`
	PriorStatus       string `json:"priorStatus"`
	RevokedGrants     int    `json:"revokedGrants"`
	GrantRevokeFailed bool   `json:"grantRevokeFailed"`
	// Contended means the scheduler fired it out from under the cancel and it is
	// live again — nothing was retired, and the honest answer is "try again".
	Contended bool `json:"contended"`
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
