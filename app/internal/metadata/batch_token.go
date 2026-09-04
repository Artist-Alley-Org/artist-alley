// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// THE PREVIEW TOKEN — what binds a preview to its apply (#1173, #1119,
// ADR 0019).
//
// The apply endpoint sends a token, an operator reason, and for two of
// the four modes a confirmation count. It sends NO MODE and NO VALUE.
// Everything else it needs is bound here, at preview time, which is
// what makes "was the previewed set the applied set" answerable at all
// — and it is why no payload-mismatch refusal exists in this contract:
// there is no second copy of the mode or the value for a request to
// disagree with.
//
// # What the token binds
//
//   - the ORDERED TARGET SET, each target with its partition and, for a
//     would_change target, that value row's own `set_at`
//   - the field's identity and its CONFIGURATION FINGERPRINT, including
//     the vocabulary options document
//   - the MODE
//   - the exact CANONICAL VALUE the apply will store
//   - the CALLER
//
// # What the token is NOT
//
// It is NEVER AUTHORITY. Not authority to write, not authority to mint
// a vocabulary term, not evidence that a reference target is still
// alive. Every one of those is re-evaluated against the CURRENT world
// at apply time, because a token is a record of what was true when the
// operator looked, and the whole point of re-checking is that it may
// have stopped being true. A token that conferred authority would be a
// capability grant with a fifteen-minute expiry that no administrator
// could revoke.
//
// # Why the binding is a server-side row
//
// A signed self-contained token would have to carry a thousand target
// UUIDs with their partitions and their timestamps — tens of kilobytes
// of opaque base64 on every apply — AND STILL need a durable row,
// because SINGLE-USE is a fact about the world rather than about the
// bearer, and no amount of signing makes a bearer forget. So the row is
// the narrower representation, not the heavier one: 32 random bytes on
// the wire, and the binding somewhere it can be transactionally
// consumed alongside the writes it authorises.
package metadata

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// batchPreviewTTL bounds how long a preview may sit before its apply.
//
// Long enough for an operator to read a partition breakdown, think, and
// type a confirmation count; short enough that the world they were
// shown is still recognisably the world. Expiry is NOT the safety
// mechanism — the apply re-checks authority, configuration, vocabulary
// and reference liveness whatever the clock says, and every per-target
// write is guarded on the `set_at` the preview saw. It is a bound on
// how stale a report an operator may act on.
const batchPreviewTTL = 15 * time.Minute

// batchTokenBytes is the token's entropy. 256 bits: the token is a
// bearer credential and guessing one must not be a strategy.
const batchTokenBytes = 32

// batchTokenPayload is the durable binding, stored as jsonb.
type batchTokenPayload struct {
	Mode      string `json:"mode"`
	FieldID   string `json:"field_id"`
	FieldCode string `json:"field_code"`
	FieldType string `json:"field_type"`

	// DefinitionFingerprint covers every configuration property whose
	// change invalidates the whole preview. VocabularyFingerprint is
	// SEPARATE, and separate on purpose: the two produce DIFFERENT
	// refusals (definition_drift and vocabulary_drift) because they
	// call for different corrections, and one combined hash could only
	// ever say "something moved".
	DefinitionFingerprint string `json:"definition_fingerprint"`
	VocabularyFingerprint string `json:"vocabulary_fingerprint"`

	// Value is the CANONICAL value — vocabulary already canonicalised,
	// rich text already sanitised. The bytes the apply stores, not the
	// bytes the operator typed.
	Value batchTokenValue `json:"value"`

	// Mintable are canonical slugs that did not exist at preview time
	// and would be created. Listed, not created: a preview mutates no
	// options document.
	Mintable []string `json:"mintable,omitempty"`

	// Targets is the ordered set with each target's partition. Apply
	// writes ONLY the would_change entries and NEVER re-expands — a
	// post that gained a member after the preview does not enlarge the
	// operation the operator confirmed.
	Targets []batchTokenTarget `json:"targets"`

	Counts              batchCounts `json:"counts"`
	SelectionEntryCount int         `json:"selection_entry_count"`
	EmptyPosts          []string    `json:"empty_posts,omitempty"`
}

type batchTokenValue struct {
	Text    *string    `json:"text,omitempty"`
	Num     *float64   `json:"num,omitempty"`
	Date    *time.Time `json:"date,omitempty"`
	Options []string   `json:"options,omitempty"`
	Ref     *string    `json:"ref,omitempty"`
}

