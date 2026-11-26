// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1574 sender against a live database: what Record announces, and what
// the delivery worker's Source.Member builds, defers or refuses, from
// real asset, version, share and outbox rows. Skips without
// AA_DB_PASSWORD.

package assetsend_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/activities"
	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/assetsend"
	"github.com/mscrnt/artist-alley/app/internal/provenance"
)

const base = "https://sender.example"

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

type fx struct {
	t      *testing.T
	pool   *pgxpool.Pool
	owner  int64
	peer   uuid.UUID
	writer *activities.Writer
	src    assetsend.Source
}

func newFx(t *testing.T) *fx {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool, err := pgxpool.New(context.Background(), fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=disable pool_max_conns=8",
		env("AA_DB_HOST", "postgres"), env("AA_DB_PORT", "5432"), env("AA_DB_USER", "artist_alley"),
		pwd, env("AA_DB_NAME", "artist_alley")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	f := &fx{t: t, pool: pool, writer: activities.NewWriter(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil),
		src: assetsend.Source{Pool: pool}}
	ctx := context.Background()
	if err := pool.QueryRow(ctx, `INSERT INTO "user" (username, fullname, approved) VALUES ($1, 'Owner', 1) RETURNING ref`,
		"as-"+uuid.NewString()[:8]).Scan(&f.owner); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO federation_peers (instance_url, display_name, instance_public_key, enabled, status,
		    handshake_by_user_ref, capabilities)
		VALUES ($1, 'receiver', 'k', TRUE, 'connected', $2, '["share-lifecycle-1","archival-1","original-transfer-1"]'::jsonb)
		RETURNING id`, "https://receiver-"+uuid.NewString()[:8]+".example", f.owner).Scan(&f.peer); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM federation_peers WHERE id = $1`, f.peer) // shares and outbox rows cascade
		_, _ = pool.Exec(c, `DELETE FROM activities WHERE actor_user_ref = $1`, f.owner)
	})
	return f
}

type asset struct {
	id, version uuid.UUID
	hash        string
	uri         string
}

// newAsset is a local asset with 3 bytes stored and verified, owned by
// the fixture's owner; sealed seals its version 1. Its base name is
// unknown.
func (f *fx) newAsset(tier string, sealed bool) asset {
	f.t.Helper()
	return f.newAssetNamed(tier, sealed, nil, provenance.SourceUnknown)
}

// newAssetNamed is newAsset with the given base name pair (#1595), which
// the version 1 trigger copies onto the version.
func (f *fx) newAssetNamed(tier string, sealed bool, basename *string, source provenance.Source) asset {
	f.t.Helper()
	ctx := context.Background()
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	a := asset{id: uuid.New(), hash: hex.EncodeToString(b)}
	a.uri = base + "/assets/" + a.id.String()
	for _, q := range []string{
		`INSERT INTO storage_objects (hash, size_bytes, content_type, backend) VALUES ($1, 3, 'image/png', 'fs')`,
		`INSERT INTO storage_variants (object_hash, variant_key, size_bytes, content_type, content_sha256, content_size_bytes,
		    digest_source, fixity_status) VALUES ($1, 'original', 3, 'image/png', $1, 3, 'write', 'verified')`,
	} {
		if _, err := f.pool.Exec(ctx, q, a.hash); err != nil {
			f.t.Fatal(err)
		}
	}
	if err := f.pool.QueryRow(ctx, `INSERT INTO assets (id, title, asset_type, owner_user_ref, file_hash, file_size_bytes, sensitivity,
		    original_basename, original_basename_source)
		VALUES ($1, 'Sender test', 1, $2, $3, 3, $4, $5, $6) RETURNING current_version_id`, a.id, f.owner, a.hash, tier,
		basename, string(source)).Scan(&a.version); err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() {
		c := context.Background()
		_, _ = f.pool.Exec(c, `DELETE FROM assets WHERE id = $1`, a.id)
		_, _ = f.pool.Exec(c, `DELETE FROM storage_pins WHERE object_hash = $1`, a.hash)
		_, _ = f.pool.Exec(c, `DELETE FROM storage_variants WHERE object_hash = $1`, a.hash)
		_, _ = f.pool.Exec(c, `DELETE FROM storage_objects WHERE hash = $1`, a.hash)
	})
	if sealed {
		f.seal(a)
	}
	return a
}

