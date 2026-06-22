package timer

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/tools"
)

func ctxDaemon(active *bool) *tools.ToolContext {
	if active == nil {
		return &tools.ToolContext{}
	}
	return &tools.ToolContext{DaemonActive: func() bool { return *active }}
}

// The lifecycle note differs across scheduler-running / not-running / absent.
func TestScheduleLifecycleNotice(t *testing.T) {
	tool := find(Tools(Deps{Store: &memStore{}}), "timer.schedule")
	args := json.RawMessage(`{"title":"ping","delayMs":1000,"payload":{"type":"enqueue"}}`)

	on := true
	running := tool.Handle(context.Background(), args, ctxDaemon(&on))
	if !running.Ok {
		t.Fatalf("running: %+v", running.Error)
	}
	if !strings.Contains(running.Summary, "resumes on the next launch") ||
		strings.Contains(running.Summary, "scheduler is NOT running") {
		t.Fatalf("running note: %q", running.Summary)
	}

	off := false
	stopped := tool.Handle(context.Background(), args, ctxDaemon(&off))
	if !stopped.Ok {
		t.Fatalf("stopped: %+v", stopped.Error)
	}
	if !strings.Contains(stopped.Summary, "scheduler is NOT running") ||
		!strings.Contains(stopped.Summary, "will not fire") {
		t.Fatalf("stopped note: %q", stopped.Summary)
	}

	// daemonActive absent ⇒ assume active (resumes-on-next-launch wording).
	absent := tool.Handle(context.Background(), args, ctxDaemon(nil))
	if !absent.Ok || !strings.Contains(absent.Summary, "resumes on the next launch") {
		t.Fatalf("absent note: %q", absent.Summary)
	}
}

// run_check is no longer a creatable payload (dropped from the enum); enqueue and
// call_safe_tool (with a toolName) are accepted.
func TestSchedulePayloadValidation(t *testing.T) {
	tool := find(Tools(Deps{Store: &memStore{}}), "timer.schedule")

	runCheck := tool.Handle(context.Background(),
		json.RawMessage(`{"title":"x","delayMs":1000,"payload":{"type":"run_check","checkPrompt":"done?"}}`),
		&tools.ToolContext{})
	if runCheck.Ok || runCheck.Error.Code != codeInvalidArgs {
		t.Fatalf("run_check must be rejected, got %+v", runCheck)
	}

	enqueue := tool.Handle(context.Background(),
		json.RawMessage(`{"title":"x","delayMs":1000,"payload":{"type":"enqueue","message":"hi"}}`),
		&tools.ToolContext{})
	if !enqueue.Ok {
		t.Fatalf("enqueue should be accepted, got %+v", enqueue.Error)
	}

	callSafe := tool.Handle(context.Background(),
		json.RawMessage(`{"title":"x","delayMs":1000,"payload":{"type":"call_safe_tool","toolCall":{"toolName":"x.y"}}}`),
		&tools.ToolContext{})
	if !callSafe.Ok {
		t.Fatalf("call_safe_tool with a toolName should be accepted, got %+v", callSafe.Error)
	}
}
