// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1574 view against a live database, real stored bytes (fs backend) and
// the production inbound grant record (inboundgrants.Store, written
// through ApplyShare / ApplyUnshare): the two rules, held equal to their Go
// twins, and the three endpoints. Skips without AA_DB_PASSWORD.

package receivedassets_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/inboundgrants"
	"github.com/mscrnt/artist-alley/app/internal/federation/receivedassets"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/storage"
	"github.com/mscrnt/artist-alley/app/internal/storage/fs"
)

const localBase = "https://receiver.example"

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

type fx struct {
	t       *testing.T
	pool    *pgxpool.Pool
	svc     *storage.Service
	grants  *inboundgrants.Store
	h       *receivedassets.Handler
	admin   int64
	u1, u2  int64
	u1Name  string
	peerA   peer
	peerX   peer
	cleanup []func()
}

type peer struct {
	id  uuid.UUID
	url string
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
	backend, err := fs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &fx{t: t, pool: pool, svc: storage.NewService(backend, pool)}
	f.grants = inboundgrants.New(pool, func(context.Context) string { return localBase }, nil, nil)
	f.h = &receivedassets.Handler{Pool: pool, Storage: f.svc}
	f.admin = f.user("ra-admin")
	f.u1Name = "ra-u1-" + uuid.NewString()[:6]
	f.u1 = f.userNamed(f.u1Name)
	f.u2 = f.user("ra-u2")
	f.peerA = f.newPeer()
	f.peerX = f.newPeer()
	t.Cleanup(func() {
		c := context.Background()
		for _, p := range []peer{f.peerA, f.peerX} {
			_, _ = pool.Exec(c, `DELETE FROM federation_peers WHERE id = $1`, p.id) // records and grants cascade
		}
		for i := len(f.cleanup) - 1; i >= 0; i-- {
			f.cleanup[i]()
		}
		_, _ = pool.Exec(c, `DELETE FROM "user" WHERE ref = ANY($1)`, []int64{f.admin, f.u1, f.u2})
	})
	return f
}

func (f *fx) user(prefix string) int64 { return f.userNamed(prefix + "-" + uuid.NewString()[:6]) }

func (f *fx) userNamed(name string) int64 {
	f.t.Helper()
	var ref int64
	if err := f.pool.QueryRow(context.Background(), `INSERT INTO "user" (username, fullname, approved) VALUES ($1, 'RA', 1) RETURNING ref`,
		name).Scan(&ref); err != nil {
		f.t.Fatal(err)
	}
	return ref
}

func (f *fx) newPeer() peer {
	f.t.Helper()
	p := peer{url: "https://origin-" + uuid.NewString()[:8] + ".example"}
	if err := f.pool.QueryRow(context.Background(), `INSERT INTO federation_peers (instance_url, display_name, instance_public_key, enabled, status,
		    handshake_by_user_ref, capabilities)
		VALUES ($1, 'origin', 'k', TRUE, 'connected', $2, '["share-lifecycle-1","archival-1","original-transfer-1"]'::jsonb) RETURNING id`,
		p.url, f.admin).Scan(&p.id); err != nil {
		f.t.Fatal(err)
	}
	return p
}

type record struct {
	id     uuid.UUID
	object string
	hash   string
	body   []byte
	peer   peer
}

// complete writes a complete received record from p for fresh bytes, in
// one transaction, exactly as the receiver core leaves one: verified,
// identified, pinned under the record.
func (f *fx) complete(p peer, sensitivity string) record {
	f.t.Helper()
	ctx := context.Background()
	body := make([]byte, 4096)
	_, _ = rand.Read(body)
	copy(body, []byte("\x89PNG\r\n\x1a\n")) // detected as image/png
	sum := sha256.Sum256(body)
	r := record{id: uuid.New(), hash: hex.EncodeToString(sum[:]), body: body, peer: p}
	r.object = p.url + "/assets/" + uuid.NewString()
	temp := storage.PinRef{SubjectType: "ra-test", SubjectID: r.id.String()}
	if _, err := f.svc.UploadOriginal(ctx, bytes.NewReader(body), "image/png", temp); err != nil {
		f.t.Fatal(err)
	}
	f.cleanup = append(f.cleanup, func() {
		c := context.Background()
		_, _ = f.pool.Exec(c, `DELETE FROM storage_pins WHERE object_hash = $1`, r.hash)
		_, _ = f.pool.Exec(c, `DELETE FROM storage_variants WHERE object_hash = $1`, r.hash)
		_, _ = f.pool.Exec(c, `DELETE FROM storage_objects WHERE hash = $1`, r.hash)
	})
	f.insert(r, sensitivity, "complete")
	if err := f.svc.RemovePin(ctx, temp, r.hash); err != nil {
		f.t.Fatal(err)
	}
	return r
}

