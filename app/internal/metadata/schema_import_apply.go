// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package metadata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// ---------------------------------------------------------------------------
// POST /fields/import/apply (#1576 S2, ADR 0083 2026-11-22 amendments)
// ---------------------------------------------------------------------------
//
// Lock order, shared with every other condition writer (ADR 0099 §8):
//
//  1. pg_advisory_xact_lock(1173, 1) then (1173, 2): both subject-kind
//     display-condition graph locks, asset first. Every writer that can
//     change a condition edge or add a code takes them first: PATCH that
//     touches display_condition (one kind), ordinary CreateField and this
//     apply (both kinds, in this order).
//  2. the preview row, FOR UPDATE (apply only). The cleanup touches
//     preview rows with SKIP LOCKED and takes no graph lock.
//  3. the definition rows the validation reads, FOR UPDATE, in one
//     ascending code order. Writers that take only a row lock (PATCH
//     without a condition, archive, extraction, vocabulary) hold one row
//     and request nothing else.
//
// No writer requests a graph lock after a row lock, and the writers that
// take both graph locks take them in the same order, so the order adds
// no cycle.

// FieldSchemaImportApplyMaxBodyBytes is the preview's cap: up to 100
// codes of any length the document allowed must fit in the decision map.
const FieldSchemaImportApplyMaxBodyBytes = FieldSchemaImportPreviewMaxBodyBytes

var (
	canonicalUUID  = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	documentDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// applyHook lets tests pause or fail an apply at a named stage. nil in
// production. A non-nil error fails the apply at that point.
var applyHook func(stage string) error

// SetImportApplyHookForTest installs the apply hook. Tests only.
func SetImportApplyHookForTest(f func(stage string) error) { applyHook = f }

func applyStage(stage string) error {
	if applyHook == nil {
		return nil
	}
	return applyHook(stage)
}

type applyRequest struct {
	previewID uuid.UUID
	digest    string
	decisions map[string]string
}

// decodeApplyRequest checks the whole apply envelope strictly. Every
// problem here is a 400: the request, not a document, is wrong.
func decodeApplyRequest(body []byte) (applyRequest, *openapi.FieldSchemaImportRefusal) {
	var req applyRequest
	bad := func(problems []ImportProblem) (applyRequest, *openapi.FieldSchemaImportRefusal) {
		r := importRefusal("request_invalid", "the request is not valid", problems)
		return req, &r
	}
	root, err := parseStrictJSON(body)
	if err != nil {
		var pe *strictParseError
		path, msg := "$", err.Error()
		if errors.As(err, &pe) {
			path, msg = pe.path, pe.msg
		}
		return bad([]ImportProblem{{path, msg}})
	}
	if root.kind != jsonObject {
		return bad([]ImportProblem{{"$", "must be an object"}})
	}
	o := newObjectReader(root, "$")
	pid, dig, dec := o.required("preview_id"), o.required("document_digest"), o.required("decisions")
	o.noOthers()
	if pid != nil {
		if pid.kind != jsonString || !canonicalUUID.MatchString(pid.str) {
			o.add("$.preview_id", "must be a lower-case UUID")
		} else {
			req.previewID = uuid.MustParse(pid.str)
		}
	}
	if dig != nil {
		if dig.kind != jsonString || !documentDigest.MatchString(dig.str) {
			o.add("$.document_digest", "must be sha256: and 64 lower-case hex digits")
		} else {
			req.digest = dig.str
		}
	}
	if dec != nil {
		if dec.kind != jsonObject {
			o.add("$.decisions", "must be an object")
		} else {
			req.decisions = make(map[string]string, len(dec.members))
			for _, m := range dec.members {
				if m.val.kind != jsonString {
					o.add("$.decisions."+m.key, "must be a string")
					continue
				}
				req.decisions[m.key] = m.val.str
			}
		}
	}
	if len(o.problems) > 0 {
		return bad(o.problems)
	}
	return req, nil
}

// decisionsDigest is the SHA-256 of the decision map with sorted keys.
func decisionsDigest(d map[string]string) string {
	b, _ := json.Marshal(d) // encoding/json sorts map keys
	return sha256Hex(b)
}

// ServeImportApply is the raw handler for POST /fields/import/apply.
func (h *Handler) ServeImportApply(w http.ResponseWriter, r *http.Request) {
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
			Error: fmt.Sprintf("request body exceeds %d bytes", FieldSchemaImportApplyMaxBodyBytes),
		})
	}
	if r.ContentLength > FieldSchemaImportApplyMaxBodyBytes {
		tooLarge()
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, FieldSchemaImportApplyMaxBodyBytes+1))
	if err != nil {
		writeImportRefusal(w, http.StatusBadRequest, "request_invalid", "could not read the request body", nil)
		return
	}
	if len(body) > FieldSchemaImportApplyMaxBodyBytes {
		tooLarge()
		return
	}
	req, refusal := decodeApplyRequest(body)
	if refusal != nil {
		writeImportJSON(w, http.StatusBadRequest, *refusal)
		return
	}
	status, resp, created, err := h.applyImport(ctx, id.UserRef, req)
	if err != nil {
		if h.Logger != nil {
			h.Logger.LogAttrs(ctx, slog.LevelError, "metadata.import_apply.error", slog.String("err", err.Error()))
		}
		writeImportJSON(w, http.StatusInternalServerError, openapi.Error{Error: "internal error"})
		return
	}
	// Side effects only after a successful commit. Cache invalidation is
	// for the definitions actually created; the audit event is for every
	// newly committed receipt, changed 0 included, and never a replay.
	if status == http.StatusOK {
		for _, fid := range created {
			h.invalidateField(ctx, fid)
		}
		if rc, ok := resp.(openapi.FieldSchemaImportReceipt); ok && !rc.Replayed && h.Audit != nil {
			actor := id.UserRef
			h.Audit.FieldSchemaImported(ctx, &actor, map[string]any{
				"receipt_id":      rc.ReceiptId.String(),
				"preview_id":      rc.PreviewId.String(),
				"document_digest": rc.DocumentDigest,
				"changed":         rc.Changed,
				"fields":          len(rc.Fields),
			})
		}
	}
	writeImportJSON(w, status, resp)
}