func (f *fx) seal(a asset) {
	f.t.Helper()
	if err := pgx.BeginFunc(context.Background(), f.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `SELECT asset_version_seal($1, '[]'::jsonb)`, a.version)
		return err
	}); err != nil {
		f.t.Fatalf("seal: %v", err)
	}
}

type grant struct {
	share, shareActivity, announcement uuid.UUID
	objectURI                          string
	payload                            []byte
}

// grantAndRecord writes an aa:Share activity and its original-file share
// row for a, then Record, in one transaction, as the grant handler does.
func (f *fx) grantAndRecord(a asset, mut func(sql *string)) grant {
	f.t.Helper()
	g, err := f.grantAndRecordErr(a, mut)
	if err != nil {
		f.t.Fatal(err)
	}
	return g
}

// grantAndRecordErr is grantAndRecord returning its error (safe outside
// the test goroutine).
func (f *fx) grantAndRecordErr(a asset, mut func(sql *string)) (grant, error) {
	ctx := context.Background()
	g := grant{share: uuid.New()}
	if err := pgx.BeginFunc(ctx, f.pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO activities (activity_uri, activity_type, actor_uri, actor_user_ref, payload)
			VALUES ($1, 'aa:Share', $2, $3, '{}'::jsonb) RETURNING id`, base+"/activities/"+uuid.NewString(),
			base+"/users/owner", f.owner).Scan(&g.shareActivity); err != nil {
			return err
		}
		ins := `INSERT INTO federation_shares (id, grantor_user_ref, object_kind, object_id, peer_id, scope, granted_activity_id, allow_original)
			VALUES ($1, $2, 'asset', $3, $4, 'view', $5, TRUE)`
		if mut != nil {
			mut(&ins)
		}
		if _, err := tx.Exec(ctx, ins, g.share, f.owner, a.id, f.peer, g.shareActivity); err != nil {
			return err
		}
		id, err := assetsend.Record(ctx, tx, f.writer, assetsend.Grant{ShareID: g.share, ShareActivityID: g.shareActivity,
			AssetID: a.id, PeerID: f.peer, PeerURL: "https://receiver.example", BaseURL: base,
			Username: func(context.Context, int64) string { return "owner" }})
		g.announcement = id
		return err
	}); err != nil {
		return g, err
	}
	if g.announcement != uuid.Nil {
		if err := f.pool.QueryRow(ctx, `SELECT object_uri, payload FROM activities WHERE id = $1`, g.announcement).
			Scan(&g.objectURI, &g.payload); err != nil {
			return g, err
		}
	}
	return g, nil
}

// shareDelivered records the aa:Share's outbox row to the peer in status.
func (f *fx) shareDelivered(g grant, status string) {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO federation_outbox (activity_id, peer_id, status)
		VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, g.shareActivity, f.peer, status); err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.pool.Exec(context.Background(), `UPDATE federation_outbox SET status = $3 WHERE activity_id = $1 AND peer_id = $2`,
		g.shareActivity, f.peer, status); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fx) member(g grant) (federation.ArchivalAsset, error) {
	f.t.Helper()
	raw, err := f.src.Member(context.Background(), g.announcement, f.peer, g.objectURI, g.payload)
	if err != nil {
		return federation.ArchivalAsset{}, err
	}
	a, perr := federation.ParseArchivalAsset(raw)
	if perr != nil {
		f.t.Fatalf("Member returned a member §17.2 refuses: %v (%s)", perr, raw)
	}
	return *a, nil
}

func TestRecord_AnnouncesOnlyAnEligibleGrant(t *testing.T) {
	f := newFx(t)
	a := f.newAsset("public", true)
	g := f.grantAndRecord(a, nil)
	if g.announcement == uuid.Nil {
		t.Fatal("an eligible grant recorded nothing")
	}
	var actor, typ string
	var ref int64
	if err := f.pool.QueryRow(context.Background(), `SELECT activity_type, actor_uri, actor_user_ref FROM activities WHERE id = $1`,
		g.announcement).Scan(&typ, &actor, &ref); err != nil {
		t.Fatal(err)
	}
	if typ != string(federation.ActivityAAAssetVersion) || actor != base+"/users/owner" || ref != f.owner || g.objectURI != a.uri {
		t.Fatalf("recorded %s by %s (%d) about %s", typ, actor, ref, g.objectURI)
	}
	var p map[string]string
	_ = json.Unmarshal(g.payload, &p)
	if p["version_id"] != a.version.String() || p["share_id"] != g.share.String() || p["share_activity_id"] != g.shareActivity.String() {
		t.Fatalf("payload %v", p)
	}

	// Not announced: a restricted asset, a deleted one, a received copy, a
	// peer without both profiles.
	restricted := f.newAsset("restricted", true)
	if g := f.grantAndRecord(restricted, nil); g.announcement != uuid.Nil {
		t.Error("a restricted asset was announced")
	}
	deleted := f.newAsset("public", true)
	if _, err := f.pool.Exec(context.Background(), `UPDATE assets SET deleted_at = now() WHERE id = $1`, deleted.id); err != nil {
		t.Fatal(err)
	}
	if g := f.grantAndRecord(deleted, nil); g.announcement != uuid.Nil {
		t.Error("a deleted asset was announced")
	}
	for _, caps := range []string{`["share-lifecycle-1","archival-1"]`, `["share-lifecycle-1","original-transfer-1"]`} {
		if _, err := f.pool.Exec(context.Background(), `UPDATE federation_peers SET capabilities = $2::jsonb WHERE id = $1`, f.peer, caps); err != nil {
			t.Fatal(err)
		}
		if g := f.grantAndRecord(f.newAsset("public", true), nil); g.announcement != uuid.Nil {
			t.Errorf("announced to a peer with %s", caps)
		}
	}
}

func TestMember_BuildsTheSealedVersionAfterTheGrantIsDelivered(t *testing.T) {
	f := newFx(t)
	a := f.newAsset("team", true)
	g := f.grantAndRecord(a, nil)

	// The grant's aa:Share has not reached the peer: not yet.
	if _, err := f.member(g); !errors.Is(err, assetsend.ErrNotReady) {
		t.Fatalf("before the aa:Share is queued: %v, want not ready", err)
	}
	f.shareDelivered(g, "queued")
	if _, err := f.member(g); !errors.Is(err, assetsend.ErrNotReady) {
		t.Fatalf("while the aa:Share is queued: %v, want not ready", err)
	}
	f.shareDelivered(g, "sent")
	m, err := f.member(g)
	if err != nil {
		t.Fatalf("after the aa:Share was sent: %v", err)
	}
	if m.ID != a.uri || m.Version.ID != a.uri+"/versions/1" || m.Version.Number != 1 || m.Version.Predecessor != nil ||
		m.Sensitivity != "team" || m.Mature == nil || *m.Mature {
		t.Fatalf("member %+v", m)
	}
	if len(m.Files) != 1 {
		t.Fatalf("%d files, want the sealed original only", len(m.Files))
	}
	file := m.Files[0]
	if file.Role != "original" || file.ContentURI != "cas://"+a.hash || file.Size == nil || *file.Size != 3 ||
		file.MediaType != "image/png" || len(file.Checksums) != 1 || file.Checksums[0].Algorithm != "sha256" ||
		file.Checksums[0].Value != a.hash {
		t.Fatalf("file %+v", file)
	}
	// Stable: a retry builds the same member.
	again, err := f.src.Member(context.Background(), g.announcement, f.peer, g.objectURI, g.payload)
	first, _ := f.src.Member(context.Background(), g.announcement, f.peer, g.objectURI, g.payload)
	if err != nil || string(again) != string(first) {
		t.Fatalf("retry built another member: %v", err)
	}
}

func TestMember_DefersADraftAndRefusesWhatIsNoLongerGranted(t *testing.T) {
	f := newFx(t)
	draft := f.newAsset("public", false)
	g := f.grantAndRecord(draft, nil)
	f.shareDelivered(g, "sent")
	if _, err := f.member(g); !errors.Is(err, assetsend.ErrNotReady) {
		t.Fatalf("a draft version: %v, want not ready", err)
	}
	f.seal(draft)
	if _, err := f.member(g); err != nil {
		t.Fatalf("once sealed: %v", err)
	}

	refusedWhen := func(name string, change func(a asset, g grant)) {
		t.Helper()
		a := f.newAsset("public", true)
		g := f.grantAndRecord(a, nil)
		f.shareDelivered(g, "sent")
		change(a, g)
		if _, err := f.member(g); !errors.Is(err, federation.ErrNotFederated) {
			t.Errorf("%s: %v, want refused for good", name, err)
		}
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := f.pool.Exec(context.Background(), sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	refusedWhen("a revoked share", func(_ asset, g grant) {
		exec(`UPDATE federation_shares SET revoked_at = now(), revoked_activity_id = $2 WHERE id = $1`, g.share, g.shareActivity)
	})
	refusedWhen("an expired share", func(_ asset, g grant) {
		exec(`UPDATE federation_shares SET expires_at = now() - interval '1 minute' WHERE id = $1`, g.share)
	})
	refusedWhen("an aa:Share that was not delivered", func(_ asset, g grant) {
		exec(`UPDATE federation_outbox SET status = 'failed' WHERE activity_id = $1`, g.shareActivity)
	})
	refusedWhen("an asset made restricted", func(a asset, _ grant) {
		exec(`UPDATE assets SET sensitivity = 'restricted' WHERE id = $1`, a.id)
	})
	refusedWhen("an asset deleted", func(a asset, _ grant) {
		exec(`UPDATE assets SET deleted_at = now() WHERE id = $1`, a.id)
	})

	// A record that names another object, or no share.
	a := f.newAsset("public", true)
	g = f.grantAndRecord(a, nil)
	f.shareDelivered(g, "sent")
	other := g
	other.objectURI = base + "/assets/" + uuid.NewString()
	if _, err := f.member(other); !errors.Is(err, federation.ErrNotFederated) {
		t.Errorf("another object: %v", err)
	}
	other = g
	other.payload = []byte(`{"asset_id":"` + a.id.String() + `"}`)
	if _, err := f.member(other); !errors.Is(err, federation.ErrNotFederated) {
		t.Errorf("no share named: %v", err)
	}
	// A share that is not this peer's whole-peer original grant.
	for name, ins := range map[string]string{
		"without the original": `INSERT INTO federation_shares (id, grantor_user_ref, object_kind, object_id, peer_id, scope, granted_activity_id, allow_original)
			VALUES ($1, $2, 'asset', $3, $4, 'view', $5, FALSE)`,
		"to one recipient": `INSERT INTO federation_shares (id, grantor_user_ref, object_kind, object_id, peer_id, scope, granted_activity_id, allow_original, target_user_url)
			VALUES ($1, $2, 'asset', $3, $4, 'view', $5, TRUE, 'https://receiver.example/users/bob')`,
	} {
		a := f.newAsset("public", true)
		stmt := ins
		// Record does not announce these (it reads the share it is given);
		// build the record by hand to check the delivery-time refusal too.
		g := f.grantAndRecord(a, func(sql *string) { *sql = stmt })
		if g.announcement != uuid.Nil {
			t.Fatalf("%s: Record announced", name)
		}
		g.payload, _ = json.Marshal(map[string]string{"share_id": g.share.String(), "share_activity_id": g.shareActivity.String(),
			"asset_id": a.id.String(), "version_id": a.version.String()})
		g.objectURI = a.uri
		f.shareDelivered(g, "sent")
		if _, err := f.member(g); !errors.Is(err, federation.ErrNotFederated) {
			t.Errorf("a share %s: %v", name, err)
		}
	}
}

// The three base name provenances of #1595, with what archival-1 may
// carry as the original's originalFilename for each: only a name the
// upload request carried. A client_reported backfill (migration 00081)
// and an unknown name are omitted.
var basenameCases = []struct {
	name     string
	basename *string
	source   provenance.Source
	want     string
}{
	{"upload_request", ptr("harbour.png"), provenance.SourceUploadRequest, "harbour.png"},
	{"client_reported", ptr("legacy-claim.png"), provenance.SourceClientReported, ""},
	{"unknown", nil, provenance.SourceUnknown, ""},
}

func ptr(s string) *string { return &s }

// wantOriginalFilename checks that raw is a valid archival-1 member whose
// original names want as its originalFilename (want "" means no member at
// all), and that no other file names one.
func (f *fx) wantOriginalFilename(what string, raw json.RawMessage, want string) {
	f.t.Helper()
	m, err := federation.ParseArchivalAsset(raw)
	if err != nil {
		f.t.Fatalf("%s: a member §17.2 refuses: %v (%s)", what, err, raw)
	}
	originals := 0
	for _, file := range m.Files {
		switch {
		case file.Role == "original":
			originals++
			if file.OriginalFilename != want {
				f.t.Errorf("%s: the original's originalFilename is %q, want %q", what, file.OriginalFilename, want)
			}
		case file.OriginalFilename != "":
			f.t.Errorf("%s: the %s file names %q", what, file.Role, file.OriginalFilename)
		}
	}
	if originals != 1 {
		f.t.Errorf("%s: %d original files", what, originals)
	}
	if want == "" && bytes.Contains(raw, []byte(`"originalFilename"`)) {
		f.t.Errorf("%s: the member carries originalFilename: %s", what, raw)
	}
}

// sealNamedSuccessor creates version n of a over fresh bytes with its
// own base name pair, makes it current, seals it and runs RecordSealed in
// one transaction, as sealNext does for a version with no name. It
// returns the announcements recorded.
func (f *fx) sealNamedSuccessor(a *asset, n int64, basename *string, source provenance.Source) []uuid.UUID {
	f.t.Helper()
	ctx := context.Background()
	hash := f.storedBytes()
	v := uuid.New()
	var ids []uuid.UUID
	if err := pgx.BeginFunc(ctx, f.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT asset_version_create($1, $2, $3, $4, 'replacement', $5, 'png', $6, $7, NULL)`,
			a.id, v, n, a.version, hash, basename, string(source)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE assets SET current_version_id = $2, file_hash = $3 WHERE id = $1`, a.id, v, hash); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT asset_version_seal($1, '[]'::jsonb)`, v); err != nil {
			return err
		}
		var err error
		ids, err = assetsend.RecordSealed(ctx, tx, f.writer, sealedInput(a.id))
		return err
	}); err != nil {
		f.t.Fatalf("version %d: %v", n, err)
	}
	a.version, a.hash = v, hash
	return ids
}

