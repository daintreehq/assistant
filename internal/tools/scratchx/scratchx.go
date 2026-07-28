// Package scratchx is the session-scoped scratch-pad tool family (scratch.create,
// scratch.set, scratch.get, scratch.delete, scratch.drop). It gives the model a
// lightweight, bounded, PURE IN-MEMORY key-value workspace for ephemeral
// within-session workflow data — a place to stash intermediate results across the
// steps of a multi-step task without polluting the conversation or touching disk.
//
// It is deliberately distinct from its two neighbours:
//   - internal/tools/memory  — DURABLE, FTS5-backed, cross-session project memory.
//   - internal/tools/artifactx — INTERNAL overflow paging the agent loop drives,
//     not the model.
//
// scratchx is the missing middle: model-driven, but session-local and disposable.
// Values are arbitrary JSON (stored as json.RawMessage so the model reads back the
// exact structure it wrote). The store lives only in process memory and vanishes
// when the session ends — zero persistence, zero cross-session leakage. Hard bounds
// (store count, keys per store, value size) stop a runaway loop from exhausting
// memory.
package scratchx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

// Hard bounds (rune counts for the text fields). A scratch pad is ephemeral
// working memory: more than a handful of named stores, or hundreds of keys, or a
// 10K-rune value, is a model bug rather than a legitimate need. These exist to cap
// worst-case memory under a runaway loop, not to constrain normal use.
const (
	maxStores     = 20     // named stores per session
	maxKeys       = 100    // keys per store
	maxValueRunes = 10_000 // size of one stored JSON value
	maxNameRunes  = 100    // store-name label
	maxKeyRunes   = 200    // key name
	maxIDRunes    = 200    // storeId handle (junk longer than this never matches)
)

// Tool-error codes. Not-found codes are Unrecoverable (you can't retry your way to
// a store/key that does not exist); the capacity codes are recoverable (the model
// can drop a store or delete a key and try again).
const (
	codeInvalidArgs    = "INVALID_ARGS"
	codeUnavailable    = "SCRATCH_UNAVAILABLE"
	codeStoreNotFound  = "SCRATCH_STORE_NOT_FOUND"
	codeKeyNotFound    = "SCRATCH_KEY_NOT_FOUND"
	codeStoreLimitFull = "SCRATCH_STORE_FULL"
	codeKeysLimitFull  = "SCRATCH_KEYS_FULL"
)

// Sentinel errors the Store methods return; handlers map them to the codes above.
var (
	ErrStoreNotFound = errors.New("scratch store not found")
	ErrKeyNotFound   = errors.New("scratch key not found")
	ErrStoresFull    = errors.New("scratch store limit reached")
	ErrKeysFull      = errors.New("scratch key limit reached")
)

// overLimit reports whether s exceeds n runes (counted, not bytes, so a multibyte
// label isn't rejected early).
func overLimit(s string, n int) bool { return len([]rune(s)) > n }

// Store is the session-scoped scratch workspace: named stores keyed by their scr_
// handle, each a flat key→JSON-value map. It is reached from the tool-dispatch
// goroutine, so every access is guarded by a single Mutex (matching the codebase's
// plain-Mutex convention — operations are fast and write-heavy, so an RWMutex buys
// nothing). One Store == one session; a fresh App builds a fresh Store, which is
// why nothing here persists or leaks across sessions.
type Store struct {
	mu     sync.Mutex
	stores map[string]*namedStore
}

// namedStore is one labelled scratch store. id is the stable scr_ handle the model
// references; name is a human-facing label only (NOT required to be unique — the id
// is the sole stable key).
type namedStore struct {
	id   string
	name string
	data map[string]json.RawMessage
}

// NewStore returns an empty scratch store.
func NewStore() *Store {
	return &Store{stores: make(map[string]*namedStore)}
}

// Create allocates a new named store and returns its scr_ handle. Fails with
// ErrStoresFull once maxStores live stores exist.
func (s *Store) Create(name string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.stores) >= maxStores {
		return "", ErrStoresFull
	}
	// Guard against the (astronomically unlikely) 8-hex-char collision so a fresh
	// empty store can never silently clobber a live one with the same handle.
	id := domain.NewID(domain.PrefixScratch)
	for {
		if _, exists := s.stores[id]; !exists {
			break
		}
		id = domain.NewID(domain.PrefixScratch)
	}
	s.stores[id] = &namedStore{id: id, name: name, data: make(map[string]json.RawMessage)}
	return id, nil
}

