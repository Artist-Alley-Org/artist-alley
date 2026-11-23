// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package metadata

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// ---------------------------------------------------------------------------
// POST /fields/import/preview (#1576, ADR 0083 2026-11-22 amendments)
// ---------------------------------------------------------------------------
//
// Served by a raw handler registered in front of the generated one, so
// that authorization is decided before a single body byte is read and
// the whole request can be checked strictly (schema_import_decode.go).
// The OpenAPI operation stays fully described.
//
// A preview writes one preview row and nothing else. It never changes a
// definition, option, value, default, permission or role.

const (
	// FieldSchemaImportPreviewMaxBodyBytes is the largest export plus 64
	// KiB for the envelope.
	FieldSchemaImportPreviewMaxBodyBytes = FieldSchemaMaxDocumentBytes + 64<<10
	// FieldSchemaImportPreviewTTL is how long a preview stays usable.
	FieldSchemaImportPreviewTTL = 15 * time.Minute
	// FieldSchemaImportPreviewsPerActor and FieldSchemaImportPreviewsTotal
	// cap the previews holding a payload. An expired preview still holds
	// one until the cleanup erases it, and still counts.
	FieldSchemaImportPreviewsPerActor = 3
	FieldSchemaImportPreviewsTotal    = 20
)

// Local states, checked in this order.
const (
	importLocalRetired    = "retired"
	importLocalMirrored   = "mirrored"
	importLocalAbsent     = "absent"
	importLocalUnreadable = "unreadable"
	importLocalIdentical  = "identical"
	importLocalDifferent  = "different"
)

// Decisions a later import step accepts.
const (
	importDecisionCreate      = "create"
	importDecisionUseExisting = "use_existing"
	importDecisionKeepLocal   = "keep_local"
	importDecisionSkip        = "skip"
)

const importPreviewAdvisory = "This preview is a review aid. The import step re-validates the final configuration under its own locks, and can refuse it if the condition graph changed after this preview."

// importBoundState is what a preview recorded about the local rows it
// read, for a later import to detect any change: a digest of every
// present row, and every code that had no row.
type importBoundState struct {
	Present map[string]string `json:"present"`
	Absent  []string          `json:"absent"`
}

// previewHook lets tests pause a preview after it holds the admission
// lock. nil in production.
var previewHook func(stage string)

// ServeImportPreview is the raw handler for POST /fields/import/preview.
func (h *Handler) ServeImportPreview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		writeImportJSON(w, http.StatusUnauthorized, openapi.Error{Error: "authentication required"})
		return
	}
	if !canAdminFields(id) {
		writeImportJSON(w, http.StatusForbidden, openapi.Error{Error: "field admin capability required"})
		return
	}
	tooLarge := func() {
		writeImportJSON(w, http.StatusRequestEntityTooLarge, openapi.Error{
			Error: fmt.Sprintf("request body exceeds %d bytes", FieldSchemaImportPreviewMaxBodyBytes),
		})
	}
	if r.ContentLength > FieldSchemaImportPreviewMaxBodyBytes {
		tooLarge()
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, FieldSchemaImportPreviewMaxBodyBytes+1))
	if err != nil {
		writeImportRefusal(w, http.StatusBadRequest, "request_invalid", "could not read the request body", nil)
		return
	}
	if len(body) > FieldSchemaImportPreviewMaxBodyBytes {
		tooLarge()
		return
	}

	status, resp, err := h.previewImport(ctx, id.UserRef, body)
	if err != nil {
		if h.Logger != nil {
			h.Logger.LogAttrs(ctx, slog.LevelError, "metadata.import_preview.error", slog.String("err", err.Error()))
		}
		writeImportJSON(w, http.StatusInternalServerError, openapi.Error{Error: "internal error"})
		return
	}
	writeImportJSON(w, status, resp)
}

