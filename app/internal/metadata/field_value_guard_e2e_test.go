// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// PER-FIELD optimistic concurrency on ordinary field values (#1119).
//
// The token is the VALUE ROW'S OWN `set_at`, never the subject's
// `updated_at`. That is the property S-i below exists to prove: two
// people editing two different fields of one asset are not in conflict,
// and a subject-level guard would fail its part (1).
//
// Everything here is SEQUENTIAL and deterministic by construction — the
// second write simply happens after the first. The genuinely
// overlapping cases live in field_value_race_test.go, which needs a
// synchronisation seam these do not.
package metadata_test

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// setAtOf pulls the token out of a write response. It is a STRING, kept
// verbatim: re-parsing and re-formatting a timestamp is how a guard
// starts failing on microsecond rounding.
func setAtOf(t *testing.T, body map[string]any) string {
	t.Helper()
	s, _ := body["set_at"].(string)
	if s == "" {
		t.Fatalf("response carries no set_at, so there is no token to guard with: %v", body)
	}
	return s
}

func (e *vocabEnv) clearAssetGuarded(t *testing.T, fieldID, token string) (int, map[string]any) {
	t.Helper()
	path := fmt.Sprintf("/assets/%s/fields/%s", e.assetID, fieldID)
	if token != "" {
		path += "?if_unchanged_since=" + url.QueryEscape(token)
	}
	rr := deleteReq(t, e.router, path)
	var m map[string]any
	if rr.Body.Len() > 0 {
		m = decodeBody(t, rr.Body.Bytes())
	}
	return rr.Code, m
}

func (e *vocabEnv) clearCollectionGuarded(t *testing.T, fieldID, token string) (int, map[string]any) {
	t.Helper()
	path := fmt.Sprintf("/collections/%s/fields/%s", e.collID, fieldID)
	if token != "" {
		path += "?if_unchanged_since=" + url.QueryEscape(token)
	}
	rr := deleteReq(t, e.router, path)
	var m map[string]any
	if rr.Body.Len() > 0 {
		m = decodeBody(t, rr.Body.Bytes())
	}
	return rr.Code, m
}

// assertConflict checks the 409 wire contract, and the part it checks
// most carefully is the part a client cannot work around: `current` is
// REQUIRED and NULLABLE, so the KEY IS ALWAYS PRESENT. A body that
// omits it on the cleared case is indistinguishable from a server that
// forgot to send one, and the client has no way to tell "removed" from
// "unknown".
func assertConflict(t *testing.T, status int, raw []byte, body map[string]any, wantPresent bool) map[string]any {
	t.Helper()
	if status != http.StatusConflict {
		t.Fatalf("status=%d want 409; body=%v", status, body)
	}
	if _, ok := body["field_id"]; !ok {
		t.Errorf("409 body must name field_id: %v", body)
	}
	if got, ok := body["present"].(bool); !ok || got != wantPresent {
		t.Errorf("present=%v want %v; body=%v", body["present"], wantPresent, body)
	}
	if msg, _ := body["error"].(string); msg == "" {
		t.Errorf("409 body must carry a sentence: %v", body)
	}
	cur, keyPresent := body["current"]
	if !keyPresent {
		t.Fatalf("`current` is REQUIRED and nullable — the key must always be present, even when null. raw=%s", raw)
	}
	if !wantPresent {
		if cur != nil {
			t.Errorf("present:false must carry current:null, got %v", cur)
		}
		return nil
	}
	m, ok := cur.(map[string]any)
	if !ok {
		t.Fatalf("present:true must carry the current value, got %T (%v)", cur, cur)
	}
	if _, ok := m["set_at"]; !ok {
		t.Errorf("the conflicting value must carry its set_at — it is the token a retry guards with: %v", m)
	}
	return m
}

// ---------------------------------------------------------------------------
// Guard arithmetic: mutual exclusion, and the unguarded default
// ---------------------------------------------------------------------------

func TestGuard_MutuallyExclusive(t *testing.T) {
	env := newVocabEnv(t)

	t.Run("asset", func(t *testing.T) {
		fid := env.assetField(t, "g_both", "text", nil)
		code, body := env.putAsset(t, fid, map[string]any{
			"value_text": "x", "if_absent": true, "if_unchanged_since": "2026-01-01T00:00:00Z",
		})
		if code != http.StatusBadRequest {
			t.Fatalf("both guards: status=%d want 400; body=%v", code, body)
		}
		if _, ok := readStored(t, env.pool, env.assetID, fid); ok {
			t.Error("a refused guard combination stored a value")
		}
	})

	t.Run("collection", func(t *testing.T) {
		fid := env.collectionField(t, "g_both", "text", nil)
		code, body := env.putCollection(t, fid, map[string]any{
			"value_text": "x", "if_absent": true, "if_unchanged_since": "2026-01-01T00:00:00Z",
		})
		if code != http.StatusBadRequest {
			t.Fatalf("both guards: status=%d want 400; body=%v", code, body)
		}
	})
}

