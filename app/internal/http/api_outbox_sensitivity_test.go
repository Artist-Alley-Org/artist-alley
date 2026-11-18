// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1525 production-wiring regression for the outbox sensitivity
// policy. Every case below builds the dispatcher with
// newOutboxDispatcher and the delivery hooks with
// wireOutboxDeliveryEncryption, the same functions server boot
// calls, then drives one real activity through dispatch and
// delivery to a fake peer inbox.
//
// What it pins: a post carrying a restricted or embargoed asset
// is stamped with that tier and is delivered only encrypted, to a
// peer that negotiated E2E and whose key we hold; everywhere else
// it is refused with nothing POSTed. Public and team posts keep
// their plaintext path. Before #1525 boot wired no sensitivity
// lookup, so every row was stamped public and a restricted post
// went to a peer without E2E in plaintext.
//
// Real Postgres; skips without AA_DB_PASSWORD.

package http

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/assets"
	"github.com/mscrnt/artist-alley/app/internal/atrest"
	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/inbox"
	"github.com/mscrnt/artist-alley/app/internal/federation/outbox"
	"github.com/mscrnt/artist-alley/app/internal/federation/peer"
	"github.com/mscrnt/artist-alley/app/internal/federation/userkeys"
	"github.com/mscrnt/artist-alley/app/internal/users"
)

// wiringBaseURL is the sending instance's base URL in these tests.
const wiringBaseURL = "https://local.example"

func initAtrestForOutboxWiring(t *testing.T) {
	t.Helper()
	if atrest.Initialised() {
		return
	}
	key := make([]byte, atrest.MasterKeyLen)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("master key: %v", err)
	}
	if err := atrest.InitWithKey(key); err != nil {
		t.Fatalf("atrest init: %v", err)
	}
}

// fakeInbox records every POST body it receives.
type fakeInbox struct {
	mu     sync.Mutex
	bodies [][]byte
}

func (f *fakeInbox) handler(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.bodies = append(f.bodies, b)
	f.mu.Unlock()
	w.WriteHeader(http.StatusAccepted)
}

func (f *fakeInbox) posts() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.bodies...)
}

type outboxWiringCase struct {
	name string
	// assetTiers are attached to the post via post_assets.
	assetTiers []string
	// coverTier, when set, is attached only as the post's cover.
	coverTier    string
	peerE2E      bool
	recipientKey bool
	senderKey    bool
	// activities queued for the peer (default 1). Two or more go
	// through the Worker's batch path.
	activities int
	// beforeDelivery runs after dispatch and before the Worker
	// (#1525 delivery-time recheck): it changes what the post
	// carries while its rows are queued.
	beforeDelivery func(t *testing.T, ctx context.Context, pool *pgxpool.Pool, postID uuid.UUID, assetIDs []uuid.UUID)

	// wantTier is the tier stamped on the row at dispatch.
	wantTier      string
	wantStatus    string // sent | refused
	wantEncrypted bool
}

func TestOutboxProductionWiring_SensitivityReachesDelivery(t *testing.T) {
	cases := []outboxWiringCase{
		{name: "restricted asset, peer without E2E", assetTiers: []string{"public", "restricted"},
			senderKey: true, recipientKey: true,
			wantTier: "restricted", wantStatus: "refused"},
		{name: "embargo asset, peer without E2E", assetTiers: []string{"embargo"},
			senderKey: true, recipientKey: true,
			wantTier: "embargo", wantStatus: "refused"},
		{name: "restricted cover only, peer without E2E", coverTier: "restricted",
			senderKey: true, recipientKey: true,
			wantTier: "restricted", wantStatus: "refused"},
		{name: "restricted asset, peer E2E, recipient key missing", assetTiers: []string{"restricted"},
			peerE2E: true, senderKey: true,
			wantTier: "restricted", wantStatus: "refused"},
		{name: "embargo asset, peer E2E, recipient key missing", assetTiers: []string{"embargo"},
			peerE2E: true, senderKey: true,
			wantTier: "embargo", wantStatus: "refused"},
		{name: "restricted asset, peer E2E, sender key missing", assetTiers: []string{"restricted"},
			peerE2E: true, recipientKey: true,
			wantTier: "restricted", wantStatus: "refused"},
		{name: "restricted asset, peer E2E with recipient key", assetTiers: []string{"team", "restricted"},
			peerE2E: true, recipientKey: true, senderKey: true,
			wantTier: "restricted", wantStatus: "sent", wantEncrypted: true},
		{name: "embargo asset, peer E2E with recipient key", assetTiers: []string{"restricted", "embargo"},
			peerE2E: true, recipientKey: true, senderKey: true,
			wantTier: "embargo", wantStatus: "sent", wantEncrypted: true},
		{name: "public asset, peer without E2E", assetTiers: []string{"public"},
			senderKey: true, recipientKey: true,
			wantTier: "public", wantStatus: "sent"},
		{name: "team asset, peer without E2E", assetTiers: []string{"public", "team"},
			senderKey: true, recipientKey: true,
			wantTier: "team", wantStatus: "sent"},
		{name: "no assets, peer E2E with recipient key", peerE2E: true, recipientKey: true, senderKey: true,
			wantTier: "public", wantStatus: "sent", wantEncrypted: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { runOutboxWiringCase(t, tc) })
	}
}

