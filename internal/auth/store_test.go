package auth

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeExitError builds a real *exec.ExitError carrying a chosen code. ProcessState is
// opaque so one cannot be constructed directly, and the classifiers branch on the exit
// code — so the tests need genuine ones.
func fakeExitError(code int) error {
	err := exec.Command("sh", "-c", "exit "+strconv.Itoa(code)).Run()
	if err == nil && code != 0 {
		panic("expected a non-zero exit")
	}
	return err
}

// --- credential keying --------------------------------------------------------------

// The property that stops a staging token reaching a production server after `/backend`
// repoints the CLI mid-session.
func TestEveryComponentOfTheCredentialKeyChangesTheAccount(t *testing.T) {
	base := CredentialKey{
		StateRoot:     "/root",
		BackendOrigin: "https://assistant.daintree.org",
		Issuer:        "https://proj.supabase.co/auth/v1",
		Environment:   "staging",
		ClientID:      "client-a",
	}
	variants := map[string]CredentialKey{
		// The state root is here because coordination (the lock, the revision marker)
		// lives under it while the OS credential entry does not. Without it, a
		// --state-dir launch shares one rotating refresh token with a default launch but
		// coordinates through a different lock — so both can rotate it at once, and a
		// logout in one is invisible to the other.
		"state root":  {StateRoot: "/other", BackendOrigin: base.BackendOrigin, Issuer: base.Issuer, Environment: base.Environment, ClientID: base.ClientID},
		"backend":     {StateRoot: base.StateRoot, BackendOrigin: "https://other.daintree.org", Issuer: base.Issuer, Environment: base.Environment, ClientID: base.ClientID},
		"issuer":      {StateRoot: base.StateRoot, BackendOrigin: base.BackendOrigin, Issuer: "https://other.supabase.co/auth/v1", Environment: base.Environment, ClientID: base.ClientID},
		"environment": {StateRoot: base.StateRoot, BackendOrigin: base.BackendOrigin, Issuer: base.Issuer, Environment: "production", ClientID: base.ClientID},
		"client":      {StateRoot: base.StateRoot, BackendOrigin: base.BackendOrigin, Issuer: base.Issuer, Environment: base.Environment, ClientID: "client-b"},
	}
	for name, v := range variants {
		if v.Account() == base.Account() {
			t.Errorf("changing the %s did not change the credential account — a token could cross deployments", name)
		}
	}
	// ...and the same tuple is stable, or a restart would lose the login.
	if base.Account() != (CredentialKey{
		StateRoot:     "/root",
		BackendOrigin: "https://assistant.daintree.org",
		Issuer:        "https://proj.supabase.co/auth/v1",
		Environment:   "staging",
		ClientID:      "client-a",
	}).Account() {
		t.Fatal("the same key produced two different accounts")
	}
}

// The account name reaches GUI keychain browsers and platform length limits.
func TestTheAccountNameIsBoundedAndOpaque(t *testing.T) {
	k := CredentialKey{
		StateRoot:     "/root",
		BackendOrigin: "https://" + strings.Repeat("a", 500) + ".example",
		Issuer:        "https://proj.supabase.co/auth/v1",
		Environment:   "staging",
		ClientID:      "c",
	}
	acct := k.Account()
	if len(acct) != 32 {
		t.Fatalf("account is %d characters, want a bounded digest", len(acct))
	}
	if strings.Contains(acct, "example") || strings.Contains(acct, "http") {
		t.Error("the account name echoes the raw key")
	}
}

func TestStoredSessionValidity(t *testing.T) {
	good := StoredSession{Version: storedSessionVersion, RefreshToken: "r", Issuer: "i", ClientID: "c"}
	if !good.Valid() {
		t.Fatal("a complete session was rejected")
	}
	for name, s := range map[string]StoredSession{
		"no version":  {RefreshToken: "r", Issuer: "i", ClientID: "c"},
		"future ver":  {Version: storedSessionVersion + 1, RefreshToken: "r", Issuer: "i", ClientID: "c"},
		"no token":    {Version: storedSessionVersion, Issuer: "i", ClientID: "c"},
		"blank token": {Version: storedSessionVersion, RefreshToken: "   ", Issuer: "i", ClientID: "c"},
		"no issuer":   {Version: storedSessionVersion, RefreshToken: "r", ClientID: "c"},
		"no client":   {Version: storedSessionVersion, RefreshToken: "r", Issuer: "i"},
	} {
		if s.Valid() {
			t.Errorf("%s: reported valid", name)
		}
	}
}