// insert writes the record in state (pending, rejected or complete).
func (f *fx) insert(r record, sensitivity, state string) {
	f.t.Helper()
	ctx := context.Background()
	member := fmt.Sprintf(`{"id":%q,"rights":"http://rightsstatements.org/vocab/InC/1.0/","description":{"title":"Harbour at dusk"},"mature":false}`, r.object)
	if err := pgx.BeginFunc(ctx, f.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO federation_received_assets
			(id, peer_id, activity_uri, actor_uri, origin_asset_uri, origin_asset_id, version_uri, version_number,
			 file_set_sha256, record, sensitivity, signer_key_id)
			VALUES ($1, $2, $3, $4, $5, gen_random_uuid(), $6, 1, $7, $8::jsonb, $9, 'k')`,
			r.id, r.peer.id, r.peer.url+"/activities/"+uuid.NewString(), r.peer.url+"/users/alice", r.object,
			r.object+"/versions/1", strings.Repeat("0", 64), member, sensitivity); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO federation_received_asset_files
			(received_asset_id, position, role, object_hash, size_bytes, checksums, declared_media_type, declared_original_filename)
			VALUES ($1, 0, 'original', $2, $3, '[]'::jsonb, 'image/jpeg', 'harbour.jpg')`, r.id, r.hash, len(r.body)); err != nil {
			return err
		}
		switch state {
		case "complete":
			for _, q := range []string{
				`INSERT INTO storage_pins (object_hash, pin_subject_type, pin_subject_id) VALUES ($2, 'federation_received_asset', $1::text)`,
				`UPDATE storage_objects SET gc_eligible_at = NULL WHERE hash = $2 AND $1::text <> ''`,
				`UPDATE federation_received_asset_files SET state = 'verified', verified_at = now(),
				    detected_media_type = 'image/png', detected_by = 'net/http.DetectContentType'
				  WHERE received_asset_id = $1 AND $2::text <> ''`,
				`UPDATE federation_received_assets SET state = 'complete', completed_at = now(), authorizing_grant_id = gen_random_uuid()
				  WHERE id = $1 AND $2::text <> ''`,
			} {
				if _, err := tx.Exec(ctx, q, r.id, r.hash); err != nil {
					return err
				}
			}
		case "rejected":
			if _, err := tx.Exec(ctx, `UPDATE federation_received_assets SET state = 'rejected', reason = 'fixity_mismatch' WHERE id = $1`, r.id); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		f.t.Fatalf("insert %s record: %v", state, err)
	}
}

