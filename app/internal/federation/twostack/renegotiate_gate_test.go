// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Gate (#1574, spec v1 §16.2, §11.2): a connected pairing negotiates its
// capabilities again, on both sides, without re-pairing, through B's
// production POST /admin/federation/peers/{id}/renegotiate.
//
// TEST-ONLY: the instances pair while this process advertises everything
// but share-lifecycle-1, which is what a pairing made before #1571
// holds, and the full production set is restored before anything else
// (both builds "upgraded"). Everything else is production. Shares made
// under the old set stay (A's and B's), and so does every row that names
// the peer.
//
//   - Anonymous (401), a member and an Auditor (403) send nothing.
//   - B's admin renegotiates: both sides store the full intersection on
//     the same row (id, key, status, enablement), every dependent row is
//     kept and none is added, and one audit row records it. A share
//     made now reaches B (the new intersection is in effect); the one made
//     under the old set is not applied retroactively (§16.6).
//   - Repeating it, and renegotiating from A, converge.
//   - A disabled row is refused (409) and nothing is sent.
//   - A down, or A unable to confirm back to B: 502 and nothing changes on
//     either side.
//   - A's final answer lost after its confirm reached B: 502, both sides
//     already hold the new set (said in the error), and renegotiating
//     again converges.

package twostack_test

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/peer"
)

// peerRow is what a renegotiation must keep (all but the last two) and
// what it may change (the stored intersection and its time).
type peerRow struct {
	id, key, status string
	enabled         bool
	created         time.Time
	caps            []string
	negotiatedAt    time.Time
}

func readPeerRow(t *testing.T, in *instance, otherURL string) peerRow {
	t.Helper()
	var r peerRow
	if err := in.pool.QueryRow(context.Background(),
		`SELECT id::text, instance_public_key, status, enabled, created_at,
		        ARRAY(SELECT jsonb_array_elements_text(capabilities)), capabilities_negotiated_at
		   FROM federation_peers WHERE instance_url = $1`, otherURL).
		Scan(&r.id, &r.key, &r.status, &r.enabled, &r.created, &r.caps, &r.negotiatedAt); err != nil {
		t.Fatalf("%s's row for %s: %v", in.name, otherURL, err)
	}
	return r
}

// dependents counts, in every table with a foreign key to
// federation_peers, the rows that name peerID, plus the peer rows for
// the URL.
func dependents(t *testing.T, in *instance, peerID, otherURL string) map[string]int {
	t.Helper()
	ctx := context.Background()
	rows, err := in.pool.Query(ctx, `SELECT c.conrelid::regclass::text, a.attname
		  FROM pg_constraint c JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = ANY (c.conkey)
		 WHERE c.confrelid = 'federation_peers'::regclass AND c.contype = 'f' ORDER BY 1, 2`)
	if err != nil {
		t.Fatal(err)
	}
	var cols [][2]string
	for rows.Next() {
		var tbl, col string
		if err := rows.Scan(&tbl, &col); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, [2]string{tbl, col})
	}
	rows.Close()
	out := map[string]int{}
	for _, tc := range cols {
		var n int
		if err := in.pool.QueryRow(ctx, `SELECT count(*) FROM `+tc[0]+` WHERE `+tc[1]+` = $1`, peerID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		out[tc[0]+"."+tc[1]] = n
	}
	var n int
	_ = in.pool.QueryRow(ctx, `SELECT count(*) FROM federation_peers WHERE instance_url = $1`, otherURL).Scan(&n)
	out["federation_peers(url)"] = n
	return out
}

type renegotiation struct {
	Previous []string `json:"previous_capabilities"`
	Current  []string `json:"capabilities"`
	Error    *string  `json:"error"`
	Peer     struct {
		ID string `json:"id"`
	} `json:"peer"`
}

func strs(c peer.CapabilitySet) []string {
	out := make([]string, 0, len(c))
	for _, t := range c {
		out = append(out, string(t))
	}
	return out
}