// Set upserts key→val in the store. A NEW key is rejected once the store holds
// maxKeys; overwriting an EXISTING key is always allowed (it replaces, it doesn't
// grow the store). The value bytes are copied because a json.RawMessage handed in
// by the decoder can alias the caller's input buffer — the store must own a stable
// snapshot the caller can't mutate out from under it.
func (s *Store) Set(id, key string, val json.RawMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.stores[id]
	if !ok {
		return ErrStoreNotFound
	}
	if _, exists := st.data[key]; !exists && len(st.data) >= maxKeys {
		return ErrKeysFull
	}
	st.data[key] = append(json.RawMessage(nil), val...)
	return nil
}

// Get returns a defensive copy of one key's value. ErrStoreNotFound / ErrKeyNotFound
// distinguish a missing store from a missing key.
func (s *Store) Get(id, key string) (json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.stores[id]
	if !ok {
		return nil, ErrStoreNotFound
	}
	val, ok := st.data[key]
	if !ok {
		return nil, ErrKeyNotFound
	}
	return append(json.RawMessage(nil), val...), nil
}

// GetAll returns a defensive copy of every key→value in the store (handlers must
// not be able to mutate internal state through the returned map).
func (s *Store) GetAll(id string) (map[string]json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.stores[id]
	if !ok {
		return nil, ErrStoreNotFound
	}
	out := make(map[string]json.RawMessage, len(st.data))
	for k, v := range st.data {
		out[k] = append(json.RawMessage(nil), v...)
	}
	return out, nil
}

// Delete removes one key. ErrStoreNotFound / ErrKeyNotFound distinguish the two
// miss cases so the handler can report which one was wrong.
func (s *Store) Delete(id, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.stores[id]
	if !ok {
		return ErrStoreNotFound
	}
	if _, ok := st.data[key]; !ok {
		return ErrKeyNotFound
	}
	delete(st.data, key)
	return nil
}

// Drop discards an entire named store and its contents.
func (s *Store) Drop(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.stores[id]; !ok {
		return ErrStoreNotFound
	}
	delete(s.stores, id)
	return nil
}

// Deps wires the family to its session-scoped store. Store may be nil (tests, or a
// non-session context) → handlers return SCRATCH_UNAVAILABLE rather than panic.
type Deps struct {
	Store *Store
}

// Tools returns the scratch tool family.
func Tools(deps Deps) []tools.Tool {
	return []tools.Tool{
		newCreateTool(deps),
		newSetTool(deps),
		newGetTool(deps),
		newDeleteTool(deps),
		newDropTool(deps),
	}
}

// --- scratch.create ---

type createArgs struct {
	Name string `json:"name"`
}

func (a *createArgs) Validate() error {
	if a.Name == "" {
		return fmt.Errorf("name is required")
	}
	if overLimit(a.Name, maxNameRunes) {
		return fmt.Errorf("name must be at most %d characters", maxNameRunes)
	}
	return nil
}

var createSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["name"],
  "properties": {
    "name": { "type": "string", "minLength": 1, "description": "A short label for this scratch store (e.g. \"pr-triage\"). For your reference only; not required to be unique." }
  }
}`)

func newCreateTool(deps Deps) tools.Tool {
	return tools.Tool{
		Name: "scratch.create",
		Description: "Create a named in-memory scratch store for ephemeral within-session data and return its storeId. " +
			"Use scratch.* tools as a temporary key-value workspace to track intermediate results across the steps of a multi-step task. " +
			"Scratch data lives only for THIS session and is never persisted — for durable cross-session notes use memory.save instead. " +
			"Keep the returned storeId; you pass it to scratch.set/get/delete/drop.",
		Risk:   domain.RiskLocal,
		Schema: createSchema,
		Decode: tools.StrictDecoder(func() any { return &createArgs{} }),
		Handle: func(_ context.Context, args json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a createArgs
			if err := tools.DecodeStrict(args, &a); err != nil {
				return tools.Fail(codeInvalidArgs, "Invalid arguments for scratch.create: "+err.Error())
			}
			if deps.Store == nil {
				return unavailable("scratch.create")
			}
			id, err := deps.Store.Create(a.Name)
			if err != nil {
				if errors.Is(err, ErrStoresFull) {
					return tools.Fail(codeStoreLimitFull,
						fmt.Sprintf("Scratch store limit reached (%d stores). Drop an unused store with scratch.drop before creating another.", maxStores))
				}
				return tools.Fail(domain.CodeInternal, "scratch.create: "+err.Error())
			}
			return tools.Ok("Created scratch store "+id+" ("+a.Name+").",
				map[string]any{"storeId": id, "name": a.Name})
		},
	}
}

// --- scratch.set ---

type setArgs struct {
	StoreID string          `json:"storeId"`
	Key     string          `json:"key"`
	Value   json.RawMessage `json:"value"`
}

// Validate enforces the static shape: storeId/key present and within bounds, and a
// value that is present (a json.RawMessage of length 0 means the field was omitted;
// an explicit JSON `null` is 4 bytes and IS a valid value to store) and within the
// rune cap. Capacity (store/keys full) is a runtime condition checked in the store.
func (a *setArgs) Validate() error {
	if a.StoreID == "" {
		return fmt.Errorf("storeId is required")
	}
	if overLimit(a.StoreID, maxIDRunes) {
		return fmt.Errorf("storeId must be at most %d characters", maxIDRunes)
	}
	if a.Key == "" {
		return fmt.Errorf("key is required")
	}
	if overLimit(a.Key, maxKeyRunes) {
		return fmt.Errorf("key must be at most %d characters", maxKeyRunes)
	}
	if len(a.Value) == 0 {
		return fmt.Errorf("value is required")
	}
	if overLimit(string(a.Value), maxValueRunes) {
		return fmt.Errorf("value must be at most %d characters", maxValueRunes)
	}
	return nil
}

var setSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["storeId", "key", "value"],
  "properties": {
    "storeId": { "type": "string", "description": "The storeId returned by scratch.create." },
    "key": { "type": "string", "minLength": 1, "description": "The key to write (upsert — overwrites if it already exists)." },
    "value": { "description": "Any JSON value to store: object, array, string, number, boolean, or null. Pass it directly as JSON — do NOT wrap it in a string." }
  }
}`)

func newSetTool(deps Deps) tools.Tool {
	return tools.Tool{
		Name: "scratch.set",
		Description: "Write a key/value into a scratch store (upsert — overwrites an existing key). " +
			"The value is any JSON (object, array, string, number, boolean, null) and is read back verbatim by scratch.get.",
		Risk:   domain.RiskLocal,
		Schema: setSchema,
		Decode: tools.StrictDecoder(func() any { return &setArgs{} }),
		Handle: func(_ context.Context, args json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a setArgs
			if err := tools.DecodeStrict(args, &a); err != nil {
				return tools.Fail(codeInvalidArgs, "Invalid arguments for scratch.set: "+err.Error())
			}
			if deps.Store == nil {
				return unavailable("scratch.set")
			}
			err := deps.Store.Set(a.StoreID, a.Key, a.Value)
			switch {
			case err == nil:
				return tools.Ok(fmt.Sprintf("Set %q in %s.", a.Key, a.StoreID),
					map[string]any{"storeId": a.StoreID, "key": a.Key})
			case errors.Is(err, ErrStoreNotFound):
				return notFoundStore("scratch.set", a.StoreID)
			case errors.Is(err, ErrKeysFull):
				return tools.Fail(codeKeysLimitFull,
					fmt.Sprintf("Scratch store %s is full (%d keys). Delete a key with scratch.delete before adding another.", a.StoreID, maxKeys))
			default:
				return tools.Fail(domain.CodeInternal, "scratch.set: "+err.Error())
			}
		},
	}
}

// --- scratch.get ---

type getArgs struct {
	StoreID string `json:"storeId"`
	Key     string `json:"key,omitempty"`
}