// withdraw applies an origin's tombstone to r as the receiver does
// (assetingest.Service.Withdraw): the tombstone kept, the record's pins
// released, the record and its originals withdrawn.
func (f *fx) withdraw(r record, reason *string) {
	f.t.Helper()
	ctx := context.Background()
	tomb := fmt.Sprintf(`{"id":%q,"formerType":"aa:Asset","deleted":"2026-11-21T09:00:00Z","lastVersion":%q,"files":[{"role":"original","contentUri":"cas://%s","checksums":[{"algorithm":"sha256","value":"%s"}]}]}`,
		r.object, r.object+"/versions/1", r.hash, r.hash)
	if err := pgx.BeginFunc(ctx, f.pool, func(tx pgx.Tx) error {
		for _, q := range []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO federation_received_tombstones (peer_id, origin_asset_uri, activity_uri, actor_uri, tombstone, deleted, reason, last_version)
			  VALUES ($1, $2, $3, $4, $5::jsonb, '2026-11-21T09:00:00Z', $6, $7)`,
				[]any{r.peer.id, r.object, r.peer.url + "/activities/" + uuid.NewString(), r.peer.url + "/users/alice", tomb, reason, r.object + "/versions/1"}},
			{`SELECT federation_received_asset_release_pins(ARRAY[$1::text])`, []any{r.id.String()}},
			{`UPDATE federation_received_assets SET state = 'withdrawn', reason = 'withdrawn', withdrawn_at = now() WHERE id = $1`, []any{r.id}},
			{`UPDATE federation_received_asset_files SET state = 'withdrawn' WHERE received_asset_id = $1 AND role = 'original'`, []any{r.id}},
		} {
			if _, err := tx.Exec(ctx, q.sql, q.args...); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		f.t.Fatalf("withdraw: %v", err)
	}
}

// grant applies an aa:Share from r's peer as the inbox would.
func (f *fx) grant(r record, original bool, target string, supersedes string) string {
	f.t.Helper()
	act := r.peer.url + "/activities/share-" + uuid.NewString()
	if _, err := f.grants.ApplyShare(context.Background(), inboundgrants.Share{
		PeerID: r.peer.id, ActivityURI: act, GrantorActorURI: r.peer.url + "/users/alice",
		ObjectKind: federation.ShareObjectKindAsset, ObjectURI: r.object, Scope: federation.ShareScopeView,
		AllowOriginal: original, TargetUserURI: target, Supersedes: supersedes,
	}); err != nil {
		f.t.Fatal(err)
	}
	return act
}

// grantFrom applies an aa:Share from another peer for r's object URI
// string (no peer could send one: the object is not on its origin).
func (f *fx) grantFrom(p peer, r record) {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO federation_inbound_grants
		(peer_id, grant_activity_uri, grantor_actor_uri, object_kind, object_uri, scope, allow_original, state)
		VALUES ($1, $2, $3, 'asset', $4, 'view', TRUE, 'live')`, p.id, p.url+"/activities/"+uuid.NewString(), p.url+"/users/mallory", r.object); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fx) revoke(r record, share string) {
	f.t.Helper()
	if _, err := f.grants.ApplyUnshare(context.Background(), inboundgrants.Unshare{
		PeerID: r.peer.id, ActivityURI: r.peer.url + "/activities/unshare-" + uuid.NewString(),
		ActorURI: r.peer.url + "/users/alice", ObjectKind: federation.ShareObjectKindAsset, ObjectURI: r.object, Revokes: share,
	}); err != nil {
		f.t.Fatal(err)
	}
}

func as(ref int64) context.Context {
	if ref == 0 {
		return context.Background()
	}
	return auth.WithIdentity(context.Background(), &auth.Identity{UserRef: ref, AuthMethod: "session"})
}

// view is what the three endpoints answer for (caller, record): listed,
// detail 200, original_available, original bytes 200 (and they match).
type view struct{ listed, detail, available, original bool }

func (f *fx) view(caller int64, r record) view {
	f.t.Helper()
	var v view
	list, err := f.h.ListReceivedAssets(as(caller), openapi.ListReceivedAssetsRequestObject{})
	if err != nil {
		f.t.Fatal(err)
	}
	if l, ok := list.(openapi.ListReceivedAssets200JSONResponse); ok {
		noStore(f.t, "list", l.Headers.CacheControl)
		n := 0
		for _, it := range l.Body.Items {
			if uuid.UUID(it.Id) == r.id {
				n++
			}
		}
		if n > 1 {
			f.t.Fatalf("record listed %d times", n)
		}
		v.listed = n == 1
	} else if caller != 0 {
		f.t.Fatalf("list for %d: %T", caller, list)
	}
	get, err := f.h.GetReceivedAsset(as(caller), openapi.GetReceivedAssetRequestObject{Id: r.id})
	if err != nil {
		f.t.Fatal(err)
	}
	if d, ok := get.(openapi.GetReceivedAsset200JSONResponse); ok {
		noStore(f.t, "detail", d.Headers.CacheControl)
		v.detail, v.available = true, d.Body.OriginalAvailable
	}
	orig, err := f.h.DownloadReceivedAssetOriginal(as(caller), openapi.DownloadReceivedAssetOriginalRequestObject{Id: r.id})
	if err != nil {
		f.t.Fatal(err)
	}
	if o, ok := orig.(openapi.DownloadReceivedAssetOriginal200ApplicationoctetStreamResponse); ok {
		noStore(f.t, "original", o.Headers.CacheControl)
		b, err := io.ReadAll(o.Body)
		if c, ok := o.Body.(io.Closer); ok {
			_ = c.Close()
		}
		if err != nil {
			f.t.Fatal(err)
		}
		sum := sha256.Sum256(b)
		if hex.EncodeToString(sum[:]) != r.hash || o.ContentLength != int64(len(r.body)) {
			f.t.Fatal("the original's bytes do not match the verified sha256")
		}
		v.original = true
	}
	return v
}

// noStore fails unless a positive answer forbids reuse.
func noStore(t *testing.T, what string, cc *string) {
	t.Helper()
	if cc == nil || *cc != "no-store" {
		t.Fatalf("%s: Cache-Control %v, want no-store", what, cc)
	}
}

func (f *fx) want(name string, caller int64, r record, want view) {
	f.t.Helper()
	if got := f.view(caller, r); got != want {
		f.t.Errorf("%s (caller %d): got %+v, want %+v", name, caller, got, want)
	}
}

var (
	everything = view{listed: true, detail: true, available: true, original: true}
	viewOnly   = view{listed: true, detail: true}
	nothing    = view{}
)

// The SQL rules agree with their Go twins over every state the matrix
// can produce, for an anonymous caller, a recipient and another user.
func TestRulesSQL_MatchGo(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	past := time.Now().Add(-time.Hour)
	// unbind leaves a targeted grant live with no recipient. ApplyShare
	// never does that (an unbound target ends as recipient_unbound), but no
	// constraint forbids it, and it must not read as peer-wide.
	unbind := func(r record) {
		if _, err := f.pool.Exec(ctx, `UPDATE federation_inbound_grants SET recipient_user_ref = NULL WHERE object_uri = $1`, r.object); err != nil {
			t.Fatal(err)
		}
	}
	cases := map[string]func() record{
		"peer-wide, original":  func() record { r := f.complete(f.peerA, "public"); f.grant(r, true, "", ""); return r },
		"peer-wide, view only": func() record { r := f.complete(f.peerA, "public"); f.grant(r, false, "", ""); return r },
		"to u1, original": func() record {
			r := f.complete(f.peerA, "public")
			f.grant(r, true, localBase+"/users/"+f.u1Name, "")
			return r
		},
		"revoked": func() record {
			r := f.complete(f.peerA, "public")
			f.revoke(r, f.grant(r, true, "", ""))
			return r
		},
		"expired": func() record {
			r := f.complete(f.peerA, "public")
			f.grant(r, true, "", "")
			if _, err := f.pool.Exec(ctx, `UPDATE federation_inbound_grants SET expires_at = $2 WHERE object_uri = $1`, r.object, past); err != nil {
				t.Fatal(err)
			}
			return r
		},
		"live but unbound target": func() record {
			r := f.complete(f.peerA, "public")
			f.grant(r, true, localBase+"/users/"+f.u1Name, "")
			unbind(r)
			return r
		},
		"team tier":      func() record { r := f.complete(f.peerA, "team"); f.grant(r, true, "", ""); return r },
		"another peer's": func() record { r := f.complete(f.peerA, "public"); f.grantFrom(f.peerX, r); return r },
		"no grant":       func() record { return f.complete(f.peerA, "public") },
		"pending": func() record {
			r := record{id: uuid.New(), peer: f.peerA, hash: strings.Repeat("cd", 32), body: []byte("x")}
			r.object = f.peerA.url + "/assets/" + uuid.NewString()
			f.insert(r, "public", "pending")
			f.grant(r, true, "", "")
			return r
		},
		"rejected": func() record {
			r := record{id: uuid.New(), peer: f.peerA, hash: strings.Repeat("ce", 32), body: []byte("x")}
			r.object = f.peerA.url + "/assets/" + uuid.NewString()
			f.insert(r, "public", "rejected")
			f.grant(r, true, "", "")
			return r
		},
		// #1575: a withdrawn record, under each grant shape.
		"withdrawn, peer-wide": func() record {
			r := f.complete(f.peerA, "public")
			f.grant(r, true, "", "")
			f.withdraw(r, nil)
			return r
		},
		"withdrawn, to u1": func() record {
			r := f.complete(f.peerA, "public")
			f.grant(r, false, localBase+"/users/"+f.u1Name, "")
			f.withdraw(r, nil)
			return r
		},
		"withdrawn, revoked": func() record {
			r := f.complete(f.peerA, "public")
			f.revoke(r, f.grant(r, true, "", ""))
			f.withdraw(r, nil)
			return r
		},
		"withdrawn, team tier": func() record {
			r := f.complete(f.peerA, "team")
			f.grant(r, true, "", "")
			f.withdraw(r, nil)
			return r
		},
		"withdrawn, another peer's grant": func() record {
			r := f.complete(f.peerA, "public")
			f.grantFrom(f.peerX, r)
			f.withdraw(r, nil)
			return r
		},
	}
	for name, mk := range cases {
		r := mk()
		facts, err := receivedassets.LoadFacts(ctx, f.pool, r.id)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, caller := range []int64{0, f.u1, f.u2} {
			var view, orig, notice bool
			var now time.Time
			if err := f.pool.QueryRow(ctx, `SELECT `+receivedassets.ViewableSQL("r", "$2")+`, `+receivedassets.OriginalReadableSQL("r", "$2")+`,
				`+receivedassets.WithdrawnVisibleSQL("r", "$2")+`, now()
				FROM federation_received_assets r WHERE r.id = $1`, r.id, caller).Scan(&view, &orig, &notice, &now); err != nil {
				t.Fatal(err)
			}
			if gv, gor := receivedassets.Viewable(facts, caller, now), receivedassets.OriginalReadable(facts, caller, now); gv != view || gor != orig {
				t.Errorf("%s, caller %d: SQL viewable=%v original=%v, Go viewable=%v original=%v", name, caller, view, orig, gv, gor)
			}
			if gn := receivedassets.WithdrawnVisible(facts, caller, now); gn != notice {
				t.Errorf("%s, caller %d: SQL withdrawal notice=%v, Go=%v", name, caller, notice, gn)
			}
			// The notice is only ever for a withdrawn record, and a
			// withdrawn record is never viewable.
			if notice && facts.State != "withdrawn" || facts.State == "withdrawn" && (view || orig) {
				t.Errorf("%s, caller %d: state %s viewable=%v original=%v notice=%v", name, caller, facts.State, view, orig, notice)
			}
		}
	}
	// Not only equal: an unbound target shows to nobody, in either twin.
	ub := f.complete(f.peerA, "public")
	f.grant(ub, true, localBase+"/users/"+f.u1Name, "")
	unbind(ub)
	for _, caller := range []int64{f.u1, f.u2} {
		if got := f.view(caller, ub); got != (view{}) {
			t.Errorf("unbound target, caller %d: %+v", caller, got)
		}
	}

	// The disabled and disconnected peer states.
	r := f.complete(f.peerA, "public")
	f.grant(r, true, "", "")
	for _, q := range []string{`UPDATE federation_peers SET enabled = FALSE WHERE id = $1`,
		`UPDATE federation_peers SET enabled = TRUE, status = 'pending_inbound' WHERE id = $1`} {
		if _, err := f.pool.Exec(ctx, q, f.peerA.id); err != nil {
			t.Fatal(err)
		}
		facts, _ := receivedassets.LoadFacts(ctx, f.pool, r.id)
		var view bool
		var now time.Time
		_ = f.pool.QueryRow(ctx, `SELECT `+receivedassets.ViewableSQL("r", "$2")+`, now() FROM federation_received_assets r WHERE r.id = $1`, r.id, f.u1).Scan(&view, &now)
		if view || receivedassets.Viewable(facts, f.u1, now) {
			t.Errorf("%s: viewable SQL=%v Go=%v", q, view, receivedassets.Viewable(facts, f.u1, now))
		}
	}
	if _, err := f.pool.Exec(ctx, `UPDATE federation_peers SET enabled = TRUE, status = 'connected' WHERE id = $1`, f.peerA.id); err != nil {
		t.Fatal(err)
	}
}

func TestAPI_PeerWideTargetedAnonymousAndUnrelatedPeer(t *testing.T) {
	f := newFx(t)

	wide := f.complete(f.peerA, "public")
	f.grant(wide, true, "", "")
	f.want("peer-wide, u1", f.u1, wide, everything)
	f.want("peer-wide, u2", f.u2, wide, everything)
	f.want("peer-wide, anonymous", 0, wide, nothing)
	list, _ := f.h.ListReceivedAssets(as(0), openapi.ListReceivedAssetsRequestObject{})
	if _, ok := list.(openapi.ListReceivedAssets401JSONResponse); !ok {
		t.Fatalf("anonymous list: %T", list)
	}

	targeted := f.complete(f.peerA, "public")
	f.grant(targeted, true, localBase+"/users/"+f.u1Name, "")
	f.want("targeted, u1", f.u1, targeted, everything)
	f.want("targeted, u2", f.u2, targeted, nothing)
	f.want("targeted, anonymous", 0, targeted, nothing)

	// A live grant for the same object URI string from another peer
	// authorises nothing on peer A's record.
	unrelated := f.complete(f.peerA, "public")
	f.grantFrom(f.peerX, unrelated)
	f.want("another peer's grant", f.u1, unrelated, nothing)

	// The detail: declared claims as received, the detected type beside
	// the declared one, the source and provenance.
	got, err := f.h.GetReceivedAsset(as(f.u1), openapi.GetReceivedAssetRequestObject{Id: wide.id})
	if err != nil {
		t.Fatal(err)
	}
	d := got.(openapi.GetReceivedAsset200JSONResponse).Body
	o := d.Asset.Original
	if d.Asset.OriginAssetUri != wide.object || d.Asset.PeerUrl != f.peerA.url || d.Asset.Sensitivity != "public" ||
		o.Sha256 != wide.hash || o.DeclaredMediaType != "image/jpeg" || o.DetectedMediaType == nil || *o.DetectedMediaType != "image/png" ||
		o.DeclaredOriginalFilename == nil || *o.DeclaredOriginalFilename != "harbour.jpg" || len(d.Files) != 1 {
		t.Fatalf("detail %+v", d)
	}
	if d.Declared["rights"] != "http://rightsstatements.org/vocab/InC/1.0/" || d.Declared["description"] == nil {
		t.Fatalf("declared claims %+v", d.Declared)
	}
	if _, leaked := d.Declared["files"]; leaked {
		t.Fatal("the file set is reported as a declared claim")
	}
}

// The original needs the original-file permission on the live grant
// itself, read on every request: a downgrade removes it at once, a
// regrant restores it, a revoke, an expiry and a disabled peer remove
// everything.
func TestAPI_DowngradeRegrantRevokeExpiryPeerDisable(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	r := f.complete(f.peerA, "public")
	g1 := f.grant(r, true, "", "")
	f.want("granted", f.u1, r, everything)
	g2 := f.grant(r, false, "", g1) // supersedes g1 without the original
	f.want("downgraded", f.u1, r, viewOnly)
	g3 := f.grant(r, true, "", g2)
	f.want("regranted", f.u1, r, everything)

	if _, err := f.pool.Exec(ctx, `UPDATE federation_peers SET enabled = FALSE WHERE id = $1`, f.peerA.id); err != nil {
		t.Fatal(err)
	}
	f.want("peer disabled", f.u1, r, nothing)
	if _, err := f.pool.Exec(ctx, `UPDATE federation_peers SET enabled = TRUE WHERE id = $1`, f.peerA.id); err != nil {
		t.Fatal(err)
	}
	f.want("peer enabled again", f.u1, r, everything)

	if _, err := f.pool.Exec(ctx, `UPDATE federation_inbound_grants SET expires_at = now() - interval '1 minute'
		WHERE peer_id = $1 AND object_uri = $2 AND state = 'live'`, f.peerA.id, r.object); err != nil {
		t.Fatal(err)
	}
	f.want("expired", f.u1, r, nothing)
	if _, err := f.pool.Exec(ctx, `UPDATE federation_inbound_grants SET expires_at = NULL
		WHERE peer_id = $1 AND object_uri = $2 AND state = 'live'`, f.peerA.id, r.object); err != nil {
		t.Fatal(err)
	}
	f.want("unexpired again", f.u1, r, everything)

	f.revoke(r, g3)
	f.want("revoked", f.u1, r, nothing)
}

// A team record is held, never shown: not to a peer-wide audience, not
// to a named recipient, not anonymously, whatever the grant carries.
func TestAPI_TeamTierFailsClosed(t *testing.T) {
	f := newFx(t)
	wide := f.complete(f.peerA, "team")
	f.grant(wide, true, "", "")
	f.want("team, peer-wide, u1", f.u1, wide, nothing)
	f.want("team, peer-wide, anonymous", 0, wide, nothing)
	named := f.complete(f.peerA, "team")
	f.grant(named, true, localBase+"/users/"+f.u1Name, "")
	f.want("team, named recipient", f.u1, named, nothing)
}

// Only a complete record is shown; listing is stable and paginated.
func TestAPI_PendingRejectedAndPagination(t *testing.T) {
	f := newFx(t)
	for _, state := range []string{"pending", "rejected"} {
		r := record{id: uuid.New(), peer: f.peerA, hash: strings.Repeat("ab", 32), body: []byte("x")}
		r.object = f.peerA.url + "/assets/" + uuid.NewString()
		f.insert(r, "public", state)
		f.grant(r, true, "", "")
		f.want(state, f.u1, r, nothing)
	}
	var ids []uuid.UUID
	for i := 0; i < 3; i++ {
		r := f.complete(f.peerA, "public")
		f.grant(r, true, "", "")
		// A second live grant (to u1 as well) must not list it twice.
		f.grant(r, false, localBase+"/users/"+f.u1Name, "")
		ids = append(ids, r.id)
	}
	limit := 2
	seen := map[uuid.UUID]int{}
	var cursor *string
	for page := 0; page < 10; page++ {
		res, err := f.h.ListReceivedAssets(as(f.u1), openapi.ListReceivedAssetsRequestObject{
			Params: openapi.ListReceivedAssetsParams{Limit: &limit, Cursor: cursor}})
		if err != nil {
			t.Fatal(err)
		}
		l := res.(openapi.ListReceivedAssets200JSONResponse).Body
		for _, it := range l.Items {
			seen[uuid.UUID(it.Id)]++
		}
		if l.NextCursor == nil {
			break
		}
		cursor = l.NextCursor
	}
	for _, id := range ids {
		if seen[id] != 1 {
			t.Fatalf("record %s listed %d times across pages", id, seen[id])
		}
	}
	bad := "not-a-cursor"
	if res, _ := f.h.ListReceivedAssets(as(f.u1), openapi.ListReceivedAssetsRequestObject{
		Params: openapi.ListReceivedAssetsParams{Cursor: &bad}}); fmt.Sprintf("%T", res) != "openapi.ListReceivedAssets400JSONResponse" {
		t.Fatalf("a malformed cursor: %T", res)
	}
}

// TestNoStore_EveryAnswerOfTheRoutes: the middleware marks every answer
// under the received-asset routes, whatever the handler then answers
// (a refusal included), and nothing else.
func TestNoStore_EveryAnswerOfTheRoutes(t *testing.T) {
	for path, want := range map[string]string{
		"/api/v1/federation/received-assets":                     "no-store",
		"/api/v1/federation/received-assets/x":                   "no-store",
		"/api/v1/federation/received-assets/x/original":          "no-store",
		"/api/v1/federation/received-assetsx":                    "",
		"/api/v1/federation/peers/visible":                       "",
		"/api/v1/assets/" + uuid.NewString() + "/files/original": "",
	} {
		for _, status := range []int{http.StatusOK, http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound} {
			h := receivedassets.NoStore(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			if got := rec.Header().Get("Cache-Control"); got != want || rec.Code != status {
				t.Errorf("%s answering %d: Cache-Control %q, want %q", path, status, got, want)
			}
		}
	}
}

// TestAdmin_RequiresSystemAdminAndListsEveryRecord (#1574 slice 2): both
// operator endpoints answer 401 to an anonymous caller and 403 to a
// signed-in caller without system.admin; for an operator the record list
// holds every state and tier, team included; unknown filters are 400.
func TestAdmin_RequiresSystemAdminAndListsEveryRecord(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	pub := f.complete(f.peerA, "public")
	team := f.complete(f.peerA, "team")
	pend := record{id: uuid.New(), peer: f.peerA, hash: strings.Repeat("ce", 32), body: []byte("x")}
	pend.object = f.peerA.url + "/assets/" + uuid.NewString()
	f.insert(pend, "public", "pending")
	rej := record{id: uuid.New(), peer: f.peerA, hash: strings.Repeat("cf", 32), body: []byte("x")}
	rej.object = f.peerA.url + "/assets/" + uuid.NewString()
	f.insert(rej, "public", "rejected")

	admin := auth.WithIdentity(ctx, &auth.Identity{UserRef: f.admin, Capabilities: []string{"system.admin"}})
	member := as(f.u1)
	pid := openapi_types.UUID(f.peerA.id)

	for name, c := range map[string]context.Context{"anonymous": ctx, "member": member} {
		r1, err := f.h.ListAdminReceivedAssets(c, openapi.ListAdminReceivedAssetsRequestObject{})
		if err != nil {
			t.Fatal(err)
		}
		r2, err := f.h.ListFederationTransferEvents(c, openapi.ListFederationTransferEventsRequestObject{})
		if err != nil {
			t.Fatal(err)
		}
		switch name {
		case "anonymous":
			_, ok1 := r1.(openapi.ListAdminReceivedAssets401JSONResponse)
			_, ok2 := r2.(openapi.ListFederationTransferEvents401JSONResponse)
			if !ok1 || !ok2 {
				t.Errorf("anonymous: %T %T", r1, r2)
			}
		default:
			_, ok1 := r1.(openapi.ListAdminReceivedAssets403JSONResponse)
			_, ok2 := r2.(openapi.ListFederationTransferEvents403JSONResponse)
			if !ok1 || !ok2 {
				t.Errorf("member: %T %T", r1, r2)
			}
		}
	}

	res, err := f.h.ListAdminReceivedAssets(admin, openapi.ListAdminReceivedAssetsRequestObject{
		Params: openapi.ListAdminReceivedAssetsParams{PeerId: &pid}})
	if err != nil {
		t.Fatal(err)
	}
	list, ok := res.(openapi.ListAdminReceivedAssets200JSONResponse)
	if !ok {
		t.Fatalf("admin: %T", res)
	}
	got := map[uuid.UUID]string{}
	for _, it := range list.Items {
		got[uuid.UUID(it.Id)] = it.State + "/" + it.Sensitivity
	}
	for id, want := range map[uuid.UUID]string{pub.id: "complete/public", team.id: "complete/team",
		pend.id: "pending/public", rej.id: "rejected/public"} {
		if got[id] != want {
			t.Errorf("record %s: %q, want %q", id, got[id], want)
		}
	}
	bad := "nonsense"
	if r, _ := f.h.ListAdminReceivedAssets(admin, openapi.ListAdminReceivedAssetsRequestObject{
		Params: openapi.ListAdminReceivedAssetsParams{State: &bad}}); func() bool {
		_, ok := r.(openapi.ListAdminReceivedAssets400JSONResponse)
		return !ok
	}() {
		t.Errorf("unknown state: %T", r)
	}
	if r, _ := f.h.ListFederationTransferEvents(admin, openapi.ListFederationTransferEventsRequestObject{
		Params: openapi.ListFederationTransferEventsParams{Outcome: &bad}}); func() bool {
		_, ok := r.(openapi.ListFederationTransferEvents400JSONResponse)
		return !ok
	}() {
		t.Errorf("unknown outcome: %T", r)
	}
}

// #1575: a withdrawn record answers 410 with the origin's tombstone
// (detail and original) to a caller who could view it, and 404 to anyone
// else, the same as a record that does not exist.
func TestAPI_WithdrawnAnswersTheNoticeOnlyToWhoCouldView(t *testing.T) {
	f := newFx(t)
	reason := "withdrawn by the artist"
	r := f.complete(f.peerA, "public")
	f.grant(r, true, "", "")
	f.want("before withdrawal", f.u1, r, everything)
	f.withdraw(r, &reason)

	for _, caller := range []int64{f.u1, f.u2} {
		if got := f.view(caller, r); got != nothing {
			t.Fatalf("caller %d after withdrawal: %+v, want nothing served", caller, got)
		}
		get, err := f.h.GetReceivedAsset(as(caller), openapi.GetReceivedAssetRequestObject{Id: r.id})
		if err != nil {
			t.Fatal(err)
		}
		n, ok := get.(openapi.GetReceivedAsset410JSONResponse)
		if !ok {
			t.Fatalf("detail for %d: %T, want 410", caller, get)
		}
		if uuid.UUID(n.Id) != r.id || n.OriginAssetUri != r.object || n.LastVersion != r.object+"/versions/1" ||
			n.Deleted != "2026-11-21T09:00:00Z" || n.Reason == nil || *n.Reason != reason || n.WithdrawnAt.IsZero() {
			t.Fatalf("notice %+v", n)
		}
		orig, err := f.h.DownloadReceivedAssetOriginal(as(caller), openapi.DownloadReceivedAssetOriginalRequestObject{Id: r.id})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := orig.(openapi.DownloadReceivedAssetOriginal410JSONResponse); !ok {
			t.Fatalf("original for %d: %T, want 410", caller, orig)
		}
	}

	// Who could not view it gets what a missing record gets.
	notFound := func(name string, caller int64, id uuid.UUID) {
		t.Helper()
		get, err := f.h.GetReceivedAsset(as(caller), openapi.GetReceivedAssetRequestObject{Id: id})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := get.(openapi.GetReceivedAsset404JSONResponse); !ok {
			t.Errorf("%s detail: %T, want 404", name, get)
		}
		orig, err := f.h.DownloadReceivedAssetOriginal(as(caller), openapi.DownloadReceivedAssetOriginalRequestObject{Id: id})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := orig.(openapi.DownloadReceivedAssetOriginal404JSONResponse); !ok {
			t.Errorf("%s original: %T, want 404", name, orig)
		}
	}
	notFound("a record that does not exist", f.u1, uuid.New())
	targeted := f.complete(f.peerA, "public")
	f.grant(targeted, true, localBase+"/users/"+f.u1Name, "")
	f.withdraw(targeted, nil)
	notFound("another user's targeted grant", f.u2, targeted.id)
	if _, ok := mustGet(t, f, f.u1, targeted.id).(openapi.GetReceivedAsset410JSONResponse); !ok {
		t.Error("the targeted recipient does not get the notice")
	}
	team := f.complete(f.peerA, "team")
	f.grant(team, true, "", "")
	f.withdraw(team, nil)
	notFound("a team-tier record", f.u1, team.id)
	ungranted := f.complete(f.peerA, "public")
	f.withdraw(ungranted, nil)
	notFound("a record with no grant", f.u1, ungranted.id)
	// Anonymous callers.
	get, err := f.h.GetReceivedAsset(as(0), openapi.GetReceivedAssetRequestObject{Id: r.id})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := get.(openapi.GetReceivedAsset410JSONResponse); ok {
		t.Error("an anonymous caller got the notice")
	}
}

func mustGet(t *testing.T, f *fx, caller int64, id uuid.UUID) openapi.GetReceivedAssetResponseObject {
	t.Helper()
	get, err := f.h.GetReceivedAsset(as(caller), openapi.GetReceivedAssetRequestObject{Id: id})
	if err != nil {
		t.Fatal(err)
	}
	return get
}

// #1575 (spec v1 §17.2.8): the declared mature and aiProvenance are those
// of the asset's newest revision by (published, activity URI), whatever
// order revisions arrived in; with none, the record's own claims. They are
// display only: who sees the record is unchanged.
func TestAPI_DeclaredFlagsFollowTheNewestRevision(t *testing.T) {
	f := newFx(t)
	r := f.complete(f.peerA, "public")
	f.grant(r, true, "", "")
	declared := func() map[string]interface{} {
		t.Helper()
		got, err := f.h.GetReceivedAsset(as(f.u1), openapi.GetReceivedAssetRequestObject{Id: r.id})
		if err != nil {
			t.Fatal(err)
		}
		ok, is := got.(openapi.GetReceivedAsset200JSONResponse)
		if !is {
			t.Fatalf("detail: %T", got)
		}
		return ok.Body.Declared
	}
	if d := declared(); d["mature"] != false || d["aiProvenance"] != nil {
		t.Fatalf("without a revision: %v", d)
	}
	t0 := time.Now().UTC().Truncate(time.Microsecond)
	revision := func(at time.Duration, uri string, mature bool, ai *string) {
		t.Helper()
		if _, err := f.pool.Exec(context.Background(), `INSERT INTO federation_received_asset_revisions
			(peer_id, origin_asset_uri, activity_uri, activity_type, actor_uri, published, version_uri, file_set_sha256, mature, ai_provenance, record)
			VALUES ($1, $2, $3, 'Update', $4, $5, $6, $7, $8, $9, '{}'::jsonb)`,
			r.peer.id, r.object, r.peer.url+"/activities/"+uri, r.peer.url+"/users/alice", t0.Add(at),
			r.object+"/versions/1", strings.Repeat("0", 64), mature, ai); err != nil {
			t.Fatal(err)
		}
	}
	gen := "generated"
	revision(2*time.Second, "b", true, &gen)
	revision(time.Second, "a", false, nil) // older, arriving after
	if d := declared(); d["mature"] != true || d["aiProvenance"] != "generated" {
		t.Fatalf("after an older revision arrived last: %v", d)
	}
	revision(2*time.Second, "c", false, nil) // the same time, a greater URI
	if d := declared(); d["mature"] != false || d["aiProvenance"] != nil {
		t.Fatalf("the greater URI on equal times: %v", d)
	}
	// The other claims are the record's own.
	if d := declared(); d["rights"] != "http://rightsstatements.org/vocab/InC/1.0/" {
		t.Fatalf("other claims: %v", d)
	}
	// Revisions decide nothing about who sees a record.
	team := f.complete(f.peerA, "team")
	f.grant(team, true, "", "")
	f.want("a team record with no revision", f.u1, team, nothing)
	f.want("the public record", f.u1, r, everything)
}
