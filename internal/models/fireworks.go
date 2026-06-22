// Fireworks AI client (OpenAI-compatible), hand-rolled on stdlib net/http.
//
// Provides three call shapes:
//   - Chat()       non-streaming completion (text + tool calls)
//   - ChatStream() streaming completion with a token callback
//   - JSON()       strict json_object completion (caller parses+validates)
//
// We own ALL retry logic (no SDK retries) — bounded backoff, per-attempt timeouts
// via context, and a pre-token-only streaming retry. Reasoning models can emit
// <think>…</think> in delta.content; ThinkFilter keeps that out of visible output.
package models

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// FireworksConfig is the subset of the resolved app config the client needs. A
// consumer-defined narrow view so the package compiles without importing config.
type FireworksConfig struct {
	BaseURL string
	APIKey  string
	Offline bool
}

// Usage is the per-call token accounting, with pointer fields mirroring the
// optional wire fields (a missing count is nil, not a misleading 0).
type Usage struct {
	PromptTokens     *int
	CompletionTokens *int
	TotalTokens      *int
	// CachedTokens is the cached-prompt subset (billed at a discount), when the
	// provider reports it under prompt_tokens_details.cached_tokens.
	CachedTokens *int
}

// ChatOptions is the input to a model call. The turn's cancellation is carried by
// the context.Context passed to each method (the AbortSignal equivalent).
type ChatOptions struct {
	Model       string
	Messages    []ChatMessage
	Tools       []ChatTool
	ToolChoice  string // "auto" | "none" | "required" | "" (omit)
	Temperature *float64
	MaxTokens   *int
	// PromptCacheKey caches a static system-prompt prefix on the Fireworks side.
	// Sent ONLY on chat/chatStream (never on json) and only when non-empty.
	PromptCacheKey string
}

// ChatResult is the normalized output of a model call.
type ChatResult struct {
	Content      string
	Reasoning    string
	ToolCalls    []ToolCallRequest
	FinishReason string
	Usage        *Usage
}

// FireworksClient speaks the OpenAI Chat Completions wire format to Fireworks.
type FireworksClient struct {
	cfg  FireworksConfig
	http *http.Client
}

// NewFireworksClient builds a client. The HTTP client has NO global timeout — each
// attempt derives its own context.WithTimeout so a long stream isn't capped by a
// short request budget.
func NewFireworksClient(cfg FireworksConfig) *FireworksClient {
	return &FireworksClient{cfg: cfg, http: &http.Client{}}
}

// guard runs at the top of every call: reject offline mode and a missing API key
// before any wire work.
func (c *FireworksClient) guard() error {
	if c.cfg.Offline {
		return &FireworksUnavailableError{Message: "offline mode"}
	}
	if c.cfg.APIKey == "" {
		return &FireworksUnavailableError{Message: "FIREWORKS_API_KEY not set"}
	}
	return nil
}

// chatRequestBody is the request payload. Pointer/omitempty fields honour the
// omit-when-undefined rule: an absent optional is omitted, never sent as null.
type chatRequestBody struct {
	Model          string          `json:"model"`
	Messages       []wireMessage   `json:"messages"`
	Tools          []ChatTool      `json:"tools,omitempty"`
	ToolChoice     string          `json:"tool_choice,omitempty"`
	Temperature    float64         `json:"temperature"`
	MaxTokens      *int            `json:"max_tokens,omitempty"`
	PromptCacheKey string          `json:"prompt_cache_key,omitempty"`
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
	Stream         bool            `json:"stream,omitempty"`
	StreamOptions  *streamOptions  `json:"stream_options,omitempty"`
}