func runOutboxWiringCase(t *testing.T, tc outboxWiringCase) {
	pool := openPoolForSensitivity(t)
	t.Cleanup(pool.Close)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	initAtrestForOutboxWiring(t)
	reg := cache.NewRegistry(pool, logger)
	t.Cleanup(reg.Stop)

	inboxSrv := &fakeInbox{}
	srv := httptest.NewServer(http.HandlerFunc(inboxSrv.handler))
	t.Cleanup(srv.Close)

	// Assets first, so their cleanup (registered by the helper)
	// runs after the post's.
	var assetIDs []uuid.UUID
	for _, tier := range tc.assetTiers {
		assetIDs = append(assetIDs, insertAssetAtTier(t, ctx, pool, tier))
	}
	var coverID *uuid.UUID
	if tc.coverTier != "" {
		id := insertAssetAtTier(t, ctx, pool, tc.coverTier)
		coverID = &id
	}

	var userRef int64
	username := "outbox-wiring-" + uuid.NewString()[:8]
	// The activities name this user's actor URI on the test base URL,
	// as activities/emit would (#1526: the signing hook refuses any
	// other actor URI).
	actorURI := wiringBaseURL + "/users/" + username
	if err := pool.QueryRow(ctx,
		`INSERT INTO "user" (username, fullname, approved) VALUES ($1, 'Outbox Wiring', 1) RETURNING ref`,
		username,
	).Scan(&userRef); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	// share-lifecycle-1: an aa:Share is addressed only to a peer that
	// negotiated it (#1571).
	caps := `["share-lifecycle-1"]`
	if tc.peerE2E {
		caps = `["e2e-encrypted","nacl-box","x25519","share-lifecycle-1"]`
	}
	var peerID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO federation_peers (instance_url, display_name, instance_public_key,
		    trust_tier, encryption_policy, enabled, status, handshake_by_user_ref, capabilities)
		 VALUES ($1, 'Wiring Peer', '', 'connected', 'plaintext', TRUE, 'connected', $2, $3::jsonb)
		 RETURNING id`,
		srv.URL, userRef, caps,
	).Scan(&peerID); err != nil {
		t.Fatalf("seed peer: %v", err)
	}
	postID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO posts (id, author_user_ref, title, visibility, cover_asset_id)
		 VALUES ($1, $2, 'Outbox Wiring', 'explicit-share', $3)`,
		postID, userRef, coverID,
	); err != nil {
		t.Fatalf("seed post: %v", err)
	}
	for i, a := range assetIDs {
		if _, err := pool.Exec(ctx,
			`INSERT INTO post_assets (post_id, asset_id, sort_order) VALUES ($1, $2, $3)`,
			postID, a, i,
		); err != nil {
			t.Fatalf("attach asset: %v", err)
		}
	}
	var senderPub []byte
	if tc.senderKey {
		pub, wrapped, err := userkeys.Generate()
		if err != nil {
			t.Fatalf("sender keypair: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO federation_user_keys (user_ref, version, algorithm, public_key, private_key_enc, is_current)
			 VALUES ($1, 1, 'naclbox-x25519-v1', $2, $3, TRUE)`,
			userRef, pub, wrapped,
		); err != nil {
			t.Fatalf("seed sender key: %v", err)
		}
		senderPub = pub
	}
	recipientPub, recipientWrapped, err := userkeys.Generate()
	if err != nil {
		t.Fatalf("recipient keypair: %v", err)
	}
	recipientPriv, err := userkeys.Unwrap(recipientWrapped)
	if err != nil {
		t.Fatalf("recipient unwrap: %v", err)
	}

	var grantActivityID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO activities (activity_uri, activity_type, actor_uri, actor_user_ref, payload)
		 VALUES ($1, 'aa:Share', $3, $2, '{}'::jsonb) RETURNING id`,
		"https://local.example/activities/"+uuid.NewString(), userRef, actorURI,
	).Scan(&grantActivityID); err != nil {
		t.Fatalf("seed grant activity: %v", err)
	}
	var shareID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO federation_shares (grantor_user_ref, object_kind, object_id, peer_id,
		    target_user_url, scope, granted_activity_id)
		 VALUES ($1, 'post', $2, $3, $4, 'view', $5) RETURNING id`,
		userRef, postID, peerID, srv.URL+"/users/bob", grantActivityID,
	).Scan(&shareID); err != nil {
		t.Fatalf("seed share: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM federation_outbox WHERE peer_id = $1`, peerID)
		_, _ = pool.Exec(c, `DELETE FROM federation_shares WHERE peer_id = $1`, peerID)
		_, _ = pool.Exec(c, `DELETE FROM activities WHERE actor_user_ref = $1`, userRef)
		_, _ = pool.Exec(c, `DELETE FROM post_assets WHERE post_id = $1`, postID)
		_, _ = pool.Exec(c, `DELETE FROM posts WHERE id = $1`, postID)
		_, _ = pool.Exec(c, `DELETE FROM federation_user_keys WHERE user_ref = $1`, userRef)
		_, _ = pool.Exec(c, `DELETE FROM federation_peers WHERE id = $1`, peerID)
		_, _ = pool.Exec(c, `DELETE FROM "user" WHERE ref = $1`, userRef)
		_, _ = pool.Exec(c, `UPDATE federation_dispatch_state SET last_dispatched_activity_id = NULL WHERE id = 1`)
	})
	// Park the dispatch cursor on the grant so RunOnce sees only
	// the activity below.
	if _, err := pool.Exec(ctx,
		`UPDATE federation_dispatch_state SET last_dispatched_activity_id = $1 WHERE id = 1`, grantActivityID); err != nil {
		t.Fatalf("park cursor: %v", err)
	}

	// The marker is the payload a leak would expose.
	marker := "wiring-secret-" + uuid.NewString()
	n := tc.activities
	if n < 1 {
		n = 1
	}
	if n > 1 && tc.wantStatus != "refused" {
		t.Fatal("test bug: multi-activity cases only assert refusal")
	}
	activityIDs := make([]uuid.UUID, n)
	for i := range activityIDs {
		if err := pool.QueryRow(ctx,
			`INSERT INTO activities (activity_uri, activity_type, actor_uri, actor_user_ref,
			    object_uri, object_kind, object_local_id, payload)
			 VALUES ($1, 'aa:Share', $6, $2, $3, 'post', $4,
			         jsonb_build_object('object_kind', 'post', 'scope', 'view',
			             'share_id', $7::text,
			             'peer_url', 'https://peer.example',
			             'target_user_url', 'https://peer.example/users/' || $5::text))
			 RETURNING id`,
			"https://local.example/activities/"+uuid.NewString(), userRef,
			"https://local.example/posts/"+postID.String(), postID.String(), marker, actorURI, shareID.String(),
		).Scan(&activityIDs[i]); err != nil {
			t.Fatalf("seed activity: %v", err)
		}
	}

	// Production wiring under test.
	auditRec := audit.NewRecorder(pool, logger)
	peers := peer.NewRegistry(pool, logger, reg)
	d := newOutboxDispatcher(pool, reg, peers, auditRec, logger)
	d.RunOnce(ctx)

	for _, aid := range activityIDs {
		var rowTier *string
		if err := pool.QueryRow(ctx,
			`SELECT sensitivity FROM federation_outbox WHERE activity_id = $1 AND peer_id = $2`,
			aid, peerID,
		).Scan(&rowTier); err != nil {
			t.Fatalf("outbox row for the activity: %v (the dispatcher did not enqueue it)", err)
		}
		if rowTier == nil || *rowTier != tc.wantTier {
			got := "<nil>"
			if rowTier != nil {
				got = *rowTier
			}
			t.Fatalf("outbox row sensitivity: got %s want %s", got, tc.wantTier)
		}
	}

	if tc.beforeDelivery != nil {
		tc.beforeDelivery(t, ctx, pool, postID, assetIDs)
	}

	_, signerKey, _ := ed25519.GenerateKey(rand.Reader)
	w := outbox.NewWorker(outbox.DefaultDeliveryConfig(), pool,
		&outbox.IdentitySigner{PrivateKey: signerKey, KeyURL: "https://local.example/instance#main-key"},
		outboxDeliveryPeerLookup(peers), logger)
	wireOutboxDeliveryEncryption(w, pool, peers,
		func(context.Context, string) ([]byte, int32, error) {
			if !tc.recipientKey {
				return nil, 0, errors.New("recipient key not cached")
			}
			return recipientPub, 1, nil
		}, auditRec)
	wireOutboxActorSigning(w, users.NewHandler(pool, logger, nil), func(context.Context) string { return wiringBaseURL })
	w.RunOnce(ctx)

	var (
		status       string
		wasEncrypted bool
	)
	for _, aid := range activityIDs {
		if err := pool.QueryRow(ctx,
			`SELECT status, was_encrypted FROM federation_outbox WHERE activity_id = $1 AND peer_id = $2`,
			aid, peerID,
		).Scan(&status, &wasEncrypted); err != nil {
			t.Fatalf("read outbox row: %v", err)
		}
		if status != tc.wantStatus {
			t.Errorf("outbox status: got %q want %q", status, tc.wantStatus)
		}
	}
	posted := inboxSrv.posts()
	if tc.wantStatus == "refused" {
		if len(posted) != 0 {
			t.Errorf("refused %s row reached the peer inbox: %s", tc.wantTier, posted[0])
		}
		return
	}
	if len(posted) != 1 {
		t.Fatalf("peer inbox POSTs: got %d want 1", len(posted))
	}
	body := posted[0]
	if wasEncrypted != tc.wantEncrypted {
		t.Errorf("was_encrypted: got %v want %v", wasEncrypted, tc.wantEncrypted)
	}
	if !tc.wantEncrypted {
		if !bytes.Contains(body, []byte(marker)) {
			t.Errorf("plaintext %s delivery lost its payload: %s", tc.wantTier, body)
		}
		return
	}
	if bytes.Contains(body, []byte(marker)) {
		t.Fatalf("encrypted %s delivery carries the payload in clear: %s", tc.wantTier, body)
	}
	env, err := federation.Unmarshal(body)
	if err != nil || env.Encryption == nil {
		t.Fatalf("encrypted delivery: unmarshal err=%v encryption=%v body=%s", err, env != nil && env.Encryption != nil, body)
	}
	plain, err := federation.DecryptActivityPayload(env.Encryption.Ciphertext, env.Encryption.Nonce, recipientPriv.Bytes(), senderPub)
	if err != nil {
		t.Fatalf("recipient cannot decrypt: %v", err)
	}
	if !bytes.Contains(plain, []byte(marker)) {
		t.Errorf("decrypted payload lacks the content: %s", plain)
	}
}