func TestTwoStack_RenegotiatePeer(t *testing.T) {
	prod := append(peer.CapabilitySet(nil), peer.KnownCapabilities...)
	var older peer.CapabilitySet
	for _, c := range prod {
		if c != peer.CapShareLifecycle1 {
			older = append(older, c)
		}
	}
	if len(older) != len(prod)-1 {
		t.Fatal("share-lifecycle-1 is not advertised in production")
	}
	peer.KnownCapabilities = older
	t.Cleanup(func() { peer.KnownCapabilities = prod })
	c := newCluster(t)
	peer.KnownCapabilities = prod
	c.runWorkers(c.a)
	c.runWorkers(c.b)
	g := &casGate{cluster: c, peerAonB: c.peerID(c.b, c.a), peerBonA: c.peerID(c.a, c.b)}
	ctx := context.Background()
	full, old := strs(peer.Intersect(prod, prod)), strs(peer.Intersect(older, older))

	rowB, rowA := readPeerRow(t, c.b, c.a.url), readPeerRow(t, c.a, c.b.url)
	if !slices.Equal(rowB.caps, old) || !slices.Equal(rowA.caps, old) {
		t.Fatalf("fixture: B holds %v, A holds %v, want %v", rowB.caps, rowA.caps, old)
	}

	// Dependent rows made under the old set: a share each way, both
	// skipped by the sender (profile_not_negotiated).
	oldObj := g.newOriginal("public")
	g.grantWith(oldObj, nil)
	shareFromB(g)
	skips := func(in *instance) int {
		var n int
		_ = in.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE event_type = $1
			AND metadata->>'reason' = 'profile_not_negotiated'`, audit.EventFederationEmissionSkipped).Scan(&n)
		return n
	}
	for deadline := time.Now().Add(60 * time.Second); skips(c.a) < 1 || skips(c.b) < 1; time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the old-set shares were never skipped by the senders")
		}
	}
	depB, depA := dependents(t, c.b, rowB.id, c.a.url), dependents(t, c.a, rowA.id, c.b.url)
	if depB["federation_shares.peer_id"] != 1 || depA["federation_shares.peer_id"] != 1 {
		t.Fatalf("fixture shares: B %v, A %v", depB, depA)
	}

	path := "/admin/federation/peers/" + rowB.id + "/renegotiate"
	audits := func(outcome string) int {
		var n int
		_ = c.b.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE event_type = $1
			AND metadata->>'peer_id' = $2 AND metadata->>'outcome' = $3`,
			audit.EventFederationPeerRenegotiated, rowB.id, outcome).Scan(&n)
		return n
	}
	unchanged := func(t *testing.T, what string, b, a peerRow) {
		t.Helper()
		for _, s := range []struct {
			in       *instance
			otherURL string
			before   peerRow
		}{{c.b, c.a.url, b}, {c.a, c.b.url, a}} {
			now := readPeerRow(t, s.in, s.otherURL)
			if now.id != s.before.id || now.key != s.before.key || now.status != s.before.status || now.enabled != s.before.enabled ||
				!slices.Equal(now.caps, s.before.caps) || !now.negotiatedAt.Equal(s.before.negotiatedAt) {
				t.Errorf("%s: %s's row changed: %+v, was %+v", what, s.in.name, now, s.before)
			}
		}
	}
	renegotiate := func(in *instance, p string) (int, renegotiation) {
		t.Helper()
		var out renegotiation
		st := c.api(in, http.MethodPost, p, nil, &out)
		return st, out
	}

	t.Run("refused callers send nothing", func(t *testing.T) {
		member, auditor := g.memberOnB(), auditorOnB(g)
		for _, who := range []struct {
			name, tok string
			want      int
		}{{"anonymous", "", 401}, {"member", member, 403}, {"auditor", auditor, 403}} {
			if st, raw := postOnB(g, who.tok, path); st != who.want {
				t.Errorf("%s: %d %s, want %d", who.name, st, raw, who.want)
			}
		}
		unchanged(t, "refused callers", rowB, rowA)
		if n := audits("renegotiated") + audits("failed") + audits("refused"); n != 0 {
			t.Errorf("refused callers left %d audit rows", n)
		}
	})

	t.Run("the admin renegotiates; rows and data are kept", func(t *testing.T) {
		st, out := renegotiate(c.b, path)
		if st != 200 || !slices.Equal(out.Previous, old) || !slices.Equal(out.Current, full) || out.Error != nil || out.Peer.ID != rowB.id {
			t.Fatalf("renegotiate: %d %+v, want 200 %v -> %v", st, out, old, full)
		}
		for _, s := range []struct {
			in       *instance
			otherURL string
			before   peerRow
			deps     map[string]int
		}{{c.b, c.a.url, rowB, depB}, {c.a, c.b.url, rowA, depA}} {
			now := readPeerRow(t, s.in, s.otherURL)
			if now.id != s.before.id || now.key != s.before.key || now.status != "connected" || !now.enabled || !now.created.Equal(s.before.created) {
				t.Errorf("%s's row is not the same row: %+v, was %+v", s.in.name, now, s.before)
			}
			if !slices.Equal(now.caps, full) || !now.negotiatedAt.After(s.before.negotiatedAt) {
				t.Errorf("%s stored %v at %v", s.in.name, now.caps, now.negotiatedAt)
			}
			if d := dependents(t, s.in, s.before.id, s.otherURL); !maps.Equal(d, s.deps) {
				t.Errorf("%s's dependent rows changed:\n before %v\n after  %v", s.in.name, s.deps, d)
			}
		}
		if n := audits("renegotiated"); n != 1 {
			t.Errorf("%d renegotiated audit rows, want 1", n)
		}
		var prevJSON, curJSON string
		if err := c.b.pool.QueryRow(ctx, `SELECT metadata->>'previous_capabilities', metadata->>'capabilities' FROM audit_events
			WHERE event_type = $1 AND metadata->>'peer_id' = $2 AND metadata->>'outcome' = 'renegotiated'`,
			audit.EventFederationPeerRenegotiated, rowB.id).Scan(&prevJSON, &curJSON); err != nil {
			t.Fatalf("read the renegotiated audit row of %s: %v", rowB.id, err)
		}
		var prev, cur []string
		_ = json.Unmarshal([]byte(prevJSON), &prev)
		_ = json.Unmarshal([]byte(curJSON), &cur)
		if !slices.Equal(prev, old) || !slices.Equal(cur, full) {
			t.Errorf("audit records %v -> %v, want %v -> %v", prev, cur, old, full)
		}
	})

	t.Run("the new intersection is in effect, the old share is not replayed", func(t *testing.T) {
		o := g.newOriginal("public")
		g.grantWith(o, nil)
		g.waitOnB(g.a.url+"/assets/"+o.asset.String(), "live")
		var n int
		_ = c.b.pool.QueryRow(ctx, `SELECT count(*) FROM federation_inbound_grants WHERE object_uri = $1`,
			g.a.url+"/assets/"+oldObj.asset.String()).Scan(&n)
		if n != 0 {
			t.Errorf("the share made under the old set was applied retroactively (%d rows)", n)
		}
	})

	t.Run("repeated and reverse renegotiation converge", func(t *testing.T) {
		if st, out := renegotiate(c.b, path); st != 200 || !slices.Equal(out.Previous, full) || !slices.Equal(out.Current, full) {
			t.Fatalf("repeat from B: %d %+v", st, out)
		}
		if st, out := renegotiate(c.a, "/admin/federation/peers/"+rowA.id+"/renegotiate"); st != 200 || !slices.Equal(out.Current, full) {
			t.Fatalf("from A: %d %+v", st, out)
		}
		for _, s := range []struct {
			in       *instance
			otherURL string
		}{{c.b, c.a.url}, {c.a, c.b.url}} {
			if d := dependents(t, s.in, readPeerRow(t, s.in, s.otherURL).id, s.otherURL); d["federation_peers(url)"] != 1 {
				t.Errorf("%s holds %d rows for its peer", s.in.name, d["federation_peers(url)"])
			}
		}
	})

	t.Run("a disabled row is refused and nothing is sent", func(t *testing.T) {
		if st := c.api(c.b, http.MethodPatch, "/admin/federation/peers/"+rowB.id, map[string]any{"enabled": false}, nil); st != 200 {
			t.Fatalf("disable: %d", st)
		}
		b, a := readPeerRow(t, c.b, c.a.url), readPeerRow(t, c.a, c.b.url)
		if st, _ := renegotiate(c.b, path); st != 409 {
			t.Errorf("disabled: %d, want 409", st)
		}
		unchanged(t, "disabled", b, a)
		if n := audits("refused"); n != 1 {
			t.Errorf("%d refused audit rows, want 1", n)
		}
		if st := c.api(c.b, http.MethodPatch, "/admin/federation/peers/"+rowB.id, map[string]any{"enabled": true}, nil); st != 200 {
			t.Fatalf("enable: %d", st)
		}
	})

	// onHandshake wraps in's served handler for handshake POSTs only.
	onHandshake := func(in *instance, f func(w http.ResponseWriter, r *http.Request, h http.Handler)) func() {
		served := in.handler.Load()
		wrapped := http.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && r.URL.Path == peer.HandshakePath {
				f(w, r, *served)
				return
			}
			(*served).ServeHTTP(w, r)
		}))
		in.handler.Store(&wrapped)
		return func() { in.handler.Store(served) }
	}
	failed := func(t *testing.T, what string, wantChanged bool) {
		t.Helper()
		st, out := renegotiate(c.b, path)
		if st != 502 || out.Error == nil || *out.Error == "" {
			t.Fatalf("%s: %d %+v, want 502 with an error", what, st, out)
		}
		changed := !slices.Equal(out.Previous, out.Current)
		if changed != wantChanged {
			t.Errorf("%s: previous %v current %v, want changed=%v", what, out.Previous, out.Current, wantChanged)
		}
		if wantChanged && !strings.Contains(*out.Error, "renegotiate again") {
			t.Errorf("%s: the error does not say both sides may differ: %s", what, *out.Error)
		}
	}

	t.Run("A down: nothing changes on either side", func(t *testing.T) {
		b, a := readPeerRow(t, c.b, c.a.url), readPeerRow(t, c.a, c.b.url)
		restore := onHandshake(c.a, func(w http.ResponseWriter, _ *http.Request, _ http.Handler) {
			http.Error(w, "gate: down", http.StatusServiceUnavailable)
		})
		before := audits("failed")
		failed(t, "A down", false)
		restore()
		unchanged(t, "A down", b, a)
		if audits("failed") != before+1 {
			t.Error("the failure was not audited")
		}
	})

	t.Run("A cannot confirm back to B: nothing changes on either side", func(t *testing.T) {
		b, a := readPeerRow(t, c.b, c.a.url), readPeerRow(t, c.a, c.b.url)
		restore := onHandshake(c.b, func(w http.ResponseWriter, _ *http.Request, _ http.Handler) {
			http.Error(w, "gate: B refuses inbound handshakes", http.StatusServiceUnavailable)
		})
		failed(t, "confirm back refused", false)
		restore()
		unchanged(t, "confirm back refused", b, a)
	})

	t.Run("A's final answer lost: both already hold the new set; renegotiating again converges", func(t *testing.T) {
		// Both builds now drop share-lifecycle-1 again (TEST-ONLY), so the
		// renegotiation has something to change.
		peer.KnownCapabilities = older
		restore := onHandshake(c.a, func(w http.ResponseWriter, r *http.Request, h http.Handler) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			http.Error(w, "gate: answer lost", http.StatusBadGateway)
		})
		failed(t, "answer lost", true)
		restore()
		for _, s := range []struct {
			in       *instance
			otherURL string
		}{{c.b, c.a.url}, {c.a, c.b.url}} {
			if now := readPeerRow(t, s.in, s.otherURL); !slices.Equal(now.caps, old) {
				t.Errorf("%s holds %v, want %v", s.in.name, now.caps, old)
			}
		}
		peer.KnownCapabilities = prod
		if st, out := renegotiate(c.b, path); st != 200 || !slices.Equal(out.Previous, old) || !slices.Equal(out.Current, full) {
			t.Fatalf("converge: %d %+v", st, out)
		}
		if a := readPeerRow(t, c.a, c.b.url); !slices.Equal(a.caps, full) {
			t.Errorf("A holds %v after converging", a.caps)
		}
	})

	// Invariant across the whole gate: one row each way, same identity.
	for _, s := range []struct {
		in       *instance
		otherURL string
		id       string
	}{{c.b, c.a.url, rowB.id}, {c.a, c.b.url, rowA.id}} {
		if now := readPeerRow(t, s.in, s.otherURL); now.id != s.id || now.status != string(federation.PeerStatusConnected) {
			t.Errorf("%s's row for its peer is %s/%s, want %s connected", s.in.name, now.id, now.status, s.id)
		}
	}
}
