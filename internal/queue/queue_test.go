package queue

import (
	"context"
	"sort"
	"sync"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// memStore is an in-memory EventStore that faithfully reproduces the SQL
// semantics the real store must implement: the ATOMIC
// dedupe-or-insert (newest open non-expired row, bump without touching createdAt,
// prior snapshot returned), notifiedAt re-arm, and the digest filter+order. A
// mutex makes the upsert atomic so concurrent-publish tests are meaningful. It
// lets us test the trickiest pure logic without SQLite.
type memStore struct {
	mu       sync.Mutex
	rows     map[string]*domain.QueueEvent
	order    []string         // insertion order, so "newest by createdAt DESC" can break ties
	notified map[string]int64 // QueueEvent has no notifiedAt column, so track it out-of-band
}

func newMemStore() *memStore {
	return &memStore{rows: map[string]*domain.QueueEvent{}, notified: map[string]int64{}}
}

// findOpenByDedupe returns the newest open non-expired row for a key (caller holds
// the lock). Matches ORDER BY COALESCE(updatedAt,createdAt) DESC.
func (m *memStore) findOpenByDedupe(key string, now int64) *domain.QueueEvent {
	var best *domain.QueueEvent
	for _, id := range m.order {
		r := m.rows[id]
		if r.DedupeKey != key || r.ResolvedAt != nil {
			continue
		}
		if r.ExpiresAt != nil && *r.ExpiresAt <= now {
			continue
		}
		if best == nil || coalesce(*r) >= coalesce(*best) {
			best = r
		}
	}
	return best
}

// UpsertEvent is the atomic dedupe-or-insert. Held under m.mu for the whole
// lookup+mutate so two concurrent same-key publishes serialize (the second sees
// the first's row and bumps it rather than double-inserting).
func (m *memStore) UpsertEvent(_ context.Context, args domain.QueuePublishArgs, now int64) (domain.QueueEvent, *domain.QueueEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var expiresAt *int64
	if args.TTLMs != nil && *args.TTLMs != 0 {
		e := now + *args.TTLMs
		expiresAt = &e
	}

	if args.DedupeKey != "" {
		if existing := m.findOpenByDedupe(args.DedupeKey, now); existing != nil {
			prior := *existing // pre-bump snapshot
			existing.Count++
			existing.Title = args.Title
			existing.Summary = args.Summary
			existing.Severity = args.Severity
			// evidence/epistemicKind fall back to existing when omitted.
			if len(args.Evidence) > 0 {
				existing.Evidence = args.Evidence
			}
			if args.EpistemicKind != "" {
				existing.EpistemicKind = args.EpistemicKind
			}
			// recommendedActions overwritten outright (nil clears).
			existing.RecommendedActions = args.RecommendedActions
			ua := now
			existing.UpdatedAt = &ua
			existing.ExpiresAt = expiresAt
			// createdAt intentionally untouched.
			return *existing, &prior, nil
		}
	}

	ev := domain.QueueEvent{
		ID:                 domain.NewID(domain.PrefixEvent),
		Source:             args.Source,
		Severity:           args.Severity,
		Title:              args.Title,
		Summary:            args.Summary,
		Target:             args.Target,
		Evidence:           args.Evidence,
		RecommendedActions: args.RecommendedActions,
		DedupeKey:          args.DedupeKey,
		EpistemicKind:      args.EpistemicKind,
		CreatedAt:          now,
		UpdatedAt:          &now,
		ExpiresAt:          expiresAt,
		Count:              1,
	}
	cp := ev
	m.rows[ev.ID] = &cp
	m.order = append(m.order, ev.ID)
	return ev, nil, nil
}

func (m *memStore) ClearNotified(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.notified, id)
	return nil
}