// setFirstAssetTier changes the post's first attached asset with
// the production query (assets.SetAssetSensitivity).
func setFirstAssetTier(tier string) func(*testing.T, context.Context, *pgxpool.Pool, uuid.UUID, []uuid.UUID) {
	return func(t *testing.T, ctx context.Context, pool *pgxpool.Pool, _ uuid.UUID, assetIDs []uuid.UUID) {
		t.Helper()
		if err := assets.New(pool).SetAssetSensitivity(ctx, assets.SetAssetSensitivityParams{
			ID: pgtype.UUID{Bytes: assetIDs[0], Valid: true}, Sensitivity: tier,
		}); err != nil {
			t.Fatalf("set asset sensitivity: %v", err)
		}
	}
}

// detachAssets removes every post_assets link of the post.
func detachAssets(t *testing.T, ctx context.Context, pool *pgxpool.Pool, postID uuid.UUID, _ []uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(ctx, `DELETE FROM post_assets WHERE post_id = $1`, postID); err != nil {
		t.Fatalf("detach assets: %v", err)
	}
}

// TestOutboxProductionWiring_DeliveryTimeRecheck pins the #1525
// correction: the Worker rechecks the object's tier just before
// sending and uses the stricter of that and the tier stamped at
// dispatch. A row queued as public whose asset becomes restricted
// or embargoed is refused (nothing POSTed) without E2E; a queued
// restricted row stays protected when its asset is lowered or
// detached; unchanged public / team rows are unaffected.
func TestOutboxProductionWiring_DeliveryTimeRecheck(t *testing.T) {
	cases := []outboxWiringCase{
		{name: "public row, asset raised to restricted, peer without E2E", assetTiers: []string{"public"},
			senderKey: true, recipientKey: true, beforeDelivery: setFirstAssetTier("restricted"),
			wantTier: "public", wantStatus: "refused"},
		{name: "public row, asset raised to embargo, peer without E2E", assetTiers: []string{"public"},
			senderKey: true, recipientKey: true, beforeDelivery: setFirstAssetTier("embargo"),
			wantTier: "public", wantStatus: "refused"},
		{name: "two public rows (batch path), asset raised to restricted, peer without E2E", assetTiers: []string{"public"},
			senderKey: true, recipientKey: true, activities: 2, beforeDelivery: setFirstAssetTier("restricted"),
			wantTier: "public", wantStatus: "refused"},
		{name: "team row, asset raised to embargo, peer E2E with keys", assetTiers: []string{"team"},
			peerE2E: true, recipientKey: true, senderKey: true, beforeDelivery: setFirstAssetTier("embargo"),
			wantTier: "team", wantStatus: "sent", wantEncrypted: true},
		{name: "public row, asset raised to restricted, peer E2E without recipient key", assetTiers: []string{"public"},
			peerE2E: true, senderKey: true, beforeDelivery: setFirstAssetTier("restricted"),
			wantTier: "public", wantStatus: "refused"},
		{name: "restricted row, asset lowered to public, peer without E2E", assetTiers: []string{"restricted"},
			senderKey: true, recipientKey: true, beforeDelivery: setFirstAssetTier("public"),
			wantTier: "restricted", wantStatus: "refused"},
		{name: "embargo row, asset detached, peer without E2E", assetTiers: []string{"embargo"},
			senderKey: true, recipientKey: true, beforeDelivery: detachAssets,
			wantTier: "embargo", wantStatus: "refused"},
		{name: "restricted row, asset lowered to public, peer E2E with keys", assetTiers: []string{"restricted"},
			peerE2E: true, recipientKey: true, senderKey: true, beforeDelivery: setFirstAssetTier("public"),
			wantTier: "restricted", wantStatus: "sent", wantEncrypted: true},
		{name: "public row unchanged, peer without E2E", assetTiers: []string{"public"},
			senderKey: true, recipientKey: true,
			wantTier: "public", wantStatus: "sent"},
		{name: "team row unchanged, peer without E2E", assetTiers: []string{"team"},
			senderKey: true, recipientKey: true,
			wantTier: "team", wantStatus: "sent"},
		{name: "team row, asset lowered to public, peer without E2E", assetTiers: []string{"team"},
			senderKey: true, recipientKey: true, beforeDelivery: setFirstAssetTier("public"),
			wantTier: "team", wantStatus: "sent"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { runOutboxWiringCase(t, tc) })
	}
}

