package scratchx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

// --- Store-level behavior ---

func TestStoreCreateReturnsScrHandle(t *testing.T) {
	s := NewStore()
	id, err := s.Create("triage")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !strings.HasPrefix(id, domain.PrefixScratch) {
		t.Errorf("store id %q lacks %q prefix", id, domain.PrefixScratch)
	}
	if len(id) != len(domain.PrefixScratch)+8 {
		t.Errorf("store id %q has unexpected length %d", id, len(id))
	}
}

func TestStoreSetGetRoundTrip(t *testing.T) {
	s := NewStore()
	id, _ := s.Create("s")
	val := json.RawMessage(`{"count":5,"items":["a","b"]}`)
	if err := s.Set(id, "state", val); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := s.Get(id, "state")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != string(val) {
		t.Errorf("Get = %s, want %s", got, val)
	}
}

func TestStoreSetUpsertOverwrites(t *testing.T) {
	s := NewStore()
	id, _ := s.Create("s")
	_ = s.Set(id, "k", json.RawMessage(`1`))
	_ = s.Set(id, "k", json.RawMessage(`2`))
	got, _ := s.Get(id, "k")
	if string(got) != "2" {
		t.Errorf("after upsert Get = %s, want 2", got)
	}
	all, _ := s.GetAll(id)
	if len(all) != 1 {
		t.Errorf("upsert grew store to %d keys, want 1", len(all))
	}
}

// TestStoreCopiesOnSet proves the store snapshots the value bytes: mutating the
// caller's buffer after Set must not change what's stored, and mutating a Get
// result must not change the store.
func TestStoreCopiesOnSet(t *testing.T) {
	s := NewStore()
	id, _ := s.Create("s")
	buf := append(json.RawMessage(nil), []byte(`{"n":1}`)...)
	_ = s.Set(id, "k", buf)
	buf[5] = '9' // mutate the caller's buffer out from under the store

	got, _ := s.Get(id, "k")
	if string(got) != `{"n":1}` {
		t.Errorf("store aliased caller buffer: Get = %s, want {\"n\":1}", got)
	}
	got[5] = '7' // mutate the returned copy
	again, _ := s.Get(id, "k")
	if string(again) != `{"n":1}` {
		t.Errorf("Get returned an aliased slice: second Get = %s", again)
	}
}

func TestStoreGetAllReturnsCopy(t *testing.T) {
	s := NewStore()
	id, _ := s.Create("s")
	_ = s.Set(id, "a", json.RawMessage(`1`))
	_ = s.Set(id, "b", json.RawMessage(`2`))
	all, err := s.GetAll(id)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("GetAll len = %d, want 2", len(all))
	}
	// Mutating the returned map must not affect the store.
	delete(all, "a")
	all["b"][0] = '9'
	again, _ := s.GetAll(id)
	if len(again) != 2 {
		t.Errorf("GetAll map mutation leaked: store has %d keys", len(again))
	}
	if string(again["b"]) != "2" {
		t.Errorf("GetAll value mutation leaked: b = %s, want 2", again["b"])
	}
}

