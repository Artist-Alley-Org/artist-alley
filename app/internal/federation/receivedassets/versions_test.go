// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package receivedassets_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mscrnt/artist-alley/app/internal/federation/receivedassets"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/storage"
)

// supersede replaces r, a complete version 1, with a complete version 2
// of the same asset, as the receiver's completion does (#1575): in one
// transaction r becomes superseded (keeping its pin and verified
// original) and the new record complete, linked to it.
func (f *fx) supersede(r record) record {
	f.t.Helper()
	ctx := context.Background()
	body := make([]byte, 4096)
	_, _ = rand.Read(body)
	copy(body, []byte("\x89PNG\r\n\x1a\n"))
	sum := sha256.Sum256(body)
	next := record{id: uuid.New(), hash: hex.EncodeToString(sum[:]), body: body, peer: r.peer, object: r.object}
	temp := storage.PinRef{SubjectType: "ra-test", SubjectID: next.id.String()}
	if _, err := f.svc.UploadOriginal(ctx, bytes.NewReader(body), "image/png", temp); err != nil {
		f.t.Fatal(err)
	}
	f.cleanup = append(f.cleanup, func() {
		c := context.Background()
		_, _ = f.pool.Exec(c, `DELETE FROM storage_pins WHERE object_hash = $1`, next.hash)
		_, _ = f.pool.Exec(c, `DELETE FROM storage_variants WHERE object_hash = $1`, next.hash)
		_, _ = f.pool.Exec(c, `DELETE FROM storage_objects WHERE hash = $1`, next.hash)
	})
	if err := pgx.BeginFunc(ctx, f.pool, func(tx pgx.Tx) error {
		for _, q := range []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO federation_received_assets
			  (id, peer_id, activity_uri, actor_uri, origin_asset_uri, origin_asset_id, version_uri, version_number,
			   file_set_sha256, record, sensitivity, signer_key_id, predecessor_version_uri)
			  SELECT $1, peer_id, $2, actor_uri, origin_asset_uri, origin_asset_id, origin_asset_uri || '/versions/2', 2,
			   $3, record, sensitivity, 'k', version_uri FROM federation_received_assets WHERE id = $4`,
				[]any{next.id, r.peer.url + "/activities/" + uuid.NewString(), strings.Repeat("1", 64), r.id}},
			{`INSERT INTO federation_received_asset_files
			  (received_asset_id, position, role, object_hash, size_bytes, checksums, declared_media_type, declared_original_filename)
			  VALUES ($1, 0, 'original', $2, $3, '[]'::jsonb, 'image/png', 'harbour-2.png')`, []any{next.id, next.hash, len(body)}},
			{`INSERT INTO storage_pins (object_hash, pin_subject_type, pin_subject_id) VALUES ($2, 'federation_received_asset', $1::text)`,
				[]any{next.id, next.hash}},
			{`UPDATE federation_received_asset_files SET state = 'verified', verified_at = now(),
			    detected_media_type = 'image/png', detected_by = 'net/http.DetectContentType'
			  WHERE received_asset_id = $1`, []any{next.id}},
			{`UPDATE federation_received_assets SET state = 'superseded', superseded_at = now() WHERE id = $1`, []any{r.id}},
			{`UPDATE federation_received_assets SET state = 'complete', completed_at = now(), authorizing_grant_id = gen_random_uuid(),
			    predecessor_id = $2 WHERE id = $1`, []any{next.id, r.id}},
		} {
			if _, err := tx.Exec(ctx, q.sql, q.args...); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		f.t.Fatalf("supersede: %v", err)
	}
	if err := f.svc.RemovePin(ctx, temp, next.hash); err != nil {
		f.t.Fatal(err)
	}
	return next
}

// Only the current version is shown (#1575): the version a later one
// replaced is listed nowhere and answers 404, its successor is served in
// full under the same grant, and the SQL rules agree with their Go twins
// on the superseded record. A withdrawal answers the notice for both.
func TestAPI_OnlyTheCurrentVersionIsShown(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	v1 := f.complete(f.peerA, "public")
	f.grant(v1, true, "", "")
	f.want("version 1 before version 2", f.u1, v1, everything)
	v2 := f.supersede(v1)
	f.want("version 2", f.u1, v2, everything)
	f.want("version 1 after version 2 replaced it", f.u1, v1, nothing)
	if _, ok := mustGet(t, f, f.u1, v1.id).(openapi.GetReceivedAsset404JSONResponse); !ok {
		t.Fatal("version 1's detail is not the same 404 as a missing record")
	}
	got, ok := mustGet(t, f, f.u1, v2.id).(openapi.GetReceivedAsset200JSONResponse)
	if !ok || got.Body.Asset.VersionNumber != 2 || got.Body.Asset.VersionUri != v1.object+"/versions/2" || uuid.UUID(got.Body.Asset.Id) != v2.id {
		t.Fatalf("version 2's detail: %+v", got)
	}
	facts, err := receivedassets.LoadFacts(ctx, f.pool, v1.id)
	if err != nil {
		t.Fatal(err)
	}
	for _, caller := range []int64{0, f.u1, f.u2} {
		var view, orig, notice bool
		var now time.Time
		if err := f.pool.QueryRow(ctx, `SELECT `+receivedassets.ViewableSQL("r", "$2")+`, `+receivedassets.OriginalReadableSQL("r", "$2")+`,
			`+receivedassets.WithdrawnVisibleSQL("r", "$2")+`, now() FROM federation_received_assets r WHERE r.id = $1`, v1.id, caller).Scan(&view, &orig, &notice, &now); err != nil {
			t.Fatal(err)
		}
		gv, gor, gn := receivedassets.Viewable(facts, caller, now), receivedassets.OriginalReadable(facts, caller, now), receivedassets.WithdrawnVisible(facts, caller, now)
		if view || orig || notice || gv || gor || gn {
			t.Fatalf("caller %d: the superseded version is SQL viewable=%v original=%v notice=%v, Go %v %v %v", caller, view, orig, notice, gv, gor, gn)
		}
	}

	// The origin withdraws the asset: every version answers the notice to
	// who could view it.
	// The tombstone and version 2 (f.withdraw), then version 1 the same
	// way, in one transaction: a superseded version keeps its pin until it
	// is withdrawn (the commit check refuses anything else).
	f.withdraw(v2, nil)
	if err := pgx.BeginFunc(ctx, f.pool, func(tx pgx.Tx) error {
		for _, q := range []string{
			`SELECT federation_received_asset_release_pins(ARRAY[$1::text])`,
			`UPDATE federation_received_assets SET state = 'withdrawn', reason = 'withdrawn', withdrawn_at = now() WHERE id = $1::uuid`,
			`UPDATE federation_received_asset_files SET state = 'withdrawn' WHERE received_asset_id = $1::uuid AND role = 'original'`,
		} {
			if _, err := tx.Exec(ctx, q, v1.id.String()); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("withdraw version 1: %v", err)
	}
	for _, r := range []record{v1, v2} {
		if _, ok := mustGet(t, f, f.u1, r.id).(openapi.GetReceivedAsset410JSONResponse); !ok {
			t.Fatalf("record %s after the withdrawal: not the 410 notice", r.id)
		}
		f.want("after the withdrawal", f.u1, r, nothing)
	}
}