type batchTokenTarget struct {
	AssetID   string `json:"asset_id"`
	Partition string `json:"partition"`
	// Reason is the machine reason on a `refused` target.
	Reason string `json:"reason,omitempty"`
	// Present and SetAt are the concurrency token for a would_change
	// target: whether a value row existed, and if so that row's own
	// `set_at`. The apply guards its write on exactly this, which is
	// how a value that moved between preview and apply becomes a
	// per-target `conflict` rather than a silent overwrite.
	Present bool       `json:"present,omitempty"`
	SetAt   *time.Time `json:"set_at,omitempty"`
	// Delete marks the one case where a would_change target's write is
	// a removal: `remove` emptying an OPTIONAL multi_select.
	Delete bool `json:"delete,omitempty"`
	// Next is the per-target result for the set modes, where what gets
	// stored depends on what the target held.
	NextOptions []string `json:"next_options,omitempty"`
}

// batchCounts is the six-way partition plus both derived totals.
type batchCounts struct {
	Expanded     int `json:"expanded"`
	Eligible     int `json:"eligible"`
	WouldChange  int `json:"would_change"`
	NoOp         int `json:"no_op"`
	Refused      int `json:"refused"`
	Inapplicable int `json:"inapplicable"`
	Unreadable   int `json:"unreadable"`
	Unauthorized int `json:"unauthorized"`
}

func (c batchCounts) wire() openapi.BatchAssetFieldCounts {
	return openapi.BatchAssetFieldCounts{
		Expanded:     c.Expanded,
		Eligible:     c.Eligible,
		WouldChange:  c.WouldChange,
		NoOp:         c.NoOp,
		Refused:      c.Refused,
		Inapplicable: c.Inapplicable,
		Unreadable:   c.Unreadable,
		Unauthorized: c.Unauthorized,
	}
}

// reconciles asserts the partition arithmetic the contract states:
//
//	expanded = would_change + no_op + refused + inapplicable
//	           + unreadable + unauthorized
//	eligible = would_change + no_op
//
// Every target of a successful preview belongs to EXACTLY ONE
// partition, so this is a real invariant rather than a description, and
// a server that fails it has mis-partitioned something.
func (c batchCounts) reconciles() bool {
	return c.Expanded == c.WouldChange+c.NoOp+c.Refused+c.Inapplicable+c.Unreadable+c.Unauthorized &&
		c.Eligible == c.WouldChange+c.NoOp
}

// ---------------------------------------------------------------------------
// Minting and reading
// ---------------------------------------------------------------------------

// newBatchToken generates the bearer secret and its stored hash.
//
// The DATABASE NEVER HOLDS THE SECRET. It holds sha256 of it, and the
// lookup hashes the caller's bytes and compares — the same shape a
// session token takes, for the same reason: a table that has never seen
// the secret cannot leak it, in a backup, in a log, or through a read
// somebody was entitled to make for another purpose.
func newBatchToken() (secret string, hash []byte, err error) {
	raw := make([]byte, batchTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, fmt.Errorf("metadata: mint preview token: %w", err)
	}
	sum := sha256.Sum256(raw)
	return base64.RawURLEncoding.EncodeToString(raw), sum[:], nil
}

// batchTokenHash decodes a caller-supplied token and returns its
// lookup hash.
//
// THE INTEGRITY CHECK, and it is deliberately the only one. With a
// server-side binding there is nothing to verify a signature over: a
// token either decodes to the right number of random bytes and names a
// row, or it does not. So "malformed", "unknown" and "tampered" are ONE
// state by construction rather than by a rule somebody has to remember
// to apply — which is exactly the property the enumeration-oracle
// collapse needs.
//
// An error here means the bytes could not possibly name a token. It is
// answered identically to a token that names nobody's row and to one
// that names somebody else's.
func batchTokenHash(token string) ([]byte, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != batchTokenBytes {
		return nil, false
	}
	sum := sha256.Sum256(raw)
	return sum[:], true
}