func (m *memStore) ListEvents(_ context.Context, opts domain.QueueDigestOptions, now int64) ([]domain.QueueEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.QueueEvent
	for _, id := range m.order {
		r := m.rows[id]
		if r.ExpiresAt != nil && *r.ExpiresAt <= now {
			continue
		}
		if !opts.IncludeResolved && r.ResolvedAt != nil {
			continue
		}
		if opts.NotifiedIsNull {
			if _, ok := m.notified[id]; ok {
				continue
			}
		}
		if opts.SeverityAtLeast != nil && domain.RankOf(r.Severity) < domain.RankOf(*opts.SeverityAtLeast) {
			continue
		}
		out = append(out, *r)
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := domain.RankOf(out[i].Severity), domain.RankOf(out[j].Severity)
		if ri != rj {
			return ri > rj // severity rank DESC
		}
		return coalesce(out[i]) > coalesce(out[j]) // recency DESC
	})
	if opts.MaxItems != nil && *opts.MaxItems < len(out) {
		out = out[:*opts.MaxItems]
	}
	return out, nil
}

// MarkNotified mirrors the storage contract: VERSION-CONDITIONAL — a row is
// stamped only while it still matches the digested snapshot (same count and
// coalesced updated-at, not already notified).
func (m *memStore) MarkNotified(_ context.Context, evs []domain.QueueEvent, ts int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range evs {
		r, ok := m.rows[e.ID]
		if !ok {
			continue
		}
		if _, already := m.notified[e.ID]; already {
			continue
		}
		if r.Count != e.Count || coalesce(*r) != coalesce(e) {
			continue // the publisher moved the row on since the digest
		}
		m.notified[e.ID] = ts
	}
	return nil
}

func (m *memStore) ResolveEvent(_ context.Context, id string, now int64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[id]
	if !ok || r.ResolvedAt != nil {
		return false, nil
	}
	t := now
	r.ResolvedAt = &t
	return true, nil
}

func coalesce(e domain.QueueEvent) int64 {
	if e.UpdatedAt != nil {
		return *e.UpdatedAt
	}
	return e.CreatedAt
}

