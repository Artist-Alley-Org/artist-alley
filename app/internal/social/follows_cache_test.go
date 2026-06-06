package social

import "testing"

// Pin the canonical-pair block key: (a,b) and (b,a) must produce the
// same string so HasBlockBetween's symmetric semantics get a single
// cache row per pair. A regression that returned two different keys
// would let one invalidation leave a stale row in the other slot —
// silently re-enabling notification delivery against a blocker.

func TestCanonicalBlockKey_IsSymmetric(t *testing.T) {
	cases := []struct {
		a, b int64
	}{
		{1, 2},
		{42, 7},
		{100, 100}, // self — keys still align (the gate doesn't allow self-block but the helper must not panic)
		{1, 9_999_999_999},
	}
	for _, c := range cases {
		ka := canonicalBlockKey(c.a, c.b)
		kb := canonicalBlockKey(c.b, c.a)
		if ka != kb {
			t.Errorf("(%d,%d) and reverse produced different keys: %q vs %q", c.a, c.b, ka, kb)
		}
	}
}

// Follow-edge key MUST be direction-sensitive — A following B is
// independent of B following A and they need separate cache rows.
func TestFollowKey_IsDirectional(t *testing.T) {
	if followKey(1, 2) == followKey(2, 1) {
		t.Error("follow edge key must distinguish direction (A→B vs B→A)")
	}
}

// Sanity: distinct pairs produce distinct canonical keys.
func TestCanonicalBlockKey_DistinctPairsProduceDistinctKeys(t *testing.T) {
	if canonicalBlockKey(1, 2) == canonicalBlockKey(1, 3) {
		t.Error("different pairs collided in canonical block key")
	}
}
