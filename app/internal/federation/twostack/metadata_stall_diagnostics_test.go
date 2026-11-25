// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Failure-site diagnostics for two shared waits (#1577): flowGate.settle
// and likeGate.waitGrantOnB. They run ONLY when the wait has already
// timed out, before the test's Fatal, so the state is read while both
// instances are still up. They change nothing:
//
//   - SELECT only, under ONE context of at most diagTimeout across both
//     databases and every query;
//   - only ids, URIs, types, statuses, attempts, times, counts, cursor
//     positions, capability presence and short reason codes; never a
//     payload, envelope, key, note or error text (a reason that is not
//     a short code is shown as omitted);
//   - every row set is bounded and ordered, and says when it was cut;
//   - a query, scan or rows error is reported by its class (SQLSTATE or
//     deadline), never ignored and never in place of the test's failure.
//
// The two databases are read one after the other, after the wait's last
// observation: the snapshot is not atomic across the instances, and a
// condition can settle between the timeout and the read.

package twostack_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	diagTimeout = 5 * time.Second
	diagRows    = 32
)

// diagReport collects one failure's sections under one deadline.
type diagReport struct {
	ctx   context.Context
	start time.Time
	parts []string
}

func newDiagReport() (*diagReport, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), diagTimeout)
	return &diagReport{ctx: ctx, start: time.Now()}, cancel
}

func (d *diagReport) note(format string, args ...any) {
	d.parts = append(d.parts, fmt.Sprintf(format, args...))
}

// diagErr is an error's safe class: its SQLSTATE, the deadline, or its
// Go type. Never its text.
func diagErr(err error) string {
	var pe *pgconn.PgError
	switch {
	case errors.As(err, &pe):
		return "SQLSTATE " + pe.Code
	case errors.Is(err, context.DeadlineExceeded):
		return "diagnostic deadline exceeded"
	case errors.Is(err, context.Canceled):
		return "diagnostic canceled"
	}
	return fmt.Sprintf("error %T", err)
}

// diagCode is SQL for col as a short reason code, or a marker when it is
// any other text.
func diagCode(col string) string {
	return "CASE WHEN " + col + " IS NULL THEN '' WHEN " + col + " ~ '^[a-z0-9_.:-]{1,64}$' THEN " + col +
		" ELSE '<non-code text omitted>' END"
}

// lines runs a SELECT of one text column on pool, reporting at most
// diagRows rows and whether more existed. The query must not end in a
// LIMIT; one is added.
func (d *diagReport) lines(label string, pool *pgxpool.Pool, sql string, args ...any) {
	rows, err := pool.Query(d.ctx, sql+fmt.Sprintf(" LIMIT %d", diagRows+1), args...)
	if err != nil {
		d.note("%s: query failed (%s)", label, diagErr(err))
		return
	}
	defer rows.Close()
	var out []string
	cut := false
	for rows.Next() {
		var s *string
		if err := rows.Scan(&s); err != nil {
			d.note("%s: scan failed (%s)", label, diagErr(err))
			return
		}
		if len(out) == diagRows {
			cut = true
			break
		}
		if s == nil {
			out = append(out, "<row with a null field>")
			continue
		}
		out = append(out, *s)
	}
	if err := rows.Err(); err != nil {
		d.note("%s: rows failed (%s)", label, diagErr(err))
		return
	}
	switch {
	case len(out) == 0:
		d.note("%s: none", label)
	case cut:
		d.note("%s (first %d, more exist): %s", label, diagRows, strings.Join(out, " | "))
	default:
		d.note("%s (%d): %s", label, len(out), strings.Join(out, " | "))
	}
}

// activityRefs reads the ids and URIs a later query on the other
// instance correlates by; ok is false when it could not.
func (d *diagReport) activityRefs(label string, pool *pgxpool.Pool, sql string, args ...any) (ids []uuid.UUID, uris []string, ok bool) {
	rows, err := pool.Query(d.ctx, sql+fmt.Sprintf(" LIMIT %d", diagRows+1), args...)
	if err != nil {
		d.note("%s: query failed (%s)", label, diagErr(err))
		return nil, nil, false
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var uri string
		if err := rows.Scan(&id, &uri); err != nil {
			d.note("%s: scan failed (%s)", label, diagErr(err))
			return nil, nil, false
		}
		if len(ids) == diagRows {
			d.note("%s: more than %d; correlating the first %d", label, diagRows, diagRows)
			break
		}
		ids, uris = append(ids, id), append(uris, uri)
	}
	if err := rows.Err(); err != nil {
		d.note("%s: rows failed (%s)", label, diagErr(err))
		return nil, nil, false
	}
	return ids, uris, true
}