// #1595: the aa:AssetVersion announcement names the original only from
// the sealed version's own upload_request base name. A client_reported
// backfill and an unknown name are omitted; neither the asset's live
// metadata.original_filename nor any storage path is a source (the name
// is exactly the stored base name or absent); and a retry builds the same
// bytes.
func TestMember_TheOriginalFilenameComesOnlyFromTheUploadRequest(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	for _, c := range basenameCases {
		t.Run(c.name, func(t *testing.T) {
			a := f.newAssetNamed("public", true, c.basename, c.source)
			g := f.grantAndRecord(a, nil)
			f.shareDelivered(g, "sent")
			raw, err := f.src.Member(ctx, g.announcement, f.peer, g.objectURI, g.payload)
			if err != nil {
				t.Fatal(err)
			}
			f.wantOriginalFilename("the announcement", raw, c.want)

			// An edit of the live metadata after ingest changes nothing.
			if _, err := f.pool.Exec(ctx, `UPDATE assets SET metadata = jsonb_set(COALESCE(metadata, '{}'::jsonb),
				'{original_filename}', '"edited-later.png"') WHERE id = $1`, a.id); err != nil {
				t.Fatal(err)
			}
			again, err := f.src.Member(ctx, g.announcement, f.peer, g.objectURI, g.payload)
			if err != nil || string(again) != string(raw) {
				t.Fatalf("a retry after a metadata edit: %v\n%s\nwant\n%s", err, again, raw)
			}
			if bytes.Contains(again, []byte("edited-later.png")) {
				t.Fatalf("the member names the live metadata: %s", again)
			}
		})
	}
}

