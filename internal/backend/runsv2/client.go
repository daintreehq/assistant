package runsv2

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/daintreehq/daintree-assistant/internal/backend"
)

// jsonDeadline bounds the plain JSON endpoints (turn submission returns
// immediately — generation happens server-side and streams via events).
const jsonDeadline = 60 * time.Second

// Client speaks the v2 durable run protocol. It is safe for concurrent use.
// Errors surface as *backend.Error (same envelope + code conventions as v1);
// notable codes: run_busy, turn_conflict, lease_claimed, result_conflict,
// lease_awaiting_approval, lease_terminal, approval_resolved, not_found.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// Config configures a Client. APIKey is optional (local dev is
// unauthenticated). HTTPClient defaults to one with NO global timeout — the
// event stream is long-lived; cancellation is via context.
type Config struct {
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client
}

// NewClient builds a Client. An empty BaseURL falls back to the v1 client's
// backend.DefaultBaseURL so both protocols always target the same server.
func NewClient(cfg Config) *Client {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		base = backend.DefaultBaseURL
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{}
	}
	return &Client{baseURL: base, apiKey: strings.TrimSpace(cfg.APIKey), http: hc}
}

// BaseURL returns the configured backend base URL (diagnostics / doctor).
func (c *Client) BaseURL() string { return c.baseURL }

// ---- runs and turns ---------------------------------------------------------

// CreateRun creates a durable run and submits its first turn.
func (c *Client) CreateRun(ctx context.Context, req CreateRun) (RunCreated, error) {
	var out RunCreated
	err := c.doJSON(ctx, http.MethodPost, "/v2/runs", req, &out)
	return out, err
}

// SubmitTurn submits a turn (idempotent on TurnID; see TurnSubmit).
func (c *Client) SubmitTurn(ctx context.Context, runID string, turn TurnSubmit) (TurnAccepted, error) {
	var out TurnAccepted
	err := c.doJSON(ctx, http.MethodPost, "/v2/runs/"+runID+"/turns", turn, &out)
	return out, err
}

// GetRun fetches the run detail view.
func (c *Client) GetRun(ctx context.Context, runID string) (Run, error) {
	var out struct {
		Run Run `json:"run"`
	}
	err := c.doJSON(ctx, http.MethodGet, "/v2/runs/"+runID, nil, &out)
	return out.Run, err
}

// ListRuns lists runs, most recently updated first.
func (c *Client) ListRuns(ctx context.Context, limit int) ([]Run, error) {
	var out struct {
		Runs []Run `json:"runs"`
	}
	path := "/v2/runs"
	if limit > 0 {
		path += "?limit=" + strconv.Itoa(limit)
	}
	err := c.doJSON(ctx, http.MethodGet, path, nil, &out)
	return out.Runs, err
}

// Cancel cancels the run's in-flight turn. Open leases resolve to synthesized
// cancelled results server-side; the run parks idle.
func (c *Client) Cancel(ctx context.Context, runID, reason string) error {
	body := map[string]string{}
	if reason != "" {
		body["reason"] = reason
	}
	return c.doJSON(ctx, http.MethodPost, "/v2/runs/"+runID+"/cancel", body, nil)
}

// ---- leases -------------------------------------------------------------------

// RunLeases lists a run's leases, optionally filtered by status.
func (c *Client) RunLeases(ctx context.Context, runID, status string) ([]Lease, error) {
	var out struct {
		Leases []Lease `json:"leases"`
	}
	path := "/v2/runs/" + runID + "/leases"
	if status != "" {
		path += "?status=" + status
	}
	err := c.doJSON(ctx, http.MethodGet, path, nil, &out)
	return out.Leases, err
}

// PendingLeases sweeps every claimable lease — the reconnect path: claim what
// this executor can serve, in claim → execute → post-result order.
func (c *Client) PendingLeases(ctx context.Context) ([]Lease, error) {
	var out struct {
		Leases []Lease `json:"leases"`
	}
	err := c.doJSON(ctx, http.MethodGet, "/v2/tool-leases", nil, &out)
	return out.Leases, err
}

// ClaimLease claims a lease for this executor. NEVER execute a tool before a
// successful claim — the claim is the mutual-exclusion fence across executors.
// A 409 lease_claimed means another live executor holds it; lease_terminal
// means a result already landed.
func (c *Client) ClaimLease(ctx context.Context, leaseID, executorID string) (LeaseClaim, error) {
	var out LeaseClaim
	err := c.doJSON(ctx, http.MethodPost, "/v2/tool-leases/"+leaseID+"/claim",
		map[string]string{"executor_id": executorID}, &out)
	return out, err
}