// Persisting an access token would add a second place to leak from in exchange for
// saving one round trip an hour; an email would put personal data in the OS credential
// store for no benefit and go stale besides.
func TestOnlyTheRefreshTokenIsPersisted(t *testing.T) {
	body, err := json.Marshal(StoredSession{
		Version: storedSessionVersion, RefreshToken: "r", Issuer: "i", ClientID: "c", Environment: "staging",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, forbidden := range []string{"access_token", "email", "user_id", "sub", "id_token", "session_id"} {
		if _, ok := fields[forbidden]; ok {
			t.Errorf("StoredSession persists %q", forbidden)
		}
	}
	if _, ok := fields["refresh_token"]; !ok {
		t.Error("StoredSession does not persist the refresh token")
	}
}

// --- MemoryStore ---------------------------------------------------------------------

func TestMemoryStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	k := CredentialKey{StateRoot: "/root", BackendOrigin: "b", Issuer: "i", Environment: "staging", ClientID: "c"}

	if _, err := s.Load(ctx, k); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty Load = %v, want ErrNotFound", err)
	}
	want := StoredSession{Version: storedSessionVersion, RefreshToken: "tok", Issuer: "i", ClientID: "c"}
	if err := s.Save(ctx, k, want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := s.Load(ctx, k)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.RefreshToken != "tok" {
		t.Fatalf("RefreshToken = %q", got.RefreshToken)
	}
	if err := s.Delete(ctx, k); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Load(ctx, k); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after Delete: %v, want ErrNotFound", err)
	}
	// A user must always be able to reach the signed-out state.
	if err := s.Delete(ctx, k); err != nil {
		t.Fatalf("deleting an absent session failed: %v", err)
	}
}

func TestMemoryStoreKeepsKeysApart(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	staging := CredentialKey{StateRoot: "/root", BackendOrigin: "b", Issuer: "i", Environment: "staging", ClientID: "c"}
	prod := CredentialKey{StateRoot: "/root", BackendOrigin: "b", Issuer: "i", Environment: "production", ClientID: "c"}

	_ = s.Save(ctx, staging, StoredSession{Version: 1, RefreshToken: "staging-tok", Issuer: "i", ClientID: "c"})
	if _, err := s.Load(ctx, prod); !errors.Is(err, ErrNotFound) {
		t.Fatal("a staging credential was readable under the production key")
	}
}

// --- KeyringStore, driven against a fake platform tool -------------------------------

// fakeTool records every invocation so a test can assert on argv and stdin without
// touching the real keychain (which would prompt the developer for a password).
type fakeTool struct {
	mu    sync.Mutex
	calls []fakeCall
	// respond returns stdout/stderr/err for one invocation.
	respond func(args []string, stdin []byte) ([]byte, []byte, error)
}

type fakeCall struct {
	args  []string
	stdin []byte
}

func (f *fakeTool) run(_ context.Context, _ string, args []string, stdin []byte) ([]byte, []byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{args: append([]string(nil), args...), stdin: append([]byte(nil), stdin...)})
	f.mu.Unlock()
	if f.respond == nil {
		return nil, nil, nil
	}
	return f.respond(args, stdin)
}

func (f *fakeTool) lastCall(t *testing.T) fakeCall {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		t.Fatal("the credential tool was never invoked")
	}
	return f.calls[len(f.calls)-1]
}