func importApplyRefusal(status int, reason, msg string, problems []ImportProblem) (int, any, []pgtype.UUID, error) {
	return status, importRefusal(reason, msg, problems), nil, nil
}

// replayReceipt answers an apply for a preview that already has a
// receipt. Another actor's receipt is not disclosed (404, as unknown).
func replayReceipt(ctx context.Context, q *Queries, rec FieldSchemaImportReceipt, actor int64, req applyRequest) (int, any, []pgtype.UUID, error) {
	if rec.ActorRef == nil || *rec.ActorRef != actor {
		return importApplyRefusal(http.StatusNotFound, "preview_not_found", "no such preview", nil)
	}
	if rec.DocumentDigest != req.digest {
		return importApplyRefusal(http.StatusConflict, "digest_mismatch", "document_digest is not the digest of the applied document", nil)
	}
	if rec.DecisionsDigest != decisionsDigest(req.decisions) {
		return importApplyRefusal(http.StatusConflict, "preview_consumed", "this preview was already applied with other decisions", nil)
	}
	out, err := receiptResponse(ctx, q, rec.ID, rec.PreviewID, rec.AppliedAt, rec.DocumentDigest, rec.DecisionsDigest, int(rec.Changed), true)
	return http.StatusOK, out, nil, err
}

func receiptResponse(ctx context.Context, q *Queries, receiptID, previewID pgtype.UUID, appliedAt pgtype.Timestamptz,
	docDigest, decDigest string, changed int, replayed bool) (openapi.FieldSchemaImportReceipt, error) {
	items, err := q.ListFieldSchemaImportReceiptItems(ctx, receiptID)
	if err != nil {
		return openapi.FieldSchemaImportReceipt{}, fmt.Errorf("metadata: import receipt items: %w", err)
	}
	out := openapi.FieldSchemaImportReceipt{
		ReceiptId:       openapi_types.UUID(uuid.UUID(receiptID.Bytes)),
		PreviewId:       openapi_types.UUID(uuid.UUID(previewID.Bytes)),
		AppliedAt:       appliedAt.Time.UTC(),
		DocumentDigest:  docDigest,
		DecisionsDigest: decDigest,
		Replayed:        replayed,
		Changed:         changed,
		Fields:          make([]openapi.FieldSchemaImportReceiptItem, 0, len(items)),
	}
	for _, it := range items {
		item := openapi.FieldSchemaImportReceiptItem{Code: it.Code, Decision: it.Decision, Outcome: it.Outcome}
		if it.FieldID.Valid {
			fid := openapi_types.UUID(uuid.UUID(it.FieldID.Bytes))
			item.FieldId = &fid
		}
		out.Fields = append(out.Fields, item)
	}
	return out, nil
}

