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
