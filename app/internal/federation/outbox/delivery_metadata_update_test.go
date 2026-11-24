// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package outbox_test

// #1577 sender through the real delivery worker: a metadata Update,
// recorded for one peer by assetsend.RecordMetadataUpdate, is sent as an
// actor-signed Update carrying exactly aa:asset and aa:metadata, singleton
// or batched, plaintext or sealed, and the same logical Update on a retry.
// A change after it was recorded that makes it ineligible refuses it for
// good before any request; it is never sent as a flags-only Update.
//
// The receiver is the stub of delivery_asset_version_test.go: this proves
// what the worker sends, not that a real receiver admits it (the signed
// two-server gate is separate).

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/assetsend"
	"github.com/mscrnt/artist-alley/app/internal/federation/outbox"
)

const mdProfiles = `["share-lifecycle-1","archival-1","original-transfer-1","asset-metadata-1"]`

var mdAllProfiles = federation.MemberContext{
	Negotiated: func(string) bool { return true },
	Applied:    func(federation.ActivityType) bool { return true },
}

// newMDFx is avFx with the metadata source wired, the peer holding all
// three profiles, and the actor key the envelopes are signed with.
func newMDFx(t *testing.T) (*avFx, ed25519.PublicKey) {
	t.Helper()
	f := newAVFx(t)
	if _, err := f.pool.Exec(context.Background(), `UPDATE federation_peers SET capabilities = $2::jsonb WHERE id = $1`, f.peer, mdProfiles); err != nil {
		t.Fatal(err)
	}
	f.worker.SetAssetMetadataUpdateMembers(assetsend.Source{Pool: f.pool}.MetadataUpdateMembers)
	return f, wireTestActorSigner(t, f.worker)
}

