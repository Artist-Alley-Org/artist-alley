package metadata

import (
	"sync"
	"testing"
	"time"
)

func TestCounter_RecordSingleSuccess(t *testing.T) {
	c := NewCounter()
	c.Record("image/jpeg", ResultSuccess, time.Time{})

	snap := c.Snapshot()
	if snap.CounterTotal != 1 {
		t.Errorf("CounterTotal = %d, want 1", snap.CounterTotal)
	}
	if snap.ByFormat["image/jpeg"] != 1 {
		t.Errorf("ByFormat[image/jpeg] = %d, want 1", snap.ByFormat["image/jpeg"])
	}
	if snap.ByResult["success"] != 1 {
		t.Errorf("ByResult[success] = %d, want 1", snap.ByResult["success"])
	}
	if snap.LastSuccess == nil {
		t.Errorf("LastSuccess should be populated after a success")
	}
	if snap.LastFailure != nil {
		t.Errorf("LastFailure should be nil after only-successes")
	}
}

func TestCounter_RecordMixedResults_AggregatesPerKey(t *testing.T) {
	c := NewCounter()
	c.Record("image/jpeg", ResultSuccess, time.Time{})
	c.Record("image/jpeg", ResultSuccess, time.Time{})
	c.Record("image/png", ResultMalformedFile, time.Time{})
	c.Record("image/webp", ResultUnsupportedFormat, time.Time{})

	snap := c.Snapshot()
	if snap.CounterTotal != 4 {
		t.Errorf("CounterTotal = %d, want 4", snap.CounterTotal)
	}
	if snap.ByFormat["image/jpeg"] != 2 || snap.ByFormat["image/png"] != 1 || snap.ByFormat["image/webp"] != 1 {
		t.Errorf("ByFormat counts: %+v", snap.ByFormat)
	}
	if snap.ByResult["success"] != 2 || snap.ByResult["malformed_file"] != 1 || snap.ByResult["unsupported_format"] != 1 {
		t.Errorf("ByResult counts: %+v", snap.ByResult)
	}
}

func TestCounter_NoMetadataCountedAsSuccessForRecency(t *testing.T) {
	// no_metadata is operationally a success (we ran, found
	// nothing, no failure to surface). LastSuccess tracks it.
	c := NewCounter()
	c.Record("image/jpeg", ResultNoMetadata, time.Time{})

	snap := c.Snapshot()
	if snap.LastSuccess == nil {
		t.Errorf("no_metadata should update LastSuccess")
	}
	if snap.LastFailure != nil {
		t.Errorf("no_metadata should NOT update LastFailure")
	}
}

func TestCounter_FailuresUpdateLastFailure(t *testing.T) {
	c := NewCounter()
	c.Record("image/jpeg", ResultMalformedFile, time.Time{})

	snap := c.Snapshot()
	if snap.LastFailure == nil {
		t.Errorf("failure should update LastFailure")
	}
	if snap.LastSuccess != nil {
		t.Errorf("failure should NOT update LastSuccess")
	}
}

func TestCounter_SnapshotIsDeepCopy(t *testing.T) {
	c := NewCounter()
	c.Record("image/jpeg", ResultSuccess, time.Time{})

	snap1 := c.Snapshot()
	snap1.ByResult["success"] = 99 // mutate the returned map
	snap1.ByFormat["image/jpeg"] = 99

	snap2 := c.Snapshot()
	if snap2.ByResult["success"] != 1 {
		t.Errorf("mutating the snapshot's map leaked back into the counter: %v", snap2.ByResult)
	}
	if snap2.ByFormat["image/jpeg"] != 1 {
		t.Errorf("mutating the snapshot's ByFormat leaked: %v", snap2.ByFormat)
	}
}

func TestCounter_ConcurrentRecord_NoLostUpdates(t *testing.T) {
	c := NewCounter()
	const goroutines = 50
	const perGoroutine = 100

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				c.Record("image/jpeg", ResultSuccess, time.Time{})
			}
		}()
	}
	wg.Wait()

	snap := c.Snapshot()
	want := int64(goroutines * perGoroutine)
	if snap.CounterTotal != want {
		t.Errorf("CounterTotal = %d, want %d (concurrent updates lost)", snap.CounterTotal, want)
	}
	if snap.ByResult["success"] != want {
		t.Errorf("ByResult[success] = %d, want %d", snap.ByResult["success"], want)
	}
}

func TestCounter_ImplementsHealthhandlerCounter(t *testing.T) {
	// Compile-time assertion lives in observability.go; this
	// test is here to make the relationship obvious from the
	// test surface.
	c := NewCounter()
	_ = c.Snapshot() // just confirm the method is reachable
}
