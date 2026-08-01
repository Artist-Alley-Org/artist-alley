// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package storage

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path"
	"strings"

	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------------------
// Split-brain reconcile (#827)
//
// Storage has two planes: the bytes (Backend) and the record of them
// (storage_variants). Everything that ASKS "does this preview exist?"
// before doing work is backend-first, because the bytes are what a
// request actually serves. Everything the API ANNOUNCES to the client —
// preview_available (#471), ladder_available (#610) — is DB-first,
// because per the zero-404 design the client never requests bytes the
// server did not announce.
//
// Those two answers are allowed to disagree, and when they do the
// install is silently broken in a way no signal reports. Recreate the
// database while keeping the storage volume — which is what restoring a
// backup does — and every preview job finds its bytes, skips, and
// reports `done`, while every card stays permanently blurred because no
// row says the `col` rung is there. Measured on dev at 785 variant rows
// for 1,947 assets.
//
// The fix is not to make the skip check DB-first (a row without bytes
// still 404s — that trade is strictly worse). It is to make the skip
// HEAL: when the backend says the bytes are there, write the row the
// renderer would have written, and let the announcement catch up with
// the truth.
// ---------------------------------------------------------------------------

// ReconcileVariant makes storage_variants agree with bytes that are
// demonstrably on the backend, and reports whether it had to insert a
// row to do it.
//
// info is the Stat the caller already performed — reconciling is meant
// to ride along on an existence check that happened anyway, never to
// add a second probe. Pass nil and the method stats for itself.
//
// Cost in the healthy case is one indexed EXISTS. The read-back that
// determines a content type only happens when a row is genuinely
// missing, which outside a restored-backup install is never.
func (s *Service) ReconcileVariant(ctx context.Context, hash, variant string, info *ObjectInfo) (bool, error) {
	if s == nil || s.Pool == nil || hash == "" || variant == "" {
		return false, nil
	}
	q := New(s.Pool)
	exists, err := q.VariantExists(ctx, VariantExistsParams{ObjectHash: hash, VariantKey: variant})
	if err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}
	if info == nil {
		info, err = s.Backend.Stat(ctx, hash, variant)
		if err != nil {
			return false, err
		}
	}
	// A row for a variant whose object we have never heard of would
	// violate storage_variants_object_hash_fkey. That is its own (worse)
	// split brain — bytes on the volume with no storage_objects row at
	// all — and healing it is the orphan sweep's remit, not this one's.
	// Report the miss rather than fail a preview job that was only
	// trying to skip.
	if _, err := q.FindObject(ctx, hash); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	n, err := q.InsertVariantIfMissing(ctx, InsertVariantIfMissingParams{
		ObjectHash:  hash,
		VariantKey:  variant,
		SizeBytes:   info.Size,
		ContentType: s.variantContentType(ctx, hash, variant),
		Metadata:    []byte("{}"),
	})
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ReconcileVariantBestEffort is ReconcileVariant for callers that are in
// the middle of deciding NOT to do work — every preview handler's skip
// path. A heal that fails must never turn a correct skip into a failed
// job, so the error is logged (at Warn: a split brain that will not heal
// is exactly the silence #827 is about) and swallowed.
//
// Returns whether a row was actually inserted, so a caller that wants to
// react to "this asset was split-brained until a moment ago" can.
func (s *Service) ReconcileVariantBestEffort(ctx context.Context, hash, variant string, info *ObjectInfo) bool {
	healed, err := s.ReconcileVariant(ctx, hash, variant, info)
	if err != nil {
		if s != nil && s.Logger != nil {
			s.Logger.LogAttrs(ctx, slog.LevelWarn, "storage.reconcile_variant_failed",
				slog.String("object_hash", hash),
				slog.String("variant_key", variant),
				slog.String("err", err.Error()))
		}
		return false
	}
	if healed && s != nil && s.Logger != nil {
		s.Logger.LogAttrs(ctx, slog.LevelInfo, "storage.reconcile_variant_healed",
			slog.String("object_hash", hash),
			slog.String("variant_key", variant))
	}
	return healed
}

// variantContentType answers what the renderer would have recorded for
// this variant key.
//
// It is NOT taken from ObjectInfo.ContentType. The FS backend has
// nowhere to store one and returns a flat "application/octet-stream"
// for every object it stats, so trusting Stat here would file every
// healed thumbnail on an install using local storage — the default —
// under the wrong type, and the admin storage breakdown groups on
// exactly this column.
//
// Keys that carry an extension (poster.jpg is `poster`, but sprites.jpg,
// sprites.vtt and the whole hls/ tree are named for their format) are
// answered from the extension, matching what the uploading handler
// passed. Ladder rungs — `col`, `preview`, `screen`, `hires` — have no
// extension AND no fixed format: encodeImage promotes a JPEG rung to PNG
// when the source has real transparency, so the install's configured
// format is not the answer either. For those the bytes are sniffed, which
// is the only source that cannot be wrong.
func (s *Service) variantContentType(ctx context.Context, hash, variant string) string {
	if ct := contentTypeForVariantKey(variant); ct != "" {
		return ct
	}
	rc, err := s.Backend.GetRange(ctx, hash, variant, 0, 512)
	if err != nil {
		return "application/octet-stream"
	}
	defer rc.Close()
	buf := make([]byte, 512)
	n, err := io.ReadFull(rc, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return "application/octet-stream"
	}
	return http.DetectContentType(buf[:n])
}

// contentTypeForVariantKey maps the extension a variant key carries to
// the content type the handler that wrote it passed to Put. Returns ""
// for extension-less keys (the raster ladder rungs), which the caller
// resolves by sniffing.
//
// The three non-image entries are the ones a generic mime table gets
// wrong or misses: an .m3u8 sniffs as text/plain, a .ts sniffs as
// application/octet-stream, and a .vtt as text/plain — all three would
// then disagree with the type the video handler recorded on the write
// path, for the same bytes.
func contentTypeForVariantKey(variant string) string {
	switch strings.ToLower(path.Ext(variant)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	case ".m3u8":
		return "application/vnd.apple.mpegurl"
	case ".ts":
		return "video/mp2t"
	case ".vtt":
		return "text/vtt"
	case ".json":
		return "application/json"
	}
	return ""
}