// mdWorker is a fresh worker wired as newAVFx and newMDFx wire theirs.
func (f *avFx) mdWorker(t *testing.T, metadata bool) (*outbox.Worker, ed25519.PublicKey) {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	w := outbox.NewWorker(outbox.DeliveryConfig{Interval: time.Hour, BatchSize: 100, RequestTimeout: 5 * time.Second},
		f.pool, &outbox.IdentitySigner{PrivateKey: priv, KeyURL: avLocal + "/federation/instance#main-key"},
		func(_ context.Context, id uuid.UUID) (outbox.PeerInfo, error) {
			return outbox.PeerInfo{ID: id, InstanceURL: f.stub.srv.URL, Enabled: true, Connected: true}, nil
		}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	pub := wireTestActorSigner(t, w)
	w.SetAssetVersionMember(assetsend.Source{Pool: f.pool}.Member)
	w.SetAssetFlagUpdateMember(assetsend.Source{Pool: f.pool}.FlagUpdateMember)
	if metadata {
		w.SetAssetMetadataUpdateMembers(assetsend.Source{Pool: f.pool}.MetadataUpdateMembers)
	}
	return w, pub
}

// shared is a sealed public asset described as given, granted to the
// peer, its aa:Share sent and its announcement still queued (not built,
// so not claimed) and not due.
func (f *avFx) shared(title, description string) (avAsset, avGrant) {
	f.t.Helper()
	ctx := context.Background()
	a := f.newAsset("public", true)
	if _, err := f.pool.Exec(ctx, `UPDATE assets SET title = $2, description = $3 WHERE id = $1`, a.id, title, description); err != nil {
		f.t.Fatal(err)
	}
	g := f.grant(a, true)
	if _, err := f.pool.Exec(ctx, `UPDATE federation_outbox SET next_attempt_at = now() + interval '1 hour' WHERE activity_id = $1`, g.announcement); err != nil {
		f.t.Fatal(err)
	}
	f.worker.RunOnce(ctx) // the aa:Share only
	if st, _, _ := f.row(g.shareActivity); st != "sent" {
		f.t.Fatalf("the aa:Share is %s", st)
	}
	// Sending the aa:Share made the announcement due again; keep it out
	// of the rounds that follow.
	if _, err := f.pool.Exec(ctx, `UPDATE federation_outbox SET next_attempt_at = now() + interval '1 hour' WHERE activity_id = $1`, g.announcement); err != nil {
		f.t.Fatal(err)
	}
	return a, g
}

// recordMD records a metadata Update of a to the peer and queues it as the
// dispatcher does (target names a recipient actor: "" for none).
func (f *avFx) recordMD(a avAsset, target string) (uuid.UUID, string) {
	f.t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	if err := pgx.BeginFunc(ctx, f.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM assets WHERE id = $1 FOR UPDATE`, a.id); err != nil {
			return err
		}
		var err error
		id, err = assetsend.RecordMetadataUpdate(ctx, tx, f.writer, assetsend.MetadataChange{AssetID: a.id, PeerID: f.peer, BaseURL: avLocal,
			Username: func(context.Context, int64) string { return "alice" }})
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
	if id == uuid.Nil {
		f.t.Fatal("nothing recorded")
	}
	var uri string
	if err := f.pool.QueryRow(ctx, `SELECT activity_uri FROM activities WHERE id = $1`, id).Scan(&uri); err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO federation_outbox (activity_id, peer_id, sensitivity, next_attempt_at, target_user_url)
		VALUES ($1, $2, 'public', now(), NULLIF($3, ''))`, id, f.peer, target); err != nil {
		f.t.Fatal(err)
	}
	return id, uri
}

// envelope is the received envelope with id uri, or nil.
func (f *avFx) envelope(uri string) []byte {
	f.stub.mu.Lock()
	defer f.stub.mu.Unlock()
	for _, e := range f.stub.got {
		var v struct{ ID string }
		_ = json.Unmarshal(e, &v)
		if v.ID == uri {
			return e
		}
	}
	return nil
}

// checkSent checks a plaintext metadata Update as it was received: its
// actor signature verifies over the envelope, it carries exactly aa:asset
// and aa:metadata and passes the receiver's member checks, nothing local
// is on it, and its aa:metadata is want.
func checkSent(t *testing.T, raw []byte, pub ed25519.PublicKey, want string) *federation.Envelope {
	t.Helper()
	env, err := federation.Unmarshal(raw)
	if err != nil {
		t.Fatalf("not an envelope: %v", err)
	}
	pubPEM, err := federation.PublicKeyToPEM(pub)
	if err != nil {
		t.Fatal(err)
	}
	if err := env.Verify(pubPEM); err != nil {
		t.Fatalf("the actor signature does not verify: %v", err)
	}
	if env.Type != federation.ActivityUpdate || env.Encryption != nil {
		t.Fatalf("type %s, encryption %v", env.Type, env.Encryption != nil)
	}
	members := make([]string, 0, len(env.Extra))
	for k := range env.Extra {
		members = append(members, k)
	}
	slices.Sort(members)
	if !slices.Equal(members, []string{federation.MemberArchivalAsset, federation.MemberAssetMetadata}) {
		t.Fatalf("members %v", members)
	}
	if string(env.Extra[federation.MemberAssetMetadata]) != want {
		t.Fatalf("aa:metadata %s, want %s", env.Extra[federation.MemberAssetMetadata], want)
	}
	if err := federation.ValidateMembers(env, mdAllProfiles); err != nil {
		t.Fatalf("the receiver's member checks: %v", err)
	}
	for _, local := range []string{"share_id", "share_activity_id", "version_id", federation.MetadataCaptureKey,
		federation.AssetMetadataMemberKey, `"omitted"`, `"v":1`} {
		if strings.Contains(string(raw), local) {
			t.Errorf("%q is on the wire", local)
		}
	}
	// Tampering with the member breaks the signature.
	tampered := *env
	tampered.Extra = map[string]json.RawMessage{federation.MemberArchivalAsset: env.Extra[federation.MemberArchivalAsset],
		federation.MemberAssetMetadata: json.RawMessage(`{"title":"Forged","description":null,"rights":null,"requiredStatement":null}`)}
	if err := tampered.Verify(pubPEM); err == nil {
		t.Fatal("a tampered envelope verifies")
	}
	return env
}

func TestDelivery_MetadataUpdate_SingletonSignedBeforeTheAnnouncement(t *testing.T) {
	f, pub := newMDFx(t)
	ctx := context.Background()
	a, g := f.shared("Harbour <at> dawn & dusk", "")
	id, uri := f.recordMD(a, "")
	hits := f.stub.hits

	f.worker.RunOnce(ctx)
	raw := f.envelope(uri)
	if raw == nil {
		t.Fatalf("not sent (%v)", f.stub.types())
	}
	if f.stub.hits != hits+1 {
		t.Fatalf("%d requests", f.stub.hits-hits)
	}
	want := `{"title":"Harbour \u003cat\u003e dawn \u0026 dusk","description":null,"rights":null,"requiredStatement":null}`
	env := checkSent(t, raw, pub, want)
	if env.Actor != avLocal+"/users/alice" || env.Object != avLocal+"/assets/"+a.id.String() {
		t.Fatalf("actor %s object %s", env.Actor, env.Object)
	}
	m, err := federation.ParseArchivalAsset(env.Extra[federation.MemberArchivalAsset])
	if err != nil || m.Version.Number != 1 || m.Description != nil || m.Rights != "" || m.Files[0].ContentURI != "cas://"+a.hash {
		t.Fatalf("aa:asset %+v %v", m, err)
	}
	// Sent before the announcement of its version, which is still queued
	// and was never built (no claims).
	if st, _, _ := f.row(id); st != "sent" {
		t.Fatalf("the Update is %s", st)
	}
	var claims int
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM federation_asset_announcement_claims WHERE asset_id = $1`, a.id).Scan(&claims)
	if st, _, _ := f.row(g.announcement); st != "queued" || claims != 0 {
		t.Fatalf("the announcement is %s with %d claims", st, claims)
	}
}

func TestDelivery_MetadataUpdate_TwoForOnePeerAreBatched(t *testing.T) {
	f, pub := newMDFx(t)
	ctx := context.Background()
	a, _ := f.shared("First", "")
	b, _ := f.shared("", "")
	_, uriA := f.recordMD(a, "")
	_, uriB := f.recordMD(b, "")
	hits := f.stub.hits
	f.worker.RunOnce(ctx)
	if f.stub.hits != hits+1 {
		t.Fatalf("%d requests for two Updates, want one batch", f.stub.hits-hits)
	}
	checkSent(t, f.envelope(uriA), pub, `{"title":"First","description":null,"rights":null,"requiredStatement":null}`)
	checkSent(t, f.envelope(uriB), pub, `{"title":null,"description":null,"rights":null,"requiredStatement":null}`)
}

// A failed first request is retried by a fresh worker as the same
// activity: the same id, published, flags and aa:metadata, and no new
// activity.
func TestDelivery_MetadataUpdate_ARetryIsTheSameUpdate(t *testing.T) {
	f, _ := newMDFx(t)
	ctx := context.Background()
	a, _ := f.shared("Frozen", "Body")
	id, uri := f.recordMD(a, "")
	var activities int
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM activities WHERE actor_user_ref = $1`, f.owner).Scan(&activities)
	f.stub.mu.Lock()
	f.stub.unavailable = string(federation.ActivityUpdate)
	f.stub.mu.Unlock()
	f.worker.RunOnce(ctx)
	if st, attempts, _ := f.row(id); st != "queued" || attempts != 1 || f.envelope(uri) != nil {
		t.Fatalf("after the 503: %s, %d attempts", st, attempts)
	}

	// Edits and a flags change after recording change nothing it says.
	if _, err := f.pool.Exec(ctx, `UPDATE assets SET title = 'Edited', mature = TRUE WHERE id = $1`, a.id); err != nil {
		t.Fatal(err)
	}
	f.stub.mu.Lock()
	f.stub.unavailable = ""
	f.stub.mu.Unlock()
	if _, err := f.pool.Exec(ctx, `UPDATE federation_outbox SET next_attempt_at = now() WHERE activity_id = $1`, id); err != nil {
		t.Fatal(err)
	}
	w, pub := f.mdWorker(t, true)
	w.RunOnce(ctx)
	raw := f.envelope(uri)
	if raw == nil {
		t.Fatal("not sent on the retry")
	}
	env := checkSent(t, raw, pub, `{"title":"Frozen","description":"Body","rights":null,"requiredStatement":null}`)
	var published time.Time
	_ = f.pool.QueryRow(ctx, `SELECT published_at FROM activities WHERE id = $1`, id).Scan(&published)
	m, _ := federation.ParseArchivalAsset(env.Extra[federation.MemberArchivalAsset])
	if !env.Published.Equal(published) || m.Mature == nil || *m.Mature {
		t.Fatalf("published %s (recorded %s), mature %v", env.Published, published, m.Mature)
	}
	var after int
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM activities WHERE actor_user_ref = $1`, f.owner).Scan(&after)
	if after != activities {
		t.Fatalf("%d activities after the retry, was %d", after, activities)
	}
}

// A change after recording that makes the Update ineligible refuses it
// for good before any request: never a flags-only Update in its place.
func TestDelivery_MetadataUpdate_RefusedWithoutARequest(t *testing.T) {
	steps := []struct {
		name   string
		change func(f *avFx, a avAsset, g avGrant, id uuid.UUID)
	}{
		{"the share revoked", func(f *avFx, _ avAsset, g avGrant, _ uuid.UUID) { f.revokeShare(g) }},
		{"the share expired", func(f *avFx, _ avAsset, g avGrant, _ uuid.UUID) {
			f.mdExec(`UPDATE federation_shares SET expires_at = now() - interval '1 hour' WHERE id = $1`, g.share)
		}},
		{"the peer disabled in the database (the worker's lookup still says enabled)", func(f *avFx, _ avAsset, _ avGrant, _ uuid.UUID) {
			f.mdExec(`UPDATE federation_peers SET enabled = FALSE WHERE id = $1`, f.peer)
		}},
		{"the peer dropped asset-metadata-1", func(f *avFx, _ avAsset, _ avGrant, _ uuid.UUID) {
			f.mdExec(`UPDATE federation_peers SET capabilities = capabilities - 'asset-metadata-1' WHERE id = $1`, f.peer)
		}},
		{"the asset's tier changed", func(f *avFx, a avAsset, _ avGrant, _ uuid.UUID) {
			f.mdExec(`UPDATE assets SET sensitivity = 'team' WHERE id = $1`, a.id)
		}},
		{"the asset deleted", func(f *avFx, a avAsset, _ avGrant, _ uuid.UUID) {
			f.mdExec(`UPDATE assets SET deleted_at = now() WHERE id = $1`, a.id)
		}},
		{"the title's source gated", func(f *avFx, _ avAsset, _ avGrant, _ uuid.UUID) {
			f.gateTitle()
		}},
		{"the capture malformed in the record", func(f *avFx, _ avAsset, _ avGrant, id uuid.UUID) {
			f.mdExec(`UPDATE activities SET payload = jsonb_set(payload, '{metadata_capture}', 'null') WHERE id = $1`, id)
		}},
		{"a wire aa:metadata injected into the record", func(f *avFx, _ avAsset, _ avGrant, id uuid.UUID) {
			f.mdExec(`UPDATE activities SET payload = payload || '{"aa:metadata":{"title":"Injected","description":null,"rights":null,"requiredStatement":null}}'::jsonb
				WHERE id = $1`, id)
		}},
		{"a built aa:asset injected into the record", func(f *avFx, _ avAsset, _ avGrant, id uuid.UUID) {
			f.mdExec(`UPDATE activities SET payload = payload || '{"asset":{"id":"x"}}'::jsonb WHERE id = $1`, id)
		}},
	}
	for _, s := range steps {
		t.Run(s.name, func(t *testing.T) {
			f, _ := newMDFx(t)
			a, g := f.shared("Secret title", "")
			id, uri := f.recordMD(a, "")
			s.change(f, a, g, id)
			hits := f.stub.hits
			f.worker.RunOnce(context.Background())
			if st, _, _ := f.row(id); st != "failed" || f.envelope(uri) != nil || f.stub.hits != hits {
				t.Fatalf("status %s, %d requests", st, f.stub.hits-hits)
			}
			var lastErr string
			_ = f.pool.QueryRow(context.Background(), `SELECT COALESCE(last_error, '') FROM federation_outbox WHERE activity_id = $1`, id).Scan(&lastErr)
			if strings.Contains(lastErr, "Secret") || strings.Contains(lastErr, "Injected") {
				t.Fatalf("the error carries a value: %s", lastErr)
			}
		})
	}

	t.Run("no metadata source wired: refused, not sent as flags", func(t *testing.T) {
		f, _ := newMDFx(t)
		a, _ := f.shared("Title", "")
		id, uri := f.recordMD(a, "")
		w, _ := f.mdWorker(t, false)
		hits := f.stub.hits
		w.RunOnce(context.Background())
		if st, _, _ := f.row(id); st != "failed" || f.envelope(uri) != nil || f.stub.hits != hits {
			t.Fatalf("status %s, %d requests", st, f.stub.hits-hits)
		}
	})

	t.Run("the aa:Share not delivered yet waits", func(t *testing.T) {
		f, _ := newMDFx(t)
		a, g := f.shared("Title", "")
		id, uri := f.recordMD(a, "")
		f.mdExec(`UPDATE federation_outbox SET status = 'queued' WHERE activity_id = $1 AND peer_id = $2`, g.shareActivity, f.peer)
		f.worker.RunOnce(context.Background())
		if st, attempts, _ := f.row(id); st != "queued" || attempts != 1 || f.envelope(uri) != nil {
			t.Fatalf("status %s, %d attempts", st, attempts)
		}
	})
}

// In one batch, a refused Update is not sent and the other is.
func TestDelivery_MetadataUpdate_ABatchSendsOnlyWhatIsStillEligible(t *testing.T) {
	f, pub := newMDFx(t)
	a, _ := f.shared("Kept", "")
	b, gb := f.shared("Withdrawn grant", "")
	_, uriA := f.recordMD(a, "")
	idB, uriB := f.recordMD(b, "")
	f.revokeShare(gb)
	f.worker.RunOnce(context.Background())
	checkSent(t, f.envelope(uriA), pub, `{"title":"Kept","description":null,"rights":null,"requiredStatement":null}`)
	if st, _, _ := f.row(idB); st != "failed" || f.envelope(uriB) != nil {
		t.Fatalf("the refused Update is %s", st)
	}
}

// Sealed for the recipient, neither member is in clear; the sealed
// payload holds exactly both, and the signature covers the ciphertext.
func TestDelivery_MetadataUpdate_SealedCarriesBothMembersInside(t *testing.T) {
	f, pub := newMDFx(t)
	ctx := context.Background()
	senderPub := seedSenderKeypair(t, ctx, f.pool, f.owner)
	recipientPub, recipientPriv := freshRecipientKeypair(t)
	wireHooks(t, f.worker, true, recipientPub)
	a, _ := f.shared("Sealed title", "")
	_, uri := f.recordMD(a, "https://receiver.example/users/bob")
	f.worker.RunOnce(ctx)
	raw := f.envelope(uri)
	if raw == nil {
		t.Fatal("not sent")
	}
	env, err := federation.Unmarshal(raw)
	if err != nil || env.Encryption == nil {
		t.Fatalf("not sealed: %v", err)
	}
	for _, m := range []string{federation.MemberArchivalAsset, federation.MemberAssetMetadata} {
		if _, ok := env.Extra[m]; ok {
			t.Fatalf("%s in clear beside the encryption block", m)
		}
	}
	if strings.Contains(string(raw), "Sealed title") {
		t.Fatal("the title is in clear")
	}
	pubPEM, _ := federation.PublicKeyToPEM(pub)
	if err := env.Verify(pubPEM); err != nil {
		t.Fatalf("the signature over the sealed envelope: %v", err)
	}
	pt, err := federation.DecryptActivityPayload(env.Encryption.Ciphertext, env.Encryption.Nonce, recipientPriv, senderPub)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	var sealed map[string]json.RawMessage
	if err := json.Unmarshal(pt, &sealed); err != nil {
		t.Fatal(err)
	}
	if string(sealed[federation.MemberAssetMetadata]) != `{"title":"Sealed title","description":null,"rights":null,"requiredStatement":null}` {
		t.Fatalf("sealed aa:metadata %s", sealed[federation.MemberAssetMetadata])
	}
	if _, err := federation.ParseArchivalAsset(sealed[federation.MemberArchivalAsset]); err != nil {
		t.Fatalf("sealed aa:asset: %v", err)
	}
	for _, local := range []string{federation.MetadataCaptureKey, "share_id", "version_id"} {
		if _, ok := sealed[local]; ok {
			t.Errorf("%q in the sealed payload", local)
		}
	}
	// The receiver's checks pass on the decrypted envelope.
	clear := *env
	clear.Encryption = nil
	clear.Extra = sealed
	if err := federation.ValidateMembers(&clear, mdAllProfiles); err != nil {
		t.Fatalf("the decrypted envelope: %v", err)
	}
}

func (f *avFx) mdExec(q string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(), q, args...); err != nil {
		f.t.Fatal(err)
	}
}

// gateTitle gates the shipped title mirror, as it is put back after the
// test.
func (f *avFx) gateTitle() {
	f.t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	var readCap *string
	if err := f.pool.QueryRow(ctx, `SELECT id, read_capability FROM field_definition WHERE code = 'title' AND mirrors_column = 'title'`).
		Scan(&id, &readCap); err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `UPDATE field_definition SET read_capability = $2 WHERE id = $1`, id, readCap)
	})
	f.mdExec(`UPDATE field_definition SET read_capability = 'system.admin' WHERE id = $1`, id)
}

var _ = errors.Is

// C1: through the real worker, an Update whose recorded identity lost its
// actor, object kind or object id is refused for good (status failed)
// without a request, not retried.
func TestDelivery_MetadataUpdate_AMissingIdentityIsRefusedNotRetried(t *testing.T) {
	for _, column := range []string{"actor_user_ref", "object_kind", "object_local_id"} {
		t.Run(column, func(t *testing.T) {
			f, _ := newMDFx(t)
			ctx := context.Background()
			a, _ := f.shared("", "")
			id, uri := f.recordMD(a, "")
			var actor *int64
			var kind, local *string
			if err := f.pool.QueryRow(ctx, `SELECT actor_user_ref, object_kind, object_local_id FROM activities WHERE id = $1`, id).
				Scan(&actor, &kind, &local); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_, _ = f.pool.Exec(context.Background(), `UPDATE activities SET actor_user_ref = $2, object_kind = $3, object_local_id = $4 WHERE id = $1`,
					id, actor, kind, local)
			})
			f.mdExec(`UPDATE activities SET `+column+` = NULL WHERE id = $1`, id)
			hits := f.stub.hits
			f.worker.RunOnce(ctx)
			if st, _, _ := f.row(id); st != "failed" || f.envelope(uri) != nil || f.stub.hits != hits {
				var lastErr string
				_ = f.pool.QueryRow(ctx, `SELECT COALESCE(last_error, '') FROM federation_outbox WHERE activity_id = $1`, id).Scan(&lastErr)
				t.Fatalf("status %s, %d requests, last error %q", st, f.stub.hits-hits, lastErr)
			}
		})
	}
}

// C1: a peer whose stored intersection lost one of the three profiles is
// refused before any request, with profile_not_negotiated and the token
// as the recorded cause, although the worker's peer lookup still reports
// it enabled and connected.
func TestDelivery_MetadataUpdate_AMissingProfileIsTheRecordedCause(t *testing.T) {
	for _, token := range []string{federation.ProfileAssetMetadata1, federation.ProfileArchival1, federation.ProfileOriginalTransfer1} {
		t.Run(token, func(t *testing.T) {
			f, _ := newMDFx(t)
			ctx := context.Background()
			a, _ := f.shared("Secret title", "")
			id, uri := f.recordMD(a, "")
			f.mdExec(`UPDATE federation_peers SET capabilities = capabilities - $2::text WHERE id = $1`, f.peer, token)
			hits := f.stub.hits
			f.worker.RunOnce(ctx)
			var lastErr string
			_ = f.pool.QueryRow(ctx, `SELECT COALESCE(last_error, '') FROM federation_outbox WHERE activity_id = $1`, id).Scan(&lastErr)
			if st, _, _ := f.row(id); st != "failed" || f.envelope(uri) != nil || f.stub.hits != hits {
				t.Fatalf("status %s, %d requests", st, f.stub.hits-hits)
			}
			if !strings.Contains(lastErr, federation.ErrProfileNotNegotiated.Error()) || !strings.Contains(lastErr, token) || strings.Contains(lastErr, "Secret") {
				t.Fatalf("recorded cause %q", lastErr)
			}
		})
	}
}
