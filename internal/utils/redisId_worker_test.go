package utils

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// TestRedisIdWorkerConcurrencyPerformance exercises high-concurrency ID generation
// to observe throughput and ensure IDs stay unique under load.
func TestRedisIdWorkerConcurrencyPerformance(t *testing.T) {
	ctx := context.Background()

	client := redis.NewClient(&redis.Options{
		Addr: "127.0.0.1:6379",
		DB:   0,
	})
	if err := client.Ping(ctx).Err(); err != nil {
		t.Skipf("redis not available: %v", err)
	}
	defer client.Close()

	worker := NewRedisIdWorker(client)

	const (
		goroutines = 100
		perWorker  = 100
	)
	total := goroutines * perWorker

	ids := make([]int64, total)
	var (
		writeIdx int64
		wg       sync.WaitGroup
		firstErr atomic.Pointer[error]
	)

	prefix := "order"
	start := time.Now()

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perWorker; j++ {
				id, err := worker.NextId(ctx, prefix)
				if err != nil {
					firstErr.CompareAndSwap(nil, &err)
					return
				}
				pos := atomic.AddInt64(&writeIdx, 1) - 1
				ids[pos] = id
			}
		}()
	}

	wg.Wait()
	if errPtr := firstErr.Load(); errPtr != nil {
		t.Fatalf("failed to generate id: %v", *errPtr)
	}

	elapsed := time.Since(start)
	if writeIdx != int64(total) {
		t.Fatalf("expected %d ids, got %d", total, writeIdx)
	}

	seen := make(map[int64]struct{}, total)
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			t.Fatalf("duplicate id detected: %d", id)
		}
		seen[id] = struct{}{}
	}

	for _, id := range ids {
		t.Logf("%d", id)
	}

	qps := float64(total) / elapsed.Seconds()
	t.Logf("generated %d ids with %d goroutines in %s (%.0f ops/sec)", total, goroutines, elapsed, qps)
}
