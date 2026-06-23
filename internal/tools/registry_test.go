package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/config"
	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/safety"
)

func noopTool(name string) *Tool {
	return &Tool{Name: name, Risk: domain.RiskRead,
		Handle: func(_ context.Context, _ json.RawMessage, _ *ToolContext) ToolResult { return Ok("ok", nil) }}
}

func TestRegisterDuplicate(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(noopTool("fs.read")); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(noopTool("fs.read")); err == nil {
		t.Fatal("duplicate registration should error")
	}
}

func TestAssertSafeRejectsFileEditTools(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(noopTool("fs.read"))
	if err := r.AssertSafe(); err != nil {
		t.Fatalf("clean registry should pass AssertSafe: %v", err)
	}
	_ = r.Register(noopTool("fs.write")) // forbidden fragment
	err := r.AssertSafe()
	if err == nil {
		t.Fatal("AssertSafe must reject a file-mutating tool")
	}
	if _, ok := err.(*safety.FileEditAttemptError); !ok {
		t.Fatalf("expected *FileEditAttemptError, got %T", err)
	}
}

func TestAssertRegisteredPassesWhenAllPresent(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(noopTool("fs.read"))
	_ = r.Register(noopTool("fs.list"))
	if err := r.AssertRegistered("core tools", []string{"fs.read", "fs.list"}); err != nil {
		t.Fatalf("all names registered should pass: %v", err)
	}
	// An empty list is vacuously satisfied.
	if err := r.AssertRegistered("core tools", nil); err != nil {
		t.Fatalf("empty list should pass: %v", err)
	}
}

func TestAssertRegisteredReportsMissingNames(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(noopTool("fs.read"))
	err := r.AssertRegistered("core tools", []string{"fs.read", "fs.search", "memory.recall"})
	if err == nil {
		t.Fatal("AssertRegistered must fail when a name is missing")
	}
	// The error names the offending list and every missing entry (drift diagnostic).
	if !strings.Contains(err.Error(), "core tools") {
		t.Fatalf("error should name the list label; got %q", err.Error())
	}
	for _, want := range []string{"fs.search", "memory.recall"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error should list missing %q; got %q", want, err.Error())
		}
	}
	if strings.Contains(err.Error(), "fs.read") {
		t.Fatalf("error must not list the registered name fs.read; got %q", err.Error())
	}
}