// TestObjectSensitivity_Coverage pins the per-kind rule both
// federation directions share.
func TestObjectSensitivity_Coverage(t *testing.T) {
	pool := openPoolForSensitivity(t)
	t.Cleanup(pool.Close)
	ctx := context.Background()

	restricted := insertAssetAtTier(t, ctx, pool, "restricted")
	team := insertAssetAtTier(t, ctx, pool, "team")
	var userRef int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO "user" (username, fullname, approved) VALUES ($1, 'Sensitivity Coverage', 1) RETURNING ref`,
		"sens-cov-"+uuid.NewString()[:8],
	).Scan(&userRef); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	bare, withTeam, withRestricted, deletedRestricted := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, p := range []struct {
		id      uuid.UUID
		asset   *uuid.UUID
		deleted bool
	}{{bare, nil, false}, {withTeam, &team, false}, {withRestricted, &restricted, false}, {deletedRestricted, &restricted, true}} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO posts (id, author_user_ref, title, visibility, deleted_at)
			 VALUES ($1, $2, 'cov', 'explicit-share', CASE WHEN $3 THEN now() END)`,
			p.id, userRef, p.deleted,
		); err != nil {
			t.Fatalf("seed post: %v", err)
		}
		if p.asset != nil {
			if _, err := pool.Exec(ctx, `INSERT INTO post_assets (post_id, asset_id) VALUES ($1, $2)`, p.id, *p.asset); err != nil {
				t.Fatalf("attach: %v", err)
			}
		}
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM post_assets WHERE post_id = ANY($1)`, []uuid.UUID{bare, withTeam, withRestricted, deletedRestricted})
		_, _ = pool.Exec(c, `DELETE FROM posts WHERE author_user_ref = $1`, userRef)
		_, _ = pool.Exec(c, `DELETE FROM "user" WHERE ref = $1`, userRef)
	})

	for _, c := range []struct {
		kind string
		id   uuid.UUID
		want string
	}{
		{"asset", restricted, "restricted"},
		{"asset", team, "team"},
		{"post", bare, "public"},
		{"post", withTeam, "team"},
		{"post", withRestricted, "restricted"},
		{"post", deletedRestricted, "restricted"},
	} {
		got, err := objectSensitivity(ctx, pool, c.kind, c.id)
		if err != nil || got != c.want {
			t.Errorf("objectSensitivity(%s %s) = %q, %v; want %q", c.kind, c.id, got, err, c.want)
		}
	}
	if _, err := objectSensitivity(ctx, pool, "post", uuid.New()); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("missing post: err=%v want pgx.ErrNoRows", err)
	}
	for _, kind := range []string{"collection", "workspace", "brand_kit", "user", "comment"} {
		if _, err := objectSensitivity(ctx, pool, kind, uuid.New()); !errors.Is(err, errSensitivityKindUnsupported) {
			t.Errorf("%s: err=%v want errSensitivityKindUnsupported", kind, err)
		}
	}

	// Outbound: anything unresolvable is an error, which the
	// dispatcher records as unresolved (encryption required).
	out := outboxSensitivityLookup(pool)
	if got, err := out(ctx, "post", withRestricted); err != nil || got != outbox.SensitivityRestricted {
		t.Errorf("outbound post: %q, %v", got, err)
	}
	if _, err := out(ctx, "collection", uuid.New()); err == nil {
		t.Error("outbound unsupported kind: want an error (unresolved), got nil")
	}

	// Inbound: posts are now covered; unsupported kinds and
	// missing objects pass through.
	in := inboxSensitivityLookup(pool)
	if got, err := in(ctx, "post", withRestricted); err != nil || got != inbox.SensitivityRestricted {
		t.Errorf("inbound post with restricted asset: %q, %v; want restricted", got, err)
	}
	if got, err := in(ctx, "post", bare); err != nil || got != inbox.SensitivityPublic {
		t.Errorf("inbound bare post: %q, %v; want public", got, err)
	}
	for _, kind := range []string{"collection", "workspace", "brand_kit", "user"} {
		if got, err := in(ctx, kind, uuid.New()); err != nil || got != inbox.SensitivityNotFound {
			t.Errorf("inbound %s: %q, %v; want not_found pass-through", kind, got, err)
		}
	}
	if got, err := in(ctx, "post", uuid.New()); err != nil || got != inbox.SensitivityNotFound {
		t.Errorf("inbound missing post: %q, %v; want not_found", got, err)
	}
}