func (d *diagReport) String() string {
	d.note("diagnostics took %s (limit %s; read after the last observation, not atomic across instances)",
		time.Since(d.start).Round(time.Millisecond), diagTimeout)
	return strings.Join(d.parts, "; ")
}

// diagPeers is SQL listing an instance's peer rows with the presence of
// the profiles the gates rely on, marking the expected row ($1).
const diagPeers = `SELECT p.id::text || ' expected=' || (p.id = $1)::text || ' url=' || p.instance_url ||
	' enabled=' || p.enabled::text || ' status=' || p.status ||
	' share-lifecycle-1=' || coalesce(p.capabilities ? 'share-lifecycle-1', false)::text ||
	' asset-metadata-1=' || coalesce(p.capabilities ? 'asset-metadata-1', false)::text ||
	' archival-1=' || coalesce(p.capabilities ? 'archival-1', false)::text ||
	' original-transfer-1=' || coalesce(p.capabilities ? 'original-transfer-1', false)::text
	FROM federation_peers p ORDER BY p.created_at, p.id`

// diagCursor is SQL for the outbox dispatcher's cursor row(s).
const diagCursor = `SELECT 'cursor=' || coalesce(s.last_dispatched_activity_id::text, 'none') ||
	' at=' || coalesce(s.last_dispatched_at::text, 'never') FROM federation_dispatch_state s ORDER BY s.id`

// diagReached is SQL marking whether the dispatcher's cursor has reached
// activity a (a.created_at, a.id), for use inside a projection.
const diagReached = `CASE WHEN NOT EXISTS (SELECT 1 FROM federation_dispatch_state) THEN 'no-cursor-row'
	WHEN (SELECT s.last_dispatched_activity_id FROM federation_dispatch_state s ORDER BY s.id LIMIT 1) IS NULL THEN 'not-reached'
	WHEN (a.created_at, a.id) <= (SELECT c.created_at, c.id FROM activities c
	      WHERE c.id = (SELECT s.last_dispatched_activity_id FROM federation_dispatch_state s ORDER BY s.id LIMIT 1)) THEN 'reached'
	ELSE 'not-reached' END`

