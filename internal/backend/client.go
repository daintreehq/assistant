package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Backend is the full client surface the app depends on (satisfied by *Client, and
// trivially by a fake in tests). Holding the app's dependency as this interface lets
// tests inject a fake backend without a live server.
type Backend interface {
	RespondStream(ctx context.Context, req RespondRequest, cb StreamCallbacks) (RespondResult, error)
	RunTask(ctx context.Context, req TaskRequest) (TaskResult, error)
	Capabilities(ctx context.Context) (Capabilities, error)
	Version(ctx context.Context) (Version, error)
	Health(ctx context.Context) error
	Ready(ctx context.Context) error
	BaseURL() string
}

// Client is the native Daintree backend HTTP client. It speaks the Daintree-native
// protocol (NOT OpenAI), streams the respond endpoint as named SSE events, and
// runs server-owned utility tasks. It is safe for concurrent use.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
	info    ClientInfo
}

// ClientConfig configures a Client. APIKey is OPTIONAL — local development runs
// the backend unauthenticated (no DAINTREE_API_KEY), so an empty key sends no
// Authorization header. HTTPClient defaults to one with NO global timeout (a
// streamed turn can run for minutes; cancellation is via context).
type ClientConfig struct {
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client
	ClientInfo ClientInfo
}

// NewClient builds a Client. An empty BaseURL falls back to DefaultBaseURL.
func NewClient(cfg ClientConfig) *Client {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		base = DefaultBaseURL
	}
	hc := cfg.HTTPClient
	if hc == nil {
		// No client-wide timeout: the respond stream can legitimately run for
		// minutes. Per-call deadlines are imposed with context where appropriate.
		hc = &http.Client{}
	}
	return &Client{baseURL: base, apiKey: strings.TrimSpace(cfg.APIKey), http: hc, info: cfg.ClientInfo}
}

// BaseURL returns the configured backend base URL (for diagnostics / doctor).
func (c *Client) BaseURL() string { return c.baseURL }

// setHeaders applies the common headers. The Authorization header is only set
// when an API key is configured (local dev runs without one).
func (c *Client) setHeaders(req *http.Request, accept string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", accept)
	req.Header.Set("X-Daintree-Protocol", fmt.Sprintf("%d", ProtocolVersion))
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
}

// RespondStream runs one generation round against /v1/daintree/respond as a
// named-event SSE stream and returns the accumulated result. It forces
// generation.stream = true. A pre-stream failure (the backend prefetches the first
// upstream token before committing the 200) arrives as an ordinary JSON error; a
// failure after the meta event arrives as a terminal SSE error event — both surface
// as *Error. The caller owns cancellation via ctx.
func (c *Client) RespondStream(ctx context.Context, req RespondRequest, cb StreamCallbacks) (RespondResult, error) {
	if req.ProtocolVersion == 0 {
		req.ProtocolVersion = ProtocolVersion
	}
	if req.Generation == nil {
		req.Generation = &Generation{}
	}
	req.Generation.Stream = true
	if req.Client == nil && c.info != (ClientInfo{}) {
		info := c.info
		req.Client = &info
	}

	body, err := json.Marshal(req)
	if err != nil {
		return RespondResult{}, fmt.Errorf("backend: marshal respond request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/daintree/respond", bytes.NewReader(body))
	if err != nil {
		return RespondResult{}, fmt.Errorf("backend: build respond request: %w", err)
	}
	c.setHeaders(httpReq, "text/event-stream")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return RespondResult{}, &Error{Code: "connect", Message: "could not reach assistant backend: " + err.Error()}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return RespondResult{}, c.readErrorResponse(resp)
	}
	return parseRespondStream(resp.Body, cb)
}

