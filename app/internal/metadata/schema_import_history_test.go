// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// The history handlers' failure paths and the cursor codec (#1576 S4),
// without a database: a query that fails is a 500 JSON error, never a
// 200 page; and only the canonical spelling of a position is a cursor.
// The routed, database-backed behaviour is tested in
// app/internal/http/fields_import_history_test.go.

package metadata

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

func TestImportHistory_QueryErrorIsA500(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), "postgres://nobody@127.0.0.1:1/none?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	pool.Close() // every query now fails
	h := &Handler{Pool: pool}
	admin := auth.WithIdentity(context.Background(), &auth.Identity{UserRef: 1, Capabilities: []string{CapFieldsAdmin}})
	cases := []struct {
		name  string
		serve func(w http.ResponseWriter, r *http.Request)
		url   string
	}{
		{"list", h.ServeImportReceipts, "/api/v1/fields/import/receipts?limit=5"},
		{"detail", func(w http.ResponseWriter, r *http.Request) { h.ServeImportReceipt(w, r, uuid.NewString()) }, "/api/v1/fields/import/receipts/x"},
	}
	for _, c := range cases {
		w := httptest.NewRecorder()
		c.serve(w, httptest.NewRequest(http.MethodGet, c.url, nil).WithContext(admin))
		var body openapi.Error
		if w.Code != http.StatusInternalServerError || json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Error == "" {
			t.Errorf("%s: %d %s, want a 500 JSON error", c.name, w.Code, w.Body.String())
		}
	}
}

func TestImportHistory_CursorIsCanonical(t *testing.T) {
	at := time.Date(2999, 1, 1, 0, 0, 0, 123456000, time.UTC)
	id := uuid.MustParse("6efb7dfd-23ff-4712-a5a6-d0731604dc11")
	good := encodeReceiptCursor(at, id)
	gotAt, gotID, err := decodeReceiptCursor(good)
	if err != nil || !gotAt.Equal(at) || gotID != id {
		t.Fatalf("round trip: %v %v %v", gotAt, gotID, err)
	}
	body := strings.TrimPrefix(good, receiptCursorPrefix)
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	last := strings.IndexByte(alphabet, body[len(body)-1])
	bad := map[string]string{
		"newline":       receiptCursorPrefix + body[:8] + "\n" + body[8:],
		"crlf":          receiptCursorPrefix + body[:8] + "\r\n" + body[8:],
		"trailing bits": receiptCursorPrefix + body[:len(body)-1] + string(alphabet[last+1]),
		"padding":       receiptCursorPrefix + base64.URLEncoding.EncodeToString([]byte(at.Format(time.RFC3339Nano)+"|"+id.String())),
		"std alphabet":  receiptCursorPrefix + base64.RawStdEncoding.EncodeToString([]byte(at.Format(time.RFC3339Nano)+"|"+id.String()+"\xfb")),
		"upper uuid":    receiptCursorPrefix + base64.RawURLEncoding.EncodeToString([]byte(at.Format(time.RFC3339Nano)+"|"+strings.ToUpper(id.String()))),
		"offset time":   receiptCursorPrefix + base64.RawURLEncoding.EncodeToString([]byte("2999-01-01T01:00:00.123456+01:00|"+id.String())),
		"prefix":        "v2." + body,
	}
	for name, c := range bad {
		if _, _, err := decodeReceiptCursor(c); err == nil {
			t.Errorf("%s: accepted %q", name, c)
		}
	}
	// A well-formed cursor for a position nobody was given is still that
	// position: the cursor is not a credential.
	other := encodeReceiptCursor(time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC), uuid.MustParse("00000000-0000-4000-8000-000000000000"))
	if _, _, err := decodeReceiptCursor(other); err != nil {
		t.Errorf("constructed canonical cursor refused: %v", err)
	}
}
