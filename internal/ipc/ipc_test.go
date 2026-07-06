package ipc

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestFileLockExcludesSecondHandle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owner.lock")
	a := NewFileLock(path)
	ok, err := a.TryAcquire()
	if err != nil || !ok {
		t.Fatalf("first acquire: ok=%v err=%v", ok, err)
	}
	// flock is per open-file-description, so a second handle in the same
	// process still contends — which is what makes this testable hermetically.
	b := NewFileLock(path)
	ok, err = b.TryAcquire()
	if err != nil {
		t.Fatalf("second acquire err: %v", err)
	}
	if ok {
		t.Fatal("second handle acquired a held lock")
	}
	if got := ReadLockHolderPid(path); got != os.Getpid() {
		t.Fatalf("holder pid = %d, want %d", got, os.Getpid())
	}
	a.Release()
	ok, err = b.TryAcquire()
	if err != nil || !ok {
		t.Fatalf("acquire after release: ok=%v err=%v", ok, err)
	}
	b.Release()
}

func TestFileLockAcquireWaits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owner.lock")
	a := NewFileLock(path)
	if ok, err := a.TryAcquire(); err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	released := make(chan struct{})
	go func() {
		time.Sleep(80 * time.Millisecond)
		a.Release()
		close(released)
	}()
	b := NewFileLock(path)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.Acquire(ctx, 10*time.Millisecond); err != nil {
		t.Fatalf("blocking acquire: %v", err)
	}
	<-released
	b.Release()
}

// echoHandler answers status with a canned reply and tracks attach lifecycle.
type echoHandler struct {
	mu         sync.Mutex
	closed     int
	attachHits int
}

func (h *echoHandler) HandleRequest(_ context.Context, req Request, conn *ServerConn) Response {
	switch req.Type {
	case ReqStatus:
		raw, _ := json.Marshal(StatusReply{State: StateStandby, Pid: os.Getpid(), StateDir: "/x"})
		return Response{OK: true, Payload: raw}
	case ReqAttach:
		h.mu.Lock()
		h.attachHits++
		h.mu.Unlock()
		conn.SetAttached(true)
		raw, _ := json.Marshal(AttachReply{OwnerReleased: true})
		return Response{OK: true, Payload: raw}
	default:
		return Response{OK: false, Error: "unknown request type"}
	}
}

func (h *echoHandler) ConnClosed(conn *ServerConn) {
	if conn.Attached() {
		h.mu.Lock()
		h.closed++
		h.mu.Unlock()
	}
}

func shortSocketPath(t *testing.T) string {
	t.Helper()
	// t.TempDir can exceed darwin's sun_path cap; use a short system temp dir.
	dir, err := os.MkdirTemp("", "dt-ipc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "d.sock")
}

func TestServerClientRoundTrip(t *testing.T) {
	path := shortSocketPath(t)
	h := &echoHandler{}
	srv := NewServer(path, h)
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Close()

	c, err := Dial(path, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	var st StatusReply
	if err := c.Call(context.Background(), ReqStatus, nil, &st); err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.State != StateStandby || st.Pid != os.Getpid() {
		t.Fatalf("unexpected status: %+v", st)
	}
	if err := c.Call(context.Background(), "bogus", nil, nil); err == nil {
		t.Fatal("bogus request type should error")
	}
}

func TestAttachLeaseEndsOnClose(t *testing.T) {
	path := shortSocketPath(t)
	h := &echoHandler{}
	srv := NewServer(path, h)
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Close()

	c, err := Dial(path, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	var rep AttachReply
	if err := c.Call(context.Background(), ReqAttach, AttachRequest{ClientPid: os.Getpid()}, &rep); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if !rep.OwnerReleased {
		t.Fatalf("unexpected attach reply: %+v", rep)
	}
	_ = c.Close()
	deadline := time.After(2 * time.Second)
	for {
		h.mu.Lock()
		closed := h.closed
		h.mu.Unlock()
		if closed == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("ConnClosed for attached conn not observed (closed=%d)", closed)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestStaleSocketFileIsReplaced(t *testing.T) {
	path := shortSocketPath(t)
	// Simulate a crashed daemon: a socket file nobody serves.
	first := NewServer(path, &echoHandler{})
	if err := first.Start(context.Background()); err != nil {
		t.Fatalf("first start: %v", err)
	}
	// Close the listener WITHOUT removing the file (crash simulation).
	first.mu.Lock()
	ln := first.ln
	first.closed = true
	first.mu.Unlock()
	_ = ln.Close()
	if _, err := os.Stat(path); err != nil {
		// listener close removed it on this platform; recreate a dead file
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	second := NewServer(path, &echoHandler{})
	if err := second.Start(context.Background()); err != nil {
		t.Fatalf("second start over stale socket: %v", err)
	}
	defer second.Close()
	c, err := Dial(path, time.Second)
	if err != nil {
		t.Fatalf("dial replaced socket: %v", err)
	}
	defer c.Close()
	var st StatusReply
	if err := c.Call(context.Background(), ReqStatus, nil, &st); err != nil {
		t.Fatalf("status on replaced socket: %v", err)
	}
}

func TestDialNoDaemon(t *testing.T) {
	path := shortSocketPath(t)
	if _, err := Dial(path, 200*time.Millisecond); err != ErrNoDaemon {
		t.Fatalf("dial absent socket: err=%v, want ErrNoDaemon", err)
	}
	// Stale file, nothing listening → also ErrNoDaemon.
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Dial(path, 200*time.Millisecond); err != ErrNoDaemon {
		t.Fatalf("dial stale socket file: err=%v, want ErrNoDaemon", err)
	}
}

func TestSocketPathForIsShortAndStable(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(SocketDirEnv, dir)
	a, err := SocketPathFor("/some/deep/state/dir")
	if err != nil {
		t.Fatal(err)
	}
	b, err := SocketPathFor("/some/deep/state/dir")
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("not deterministic: %s vs %s", a, b)
	}
	c, _ := SocketPathFor("/other/dir")
	if c == a {
		t.Fatal("different state dirs mapped to the same socket")
	}
	if base := filepath.Base(a); len(base) > 24 {
		t.Fatalf("socket basename too long: %s", base)
	}
}