// lockConditionGraphs takes both subject-kind graph locks, asset first.
func lockConditionGraphs(ctx context.Context, q *Queries) error {
	for _, kind := range []string{string(SubjectAsset), string(SubjectCollection)} {
		if err := q.LockFieldDisplayConditionGraph(ctx, LockFieldDisplayConditionGraphParams{
			LockSpace:  displayConditionLockSpace,
			SubjectKey: displayConditionSubjectKey(kind),
		}); err != nil {
			return fmt.Errorf("metadata: display condition lock: %w", err)
		}
	}
	return nil
}

// strictGraph is every definition's code and stored condition, read
// under both graph locks. Unlike the forgiving PATCH loader, a stored
// condition that does not decode is reported, not treated as empty.
type strictGraph struct {
	nodes      map[string]conditionGraphNode
	unreadable map[string]bool
}

func loadStrictGraph(ctx context.Context, q *Queries) (strictGraph, error) {
	rows, err := q.ListFieldDefinitionsForConditionGraph(ctx)
	if err != nil {
		return strictGraph{}, fmt.Errorf("metadata: condition graph: %w", err)
	}
	g := strictGraph{nodes: make(map[string]conditionGraphNode, len(rows)), unreadable: map[string]bool{}}
	for _, r := range rows {
		cond, err := DecodeDisplayCondition(r.DisplayCondition)
		if err != nil {
			g.unreadable[r.Code] = true
		}
		g.nodes[r.Code] = conditionGraphNode{
			Code: r.Code, Type: r.Type, Status: r.Status, subject: r.SubjectKind,
			AppliesTo: r.AppliesTo, MirrorsColumn: r.MirrorsColumn, Condition: cond,
		}
	}
	return g, nil
}

// termCodes is the set of codes a condition's parseable terms name.
func termCodes(terms []string) []string {
	var out []string
	for _, raw := range terms {
		if t, ok := ParseDisplayConditionTerm(raw); ok {
			out = append(out, t.Code)
		}
	}
	return out
}

// resolutionPlan is what a set of new codes means for the stored graph:
// the stored dependents whose terms name one of them, and every row the
// validation will read.
type resolutionPlan struct {
	dependents []string // existing codes whose condition names a new code
	readSet    []string // sorted: rows to lock FOR UPDATE
	unreadable []string // existing codes whose stored condition does not decode
}

// planResolution computes, under both graph locks, the rows an apply or
// an ordinary create must lock and validate. newNodes are the definitions
// being created, with their final attributes. extra are further existing
// codes the caller binds (incoming codes and their direct references).
func planResolution(g strictGraph, newNodes map[string]conditionGraphNode, extra []string) resolutionPlan {
	set := map[string]bool{}
	var plan resolutionPlan
	add := func(c string) {
		if _, ok := g.nodes[c]; ok {
			set[c] = true
		}
	}
	for _, c := range extra {
		add(c)
	}
	for code := range g.unreadable {
		plan.unreadable = append(plan.unreadable, code)
	}
	sort.Strings(plan.unreadable)
	for code, n := range g.nodes {
		for _, c := range termCodes(n.Condition) {
			if _, isNew := newNodes[c]; isNew {
				plan.dependents = append(plan.dependents, code)
				add(code)
				for _, cc := range termCodes(n.Condition) {
					add(cc)
				}
				break
			}
		}
	}
	for _, n := range newNodes {
		for _, c := range termCodes(n.Condition) {
			add(c)
		}
	}
	sort.Strings(plan.dependents)
	for c := range set {
		plan.readSet = append(plan.readSet, c)
	}
	sort.Strings(plan.readSet)
	return plan
}