// #1595: each version is announced with its own base name pair, not the
// asset's current one or another version's.
func TestMember_EachVersionKeepsItsOwnOriginalFilename(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	a := f.newAssetNamed("public", true, ptr("first.png"), provenance.SourceUploadRequest)
	g1 := f.grantAndRecord(a, nil)
	f.shareDelivered(g1, "sent")

	ids := f.sealNamedSuccessor(&a, 2, ptr("second.png"), provenance.SourceUploadRequest)
	if len(ids) != 1 {
		t.Fatalf("version 2 announced %d times", len(ids))
	}
	g2 := grant{announcement: ids[0], shareActivity: g1.shareActivity}
	if err := f.pool.QueryRow(ctx, `SELECT object_uri, payload FROM activities WHERE id = $1`, ids[0]).Scan(&g2.objectURI, &g2.payload); err != nil {
		t.Fatal(err)
	}
	v2, err := f.src.Member(ctx, g2.announcement, f.peer, g2.objectURI, g2.payload)
	if err != nil {
		t.Fatal(err)
	}
	if m := f.parsed(v2); m.Version.Number != 2 {
		t.Fatalf("version %d, want 2", m.Version.Number)
	}
	f.wantOriginalFilename("version 2", v2, "second.png")

	// Version 1's announcement, built after version 2 exists, still names
	// version 1's file.
	v1, err := f.src.Member(ctx, g1.announcement, f.peer, g1.objectURI, g1.payload)
	if err != nil {
		t.Fatal(err)
	}
	if m := f.parsed(v1); m.Version.Number != 1 {
		t.Fatalf("version %d, want 1", m.Version.Number)
	}
	f.wantOriginalFilename("version 1", v1, "first.png")

	// A third version with no known name is announced without one, while
	// the earlier versions keep theirs.
	ids = f.sealNamedSuccessor(&a, 3, nil, provenance.SourceUnknown)
	if len(ids) != 1 {
		t.Fatalf("version 3 announced %d times", len(ids))
	}
	g3 := grant{announcement: ids[0]}
	if err := f.pool.QueryRow(ctx, `SELECT object_uri, payload FROM activities WHERE id = $1`, ids[0]).Scan(&g3.objectURI, &g3.payload); err != nil {
		t.Fatal(err)
	}
	v3, err := f.src.Member(ctx, g3.announcement, f.peer, g3.objectURI, g3.payload)
	if err != nil {
		t.Fatal(err)
	}
	f.wantOriginalFilename("version 3", v3, "")
	if again, err := f.src.Member(ctx, g2.announcement, f.peer, g2.objectURI, g2.payload); err != nil || string(again) != string(v2) {
		t.Fatalf("version 2 rebuilt after version 3: %v", err)
	}
}
