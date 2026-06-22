package app

import (
	"sync"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// tier_race_test.go locks in the Config.Tier data race fix: /permissions mutates
// Config.Tier from the UI goroutine while buildContext (whole-Config copy) and
// PromptContext read it on agent/tool goroutines. With the cfgMu guard + the
// SetTier/Tier/snapshotConfig accessors, a `-race` run over concurrent writes and
// reads must be clean.
func TestAppTier_ConcurrentSetAndRead_NoRace(t *testing.T) {
	a := newOfflineApp(t)
	t.Cleanup(func() { _ = a.Shutdown() })

	tiers := []domain.Tier{domain.TierSupervisor, domain.TierOperator, domain.TierSystem}

	var readers, writer sync.WaitGroup
	stop := make(chan struct{})

	// Writer: continuously cycle the tier (the /permissions write path).
	writer.Add(1)
	go func() {
		defer writer.Done()
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			a.SetTier(tiers[i%len(tiers)])
			i++
		}
	}()

	// Readers: hit every concurrent Tier reader — the whole-Config snapshot in
	// buildContext, PromptContext, and the Tier accessor.
	for i := 0; i < 4; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for j := 0; j < 2000; j++ {
				_ = a.buildContext(domain.ActorMain, "")
				_ = a.PromptContext()
				_ = a.Tier()
			}
		}()
	}

	readers.Wait()
	close(stop)
	writer.Wait()

	// After the churn the tier is one of the valid set (never torn into garbage).
	if !a.Tier().IsValid() {
		t.Fatalf("tier ended invalid after concurrent churn: %q", a.Tier())
	}
}
