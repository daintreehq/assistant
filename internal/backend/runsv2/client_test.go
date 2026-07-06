package runsv2

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daintreehq/daintree-assistant/internal/backend"
)

func testClient(t *testing.T, handler http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewClient(Config{BaseURL: srv.URL, APIKey: "k"}), srv
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func TestCreateRunAndSubmitTurn(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v2/runs", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer k" {
			t.Errorf("missing auth header, got %q", got)
		}
		var req CreateRun
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if req.Turn.TurnID != "t1" || len(req.Turn.Messages) != 1 {
			t.Errorf("unexpected turn payload: %+v", req.Turn)
		}
		writeJSON(w, http.StatusCreated, RunCreated{
			Run:  Run{RunID: "run-1", Status: "running"},
			Turn: TurnAccepted{RunID: "run-1", TurnID: "t1", Status: "running", Seq: 2},
		})
	})
	mux.HandleFunc("POST /v2/runs/run-1/turns", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, TurnAccepted{
			RunID: "run-1", TurnID: "t1", Status: "running", Duplicate: true,
		})
	})
	c, _ := testClient(t, mux)

	created, err := c.CreateRun(context.Background(), CreateRun{
		SessionID: "s",
		Turn: TurnSubmit{
			TurnID:   "t1",
			Messages: []Message{{Role: "user", Content: "hello"}},
		},
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if created.Run.RunID != "run-1" || created.Turn.Seq != 2 {
		t.Errorf("unexpected response: %+v", created)
	}

	again, err := c.SubmitTurn(context.Background(), "run-1", TurnSubmit{
		TurnID:   "t1",
		Messages: []Message{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("SubmitTurn: %v", err)
	}
	if !again.Duplicate {
		t.Error("expected duplicate acknowledgement")
	}
}

func TestErrorEnvelopeMapsToBackendError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v2/runs/run-1/turns", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": map[string]any{
				"type":    "invalid_request_error",
				"code":    "run_busy",
				"message": "A turn is already in flight for this run; cancel it first.",
			},
		})
	})
	c, _ := testClient(t, mux)
	_, err := c.SubmitTurn(context.Background(), "run-1", TurnSubmit{TurnID: "t2"})
	var be *backend.Error
	if !errors.As(err, &be) {
		t.Fatalf("expected *backend.Error, got %T: %v", err, err)
	}
	if be.HTTPStatus != http.StatusConflict || be.Code != "run_busy" {
		t.Errorf("unexpected error mapping: %+v", be)
	}
}

func TestClaimAndResultFences(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v2/tool-leases/lease-1/claim", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["executor_id"] != "cli-a" {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error": map[string]any{"code": "lease_claimed", "message": "taken"},
			})
			return
		}
		writeJSON(w, http.StatusOK, LeaseClaim{
			Outcome: "claimed",
			Lease:   Lease{LeaseID: "lease-1", Status: "claimed", IdempotencyKey: "idem"},
		})
	})
	var resultCalls atomic.Int64
	mux.HandleFunc("POST /v2/tool-leases/lease-1/result", func(w http.ResponseWriter, r *http.Request) {
		outcome := "recorded"
		if resultCalls.Add(1) > 1 {
			outcome = "duplicate"
		}
		writeJSON(w, http.StatusOK, LeaseResultAck{
			Outcome: outcome,
			Lease:   Lease{LeaseID: "lease-1", Status: "completed"},
		})
	})
	c, _ := testClient(t, mux)

	claim, err := c.ClaimLease(context.Background(), "lease-1", "cli-a")
	if err != nil {
		t.Fatalf("ClaimLease: %v", err)
	}
	if claim.Outcome != "claimed" || claim.Lease.IdempotencyKey != "idem" {
		t.Errorf("unexpected claim: %+v", claim)
	}
	if _, err := c.ClaimLease(context.Background(), "lease-1", "cli-b"); err == nil {
		t.Error("expected lease_claimed conflict for second executor")
	}

	result := LeaseResult{ExecutorID: "cli-a", Status: "ok", Content: "data"}
	first, err := c.PostLeaseResult(context.Background(), "lease-1", result)
	if err != nil || first.Outcome != "recorded" {
		t.Fatalf("first result: %+v err=%v", first, err)
	}
	second, err := c.PostLeaseResult(context.Background(), "lease-1", result)
	if err != nil || second.Outcome != "duplicate" {
		t.Fatalf("retry should be a safe duplicate: %+v err=%v", second, err)
	}
}

func TestStreamEventsParsesFramesAndCursors(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v2/runs/run-1/events", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("after"); got != "5" {
			t.Errorf("expected after=5, got %q", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, ": ping\n\n")
		fmt.Fprint(w, "id: 6\nevent: turn.submitted\ndata: {\"seq\":6,\"ts\":1.5,\"payload\":{\"turn_id\":\"t1\"}}\n\n")
		fmt.Fprint(w, "event: model.delta\ndata: {\"seq\":null,\"ts\":0,\"payload\":{\"content\":\"hi\"}}\n\n")
		fmt.Fprint(w, "id: 7\nevent: turn.completed\ndata: {\"seq\":7,\"ts\":2.0,\"payload\":{}}\n\n")
	})
	c, _ := testClient(t, mux)

	var events []Event
	err := c.StreamEvents(context.Background(), "run-1", 5, func(ev Event) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("StreamEvents: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d: %+v", len(events), events)
	}
	if events[0].Seq != 6 || events[0].Type != "turn.submitted" {
		t.Errorf("bad first event: %+v", events[0])
	}
	if !events[1].Ephemeral() || events[1].Type != "model.delta" {
		t.Errorf("delta should be ephemeral: %+v", events[1])
	}
	var payload struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(events[1].Payload, &payload); err != nil || payload.Content != "hi" {
		t.Errorf("delta payload: %v %+v", err, payload)
	}
	if events[2].Seq != 7 {
		t.Errorf("bad final event: %+v", events[2])
	}
}

