package terminalobs

import "testing"

// A working observation with no prior send counts; a later send invalidates it;
// fresh work after the send re-validates.
func TestSeenWorkingSinceLastCommand(t *testing.T) {
	m := NewMemory()
	if m.SeenWorkingSinceLastCommand("t1") {
		t.Fatal("empty memory must report false")
	}
	m.MarkWorking("t1", 100)
	if !m.SeenWorkingSinceLastCommand("t1") {
		t.Fatal("working with no prior send must count")
	}
	m.MarkCommandSent("t1", 200)
	if m.SeenWorkingSinceLastCommand("t1") {
		t.Fatal("a send after the last working observation must invalidate the latch")
	}
	m.MarkWorking("t1", 300)
	if !m.SeenWorkingSinceLastCommand("t1") {
		t.Fatal("fresh work after the send must re-validate the latch")
	}
}

// A same-millisecond send/working tie fails SAFE: within one clock tick the real
// order is unknowable (a delayed poll mark can postdate a send it actually
// preceded), and a wrong "true" would settle a wait on pre-send evidence. False
// only costs the slow fresh-evidence/grace path.
func TestSameMillisecondTieFailsSafe(t *testing.T) {
	m := NewMemory()
	m.MarkCommandSent("t1", 500)
	m.MarkWorking("t1", 500)
	if m.SeenWorkingSinceLastCommand("t1") {
		t.Fatal("a same-ms tie must fail safe (false), not seed a settle")
	}
	m.MarkWorking("t1", 501)
	if !m.SeenWorkingSinceLastCommand("t1") {
		t.Fatal("working strictly after the send must count")
	}
}

// Timestamps are monotonic per key: an older mark never regresses a newer one.
func TestMarksNeverRegress(t *testing.T) {
	m := NewMemory()
	m.MarkCommandSent("t1", 200)
	m.MarkWorking("t1", 300)
	m.MarkWorking("t1", 100) // stale duplicate (e.g. an out-of-order poll)
	if !m.SeenWorkingSinceLastCommand("t1") {
		t.Fatal("the stale MarkWorking(100) must not regress lastWorkingAt below the 300 observation")
	}
	m.MarkCommandSent("t1", 400)
	m.MarkCommandSent("t1", 50) // stale duplicate
	if m.SeenWorkingSinceLastCommand("t1") {
		t.Fatal("the stale MarkCommandSent(50) must not regress lastCommandAt below the 400 send")
	}
}

// Nil receivers and blank ids are inert (unwired consumers keep old behaviour).
func TestNilAndBlankSafety(t *testing.T) {
	var m *Memory
	m.MarkWorking("t1", 1)
	m.MarkCommandSent("t1", 1)
	if m.SeenWorkingSinceLastCommand("t1") {
		t.Fatal("nil memory must report false")
	}
	real := NewMemory()
	real.MarkWorking("", 1)
	if real.SeenWorkingSinceLastCommand("") {
		t.Fatal("blank terminal id must never latch")
	}
}