// PostLeaseResult records the lease's exactly-once result. Retrying with an
// identical payload is safe (Outcome "duplicate"); a different payload after a
// result landed is a 409 result_conflict and means a logic bug or a lost fence.
func (c *Client) PostLeaseResult(ctx context.Context, leaseID string, result LeaseResult) (LeaseResultAck, error) {
	var out LeaseResultAck
	err := c.doJSON(ctx, http.MethodPost, "/v2/tool-leases/"+leaseID+"/result", result, &out)
	return out, err
}

// ---- approvals -------------------------------------------------------------------

// ResolveApproval approves or declines a parked background mutation.
func (c *Client) ResolveApproval(ctx context.Context, approvalID string, resolve ApprovalResolve) (Approval, error) {
	var out struct {
		Approval Approval `json:"approval"`
	}
	err := c.doJSON(ctx, http.MethodPost, "/v2/approvals/"+approvalID, resolve, &out)
	return out.Approval, err
}

// ---- executors ---------------------------------------------------------------------

// SendHeartbeat advertises this executor's tool inventory and liveness, and
// returns the pending-lease sweep in the same round trip.
func (c *Client) SendHeartbeat(ctx context.Context, hb Heartbeat) (HeartbeatAck, error) {
	var out HeartbeatAck
	err := c.doJSON(ctx, http.MethodPost, "/v2/executors/heartbeat", hb, &out)
	return out, err
}

// ---- wakeups ------------------------------------------------------------------------

// ScheduleWakeup registers a durable server-side wake for the run.
func (c *Client) ScheduleWakeup(ctx context.Context, runID string, w WakeupCreate) (Wakeup, error) {
	var out struct {
		Wakeup Wakeup `json:"wakeup"`
	}
	err := c.doJSON(ctx, http.MethodPost, "/v2/runs/"+runID+"/wakeups", w, &out)
	return out.Wakeup, err
}

// CancelWakeup cancels a scheduled wake.
func (c *Client) CancelWakeup(ctx context.Context, runID, wakeupID string) error {
	return c.doJSON(ctx, http.MethodDelete, "/v2/runs/"+runID+"/wakeups/"+wakeupID, nil, nil)
}

// ---- replay --------------------------------------------------------------------------

// Replay asks the backend to reproduce and verify the exact prompt of a
// recorded round from its event log.
func (c *Client) Replay(ctx context.Context, runID string, round int, includeRequest bool) (ReplayReport, error) {
	var out ReplayReport
	path := fmt.Sprintf("/v2/runs/%s/rounds/%d/replay", runID, round)
	if includeRequest {
		path += "?include_request=true"
	}
	err := c.doJSON(ctx, http.MethodGet, path, nil, &out)
	return out, err
}

// ---- event stream ----------------------------------------------------------------------