func (a *getArgs) Validate() error {
	if a.StoreID == "" {
		return fmt.Errorf("storeId is required")
	}
	if overLimit(a.StoreID, maxIDRunes) {
		return fmt.Errorf("storeId must be at most %d characters", maxIDRunes)
	}
	if overLimit(a.Key, maxKeyRunes) {
		return fmt.Errorf("key must be at most %d characters", maxKeyRunes)
	}
	return nil
}

var getSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["storeId"],
  "properties": {
    "storeId": { "type": "string", "description": "The storeId returned by scratch.create." },
    "key": { "type": "string", "description": "The key to read. Omit to return ALL keys and values in the store." }
  }
}`)

func newGetTool(deps Deps) tools.Tool {
	return tools.Tool{
		Name: "scratch.get",
		Description: "Read from a scratch store. Pass a key to read one value, or omit key to return every key and value in the store. " +
			"Values come back as the exact JSON that was stored.",
		Risk:   domain.RiskRead,
		Schema: getSchema,
		Decode: tools.StrictDecoder(func() any { return &getArgs{} }),
		Handle: func(_ context.Context, args json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a getArgs
			if err := tools.DecodeStrict(args, &a); err != nil {
				return tools.Fail(codeInvalidArgs, "Invalid arguments for scratch.get: "+err.Error())
			}
			if deps.Store == nil {
				return unavailable("scratch.get")
			}
			// A non-empty key means single-value read. An empty key can never name a
			// real entry (scratch.set rejects empty keys), so it unambiguously means
			// "return the whole store".
			if a.Key != "" {
				val, err := deps.Store.Get(a.StoreID, a.Key)
				switch {
				case err == nil:
					return tools.Ok(fmt.Sprintf("Read %q from %s.", a.Key, a.StoreID),
						map[string]any{"storeId": a.StoreID, "key": a.Key, "value": val})
				case errors.Is(err, ErrStoreNotFound):
					return notFoundStore("scratch.get", a.StoreID)
				case errors.Is(err, ErrKeyNotFound):
					return tools.Fail(codeKeyNotFound,
						fmt.Sprintf("scratch.get: no key %q in store %s.", a.Key, a.StoreID), tools.Unrecoverable())
				default:
					return tools.Fail(domain.CodeInternal, "scratch.get: "+err.Error())
				}
			}
			entries, err := deps.Store.GetAll(a.StoreID)
			if err != nil {
				if errors.Is(err, ErrStoreNotFound) {
					return notFoundStore("scratch.get", a.StoreID)
				}
				return tools.Fail(domain.CodeInternal, "scratch.get: "+err.Error())
			}
			return tools.Ok(fmt.Sprintf("Read %d key(s) from %s.", len(entries), a.StoreID),
				map[string]any{"storeId": a.StoreID, "entries": entries, "keys": sortedKeys(entries), "count": len(entries)})
		},
	}
}

// --- scratch.delete ---

type deleteArgs struct {
	StoreID string `json:"storeId"`
	Key     string `json:"key"`
}

func (a *deleteArgs) Validate() error {
	if a.StoreID == "" {
		return fmt.Errorf("storeId is required")
	}
	if overLimit(a.StoreID, maxIDRunes) {
		return fmt.Errorf("storeId must be at most %d characters", maxIDRunes)
	}
	if a.Key == "" {
		return fmt.Errorf("key is required")
	}
	if overLimit(a.Key, maxKeyRunes) {
		return fmt.Errorf("key must be at most %d characters", maxKeyRunes)
	}
	return nil
}

var deleteSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["storeId", "key"],
  "properties": {
    "storeId": { "type": "string", "description": "The storeId returned by scratch.create." },
    "key": { "type": "string", "minLength": 1, "description": "The key to delete." }
  }
}`)