// batchTokenBoundTo reports whether a stored preview belongs to this
// caller.
//
// Constant-time, and not because a user ref is a secret: because this
// comparison is the ONE thing standing between a caller and an
// enumeration oracle over everybody else's previews, and a security
// comparison that has a fast path is a security comparison somebody can
// measure. The cost is nothing.
func batchTokenBoundTo(row MetadataBatchPreview, callerRef int64) bool {
	var a, b [8]byte
	for i := 0; i < 8; i++ {
		a[i] = byte(row.CallerUserRef >> (8 * i))
		b[i] = byte(callerRef >> (8 * i))
	}
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

// ---------------------------------------------------------------------------
// Configuration fingerprints
// ---------------------------------------------------------------------------

// definitionFingerprint covers every configuration property whose
// change makes a preview's verdicts wrong.
//
// The list is not "everything on the row". It is the properties a
// preview READ in order to reach its answers: the field's status (an
// archived field takes no values), read_only, required, regexp_filter,
// type, mirrors_column, applies_to, and the two capability gates. A
// change to the field's LABEL invalidates nothing and does not appear.
//
// Encoded as JSON with a fixed member order rather than concatenated,
// so a value containing the separator cannot forge a match with a
// different configuration — the classic length-extension shape of a
// hand-rolled fingerprint.
func definitionFingerprint(f FieldDefinition) string {
	payload := struct {
		Status     string  `json:"status"`
		Type       string  `json:"type"`
		ReadOnly   bool    `json:"read_only"`
		Required   bool    `json:"required"`
		Regexp     *string `json:"regexp_filter"`
		Mirrors    *string `json:"mirrors_column"`
		AppliesTo  []int64 `json:"applies_to"`
		ReadCap    *string `json:"read_capability"`
		WriteCap   *string `json:"write_capability"`
		SubjectKnd string  `json:"subject_kind"`
	}{
		Status: f.Status, Type: f.Type, ReadOnly: f.ReadOnly, Required: f.Required,
		Regexp: f.RegexpFilter, Mirrors: f.MirrorsColumn, AppliesTo: f.AppliesTo,
		ReadCap: f.ReadCapability, WriteCap: f.WriteCapability, SubjectKnd: f.SubjectKind,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		// A struct of strings, bools and an int slice cannot fail to
		// marshal. Returning a value that can never match anything is
		// the fail-closed answer if it somehow does.
		return "unfingerprintable"
	}
	sum := sha256.Sum256(raw)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// vocabularyFingerprint covers the options document, SEPARATELY, so a
// curation change reports vocabulary_drift rather than the
// definition_drift a combined hash would give.
//
// Over the raw stored bytes. A semantic comparison would have to decide
// whether reordering two terms counts, and the honest answer for a
// preview that bound a canonical slug set is that any change to the
// document may have moved something the preview resolved.
func vocabularyFingerprint(options []byte) string {
	sum := sha256.Sum256(options)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// Conversions
// ---------------------------------------------------------------------------

func tokenValueOf(v batchValue) batchTokenValue {
	out := batchTokenValue{Text: v.Text, Num: v.Num, Options: v.Options}
	if v.Date.Valid {
		t := v.Date.Time
		out.Date = &t
	}
	if v.Ref.Valid {
		s := uuid.UUID(v.Ref.Bytes).String()
		out.Ref = &s
	}
	return out
}

func (t batchTokenValue) batchValue() batchValue {
	out := batchValue{Text: t.Text, Num: t.Num, Options: t.Options}
	if t.Date != nil {
		out.Date = pgtype.Timestamptz{Time: *t.Date, Valid: true}
	}
	if t.Ref != nil {
		if id, err := uuid.Parse(*t.Ref); err == nil {
			out.Ref = pgtype.UUID{Bytes: id, Valid: true}
		}
	}
	return out
}

func decodeBatchPayload(raw []byte) (batchTokenPayload, error) {
	var p batchTokenPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, fmt.Errorf("metadata: decode preview payload: %w", err)
	}
	return p, nil
}

// storeBatchPreview writes the binding and returns the bearer secret.
// Nothing else in the system can produce one.
func (h *Handler) storeBatchPreview(
	ctx context.Context,
	q *Queries,
	callerRef int64,
	fieldID pgtype.UUID,
	mode batchMode,
	payload batchTokenPayload,
	now time.Time,
) (token string, row InsertBatchPreviewRow, err error) {
	secret, hash, err := newBatchToken()
	if err != nil {
		return "", row, err
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", row, fmt.Errorf("metadata: encode preview payload: %w", err)
	}
	row, err = q.InsertBatchPreview(ctx, InsertBatchPreviewParams{
		TokenHash:     hash,
		CallerUserRef: callerRef,
		FieldID:       fieldID,
		Mode:          string(mode),
		WouldChange:   int32(payload.Counts.WouldChange),
		Payload:       encoded,
		ExpiresAt:     pgtype.Timestamptz{Time: now.Add(batchPreviewTTL), Valid: true},
	})
	if err != nil {
		return "", row, fmt.Errorf("metadata: store preview: %w", err)
	}
	return secret, row, nil
}
