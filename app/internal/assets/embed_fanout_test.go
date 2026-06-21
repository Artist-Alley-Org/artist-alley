package assets

// Phase 1.14.B — bridge contract check between the assets-side
// fanout enqueue and the ai/jobs-side handler. The two packages can't
// import each other (jobs depends on assets via the bridge), so the
// idempotency-key format is duplicated. This test pins the value so
// a future driveby in either package surfaces the drift immediately.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/google/uuid"
)

func TestAIEmbedIdempotencyKey_MatchesAIJobsFormat(t *testing.T) {
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	model := "nomic-embed-text"

	got := aiEmbedIdempotencyKey(id.String(), model)

	// Reference value computed inline using the same SHA-256 over
	// the canonical key format aijobs.EmbedIdempotencyKey emits.
	// Tests on the ai/jobs side use the same format; if either side
	// drifts the keys diverge and re-enqueue deduplication breaks.
	want := sha256.Sum256([]byte(fmt.Sprintf("ai.embed|%s|%s", id, model)))
	if got != hex.EncodeToString(want[:]) {
		t.Errorf("aiEmbedIdempotencyKey drift:\n  got  %s\n  want %s", got, hex.EncodeToString(want[:]))
	}
}

func TestAIEmbedJobType_IsCanonicalString(t *testing.T) {
	if string(aiEmbedJobType) != "ai.embed" {
		t.Errorf("aiEmbedJobType = %q, want ai.embed (must match aijobs.JobTypeEmbed)", aiEmbedJobType)
	}
}