// The unguarded path is not legacy to be tightened later: the upload
// flush depends on it, and so does every non-edit-surface caller.
func TestGuard_UnguardedKeepsLastWriteWins(t *testing.T) {
	env := newVocabEnv(t)
	fid := env.assetField(t, "g_unguarded", "text", nil)

	if code, _ := env.putAsset(t, fid, map[string]any{"value_text": "first"}); code != http.StatusOK {
		t.Fatal("first")
	}
	if code, _ := env.putAsset(t, fid, map[string]any{"value_text": "second"}); code != http.StatusOK {
		t.Fatal("an unguarded write must still overwrite")
	}
	got, _ := readStored(t, env.pool, env.assetID, fid)
	if got.Text == nil || *got.Text != "second" {
		t.Errorf("stored=%v want second", got.Text)
	}

	// Unguarded clear: 204 whether or not a row existed.
	if code, body := env.clearAsset(t, fid); code != http.StatusNoContent {
		t.Fatalf("unguarded clear: status=%d body=%v", code, body)
	}
	if code, body := env.clearAsset(t, fid); code != http.StatusNoContent {
		t.Fatalf("unguarded clear on an absent row: status=%d body=%v", code, body)
	}
}

// ---------------------------------------------------------------------------
// The sequential race matrix, both subject kinds
// ---------------------------------------------------------------------------

type guardSubject struct {
	name  string
	field func(t *testing.T, code, ftype string) string
	put   func(t *testing.T, fieldID string, body map[string]any) (int, map[string]any)
	clear func(t *testing.T, fieldID, token string) (int, map[string]any)
	read  func(t *testing.T, fieldID string) (storedValue, bool)
	raw   func(t *testing.T, fieldID string, body map[string]any) ([]byte, int, map[string]any)
}

func guardSubjects(env *vocabEnv) []guardSubject {
	return []guardSubject{
		{
			name:  "asset",
			field: func(t *testing.T, code, ftype string) string { return env.assetField(t, code, ftype, nil) },
			put:   env.putAsset,
			clear: env.clearAssetGuarded,
			read: func(t *testing.T, fieldID string) (storedValue, bool) {
				return readStored(t, env.pool, env.assetID, fieldID)
			},
			raw: func(t *testing.T, fieldID string, body map[string]any) ([]byte, int, map[string]any) {
				rr := putJSON(t, env.router, fmt.Sprintf("/assets/%s/fields/%s", env.assetID, fieldID), body)
				return rr.Body.Bytes(), rr.Code, decodeBody(t, rr.Body.Bytes())
			},
		},
		{
			name:  "collection",
			field: func(t *testing.T, code, ftype string) string { return env.collectionField(t, code, ftype, nil) },
			put:   env.putCollection,
			clear: env.clearCollectionGuarded,
			read: func(t *testing.T, fieldID string) (storedValue, bool) {
				return readCollectionStored(t, env.pool, env.collID, fieldID)
			},
			raw: func(t *testing.T, fieldID string, body map[string]any) ([]byte, int, map[string]any) {
				rr := putJSON(t, env.router, fmt.Sprintf("/collections/%s/fields/%s", env.collID, fieldID), body)
				return rr.Body.Bytes(), rr.Code, decodeBody(t, rr.Body.Bytes())
			},
		},
	}
}