// decodedPreview is a preview request that passed every check.
type decodedPreview struct {
	doc       openapi.FieldSchemaExport
	source    ImportSource
	canonical []byte
	digest    string
}

// decodePreviewRequest runs every request check. On refusal it returns
// the status and body; otherwise status is 0.
func decodePreviewRequest(body []byte) (decodedPreview, int, *openapi.FieldSchemaImportRefusal, error) {
	var d decodedPreview
	refuse := func(status int, reason, msg string, problems []ImportProblem) (decodedPreview, int, *openapi.FieldSchemaImportRefusal, error) {
		r := importRefusal(reason, msg, problems)
		return d, status, &r, nil
	}
	root, err := parseStrictJSON(body)
	if err != nil {
		var pe *strictParseError
		if errors.As(err, &pe) && !pe.syntax && pe.inDocument {
			return refuse(http.StatusUnprocessableEntity, "document_invalid",
				"the document breaks the aa-field-schema/1 contract", []ImportProblem{{pe.path, pe.msg}})
		}
		path, msg := "$", err.Error()
		if errors.As(err, &pe) {
			path, msg = pe.path, pe.msg
		}
		return refuse(http.StatusBadRequest, "request_invalid", "the request is not valid", []ImportProblem{{path, msg}})
	}
	docNode, source, problems := decodePreviewEnvelope(root)
	if len(problems) > 0 {
		return refuse(http.StatusBadRequest, "request_invalid", "the request envelope is not valid", problems)
	}
	doc, problems, err := decodeImportDocument(docNode)
	if errors.Is(err, errUnsupportedFormat) {
		return refuse(http.StatusUnprocessableEntity, "unsupported_format",
			"this server imports aa-field-schema/1 documents only", []ImportProblem{{"$.document.format", "is not aa-field-schema/1"}})
	}
	if len(problems) > 0 {
		return refuse(http.StatusUnprocessableEntity, "document_invalid", "the document breaks the aa-field-schema/1 contract", problems)
	}
	canonical, digest, err := canonicalImportDocument(doc)
	if err != nil {
		return d, 0, nil, fmt.Errorf("metadata: import canonical encode: %w", err)
	}
	if len(canonical) > FieldSchemaMaxDocumentBytes {
		return refuse(http.StatusUnprocessableEntity, "document_too_large",
			fmt.Sprintf("the document re-encodes to %d bytes, over the %d byte limit", len(canonical), FieldSchemaMaxDocumentBytes),
			[]ImportProblem{{"$.document", "too large once re-encoded"}})
	}
	return decodedPreview{doc: doc, source: source, canonical: canonical, digest: digest}, 0, nil, nil
}