func TestWireNameRoundTrip(t *testing.T) {
	r := NewRegistry()
	_ = r.RegisterAll(noopTool("fs.read"), noopTool("daintree.call"))
	specs, err := r.OpenAITools(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 2 {
		t.Fatalf("want 2 specs, got %d", len(specs))
	}
	// Every "." becomes "__".
	for _, s := range specs {
		if strings.Contains(s.Function.Name, ".") {
			t.Errorf("wire name should not contain '.': %q", s.Function.Name)
		}
	}
	if got := r.ResolveWireName("fs__read"); got != "fs.read" {
		t.Errorf("resolve fs__read = %q want fs.read", got)
	}
	if got := r.ResolveWireName("daintree__call"); got != "daintree.call" {
		t.Errorf("resolve daintree__call = %q want daintree.call", got)
	}
	if got := r.ResolveWireName("unknown__x"); got != "" {
		t.Errorf("unknown wire name should resolve to empty, got %q", got)
	}
}

func TestOpenAIToolsFilterNarrows(t *testing.T) {
	r := NewRegistry()
	_ = r.RegisterAll(noopTool("fs.read"), noopTool("fs.list"))
	// Project all, then narrow: a previously-wide name must no longer resolve.
	if _, err := r.OpenAITools(nil); err != nil {
		t.Fatal(err)
	}
	if r.ResolveWireName("fs__list") != "fs.list" {
		t.Fatal("fs.list should resolve after wide projection")
	}
	if _, err := r.OpenAITools([]string{"fs.read"}); err != nil {
		t.Fatal(err)
	}
	if r.ResolveWireName("fs__list") != "" {
		t.Fatal("narrowed projection must drop fs.list from the alias map")
	}
}

// schemaTool builds a read-only tool carrying an explicit JSON Schema.
func schemaTool(name, schema string) *Tool {
	return &Tool{Name: name, Risk: domain.RiskRead, Schema: json.RawMessage(schema),
		Handle: func(_ context.Context, _ json.RawMessage, _ *ToolContext) ToolResult { return Ok("ok", nil) }}
}

// TestRegisterInvalidSchemaFails: a malformed JSON Schema is a wiring bug and must
// fail fast at registration (the cold path), not at the first projection.
func TestRegisterInvalidSchemaFails(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(schemaTool("bad.tool", `{not valid json`)); err == nil {
		t.Fatal("Register must reject a malformed JSON Schema")
	}
}

// TestRegisterEmptySchemaDefaultsToNoArgs: a tool with no schema projects the
// canonical permissive empty object (byte-for-byte the prior inline default).
func TestRegisterEmptySchemaDefaultsToNoArgs(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(noopTool("fs.read")); err != nil {
		t.Fatal(err)
	}
	specs, err := r.OpenAITools(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(specs[0].Function.Parameters); got != `{"properties":{},"type":"object"}` {
		t.Fatalf("empty-schema params = %s want the canonical no-arg object", got)
	}
}

// TestRegisterSchemaCanonicalizedAndImmutable: the schema is canonicalized (sorted-
// compact) ONCE at registration; mutating the source Tool.Schema afterward does not
// change the projection, and the projection is byte-stable across calls.
func TestRegisterSchemaCanonicalizedAndImmutable(t *testing.T) {
	r := NewRegistry()
	tool := schemaTool("search.run", "{\n  \"type\": \"object\",\n  \"properties\": { \"q\": { \"type\": \"string\" } }\n}")
	if err := r.Register(tool); err != nil {
		t.Fatal(err)
	}
	// Mutate the source schema AFTER registration — the captured projection is frozen.
	tool.Schema = json.RawMessage(`{"type":"object","properties":{}}`)

	specs, err := r.OpenAITools(nil)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"properties":{"q":{"type":"string"}},"type":"object"}`
	if got := string(specs[0].Function.Parameters); got != want {
		t.Fatalf("projected params = %s want canonical %s", got, want)
	}
	// Byte-stable across calls (prompt-cache stability invariant).
	specs2, err := r.OpenAITools(nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(specs2[0].Function.Parameters) != want {
		t.Fatalf("projection params must be byte-stable across calls, got %s", specs2[0].Function.Parameters)
	}
}

func TestExemplarFsReadSecretGuard(t *testing.T) {
	tool := NewFsReadTool()
	tctx := &ToolContext{Config: config.AppConfig{}, ProjectPath: t.TempDir(), Actor: domain.ActorMain}
	res := tool.Handle(context.Background(), json.RawMessage(`{"path":".env"}`), tctx)
	if res.Ok {
		t.Fatal("fs.read of .env should be denied by the secret guard")
	}
}

func TestCapJSON(t *testing.T) {
	small := `{"a":1}`
	if capJSON(small) != small {
		t.Fatal("small JSON should pass through unchanged")
	}
	big := strings.Repeat("x", maxAuditJSON+500)
	out := capJSON(big)
	var wrapped struct {
		Truncated bool   `json:"truncated"`
		Bytes     int    `json:"bytes"`
		Preview   string `json:"preview"`
	}
	if err := json.Unmarshal([]byte(out), &wrapped); err != nil {
		t.Fatalf("capped output must be JSON: %v", err)
	}
	if !wrapped.Truncated || wrapped.Bytes != len(big) {
		t.Fatalf("capped output wrong: %+v", wrapped)
	}
	if len([]rune(wrapped.Preview)) != maxAuditJSON-previewHeadroom {
		t.Fatalf("preview length = %d want %d", len([]rune(wrapped.Preview)), maxAuditJSON-previewHeadroom)
	}
}