func TestGuard_SequentialMatrix(t *testing.T) {
	env := newVocabEnv(t)

	for _, s := range guardSubjects(env) {
		t.Run(s.name, func(t *testing.T) {
			t.Run("stale_set_after_set", func(t *testing.T) {
				fid := s.field(t, "g_ss", "text")
				_, first := s.put(t, fid, map[string]any{"value_text": "one"})
				token := setAtOf(t, first)
				if code, _ := s.put(t, fid, map[string]any{"value_text": "two"}); code != http.StatusOK {
					t.Fatal("somebody else's write")
				}
				raw, code, body := s.raw(t, fid, map[string]any{"value_text": "stale", "if_unchanged_since": token})
				cur := assertConflict(t, code, raw, body, true)
				if cur["value_text"] != "two" {
					t.Errorf("current.value_text=%v want two", cur["value_text"])
				}
				got, _ := s.read(t, fid)
				if got.Text == nil || *got.Text != "two" {
					t.Errorf("the stale write overwrote the newer value: %v", got.Text)
				}
			})

			t.Run("stale_set_after_clear_creates_nothing", func(t *testing.T) {
				fid := s.field(t, "g_sc", "text")
				_, first := s.put(t, fid, map[string]any{"value_text": "one"})
				token := setAtOf(t, first)
				if code, _ := s.clear(t, fid, ""); code != http.StatusNoContent {
					t.Fatal("clear")
				}
				raw, code, body := s.raw(t, fid, map[string]any{"value_text": "resurrect", "if_unchanged_since": token})
				assertConflict(t, code, raw, body, false)
				// THE RESURRECTION GUARD. if_unchanged_since on a row
				// that does not exist is a conflict, not an insert.
				if _, ok := s.read(t, fid); ok {
					t.Error("a guarded write against a cleared row created one anyway")
				}
			})

			t.Run("stale_clear_after_set", func(t *testing.T) {
				fid := s.field(t, "g_cs", "text")
				_, first := s.put(t, fid, map[string]any{"value_text": "one"})
				token := setAtOf(t, first)
				if code, _ := s.put(t, fid, map[string]any{"value_text": "newer"}); code != http.StatusOK {
					t.Fatal("newer write")
				}
				code, body := s.clear(t, fid, token)
				if code != http.StatusConflict {
					t.Fatalf("stale guarded clear: status=%d want 409; body=%v", code, body)
				}
				if got, ok := body["present"].(bool); !ok || !got {
					t.Errorf("present=%v want true", body["present"])
				}
				got, ok := s.read(t, fid)
				if !ok || got.Text == nil || *got.Text != "newer" {
					t.Errorf("A STALE CLEAR ERASED A NEWER VALUE: %v", got.Text)
				}
			})

			t.Run("stale_clear_after_clear", func(t *testing.T) {
				fid := s.field(t, "g_cc", "text")
				_, first := s.put(t, fid, map[string]any{"value_text": "one"})
				token := setAtOf(t, first)
				if code, _ := s.clear(t, fid, ""); code != http.StatusNoContent {
					t.Fatal("clear")
				}
				code, body := s.clear(t, fid, token)
				if code != http.StatusConflict {
					t.Fatalf("stale guarded clear on an absent row: status=%d want 409; body=%v", code, body)
				}
				if got, ok := body["present"].(bool); !ok || got {
					t.Errorf("present=%v want false", body["present"])
				}
				if cur, keyPresent := body["current"]; !keyPresent {
					t.Errorf("`current` key must be present even when null: %v", body)
				} else if cur != nil {
					t.Errorf("current must be null: %v", cur)
				}
			})

			t.Run("if_absent_against_an_existing_row", func(t *testing.T) {
				fid := s.field(t, "g_ia", "text")
				if code, _ := s.put(t, fid, map[string]any{"value_text": "already here"}); code != http.StatusOK {
					t.Fatal("seed")
				}
				raw, code, body := s.raw(t, fid, map[string]any{"value_text": "mine", "if_absent": true})
				cur := assertConflict(t, code, raw, body, true)
				if cur["value_text"] != "already here" {
					t.Errorf("current=%v", cur["value_text"])
				}
				got, _ := s.read(t, fid)
				if got.Text == nil || *got.Text != "already here" {
					t.Errorf("if_absent overwrote an existing row: %v", got.Text)
				}
			})

			t.Run("if_absent_first_write_succeeds", func(t *testing.T) {
				fid := s.field(t, "g_ia2", "text")
				code, body := s.put(t, fid, map[string]any{"value_text": "first", "if_absent": true})
				if code != http.StatusOK {
					t.Fatalf("if_absent on a genuinely absent row: status=%d body=%v", code, body)
				}
				setAtOf(t, body)
				got, ok := s.read(t, fid)
				if !ok || got.Text == nil || *got.Text != "first" {
					t.Errorf("stored=%v", got.Text)
				}
			})

			// The 409's set_at is not decoration: a client re-baselines
			// on it and its next write must be accepted.
			t.Run("conflict_token_is_usable", func(t *testing.T) {
				fid := s.field(t, "g_rebase", "text")
				_, first := s.put(t, fid, map[string]any{"value_text": "one"})
				stale := setAtOf(t, first)
				if code, _ := s.put(t, fid, map[string]any{"value_text": "two"}); code != http.StatusOK {
					t.Fatal("newer")
				}
				raw, code, body := s.raw(t, fid, map[string]any{"value_text": "x", "if_unchanged_since": stale})
				cur := assertConflict(t, code, raw, body, true)
				fresh, _ := cur["set_at"].(string)
				if fresh == "" {
					t.Fatal("no set_at on the conflicting value")
				}
				code, body = s.put(t, fid, map[string]any{"value_text": "reconciled", "if_unchanged_since": fresh})
				if code != http.StatusOK {
					t.Fatalf("re-baselined write refused: status=%d body=%v", code, body)
				}
				got, _ := s.read(t, fid)
				if got.Text == nil || *got.Text != "reconciled" {
					t.Errorf("stored=%v", got.Text)
				}
			})

			// A guarded write must advance the token, or the SECOND save
			// from one open form is guaranteed to conflict with itself.
			t.Run("successful_guarded_write_returns_a_new_token", func(t *testing.T) {
				fid := s.field(t, "g_advance", "text")
				_, first := s.put(t, fid, map[string]any{"value_text": "one", "if_absent": true})
				t1 := setAtOf(t, first)
				code, second := s.put(t, fid, map[string]any{"value_text": "two", "if_unchanged_since": t1})
				if code != http.StatusOK {
					t.Fatalf("guarded write: status=%d body=%v", code, second)
				}
				t2 := setAtOf(t, second)
				if t2 == t1 {
					t.Fatal("set_at did not advance, so a second guarded save could never succeed")
				}
				if code, body := s.put(t, fid, map[string]any{"value_text": "three", "if_unchanged_since": t2}); code != http.StatusOK {
					t.Fatalf("second save from the RETURNED token: status=%d body=%v", code, body)
				}
			})

			// S-i, the field-locality discriminator. A SUBJECT-level
			// guard fails part (1): editing Y would invalidate the token
			// the editor holds for X, and two people working on two
			// different fields of one record would collide constantly.
			t.Run("locality_neighbour_write_does_not_invalidate", func(t *testing.T) {
				x := s.field(t, "g_locx", "text")
				y := s.field(t, "g_locy", "text")
				_, bx := s.put(t, x, map[string]any{"value_text": "x1"})
				tokenX := setAtOf(t, bx)
				if code, _ := s.put(t, y, map[string]any{"value_text": "y1"}); code != http.StatusOK {
					t.Fatal("seed y")
				}
				// (1) somebody changed Y only — the guarded write to X SUCCEEDS.
				if code, _ := s.put(t, y, map[string]any{"value_text": "y2"}); code != http.StatusOK {
					t.Fatal("neighbour write")
				}
				if code, body := s.put(t, x, map[string]any{"value_text": "x2", "if_unchanged_since": tokenX}); code != http.StatusOK {
					t.Fatalf("A NEIGHBOUR'S WRITE INVALIDATED THIS FIELD'S TOKEN — the guard is not field-local: status=%d body=%v", code, body)
				}
				// (2) somebody changed X — the stale write to X 409s.
				_, bx2 := s.put(t, x, map[string]any{"value_text": "x3"})
				setAtOf(t, bx2)
				raw, code, body := s.raw(t, x, map[string]any{"value_text": "x4", "if_unchanged_since": tokenX})
				assertConflict(t, code, raw, body, true)

				gotY, _ := s.read(t, y)
				if gotY.Text == nil || *gotY.Text != "y2" {
					t.Errorf("neighbour Y disturbed: %v", gotY.Text)
				}
			})
		})
	}
}

