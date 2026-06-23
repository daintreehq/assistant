package mcp

import (
	"context"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Covers the resource-subscription surface added for issue #204: capability
// reporting, subscribe/unsubscribe intent tracking + forwarding, ReadResource text
// extraction, the non-blocking resource-updated handler, and reconnect re-issue.

func TestSupportsSubscribeReflectsServer(t *testing.T) {
	low := &fakeLow{supportsSub: true}
	c := newInjected(low)
	c.Connect(context.Background())
	if !c.SupportsSubscribe() {
		t.Error("SupportsSubscribe should reflect the live server capability")
	}
	// Disconnected → must report false (caller falls back to polling), never panic.
	_ = c.Close()
	if c.SupportsSubscribe() {
		t.Error("a disconnected client must report SupportsSubscribe()==false")
	}
}

func TestSubscribeRecordsIntentAndForwards(t *testing.T) {
	low := &fakeLow{supportsSub: true}
	c := newInjected(low)
	c.Connect(context.Background())
	if err := c.Subscribe(context.Background(), "daintree://agent/a/state"); err != nil {
		t.Fatal(err)
	}
	if len(low.subscribed) != 1 || low.subscribed[0] != "daintree://agent/a/state" {
		t.Errorf("subscribe must forward to the low client, got %v", low.subscribed)
	}
	c.mu.Lock()
	_, tracked := c.subs["daintree://agent/a/state"]
	c.mu.Unlock()
	if !tracked {
		t.Error("subscribe must record the URI so a reconnect re-issues it")
	}
}

func TestUnsubscribeForgetsAndForwards(t *testing.T) {
	low := &fakeLow{supportsSub: true}
	c := newInjected(low)
	c.Connect(context.Background())
	_ = c.Subscribe(context.Background(), "daintree://agent/a/state")
	if err := c.Unsubscribe(context.Background(), "daintree://agent/a/state"); err != nil {
		t.Fatal(err)
	}
	if len(low.unsubscribed) != 1 || low.unsubscribed[0] != "daintree://agent/a/state" {
		t.Errorf("unsubscribe must forward to the low client, got %v", low.unsubscribed)
	}
	c.mu.Lock()
	_, tracked := c.subs["daintree://agent/a/state"]
	c.mu.Unlock()
	if tracked {
		t.Error("unsubscribe must forget the URI so a reconnect won't resurrect it")
	}
}

func TestReadResourceReturnsText(t *testing.T) {
	low := &fakeLow{readResults: map[string]string{"daintree://agent/a/state": `{"state":"waiting"}`}}
	c := newInjected(low)
	c.Connect(context.Background())
	got, err := c.ReadResource(context.Background(), "daintree://agent/a/state")
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"state":"waiting"}` {
		t.Errorf("ReadResource = %q, want the resource text body", got)
	}
}

func TestResourceUpdatedForwardsURI(t *testing.T) {
	c := newInjected(&fakeLow{})
	// A nil request / nil params must be a safe no-op (defensive against the SDK).
	c.onResourceUpdated(context.Background(), nil)
	c.onResourceUpdated(context.Background(), &sdkmcp.ResourceUpdatedNotificationRequest{})

	c.onResourceUpdated(context.Background(), &sdkmcp.ResourceUpdatedNotificationRequest{
		Params: &sdkmcp.ResourceUpdatedNotificationParams{URI: "daintree://agent/a/state"},
	})
	select {
	case uri := <-c.ResourceUpdates():
		if uri != "daintree://agent/a/state" {
			t.Errorf("forwarded URI = %q, want the changed resource URI", uri)
		}
	default:
		t.Fatal("a resource-updated notification must be forwarded onto ResourceUpdates")
	}
}

func TestResourceUpdatedDropsWhenFull(t *testing.T) {
	c := newInjected(&fakeLow{})
	req := &sdkmcp.ResourceUpdatedNotificationRequest{
		Params: &sdkmcp.ResourceUpdatedNotificationParams{URI: "u"},
	}
	// Overfill the buffer: the handler must never block (it runs on the receive loop).
	for i := 0; i < resourceUpdateBuffer+10; i++ {
		c.onResourceUpdated(context.Background(), req)
	}
	drained := 0
	for {
		select {
		case <-c.ResourceUpdates():
			drained++
		default:
			if drained != resourceUpdateBuffer {
				t.Errorf("buffer should hold exactly %d, drained %d", resourceUpdateBuffer, drained)
			}
			return
		}
	}
}

func TestResubscribeReissuesAll(t *testing.T) {
	low := &fakeLow{supportsSub: true}
	c := newInjected(low)
	c.Connect(context.Background())
	_ = c.Subscribe(context.Background(), "daintree://agent/a/state")
	_ = c.Subscribe(context.Background(), "daintree://agent/b/state")
	fresh := &fakeLow{}
	c.resubscribe(fresh, []string{"daintree://agent/a/state", "daintree://agent/b/state"})
	if len(fresh.subscribed) != 2 {
		t.Fatalf("resubscribe must re-issue every live URI on the fresh session, got %v", fresh.subscribed)
	}
}

// TestSharedURIRefcount: two local subscribers (e.g. two watchers on the same
// terminal) share one wire subscription. Only the first Subscribe and the last
// Unsubscribe hit the wire, so one watcher stopping can't revoke the other's push.
func TestSharedURIRefcount(t *testing.T) {
	low := &fakeLow{supportsSub: true}
	c := newInjected(low)
	c.Connect(context.Background())
	const uri = "daintree://agent/shared/state"

	_ = c.Subscribe(context.Background(), uri)
	_ = c.Subscribe(context.Background(), uri)
	if len(low.subscribed) != 1 {
		t.Errorf("a shared URI must issue exactly one wire Subscribe, got %v", low.subscribed)
	}

	// First withdrawal: the other subscriber still needs it → no wire Unsubscribe.
	_ = c.Unsubscribe(context.Background(), uri)
	if len(low.unsubscribed) != 0 {
		t.Errorf("first Unsubscribe must NOT hit the wire while a subscriber remains, got %v", low.unsubscribed)
	}
	// Last withdrawal: now it's safe to tear down.
	_ = c.Unsubscribe(context.Background(), uri)
	if len(low.unsubscribed) != 1 || low.unsubscribed[0] != uri {
		t.Errorf("last Unsubscribe must issue the wire Unsubscribe, got %v", low.unsubscribed)
	}
}

// TestResubscribeSkipsUnsubscribed: a URI dropped between the reconnect snapshot
// and the resubscribe goroutine must NOT be resurrected on the fresh session.
func TestResubscribeSkipsUnsubscribed(t *testing.T) {
	low := &fakeLow{supportsSub: true}
	c := newInjected(low)
	c.Connect(context.Background())
	const uri = "daintree://agent/gone/state"
	_ = c.Subscribe(context.Background(), uri)
	_ = c.Unsubscribe(context.Background(), uri) // refcount → 0, forgotten

	fresh := &fakeLow{}
	c.resubscribe(fresh, []string{uri}) // stale snapshot still lists it
	if len(fresh.subscribed) != 0 {
		t.Errorf("resubscribe must skip a URI no longer wanted, got %v", fresh.subscribed)
	}
}