// settleDiagnostics is flowGate.settle's failure snapshot for asset o.
func settleDiagnostics(g *flowGate, o original, held []uuid.UUID, counts [4]int) string {
	d, cancel := newDiagReport()
	defer cancel()
	d.note("expected: dir=%s source=%s destination=%s source row for destination=%s destination row for source=%s asset=%s object=%s held=%v",
		g.dir, g.src.name, g.dst.name, g.peerBonA, g.peerAonB, o.asset, g.assetURI(o), held)
	d.note("last observed (the wait's own final poll): work=%d unrouted=%d queued=%d pending=%d", counts[0], counts[1], counts[2], counts[3])
	d.lines("source peers", g.src.pool, diagPeers, g.peerBonA)
	d.lines("destination peers", g.dst.pool, diagPeers, g.peerAonB)
	d.lines("source metadata work (asset or all)", g.src.pool, `SELECT w.id::text || ' kind=' || w.kind || ' asset=' || coalesce(w.asset_id::text, '') ||
		' asset_cursor=' || coalesce(w.asset_cursor::text, '') || ' peer_cursor=' || coalesce(w.peer_cursor::text, '') ||
		' created=' || w.created_at::text
		FROM federation_metadata_work w WHERE w.asset_id = $1 OR w.kind = 'all' ORDER BY w.created_at, w.id`, o.asset)
	d.lines("source dispatcher", g.src.pool, diagCursor)
	d.lines("source activities of the asset", g.src.pool, `SELECT a.id::text || ' ' || a.activity_type ||
		' capture=' || (a.payload ? 'metadata_capture')::text || ' created=' || a.created_at::text || ' dispatcher=' || `+diagReached+` ||
		' routed_to_destination=' || EXISTS (SELECT 1 FROM federation_outbox ob WHERE ob.activity_id = a.id AND ob.peer_id = $2)::text
		FROM activities a WHERE a.object_local_id = $1 ORDER BY a.created_at, a.id`, o.asset.String(), g.peerBonA)
	d.lines("source outbox of the asset to the destination", g.src.pool, `SELECT ob.activity_id::text || ' ' || a.activity_type ||
		' status=' || ob.status || ' attempts=' || ob.attempts::text || ' next=' || coalesce(ob.next_attempt_at::text, 'null') ||
		' due=' || coalesce((ob.next_attempt_at <= now())::text, 'null') || ' sent=' || coalesce(ob.sent_at::text, '') ||
		' targeted=' || (ob.target_user_url IS NOT NULL)::text || ' held=' || (ob.activity_id = ANY ($3))::text ||
		' refused=' || `+diagCode("ob.refused_reason")+` || ' has_error=' || (ob.last_error IS NOT NULL)::text
		FROM federation_outbox ob JOIN activities a ON a.id = ob.activity_id
		WHERE a.object_local_id = $1 AND ob.peer_id = $2 ORDER BY ob.created_at, ob.activity_id`, o.asset.String(), g.peerBonA, held)
	d.lines("destination inbox about the object (not processed)", g.dst.pool, `SELECT i.id::text || ' ' || i.activity_type || ' ' || i.activity_uri ||
		' status=' || i.status || ' attempts=' || i.dispatch_attempts::text || ' received=' || i.received_at::text ||
		' reason=' || `+diagCode("i.reject_reason")+` || ' has_error=' || (i.last_error IS NOT NULL)::text
		FROM federation_inbox i WHERE i.peer_id = $1 AND i.envelope_json->>'object' = $2 AND i.status <> 'processed'
		ORDER BY i.received_at, i.id`, g.peerAonB, g.assetURI(o))
	d.lines("destination inbox about the object by status", g.dst.pool, `SELECT i.status || '=' || count(*)::text
		FROM federation_inbox i WHERE i.peer_id = $1 AND i.envelope_json->>'object' = $2 GROUP BY i.status ORDER BY i.status`,
		g.peerAonB, g.assetURI(o))
	d.lines("destination received records of the asset", g.dst.pool, `SELECT r.id::text || ' ' || r.activity_uri || ' state=' || r.state ||
		' reason=' || `+diagCode("r.reason")+` || ' attempts=' || r.attempts::text || ' next=' || coalesce(r.next_attempt_at::text, 'null') ||
		' due=' || coalesce((r.next_attempt_at <= now())::text, 'null')
		FROM federation_received_assets r WHERE r.peer_id = $1 AND r.origin_asset_uri = $2 ORDER BY r.received_at, r.id`,
		g.peerAonB, g.assetURI(o))
	d.lines("destination kept revisions of the asset", g.dst.pool, `SELECT 'revisions=' || count(*)::text || ' with_metadata=' ||
		count(m.revision_id)::text FROM federation_received_asset_revisions r
		LEFT JOIN federation_received_asset_metadata m ON m.revision_id = r.id WHERE r.peer_id = $1 AND r.origin_asset_uri = $2`,
		g.peerAonB, g.assetURI(o))
	return d.String()
}