// Respond runs a non-streaming generation round (used for tests / simple callers).
func (c *Client) Respond(ctx context.Context, req RespondRequest) (RespondResponse, error) {
	if req.ProtocolVersion == 0 {
		req.ProtocolVersion = ProtocolVersion
	}
	if req.Generation == nil {
		req.Generation = &Generation{}
	}
	req.Generation.Stream = false
	if req.Client == nil && c.info != (ClientInfo{}) {
		info := c.info
		req.Client = &info
	}
	var out RespondResponse
	if err := c.doJSON(ctx, http.MethodPost, "/v1/daintree/respond", req, &out); err != nil {
		return RespondResponse{}, err
	}
	return out, nil
}

// RunTask runs a server-owned utility task against /v1/daintree/tasks. The CLI
// sends task DATA only; the backend owns the prompt, model, schema, and output
// mode. Decode TaskResult.Output into the task-specific output struct.
func (c *Client) RunTask(ctx context.Context, req TaskRequest) (TaskResult, error) {
	var out TaskResult
	if err := c.doJSON(ctx, http.MethodPost, "/v1/daintree/tasks", req, &out); err != nil {
		return TaskResult{}, err
	}
	return out, nil
}

// Capabilities fetches the backend's capability descriptor. Cache the result —
// refresh only on startup, reconnect, or /doctor.
func (c *Client) Capabilities(ctx context.Context) (Capabilities, error) {
	var out Capabilities
	if err := c.doJSON(ctx, http.MethodGet, "/v1/daintree/capabilities", nil, &out); err != nil {
		return Capabilities{}, err
	}
	return out, nil
}

// Version fetches the unauthenticated /version descriptor.
func (c *Client) Version(ctx context.Context) (Version, error) {
	var out Version
	if err := c.doJSON(ctx, http.MethodGet, "/version", nil, &out); err != nil {
		return Version{}, err
	}
	return out, nil
}

// Health probes /healthz (liveness). Returns nil when the backend reports ok.
func (c *Client) Health(ctx context.Context) error {
	var out struct {
		Status string `json:"status"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/healthz", nil, &out); err != nil {
		return err
	}
	if out.Status != "ok" {
		return &Error{Code: "not_healthy", Message: "backend health: " + out.Status}
	}
	return nil
}

// Ready probes /readyz (readiness: config, secrets, prompts, catalog, provider).
// Returns nil only when the backend reports ready (a 503 surfaces as *Error).
func (c *Client) Ready(ctx context.Context) error {
	var out struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/readyz", nil, &out); err != nil {
		return err
	}
	if out.Status != "ready" {
		msg := "backend not ready"
		if out.Error != "" {
			msg += ": " + out.Error
		}
		return &Error{HTTPStatus: 503, Code: "not_ready", Message: msg}
	}
	return nil
}

// doJSON performs a JSON request/response with a default short deadline for the
// non-streaming endpoints. A nil body sends no payload (GET); a non-2xx decodes the
// error envelope. out may be nil to discard the body.
func (c *Client) doJSON(ctx context.Context, method, path string, body any, out any) error {
	// Bound the non-streaming calls so a wedged backend can't hang a turn; the
	// streamed respond path does NOT route through here.
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
	}

	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("backend: marshal %s: %w", path, err)
		}
		reader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("backend: build %s request: %w", path, err)
	}
	c.setHeaders(req, "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return &Error{Code: "connect", Message: "could not reach assistant backend: " + err.Error()}
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return c.readErrorResponse(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(out); err != nil {
		return &Error{HTTPStatus: resp.StatusCode, Code: "decode", Message: "could not decode backend response: " + err.Error()}
	}
	return nil
}

// readErrorResponse decodes a non-2xx response into an *Error, preferring the
// stable Daintree error envelope and falling back to the raw body.
func (c *Client) readErrorResponse(resp *http.Response) *Error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
	var env Envelope
	if err := json.Unmarshal(raw, &env); err == nil && (env.Error.Code != "" || env.Error.Message != "" || env.Error.Type != "") {
		return newError(resp.StatusCode, env, retryAfter, false)
	}
	e := httpError(resp.StatusCode, string(raw))
	e.RetryAfter = retryAfter
	return e
}
