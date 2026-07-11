package waitbudget

import (
	"context"
	"testing"
	"time"
)

// Consume clamps at the balance, never over-grants, and drains to exactly zero.
func TestConsume_ClampsAndDrains(t *testing.T) {
	b := New(10 * time.Millisecond)
	if got := b.Consume(4 * time.Millisecond); got != 4*time.Millisecond {
		t.Fatalf("full grant = %v, want 4ms", got)
	}
	if got := b.Consume(100 * time.Millisecond); got != 6*time.Millisecond {
		t.Fatalf("clamped grant = %v, want the 6ms balance", got)
	}
	if !b.Exhausted() {
		t.Fatal("budget should be exhausted after draining")
	}
	if got := b.Consume(time.Millisecond); got != 0 {
		t.Fatalf("drained budget granted %v, want 0", got)
	}
	if got := b.Remaining(); got != 0 {
		t.Fatalf("Remaining = %v, want 0", got)
	}
}

// The nil receiver is the UNBUDGETED mode every pre-existing caller relies on:
// grants in full, never exhausted, huge Remaining.
func TestNilBudget_IsUnbudgeted(t *testing.T) {
	var b *Budget
	if got := b.Consume(time.Hour); got != time.Hour {
		t.Fatalf("nil budget must grant in full, got %v", got)
	}
	if b.Exhausted() {
		t.Fatal("nil budget must never be exhausted")
	}
	if b.Remaining() < time.Hour {
		t.Fatal("nil budget Remaining must read as plenty")
	}
}

// From on a bare context yields nil (unbudgeted); With/From round-trip the budget;
// With(nil) is a no-op.
func TestContextRoundTrip(t *testing.T) {
	if got := From(context.Background()); got != nil {
		t.Fatalf("bare ctx should carry no budget, got %v", got)
	}
	b := New(time.Second)
	ctx := With(context.Background(), b)
	if got := From(ctx); got != b {
		t.Fatal("With/From must round-trip the same budget")
	}
	if got := With(context.Background(), nil); got != context.Background() {
		t.Fatal("With(nil) must return ctx unchanged")
	}
}

// New clamps a negative allowance to zero (immediately exhausted, never negative).
func TestNew_ClampsNegative(t *testing.T) {
	b := New(-time.Second)
	if !b.Exhausted() || b.Remaining() != 0 {
		t.Fatalf("negative allowance should clamp to an exhausted zero budget, got %v", b.Remaining())
	}
}