// StreamEvents opens one SSE connection to the run's event log, replaying
// persisted events after the `after` cursor and then tailing live. The handler
// is invoked for every event (persisted and ephemeral); returning an error
// stops the stream and surfaces that error. Returns nil when the server ends
// the stream. The caller owns reconnection — use TailEvents for the
// auto-reconnect loop.
func (c *Client) StreamEvents(ctx context.Context, runID string, after int64, handler func(Event) error) error {
	path := fmt.Sprintf("%s/v2/runs/%s/events?after=%d", c.baseURL, runID, after)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return decodeHTTPError(resp)
	}

	scanner := bufio.NewScanner(resp.Body)
	// Tool-result payloads can be up to ~1 MiB; leave generous headroom.
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)

	var ev Event
	// SSE allows a frame's data to span multiple `data:` lines, joined with
	// newlines at dispatch — accumulate, parse once on the blank-line boundary.
	var dataLines []string
	flush := func() error {
		if len(dataLines) == 0 {
			ev = Event{}
			return nil
		}
		raw := strings.Join(dataLines, "\n")
		var frame struct {
			Seq     int64           `json:"seq"`
			TS      float64         `json:"ts"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal([]byte(raw), &frame); err == nil {
			if frame.Seq > 0 {
				ev.Seq = frame.Seq
			}
			ev.TS = frame.TS
			ev.Payload = frame.Payload
		} else {
			ev.Payload = json.RawMessage(raw)
		}
		out := ev
		ev = Event{}
		dataLines = nil
		return handler(out)
	}
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			if err := flush(); err != nil {
				return err
			}
		case strings.HasPrefix(line, ":"):
			// keepalive comment
		case strings.HasPrefix(line, "id:"):
			if v, err := strconv.ParseInt(strings.TrimSpace(line[3:]), 10, 64); err == nil {
				ev.Seq = v
			}
		case strings.HasPrefix(line, "event:"):
			ev.Type = strings.TrimSpace(line[6:])
		case strings.HasPrefix(line, "data:"):
			dataLines = append(dataLines, strings.TrimSpace(line[5:]))
		}
	}
	if err := flush(); err != nil {
		return err
	}
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		return err
	}
	return ctx.Err()
}

// TailEvents follows a run's event log until ctx is cancelled or the handler
// returns an error, transparently reconnecting with the last seen cursor after
// transport drops (nothing durable is ever lost: persisted events replay from
// the cursor; only advisory deltas are skipped). The stream.reset event is
// handled internally — it just forces a reconnect from the cursor.
//
// The resume cursor only advances after the handler SUCCEEDS on an event, so
// a handler failure never skips the event it failed on; handler errors are
// carried out via a sentinel wrapper (never guessed from error text) and
// terminate the tail verbatim. HTTP-level 4xx errors terminate too (the run is
// gone / auth is broken — reconnecting cannot help); everything else (5xx,
// transport drops) retries with capped exponential backoff.
func (c *Client) TailEvents(ctx context.Context, runID string, after int64, handler func(Event) error) error {
	cursor := after
	backoff := 250 * time.Millisecond
	for {
		streamErr := c.StreamEvents(ctx, runID, cursor, func(ev Event) error {
			if ev.Type == "stream.reset" {
				return errReset
			}
			if err := handler(ev); err != nil {
				return &handlerError{err: err}
			}
			if ev.Seq > cursor {
				cursor = ev.Seq
			}
			backoff = 250 * time.Millisecond // healthy stream: reset the backoff
			return nil
		})
		var hErr *handlerError
		var be *backend.Error
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case errors.As(streamErr, &hErr):
			return hErr.err
		case streamErr == nil || errors.Is(streamErr, errReset):
			// Server ended the stream (or asked for a resume): reconnect now.
		case errors.As(streamErr, &be) && be.HTTPStatus >= 400 && be.HTTPStatus < 500:
			return streamErr
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 8*time.Second {
			backoff *= 2
		}
	}
}

var errReset = fmt.Errorf("runsv2: stream reset requested")

// handlerError marks an error as caller-originated so the tail loop can tell
// it apart from transport failures without inspecting error text.
type handlerError struct{ err error }

func (e *handlerError) Error() string { return e.err.Error() }
func (e *handlerError) Unwrap() error { return e.err }

// ---- plumbing -------------------------------------------------------------------------

func (c *Client) doJSON(ctx context.Context, method, path string, body, out any) error {
	// Default deadline only when the caller did not set one — a caller-supplied
	// longer deadline (e.g. a slow replay reconstruction) must be honored.
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, jsonDeadline)
		defer cancel()
	}

	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return decodeHTTPError(resp)
	}
	if out == nil {
		io.Copy(io.Discard, resp.Body) //nolint:errcheck
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// decodeHTTPError maps a non-2xx response onto *backend.Error (v1 and v2 share
// the error envelope). Non-JSON bodies (proxy HTML, empty responses) degrade
// to a stable http_error code with a bounded message — never a huge code-less
// blob.
func decodeHTTPError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var env backend.Envelope
	if err := json.Unmarshal(raw, &env); err != nil || env.Error.Message == "" && env.Error.Code == "" {
		message := strings.TrimSpace(string(raw))
		if len(message) > 2048 {
			message = message[:2048]
		}
		if message == "" {
			message = "request failed (empty error body)"
		}
		return &backend.Error{
			HTTPStatus: resp.StatusCode,
			Type:       "api_error",
			Code:       "http_error",
			Message:    message,
		}
	}
	return &backend.Error{
		HTTPStatus: resp.StatusCode,
		Type:       env.Error.Type,
		Code:       env.Error.Code,
		Message:    env.Error.Message,
		Param:      env.Error.Param,
	}
}
