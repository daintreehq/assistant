package agenttaskx

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/daintreehq/assistant/internal/config"
)

// launchCatalog is an advertised tool catalog whose agent.launch schema does (or
// does not) carry the handback argument — the two hosts this CLI will meet.
func launchCatalog(withHandback bool) []MCPToolInfo {
	props := map[string]any{
		"agentId":    map[string]any{"type": "string"},
		"prompt":     map[string]any{"type": "string"},
		"requestKey": map[string]any{"type": "string"},
	}
	if withHandback {
		props["handback"] = map[string]any{"type": "boolean"}
	}
	return []MCPToolInfo{
		{Name: "terminal.list", InputSchema: map[string]any{"properties": map[string]any{"handback": map[string]any{}}}, InputSchemaProvided: true},
		{Name: "agent.launch", InputSchema: map[string]any{"type": "object", "properties": props}, InputSchemaProvided: true},
	}
}

func handbackOn() config.AppConfig { return config.AppConfig{AgentHandback: true} }

// launchJSON is the outgoing agent.launch argument map as canonical JSON (json.Marshal
// sorts map keys), so "identical to today's call" is asserted on bytes, not on a
// hand-picked subset of keys.
func launchJSON(t *testing.T, m *scriptMCP) string {
	t.Helper()
	args := m.lastLaunchArgs()
	if args == nil {
		t.Fatal("agent.launch was never called")
	}
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func handbackSpawn() spawnArgs {
	a := baseSpawn()
	a.TaskPrompt = "Repair the OAuth callback handler."
	a.WorktreeID = "wt-1"
	return a
}

func TestSpawnRequestsHandbackWhenTheHostAdvertisesIt(t *testing.T) {
	for _, mode := range []string{"edit", "explore"} {
		a := handbackSpawn()
		a.Mode = mode
		mcp := &scriptMCP{connected: true, launchResult: launchOK("term_1"), toolList: launchCatalog(true)}
		if res := runSpawn(Deps{MCP: mcp, DB: newSagaStore(), Config: handbackOn()}, a); !res.Ok {
			t.Fatalf("mode %s: expected ok, got %+v", mode, res.Error)
		}
		if got := mcp.lastLaunchArgs()["handback"]; got != true {
			t.Errorf("mode %s: launch args handback = %v, want true", mode, got)
		}
		// Cache-first: a forced catalog refresh would put a round trip in front of
		// every spawn.
		for _, force := range mcp.listToolForce {
			if force {
				t.Errorf("mode %s: the catalog read must be cache-first (force=false)", mode)
			}
		}
	}
}

// The version-skew guarantee: against a host that cannot be asked, and with the switch
// off, the call is byte-identical to the one this CLI sent before the feature existed.
func TestSpawnWithoutHandbackIsByteIdenticalToTheLegacyCall(t *testing.T) {
	a := handbackSpawn()
	legacy := &scriptMCP{connected: true, launchResult: launchOK("t")}
	_ = runSpawn(Deps{MCP: legacy, DB: newSagaStore()}, a)
	want := launchJSON(t, legacy)
	// The baseline above comes from the same code, so pin what it must be from the
	// outside too: exactly the pre-feature argument names, carrying exactly the prompt
	// and name the (untouched) builders produce.
	la := legacy.lastLaunchArgs()
	for _, k := range []string{"agentId", "name", "prompt", "requestKey", "worktreeId"} {
		if _, ok := la[k]; !ok {
			t.Fatalf("legacy launch lost its %q argument: %s", k, want)
		}
	}
	if len(la) != 5 {
		t.Fatalf("legacy launch carries %d args, want exactly the 5 pre-feature ones: %s", len(la), want)
	}
	built := a
	built.Mode = "edit"
	if la["prompt"] != buildAgentPrompt(&built) || la["worktreeId"] != "wt-1" {
		t.Fatalf("legacy launch prompt/worktree drifted from the builders: %s", want)
	}
	// And a SUPPORTED launch is that same call plus the one flag, nothing else moved.
	flagged := &scriptMCP{connected: true, launchResult: launchOK("t"), toolList: launchCatalog(true)}
	_ = runSpawn(Deps{MCP: flagged, DB: newSagaStore(), Config: handbackOn()}, a)
	fa := flagged.lastLaunchArgs()
	if fa["handback"] != true {
		t.Fatal("precondition: the supported launch must carry handback")
	}
	delete(fa, "handback")
	if b, _ := json.Marshal(fa); string(b) != want {
		t.Fatalf("handback changed more than its own key\n got: %s\nwant: %s", b, want)
	}

	unadvertised := launchCatalog(true)
	unadvertised[1].InputSchemaProvided = false
	cases := []struct {
		name string
		mcp  *scriptMCP
		cfg  config.AppConfig
	}{
		{"host schema lacks the argument", &scriptMCP{toolList: launchCatalog(false)}, handbackOn()},
		{"host advertises no agent.launch", &scriptMCP{toolList: launchCatalog(true)[:1]}, handbackOn()},
		{"schema is the client's stand-in, not advertised", &scriptMCP{toolList: unadvertised}, handbackOn()},
		{"catalog read fails", &scriptMCP{toolListErr: errBoom("list failed")}, handbackOn()},
		{"empty catalog", &scriptMCP{}, handbackOn()},
		{"switch off on a supporting host", &scriptMCP{toolList: launchCatalog(true)}, config.AppConfig{AgentHandback: false}},
	}
	for _, tc := range cases {
		tc.mcp.connected = true
		tc.mcp.launchResult = launchOK("t")
		if res := runSpawn(Deps{MCP: tc.mcp, DB: newSagaStore(), Config: tc.cfg}, a); !res.Ok {
			t.Fatalf("%s: expected ok, got %+v", tc.name, res.Error)
		}
		if got := launchJSON(t, tc.mcp); got != want {
			t.Errorf("%s: launch args changed\n got: %s\nwant: %s", tc.name, got, want)
		}
	}
	// Off means off: not even the catalog is consulted.
	off := cases[len(cases)-1].mcp
	if len(off.listToolForce) != 0 {
		t.Errorf("switch off must not read the catalog, got %d reads", len(off.listToolForce))
	}
}

// The flag is transport, not identity: the key is a function of the prompt this CLI
// built, and Daintree appends its instruction afterwards.
func TestSpawnIdempotencyKeyIsUnchangedByHandback(t *testing.T) {
	a := handbackSpawn()
	key := func(m *scriptMCP, cfg config.AppConfig) string {
		m.connected, m.launchResult = true, launchOK("t")
		_ = runSpawn(Deps{MCP: m, DB: newSagaStore(), Config: cfg}, a)
		k, _ := m.lastLaunchArgs()["requestKey"].(string)
		return k
	}
	with := &scriptMCP{toolList: launchCatalog(true)}
	keyWith := key(with, handbackOn())
	if with.lastLaunchArgs()["handback"] != true {
		t.Fatal("precondition: this launch must carry handback")
	}
	keyWithout := key(&scriptMCP{toolList: launchCatalog(false)}, handbackOn())
	keyOff := key(&scriptMCP{toolList: launchCatalog(true)}, config.AppConfig{})
	if keyWith != keyWithout || keyWith != keyOff {
		t.Fatalf("requestKey moved with handback: with=%s without=%s off=%s", keyWith, keyWithout, keyOff)
	}
	// The pre-feature key for these exact args, pinned as a literal: equality between
	// three runs of the same code would still pass if all three had drifted together.
	const preFeatureKey = "c51cfdc574d4092a"
	if keyWith != preFeatureKey {
		t.Fatalf("requestKey = %s, want the pre-feature key %s", keyWith, preFeatureKey)
	}
}

// A cold catalog makes discovery a round trip. A cancel landing inside it must stop
// the spawn before the saga row exists, like the roster read beside it.
func TestSpawnCancelledDuringHandbackDiscoveryWritesNoSaga(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	db := newSagaStore()
	mcp := &scriptMCP{connected: true, launchResult: launchOK("t"), toolList: launchCatalog(true), onListTools: cancel}
	a := handbackSpawn()
	res := spawnMain(ctx, Deps{MCP: mcp, DB: db, Config: handbackOn()}, &a)
	if res.Ok || res.Error == nil || res.Error.Code != codeCancelled {
		t.Fatalf("expected CANCELLED, got %+v", res)
	}
	if mcp.launchCount() != 0 || len(db.launches) != 0 {
		t.Fatalf("cancelled discovery must not launch or write a saga (launches=%d rows=%d)", mcp.launchCount(), len(db.launches))
	}
}
