package mcp

import (
	"context"
	"encoding/json"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// rawTool is the minimal live-tool shape the low-level client surfaces (the SDK's
// *mcp.Tool reduced to what we consume). InputSchema stays `any` (verbatim) so the
// default-substitution decision lives in the high-level mapper.
type rawTool struct {
	Name        string
	Description string
	InputSchema any // nil when the live tool advertised none
}

// rawResult is the minimal callTool response shape (the SDK's *mcp.CallToolResult
// reduced). Content is kept verbatim ([]any) for pass-through; Text is the
// flattened text already extracted from text blocks; StructuredContent/IsError
// pass through.
type rawResult struct {
	Content           []any
	Text              string
	StructuredContent any
	IsError           bool
}

// LowLevelClient is the mockable SDK subset the high-level Client depends on. The
// real impl wraps a *mcp.ClientSession; tests inject a fake. GetServerVersion is
// nil-safe (it replaces raw.getServerVersion() — only metadata, never a network
// call). Keep this interface narrow: it IS the test seam.
type LowLevelClient interface {
	ListTools(ctx context.Context) ([]rawTool, error)
	CallTool(ctx context.Context, name string, args map[string]any) (rawResult, error)
	GetServerVersion() *ServerInfo // may return nil
	// SupportsSubscribe reports whether the connected server advertised
	// capabilities.resources.subscribe (nil-safe; false when unknown).
	SupportsSubscribe() bool
	// Subscribe/Unsubscribe register/cancel interest in a resource URI. The server
	// then pushes resources/updated notifications routed to the Client's handler.
	Subscribe(ctx context.Context, uri string) error
	Unsubscribe(ctx context.Context, uri string) error
	// ReadResource fetches a resource's current contents, returning the FIRST
	// content block's text ("" when the resource has no text body).
	ReadResource(ctx context.Context, uri string) (string, error)
	Close() error
}

// sdkLowLevel adapts a connected *mcp.ClientSession to LowLevelClient.
type sdkLowLevel struct {
	session *sdkmcp.ClientSession
}

func (s *sdkLowLevel) ListTools(ctx context.Context) ([]rawTool, error) {
	res, err := s.session.ListTools(ctx, &sdkmcp.ListToolsParams{})
	if err != nil {
		return nil, err
	}
	out := make([]rawTool, 0, len(res.Tools))
	for _, t := range res.Tools {
		if t == nil {
			continue
		}
		out = append(out, rawTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema, // may be nil → high-level mapper substitutes the default
		})
	}
	return out, nil
}

func (s *sdkLowLevel) CallTool(ctx context.Context, name string, args map[string]any) (rawResult, error) {
	res, err := s.session.CallTool(ctx, &sdkmcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return rawResult{}, err
	}
	return normalizeSDKResult(res), nil
}

// normalizeSDKResult flattens the SDK's CallToolResult into rawResult: text blocks
// joined by "\n" (empties filtered), content blocks marshaled back to verbatim
// JSON values, structuredContent/isError passed through. Marshaling each typed
// Content back to a generic `any` preserves the raw block shape for callers that
// inspect content[] without coupling us to the SDK's concrete content structs.
func normalizeSDKResult(res *sdkmcp.CallToolResult) rawResult {
	if res == nil {
		return rawResult{}
	}
	var texts []string
	content := make([]any, 0, len(res.Content))
	for _, block := range res.Content {
		if block == nil {
			continue
		}
		// Verbatim pass-through: marshal the typed block, decode to a generic value.
		if b, err := json.Marshal(block); err == nil {
			var v any
			if json.Unmarshal(b, &v) == nil {
				content = append(content, v)
			}
		}
		// Text extraction: only *TextContent carries a text payload.
		if tc, ok := block.(*sdkmcp.TextContent); ok {
			if tc.Text != "" {
				texts = append(texts, tc.Text)
			}
		}
	}
	return rawResult{
		Content:           content,
		Text:              joinNonEmpty(texts),
		StructuredContent: res.StructuredContent,
		IsError:           res.IsError,
	}
}

func (s *sdkLowLevel) GetServerVersion() *ServerInfo {
	if s.session == nil {
		return nil
	}
	init := s.session.InitializeResult()
	if init == nil || init.ServerInfo == nil {
		return nil
	}
	return &ServerInfo{Name: init.ServerInfo.Name, Version: init.ServerInfo.Version}
}

// SupportsSubscribe walks InitializeResult().Capabilities.Resources.Subscribe with
// a full nil-chain guard — a freshly-connected session may not have an
// InitializeResult yet, and a server may omit any link in the capabilities chain.
func (s *sdkLowLevel) SupportsSubscribe() bool {
	if s.session == nil {
		return false
	}
	init := s.session.InitializeResult()
	if init == nil || init.Capabilities == nil || init.Capabilities.Resources == nil {
		return false
	}
	return init.Capabilities.Resources.Subscribe
}

func (s *sdkLowLevel) Subscribe(ctx context.Context, uri string) error {
	if s.session == nil {
		return nil
	}
	return s.session.Subscribe(ctx, &sdkmcp.SubscribeParams{URI: uri})
}

func (s *sdkLowLevel) Unsubscribe(ctx context.Context, uri string) error {
	if s.session == nil {
		return nil
	}
	return s.session.Unsubscribe(ctx, &sdkmcp.UnsubscribeParams{URI: uri})
}

// ReadResource returns the first content block's text. A resource-updated
// notification carries NO payload (only a "dirty" URI), so the read after it is
// how the new state is actually fetched. Blob-only/empty resources yield "".
func (s *sdkLowLevel) ReadResource(ctx context.Context, uri string) (string, error) {
	if s.session == nil {
		return "", nil
	}
	res, err := s.session.ReadResource(ctx, &sdkmcp.ReadResourceParams{URI: uri})
	if err != nil {
		return "", err
	}
	if res == nil {
		return "", nil
	}
	for _, c := range res.Contents {
		if c != nil && c.Text != "" {
			return c.Text, nil
		}
	}
	return "", nil
}

func (s *sdkLowLevel) Close() error {
	if s.session == nil {
		return nil
	}
	return s.session.Close()
}

// joinNonEmpty joins with "\n" after filtering empties (filter(Boolean)+join).
func joinNonEmpty(parts []string) string {
	out := ""
	first := true
	for _, p := range parts {
		if p == "" {
			continue
		}
		if !first {
			out += "\n"
		}
		out += p
		first = false
	}
	return out
}
