package inbox_test

import (
	"sync"
	"testing"
	"time"

	"github.com/mscrnt/artist-alley/app/internal/federation/inbox"
)

func TestReplayCache_FirstSeenReturnsFalse(t *testing.T) {
	c := inbox.NewReplayCache(100, time.Minute)
	if c.Seen("https://peer.example/activities/abc") {
		t.Error("first sighting should NOT report seen")
	}
}

func TestReplayCache_SecondSeenReturnsTrue(t *testing.T) {
	c := inbox.NewReplayCache(100, time.Minute)
	uri := "https://peer.example/activities/abc"
	c.Seen(uri)
	if !c.Seen(uri) {
		t.Error("second sighting within TTL should report seen")
	}
}

func TestReplayCache_ExpiredEntryDoesNotReportSeen(t *testing.T) {
	c := inbox.NewReplayCache(100, 10*time.Millisecond)
	uri := "https://peer.example/activities/abc"
	c.Seen(uri)
	time.Sleep(20 * time.Millisecond)
	if c.Seen(uri) {
		t.Error("entry past TTL should NOT report seen")
	}
}

func TestReplayCache_RefreshKeepsEntryLive(t *testing.T) {
	c := inbox.NewReplayCache(100, 50*time.Millisecond)
	uri := "https://peer.example/activities/abc"
	c.Seen(uri)
	// Refresh halfway through; should still report seen for
	// another ~50ms.
	time.Sleep(25 * time.Millisecond)
	if !c.Seen(uri) {
		t.Fatal("mid-TTL refresh should report seen")
	}
	time.Sleep(30 * time.Millisecond)
	if !c.Seen(uri) {
		t.Error("after refresh, entry should still be in cache (refresh resets TTL)")
	}
}

func TestReplayCache_BoundsAtCapacity(t *testing.T) {
	c := inbox.NewReplayCache(10, time.Minute)
	for i := 0; i < 20; i++ {
		c.Seen(makeURI(i))
	}
	// 20 inserted, max 10 → cache should be bounded around 10.
	// Allow a small slack because the eviction is one-at-a-time
	// over the threshold (so 11 is possible momentarily, but
	// never approach 20).
	if got := c.Len(); got > 12 {
		t.Errorf("cache should bound at ~10; got %d entries", got)
	}
}

func TestReplayCache_ConcurrentSafeRoundTrip(t *testing.T) {
	c := inbox.NewReplayCache(1000, time.Minute)
	const goroutines = 8
	const perGoroutine = 100
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				c.Seen(makeURI(base*perGoroutine + i))
			}
		}(g)
	}
	wg.Wait()
	// 800 unique entries inserted; cap is 1000, so all should
	// still be present (no eviction race triggered).
	if got := c.Len(); got != goroutines*perGoroutine {
		t.Errorf("expected %d entries, got %d", goroutines*perGoroutine, got)
	}
}

func makeURI(i int) string {
	return "https://peer.example/activities/" + itoa(i)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}
