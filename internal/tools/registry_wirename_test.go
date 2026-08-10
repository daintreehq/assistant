package tools

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/config"
	"github.com/daintreehq/assistant/internal/domain"
)

var openAINameRePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// dottedTool is a minimal read-only tool carrying an arbitrary dotted name, used
// to exercise the wire-name alias layer.
func dottedTool(name string) *Tool {
	return &Tool{Name: name, Risk: domain.RiskRead,
		Handle: func(_ context.Context, _ json.RawMessage, _ *ToolContext) ToolResult { return Ok("ran", nil) }}
}

// TestWireNameExactRoundTrip exercises the exact projections:
// test.read → test__read, multi-dot names sanitize EVERY segment, and an unknown
// wire name resolves to "".
func TestWireNameExactRoundTrip(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(dottedTool("test.read"))
	specs, err := r.OpenAITools(nil)
	if err != nil {
		t.Fatal(err)
	}
	if specs[0].Function.Name != "test__read" {
		t.Fatalf("want wire name test__read, got %q", specs[0].Function.Name)
	}
	if got := r.ResolveWireName("test__read"); got != "test.read" {
		t.Fatalf("resolve test__read = %q want test.read", got)
	}

	// Multi-dot: every segment's '.' becomes '__'.
	r2 := NewRegistry()
	_ = r2.Register(dottedTool("watcher.terminal.create"))
	specs2, err := r2.OpenAITools(nil)
	if err != nil {
		t.Fatal(err)
	}
	if specs2[0].Function.Name != "watcher__terminal__create" {
		t.Fatalf("want watcher__terminal__create, got %q", specs2[0].Function.Name)
	}
	if got := r2.ResolveWireName("watcher__terminal__create"); got != "watcher.terminal.create" {
		t.Fatalf("multi-dot round-trip wrong: %q", got)
	}

	// Unknown wire name → empty.
	if got := r2.ResolveWireName("nope__missing"); got != "" {
		t.Fatalf("unknown wire name should resolve empty, got %q", got)
	}
}

// TestWireNameTooLongThrows: a sanitized wire name over 64
// chars must fail projection (the model can't use an illegal function name).
func TestWireNameTooLongThrows(t *testing.T) {
	r := NewRegistry()
	// 30 dotted segments → a __-joined wire name far longer than 64 chars.
	segs := make([]string, 30)
	for i := range segs {
		segs[i] = "seg"
	}
	r.Register(dottedTool(strings.Join(segs, ".")))
	if _, err := r.OpenAITools(nil); err == nil {
		t.Fatal("over-64-char wire name must throw")
	} else if !strings.Contains(strings.ToLower(err.Error()), "does not match") {
		t.Fatalf("expected a 'does not match' error, got %v", err)
	}
}

// TestWireNameCollisionThrows: two internal names that map
// to the same wire name (fs.read and fs__read both → fs__read) must be detected.
func TestWireNameCollisionThrows(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(dottedTool("fs.read"))
	_ = r.Register(dottedTool("fs__read"))
	if _, err := r.OpenAITools(nil); err == nil {
		t.Fatal("wire-name collision must throw")
	} else if !strings.Contains(strings.ToLower(err.Error()), "collision") {
		t.Fatalf("expected a 'collision' error, got %v", err)
	}
}

// TestWireNameFilteredAliasMapOnly: a narrowed projection
// only puts the filtered tools in the alias map.
func TestWireNameFilteredAliasMapOnly(t *testing.T) {
	r := NewRegistry()
	_ = r.RegisterAll(dottedTool("test.read"), dottedTool("test.project"))
	specs, err := r.OpenAITools([]string{"test.read"})
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 1 || specs[0].Function.Name != "test__read" {
		t.Fatalf("filter should narrow to test__read, got %+v", specs)
	}
	if r.ResolveWireName("test__read") != "test.read" {
		t.Fatal("filtered tool must still resolve")
	}
	if r.ResolveWireName("test__project") != "" {
		t.Fatal("excluded tool must not be in the alias map")
	}
}

// TestEveryToolProjectsToLegalWireName guarantees that
// every projected wire name is OpenAI-legal, dot-free, and round-trips.
func TestEveryToolProjectsToLegalWireName(t *testing.T) {
	r := NewRegistry()
	for _, n := range []string{"fs.read", "fs.list", "fs.search", "daintree.call", "queue.publish", "watcher.create"} {
		_ = r.Register(dottedTool(n))
	}
	specs, err := r.OpenAITools(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 6 {
		t.Fatalf("want 6 specs (no silent drops), got %d", len(specs))
	}
	for _, s := range specs {
		if !openAINameRePattern.MatchString(s.Function.Name) {
			t.Errorf("illegal wire name %q", s.Function.Name)
		}
		if strings.Contains(s.Function.Name, ".") {
			t.Errorf("wire name must not contain '.': %q", s.Function.Name)
		}
		if r.ResolveWireName(s.Function.Name) == "" {
			t.Errorf("wire name %q must round-trip to an internal name", s.Function.Name)
		}
	}
}

// TestDispatchByInternalNameAfterProjection: dispatch still
// works by the internal dotted name after a wire projection.
func TestDispatchByInternalNameAfterProjection(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(dottedTool("test.read"))
	if _, err := r.OpenAITools(nil); err != nil {
		t.Fatal(err)
	}
	res := r.Dispatch(context.Background(), "test.read", nil,
		&ToolContext{Config: config.AppConfig{Tier: domain.TierOperator}, DB: &fakeStore{}, Actor: domain.ActorMain})
	if !res.Ok {
		t.Fatalf("dispatch by internal name should succeed, got %+v", res.Error)
	}
}
