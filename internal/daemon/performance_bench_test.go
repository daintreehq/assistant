package daemon

import (
	"context"
	"sync"
	"testing"
	"time"
)

const benchmarkDueJobs = 10_000

func benchmarkJobs() []func(context.Context) {
	jobs := make([]func(context.Context), benchmarkDueJobs)
	for i := range jobs {
		jobs[i] = func(context.Context) {}
	}
	return jobs
}

// runLegacyTickJobs preserves the old semaphore-inside-each-goroutine dispatcher
// solely as a benchmark control. Production uses tickJobPool.
func runLegacyTickJobs(ctx context.Context, jobs []func(context.Context)) {
	sem := make(chan struct{}, tickJobConcurrency)
	var wg sync.WaitGroup
	for _, job := range jobs {
		job := job
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			jctx, cancel := context.WithTimeout(ctx, time.Duration(itemDeadlineMS)*time.Millisecond)
			job(jctx)
			cancel()
		}()
	}
	wg.Wait()
}

func BenchmarkTickJobDispatch10K(b *testing.B) {
	jobs := benchmarkJobs()
	ctx := context.Background()
	b.Run("legacy_goroutine_per_item", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			runLegacyTickJobs(ctx, jobs)
		}
	})
	b.Run("fixed_worker_pool", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			pool := newTickJobPool(ctx, len(jobs), nil)
			for _, job := range jobs {
				pool.Submit(job)
			}
			pool.CloseAndWait()
		}
	})
}
