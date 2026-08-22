package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/ipc"
	"github.com/daintreehq/assistant/internal/storage"
	"github.com/daintreehq/assistant/internal/supervisor"
)

// daemonHarness runs the built binary's `daemon` subcommand as a real detached-
// style subprocess against fake backend/MCP servers, with a hermetic state dir
// and a short socket dir. Kill() is the crash simulation (SIGKILL — no cleanup
// code runs, exactly like a machine-level crash of the daemon).
type daemonHarness struct {
	t        *testing.T
	bin      string
	stateDir string
	env      []string
	cmd      *exec.Cmd
	exited   chan error
}

// newDaemonEnv provisions the hermetic dirs + env shared by the daemon and the
// test-side IPC clients (which derive the socket path from the same env).
func newDaemonEnv(t *testing.T, backendURL, mcpURL, mcpToken string) (stateDir string, env []string) {
	t.Helper()
	// Short paths: the control socket path must stay under darwin's sun_path cap.
	stateDir, err := os.MkdirTemp("", "dt-e2e-state")
	if err != nil {
		t.Fatal(err)
	}
	sockDir, err := os.MkdirTemp("", "dt-e2e-sock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stateDir); _ = os.RemoveAll(sockDir) })
	// The TEST process derives the same socket path via the env var.
	t.Setenv(ipc.SocketDirEnv, sockDir)
	env = append(os.Environ(),
		"DAINTREE_ASSISTANT_STATE_DIR="+stateDir,
		ipc.SocketDirEnv+"="+sockDir,
		"DAINTREE_BACKEND_URL="+backendURL,
		// The daemon refuses to start signed out (it would adopt work and then 401 in
		// every autonomous wake turn). The temp state dir isolates this from the real
		// sign-in; the fake backend ignores the value.
		"DAINTREE_API_KEY=test-key",
		"DAINTREE_MCP_URL="+mcpURL,
		"DAINTREE_MCP_TOKEN="+mcpToken,
		"DAINTREE_ASSISTANT_TIER=operator",
		"DAINTREE_ASSISTANT_DEBUG_LOG=0",
		"DAINTREE_ASSISTANT_DAEMON_FAST=1",
	)
	return stateDir, env
}

// startDaemon launches the daemon subprocess and waits until its control
// socket answers.
func startDaemon(t *testing.T, bin, stateDir string, env []string) *daemonHarness {
	t.Helper()
	h := &daemonHarness{t: t, bin: bin, stateDir: stateDir, env: env}
	h.start()
	return h
}

func (h *daemonHarness) start() {
	h.t.Helper()
	cmd := exec.Command(h.bin, "daemon")
	cmd.Env = h.env
	logf, err := os.OpenFile(filepath.Join(h.stateDir, "daemon-test.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err == nil {
		cmd.Stdout = logf
		cmd.Stderr = logf
	}
	if err := cmd.Start(); err != nil {
		h.t.Fatalf("start daemon: %v", err)
	}
	h.cmd = cmd
	h.exited = make(chan error, 1)
	go func() { h.exited <- cmd.Wait() }()
	h.t.Cleanup(func() { _ = cmd.Process.Kill() })
	// Wait for the control socket.
	waitFor(h.t, 10*time.Second, "daemon socket", func() bool {
		_, err := supervisor.QueryStatus(context.Background(), h.stateDir)
		return err == nil
	})
}

// kill is the crash: SIGKILL, then wait for the process to be reaped.
func (h *daemonHarness) kill() {
	h.t.Helper()
	_ = h.cmd.Process.Kill()
	select {
	case <-h.exited:
	case <-time.After(5 * time.Second):
		h.t.Fatal("killed daemon did not exit")
	}
}

// stop asks the daemon to shut down cleanly and waits for exit.
func (h *daemonHarness) stop() {
	h.t.Helper()
	_ = supervisor.RequestShutdown(context.Background(), h.stateDir)
	select {
	case <-h.exited:
	case <-time.After(15 * time.Second):
		h.t.Fatal("daemon did not exit after shutdown request")
	}
}

// status queries the daemon's control socket.
func (h *daemonHarness) status() (*ipc.StatusReply, error) {
	return supervisor.QueryStatus(context.Background(), h.stateDir)
}

// waitStatus polls the daemon status until cond holds.
func (h *daemonHarness) waitStatus(what string, cond func(ipc.StatusReply) bool) {
	h.t.Helper()
	waitFor(h.t, 20*time.Second, what, func() bool {
		st, err := h.status()
		return err == nil && cond(*st)
	})
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// seedStore opens the project DB (BEFORE the daemon starts — never concurrently
// with a live owner) and runs fn against it.
func seedStore(t *testing.T, stateDir string, fn func(s *storage.Store)) {
	t.Helper()
	s, err := storage.Open(filepath.Join(stateDir, "state.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	fn(s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

// seedRunningAsync inserts one live async invocation watching term-1.
func seedRunningAsync(t *testing.T, stateDir, id string) {
	t.Helper()
	seedStore(t, stateDir, func(s *storage.Store) {
		if _, err := s.InsertAsyncInvocation(domain.AsyncInvocationRecord{
			ID: id, ToolName: "terminal.await.async", Title: "e2e async job",
			GroupID: id, SessionID: "ses_seed", TerminalIdsJson: `["term-1"]`,
			Status: domain.AsyncRunning, CreatedAt: domain.NowMS() - 60_000,
			ExpiresAt: domain.NowMS() + 30*60_000,
		}); err != nil {
			t.Fatal(err)
		}
	})
}

// wakeCalls counts /respond requests carrying turn.is_wake=true.
func (f *fakeBackend) wakeCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, req := range f.requests {
		if turn, ok := req["turn"].(map[string]any); ok {
			if w, _ := turn["is_wake"].(bool); w {
				n++
			}
		}
	}
	return n
}

// asyncEvents returns the queue events published for async completions.
func asyncEvents(t *testing.T, stateDir string) []domain.QueueEvent {
	t.Helper()
	var out []domain.QueueEvent
	seedStore(t, stateDir, func(s *storage.Store) {
		evs, err := s.ListEvents(domain.QueueDigestOptions{IncludeResolved: true})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range evs {
			if e.Source == domain.SourceAsyncTool {
				out = append(out, e)
			}
		}
	})
	return out
}

// TestDaemonAsyncCompletionWakesExactlyOnce is the core detach story: async
// work seeded by a prior session completes under the DETACHED daemon, which
// publishes exactly one queue event and runs exactly one autonomous wake turn
// (turn.is_wake on the wire), recording the outcome durably.
func TestDaemonAsyncCompletionWakesExactlyOnce(t *testing.T) {
	if raceEnabled {
		t.Skip("subprocess test adds no race coverage")
	}
	bin := buildBinary(t)
	backend := newFakeBackend(t, sseRound{contentTokens: []string{"Handled the completion."}})
	dt := newScriptableMCP(t)
	dt.setTerminal("term-1", "working", "", nil)

	stateDir, env := newDaemonEnv(t, backend.baseURL(), dt.url(), "tok-1")
	seedRunningAsync(t, stateDir, "asy_e2e1")

	h := startDaemon(t, bin, stateDir, env)
	h.waitStatus("daemon supervising with adopted async", func(st ipc.StatusReply) bool {
		return st.State == ipc.StateSupervising && st.McpConnected && st.LiveAsync == 1
	})

	// The agent finishes: working → waiting. The coordinator's FSM settles it,
	// publishes, and the daemon wakes.
	dt.setTerminal("term-1", "waiting", "", nil)
	waitFor(t, 30*time.Second, "autonomous wake turn", func() bool { return backend.wakeCalls() >= 1 })
	h.waitStatus("wake turn recorded", func(st ipc.StatusReply) bool { return st.WakeTurnsRun == 1 })
	h.stop()

	if got := backend.wakeCalls(); got != 1 {
		t.Fatalf("wake turns on the wire = %d, want exactly 1", got)
	}
	evs := asyncEvents(t, stateDir)
	if len(evs) != 1 {
		t.Fatalf("async queue events = %d, want exactly 1 (%+v)", len(evs), evs)
	}
	seedStore(t, stateDir, func(s *storage.Store) {
		rec, err := s.GetAsyncInvocation("asy_e2e1")
		if err != nil || rec == nil {
			t.Fatalf("async row missing: %v", err)
		}
		if rec.Status != domain.AsyncSucceeded {
			t.Errorf("async status = %s, want succeeded", rec.Status)
		}
		if rec.QueueEventID == nil || *rec.QueueEventID != evs[0].ID {
			t.Errorf("async row not stamped with its queue event: %v", rec.QueueEventID)
		}
		// The wake conversation is durable: the session pointer's transcript has
		// the wake prompt + the assistant's reply.
		sid, _ := s.GetRuntimeState(storage.RuntimeKeyCurrentSession)
		if sid == "" {
			t.Fatal("daemon should have pinned a current session")
		}
		msgs, err := s.ListMessages(sid)
		if err != nil || len(msgs) < 2 {
			t.Fatalf("wake conversation not persisted: %d msgs, %v", len(msgs), err)
		}
		var sawWake, sawReply bool
		for _, m := range msgs {
			if m.Role == "user" && strings.Contains(m.Content, "[automatic wake-up]") {
				sawWake = true
			}
			if m.Role == "assistant" && strings.Contains(m.Content, "Handled the completion.") {
				sawReply = true
			}
		}
		if !sawWake || !sawReply {
			t.Errorf("wake exchange missing from transcript (wake=%v reply=%v)", sawWake, sawReply)
		}
	})
}

// TestDaemonKillRestartCompletesExactlyOnce: SIGKILL the daemon mid-watch,
// restart it, finish the terminal — the completion appears exactly once (the
// restart ADOPTS the live row instead of abandoning it).
func TestDaemonKillRestartCompletesExactlyOnce(t *testing.T) {
	if raceEnabled {
		t.Skip("subprocess test adds no race coverage")
	}
	bin := buildBinary(t)
	backend := newFakeBackend(t, sseRound{contentTokens: []string{"Done after restart."}})
	dt := newScriptableMCP(t)
	dt.setTerminal("term-1", "working", "", nil)

	stateDir, env := newDaemonEnv(t, backend.baseURL(), dt.url(), "tok-1")
	seedRunningAsync(t, stateDir, "asy_e2e2")

	h := startDaemon(t, bin, stateDir, env)
	h.waitStatus("first daemon adopted the async row", func(st ipc.StatusReply) bool {
		return st.State == ipc.StateSupervising && st.LiveAsync == 1
	})
	h.kill() // crash: no cleanup runs; flock releases via the kernel

	h2 := startDaemon(t, bin, stateDir, env)
	h2.waitStatus("restarted daemon re-adopted the async row", func(st ipc.StatusReply) bool {
		return st.State == ipc.StateSupervising && st.McpConnected && st.LiveAsync == 1
	})
	dt.setTerminal("term-1", "waiting", "", nil)
	waitFor(t, 30*time.Second, "wake after restart", func() bool { return backend.wakeCalls() >= 1 })
	h2.stop()

	if evs := asyncEvents(t, stateDir); len(evs) != 1 {
		t.Fatalf("async queue events after kill+restart = %d, want exactly 1", len(evs))
	}
}

// TestDaemonRetriesUnpublishedCompletion: the prior owner finalized the row but
// died before the queue publish (queueEventId NULL). The daemon's adoption
// retries the publish exactly once and the wake fires.
func TestDaemonRetriesUnpublishedCompletion(t *testing.T) {
	if raceEnabled {
		t.Skip("subprocess test adds no race coverage")
	}
	bin := buildBinary(t)
	backend := newFakeBackend(t, sseRound{contentTokens: []string{"Recovered the lost completion."}})
	dt := newScriptableMCP(t)

	stateDir, env := newDaemonEnv(t, backend.baseURL(), dt.url(), "tok-1")
	seedStore(t, stateDir, func(s *storage.Store) {
		outcomes := `{"term-9":{"status":"finished"}}`
		if _, err := s.InsertAsyncInvocation(domain.AsyncInvocationRecord{
			ID: "asy_lost", ToolName: "terminal.await.async", Title: "publish lost",
			GroupID: "asy_lost", SessionID: "ses_seed", TerminalIdsJson: `["term-9"]`,
			Status: domain.AsyncSucceeded, CreatedAt: domain.NowMS() - 120_000,
			ExpiresAt: domain.NowMS() + 30*60_000, OutcomesJson: &outcomes,
		}); err != nil {
			t.Fatal(err)
		}
	})

	h := startDaemon(t, bin, stateDir, env)
	waitFor(t, 30*time.Second, "publish-retry wake", func() bool { return backend.wakeCalls() >= 1 })
	h.stop()

	evs := asyncEvents(t, stateDir)
	if len(evs) != 1 {
		t.Fatalf("retried publish produced %d events, want exactly 1", len(evs))
	}
	seedStore(t, stateDir, func(s *storage.Store) {
		rec, _ := s.GetAsyncInvocation("asy_lost")
		if rec == nil || rec.QueueEventID == nil {
			t.Fatalf("retried row must be stamped with its event, got %+v", rec)
		}
	})
}

// TestDaemonUngatedMutationBecomesPendingApproval: a wake turn that wants a
// mutating tool WITHOUT a wake grant must not execute it — the call is denied
// and a blocked pending-approval item lands in the inbox for the next attach.
func TestDaemonUngatedMutationBecomesPendingApproval(t *testing.T) {
	if raceEnabled {
		t.Skip("subprocess test adds no race coverage")
	}
	bin := buildBinary(t)
	backend := newFakeBackend(t,
		sseRound{toolName: "terminal.sendCommand", toolArgs: `{"terminalId":"term-1","command":"make deploy"}`},
		sseRound{contentTokens: []string{"Blocked: needs your approval."}},
	)
	dt := newScriptableMCP(t)
	dt.setTerminal("term-1", "working", "", nil)

	stateDir, env := newDaemonEnv(t, backend.baseURL(), dt.url(), "tok-1")
	seedRunningAsync(t, stateDir, "asy_e2e4")

	h := startDaemon(t, bin, stateDir, env)
	h.waitStatus("supervising", func(st ipc.StatusReply) bool { return st.State == ipc.StateSupervising && st.McpConnected })
	dt.setTerminal("term-1", "waiting", "", nil)
	waitFor(t, 30*time.Second, "wake with denied mutation", func() bool { return backend.wakeCalls() >= 2 })
	h.waitStatus("pending approval visible", func(st ipc.StatusReply) bool { return st.PendingApproval >= 1 })
	h.stop()

	if got := dt.sentCommands(); len(got) != 0 {
		t.Fatalf("ungated mutation must NOT reach Daintree, got %v", got)
	}
	seedStore(t, stateDir, func(s *storage.Store) {
		evs, _ := s.ListEvents(domain.QueueDigestOptions{})
		var blocked *domain.QueueEvent
		for i := range evs {
			if evs[i].Severity == domain.SeverityBlocked && strings.Contains(evs[i].Title, "terminal.sendCommand") {
				blocked = &evs[i]
			}
		}
		if blocked == nil {
			t.Fatalf("expected a blocked pending-approval event, got %+v", evs)
		}
	})
}

// TestDaemonWakeGrantAuthorizesMutation: with a scoped 'wake' grant in place
// the same mutating call executes unattended and consumes one grant use.
func TestDaemonWakeGrantAuthorizesMutation(t *testing.T) {
	if raceEnabled {
		t.Skip("subprocess test adds no race coverage")
	}
	bin := buildBinary(t)
	backend := newFakeBackend(t,
		sseRound{toolName: "terminal.sendCommand", toolArgs: `{"terminalId":"term-1","command":"make deploy"}`},
		sseRound{contentTokens: []string{"Deployed."}},
	)
	dt := newScriptableMCP(t)
	dt.setTerminal("term-1", "working", "", nil)

	stateDir, env := newDaemonEnv(t, backend.baseURL(), dt.url(), "tok-1")
	seedRunningAsync(t, stateDir, "asy_e2e5")
	seedStore(t, stateDir, func(s *storage.Store) {
		toolNames := `["terminal.sendCommand"]`
		if _, err := s.InsertGrant(domain.AutomationGrantRecord{
			ActorID: domain.WakeActorID, ActorType: domain.GrantActorWake,
			AllowedToolNamesJson: &toolNames, ExpiresAt: domain.NowMS() + 60*60_000, MaxUses: 5,
		}); err != nil {
			t.Fatal(err)
		}
	})

	h := startDaemon(t, bin, stateDir, env)
	h.waitStatus("supervising", func(st ipc.StatusReply) bool { return st.State == ipc.StateSupervising && st.McpConnected })
	dt.setTerminal("term-1", "waiting", "", nil)
	waitFor(t, 30*time.Second, "granted mutation to execute", func() bool { return len(dt.sentCommands()) >= 1 })
	waitFor(t, 30*time.Second, "wake completion", func() bool { return backend.wakeCalls() >= 2 })
	h.stop()

	if got := dt.sentCommands(); len(got) != 1 || !strings.Contains(got[0], "make deploy") {
		t.Fatalf("granted mutation should reach Daintree exactly once, got %v", got)
	}
	seedStore(t, stateDir, func(s *storage.Store) {
		grants, err := s.ListGrants(domain.WakeActorID, domain.NowMS())
		if err != nil || len(grants) != 1 {
			t.Fatalf("wake grant missing: %v %v", grants, err)
		}
		if grants[0].UsesRemaining != 4 {
			t.Errorf("grant uses remaining = %d, want 4 (one consumed)", grants[0].UsesRemaining)
		}
	})
}

// TestDaemonAttachYieldsAndSecondAttachRefused: an attach makes the daemon
// release the owner lease for exactly the connection's lifetime; a second
// attach while one holds it is refused (never a second scheduler).
func TestDaemonAttachYieldsAndSecondAttachRefused(t *testing.T) {
	if raceEnabled {
		t.Skip("subprocess test adds no race coverage")
	}
	bin := buildBinary(t)
	backend := newFakeBackend(t, sseRound{contentTokens: []string{"ok"}})
	dt := newScriptableMCP(t)
	stateDir, env := newDaemonEnv(t, backend.baseURL(), dt.url(), "tok-1")
	// Live work so the daemon never idle-exits mid-test.
	seedRunningAsync(t, stateDir, "asy_e2e6")

	h := startDaemon(t, bin, stateDir, env)
	h.waitStatus("supervising", func(st ipc.StatusReply) bool { return st.State == ipc.StateSupervising })

	socketPath, err := ipc.SocketPathFor(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	a, err := ipc.Dial(socketPath, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	var rep ipc.AttachReply
	actx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := a.Call(actx, ipc.ReqAttach, ipc.AttachRequest{ClientPid: os.Getpid()}, &rep); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if !rep.OwnerReleased {
		t.Fatalf("attach should release the owner lease, got %+v", rep)
	}
	// The lease is genuinely free: this process can take it.
	lock := ipc.NewFileLock(filepath.Join(stateDir, ipc.OwnerLockName))
	ok, err := lock.TryAcquire()
	if err != nil || !ok {
		t.Fatalf("owner lease should be free after attach: ok=%v err=%v", ok, err)
	}

	// A second attach while the first holds the lease is refused.
	b, err := ipc.Dial(socketPath, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := b.Call(context.Background(), ipc.ReqAttach, ipc.AttachRequest{ClientPid: os.Getpid()}, &ipc.AttachReply{}); err == nil {
		t.Fatal("second attach should be refused while the first is live")
	}

	// Detach (drop the lease + the connection): the daemon resumes supervising.
	lock.Release()
	_ = a.Close()
	h.waitStatus("daemon resumed after detach", func(st ipc.StatusReply) bool { return st.State == ipc.StateSupervising })
	h.stop()
}

// TestDaemonCredentialRevocationBlocksThenRecovers: rotating the MCP bearer
// under the daemon (Daintree revokes per-session tokens on window close /
// displacement) must flip supervision to blocked WITHOUT abandoning work or
// hammering reconnects, and a fresh credentials push over the control socket
// must restore it.
func TestDaemonCredentialRevocationBlocksThenRecovers(t *testing.T) {
	if raceEnabled {
		t.Skip("subprocess test adds no race coverage")
	}
	bin := buildBinary(t)
	backend := newFakeBackend(t, sseRound{contentTokens: []string{"ok"}})
	dt := newScriptableMCP(t)
	dt.setTerminal("term-1", "working", "", nil)
	dt.requireToken("tok-1")

	stateDir, env := newDaemonEnv(t, backend.baseURL(), dt.url(), "tok-1")
	seedRunningAsync(t, stateDir, "asy_e2e7")

	h := startDaemon(t, bin, stateDir, env)
	h.waitStatus("connected with the original token", func(st ipc.StatusReply) bool {
		return st.State == ipc.StateSupervising && st.McpConnected
	})

	// Revoke: rotate the accepted token. The daemon's reads start failing 401.
	dt.requireToken("tok-2")
	h.waitStatus("blocked on revoked credentials", func(st ipc.StatusReply) bool {
		return st.McpBlocked && !st.McpConnected
	})
	// The async row is BLOCKED, not abandoned.
	if st, err := h.status(); err != nil || st.LiveAsync != 1 {
		t.Fatalf("live async must survive the outage, got %+v (%v)", st, err)
	}

	// A fresh launch pushes new credentials; the daemon rebuilds and reconnects.
	socketPath, err := ipc.SocketPathFor(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	c, err := ipc.Dial(socketPath, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Call(context.Background(), ipc.ReqCredentials, ipc.Credentials{McpToken: "tok-2"}, nil); err != nil {
		t.Fatalf("credentials push: %v", err)
	}
	h.waitStatus("reconnected with fresh credentials", func(st ipc.StatusReply) bool {
		return st.McpConnected && !st.McpBlocked
	})
	h.stop()
}

// TestDaemonTimerFiresDetached: a persisted due timer fires under the daemon
// with no attached session anywhere near it.
func TestDaemonTimerFiresDetached(t *testing.T) {
	if raceEnabled {
		t.Skip("subprocess test adds no race coverage")
	}
	bin := buildBinary(t)
	backend := newFakeBackend(t, sseRound{contentTokens: []string{"ok"}})
	dt := newScriptableMCP(t)
	stateDir, env := newDaemonEnv(t, backend.baseURL(), dt.url(), "tok-1")
	seedStore(t, stateDir, func(s *storage.Store) {
		if _, err := s.InsertTimer(domain.TimerRecord{
			Title: "detached reminder", FireAt: domain.NowMS() - 1000,
			PayloadType: "enqueue", PayloadJson: `{"type":"enqueue","message":"the detached timer fired"}`,
		}); err != nil {
			t.Fatal(err)
		}
	})

	h := startDaemon(t, bin, stateDir, env)
	h.waitStatus("timer fired into the inbox", func(st ipc.StatusReply) bool { return st.OpenAttention >= 1 })
	h.stop()

	seedStore(t, stateDir, func(s *storage.Store) {
		evs, _ := s.ListEvents(domain.QueueDigestOptions{})
		found := false
		for _, e := range evs {
			if e.Source == domain.SourceTimer && strings.Contains(e.Summary+e.Title, "detached timer fired") {
				found = true
			}
		}
		if !found {
			t.Fatalf("timer event missing from inbox: %+v", evs)
		}
	})
}

// TestDaemonIdleExit: with nothing to supervise the daemon exits on its own
// (exit 0) instead of lingering forever.
func TestDaemonIdleExit(t *testing.T) {
	if raceEnabled {
		t.Skip("subprocess test adds no race coverage")
	}
	bin := buildBinary(t)
	backend := newFakeBackend(t, sseRound{contentTokens: []string{"ok"}})
	dt := newScriptableMCP(t)
	stateDir, env := newDaemonEnv(t, backend.baseURL(), dt.url(), "tok-1")
	env = append(env, "DAINTREE_ASSISTANT_DAEMON_IDLE_EXIT_MS=400")

	h := startDaemon(t, bin, stateDir, env)
	select {
	case err := <-h.exited:
		if err != nil {
			t.Fatalf("idle exit should be clean, got %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("daemon did not idle-exit")
	}
	if _, err := h.status(); err == nil {
		t.Fatal("socket should be gone after idle exit")
	}
}