func TestStoreDelete(t *testing.T) {
	s := NewStore()
	id, _ := s.Create("s")
	_ = s.Set(id, "k", json.RawMessage(`1`))
	if err := s.Delete(id, "k"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(id, "k"); !errors.Is(err, ErrKeyNotFound) {
		t.Errorf("after Delete, Get err = %v, want ErrKeyNotFound", err)
	}
	if err := s.Delete(id, "k"); !errors.Is(err, ErrKeyNotFound) {
		t.Errorf("double Delete err = %v, want ErrKeyNotFound", err)
	}
}

func TestStoreDrop(t *testing.T) {
	s := NewStore()
	id, _ := s.Create("s")
	_ = s.Set(id, "k", json.RawMessage(`1`))
	if err := s.Drop(id); err != nil {
		t.Fatalf("Drop: %v", err)
	}
	if _, err := s.Get(id, "k"); !errors.Is(err, ErrStoreNotFound) {
		t.Errorf("after Drop, Get err = %v, want ErrStoreNotFound", err)
	}
	if err := s.Drop(id); !errors.Is(err, ErrStoreNotFound) {
		t.Errorf("double Drop err = %v, want ErrStoreNotFound", err)
	}
}

func TestStoreNotFoundErrors(t *testing.T) {
	s := NewStore()
	if err := s.Set("scr_nope", "k", json.RawMessage(`1`)); !errors.Is(err, ErrStoreNotFound) {
		t.Errorf("Set missing store err = %v, want ErrStoreNotFound", err)
	}
	if _, err := s.Get("scr_nope", "k"); !errors.Is(err, ErrStoreNotFound) {
		t.Errorf("Get missing store err = %v, want ErrStoreNotFound", err)
	}
	if _, err := s.GetAll("scr_nope"); !errors.Is(err, ErrStoreNotFound) {
		t.Errorf("GetAll missing store err = %v, want ErrStoreNotFound", err)
	}
	if err := s.Delete("scr_nope", "k"); !errors.Is(err, ErrStoreNotFound) {
		t.Errorf("Delete missing store err = %v, want ErrStoreNotFound", err)
	}
}

// --- Bounds (store-level) ---

func TestStoreCapEnforced(t *testing.T) {
	s := NewStore()
	for i := 0; i < maxStores; i++ {
		if _, err := s.Create(fmt.Sprintf("s%d", i)); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
	}
	if _, err := s.Create("one-too-many"); !errors.Is(err, ErrStoresFull) {
		t.Errorf("Create past cap err = %v, want ErrStoresFull", err)
	}
}

func TestStoreCapFreedByDrop(t *testing.T) {
	s := NewStore()
	var first string
	for i := 0; i < maxStores; i++ {
		id, err := s.Create(fmt.Sprintf("s%d", i))
		if err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
		if i == 0 {
			first = id
		}
	}
	if err := s.Drop(first); err != nil {
		t.Fatalf("Drop: %v", err)
	}
	if _, err := s.Create("now-fits"); err != nil {
		t.Errorf("Create after Drop = %v, want nil (slot freed)", err)
	}
}

func TestKeyCapEnforced(t *testing.T) {
	s := NewStore()
	id, _ := s.Create("s")
	for i := 0; i < maxKeys; i++ {
		if err := s.Set(id, fmt.Sprintf("k%d", i), json.RawMessage(`1`)); err != nil {
			t.Fatalf("Set %d: %v", i, err)
		}
	}
	if err := s.Set(id, "one-too-many", json.RawMessage(`1`)); !errors.Is(err, ErrKeysFull) {
		t.Errorf("Set past key cap err = %v, want ErrKeysFull", err)
	}
	// Overwriting an EXISTING key at cap must still succeed (it replaces, not grows).
	if err := s.Set(id, "k0", json.RawMessage(`2`)); err != nil {
		t.Errorf("upsert existing key at cap err = %v, want nil", err)
	}
}

// --- Cross-session isolation ---

func TestStoresAreIndependent(t *testing.T) {
	a := NewStore()
	b := NewStore()
	id, _ := a.Create("s")
	_ = a.Set(id, "k", json.RawMessage(`1`))
	// b is a separate session: the handle from a must not resolve.
	if _, err := b.Get(id, "k"); !errors.Is(err, ErrStoreNotFound) {
		t.Errorf("store b saw store a's handle: err = %v, want ErrStoreNotFound", err)
	}
}

// --- Tool handler tests ---

func toolByName(t *testing.T, ts []tools.Tool, name string) tools.Tool {
	t.Helper()
	for _, tl := range ts {
		if tl.Name == name {
			return tl
		}
	}
	t.Fatalf("tool %q not found", name)
	return tools.Tool{}
}

// run drives a tool the way Dispatch does: Decode (strict + Validate) then Handle.
// It fails the test if Decode rejects the args — use decodeErr for the reject path.
func run(t *testing.T, tl tools.Tool, rawArgs string) tools.ToolResult {
	t.Helper()
	canonical, err := tl.Decode(json.RawMessage(rawArgs))
	if err != nil {
		t.Fatalf("%s Decode(%s) unexpected error: %v", tl.Name, rawArgs, err)
	}
	return tl.Handle(context.Background(), canonical, nil)
}

// decodeErr returns the Decode-phase error (the INVALID_ARGS path Dispatch maps),
// or nil if the args were accepted.
func decodeErr(tl tools.Tool, rawArgs string) error {
	_, err := tl.Decode(json.RawMessage(rawArgs))
	return err
}

func resultMap(t *testing.T, r tools.ToolResult) map[string]any {
	t.Helper()
	if !r.Ok {
		t.Fatalf("result not ok: %+v", r.Error)
	}
	m, ok := r.Result.(map[string]any)
	if !ok {
		t.Fatalf("result is %T, want map[string]any", r.Result)
	}
	return m
}

func TestToolsCount(t *testing.T) {
	if got := len(Tools(Deps{Store: NewStore()})); got != 5 {
		t.Errorf("Tools count = %d, want 5", got)
	}
}

func TestToolCreateSetGetRoundTrip(t *testing.T) {
	ts := Tools(Deps{Store: NewStore()})
	create := toolByName(t, ts, "scratch.create")
	set := toolByName(t, ts, "scratch.set")
	get := toolByName(t, ts, "scratch.get")

	cm := resultMap(t, run(t, create, `{"name":"triage"}`))
	id, _ := cm["storeId"].(string)
	if !strings.HasPrefix(id, domain.PrefixScratch) {
		t.Fatalf("create returned storeId %q", id)
	}

	// Any JSON value round-trips verbatim (object, array, string, number, bool, null).
	cases := map[string]string{
		"obj":  `{"a":1,"b":[2,3]}`,
		"arr":  `[1,2,3]`,
		"str":  `"hello"`,
		"num":  `42`,
		"bool": `true`,
		"null": `null`,
	}
	for key, val := range cases {
		run(t, set, fmt.Sprintf(`{"storeId":%q,"key":%q,"value":%s}`, id, key, val))
		gm := resultMap(t, run(t, get, fmt.Sprintf(`{"storeId":%q,"key":%q}`, id, key)))
		raw, ok := gm["value"].(json.RawMessage)
		if !ok {
			t.Fatalf("get %s value is %T, want json.RawMessage", key, gm["value"])
		}
		if !jsonEqual(t, string(raw), val) {
			t.Errorf("get %s = %s, want %s", key, raw, val)
		}
	}
}

func TestToolGetAllKeys(t *testing.T) {
	store := NewStore()
	ts := Tools(Deps{Store: store})
	get := toolByName(t, ts, "scratch.get")
	id, _ := store.Create("s")
	_ = store.Set(id, "b", json.RawMessage(`2`))
	_ = store.Set(id, "a", json.RawMessage(`1`))

	m := resultMap(t, run(t, get, fmt.Sprintf(`{"storeId":%q}`, id)))
	if count, _ := m["count"].(int); count != 2 {
		t.Errorf("count = %v, want 2", m["count"])
	}
	keys, _ := m["keys"].([]string)
	if !reflect.DeepEqual(keys, []string{"a", "b"}) {
		t.Errorf("keys = %v, want sorted [a b]", keys)
	}
	entries, ok := m["entries"].(map[string]json.RawMessage)
	if !ok || len(entries) != 2 {
		t.Errorf("entries = %v (%T), want 2-entry map", m["entries"], m["entries"])
	}
}

func TestToolDeleteAndDrop(t *testing.T) {
	store := NewStore()
	ts := Tools(Deps{Store: store})
	del := toolByName(t, ts, "scratch.delete")
	drop := toolByName(t, ts, "scratch.drop")
	id, _ := store.Create("s")
	_ = store.Set(id, "k", json.RawMessage(`1`))

	run(t, del, fmt.Sprintf(`{"storeId":%q,"key":"k"}`, id))
	if _, err := store.Get(id, "k"); !errors.Is(err, ErrKeyNotFound) {
		t.Errorf("after scratch.delete, key still present: %v", err)
	}
	run(t, drop, fmt.Sprintf(`{"storeId":%q}`, id))
	if err := store.Drop(id); !errors.Is(err, ErrStoreNotFound) {
		t.Errorf("after scratch.drop, store still present: %v", err)
	}
}

func TestToolErrorCodes(t *testing.T) {
	store := NewStore()
	ts := Tools(Deps{Store: store})
	set := toolByName(t, ts, "scratch.set")
	get := toolByName(t, ts, "scratch.get")
	del := toolByName(t, ts, "scratch.delete")
	drop := toolByName(t, ts, "scratch.drop")

	// Missing store → SCRATCH_STORE_NOT_FOUND, unrecoverable.
	r := set.Handle(context.Background(), mustCanon(t, set, `{"storeId":"scr_nope","key":"k","value":1}`), nil)
	assertFail(t, r, codeStoreNotFound, false)

	r = get.Handle(context.Background(), mustCanon(t, get, `{"storeId":"scr_nope","key":"k"}`), nil)
	assertFail(t, r, codeStoreNotFound, false)
	r = del.Handle(context.Background(), mustCanon(t, del, `{"storeId":"scr_nope","key":"k"}`), nil)
	assertFail(t, r, codeStoreNotFound, false)
	r = drop.Handle(context.Background(), mustCanon(t, drop, `{"storeId":"scr_nope"}`), nil)
	assertFail(t, r, codeStoreNotFound, false)

	// Missing key → SCRATCH_KEY_NOT_FOUND, unrecoverable.
	id, _ := store.Create("s")
	r = get.Handle(context.Background(), mustCanon(t, get, fmt.Sprintf(`{"storeId":%q,"key":"absent"}`, id)), nil)
	assertFail(t, r, codeKeyNotFound, false)
	r = del.Handle(context.Background(), mustCanon(t, del, fmt.Sprintf(`{"storeId":%q,"key":"absent"}`, id)), nil)
	assertFail(t, r, codeKeyNotFound, false)
}

func TestToolStoreFullCode(t *testing.T) {
	store := NewStore()
	ts := Tools(Deps{Store: store})
	create := toolByName(t, ts, "scratch.create")
	for i := 0; i < maxStores; i++ {
		run(t, create, fmt.Sprintf(`{"name":"s%d"}`, i))
	}
	r := create.Handle(context.Background(), mustCanon(t, create, `{"name":"too-many"}`), nil)
	// Capacity is recoverable (drop a store and retry) — NOT INVALID_ARGS.
	assertFail(t, r, codeStoreLimitFull, true)
}

func TestToolKeysFullCode(t *testing.T) {
	store := NewStore()
	ts := Tools(Deps{Store: store})
	set := toolByName(t, ts, "scratch.set")
	id, _ := store.Create("s")
	for i := 0; i < maxKeys; i++ {
		_ = store.Set(id, fmt.Sprintf("k%d", i), json.RawMessage(`1`))
	}
	r := set.Handle(context.Background(), mustCanon(t, set, fmt.Sprintf(`{"storeId":%q,"key":"too-many","value":1}`, id)), nil)
	assertFail(t, r, codeKeysLimitFull, true)
}

func TestToolNilStoreUnavailable(t *testing.T) {
	ts := Tools(Deps{Store: nil})
	for _, name := range []string{"scratch.create", "scratch.set", "scratch.get", "scratch.delete", "scratch.drop"} {
		tl := toolByName(t, ts, name)
		// A schema-valid arg set so Decode passes and we reach the nil-store guard.
		args := map[string]string{
			"scratch.create": `{"name":"s"}`,
			"scratch.set":    `{"storeId":"scr_x","key":"k","value":1}`,
			"scratch.get":    `{"storeId":"scr_x"}`,
			"scratch.delete": `{"storeId":"scr_x","key":"k"}`,
			"scratch.drop":   `{"storeId":"scr_x"}`,
		}[name]
		r := tl.Handle(context.Background(), mustCanon(t, tl, args), nil)
		assertFail(t, r, codeUnavailable, false)
	}
}

// --- Bounds via the Decode pipeline (the INVALID_ARGS path) ---

func TestDecodeRejectsBadArgs(t *testing.T) {
	ts := Tools(Deps{Store: NewStore()})
	create := toolByName(t, ts, "scratch.create")
	set := toolByName(t, ts, "scratch.set")
	get := toolByName(t, ts, "scratch.get")

	bigName := strings.Repeat("x", maxNameRunes+1)
	bigKey := strings.Repeat("k", maxKeyRunes+1)
	bigVal := `"` + strings.Repeat("v", maxValueRunes) + `"` // quotes push it over the rune cap

	cases := []struct {
		tool tools.Tool
		args string
		why  string
	}{
		{create, `{"name":"` + bigName + `"}`, "name over cap"},
		{create, `{}`, "missing required name"},
		{create, `{"name":"s","extra":1}`, "unknown field rejected"},
		{set, `{"storeId":"scr_x","key":"k"}`, "missing value"},
		{set, `{"storeId":"","key":"k","value":1}`, "empty storeId"},
		{set, `{"storeId":"scr_x","key":"","value":1}`, "empty key"},
		{set, `{"storeId":"scr_x","key":"` + bigKey + `","value":1}`, "key over cap"},
		{set, `{"storeId":"scr_x","key":"k","value":` + bigVal + `}`, "value over cap"},
		{get, `{}`, "missing required storeId"},
	}
	for _, c := range cases {
		if err := decodeErr(c.tool, c.args); err == nil {
			t.Errorf("%s: expected Decode error (%s), got nil", c.tool.Name, c.why)
		}
	}
}

func TestDecodeAcceptsValueAtCap(t *testing.T) {
	ts := Tools(Deps{Store: NewStore()})
	set := toolByName(t, ts, "scratch.set")
	// A string value of exactly maxValueRunes runes (incl. the two quotes).
	val := `"` + strings.Repeat("v", maxValueRunes-2) + `"`
	args := fmt.Sprintf(`{"storeId":"scr_x","key":"k","value":%s}`, val)
	if err := decodeErr(set, args); err != nil {
		t.Errorf("value at exactly the cap rejected: %v", err)
	}
}

// --- Race ---

// TestStoreConcurrentAccess hammers every mutating + reading path from many
// goroutines; run under -race to catch unguarded map access.
func TestStoreConcurrentAccess(t *testing.T) {
	s := NewStore()
	// Seed a few stores so set/get/delete have live targets.
	ids := make([]string, 5)
	for i := range ids {
		ids[i], _ = s.Create(fmt.Sprintf("seed%d", i))
	}

	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			id := ids[g%len(ids)]
			key := fmt.Sprintf("k%d", g)
			for i := 0; i < 50; i++ {
				_ = s.Set(id, key, json.RawMessage(fmt.Sprintf(`%d`, i)))
				_, _ = s.Get(id, key)
				_, _ = s.GetAll(id)
				_ = s.Delete(id, key)
				if i%10 == 0 {
					newID, err := s.Create(fmt.Sprintf("tmp%d-%d", g, i))
					if err == nil {
						_ = s.Drop(newID)
					}
				}
			}
		}(g)
	}
	wg.Wait()
}

// --- helpers ---

func mustCanon(t *testing.T, tl tools.Tool, raw string) json.RawMessage {
	t.Helper()
	canon, err := tl.Decode(json.RawMessage(raw))
	if err != nil {
		t.Fatalf("%s Decode(%s): %v", tl.Name, raw, err)
	}
	return canon
}

func assertFail(t *testing.T, r tools.ToolResult, wantCode string, wantRecoverable bool) {
	t.Helper()
	if r.Ok {
		t.Fatalf("result ok, want failure with code %s", wantCode)
	}
	if r.Error == nil {
		t.Fatalf("result has nil Error, want code %s", wantCode)
	}
	if r.Error.Code != wantCode {
		t.Errorf("error code = %s, want %s", r.Error.Code, wantCode)
	}
	if r.Error.Recoverable != wantRecoverable {
		t.Errorf("recoverable = %v, want %v (code %s)", r.Error.Recoverable, wantRecoverable, wantCode)
	}
}

func jsonEqual(t *testing.T, a, b string) bool {
	t.Helper()
	var av, bv any
	if err := json.Unmarshal([]byte(a), &av); err != nil {
		return false
	}
	if err := json.Unmarshal([]byte(b), &bv); err != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}