type responseFormat struct {
	Type string `json:"type"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// doRequest performs one HTTP POST attempt against /chat/completions with the given
// per-attempt timeout, returning the response for the caller to read. The caller
// MUST close the body. A non-2xx status returns an *apiError (with headers, for
// Retry-After). A context cancellation surfaces as context.Canceled.
func (c *FireworksClient) doRequest(ctx context.Context, body []byte, timeoutMs int) (*http.Response, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutMs)*time.Millisecond)
	// NB: the caller owns body-close; we cannot cancel here or we'd kill the stream.
	// For non-stream the caller cancels via the returned closer; for safety on the
	// error path we cancel before returning the error.
	req, err := http.NewRequestWithContext(attemptCtx, http.MethodPost,
		strings.TrimRight(c.cfg.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	req.Header.Set("Accept", "text/event-stream, application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		cancel()
		// Prefer the turn's cancellation cause over a wrapped transport error so an
		// abort is classified as such.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		resp.Body.Close()
		cancel()
		return nil, &apiError{status: resp.StatusCode, headers: resp.Header, body: string(b)}
	}
	// Success: hand the live body to the caller. Wrap the body so its Close also
	// releases the per-attempt context (otherwise the WithTimeout leaks).
	resp.Body = &ctxClosingBody{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

const maxErrorBodyBytes = 64 * 1024

// ctxClosingBody cancels the per-attempt context when the response body is closed,
// so the deadline goroutine doesn't leak.
type ctxClosingBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *ctxClosingBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

// buildBody assembles the request payload for chat/stream/json. mode selects the
// path-specific fields (default temperature, stream flags, response_format,
// prompt_cache_key inclusion).
func (c *FireworksClient) buildBody(opts ChatOptions, stream, jsonMode bool) ([]byte, error) {
	wm, err := toWireMessages(opts.Messages)
	if err != nil {
		return nil, err
	}
	body := chatRequestBody{
		Model:     opts.Model,
		Messages:  wm,
		MaxTokens: opts.MaxTokens,
	}
	if jsonMode {
		// json path: NO tools/tool_choice, default temperature 0, response_format,
		// and NO prompt_cache_key.
		body.Temperature = 0
		if opts.Temperature != nil {
			body.Temperature = *opts.Temperature
		}
		body.ResponseFormat = &responseFormat{Type: "json_object"}
	} else {
		// Normalize parameterless tools so a nil/empty Parameters never marshals to
		// the wire `null` Fireworks rejects (→ empty-object schema instead).
		body.Tools = normalizeTools(opts.Tools)
		body.ToolChoice = opts.ToolChoice
		body.Temperature = 0.3
		if opts.Temperature != nil {
			body.Temperature = *opts.Temperature
		}
		if opts.PromptCacheKey != "" {
			body.PromptCacheKey = opts.PromptCacheKey
		}
		if stream {
			body.Stream = true
			body.StreamOptions = &streamOptions{IncludeUsage: true}
		}
	}
	return json.Marshal(body)
}

// normalizeAbort maps a context cancellation to CancelledError so the raw
// transport/abort error never leaks. Returns the original error otherwise.
func normalizeAbort(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || ctx.Err() == context.Canceled {
		return newCancelled()
	}
	return err
}

// Chat runs a non-streaming completion with bounded retry + per-attempt timeout.
func (c *FireworksClient) Chat(ctx context.Context, opts ChatOptions) (ChatResult, error) {
	if err := c.guard(); err != nil {
		return ChatResult{}, err
	}
	body, err := c.buildBody(opts, false, false)
	if err != nil {
		return ChatResult{}, err
	}
	resp, err := retryModelCall(ctx, ModelRetryPolicy, func() (*chatResponse, error) {
		httpResp, herr := c.doRequest(ctx, body, ModelRequestTimeoutMs)
		if herr != nil {
			return nil, herr
		}
		defer httpResp.Body.Close()
		var parsed chatResponse
		if derr := json.NewDecoder(httpResp.Body).Decode(&parsed); derr != nil {
			return nil, derr
		}
		return &parsed, nil
	})
	if err != nil {
		return ChatResult{}, normalizeAbort(ctx, err)
	}
	if len(resp.Choices) == 0 {
		return ChatResult{}, errors.New("fireworks: empty choices in response")
	}
	choice := resp.Choices[0]
	filter := &ThinkFilter{}
	content := ""
	if choice.Message.Content != nil {
		content = *choice.Message.Content
	}
	filter.Push(content)
	filter.End()
	return ChatResult{
		Content:      strings.TrimSpace(filter.Visible()),
		Reasoning:    strings.TrimSpace(filter.Reasoning()),
		ToolCalls:    normalizeToolCalls(choice.Message.ToolCalls),
		FinishReason: finishOrStop(choice.FinishReason),
		Usage:        resp.Usage.toUsage(),
	}, nil
}

// JSON runs a strict json_object completion. The caller (the Router) strips think,
// extracts the balanced JSON span, and parses/validates — here we just return the
// raw model content (already think-stripped + extracted) for the caller to decode.
// Returns the cleaned JSON string AND the parsed token usage, so the Router can
// meter json-path spend the same way it meters Chat/Stream (usage is nil when the
// provider reported none).
func (c *FireworksClient) JSON(ctx context.Context, opts ChatOptions) (string, *Usage, error) {
	if err := c.guard(); err != nil {
		return "", nil, err
	}
	body, err := c.buildBody(opts, false, true)
	if err != nil {
		return "", nil, err
	}
	resp, err := retryModelCall(ctx, ModelRetryPolicy, func() (*chatResponse, error) {
		httpResp, herr := c.doRequest(ctx, body, ModelRequestTimeoutMs)
		if herr != nil {
			return nil, herr
		}
		defer httpResp.Body.Close()
		var parsed chatResponse
		if derr := json.NewDecoder(httpResp.Body).Decode(&parsed); derr != nil {
			return nil, derr
		}
		return &parsed, nil
	})
	if err != nil {
		return "", nil, normalizeAbort(ctx, err)
	}
	// A protocol failure must surface as a real error, not be papered over with an
	// empty object: an empty-choices response (mirror Chat()'s handling) or empty
	// content in json_object mode would otherwise hand the caller a silent "{}" that
	// validates to a misleading empty result. Let the caller see the failure.
	if len(resp.Choices) == 0 {
		return "", nil, errors.New("fireworks: empty choices in response")
	}
	content := resp.Choices[0].Message.Content
	if content == nil || *content == "" {
		return "", nil, errors.New("fireworks: empty content in json_object response")
	}
	cleaned := stripThink(*content)
	return ExtractJson(cleaned), resp.Usage.toUsage(), nil
}

// ChatStream runs a streaming completion. onToken (may be nil) is called with each
// newly-visible chunk. Retry is PRE-TOKEN ONLY: once any visible token has reached
// the caller, a later failure propagates unchanged (retrying would duplicate output
// into the immutable transcript).
func (c *FireworksClient) ChatStream(ctx context.Context, opts ChatOptions, onToken func(string)) (ChatResult, error) {
	if err := c.guard(); err != nil {
		return ChatResult{}, err
	}
	body, err := c.buildBody(opts, true, false)
	if err != nil {
		return ChatResult{}, err
	}

	emitted := false
	for attempt := 0; ; attempt++ {
		// Fresh accumulators per attempt — a retry restarts the stream from scratch.
		filter := &ThinkFilter{}
		toolAcc := map[int]*toolAccEntry{}
		finishReason := "stop"
		var usage *Usage
		// capErr records a per-tool-argument ceiling breach detected inside the SSE
		// callback (which can't return an error); checked after parseSSE returns.
		var capErr error

		res, streamErr := func() (ChatResult, error) {
			httpResp, herr := c.doRequest(ctx, body, ModelStreamTimeoutMs)
			if herr != nil {
				return ChatResult{}, herr
			}
			defer httpResp.Body.Close()

			perr := parseSSE(httpResp.Body, func(chunk *streamChunk) {
				// Usage may ride the final usage-only chunk (empty choices) or, on
				// some providers, the final content chunk — capture whenever present.
				if chunk.Usage != nil {
					usage = chunk.Usage.toUsage()
				}
				if len(chunk.Choices) == 0 {
					return
				}
				choice := chunk.Choices[0]
				if choice.Delta.Content != nil && *choice.Delta.Content != "" {
					visible := filter.Push(*choice.Delta.Content)
					if visible != "" {
						emitted = true
						if onToken != nil {
							onToken(visible)
						}
					}
				}
				for _, tc := range choice.Delta.ToolCalls {
					idx := 0
					if tc.Index != nil {
						idx = *tc.Index
					}
					cur := toolAcc[idx]
					if cur == nil {
						cur = &toolAccEntry{}
						toolAcc[idx] = cur
					}
					if tc.ID != "" {
						cur.id = tc.ID
					}
					if tc.Function != nil {
						if tc.Function.Name != "" {
							cur.name = tc.Function.Name
						}
						if tc.Function.Arguments != "" {
							if len(cur.args)+len(tc.Function.Arguments) > maxToolArgBytes {
								if capErr == nil {
									capErr = errors.New("fireworks: tool-call arguments exceeded byte ceiling")
								}
								continue
							}
							cur.args += tc.Function.Arguments
						}
					}
				}
				if choice.FinishReason != nil && *choice.FinishReason != "" {
					finishReason = *choice.FinishReason
				}
			})
			if perr != nil {
				return ChatResult{}, perr
			}
			if capErr != nil {
				return ChatResult{}, capErr
			}
			tail := filter.End()
			if tail != "" {
				emitted = true
				if onToken != nil {
					onToken(tail)
				}
			}
			return ChatResult{
				Content:      strings.TrimSpace(filter.Visible()),
				Reasoning:    strings.TrimSpace(filter.Reasoning()),
				ToolCalls:    buildStreamToolCalls(toolAcc),
				FinishReason: finishReason,
				Usage:        usage,
			}, nil
		}()

		if streamErr == nil {
			return res, nil
		}

		// A user-initiated abort (or a cancel that raced a transient error) is a
		// clean stop, not a model failure.
		if errors.Is(streamErr, context.Canceled) || ctx.Err() == context.Canceled {
			return ChatResult{}, newCancelled()
		}
		// Retriable ONLY before the first emitted token; otherwise honour the budget.
		if emitted || attempt >= ModelRetryPolicy.MaxRetries || !isRetriableModelError(streamErr) {
			// A budget-exhausted 429 is wrapped into the exported RateLimitedError so it
			// can cross into the agent's classifyStreamError. The emitted path returns
			// raw — a partially-streamed turn must NOT be relabelled as a clean
			// rate-limit (a mid-stream read error is not a 429 anyway).
			if !emitted {
				return ChatResult{}, wrapExhaustedRateLimit(streamErr)
			}
			return ChatResult{}, streamErr
		}
		if serr := abortableSleep(ctx, modelRetryDelayMs(attempt, streamErr, ModelRetryPolicy)); serr != nil {
			// Cancelled mid-backoff → clean cancellation.
			return ChatResult{}, newCancelled()
		}
	}
}

type toolAccEntry struct {
	id, name, args string
}

// buildStreamToolCalls finalizes the accumulated streamed tool calls: sorted by
// index ascending, synthesizing a stable id from hashString(name+args) when none
// was sent, defaulting args to "{}", and dropping any with an empty name.
func buildStreamToolCalls(acc map[int]*toolAccEntry) []ToolCallRequest {
	idxs := make([]int, 0, len(acc))
	for k := range acc {
		idxs = append(idxs, k)
	}
	sort.Ints(idxs)
	out := make([]ToolCallRequest, 0, len(idxs))
	for _, k := range idxs {
		v := acc[k]
		id := v.id
		if id == "" {
			id = "call_" + strconv.Itoa(absInt(hashString(v.name+v.args)))
		}
		args := v.args
		if args == "" {
			args = "{}"
		}
		if v.name == "" {
			continue
		}
		out = append(out, ToolCallRequest{
			ID: id, Type: "function",
			Function: ToolCallFunction{Name: v.name, Arguments: args},
		})
	}
	return out
}

func finishOrStop(fr *string) string {
	if fr == nil || *fr == "" {
		return "stop"
	}
	return *fr
}

// parseSSE hand-parses a Fireworks SSE stream: lines starting `data: ` carry a
// JSON chunk; `data: [DONE]` ends the stream. Tolerant of CRLF and of blank-line
// event separators. The caller closes the body.
//
// Reaching EOF WITHOUT the `[DONE]` terminator is a TRUNCATED stream, not a
// complete one — returning success there would fabricate a clean "stop" out of a
// partial response (missing tail content + usage). We surface it as an error so
// the caller's pre-token retry / post-token failure path applies. Likewise a
// malformed `data:` JSON payload is a protocol failure, not a chunk to silently
// drop — a dropped content/usage delta would corrupt the result. We do NOT error
// on the normal `[DONE]` terminator, on comment/empty separator lines, or on a
// `data:` whose JSON parses (unknown fields are ignored by encoding/json).
//
// A total stream-byte ceiling caps cumulative payload bytes over the (300s) window
// so a runaway stream can't grow the visible/reasoning/tool buffers unbounded.
func parseSSE(r io.Reader, onChunk func(*streamChunk)) error {
	sc := bufio.NewScanner(r)
	// SSE chunks (esp. tool-call argument deltas / large content) can exceed the
	// 64KB default token size — raise the cap generously.
	sc.Buffer(make([]byte, 0, 64*1024), maxSSELineBytes)
	var totalBytes int64
	sawDone := false
	for sc.Scan() {
		line := sc.Bytes()
		// CRLF tolerance: Scanner strips the trailing \n but a \r can remain.
		line = bytes.TrimRight(line, "\r")
		if len(line) == 0 {
			continue // event separator
		}
		if !bytes.HasPrefix(line, dataPrefix) {
			continue // comments (": ...") and non-data fields are ignored
		}
		payload := bytes.TrimSpace(line[len(dataPrefix):])
		if bytes.Equal(payload, doneMarker) {
			sawDone = true
			return nil
		}
		totalBytes += int64(len(payload))
		if totalBytes > maxStreamTotalBytes {
			return errors.New("fireworks: stream exceeded total byte ceiling")
		}
		var chunk streamChunk
		if err := json.Unmarshal(payload, &chunk); err != nil {
			// A malformed chunk is a protocol failure — surfacing it lets the
			// pre-token-retry / post-token-failure semantics apply instead of
			// fabricating a complete result from a corrupted stream. Wrap
			// io.ErrUnexpectedEOF so the retry classifier treats a corrupted/
			// truncated payload as the transient read failure it is.
			return fmt.Errorf("fireworks: malformed SSE data payload: %w (%v)", io.ErrUnexpectedEOF, err)
		}
		// A provider error delivered mid-stream (after HTTP 200) arrives as
		// `data: {"error": {...}}` with no choices. Surface it as a real failure instead
		// of letting the empty-choices path silently fabricate a clean, empty completion.
		if chunk.Error != nil {
			msg := chunk.Error.Message
			if msg == "" {
				msg = chunk.Error.Code
			}
			if msg == "" {
				msg = chunk.Error.Type
			}
			if msg == "" {
				msg = "unknown stream error"
			}
			return fmt.Errorf("fireworks: provider stream error: %s", msg)
		}
		onChunk(&chunk)
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if !sawDone {
		// EOF before [DONE] — a truncated stream, not a clean completion.
		return io.ErrUnexpectedEOF
	}
	return nil
}

const maxSSELineBytes = 8 * 1024 * 1024 // 8MB ceiling per SSE line

// maxStreamTotalBytes caps the cumulative `data:` payload bytes a single stream
// may carry over its (300s) window — a backstop against an unbounded visible /
// reasoning / tool-argument buffer, distinct from the per-line cap.
const maxStreamTotalBytes = 64 * 1024 * 1024 // 64MB total stream ceiling

// maxToolArgBytes caps the accumulated argument bytes for a single streamed tool
// call — a runaway argument fragment stream can't grow one entry unbounded.
const maxToolArgBytes = 8 * 1024 * 1024 // 8MB per tool-call arguments

var (
	dataPrefix = []byte("data:")
	doneMarker = []byte("[DONE]")
)