// validateFinalGraph checks every created dependent and every stored
// dependent a creation resolves, against the COMPLETE final overlay:
// current edges, locked attributes and every new node. Missing terms
// stay verbatim and are skipped by the mode; every resolved term passes
// the ordinary rules, and the existing cycle walk runs from each checked
// dependent over the complete graph.
func validateFinalGraph(final map[string]conditionGraphNode, check []string) []ImportProblem {
	var problems []ImportProblem
	sort.Strings(check)
	for _, code := range check {
		dep := final[code]
		if len(dep.Condition) == 0 {
			continue
		}
		if msg := validateDisplayConditionConfigMode(dep, dep.Condition, final, true); msg != "" {
			problems = appendProblem(problems, code, msg)
		}
	}
	return problems
}

// lockedRows converts locked rows into the preview's row type, so the
// SAME projection, precedence and digest code decides both.
func lockedRows(rows []LockFieldDefinitionsForImportRow) map[string]ListFieldDefinitionsForImportPreviewRow {
	out := make(map[string]ListFieldDefinitionsForImportPreviewRow, len(rows))
	for _, r := range rows {
		out[r.FieldDefinition.Code] = ListFieldDefinitionsForImportPreviewRow{FieldDefinition: r.FieldDefinition, RowDigest: r.RowDigest}
	}
	return out
}

func nodeOf(d FieldDefinition) conditionGraphNode {
	cond, _ := DecodeDisplayCondition(d.DisplayCondition)
	return conditionGraphNode{Code: d.Code, Type: d.Type, Status: d.Status, subject: d.SubjectKind,
		AppliesTo: d.AppliesTo, MirrorsColumn: d.MirrorsColumn, Condition: cond}
}

// portableToLocalOptions converts portable terms into a local options
// document: the same slugs, labels, order and nesting, all active, with
// no aliases or replacement pointers.
func portableToLocalOptions(opts []openapi.FieldSchemaExportOption) []FieldOption {
	out := make([]FieldOption, 0, len(opts))
	for _, o := range opts {
		fo := FieldOption{Value: o.Value, Children: portableToLocalOptions(o.Children)}
		if o.Label != nil {
			fo.Label = *o.Label
		}
		if len(fo.Children) == 0 {
			fo.Children = nil
		}
		out = append(out, fo)
	}
	return out
}

// classifyStorageError turns a database refusal of a client value into
// a typed answer. Anything else stays an internal error.
func classifyStorageError(err error, code string) (int, any, bool) {
	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		return 0, nil, false
	}
	switch {
	case pg.Code == "54000", strings.HasPrefix(pg.Code, "22"), pg.Code == "23514":
		return http.StatusUnprocessableEntity, importRefusal("unstorable_value",
			"an incoming value cannot be stored by this server; nothing was changed",
			[]ImportProblem{{code, pg.Message}}), true
	case pg.Code == "23505":
		return http.StatusConflict, importRefusal("preview_stale",
			"a definition with this code appeared since the preview; preview again",
			[]ImportProblem{{code, "already exists"}}), true
	}
	return 0, nil, false
}