// ---------------------------------------------------------------------------
// Mirrored fields are OUTSIDE the per-field contract
// ---------------------------------------------------------------------------

// A mirrored value IS the asset's column: there is no asset_field_value
// row to carry a `set_at`, so a per-field token would guard a column
// against a timestamp from a different plane. Both guards are refused
// by name, and the UNGUARDED mirrored write is untouched.
func TestGuard_MirroredFieldsRefuseBothGuards(t *testing.T) {
	env := newVocabEnv(t)
	fid := mirroredFieldID(t, env, "description")

	for _, body := range []map[string]any{
		{"value_text": "x", "if_absent": true},
		{"value_text": "x", "if_unchanged_since": "2026-01-01T00:00:00Z"},
	} {
		code, got := env.putAsset(t, fid, body)
		if code != http.StatusBadRequest {
			t.Errorf("guarded mirrored write: status=%d want 400; body=%v", code, got)
		}
	}

	code, got := env.clearAssetGuarded(t, fid, "2026-01-01T00:00:00Z")
	if code != http.StatusBadRequest {
		t.Errorf("guarded mirrored clear: status=%d want 400; body=%v", code, got)
	}

	// Unchanged: the unguarded mirrored write still works.
	if code, body := env.putAsset(t, fid, map[string]any{"value_text": "still writable"}); code != http.StatusOK {
		t.Fatalf("unguarded mirrored write regressed: status=%d body=%v", code, body)
	}
}