func TestTailEventsReconnectsWithCursor(t *testing.T) {
	var connects atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v2/runs/run-1/events", func(w http.ResponseWriter, r *http.Request) {
		n := connects.Add(1)
		after := r.URL.Query().Get("after")
		w.Header().Set("Content-Type", "text/event-stream")
		switch n {
		case 1:
			if after != "0" {
				t.Errorf("first connect should start at 0, got %q", after)
			}
			fmt.Fprint(w, "id: 1\nevent: run.created\ndata: {\"seq\":1,\"ts\":1,\"payload\":{}}\n\n")
			// Server ends the stream; the tail must reconnect from seq 1.
		default:
			if after != "1" {
				t.Errorf("reconnect should resume after=1, got %q", after)
			}
			fmt.Fprint(w, "id: 2\nevent: turn.completed\ndata: {\"seq\":2,\"ts\":2,\"payload\":{}}\n\n")
		}
	})
	c, _ := testClient(t, mux)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stop := errors.New("done")
	var seen []string
	err := c.TailEvents(ctx, "run-1", 0, func(ev Event) error {
		seen = append(seen, ev.Type)
		if ev.Type == "turn.completed" {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) {
		t.Fatalf("expected handler stop error, got %v", err)
	}
	if len(seen) != 2 || seen[0] != "run.created" || seen[1] != "turn.completed" {
		t.Errorf("unexpected event sequence: %v", seen)
	}
	if connects.Load() < 2 {
		t.Errorf("expected a reconnect, got %d connects", connects.Load())
	}
}

func TestTailEventsHandlerErrorTerminatesEvenWhenTextLooksLikeTransport(t *testing.T) {
	// Regression: handler errors used to be classified by substring matching,
	// so an error containing "EOF" was mistaken for a transport drop and the
	// tail silently retried past the event. The sentinel wrapper must carry
	// the handler error out verbatim on the FIRST connect.
	var connects atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v2/runs/run-1/events", func(w http.ResponseWriter, r *http.Request) {
		connects.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "id: 1\nevent: assistant.message\ndata: {\"seq\":1,\"ts\":1,\"payload\":{}}\n\n")
	})
	c, _ := testClient(t, mux)

	handlerFailure := errors.New("decode payload: unexpected EOF")
	err := c.TailEvents(context.Background(), "run-1", 0, func(ev Event) error {
		return handlerFailure
	})
	if !errors.Is(err, handlerFailure) {
		t.Fatalf("expected the handler error verbatim, got %v", err)
	}
	if connects.Load() != 1 {
		t.Errorf("handler error must not trigger a reconnect, got %d connects", connects.Load())
	}
}

func TestStreamEventsJoinsMultiLineDataFrames(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v2/runs/run-1/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// One frame whose JSON is split across two data: lines (SSE joins with \n).
		fmt.Fprint(w, "id: 3\nevent: turn.submitted\ndata: {\"seq\":3,\"ts\":1,\ndata: \"payload\":{\"turn_id\":\"t1\"}}\n\n")
	})
	c, _ := testClient(t, mux)

	var events []Event
	if err := c.StreamEvents(context.Background(), "run-1", 0, func(ev Event) error {
		events = append(events, ev)
		return nil
	}); err != nil {
		t.Fatalf("StreamEvents: %v", err)
	}
	if len(events) != 1 || events[0].Seq != 3 {
		t.Fatalf("expected one parsed event, got %+v", events)
	}
	var payload struct {
		TurnID string `json:"turn_id"`
	}
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil || payload.TurnID != "t1" {
		t.Errorf("multi-line payload not joined correctly: %v %+v", err, payload)
	}
}

func TestHeartbeatReturnsPendingSweep(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v2/executors/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		var hb Heartbeat
		if err := json.NewDecoder(r.Body).Decode(&hb); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if hb.ExecutorID != "cli-a" || len(hb.Tools) != 1 || hb.Tools[0].Risk != "read" {
			t.Errorf("unexpected heartbeat: %+v", hb)
		}
		writeJSON(w, http.StatusOK, HeartbeatAck{
			ExecutorID:    "cli-a",
			ExpiresAt:     123,
			PendingLeases: []Lease{{LeaseID: "lease-9", ToolName: "fs__read", Status: "pending"}},
		})
	})
	c, _ := testClient(t, mux)

	ack, err := c.SendHeartbeat(context.Background(), Heartbeat{
		ExecutorID: "cli-a",
		Tools:      []ExecutorTool{{Name: "fs__read", Risk: "read"}},
	})
	if err != nil {
		t.Fatalf("SendHeartbeat: %v", err)
	}
	if len(ack.PendingLeases) != 1 || ack.PendingLeases[0].LeaseID != "lease-9" {
		t.Errorf("unexpected ack: %+v", ack)
	}
}
