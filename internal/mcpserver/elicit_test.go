package mcpserver

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/assistant/internal/agent"
	"github.com/daintreehq/assistant/internal/domain"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// elicit_test.go pins that pushing an approval is an ACCELERANT and never the
// mechanism. Every one of these cases must leave the polling path working, because
// elicitation support varies by client and a server that depended on it would have made
// approvals unusable for everyone else.

// connectElicit stands up the server against a client with the given elicitation
// handler. A nil handler means a client that does not support elicitation at all.
func connectElicit(t *testing.T, fake *fakeRuntime, handler func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error)) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	reg := NewUnconfinedRegistry(ctx, func(_, _ context.Context, _ OpenParams) (Runtime, error) { return fake, nil })
	srv := mcp.NewServer(&mcp.Implementation{Name: ServerName, Version: "test"}, nil)
	Register(srv, reg, NewBinaryInfo("test"), ctx)

	st, ct := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	opts := &mcp.ClientOptions{}
	if handler != nil {
		opts.ElicitationHandler = handler
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "test"}, opts).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() {
		_ = cs.Close()
		reg.CloseAll()
	})
	return cs
}

// ask starts a turn whose first action raises an approval.
func ask(t *testing.T, cs *mcp.ClientSession) {
	t.Helper()
	sess := openSession(t, cs)
	if err := call(t, cs, "daintree.ask", AskInput{SessionID: sess.SessionID, Prompt: "push"}, &RunOutput{}); err != nil {
		t.Fatalf("ask: %v", err)
	}
}

// askAndPark is ask() for the cases that then INSPECT the parked approval. It is only
// usable when the elicitation path will not resolve it first — a fast accept or decline
// can settle the approval before the test looks, which is a race in the test, not the
// code, and why the decision cases wait on the outcome instead.
func askAndPark(t *testing.T, cs *mcp.ClientSession, fake *fakeRuntime) PendingApproval {
	t.Helper()
	ask(t, cs)
	return waitForPending(t, fake.approvals, 1)[0]
}

// parkingRuntime is a fake whose turn parks one approval.
func parkingRuntime() (*fakeRuntime, chan bool) {
	fake := newFakeRuntime("ses_test")
	fake.approvals = NewApprovals(ApprovalDelegate, 2*time.Second)
	outcome := make(chan bool, 1)
	fake.script = func(sink agent.EventSink) {
		go func() {
			outcome <- fake.approvals.Confirm(context.Background(), ApprovalRequest{
				Tool: "git.push", Risk: domain.RiskGit, Consequence: "pushes to origin/main",
			})
		}()
	}
	return fake, outcome
}

func TestElicitationAcceptResolvesTheApproval(t *testing.T) {
	fake, outcome := parkingRuntime()
	// A channel, not a shared variable: the handler runs on the client's goroutine.
	messages := make(chan string, 1)
	cs := connectElicit(t, fake, func(_ context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		messages <- req.Params.Message
		return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"approve": true}}, nil
	})
	ask(t, cs)

	select {
	case ok := <-outcome:
		if !ok {
			t.Error("an accepted elicitation must allow the call")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the elicitation never resolved the approval")
	}
	// The prompt must carry what a decision is actually made on.
	sawMessage := <-messages
	if !strings.Contains(sawMessage, "git.push") || !strings.Contains(sawMessage, "origin/main") {
		t.Errorf("elicit message = %q, want the tool and its consequence", sawMessage)
	}
	fake.letFinish()
}

func TestElicitationDeclineRefusesTheCall(t *testing.T) {
	fake, outcome := parkingRuntime()
	cs := connectElicit(t, fake, func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		return &mcp.ElicitResult{Action: "decline"}, nil
	})
	ask(t, cs)

	select {
	case ok := <-outcome:
		if ok {
			t.Error("a declined elicitation must refuse the call")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the decline never reached the approval")
	}
	fake.letFinish()
}

// TestElicitationCancelLeavesItParked: "cancel" is a viewer dismissing the prompt, not a
// decision. Resolving it either way would put words in their mouth.
func TestElicitationCancelLeavesItParked(t *testing.T) {
	fake, outcome := parkingRuntime()
	cs := connectElicit(t, fake, func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		return &mcp.ElicitResult{Action: "cancel"}, nil
	})
	pa := askAndPark(t, cs, fake)

	// Still parked shortly after the cancel, and still answerable through the tools.
	time.Sleep(150 * time.Millisecond)
	if len(fake.approvals.Pending()) != 1 {
		t.Fatal("a cancelled elicitation must leave the approval parked for the explicit tools")
	}
	if !fake.approvals.Resolve(pa.ID, DecisionApproved) {
		t.Fatal("the approval should still be answerable")
	}
	select {
	case ok := <-outcome:
		if !ok {
			t.Error("the explicit approval was not honoured")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the approval never resolved")
	}
	fake.letFinish()
}

// TestElicitationFailureFallsBackToPolling is the load-bearing one: a client with no
// elicitation support, or one that errors, must leave the surface exactly as usable as
// if this feature did not exist.
func TestElicitationFailureFallsBackToPolling(t *testing.T) {
	for name, handler := range map[string]func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error){
		"unsupported by the client": nil,
		"handler errors": func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			return nil, errors.New("no thanks")
		},
	} {
		t.Run(name, func(t *testing.T) {
			fake, outcome := parkingRuntime()
			cs := connectElicit(t, fake, handler)
			pa := askAndPark(t, cs, fake)

			// The approval is still parked and still answerable through the tools.
			var list ApprovalsOutput
			if err := call(t, cs, "daintree.approvals", SessionRefInput{SessionID: fake.id}, &list); err != nil {
				t.Fatalf("approvals: %v", err)
			}
			if list.Count != 1 {
				t.Fatalf("the approval must remain pollable, got %+v", list)
			}
			if err := call(t, cs, "daintree.approve", ApproveInput{
				SessionID: fake.id, ApprovalID: pa.ID, Approve: true,
			}, &ActedOutput{}); err != nil {
				t.Fatalf("approve: %v", err)
			}
			select {
			case ok := <-outcome:
				if !ok {
					t.Error("the explicit approval was not honoured")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the approval never resolved through the polling path")
			}
			fake.letFinish()
		})
	}
}

// TestElicitNotifierIsNilWithoutASession guards the constructor's own precondition.
func TestElicitNotifierIsNilWithoutASession(t *testing.T) {
	if elicitNotifier(context.Background(), nil, NewApprovals(ApprovalDelegate, 0), time.Second) != nil {
		t.Error("a nil client session must produce no notifier")
	}
}