func newDeleteTool(deps Deps) tools.Tool {
	return tools.Tool{
		Name:        "scratch.delete",
		Description: "Delete ONE key from a scratch store (storeId + key). Use it to drop an intermediate result you no longer need, including to free a slot when a store hits its 100-key limit (SCRATCH_KEYS_FULL). A missing key fails SCRATCH_KEY_NOT_FOUND and a missing store SCRATCH_STORE_NOT_FOUND, both unrecoverable. To discard the whole store instead, use scratch.drop.",
		Risk:        domain.RiskLocal,
		Schema:      deleteSchema,
		Decode:      tools.StrictDecoder(func() any { return &deleteArgs{} }),
		Handle: func(_ context.Context, args json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a deleteArgs
			if err := tools.DecodeStrict(args, &a); err != nil {
				return tools.Fail(codeInvalidArgs, "Invalid arguments for scratch.delete: "+err.Error())
			}
			if deps.Store == nil {
				return unavailable("scratch.delete")
			}
			err := deps.Store.Delete(a.StoreID, a.Key)
			switch {
			case err == nil:
				return tools.Ok(fmt.Sprintf("Deleted %q from %s.", a.Key, a.StoreID),
					map[string]any{"storeId": a.StoreID, "key": a.Key})
			case errors.Is(err, ErrStoreNotFound):
				return notFoundStore("scratch.delete", a.StoreID)
			case errors.Is(err, ErrKeyNotFound):
				return tools.Fail(codeKeyNotFound,
					fmt.Sprintf("scratch.delete: no key %q in store %s.", a.Key, a.StoreID), tools.Unrecoverable())
			default:
				return tools.Fail(domain.CodeInternal, "scratch.delete: "+err.Error())
			}
		},
	}
}

// --- scratch.drop ---

type dropArgs struct {
	StoreID string `json:"storeId"`
}

func (a *dropArgs) Validate() error {
	if a.StoreID == "" {
		return fmt.Errorf("storeId is required")
	}
	if overLimit(a.StoreID, maxIDRunes) {
		return fmt.Errorf("storeId must be at most %d characters", maxIDRunes)
	}
	return nil
}

var dropSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["storeId"],
  "properties": {
    "storeId": { "type": "string", "description": "The storeId returned by scratch.create." }
  }
}`)

func newDropTool(deps Deps) tools.Tool {
	return tools.Tool{
		Name:        "scratch.drop",
		Description: "Discard an ENTIRE scratch store and every key in it, freeing one of the 20 per-session store slots (drop a finished store when scratch.create returns SCRATCH_STORE_FULL). Unknown storeId ⇒ SCRATCH_STORE_NOT_FOUND (unrecoverable). Scratch data is in-memory and session-only so nothing durable is lost — but there is no undo. To remove a single key instead, use scratch.delete.",
		Risk:        domain.RiskLocal,
		Schema:      dropSchema,
		Decode:      tools.StrictDecoder(func() any { return &dropArgs{} }),
		Handle: func(_ context.Context, args json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a dropArgs
			if err := tools.DecodeStrict(args, &a); err != nil {
				return tools.Fail(codeInvalidArgs, "Invalid arguments for scratch.drop: "+err.Error())
			}
			if deps.Store == nil {
				return unavailable("scratch.drop")
			}
			err := deps.Store.Drop(a.StoreID)
			switch {
			case err == nil:
				return tools.Ok("Dropped scratch store "+a.StoreID+".",
					map[string]any{"storeId": a.StoreID})
			case errors.Is(err, ErrStoreNotFound):
				return notFoundStore("scratch.drop", a.StoreID)
			default:
				return tools.Fail(domain.CodeInternal, "scratch.drop: "+err.Error())
			}
		},
	}
}

// --- shared failure helpers ---

// unavailable is the SCRATCH_UNAVAILABLE result for a nil store (no session
// context). Unrecoverable — retrying can't conjure a store that isn't wired.
func unavailable(tool string) tools.ToolResult {
	return tools.Fail(codeUnavailable,
		tool+": scratch storage is not available in this context.", tools.Unrecoverable())
}

// notFoundStore is the SCRATCH_STORE_NOT_FOUND result. Unrecoverable — a stale or
// wrong handle won't start resolving on retry; the model must create a store first.
func notFoundStore(tool, id string) tools.ToolResult {
	return tools.Fail(codeStoreNotFound,
		fmt.Sprintf("%s: no scratch store with id %q. Scratch stores live only for the current session; create one with scratch.create.", tool, id),
		tools.Unrecoverable())
}

// sortedKeys returns the entry keys in deterministic order so a whole-store read is
// stable across calls (Go map iteration order is randomized).
func sortedKeys(entries map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(entries))
	for k := range entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
