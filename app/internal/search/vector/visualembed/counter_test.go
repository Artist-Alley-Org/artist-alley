package visualembed

import (
	"sync"
	"testing"
)

func TestCounter_Snapshot_ZeroInitially(t *testing.T) {
	c := NewCounter()
	snap := c.Snapshot()
	for _, k := range []string{
		"visual_embed_auto_success",
		"visual_embed_auto_transient_failed",
		"visual_embed_auto_permanent_failed",
		"visual_embed_auto_rate_limited_wait",
		"visual_embed_auto_skipped",
		"visual_embed_auto_pending",
	} {
		if snap[k] != 0 {
			t.Fatalf("%s: got %d, want 0", k, snap[k])
		}
	}
}

func TestCounter_RecordSuccess_Increments(t *testing.T) {
	c := NewCounter()
	c.RecordSuccess()
	c.RecordSuccess()
	if got := c.Snapshot()["visual_embed_auto_success"]; got != 2 {
		t.Fatalf("success: got %d, want 2", got)
	}
}

func TestCounter_NilSafe(t *testing.T) {
	// All record + snapshot methods must accept a nil receiver so
	// tests + fake wiring don't need to hold a real Counter.
	var c *Counter
	c.RecordSuccess()
	c.RecordTransientFailed()
	c.RecordPermanentFailed()
	c.RecordRateLimitedWait()
	c.RecordSkipped()
	c.StartPending()
	c.EndPending()
	if c.Snapshot() != nil {
		t.Fatal("nil Counter Snapshot should return nil, got non-nil map")
	}
}

func TestCounter_Pending_StartEnd(t *testing.T) {
	c := NewCounter()
	c.StartPending()
	c.StartPending()
	c.StartPending()
	if got := c.Snapshot()["visual_embed_auto_pending"]; got != 3 {
		t.Fatalf("pending mid-flight: got %d, want 3", got)
	}
	c.EndPending()
	c.EndPending()
	if got := c.Snapshot()["visual_embed_auto_pending"]; got != 1 {
		t.Fatalf("pending after 2 ends: got %d, want 1", got)
	}
}

func TestCounter_ConcurrentIncrements_NoDataRace(t *testing.T) {
	c := NewCounter()
	const workers = 32
	const perWorker = 100
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < perWorker; j++ {
				c.RecordSuccess()
				c.StartPending()
				c.EndPending()
			}
		}()
	}
	wg.Wait()
	if got := c.Snapshot()["visual_embed_auto_success"]; got != int64(workers*perWorker) {
		t.Fatalf("success under contention: got %d, want %d", got, workers*perWorker)
	}
	if got := c.Snapshot()["visual_embed_auto_pending"]; got != 0 {
		t.Fatalf("pending after equal start/end pairs: got %d, want 0", got)
	}
}