// applyImport runs the apply. created lists the definitions written, for
// post-commit cache invalidation; it is empty on any refusal.
func (h *Handler) applyImport(ctx context.Context, actor int64, req applyRequest) (int, any, []pgtype.UUID, error) {
	pid := pgtype.UUID{Bytes: req.previewID, Valid: true}

	// 0. Replay first, before any expiry or staleness check.
	if rec, err := New(h.Pool).GetFieldSchemaImportReceiptByPreview(ctx, pid); err == nil {
		return replayReceipt(ctx, New(h.Pool), rec, actor, req)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return 0, nil, nil, fmt.Errorf("metadata: import receipt read: %w", err)
	}

	tx, err := h.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return 0, nil, nil, fmt.Errorf("metadata: import apply begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := New(tx)

	// 1. Both graph locks, before any graph or definition read.
	if err := lockConditionGraphs(ctx, q); err != nil {
		return 0, nil, nil, err
	}
	if err := applyStage("locked"); err != nil {
		return 0, nil, nil, err
	}
	// A concurrent apply of the same preview may have committed while we
	// waited: answer as a replay.
	if rec, err := q.GetFieldSchemaImportReceiptByPreview(ctx, pid); err == nil {
		return replayReceipt(ctx, q, rec, actor, req)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return 0, nil, nil, fmt.Errorf("metadata: import receipt reread: %w", err)
	}

	// 2. The preview row.
	pv, err := q.LockFieldSchemaImportPreviewForApply(ctx, pid)
	if errors.Is(err, pgx.ErrNoRows) {
		return importApplyRefusal(http.StatusNotFound, "preview_not_found", "no such preview", nil)
	}
	if err != nil {
		return 0, nil, nil, fmt.Errorf("metadata: import preview lock: %w", err)
	}
	if pv.ActorRef != actor {
		return importApplyRefusal(http.StatusNotFound, "preview_not_found", "no such preview", nil)
	}
	// The deadline is judged by the database clock in its own statement,
	// here and again immediately before commit (below).
	dl, err := q.FieldSchemaImportPreviewDeadline(ctx, pv.ExpiresAt)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("metadata: import preview deadline: %w", err)
	}
	if dl.BeyondRetention {
		// More than 24 hours past expiry: answered as unknown, whether or
		// not the cleanup has deleted the row yet.
		return importApplyRefusal(http.StatusNotFound, "preview_not_found", "no such preview", nil)
	}
	if pv.State != "live" || dl.Expired {
		return importApplyRefusal(http.StatusGone, "preview_expired", "the preview expired; preview again", nil)
	}
	if pv.DocumentDigest != req.digest {
		return importApplyRefusal(http.StatusConflict, "digest_mismatch", "document_digest is not the preview's", nil)
	}
	if sha256Hex(pv.Document) != pv.DocumentDigest {
		return 0, nil, nil, errors.New("metadata: stored preview document does not match its digest")
	}
	node, err := parseStrictJSON(pv.Document)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("metadata: stored preview document: %w", err)
	}
	doc, problems, err := decodeImportDocument(node)
	if err != nil || len(problems) > 0 {
		return 0, nil, nil, fmt.Errorf("metadata: stored preview document invalid: %v %v", err, problems)
	}
	var bound importBoundState
	if err := json.Unmarshal(pv.BoundState, &bound); err != nil {
		return 0, nil, nil, fmt.Errorf("metadata: stored preview bound state: %w", err)
	}

	// The decision key set must be exactly the incoming codes.
	incoming := make(map[string]openapi.FieldSchemaExportField, len(doc.Fields))
	var decisionProblems []ImportProblem
	for _, f := range doc.Fields {
		incoming[f.Code] = f
		if _, ok := req.decisions[f.Code]; !ok {
			decisionProblems = appendProblem(decisionProblems, "$.decisions."+f.Code, "is required: every incoming code needs a decision")
		}
	}
	extraKeys := make([]string, 0)
	for k := range req.decisions {
		if _, ok := incoming[k]; !ok {
			extraKeys = append(extraKeys, k)
		}
	}
	sort.Strings(extraKeys)
	for _, k := range extraKeys {
		decisionProblems = appendProblem(decisionProblems, "$.decisions."+k, "is not a code in the preview")
	}

	// 3. Graph edges (stable under the locks), then the rows to lock.
	g, err := loadStrictGraph(ctx, q)
	if err != nil {
		return 0, nil, nil, err
	}
	newNodes := map[string]conditionGraphNode{}
	for code, d := range req.decisions {
		if f, ok := incoming[code]; ok && d == importDecisionCreate {
			var cond []string
			if f.DisplayCondition != nil {
				cond = *f.DisplayCondition
			}
			newNodes[code] = conditionGraphNode{Code: code, Type: f.Type, Status: "active",
				subject: string(f.SubjectKind), AppliesTo: []int64{}, Condition: cond}
		}
	}
	boundCodes := make([]string, 0, len(bound.Present)+len(bound.Absent))
	for c := range bound.Present {
		boundCodes = append(boundCodes, c)
	}
	boundCodes = append(boundCodes, bound.Absent...)
	plan := planResolution(g, newNodes, boundCodes)
	rows, err := q.LockFieldDefinitionsForImport(ctx, plan.readSet)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("metadata: import row lock: %w", err)
	}
	if err := applyStage("rows_locked"); err != nil {
		return 0, nil, nil, err
	}
	local := lockedRows(rows)

	// 4. Freshness: every bound row unchanged, every absent code absent.
	var stale []ImportProblem
	for _, c := range boundCodes {
		want, present := bound.Present[c]
		row, now := local[c]
		_, inGraph := g.nodes[c]
		switch {
		case present && (!now || row.RowDigest != want):
			stale = appendProblem(stale, c, "changed or removed since the preview")
		case !present && (now || inGraph):
			stale = appendProblem(stale, c, "appeared since the preview")
		}
	}
	if len(stale) > 0 {
		sort.Slice(stale, func(i, j int) bool { return stale[i].Path < stale[j].Path })
		return importApplyRefusal(http.StatusConflict, "preview_stale", "local definitions changed since the preview; preview again", stale)
	}

	// Decisions against the allowed set recomputed from the locked rows.
	for _, f := range doc.Fields {
		d, ok := req.decisions[f.Code]
		if !ok {
			continue
		}
		allowed := previewField(f, local).Decisions
		if !containsString(allowed, d) {
			decisionProblems = appendProblem(decisionProblems, "$.decisions."+f.Code,
				fmt.Sprintf("%q is not allowed here; allowed: %s", d, strings.Join(allowed, ", ")))
		}
	}
	if len(decisionProblems) > 0 {
		return importApplyRefusal(http.StatusUnprocessableEntity, "decision_invalid", "the decisions do not match the preview", decisionProblems)
	}

	// 5. The complete final overlay, then validation, before any write.
	if len(newNodes) > 0 {
		var corrupt []ImportProblem
		for _, c := range plan.unreadable {
			if n, ok := g.nodes[c]; ok {
				for _, nn := range newNodes {
					if n.SameSubjectKindAs(nn) {
						corrupt = appendProblem(corrupt, c, "stored display condition does not decode; it cannot be checked against the new fields")
						break
					}
				}
			}
		}
		if len(corrupt) > 0 {
			return importApplyRefusal(http.StatusUnprocessableEntity, "graph_refused", "the final configuration cannot be validated", corrupt)
		}
		final := make(map[string]conditionGraphNode, len(g.nodes)+len(newNodes))
		for c, n := range g.nodes {
			final[c] = n
		}
		for c, r := range local {
			n := nodeOf(r.FieldDefinition)
			n.Condition = g.nodes[c].Condition
			final[c] = n
		}
		for c, n := range newNodes {
			final[c] = n
		}
		check := append([]string{}, plan.dependents...)
		for c := range newNodes {
			check = append(check, c)
		}
		if gp := validateFinalGraph(final, check); len(gp) > 0 {
			return importApplyRefusal(http.StatusUnprocessableEntity, "graph_refused", "the final configuration breaks a display-condition rule", gp)
		}
	}

	// 6. Writes, in code order.
	codes := make([]string, 0, len(doc.Fields))
	for _, f := range doc.Fields {
		codes = append(codes, f.Code)
	}
	sort.Strings(codes)
	type item struct {
		code, decision, outcome string
		fieldID                 pgtype.UUID
		digest                  string
	}
	items := make([]item, 0, len(codes))
	var created []pgtype.UUID
	for _, code := range codes {
		f := incoming[code]
		d := req.decisions[code]
		fb, _ := json.Marshal(f)
		it := item{code: code, decision: d, digest: sha256Hex(fb)}
		if r, ok := local[code]; ok {
			it.fieldID = r.FieldDefinition.ID
		}
		switch d {
		case importDecisionCreate:
			opts := []byte(`{}`)
			if f.Options != nil {
				b, err := json.Marshal(map[string]any{"values": portableToLocalOptions(*f.Options)})
				if err != nil {
					return 0, nil, nil, err
				}
				opts = b
			}
			var cond []byte
			if f.DisplayCondition != nil {
				cond, _ = json.Marshal(*f.DisplayCondition)
			}
			actorRef := actor
			fid, err := q.InsertImportedFieldDefinition(ctx, InsertImportedFieldDefinitionParams{
				Code: f.Code, Label: f.Label, Description: f.Description, Type: f.Type, Options: opts,
				Required: f.Required, Searchable: f.Searchable, DisplayOrder: int32(f.DisplayOrder),
				DisplayGroup: f.DisplayGroup, SubjectKind: string(f.SubjectKind), OpenVocabulary: f.OpenVocabulary,
				ShowOnCard: f.ShowOnCard, ShowInAdvancedSearch: f.ShowInAdvancedSearch, ShowOnUpload: f.ShowOnUpload,
				EditTab: f.EditTab, RegexpFilter: f.RegexpFilter, DisplayCondition: cond, CreatedByUserRef: &actorRef,
			})
			if err != nil {
				if st, body, ok := classifyStorageError(err, code); ok {
					return st, body, nil, nil
				}
				return 0, nil, nil, fmt.Errorf("metadata: import insert %s: %w", code, err)
			}
			it.fieldID, it.outcome = fid, "created"
			created = append(created, fid)
			if err := applyStage("inserted"); err != nil {
				return 0, nil, nil, err
			}
		case importDecisionUseExisting:
			it.outcome = "matched"
		case importDecisionKeepLocal:
			it.outcome = "kept_local"
		default:
			it.outcome = "skipped"
		}
		items = append(items, it)
	}

	// 7. Receipt, items, and the preview row, in the same transaction.
	decJSON, _ := json.Marshal(req.decisions)
	ins, err := q.InsertFieldSchemaImportReceipt(ctx, InsertFieldSchemaImportReceiptParams{
		PreviewID: pid, ActorRef: &actor, Format: string(doc.Format), DocumentDigest: pv.DocumentDigest,
		DecisionsDigest: decisionsDigest(req.decisions), Decisions: decJSON,
		SourceLabel: pv.SourceLabel, SourceUri: pv.SourceUri, Changed: int32(len(created)),
	})
	if err != nil {
		if st, body, ok := classifyStorageError(err, "$.receipt"); ok {
			return st, body, nil, nil
		}
		return 0, nil, nil, fmt.Errorf("metadata: import receipt insert: %w", err)
	}
	for i, it := range items {
		var fid pgtype.UUID
		if it.fieldID.Valid {
			fid = it.fieldID
		}
		if err := q.InsertFieldSchemaImportReceiptItem(ctx, InsertFieldSchemaImportReceiptItemParams{
			ReceiptID: ins.ID, Ordinal: int16(i), Code: it.code, Decision: it.decision, Outcome: it.outcome,
			FieldID: fid, IncomingFieldDigest: it.digest,
		}); err != nil {
			if st, body, ok := classifyStorageError(err, it.code); ok {
				return st, body, nil, nil
			}
			return 0, nil, nil, fmt.Errorf("metadata: import receipt item: %w", err)
		}
	}
	if err := q.DeleteFieldSchemaImportPreview(ctx, pid); err != nil {
		return 0, nil, nil, fmt.Errorf("metadata: import preview delete: %w", err)
	}
	if err := applyStage("before_commit"); err != nil {
		return 0, nil, nil, err
	}
	out, err := receiptResponse(ctx, q, ins.ID, pid, ins.AppliedAt, pv.DocumentDigest, decisionsDigest(req.decisions), len(created), false)
	if err != nil {
		return 0, nil, nil, err
	}
	// The final deadline check, after every wait and immediately before
	// commit: a preview that expired while this apply waited on a lock
	// is refused, and everything written above is rolled back.
	dl, err = q.FieldSchemaImportPreviewDeadline(ctx, pv.ExpiresAt)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("metadata: import preview deadline: %w", err)
	}
	if dl.Expired {
		return importApplyRefusal(http.StatusGone, "preview_expired", "the preview expired while the apply waited; preview again", nil)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, nil, nil, fmt.Errorf("metadata: import apply commit: %w", err)
	}
	return http.StatusOK, out, created, nil
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Ordinary creation under the same serialization (#1576 S2)
// ---------------------------------------------------------------------------

// createFieldHook lets tests pause an ordinary create at a named stage.
var createFieldHook func(stage string)

// SetCreateFieldHookForTest installs the create hook. Tests only.
func SetCreateFieldHookForTest(f func(stage string)) { createFieldHook = f }

func createStage(stage string) {
	if createFieldHook != nil {
		createFieldHook(stage)
	}
}

// createFieldSerialized inserts an ordinary field inside one transaction
// that first takes both graph locks, then validates every stored
// dependent the new code resolves (imported conditions that named it
// while it did not exist), with the new field's real attributes, before
// inserting. conflict is non-nil when the create is refused.
func (h *Handler) createFieldSerialized(ctx context.Context, p CreateFieldDefinitionParams) (FieldDefinition, *openapi.DisplayConditionConflict, error) {
	createStage("before_lock")
	tx, err := h.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return FieldDefinition{}, nil, fmt.Errorf("metadata: create begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := New(tx)
	if err := lockConditionGraphs(ctx, q); err != nil {
		return FieldDefinition{}, nil, err
	}
	g, err := loadStrictGraph(ctx, q)
	if err != nil {
		return FieldDefinition{}, nil, err
	}
	newNode := conditionGraphNode{Code: p.Code, Type: p.Type, Status: p.Status, subject: p.SubjectKind,
		AppliesTo: p.AppliesTo}
	plan := planResolution(g, map[string]conditionGraphNode{p.Code: newNode}, nil)
	createStage("after_check")
	if len(plan.dependents) > 0 {
		conflict := &openapi.DisplayConditionConflict{Reason: "display_condition_conflict",
			Error: "stored display conditions naming this code would break a rule; nothing was created"}
		for _, c := range plan.dependents {
			if g.unreadable[c] {
				conflict.Dependents = append(conflict.Dependents, struct {
					Field   string `json:"field"`
					Problem string `json:"problem"`
				}{c, "stored display condition does not decode"})
			}
		}
		rows, err := q.LockFieldDefinitionsForImport(ctx, plan.readSet)
		if err != nil {
			return FieldDefinition{}, nil, fmt.Errorf("metadata: create row lock: %w", err)
		}
		createStage("rows_locked")
		final := make(map[string]conditionGraphNode, len(g.nodes)+1)
		for c, n := range g.nodes {
			final[c] = n
		}
		for _, r := range rows {
			n := nodeOf(r.FieldDefinition)
			n.Condition = g.nodes[r.FieldDefinition.Code].Condition
			final[r.FieldDefinition.Code] = n
		}
		final[p.Code] = newNode
		for _, pr := range validateFinalGraph(final, plan.dependents) {
			conflict.Dependents = append(conflict.Dependents, struct {
				Field   string `json:"field"`
				Problem string `json:"problem"`
			}{pr.Path, pr.Problem})
		}
		if len(conflict.Dependents) > 0 {
			return FieldDefinition{}, conflict, nil
		}
	}
	row, err := q.CreateFieldDefinition(ctx, p)
	if err != nil {
		return FieldDefinition{}, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return FieldDefinition{}, nil, fmt.Errorf("metadata: create commit: %w", err)
	}
	return row, nil, nil
}
