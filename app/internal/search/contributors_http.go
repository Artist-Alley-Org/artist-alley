// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package search

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/search/facet"
	"github.com/mscrnt/artist-alley/app/internal/users"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// ContributorsHandler is the http.Handler for GET /search/contributors
// (#1173, sprint 18d) — the picker behind the advanced page's
// Contributor control.
//
// Mounted as a raw chi route beside /search, /search/facets and
// /search/suggest, which is ADR 0056 §10's decision for the whole search
// family rather than a choice made here.
//
// # What it discloses, and why that is nothing new
//
// It answers "whose work is in the set I am already looking at". The
// population is [facet.Contributors]' shared one — the same visibility
// gate, capabilities, mature axis and active selection the search itself
// runs — so every contributor it names owns at least one row the caller
// could reach by paging their own results. The projection is `user_ref`
// plus the resolved display label and NOTHING ELSE: no email, no roles,
// no account state, no counts. This is not a user directory, and
// widening it into one would be a new disclosure surface rather than a
// bigger version of this one.
//
// # ⛔ AUTHENTICATED, AND THAT IS WHAT KEEPS ADR 0070 §3 OUT OF THE SQL
//
// §3's anonymous rule skips the `fullname` rung, and ADR 0024's opt-out
// withholds an opted-out user from an anonymous caller entirely. Both
// branches are UNREACHABLE here because the handler refuses an anonymous
// caller outright — so `anonymous` is a constant `false` on the one call
// to [users.ResolveDisplayName] below, matching `fullname` in SQL is
// consistent with §3 rather than a leak (the caller would be shown that
// name anyway), and the opt-out cannot fire. The 401 is the guarantee;
// it is asserted rather than assumed.
type ContributorsHandler struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger
}

// Contributor is one row of the response.
type Contributor struct {
	UserRef int64 `json:"user_ref"`
	// DisplayName is [users.ResolveDisplayName] and nothing else,
	// INCLUDING its rung-4 `user <ref>` fallback for a contributor with
	// no stored name. That fallback is the reason the prefix is optional
	// — see [facet.ContributorQuery.Prefix].
	DisplayName string `json:"display_name"`
}

type contributorsResponse struct {
	Contributors []Contributor `json:"contributors"`
	// NextCursor is "" on the LAST page. A client distinguishes
	// "terminal" from "more available" by this field alone, and the
	// server computes it by over-fetching rather than by comparing the
	// page to the limit — see [facet.ContributorPage.Next].
	NextCursor string `json:"next_cursor"`
}

// ServeHTTP implements http.Handler.
//
// Wire shape — the SAME query parameters /search takes, so a client
// sends one set to both and the suggestions describe the query being
// built rather than the corpus:
//
//	GET /search/contributors?prefix=ak&q=cat&dsl=…&filter=type:1&cursor=…&limit=25
//
// Response:
//
//	{ "contributors": [ {"user_ref": 7, "display_name": "Akira Tanaka"} ],
//	  "next_cursor": "eyJ…" }
//
// `prefix` is OPTIONAL and an empty one browses the whole qualifying
// population; see [facet.ContributorQuery.Prefix] for why that is a
// correctness property rather than a convenience.
func (h *ContributorsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	id := auth.IdentityFromContext(r.Context())
	if id == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		return
	}

	qs := r.URL.Query()

	selection, err := facet.ParseSelection(qs["filter"])
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_filter"})
		return
	}

	queryText := qs.Get("q")

	// The DSL arm folds through the SAME compiler and the SAME
	// [SelectionFromDSL] bridge /search uses — see [foldDSL], which is
	// the one place either suggestion endpoint speaks it.
	if in := qs.Get("dsl"); in != "" {
		sel, text, derr := foldDSL(in, selection, queryText)
		if derr != nil {
			writeDSLError(w, derr)
			return
		}
		selection, queryText = sel, text
	}

	limit := 0
	if s := qs.Get("limit"); s != "" {
		n, cerr := strconv.Atoi(s)
		if cerr != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_limit"})
			return
		}
		limit = n
	}

	after, cerr := decodeContributorCursor(qs.Get("cursor"))
	if cerr != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_cursor"})
		return
	}

	ref := id.UserRef
	req := facet.Request{
		QueryText: queryText,
		Selection: selection,
		Caller:    visibility.NewCaller(&ref),
		Caps:      visibility.ResolveContentCaps(func(code string) bool { return id.Can(code) }),
		PostCaps:  visibility.ResolvePostCaps(func(code string) bool { return id.Can(code) }),
		MutationCaps: visibility.ResolveAssetMutationCaps(
			func(code string) bool { return id.Can(code) },
			id.ScopedTeams(visibility.AssetsAdmin),
		),
		Mature: visibility.MatureFromContext(r.Context()),
	}

	// The same whole-query gate [facet.Dispatcher.Run] applies before it
	// runs an aggregator, for the same reason: a selection naming a
	// collection the caller may not open would otherwise answer "who
	// contributes to it" without ever listing a row. Empty, not an error
	// — the response must not distinguish "not yours" from "nobody".
	if ok, aerr := selection.Authorize(
		r.Context(), h.Pool, req.Caller, req.Caps.Checker(),
	); aerr != nil || !ok {
		if aerr != nil && h.Logger != nil {
			h.Logger.LogAttrs(r.Context(), slog.LevelWarn,
				"search.contributors.authorize_error", slog.String("err", aerr.Error()))
		}
		writeJSON(w, http.StatusOK, contributorsResponse{Contributors: []Contributor{}})
		return
	}

	page, err := facet.Contributors(r.Context(), h.Pool, facet.ContributorQuery{
		Request: req,
		Prefix:  qs.Get("prefix"),
		After:   after,
		Limit:   limit,
	})
	if err != nil {
		if errors.Is(err, facet.ErrContributorLimit) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_limit"})
			return
		}
		if h.Logger != nil {
			h.Logger.LogAttrs(r.Context(), slog.LevelWarn,
				"search.contributors.error", slog.String("err", err.Error()))
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
		return
	}

	out := contributorsResponse{Contributors: make([]Contributor, 0, len(page.Rows))}
	for _, row := range page.Rows {
		out.Contributors = append(out.Contributors, Contributor{
			UserRef: row.Ref,
			// ⛔ THE ONE EXPRESSION, ADR 0070 §3. `anonymous` is false
			// because the 401 above makes the anonymous branch
			// unreachable; it is a literal rather than a variable so
			// nothing here can be mistaken for a second policy.
			DisplayName: users.ResolveDisplayName(
				row.ProfileDisplayName, row.Fullname, row.Username, row.Ref, false),
		})
	}
	out.NextCursor = encodeContributorCursor(page.Next)
	writeJSON(w, http.StatusOK, out)
}

// encodeContributorCursor renders the keyset as the opaque token the
// response carries. "" for nil, which is the TERMINAL signal.
func encodeContributorCursor(c *facet.ContributorCursor) string {
	if c == nil {
		return ""
	}
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeContributorCursor is the inverse. Empty input is the first page.
// Malformed input is an ERROR rather than a silent first page: quietly
// restarting would re-serve rows the caller has already seen and look
// like a duplicate rather than a bad request.
func decodeContributorCursor(s string) (*facet.ContributorCursor, error) {
	if s == "" {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		raw2, err2 := base64.StdEncoding.DecodeString(s)
		if err2 != nil {
			return nil, ErrBadCursor
		}
		raw = raw2
	}
	var c facet.ContributorCursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, ErrBadCursor
	}
	return &c, nil
}