// previewImport checks the request and records the preview. It returns
// the HTTP status and the body to send.
func (h *Handler) previewImport(ctx context.Context, actor int64, body []byte) (int, any, error) {
	req, status, refusal, err := decodePreviewRequest(body)
	if err != nil {
		return 0, nil, err
	}
	if refusal != nil {
		return status, *refusal, nil
	}
	doc, source, canonical, digest := req.doc, req.source, req.canonical, req.digest

	incoming := map[string]bool{}
	for _, f := range doc.Fields {
		incoming[f.Code] = true
	}
	codes := previewReadSet(doc)

	// READ COMMITTED: every statement after the admission lock reads a
	// snapshot taken after the lock was granted.
	tx, err := h.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return 0, nil, fmt.Errorf("metadata: import preview begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := New(tx)
	if err := q.LockFieldSchemaImportAdmission(ctx); err != nil {
		return 0, nil, fmt.Errorf("metadata: import preview lock: %w", err)
	}
	if previewHook != nil {
		previewHook("locked")
	}
	counts, err := q.CountFieldSchemaImportPayloads(ctx, actor)
	if err != nil {
		return 0, nil, fmt.Errorf("metadata: import preview count: %w", err)
	}
	if counts.ActorCount >= FieldSchemaImportPreviewsPerActor || counts.TotalCount >= FieldSchemaImportPreviewsTotal {
		return http.StatusTooManyRequests, importRefusal("preview_limit",
			fmt.Sprintf("at most %d previews per operator and %d in total can be held; an expired preview is released by the periodic cleanup",
				FieldSchemaImportPreviewsPerActor, FieldSchemaImportPreviewsTotal), nil), nil
	}
	rows, err := q.ListFieldDefinitionsForImportPreview(ctx, codes)
	if err != nil {
		return 0, nil, fmt.Errorf("metadata: import preview read: %w", err)
	}

	local := make(map[string]ListFieldDefinitionsForImportPreviewRow, len(rows))
	bound := importBoundState{Present: map[string]string{}, Absent: []string{}}
	for _, r := range rows {
		local[r.FieldDefinition.Code] = r
		bound.Present[r.FieldDefinition.Code] = r.RowDigest
	}
	for _, c := range codes {
		if _, ok := local[c]; !ok {
			bound.Absent = append(bound.Absent, c)
		}
	}

	out := openapi.FieldSchemaImportPreview{
		DocumentDigest:  digest,
		Fields:          make([]openapi.FieldSchemaImportPreviewField, 0, len(doc.Fields)),
		UnresolvedTerms: []openapi.FieldSchemaImportUnresolvedTerm{},
		Advisory:        importPreviewAdvisory,
		Source:          openapi.FieldSchemaImportSource{Label: source.Label, Uri: source.URI},
	}
	allowed := map[string][]string{}
	for _, f := range doc.Fields {
		pf := previewField(f, local)
		allowed[f.Code] = pf.Decisions
		out.Fields = append(out.Fields, pf)
		if f.DisplayCondition != nil {
			for _, term := range *f.DisplayCondition {
				t, _ := ParseDisplayConditionTerm(term)
				if _, here := local[t.Code]; !here && !incoming[t.Code] {
					out.UnresolvedTerms = append(out.UnresolvedTerms,
						openapi.FieldSchemaImportUnresolvedTerm{Field: f.Code, Term: term})
				}
			}
		}
	}

	allowedJSON, err := json.Marshal(allowed)
	if err != nil {
		return 0, nil, err
	}
	boundJSON, err := json.Marshal(bound)
	if err != nil {
		return 0, nil, err
	}
	ins, err := q.InsertFieldSchemaImportPreview(ctx, InsertFieldSchemaImportPreviewParams{
		ActorRef:       actor,
		TtlSeconds:     FieldSchemaImportPreviewTTL.Seconds(),
		Document:       canonical,
		DocumentDigest: digest,
		SourceLabel:    source.Label,
		SourceUri:      source.URI,
		Allowed:        allowedJSON,
		BoundState:     boundJSON,
	})
	if err != nil {
		return 0, nil, fmt.Errorf("metadata: import preview insert: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, nil, fmt.Errorf("metadata: import preview commit: %w", err)
	}
	out.PreviewId = openapi_types.UUID(uuid.UUID(ins.ID.Bytes))
	out.CreatedAt = ins.CreatedAt.Time.UTC()
	out.ExpiresAt = ins.ExpiresAt.Time.UTC()
	return http.StatusOK, out, nil
}

// previewReadSet is every incoming code and every code an incoming
// condition names, sorted and unique. It is bounded by the document,
// not by a separate count.
func previewReadSet(doc openapi.FieldSchemaExport) []string {
	set := map[string]bool{}
	for _, f := range doc.Fields {
		set[f.Code] = true
		if f.DisplayCondition != nil {
			for _, term := range *f.DisplayCondition {
				if t, ok := ParseDisplayConditionTerm(term); ok {
					set[t.Code] = true
				}
			}
		}
	}
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// previewField decides one incoming field's local state and allowed
// decisions, in the documented precedence.
func previewField(in openapi.FieldSchemaExportField, local map[string]ListFieldDefinitionsForImportPreviewRow) openapi.FieldSchemaImportPreviewField {
	pf := openapi.FieldSchemaImportPreviewField{
		Code:        in.Code,
		Incoming:    in,
		Differences: []openapi.FieldSchemaImportDifference{},
	}
	r, ok := local[in.Code]
	if !ok {
		pf.Local = importLocalAbsent
		pf.Decisions = []string{importDecisionCreate, importDecisionSkip}
		return pf
	}
	def := r.FieldDefinition
	lid := openapi_types.UUID(uuid.UUID(def.ID.Bytes))
	status := def.Status
	pf.LocalId, pf.LocalStatus = &lid, &status
	switch {
	case def.Status != "active":
		pf.Local = importLocalRetired
		pf.Decisions = []string{importDecisionSkip}
		return pf
	case def.MirrorsColumn != nil && *def.MirrorsColumn != "":
		pf.Local = importLocalMirrored
		pf.Decisions = []string{importDecisionKeepLocal, importDecisionSkip}
		return pf
	}
	proj, err := portableField(def)
	if err != nil {
		msg := err.Error()
		pf.Local = importLocalUnreadable
		pf.Problem = &msg
		pf.Decisions = []string{importDecisionKeepLocal, importDecisionSkip}
		return pf
	}
	diffs, err := portableDifferences(proj, in)
	if err != nil {
		msg := "the local definition could not be compared: " + err.Error()
		pf.Local = importLocalUnreadable
		pf.Problem = &msg
		pf.Decisions = []string{importDecisionKeepLocal, importDecisionSkip}
		return pf
	}
	if len(diffs) == 0 {
		pf.Local = importLocalIdentical
		pf.Decisions = []string{importDecisionUseExisting, importDecisionSkip}
		return pf
	}
	pf.Local = importLocalDifferent
	pf.Differences = diffs
	pf.Decisions = []string{importDecisionKeepLocal, importDecisionSkip}
	return pf
}

// portableDifferences compares two portable fields member by member,
// through their JSON encodings, so equality means exactly what the
// export would write. Members are reported sorted by name.
func portableDifferences(local, incoming openapi.FieldSchemaExportField) ([]openapi.FieldSchemaImportDifference, error) {
	lm, err := membersOf(local)
	if err != nil {
		return nil, err
	}
	im, err := membersOf(incoming)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(im))
	for k := range im {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := []openapi.FieldSchemaImportDifference{}
	for _, k := range keys {
		if bytes.Equal(lm[k], im[k]) {
			continue
		}
		d := openapi.FieldSchemaImportDifference{Property: k, Local: lm[k], Incoming: im[k]}
		if k == "options" {
			onlyIn, onlyLocal, changed := optionTermDiff(local.Options, incoming.Options)
			d.TermsOnlyIncoming, d.TermsOnlyLocal, d.TermsChanged = &onlyIn, &onlyLocal, &changed
		}
		out = append(out, d)
	}
	return out, nil
}

func membersOf(f openapi.FieldSchemaExportField) (map[string]json.RawMessage, error) {
	b, err := json.Marshal(f)
	if err != nil {
		return nil, err
	}
	m := map[string]json.RawMessage{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

type flatTerm struct {
	parent string
	label  string
	index  int
}

func flattenTerms(opts *[]openapi.FieldSchemaExportOption) map[string]flatTerm {
	out := map[string]flatTerm{}
	if opts == nil {
		return out
	}
	var walk func(list []openapi.FieldSchemaExportOption, parent string)
	walk = func(list []openapi.FieldSchemaExportOption, parent string) {
		for i, o := range list {
			label := ""
			if o.Label != nil {
				label = *o.Label
			}
			out[o.Value] = flatTerm{parent: parent, label: label, index: i}
			walk(o.Children, o.Value)
		}
	}
	walk(*opts, "")
	return out
}

func optionTermDiff(local, incoming *[]openapi.FieldSchemaExportOption) (onlyIn, onlyLocal, changed []string) {
	lt, it := flattenTerms(local), flattenTerms(incoming)
	onlyIn, onlyLocal, changed = []string{}, []string{}, []string{}
	for v, t := range it {
		l, ok := lt[v]
		switch {
		case !ok:
			onlyIn = append(onlyIn, v)
		case l != t:
			changed = append(changed, v)
		}
	}
	for v := range lt {
		if _, ok := it[v]; !ok {
			onlyLocal = append(onlyLocal, v)
		}
	}
	sort.Strings(onlyIn)
	sort.Strings(onlyLocal)
	sort.Strings(changed)
	return onlyIn, onlyLocal, changed
}

func importRefusal(reason, msg string, problems []ImportProblem) openapi.FieldSchemaImportRefusal {
	out := openapi.FieldSchemaImportRefusal{Reason: reason, Error: msg, Problems: []openapi.FieldSchemaImportProblem{}}
	for i, p := range problems {
		if i == importMaxProblems {
			break
		}
		out.Problems = append(out.Problems, openapi.FieldSchemaImportProblem{Path: p.Path, Problem: p.Problem})
	}
	return out
}

func writeImportRefusal(w http.ResponseWriter, status int, reason, msg string, problems []ImportProblem) {
	writeImportJSON(w, status, importRefusal(reason, msg, problems))
}

func writeImportJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// Cleanup
// ---------------------------------------------------------------------------

// SchemaImportPreviewSweeper erases expired previews' payloads, keeping a
// compact tombstone for 24 hours, and deletes tombstones after that. Each
// batch is its own statement, takes no lock but the rows it updates, and
// skips rows another transaction holds.
type SchemaImportPreviewSweeper struct {
	h        *Handler
	interval time.Duration
	batch    int32
}

// NewSchemaImportPreviewSweeper builds the sweeper. Interval and batch
// default to five minutes and 500.
func NewSchemaImportPreviewSweeper(h *Handler, interval time.Duration, batch int32) *SchemaImportPreviewSweeper {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	if batch <= 0 {
		batch = 500
	}
	return &SchemaImportPreviewSweeper{h: h, interval: interval, batch: batch}
}

// Run sweeps at start and then on every tick, until ctx ends.
func (s *SchemaImportPreviewSweeper) Run(ctx context.Context) {
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		if _, _, err := s.SweepOnce(ctx); err != nil && ctx.Err() == nil && s.h.Logger != nil {
			s.h.Logger.LogAttrs(ctx, slog.LevelWarn, "metadata.import_preview.sweep_error", slog.String("err", err.Error()))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// SweepOnce compacts and deletes in batches until a batch comes back
// short. It returns the rows compacted and the tombstones deleted.
func (s *SchemaImportPreviewSweeper) SweepOnce(ctx context.Context) (compacted, deleted int64, err error) {
	q := New(s.h.Pool)
	for {
		n, err := q.CompactExpiredFieldSchemaImportPreviews(ctx, s.batch)
		if err != nil {
			return compacted, deleted, fmt.Errorf("metadata: import preview compact: %w", err)
		}
		compacted += n
		if n < int64(s.batch) {
			break
		}
	}
	for {
		n, err := q.DeleteExpiredFieldSchemaImportTombstones(ctx, s.batch)
		if err != nil {
			return compacted, deleted, fmt.Errorf("metadata: import preview tombstone delete: %w", err)
		}
		deleted += n
		if n < int64(s.batch) {
			break
		}
	}
	return compacted, deleted, nil
}

// SetImportPreviewHookForTest installs a hook a test uses to pause a
// preview at a named stage ("locked": the admission lock is held).
// Tests only; pass nil to remove it.
func SetImportPreviewHookForTest(f func(stage string)) { previewHook = f }
