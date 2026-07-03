package saved

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/email"
	"github.com/mscrnt/artist-alley/app/internal/jobs"
)

// NotifyVerb is the verb string enqueued on the notification.email
// job. Dots → underscores lookup on templateForVerb picks up the
// notification_saved_search_digest template automatically.
const NotifyVerb = "saved_search.digest"

// NotifyVerbRemoved is the Phase 1.16.B-5 variant fired when a
// saved-search's previous hit set has been entirely removed. Per
// pre-audit Q4 finding, templateForVerb is strictly one-per-verb,
// so the "everything removed" case requires a distinct verb (not
// a template-variant on the same verb the brief originally
// specified). Maps to notification_saved_search_removed_digest.
const NotifyVerbRemoved = "saved_search.removed_digest"

// Notifier enqueues a digest email for a Delta. Skips silently
// when the delta is unchanged or the row has channel='none'.
//
// One notification.email job per run; the Phase 1.19.A handler
// resolves the recipient's email address + renders the digest via
// the template registry.
type Notifier struct {
	Jobs *jobs.Service
}

// NewNotifier constructs a Notifier.
func NewNotifier(j *jobs.Service) *Notifier { return &Notifier{Jobs: j} }

// Emit enqueues a digest email for the given delta. Returns
// (sent, err); sent=false means the notifier decided not to send
// (delta unchanged, channel=none, no Added IDs) without error.
func (n *Notifier) Emit(ctx context.Context, row Row, delta Delta, run RunResult, siteURL string) (bool, error) {
	if n == nil || n.Jobs == nil {
		return false, nil
	}
	if !delta.HashChanged {
		return false, nil
	}
	if row.NotifyChannel != NotifyChannelEmail {
		return false, nil
	}
	// Phase 1.16.B-5 — "everything removed" digest. A hash change
	// with zero Added AND zero HitIDs in the fresh run means every
	// previously-matching asset is gone; the user gets a distinct
	// email variant ("all matches removed") instead of silence.
	// Partial removals with any Added still take the normal
	// digest path below.
	allRemoved := len(delta.Added) == 0 && len(run.HitIDs) == 0 && len(delta.Removed) > 0
	if !allRemoved && len(delta.Added) == 0 {
		// Hash changed but no Added and not "all removed" — this
		// is the "partial removal, nothing new" case; skip the
		// email to avoid a misleading "0 new matches" note.
		return false, nil
	}

	// Verb + payload branch on the "everything removed" flag. The
	// two branches share the recipient + target-kind fields but
	// carry different template data.
	verb := NotifyVerb
	var payload map[string]any
	if allRemoved {
		verb = NotifyVerbRemoved
		payload = map[string]any{
			"search_name":   row.Name,
			"removed_count": len(delta.Removed),
			"results_url":   siteURL + "/search?dsl=" + urlEscape(row.DSL),
		}
	} else {
		// Build the per-hit projection. Cross-reference the run's
		// metadata so each Added ID gets its title + summary + URL.
		metaByID := make(map[uuid.UUID]HitMeta, len(run.HitsMeta))
		for _, m := range run.HitsMeta {
			metaByID[m.ID] = m
		}
		hits := make([]map[string]any, 0, len(delta.Added))
		for _, id := range delta.Added {
			m := metaByID[id]
			title := m.Title
			if title == "" {
				title = id.String()
			}
			hits = append(hits, map[string]any{
				"title":   title,
				"summary": m.Summary,
				"url":     siteURL + "/assets/" + id.String(),
			})
		}
		payload = map[string]any{
			"search_name": row.Name,
			"added_count": len(delta.Added),
			"results_url": siteURL + "/search?dsl=" + urlEscape(row.DSL),
			"hits":        hits,
		}
	}

	// The generic NotificationJobPayload carries the recipient +
	// verb + template data map. Payload map wins on key collision
	// per the handler's merge order.
	body, err := json.Marshal(email.NotificationJobPayload{
		RecipientUserRef: row.OwnerUserRef,
		Verb:             verb,
		TargetKind:       "saved_search",
		TargetID:         row.ID.String(),
		Payload:          payload,
	})
	if err != nil {
		return false, fmt.Errorf("saved.Notifier: marshal: %w", err)
	}
	// Idempotency: (saved_search_id, this-run-hash) collides on
	// the same delta so a re-fired run doesn't double-send. The
	// jobs.Service resolves the existing job on unique-violation
	// and returns success without error.
	if _, err := n.Jobs.Enqueue(ctx, email.JobTypeNotificationEmail, json.RawMessage(body), jobs.EnqueueOpts{
		IdempotencyKey: "saved_search:" + row.ID.String() + ":" + run.Hash,
	}); err != nil {
		return false, fmt.Errorf("saved.Notifier: enqueue: %w", err)
	}
	return true, nil
}

// urlEscape is a tiny helper for encoding a DSL string into a URL
// query parameter. Doesn't drag net/url into this package's
// import list.
func urlEscape(s string) string {
	// Minimal escape — the results_url only ever concatenates DSL
	// strings that came from the caller's own POST, which
	// validation already sanitised. `&`, `?`, ` `, `#` are the
	// only characters that break the URL grammar in practice.
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case ' ':
			out = append(out, '%', '2', '0')
		case '&':
			out = append(out, '%', '2', '6')
		case '?':
			out = append(out, '%', '3', 'F')
		case '#':
			out = append(out, '%', '2', '3')
		case '%':
			out = append(out, '%', '2', '5')
		default:
			out = append(out, c)
		}
	}
	return string(out)
}