func TestPublishDedupeDoesNotBumpCreatedAt(t *testing.T) {
	clock := int64(1000)
	q := New(newMemStore(), func() int64 { return clock })
	ctx := context.Background()

	first, err := q.Publish(ctx, domain.QueuePublishArgs{
		Source: domain.SourceTerminalWatcher, Severity: domain.SeverityAttention,
		Title: "still working", Summary: "agent busy", DedupeKey: "term-7",
		Evidence: []string{"tail-a"}, EpistemicKind: domain.EpistemicObserved,
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Count != 1 || first.CreatedAt != 1000 {
		t.Fatalf("first publish: count=%d createdAt=%d", first.Count, first.CreatedAt)
	}

	clock = 5000 // time advances before the dedupe bump
	second, err := q.Publish(ctx, domain.QueuePublishArgs{
		Source: domain.SourceTerminalWatcher, Severity: domain.SeverityUrgent,
		Title: "completed", Summary: "agent done", DedupeKey: "term-7",
		// no evidence / epistemicKind ⇒ must fall back to existing.
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Fatalf("dedupe should reuse id: %s vs %s", second.ID, first.ID)
	}
	if second.Count != 2 {
		t.Fatalf("count should bump to 2, got %d", second.Count)
	}
	if second.CreatedAt != 1000 {
		t.Fatalf("createdAt MUST stay pinned at 1000, got %d", second.CreatedAt)
	}
	if second.UpdatedAt == nil || *second.UpdatedAt != 5000 {
		t.Fatalf("updatedAt must advance to 5000, got %v", second.UpdatedAt)
	}
	if second.Title != "completed" || second.Severity != domain.SeverityUrgent {
		t.Fatalf("title/severity must refresh, got %q/%s", second.Title, second.Severity)
	}
	// evidence + epistemicKind fall back to the first publish's values.
	if len(second.Evidence) != 1 || second.Evidence[0] != "tail-a" {
		t.Fatalf("evidence should fall back to existing, got %v", second.Evidence)
	}
	if second.EpistemicKind != domain.EpistemicObserved {
		t.Fatalf("epistemicKind should fall back to existing, got %s", second.EpistemicKind)
	}
}

func TestPublishDedupeOverwritesRecommendedActions(t *testing.T) {
	q := New(newMemStore(), func() int64 { return 1 })
	ctx := context.Background()
	_, _ = q.Publish(ctx, domain.QueuePublishArgs{
		Source: domain.SourceTerminalWatcher, Severity: domain.SeverityAttention,
		Title: "t", Summary: "s", DedupeKey: "k",
		RecommendedActions: []domain.RecommendedAction{{Label: "Focus", ToolName: "terminal.focus"}},
	})
	// second publish carries NO actions ⇒ they must be cleared, not retained.
	got, err := q.Publish(ctx, domain.QueuePublishArgs{
		Source: domain.SourceTerminalWatcher, Severity: domain.SeverityAttention,
		Title: "t2", Summary: "s2", DedupeKey: "k",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.RecommendedActions != nil {
		t.Fatalf("recommendedActions should be cleared on dedupe with none, got %v", got.RecommendedActions)
	}
}

func TestDigestSeverityOrderingAndFilter(t *testing.T) {
	clock := int64(100)
	q := New(newMemStore(), func() int64 { return clock })
	ctx := context.Background()

	// Publish in a deliberately scrambled severity order, each at a distinct time.
	specs := []struct {
		sev   domain.Severity
		title string
	}{
		{domain.SeverityInfo, "info"},
		{domain.SeverityError, "error"},
		{domain.SeverityDebug, "debug"},
		{domain.SeverityUrgent, "urgent"},
		{domain.SeverityDone, "done"},
		{domain.SeverityBlocked, "blocked"},
		{domain.SeverityAttention, "attention"},
	}
	for _, s := range specs {
		clock++
		if _, err := q.Publish(ctx, domain.QueuePublishArgs{
			Source: domain.SourceSystem, Severity: s.sev, Title: s.title, Summary: s.title,
		}); err != nil {
			t.Fatal(err)
		}
	}

	all, err := q.Digest(ctx, domain.QueueDigestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Expect rank DESC: error(6) urgent(5) blocked(4) attention(3) done(2) info(1) debug(0).
	wantOrder := []string{"error", "urgent", "blocked", "attention", "done", "info", "debug"}
	if len(all) != len(wantOrder) {
		t.Fatalf("want %d events, got %d", len(wantOrder), len(all))
	}
	for i, w := range wantOrder {
		if all[i].Title != w {
			t.Fatalf("order[%d]: want %q got %q (full: %v)", i, w, all[i].Title, titles(all))
		}
	}

	// severityAtLeast=attention (rank 3) ⇒ keep error,urgent,blocked,attention.
	atLeast := domain.SeverityAttention
	filtered, err := q.Digest(ctx, domain.QueueDigestOptions{SeverityAtLeast: &atLeast})
	if err != nil {
		t.Fatal(err)
	}
	wantFiltered := []string{"error", "urgent", "blocked", "attention"}
	if got := titles(filtered); !equal(got, wantFiltered) {
		t.Fatalf("severityAtLeast filter: want %v got %v", wantFiltered, got)
	}

	// maxItems caps after ordering.
	max := 2
	capped, err := q.Digest(ctx, domain.QueueDigestOptions{MaxItems: &max})
	if err != nil {
		t.Fatal(err)
	}
	if got := titles(capped); !equal(got, []string{"error", "urgent"}) {
		t.Fatalf("maxItems: want [error urgent] got %v", got)
	}
}

func TestDigestExcludesExpiredAndResolved(t *testing.T) {
	clock := int64(1000)
	q := New(newMemStore(), func() int64 { return clock })
	ctx := context.Background()

	ttl := int64(500)
	expiring, _ := q.Publish(ctx, domain.QueuePublishArgs{
		Source: domain.SourceTimer, Severity: domain.SeverityInfo,
		Title: "expiring", Summary: "s", TTLMs: &ttl,
	})
	live, _ := q.Publish(ctx, domain.QueuePublishArgs{
		Source: domain.SourceTimer, Severity: domain.SeverityInfo, Title: "live", Summary: "s",
	})

	clock = 2000 // past the expiry (1000+500)
	open, err := q.Digest(ctx, domain.QueueDigestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := titles(open); !equal(got, []string{"live"}) {
		t.Fatalf("expired event must be excluded, got %v", got)
	}
	_ = expiring

	// resolve the live one ⇒ default digest drops it, includeResolved keeps it.
	changed, err := q.Resolve(ctx, live.ID)
	if err != nil || !changed {
		t.Fatalf("resolve: changed=%v err=%v", changed, err)
	}
	again, _ := q.Resolve(ctx, live.ID)
	if again {
		t.Fatal("second resolve should report no change")
	}
	open, _ = q.Digest(ctx, domain.QueueDigestOptions{})
	if len(open) != 0 {
		t.Fatalf("resolved event must be excluded by default, got %v", titles(open))
	}
	withResolved, _ := q.Digest(ctx, domain.QueueDigestOptions{IncludeResolved: true})
	if got := titles(withResolved); !equal(got, []string{"live"}) {
		t.Fatalf("includeResolved should surface live, got %v", got)
	}
}

func TestMarkNotifiedAndNotifiedIsNullFilter(t *testing.T) {
	q := New(newMemStore(), func() int64 { return 1 })
	ctx := context.Background()
	a, _ := q.Publish(ctx, domain.QueuePublishArgs{Source: domain.SourceSystem, Severity: domain.SeverityInfo, Title: "a", Summary: "s"})
	_, _ = q.Publish(ctx, domain.QueuePublishArgs{Source: domain.SourceSystem, Severity: domain.SeverityInfo, Title: "b", Summary: "s"})

	if err := q.MarkNotified(ctx, []domain.QueueEvent{a}); err != nil {
		t.Fatal(err)
	}
	unnotified, err := q.Digest(ctx, domain.QueueDigestOptions{NotifiedIsNull: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := titles(unnotified); !equal(got, []string{"b"}) {
		t.Fatalf("notifiedIsNull should drop the notified one, got %v", got)
	}
	// empty input is a no-op (must not error).
	if err := q.MarkNotified(ctx, nil); err != nil {
		t.Fatalf("empty markNotified should be a no-op, got %v", err)
	}
}

func TestFormat(t *testing.T) {
	if got := Format(nil); got != "Inbox is empty." {
		t.Fatalf("empty inbox: %q", got)
	}
	ev := domain.QueueEvent{
		ID: "evt_abc12345", Severity: domain.SeverityBlocked, Title: "merge conflict",
		Summary: "rebase needed", Count: 3,
		Target:   &domain.EventTarget{TerminalID: "term-9", WorktreeID: "wt-1"},
		Evidence: []string{"e1", "e2"},
	}
	got := Format([]domain.QueueEvent{ev})
	want := "  ⛔ evt_abc12345 merge conflict [term term-9] (×3)\n     rebase needed\n     evidence: e1 | e2"
	if got != want {
		t.Fatalf("format mismatch:\n got: %q\nwant: %q", got, want)
	}

	// worktree-only target falls back to [wt ...]; unknown severity ⇒ info glyph.
	ev2 := domain.QueueEvent{
		ID: "evt_def", Severity: domain.Severity("weird"), Title: "t", Summary: "s", Count: 1,
		Target: &domain.EventTarget{WorktreeID: "wt-2"},
	}
	got2 := Format([]domain.QueueEvent{ev2})
	want2 := "  ℹ evt_def t [wt wt-2]\n     s"
	if got2 != want2 {
		t.Fatalf("format2 mismatch:\n got: %q\nwant: %q", got2, want2)
	}
}

// TestPublishConcurrentDedupeNoDoubleInsert hammers Publish with N concurrent
// same-key publishes and asserts the atomic upsert collapses them into ONE row
// (count == N), never a second insert. Before routing Publish through the atomic
// UpsertEvent, the FindOpenByDedupe-then-Insert split let two publishers both miss
// the lookup and double-insert.
func TestPublishConcurrentDedupeNoDoubleInsert(t *testing.T) {
	store := newMemStore()
	q := New(store, func() int64 { return 1 })
	ctx := context.Background()

	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := q.Publish(ctx, domain.QueuePublishArgs{
				Source: domain.SourceTerminalWatcher, Severity: domain.SeverityAttention,
				Title: "t", Summary: "s", DedupeKey: "race-key",
			})
			if err != nil {
				t.Errorf("publish: %v", err)
			}
		}()
	}
	wg.Wait()

	// Exactly one row for the key, with count == n.
	store.mu.Lock()
	rows := 0
	var only *domain.QueueEvent
	for _, id := range store.order {
		if store.rows[id].DedupeKey == "race-key" {
			rows++
			only = store.rows[id]
		}
	}
	store.mu.Unlock()
	if rows != 1 {
		t.Fatalf("concurrent same-key publishes must collapse to ONE row, got %d", rows)
	}
	if only.Count != n {
		t.Fatalf("the surviving row should count every publish, got count=%d want %d", only.Count, n)
	}
}

// TestPublishRearmsOnMaterialChange verifies the notification re-arm: a bump that
// escalates severity (or changes title/summary) clears notifiedAt so the next
// notify() re-delivers, while an IDENTICAL bump leaves a notified event suppressed.
func TestPublishRearmsOnMaterialChange(t *testing.T) {
	clock := int64(1000)
	store := newMemStore()
	q := New(store, func() int64 { return clock })
	ctx := context.Background()

	first, _ := q.Publish(ctx, domain.QueuePublishArgs{
		Source: domain.SourceTerminalWatcher, Severity: domain.SeverityAttention,
		Title: "w: waiting", Summary: "agent waiting", DedupeKey: "k",
	})
	// Notifier delivered it.
	if err := q.MarkNotified(ctx, []domain.QueueEvent{first}); err != nil {
		t.Fatal(err)
	}
	unnotified, _ := q.Digest(ctx, domain.QueueDigestOptions{NotifiedIsNull: true})
	if len(unnotified) != 0 {
		t.Fatalf("after markNotified the event must be suppressed, got %d", len(unnotified))
	}

	// Identical bump: same severity/title/summary ⇒ NOT material ⇒ stays suppressed.
	clock = 2000
	_, _ = q.Publish(ctx, domain.QueuePublishArgs{
		Source: domain.SourceTerminalWatcher, Severity: domain.SeverityAttention,
		Title: "w: waiting", Summary: "agent waiting", DedupeKey: "k",
	})
	unnotified, _ = q.Digest(ctx, domain.QueueDigestOptions{NotifiedIsNull: true})
	if len(unnotified) != 0 {
		t.Fatalf("an identical bump must NOT re-arm; want suppressed, got %d", len(unnotified))
	}

	// Severity escalation (attention → blocked): material ⇒ re-armed ⇒ re-surfaces.
	clock = 3000
	bumped, _ := q.Publish(ctx, domain.QueuePublishArgs{
		Source: domain.SourceTerminalWatcher, Severity: domain.SeverityBlocked,
		Title: "w: merge conflict", Summary: "rebase needed", DedupeKey: "k",
	})
	if bumped.ID != first.ID {
		t.Fatalf("escalation should bump the same row, got %s vs %s", bumped.ID, first.ID)
	}
	unnotified, _ = q.Digest(ctx, domain.QueueDigestOptions{NotifiedIsNull: true})
	if len(unnotified) != 1 || unnotified[0].ID != first.ID {
		t.Fatalf("a material escalation must re-arm the event, got %v", titles(unnotified))
	}
}

func titles(evs []domain.QueueEvent) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Title
	}
	return out
}

func equal(a, b []string) bool {
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