// grantDiagnostics is likeGate.waitGrantOnB's failure snapshot for the
// grant of objectURI to target ("" for peer-wide) expected in state.
func grantDiagnostics(g *likeGate, objectURI, target, state, last string) string {
	d, cancel := newDiagReport()
	defer cancel()
	d.note("expected: object=%s target=%q state=%s A row for B=%s B row for A=%s", objectURI, target, state, g.peerBonA, g.peerAonB)
	d.note("last observed (the wait's own final poll): %s", last)
	d.lines("A peers", g.a.pool, diagPeers, g.peerBonA)
	d.lines("B peers", g.b.pool, diagPeers, g.peerAonB)
	const shareActs = `FROM activities a WHERE a.activity_type IN ('aa:Share', 'aa:Unshare')
		AND (a.payload->>'object_uri' = $1 OR a.object_uri = $1)`
	ids, uris, ok := d.activityRefs("A share activities of the object", g.a.pool,
		`SELECT a.id, coalesce(a.activity_uri, '') `+shareActs+` ORDER BY a.created_at, a.id`, objectURI)
	d.lines("A share activities of the object", g.a.pool, `SELECT a.id::text || ' ' || a.activity_type || ' ' || coalesce(a.activity_uri, '') ||
		' created=' || a.created_at::text || ' share_id=' || coalesce(a.payload->>'share_id', '') ||
		' object_id=' || coalesce(a.payload->>'object_id', '') || ' dispatcher=' || `+diagReached+` `+shareActs+` ORDER BY a.created_at, a.id`, objectURI)
	d.lines("A dispatcher", g.a.pool, diagCursor)
	if !ok {
		d.note("correlation stopped: the object's share activities could not be read")
		return d.String()
	}
	if len(ids) == 0 {
		d.note("correlation: A recorded no aa:Share or aa:Unshare naming the object")
	}
	d.lines("A shares named by those activities", g.a.pool, `SELECT s.id::text || ' peer=' || s.peer_id::text ||
		' current_peer=' || (s.peer_id = $2)::text || ' target=' || coalesce(s.target_user_url, '') ||
		' target_matches=' || (coalesce(s.target_user_url, '') = $3)::text || ' scope=' || s.scope ||
		' granted_activity=' || coalesce(s.granted_activity_id::text, '') || ' granted=' || coalesce(s.granted_at::text, '') ||
		' revoked=' || coalesce(s.revoked_at::text, '') || ' expires=' || coalesce(s.expires_at::text, '')
		FROM federation_shares s WHERE s.id::text IN (SELECT a.payload->>'share_id' FROM activities a WHERE a.id = ANY ($1))
		ORDER BY s.created_at, s.id`, ids, g.peerBonA, target)
	d.lines("A outbox of those activities", g.a.pool, `SELECT ob.activity_id::text || ' peer=' || ob.peer_id::text ||
		' current_peer=' || (ob.peer_id = $2)::text || ' status=' || ob.status || ' attempts=' || ob.attempts::text ||
		' next=' || coalesce(ob.next_attempt_at::text, 'null') || ' due=' || coalesce((ob.next_attempt_at <= now())::text, 'null') || ' sent=' || coalesce(ob.sent_at::text, '') ||
		' refused=' || `+diagCode("ob.refused_reason")+` || ' has_error=' || (ob.last_error IS NOT NULL)::text
		FROM federation_outbox ob WHERE ob.activity_id = ANY ($1) ORDER BY ob.created_at, ob.activity_id`, ids, g.peerBonA)
	d.lines("A skipped emissions of those activities", g.a.pool, `SELECT e.occurred_at::text || ' activity=' || coalesce(e.metadata->>'activity_id', '') ||
		' reason=' || `+diagCode("e.metadata->>'reason'")+`
		FROM audit_events e WHERE e.event_type = 'federation.emission.skipped' AND e.metadata->>'activity_id' = ANY ($1)
		ORDER BY e.occurred_at, e.id`, uuidTexts(ids))
	d.lines("A per-peer skipped share emissions to the current peer since the object's first share (time window only, not activity-keyed)",
		g.a.pool, `SELECT e.occurred_at::text || ' verb=' || coalesce(e.metadata->>'verb', '') || ' reason=' || `+diagCode("e.metadata->>'reason'")+`
		FROM audit_events e WHERE e.event_type = 'federation.emission.skipped' AND e.metadata->>'peer_id' = $1
		AND e.metadata->>'verb' IN ('aa:Share', 'aa:Unshare')
		AND e.occurred_at >= coalesce((SELECT min(a.created_at) FROM activities a WHERE a.id = ANY ($2)), now())
		ORDER BY e.occurred_at, e.id`, g.peerBonA.String(), ids)
	d.lines("B inbox of those activities", g.b.pool, `SELECT i.id::text || ' ' || i.activity_type || ' ' || i.activity_uri ||
		' peer=' || i.peer_id::text || ' current_peer=' || (i.peer_id = $2)::text || ' status=' || i.status ||
		' attempts=' || i.dispatch_attempts::text || ' received=' || i.received_at::text ||
		' processed=' || coalesce(i.processed_at::text, '') || ' reason=' || `+diagCode("i.reject_reason")+` ||
		' has_error=' || (i.last_error IS NOT NULL)::text
		FROM federation_inbox i WHERE i.activity_uri = ANY ($1) ORDER BY i.received_at, i.id`, uris, g.peerAonB)
	d.lines("B grants of the object", g.b.pool, `SELECT gr.id::text || ' peer=' || gr.peer_id::text || ' current_peer=' || (gr.peer_id = $2)::text ||
		' state=' || gr.state || ' target=' || coalesce(gr.target_user_uri, '') || ' target_matches=' || (coalesce(gr.target_user_uri, '') = $3)::text ||
		' grant_activity=' || gr.grant_activity_uri || ' end=' || `+diagCode("gr.end_reason")+` ||
		' received=' || gr.received_at::text || ' ended=' || coalesce(gr.ended_at::text, '')
		FROM federation_inbound_grants gr WHERE gr.object_uri = $1 ORDER BY gr.received_at, gr.id`, objectURI, g.peerAonB, target)
	return d.String()
}

func uuidTexts(ids []uuid.UUID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, id.String())
	}
	return out
}

// lastGrant is the safe description of waitGrantOnB's final poll.
func lastGrant(state string, err error) string {
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "no grant row for the object and target"
	case err != nil:
		return "query failed (" + diagErr(err) + ")"
	}
	return "newest grant state=" + state
}