// THE property of this file: argv is world-readable through `ps`, so a refresh token
// passed as an argument is disclosed to every local user for as long as the command
// runs — and this command runs on every rotation.
//
// The check is deliberately stronger than "the raw string is absent from argv": the
// payload is hex-encoded on macOS, so a naive substring test would pass even if the hex
// WERE in argv. Both encodings are checked.
func TestTheSecretNeverReachesArgvInAnyEncoding(t *testing.T) {
	const secret = "super-secret-refresh-token-value"
	f := &fakeTool{}
	ks := &KeyringStore{run: f.run, toolPath: "/fake/tool"}
	k := CredentialKey{StateRoot: "/root", BackendOrigin: "b", Issuer: "i", Environment: "staging", ClientID: "c"}

	err := ks.Save(context.Background(), k, StoredSession{
		Version: storedSessionVersion, RefreshToken: secret, Issuer: "i", ClientID: "c",
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	call := f.lastCall(t)
	secretHex := hex.EncodeToString([]byte(secret))
	joined := strings.Join(call.args, " ")
	if strings.Contains(joined, secret) {
		t.Fatalf("the refresh token appeared in argv: %q", joined)
	}
	if strings.Contains(joined, secretHex) {
		t.Fatalf("the hex-encoded refresh token appeared in argv: %q", joined)
	}
	stdin := string(call.stdin)
	if !strings.Contains(stdin, secret) && !strings.Contains(stdin, secretHex) {
		t.Fatal("the refresh token did not reach the tool in any encoding — nothing was stored")
	}
}

// The earlier fake accepted stdin directly, so it could not have caught the real macOS
// behaviour: bare `-w` is an interactive prompt that reads /dev/tty and exits 2 with a
// usage dump when there is no terminal. Pinning the exact argv is what stops that class
// of bug returning.
func TestTheCommandShapesArePinned(t *testing.T) {
	k := CredentialKey{StateRoot: "/root", BackendOrigin: "b", Issuer: "i", Environment: "staging", ClientID: "c"}
	acct := k.Account()

	save := &fakeTool{}
	_ = (&KeyringStore{run: save.run, toolPath: "/fake/tool"}).Save(context.Background(), k,
		StoredSession{Version: storedSessionVersion, RefreshToken: "tok", Issuer: "i", ClientID: "c"})
	saveCall := save.lastCall(t)

	load := &fakeTool{respond: func([]string, []byte) ([]byte, []byte, error) {
		return []byte(`{"v":1,"refresh_token":"t","issuer":"i","client_id":"c"}`), nil, nil
	}}
	_, _ = (&KeyringStore{run: load.run, toolPath: "/fake/tool"}).Load(context.Background(), k)
	loadArgs := load.lastCall(t).args

	del := &fakeTool{}
	_ = (&KeyringStore{run: del.run, toolPath: "/fake/tool"}).Delete(context.Background(), k)
	delArgs := del.lastCall(t).args

	switch runtime.GOOS {
	case "darwin":
		// The whole point: argv is ONLY ["-i"]. Everything else, secret included, rides
		// stdin as a command stream.
		if !slicesEqual(saveCall.args, []string{"-i"}) {
			t.Errorf("save argv = %v, want exactly [-i]", saveCall.args)
		}
		cmd := string(saveCall.stdin)
		if !strings.HasPrefix(cmd, "add-generic-password -U ") {
			t.Errorf("save command = %q", cmd)
		}
		if !strings.Contains(cmd, " -X ") {
			t.Error("save does not use -X; hex is what removes every quoting question")
		}
		if strings.Contains(cmd, " -v") {
			t.Error("save uses -v, which echoes the whole command — secret included — to stdout")
		}
		if !slicesEqual(loadArgs, []string{"find-generic-password", "-s", ServiceName, "-a", acct, "-w"}) {
			t.Errorf("load argv = %v", loadArgs)
		}
		if !slicesEqual(delArgs, []string{"delete-generic-password", "-s", ServiceName, "-a", acct}) {
			t.Errorf("delete argv = %v", delArgs)
		}
	case "linux":
		if len(saveCall.args) == 0 || saveCall.args[0] != "store" {
			t.Errorf("save argv = %v, want a store command", saveCall.args)
		}
		if !containsPair(saveCall.args, "service", ServiceName) || !containsPair(saveCall.args, "account", acct) {
			t.Errorf("save argv is missing its attribute pairs: %v", saveCall.args)
		}
		if !slicesEqual(loadArgs, []string{"lookup", "service", ServiceName, "account", acct}) {
			t.Errorf("load argv = %v", loadArgs)
		}
		if !slicesEqual(delArgs, []string{"clear", "service", ServiceName, "account", acct}) {
			t.Errorf("delete argv = %v", delArgs)
		}
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsPair(args []string, k, v string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == k && args[i+1] == v {
			return true
		}
	}
	return false
}

// On Linux a missing item is exit 1 with NOTHING on either stream — indistinguishable by
// exit code from a real failure. Modelling absence as exit 0 (as the earlier test did)
// meant first launch would report a store failure and logout would be blocked, and the
// test would have passed anyway.
func TestLinuxClassificationOfTheNormalAbsentCase(t *testing.T) {
	quiet := fakeExitError(1)
	if err := classifyLinux(quiet, nil, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("quiet exit 1 = %v, want ErrNotFound — first launch would look broken and logout would be blocked", err)
	}
	// Exit 1 that actually SAID something is not the absent case.
	for msg, want := range map[string]error{
		"Cannot autolaunch D-Bus without X11": ErrStoreUnavailable,
		"Prompt was dismissed":                ErrStoreLocked,
		"No such secret item at path":         ErrNotFound,
	} {
		if err := classifyLinux(quiet, nil, []byte(msg)); !errors.Is(err, want) {
			t.Errorf("%q = %v, want %v", msg, err, want)
		}
	}
}

// 44 is errSecItemNotFound and is the ordinary signed-out state; 36/29/51 are
// locked/denied and mean "unlock", which is a completely different instruction from
// "sign in again". An unrecognised code must fall through SAFELY.
func TestDarwinExitCodesMapToTheRightSentinels(t *testing.T) {
	for code, want := range map[int]error{
		dsItemNotFound:        ErrNotFound,
		dsInteractionNotAllow: ErrStoreLocked,
		dsKeychainLocked:      ErrStoreLocked,
		dsAuthFailed:          ErrStoreLocked,
		dsNoSuchKeychain:      ErrStoreUnavailable,
		99:                    ErrStoreUnavailable,
	} {
		if got := classifyDarwin(fakeExitError(code), nil); !errors.Is(got, want) {
			t.Errorf("exit %d = %v, want %v", code, got, want)
		}
	}
}

// A user must always be able to reach the signed-out state.
func TestDeletingAnAbsentCredentialSucceeds(t *testing.T) {
	k := CredentialKey{StateRoot: "/root", BackendOrigin: "b", Issuer: "i", Environment: "staging", ClientID: "c"}
	f := &fakeTool{respond: func([]string, []byte) ([]byte, []byte, error) {
		if runtime.GOOS == "darwin" {
			return nil, []byte("security: The specified item could not be found in the keychain."), fakeExitError(dsItemNotFound)
		}
		return nil, nil, fakeExitError(1)
	}}
	if err := (&KeyringStore{run: f.run, toolPath: "/fake/tool"}).Delete(context.Background(), k); err != nil {
		t.Fatalf("deleting an absent credential failed: %v — logout would be blocked", err)
	}
}

func TestTheStoreDistinguishesItsFourFailureModes(t *testing.T) {
	k := CredentialKey{StateRoot: "/root", BackendOrigin: "b", Issuer: "i", Environment: "staging", ClientID: "c"}

	for name, payload := range map[string]string{
		"garbage":           "not json at all",
		"incomplete":        `{"v":1}`,
		"newer build":       `{"v":99,"refresh_token":"r","issuer":"i","client_id":"c"}`,
		"json but no token": `{"v":1,"issuer":"i","client_id":"c"}`,
	} {
		f := &fakeTool{respond: func([]string, []byte) ([]byte, []byte, error) {
			return []byte(payload), nil, nil
		}}
		_, err := (&KeyringStore{run: f.run, toolPath: "/fake/tool"}).Load(context.Background(), k)
		if !errors.Is(err, ErrStoreCorrupt) {
			t.Errorf("%s = %v, want ErrStoreCorrupt", name, err)
		}
	}
}

func TestAnIncompleteSessionIsNeverStored(t *testing.T) {
	f := &fakeTool{}
	ks := &KeyringStore{run: f.run, toolPath: "/fake/tool"}
	k := CredentialKey{StateRoot: "/root", BackendOrigin: "b", Issuer: "i", Environment: "staging", ClientID: "c"}

	if err := ks.Save(context.Background(), k, StoredSession{RefreshToken: "r"}); err == nil {
		t.Fatal("a session with no issuer or client was stored")
	}
	f.mu.Lock()
	n := len(f.calls)
	f.mu.Unlock()
	if n != 0 {
		t.Fatal("the credential tool was invoked for an invalid session")
	}
}

func TestKeyringRoundTripThroughTheFakeTool(t *testing.T) {
	var stored []byte
	f := &fakeTool{respond: func(args []string, stdin []byte) ([]byte, []byte, error) {
		switch {
		case slicesEqual(args, []string{"-i"}):
			// macOS: the command stream arrives on stdin. Pull the hex payload out of it
			// exactly as `security` would.
			cmd := string(stdin)
			i := strings.Index(cmd, " -X ")
			if i < 0 {
				return nil, []byte("security: no -X payload"), nil
			}
			decoded, err := hex.DecodeString(strings.TrimSpace(cmd[i+4:]))
			if err != nil {
				return nil, []byte("security: bad hex"), nil
			}
			stored = decoded
			return nil, nil, nil
		case args[0] == "store":
			stored = append([]byte(nil), stdin...)
			return nil, nil, nil
		case args[0] == "find-generic-password", args[0] == "lookup":
			if stored == nil {
				if runtime.GOOS == "darwin" {
					return nil, nil, fakeExitError(dsItemNotFound)
				}
				return nil, nil, fakeExitError(1)
			}
			return stored, nil, nil
		}
		return nil, nil, nil
	}}
	ks := &KeyringStore{run: f.run, toolPath: "/fake/tool"}
	k := CredentialKey{StateRoot: "/root", BackendOrigin: "b", Issuer: "i", Environment: "staging", ClientID: "c"}
	want := StoredSession{Version: storedSessionVersion, RefreshToken: "tok-1", Issuer: "i", ClientID: "c", Environment: "staging"}

	if _, err := ks.Load(context.Background(), k); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Load before Save = %v, want ErrNotFound", err)
	}
	if err := ks.Save(context.Background(), k, want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := ks.Load(context.Background(), k)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.RefreshToken != want.RefreshToken || got.Issuer != want.Issuer {
		t.Fatalf("round trip lost data: %+v", got)
	}
}

// `security find-generic-password -w` prints the stored bytes as PLAIN text when they
// are printable and as HEX when they are not. Our payload is printable JSON, so plain is
// the normal path — decoding hex on the way in makes the round trip total rather than
// dependent on a formatting rule we do not control.
func TestAHexPrintedPayloadIsStillDecodable(t *testing.T) {
	body := []byte(`{"v":1,"refresh_token":"t","issuer":"i","client_id":"c"}`)
	f := &fakeTool{respond: func([]string, []byte) ([]byte, []byte, error) {
		return []byte(hex.EncodeToString(body)), nil, nil
	}}
	k := CredentialKey{StateRoot: "/root", BackendOrigin: "b", Issuer: "i", Environment: "staging", ClientID: "c"}
	got, err := (&KeyringStore{run: f.run, toolPath: "/fake/tool"}).Load(context.Background(), k)
	if err != nil {
		t.Fatalf("Load of a hex-printed payload: %v", err)
	}
	if got.RefreshToken != "t" {
		t.Fatalf("RefreshToken = %q", got.RefreshToken)
	}
}

// OpenStore must return the memory tier WITH a reason, never silently. A user whose
// login evaporates on exit and was never told experiences it as the assistant randomly
// forgetting them.
func TestNoCredentialServiceDegradesExplicitly(t *testing.T) {
	ks := NewKeyringStore()
	err := ks.probe(context.Background())
	if err == nil {
		// A machine WITH a credential service: assert the positive contract instead.
		if got := ks.Tier(context.Background()); got != TierKeychain {
			t.Fatalf("Tier = %q, want %q", got, TierKeychain)
		}
		return
	}
	// A machine without one: the error must be actionable, not a bare failure.
	if !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("probe error = %v, want ErrStoreUnavailable", err)
	}
	store, tier, oerr := OpenStore(context.Background())
	if tier != TierMemory {
		t.Fatalf("tier = %q, want %q", tier, TierMemory)
	}
	if oerr == nil {
		t.Fatal("OpenStore degraded to memory storage without saying why")
	}
	if _, ok := store.(*MemoryStore); !ok {
		t.Fatalf("store is %T, want *MemoryStore", store)
	}
}

// --- revision -------------------------------------------------------------------------

func TestRevisionStartsAtZeroAndIncrements(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	r := NewRevision(dir)
	if !r.Current().Zero() {
		t.Fatalf("Current with no file = %v, want the zero marker", r.Current())
	}
	if r.Changed() {
		t.Fatal("Changed reported true before anything happened")
	}
	for i := uint64(1); i <= 3; i++ {
		if err := r.Bump(ctx); err != nil {
			t.Fatalf("Bump %d: %v", i, err)
		}
		if got := r.Current().Counter; got != i {
			t.Fatalf("Counter = %d, want %d", got, i)
		}
	}
	if r.Current().Nonce == "" {
		t.Fatal("the marker carries no nonce — a deleted file would be indistinguishable from a fresh one")
	}
}

// The signal the daemon depends on: another process logged out, and this one must find
// out before its next paid request.
func TestAnotherProcessesBumpIsVisibleAsAChange(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	daemon := NewRevision(dir)
	terminal := NewRevision(dir)

	daemon.MarkObserved(daemon.Current())

	if err := terminal.Bump(ctx); err != nil {
		t.Fatalf("Bump: %v", err)
	}
	if !daemon.Changed() {
		t.Fatal("the daemon did not see the logout — it would keep spending on a signed-out account")
	}
	// Observing is the caller's decision, made after it has actually reloaded: marking
	// it inside Changed would let a process notice once, fail to reload, and never
	// notice again.
	if !daemon.Changed() {
		t.Fatal("Changed auto-adopted; a failed reload would be forgotten")
	}
	daemon.MarkObserved(daemon.Current())
	if daemon.Changed() {
		t.Fatal("Changed is still true after MarkObserved")
	}
}

// THE reason the marker carries a nonce. With a bare counter, deleting the file — a
// `reset`, a tmp cleaner, a fresh container layer — takes every reader back to 0. A
// daemon that had observed 1 would then see a recreated file bumped back to 1 and
// conclude nothing had changed, silently keeping a logged-out session alive.
func TestDeletingAndRecreatingTheMarkerIsStillSeenAsAChange(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	daemon := NewRevision(dir)
	other := NewRevision(dir)

	if err := other.Bump(ctx); err != nil { // marker is now <nonceA>:1
		t.Fatalf("first bump: %v", err)
	}
	daemon.MarkObserved(daemon.Current()) // the daemon adopts <nonceA>:1
	if daemon.Changed() {
		t.Fatal("Changed immediately after adopting")
	}

	// The file goes away, then a logout recreates it at counter 1 again.
	if err := os.Remove(daemon.Path()); err != nil {
		t.Fatalf("remove: %v", err)
	}
	fresh := NewRevision(dir)
	if err := fresh.Bump(ctx); err != nil {
		t.Fatalf("bump after deletion: %v", err)
	}
	if got := fresh.Current().Counter; got != 1 {
		t.Fatalf("counter after recreation = %d, want 1 — the ABA setup is wrong", got)
	}
	if !daemon.Changed() {
		t.Fatal("the daemon missed a logout because the recreated marker collided with what it observed")
	}
}

// Two DIFFERENT credentials hold different per-credential locks, so their bumps are
// concurrent as far as the credential lock is concerned. A lost update here is not
// cosmetic: a bump that read 5 and was descheduled can overwrite 7 with 6, and once a
// later mutation restores 7 a daemon that observed 7 never learns anything happened.
func TestConcurrentBumpsDoNotLoseUpdates(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	const n = 12
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := NewRevision(dir).Bump(ctx); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Bump: %v", err)
	}
	if got := NewRevision(dir).Current().Counter; got != n {
		t.Fatalf("counter = %d after %d concurrent bumps — updates were lost", got, n)
	}
}

// This marker's only job is to invalidate a cache. An unreadable file must not take the
// assistant offline over a value that is neither secret nor authoritative — but it must
// also never compare EQUAL to a real observation.
func TestACorruptMarkerReadsAsZeroAndStillSignalsAChange(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	r := NewRevision(dir)
	if err := r.Bump(ctx); err != nil {
		t.Fatalf("Bump: %v", err)
	}
	observer := NewRevision(dir)
	observer.MarkObserved(observer.Current())

	for _, body := range []string{"", "not-a-marker", "-5", "nononce ", " 7", strings.Repeat("9", 500), "\x00\x01"} {
		if err := os.WriteFile(r.Path(), []byte(body), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		if got := NewRevision(dir).Current(); !got.Zero() {
			t.Errorf("body %q read as %v, want the zero marker", body, got)
		}
		if !observer.Changed() {
			t.Errorf("body %q compared equal to a real observation", body)
		}
	}
	// ...and a bump over a corrupt file still produces a usable, increasing value.
	if err := NewRevision(dir).Bump(ctx); err != nil {
		t.Fatalf("Bump over a corrupt file: %v", err)
	}
	if NewRevision(dir).Current().Zero() {
		t.Fatal("the marker is still zero after a bump")
	}
}

func TestTheMarkerFileIs0600AndHoldsNoSecret(t *testing.T) {
	dir := t.TempDir()
	r := NewRevision(dir)
	if err := r.Bump(context.Background()); err != nil {
		t.Fatalf("Bump: %v", err)
	}
	info, err := os.Stat(r.Path())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}
	body, err := os.ReadFile(r.Path())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// "<hex nonce> <counter>" and nothing else. This file is polled by every process on
	// the machine; a credential must never end up in it.
	nonce, count, ok := strings.Cut(strings.TrimSpace(string(body)), " ")
	if !ok || len(nonce) != revisionNonceBytes*2 || count != "1" {
		t.Fatalf("the marker file holds %q — it must be a bare nonce and counter", string(body))
	}
}

// Separate state roots must not share coordination state.
func TestDifferentStateRootsCoordinateIndependently(t *testing.T) {
	ctx := context.Background()
	a := NewRevision(t.TempDir())
	b := NewRevision(t.TempDir())
	if err := a.Bump(ctx); err != nil {
		t.Fatalf("Bump: %v", err)
	}
	if !b.Current().Zero() {
		t.Fatal("a bump in one state root reached another")
	}
}

// --- cross-process lock ----------------------------------------------------------------

func TestTheCredentialLockIsExclusiveAndPerCredential(t *testing.T) {
	dir, err := AuthDir(t.TempDir())
	if err != nil {
		t.Fatalf("AuthDir: %v", err)
	}
	a := CredentialKey{StateRoot: "/root", BackendOrigin: "b", Issuer: "i", Environment: "staging", ClientID: "c"}
	b := CredentialKey{StateRoot: "/root", BackendOrigin: "b", Issuer: "i", Environment: "production", ClientID: "c"}

	held, err := acquireCredentialLock(context.Background(), dir, a)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	// A DIFFERENT credential must not be blocked: a staging login and a production one
	// have no reason to serialize against each other.
	other, err := acquireCredentialLock(context.Background(), dir, b)
	if err != nil {
		t.Fatalf("a different credential was blocked: %v", err)
	}
	other.release()

	// The SAME credential must block until released.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := acquireCredentialLock(ctx, dir, a); err == nil {
		t.Fatal("the same credential was locked twice concurrently — two processes could rotate at once")
	}

	held.release()
	regained, err := acquireCredentialLock(context.Background(), dir, a)
	if err != nil {
		t.Fatalf("the lock was not released: %v", err)
	}
	regained.release()
}

func TestAuthDirIs0700UnderTheStateRoot(t *testing.T) {
	root := t.TempDir()
	dir, err := AuthDir(root)
	if err != nil {
		t.Fatalf("AuthDir: %v", err)
	}
	if filepath.Dir(dir) != root {
		t.Fatalf("auth dir %q is not under the state root %q", dir, root)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("mode = %o, want 700", perm)
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	dir, err := AuthDir(t.TempDir())
	if err != nil {
		t.Fatalf("AuthDir: %v", err)
	}
	k := CredentialKey{StateRoot: "/root", BackendOrigin: "b", Issuer: "i", Environment: "staging", ClientID: "c"}
	l, err := acquireCredentialLock(context.Background(), dir, k)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	l.release()
	l.release() // must not panic
	var nilLock *credentialLock
	nilLock.release() // must not panic
}
