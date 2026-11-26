// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1597: the receiver's transport for one access copy of one sealed
// version, through the same state machine as an original (client.go).
// Pinned here, against a live database and the origin stub:
//
//   - the exact version-bound route, canonical size and a valid signature
//     on every HEAD and GET, for the instance and an actor signer; the
//     original route is unchanged;
//   - download and verified reuse, each with its own pin, and the
//     fixity, length and redirect checks kept;
//   - no access copy is received on the strength of an original grant,
//     of bytes already held, or of another version's answer;
//   - all four derived-transfer-1 profiles are needed, at every check;
//   - a downgrade or an access revoke at the reuse and import boundaries
//     records nothing;
//   - requests for different versions or classes of the same bytes share
//     one transfer but never one authority, fixity check or pin.
//
// Skips without AA_DB_PASSWORD.

package casfetch_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/cas"
	"github.com/mscrnt/artist-alley/app/internal/federation/casfetch"
	"github.com/mscrnt/artist-alley/app/internal/federation/outbox"
)

// grantCall is one question the client asked its inbound record.
type grantCall struct {
	Tx    bool // AllowsTx (inside a recording transaction) rather than Allows
	Class string
}

// classGrants is a scripted InboundGrants that answers per class and
// records every question with the class it carried.
type classGrants struct {
	mu    sync.Mutex
	allow map[string]bool
	calls []grantCall
}

func newClassGrants(classes ...string) *classGrants {
	g := &classGrants{allow: map[string]bool{}}
	for _, c := range classes {
		g.allow[c] = true
	}
	return g
}

func (g *classGrants) decide(tx bool, q casfetch.GrantQuery) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, grantCall{Tx: tx, Class: q.Class})
	if !g.allow[q.Class] {
		return casfetch.ErrNoInboundGrant
	}
	return nil
}

func (g *classGrants) Allows(_ context.Context, q casfetch.GrantQuery) error {
	return g.decide(false, q)
}

func (g *classGrants) AllowsTx(_ context.Context, _ pgx.Tx, q casfetch.GrantQuery) error {
	return g.decide(true, q)
}

func (g *classGrants) set(class string, v bool) {
	g.mu.Lock()
	g.allow[class] = v
	g.mu.Unlock()
}

// count is the number of questions asked (inside a transaction or not)
// for class.
func (g *classGrants) count(tx bool, class string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, c := range g.calls {
		if c.Tx == tx && c.Class == class {
			n++
		}
	}
	return n
}

func (g *classGrants) total() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.calls)
}

// setCaps replaces the receiver's stored capability intersection for the
// peer.
func (f *fx) setCaps(tokens []string) {
	f.t.Helper()
	b, err := json.Marshal(tokens)
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE federation_peers SET capabilities = $2::jsonb WHERE id = $1`, f.peer.ID, string(b)); err != nil {
		f.t.Fatal(err)
	}
}

func without(tokens []string, drop string) []string {
	return slices.DeleteFunc(slices.Clone(tokens), func(s string) bool { return s == drop })
}

// newAccessFx is newFx with all four derived-transfer-1 profiles stored
// for the peer and a per-class inbound record allowing classes.
func newAccessFx(t *testing.T, classes ...string) (*fx, *classGrants) {
	t.Helper()
	f := newFx(t)
	f.setCaps(federation.DerivedTransferProfiles)
	g := newClassGrants(classes...)
	f.client = f.newClient(g)
	return f, g
}

func (f *fx) accessReq(version int64) casfetch.Request {
	r := f.req()
	r.Class = cas.ClassAccess
	r.Version = version
	return r
}

// accessPath is the version-bound route spelled out from the spec
// (§8.5.1), not from cas.AccessFilePath, so the helper is checked too.
func (f *fx) accessPath(version int64) string {
	return "/federation/assets/" + f.asset.String() + "/versions/" + strconv.FormatInt(version, 10) +
		"/files/access/" + f.hash
}

func (f *fx) originalPath() string {
	return "/federation/assets/" + f.asset.String() + "/files/original/" + f.hash
}

func (f *fx) sizeQuery() string { return "size=" + strconv.Itoa(len(f.body)) }

// heads counts the HEADs the origin answered on path.
func (o *origin) headsOn(path string) int {
	n := 0
	for _, r := range o.requests() {
		if r.Method == http.MethodHead && r.Path == path {
			n++
		}
	}
	return n
}

func (o *origin) getsOn(path string) int {
	n := 0
	for _, r := range o.requests() {
		if r.Method == http.MethodGet && r.Path == path {
			n++
		}
	}
	return n
}

// auditOf reads the class and version the audit row for this asset names.
func (f *fx) auditOf(event string) (class, version string) {
	f.t.Helper()
	if err := f.pool.QueryRow(context.Background(),
		`SELECT metadata->>'class', metadata->>'version' FROM audit_events
		  WHERE event_type = $1 AND metadata->>'asset_id' = $2`, event, f.asset.String()).Scan(&class, &version); err != nil {
		f.t.Fatalf("audit %s: %v", event, err)
	}
	return class, version
}

// awaitOrFail waits for ch, failing fast (not at the package timeout)
// when the step it stands for never happens.
func awaitOrFail(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s did not happen", what)
	}
}

func wantRevokedBecause(t *testing.T, err error, cause string) {
	t.Helper()
	wantReason(t, err, casfetch.ReasonRevokedDuringFetch)
	var e *casfetch.Error
	if !errors.As(err, &e) || e.OriginReason != cause {
		t.Fatalf("cause %+v, want %s", e, cause)
	}
}

// The exact version-bound route, the canonical size, and a signature the
// origin can verify, on every request; one GET and three HEADs, all on
// the access route; the access class asked of Allows and AllowsTx; the
// audit names the class and version.
func TestFetchAccess_VersionedRouteAndSignature(t *testing.T) {
	signers := map[string]func(priv ed25519.PrivateKey) casfetch.Signer{
		"instance": func(priv ed25519.PrivateKey) casfetch.Signer {
			return casfetch.InstanceSigner(&outbox.IdentitySigner{PrivateKey: priv, KeyURL: localBase + "/federation/instance#main-key"})
		},
		"actor": func(priv ed25519.PrivateKey) casfetch.Signer {
			return casfetch.ActorSigner(localBase+"/users/alice", priv)
		},
	}
	for sname, mk := range signers {
		for _, v := range []int64{1, 7, federation.ArchivalMaxSafeInteger} {
			t.Run(sname+"/v"+strconv.FormatInt(v, 10), func(t *testing.T) {
				f, g := newAccessFx(t, cas.ClassAccess)
				pub, priv, _ := ed25519.GenerateKey(rand.Reader)
				f.origin.pub = pub
				r := f.accessReq(v)
				r.Signer = mk(priv)
				res, err := f.client.Fetch(context.Background(), r)
				if err != nil {
					t.Fatal(err)
				}
				if res.Hash != f.hash || res.Reused || res.Deduped || res.Pin.SubjectType != casfetch.IngestPinType {
					t.Fatalf("result %+v", res)
				}
				if got, want := cas.AccessFilePath(f.asset, v, f.hash), f.accessPath(v); got != want {
					t.Fatalf("AccessFilePath %q, want %q", got, want)
				}
				reqs := f.origin.requests()
				var methods []string
				for _, sr := range reqs {
					methods = append(methods, sr.Method)
					if sr.Path != f.accessPath(v) || sr.RawQuery != f.sizeQuery() {
						t.Errorf("%s %s?%s, want %s?%s", sr.Method, sr.Path, sr.RawQuery, f.accessPath(v), f.sizeQuery())
					}
					if sr.VerifyErr != nil || sr.KeyID != r.Signer.KeyID {
						t.Errorf("%s signature: key %q err %v, want %q verified", sr.Method, sr.KeyID, sr.VerifyErr, r.Signer.KeyID)
					}
				}
				if want := []string{"HEAD", "GET", "HEAD", "HEAD"}; !slices.Equal(methods, want) {
					t.Fatalf("requests %v, want %v", methods, want)
				}
				if a, tx := g.count(false, cas.ClassAccess), g.count(true, cas.ClassAccess); a != 3 || tx != 1 || g.total() != 4 {
					t.Fatalf("grant questions: Allows(access)=%d AllowsTx(access)=%d total=%d, want 3/1/4", a, tx, g.total())
				}
				if f.ingestPins() != 1 {
					t.Fatalf("ingest pins %d", f.ingestPins())
				}
				var originID string
				_ = f.pool.QueryRow(context.Background(), `SELECT origin_server_id::text FROM storage_objects WHERE hash = $1`, f.hash).Scan(&originID)
				if originID != f.peer.ID.String() {
					t.Fatalf("origin_server_id %q", originID)
				}
				if class, version := f.auditOf("federation.cas.fetched"); class != cas.ClassAccess || version != strconv.FormatInt(v, 10) {
					t.Fatalf("audit class %q version %q", class, version)
				}
				f.noLeftovers()
			})
		}
	}
}

// An original still goes to the unversioned original route, asks the
// original class, and audits class original with no version.
func TestFetchOriginal_RouteUnchanged(t *testing.T) {
	f := newFx(t)
	g := newClassGrants(cas.ClassOriginal)
	f.client = f.newClient(g)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	f.origin.pub = pub
	r := f.req()
	r.Signer = casfetch.InstanceSigner(&outbox.IdentitySigner{PrivateKey: priv, KeyURL: localBase + "/federation/instance#main-key"})
	if _, err := f.client.Fetch(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	for _, sr := range f.origin.requests() {
		if sr.Path != f.originalPath() || sr.RawQuery != f.sizeQuery() || sr.VerifyErr != nil {
			t.Errorf("%s %s?%s verify=%v, want %s?%s", sr.Method, sr.Path, sr.RawQuery, sr.VerifyErr, f.originalPath(), f.sizeQuery())
		}
	}
	if g.count(false, cas.ClassOriginal) != 3 || g.count(true, cas.ClassOriginal) != 1 || g.total() != 4 {
		t.Fatalf("grant questions %+v", g.calls)
	}
	if class, version := f.auditOf("federation.cas.fetched"); class != cas.ClassOriginal || version != "0" {
		t.Fatalf("audit class %q version %q", class, version)
	}
}

// Bytes already held are re-verified for the access request and pinned
// with its own pin, after two HEADs on the access route and no GET.
func TestFetchAccess_VerifiedReuse(t *testing.T) {
	f, g := newAccessFx(t, cas.ClassAccess)
	f.holdLocally()
	res, err := f.client.Fetch(context.Background(), f.accessReq(3))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Reused || !res.Deduped || res.Pin.SubjectType != casfetch.IngestPinType {
		t.Fatalf("result %+v", res)
	}
	if f.origin.gets.Load() != 0 || f.origin.headsOn(f.accessPath(3)) != 2 || len(f.origin.requests()) != 2 {
		t.Fatalf("requests %+v, want two HEADs on the access route", f.origin.requests())
	}
	if f.ingestPins() != 1 || g.count(true, cas.ClassAccess) != 1 || g.count(false, cas.ClassAccess) != 2 {
		t.Fatalf("ingest pins %d, grant questions %+v", f.ingestPins(), g.calls)
	}
}

// The held bytes are verified against the access request's own
// checksums: a mismatch is refused with the stored basis and no pin.
func TestFetchAccess_ReuseChecksItsOwnFixity(t *testing.T) {
	f, _ := newAccessFx(t, cas.ClassAccess)
	f.holdLocally()
	r := f.accessReq(3)
	r.Checksums = []federation.ArchivalChecksum{{Algorithm: "sha512", Value: strings.Repeat("0", 128)}}
	_, err := f.client.Fetch(context.Background(), r)
	wantReason(t, err, casfetch.ReasonFixityMismatch)
	var e *casfetch.Error
	if !errors.As(err, &e) || e.Observed == nil || e.Observed.Basis != casfetch.ObservedStored {
		t.Fatalf("observation %+v, want the stored basis", e)
	}
	if f.ingestPins() != 0 || f.origin.gets.Load() != 0 {
		t.Fatalf("ingest pins %d, GETs %d", f.ingestPins(), f.origin.gets.Load())
	}
}

// The transfer checks are the original's: fixity, length, size cap,
// redirects and the idle deadline, with nothing held after a failure.
func TestFetchAccess_StreamFailures(t *testing.T) {
	for mode, want := range map[string]string{
		"oversize": casfetch.ReasonFixityMismatch,
		"short":    casfetch.ReasonFixityMismatch,
		"tamper":   casfetch.ReasonFixityMismatch,
		"length":   casfetch.ReasonLengthMismatch,
		"redirect": casfetch.ReasonRedirectRefused,
		"stall":    casfetch.ReasonTimeout,
	} {
		t.Run(mode, func(t *testing.T) {
			f, _ := newAccessFx(t, cas.ClassAccess)
			f.origin.getMode.Store(mode)
			_, err := f.client.Fetch(context.Background(), f.accessReq(2))
			wantReason(t, err, want)
			if f.origin.followed.Load() != 0 {
				t.Fatal("a redirect was followed")
			}
			f.nothingHeld()
		})
	}
	t.Run("HEAD redirect", func(t *testing.T) {
		f, _ := newAccessFx(t, cas.ClassAccess)
		f.origin.headCode.Store(302)
		_, err := f.client.Fetch(context.Background(), f.accessReq(2))
		wantReason(t, err, casfetch.ReasonRedirectRefused)
		if f.origin.followed.Load() != 0 || f.origin.gets.Load() != 0 {
			t.Fatal("redirect followed or GET sent")
		}
	})
}

// A request that is not exactly one access copy of one version, or an
// original that names a version, is refused before any question.
func TestFetchAccess_ValidationRefusesBeforeContact(t *testing.T) {
	cases := map[string]struct {
		mut  func(f *fx, r *casfetch.Request)
		want string
	}{
		"version 0":           {func(_ *fx, r *casfetch.Request) { r.Version = 0 }, casfetch.ReasonInvalidRequest},
		"negative version":    {func(_ *fx, r *casfetch.Request) { r.Version = -1 }, casfetch.ReasonInvalidRequest},
		"version past 2^53-1": {func(_ *fx, r *casfetch.Request) { r.Version = federation.ArchivalMaxSafeInteger + 1 }, casfetch.ReasonInvalidRequest},
		"original with version": {func(_ *fx, r *casfetch.Request) {
			r.Class, r.Version = cas.ClassOriginal, 3
		}, casfetch.ReasonInvalidRequest},
		"default class with version": {func(_ *fx, r *casfetch.Request) { r.Class, r.Version = "", 3 }, casfetch.ReasonInvalidRequest},
		"preview":                    {func(_ *fx, r *casfetch.Request) { r.Class = cas.ClassPreview }, casfetch.ReasonClassNotAvailable},
		"Access spelling":            {func(_ *fx, r *casfetch.Request) { r.Class = "Access" }, casfetch.ReasonClassNotAvailable},
		"restricted":                 {func(_ *fx, r *casfetch.Request) { r.AnnouncedSensitivity = "restricted" }, casfetch.ReasonSensitivity},
		"embargo":                    {func(_ *fx, r *casfetch.Request) { r.AnnouncedSensitivity = "embargo" }, casfetch.ReasonSensitivity},
		"too large":                  {func(_ *fx, r *casfetch.Request) { r.Size = 1 << 40 }, casfetch.ReasonTooLarge},
		"foreign origin": {func(f *fx, r *casfetch.Request) {
			r.OriginAssetURI = "https://other.example/assets/" + f.asset.String()
		}, casfetch.ReasonForeignOrigin},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f, g := newAccessFx(t, cas.ClassAccess, cas.ClassOriginal)
			r := f.accessReq(4)
			tc.mut(f, &r)
			_, err := f.client.Fetch(context.Background(), r)
			wantReason(t, err, tc.want)
			if len(f.origin.requests()) != 0 || f.origin.heads.Load()+f.origin.gets.Load() != 0 || g.total() != 0 {
				t.Fatalf("contacted the origin (%d) or the inbound record (%d)", len(f.origin.requests()), g.total())
			}
		})
	}
}

// Each of the four profiles is needed; original-transfer-1 alone serves
// originals and not access copies. Nothing is asked of the inbound
// record or the origin.
func TestFetchAccess_NeedsAllFourProfiles(t *testing.T) {
	if len(federation.DerivedTransferProfiles) != 4 {
		t.Fatalf("DerivedTransferProfiles = %v, want four", federation.DerivedTransferProfiles)
	}
	caps := map[string][]string{
		"original-transfer-1 only": {federation.ProfileOriginalTransfer1},
		"none":                     {},
	}
	for _, p := range federation.DerivedTransferProfiles {
		caps["without "+p] = without(federation.DerivedTransferProfiles, p)
	}
	for name, tokens := range caps {
		t.Run(name, func(t *testing.T) {
			f, g := newAccessFx(t, cas.ClassAccess, cas.ClassOriginal)
			f.holdLocally()
			f.setCaps(tokens)
			_, err := f.client.Fetch(context.Background(), f.accessReq(1))
			wantReason(t, err, casfetch.ReasonProfile)
			if len(f.origin.requests()) != 0 || g.total() != 0 || f.ingestPins() != 0 {
				t.Fatalf("requests %d, grant questions %d, ingest pins %d", len(f.origin.requests()), g.total(), f.ingestPins())
			}
			// The original path keeps its own one-profile rule.
			_, err = f.client.Fetch(context.Background(), f.req())
			if slices.Contains(tokens, federation.ProfileOriginalTransfer1) {
				if err != nil {
					t.Fatalf("original with original-transfer-1 stored: %v", err)
				}
			} else {
				wantReason(t, err, casfetch.ReasonProfile)
			}
		})
	}
}

// An original grant, bytes already held, or another version's answer
// is not an access grant.
func TestFetchAccess_NotGrantedByOriginalHeldBytesOrAnotherVersion(t *testing.T) {
	t.Run("original granted, access not", func(t *testing.T) {
		f, g := newAccessFx(t, cas.ClassOriginal)
		_, err := f.client.Fetch(context.Background(), f.accessReq(1))
		wantReason(t, err, casfetch.ReasonNoInboundGrant)
		if len(f.origin.requests()) != 0 {
			t.Fatal("the origin was contacted without an access grant")
		}
		f.nothingHeld()
		// The same record does allow the original...
		if _, err := f.client.Fetch(context.Background(), f.req()); err != nil {
			t.Fatal(err)
		}
		// ...and the access copy is still refused with the bytes now held.
		_, err = f.client.Fetch(context.Background(), f.accessReq(1))
		wantReason(t, err, casfetch.ReasonNoInboundGrant)
		if f.ingestPins() != 1 || f.origin.headsOn(f.accessPath(1)) != 0 {
			t.Fatalf("ingest pins %d, access HEADs %d", f.ingestPins(), f.origin.headsOn(f.accessPath(1)))
		}
		if g.count(false, cas.ClassAccess) != 2 || g.count(true, cas.ClassAccess) != 0 {
			t.Fatalf("grant questions %+v", g.calls)
		}
	})
	t.Run("bytes held, original granted", func(t *testing.T) {
		f, _ := newAccessFx(t, cas.ClassOriginal)
		f.holdLocally()
		_, err := f.client.Fetch(context.Background(), f.accessReq(1))
		wantReason(t, err, casfetch.ReasonNoInboundGrant)
		if f.ingestPins() != 0 || len(f.origin.requests()) != 0 {
			t.Fatal("pinned or contacted the origin without an access grant")
		}
	})
	t.Run("origin refuses the access route", func(t *testing.T) {
		f, _ := newAccessFx(t, cas.ClassAccess, cas.ClassOriginal)
		f.holdLocally()
		f.origin.headDeny = func(r *http.Request) bool { return strings.Contains(r.URL.Path, "/versions/") }
		_, err := f.client.Fetch(context.Background(), f.accessReq(1))
		wantReason(t, err, casfetch.ReasonOriginRefused)
		if f.ingestPins() != 0 || f.origin.gets.Load() != 0 {
			t.Fatal("pinned or fetched after the origin refused the access copy")
		}
		// The original route answers on its own.
		res, err := f.client.Fetch(context.Background(), f.req())
		if err != nil || !res.Reused {
			t.Fatalf("original: %+v %v", res, err)
		}
	})
	t.Run("another version refused", func(t *testing.T) {
		f, _ := newAccessFx(t, cas.ClassAccess)
		v2 := f.accessPath(2)
		f.origin.headDeny = func(r *http.Request) bool { return r.URL.Path == v2 }
		if _, err := f.client.Fetch(context.Background(), f.accessReq(1)); err != nil {
			t.Fatal(err)
		}
		_, err := f.client.Fetch(context.Background(), f.accessReq(2))
		wantReason(t, err, casfetch.ReasonOriginRefused)
		if f.ingestPins() != 1 || f.origin.gets.Load() != 1 {
			t.Fatalf("ingest pins %d, GETs %d: version 2 rode on version 1", f.ingestPins(), f.origin.gets.Load())
		}
		// A third version the origin serves reuses the bytes with its own pin.
		res, err := f.client.Fetch(context.Background(), f.accessReq(3))
		if err != nil || !res.Reused || f.ingestPins() != 2 || f.origin.headsOn(f.accessPath(3)) != 2 {
			t.Fatalf("version 3: %+v %v, pins %d", res, err, f.ingestPins())
		}
	})
}

// A downgrade (derived-transfer-1 dropped, original-transfer-1 kept) or
// an access revoke (the original grant kept) at each boundary of reuse
// and of the import records nothing.
func TestFetchAccess_RecheckedAtReuseAndImportBoundaries(t *testing.T) {
	changes := map[string]struct {
		apply func(f *fx, g *classGrants)
		cause string
	}{
		"downgraded": {func(f *fx, _ *classGrants) {
			f.setCaps(without(federation.DerivedTransferProfiles, federation.ProfileDerivedTransfer1))
		}, casfetch.ReasonProfile},
		"access revoked": {func(_ *fx, g *classGrants) { g.set(cas.ClassAccess, false) }, casfetch.ReasonNoInboundGrant},
	}
	type boundary struct {
		held bool
		// arm installs the hook that applies change; it returns a func
		// that waits for the hook and releases it, or nil.
		arm func(t *testing.T, f *fx, change func()) func()
		// inTx: the refusal comes from the check inside the recording
		// transaction; the peer check there answers its own reason.
		inTx bool
	}
	boundaries := map[string]boundary{
		"reuse verify read": {held: true, arm: func(t *testing.T, f *fx, change func()) func() {
			g := newGate(t)
			f.be.setBeforeGet(g.hook())
			return func() { <-g.in; change(); g.open() }
		}},
		"reuse pin transaction": {held: true, inTx: true, arm: func(_ *testing.T, f *fx, change func()) func() {
			casfetch.SetBeforeReusePin(f.client, change)
			return nil
		}},
		"download": {arm: func(t *testing.T, f *fx, change func()) func() {
			g := newGate(t)
			f.origin.beforeGET = g.hook()
			return func() { <-g.in; change(); g.open() }
		}},
		"import put": {arm: func(t *testing.T, f *fx, change func()) func() {
			g := newGate(t)
			f.be.setBeforePut(g.hook())
			return func() { <-g.in; change(); g.open() }
		}},
		"import commit transaction": {inTx: true, arm: func(_ *testing.T, f *fx, change func()) func() {
			casfetch.SetAfterFinalCheck(f.client, change)
			return nil
		}},
	}
	for bname, b := range boundaries {
		for cname, c := range changes {
			t.Run(bname+"/"+cname, func(t *testing.T) {
				f, g := newAccessFx(t, cas.ClassAccess, cas.ClassOriginal)
				if b.held {
					f.holdLocally()
				}
				drive := b.arm(t, f, func() { c.apply(f, g) })
				done := make(chan error, 1)
				go func() { _, err := f.client.Fetch(context.Background(), f.accessReq(5)); done <- err }()
				if drive != nil {
					drive()
				}
				err := <-done
				if b.inTx && c.cause == casfetch.ReasonProfile {
					wantReason(t, err, casfetch.ReasonProfile)
				} else {
					wantRevokedBecause(t, err, c.cause)
				}
				if f.ingestPins() != 0 {
					t.Fatalf("ingest pins %d after the change", f.ingestPins())
				}
				if b.held {
					if f.origin.gets.Load() != 0 {
						t.Fatal("fetched bytes already held")
					}
				} else if n := f.count(`SELECT count(*) FROM storage_objects WHERE hash = $1`); n != 0 {
					// A failed import leaves only its intent, for the
					// reconciler (storage.ImportVerified); no object row.
					t.Fatalf("storage_objects rows %d after the change", n)
				}
				if n := f.count(`SELECT count(*) FROM federation_cas_fetch_leases WHERE object_hash = $1`); n != 0 {
					t.Fatalf("leases %d after the change", n)
				}
				// The original class is untouched by either change.
				if _, err := f.client.Fetch(context.Background(), f.req()); err != nil {
					t.Fatalf("original after the change: %v", err)
				}
			})
		}
	}
}

// Requests for different versions and classes of the same bytes: one
// GET, but each request asks its own route and its own grant class and
// gets its own pin. In one process (claims) and across workers (lease).
func TestFetchAccess_SharedBytesOwnAuthorityAndPins(t *testing.T) {
	for _, tc := range []struct {
		workers      int
		withOriginal bool
	}{{1, false}, {3, false}, {1, true}, {3, true}} {
		name := strconv.Itoa(tc.workers) + " workers"
		if tc.withOriginal {
			name += ", with an original"
		}
		t.Run(name, func(t *testing.T) {
			f, g := newAccessFx(t, cas.ClassAccess, cas.ClassOriginal)
			f.origin.getDelay = 300 * time.Millisecond
			clients := []*casfetch.Client{f.client, f.client, f.client}
			if tc.workers > 1 {
				clients = []*casfetch.Client{f.client, f.newClient(g), f.newClient(g)}
			}
			reqs := []casfetch.Request{f.accessReq(1), f.accessReq(2)}
			paths := []string{f.accessPath(1), f.accessPath(2)}
			if tc.withOriginal {
				reqs, paths = append(reqs, f.req()), append(paths, f.originalPath())
			}
			results := make([]*casfetch.Result, len(reqs))
			errs := make([]error, len(reqs))
			var wg sync.WaitGroup
			for i := range reqs {
				wg.Add(1)
				go func() { defer wg.Done(); results[i], errs[i] = clients[i].Fetch(context.Background(), reqs[i]) }()
			}
			wg.Wait()
			for i, err := range errs {
				if err != nil {
					t.Fatalf("request %d: %v", i, err)
				}
			}
			// Access versions share one transfer of their bytes. An
			// original is in its own lease namespace (leaseKeyFor): it may
			// fetch alongside, never waits behind, and at most one of each.
			accessGETs := f.origin.getsOn(f.accessPath(1)) + f.origin.getsOn(f.accessPath(2))
			originalGETs := f.origin.getsOn(f.originalPath())
			if tc.withOriginal {
				if accessGETs > 1 || originalGETs > 1 || accessGETs+originalGETs < 1 {
					t.Fatalf("GETs: access %d, original %d; want at most one per class, at least one", accessGETs, originalGETs)
				}
			} else if accessGETs != 1 || originalGETs != 0 {
				t.Fatalf("GETs: access %d, original %d; want exactly one shared access transfer", accessGETs, originalGETs)
			}
			fresh := 0
			seen := map[string]bool{}
			for i, res := range results {
				if !res.Reused && i < 2 {
					fresh++
				}
				seen[res.IngestID.String()] = true
				if h := f.origin.headsOn(paths[i]); h < 2 {
					t.Errorf("request %d asked its route %d times, want at least 2", i, h)
				}
			}
			if fresh > 1 || len(seen) != len(reqs) || f.ingestPins() != len(reqs) {
				t.Fatalf("access fresh %d, distinct ingests %d, ingest pins %d", fresh, len(seen), f.ingestPins())
			}
			wantOriginalTx := 0
			if tc.withOriginal {
				wantOriginalTx = 1
			}
			if g.count(true, cas.ClassAccess) != 2 || g.count(true, cas.ClassOriginal) != wantOriginalTx {
				t.Fatalf("in-transaction grant questions %+v", g.calls)
			}
		})
	}
}

// A caller that waits behind another version's transfer is refused when
// its own version is refused, whatever the leader got: it never takes
// the leader's lease, answer or pin. Its own route is asked again after
// the wait.
func TestFetchAccess_WaiterLosesItsOwnVersion(t *testing.T) {
	for _, workers := range []int{1, 2} {
		t.Run(strconv.Itoa(workers)+" workers", func(t *testing.T) {
			f, g := newAccessFx(t, cas.ClassAccess)
			waiter := f.client
			if workers > 1 {
				waiter = f.newClient(g)
			}
			v2 := f.accessPath(2)
			var denyV2 atomic.Bool
			firstV2 := make(chan struct{})
			var once sync.Once
			f.origin.headDeny = func(r *http.Request) bool {
				if r.URL.Path != v2 {
					return false
				}
				d := denyV2.Load()
				once.Do(func() { close(firstV2) })
				return d
			}
			getGate := newGate(t)
			f.origin.beforeGET = getGate.hook()

			leader := make(chan error, 1)
			go func() { _, err := f.client.Fetch(context.Background(), f.accessReq(1)); leader <- err }()
			awaitOrFail(t, getGate.in, "version 1's GET") // in flight, holding the claim and the lease
			waited := make(chan error, 1)
			go func() { _, err := waiter.Fetch(context.Background(), f.accessReq(2)); waited <- err }()
			awaitOrFail(t, firstV2, "version 2's first HEAD") // allowed once, before the wait
			denyV2.Store(true)
			getGate.open()

			if err := <-leader; err != nil {
				t.Fatalf("leader: %v", err)
			}
			err := <-waited
			switch casfetch.ReasonOf(err) {
			case casfetch.ReasonOriginRefused: // refused on its own re-authorisation after the wait
			case casfetch.ReasonRevokedDuringFetch: // reached the leader's bytes, refused after re-verifying them
				wantRevokedBecause(t, err, casfetch.ReasonOriginRefused)
			default:
				t.Fatalf("waiter: %v, want its own version refused", err)
			}
			if f.ingestPins() != 1 {
				t.Fatalf("ingest pins %d, want only the leader's", f.ingestPins())
			}
			if h := f.origin.headsOn(v2); h < 2 {
				t.Fatalf("version 2 asked %d times, want again after the wait", h)
			}
			t.Logf("GETs %d (v1 %d, v2 %d), v2 HEADs %d, waiter reason %s",
				f.origin.gets.Load(), f.origin.getsOn(f.accessPath(1)), f.origin.getsOn(v2), f.origin.headsOn(v2), casfetch.ReasonOf(err))
			f.noLeftovers()
		})
	}
}

// A caller that waits behind another version's transfer re-verifies the
// bytes against its own checksums: its mismatch is its own, and the
// other request still completes with its own pin.
func TestFetchAccess_WaiterChecksItsOwnFixity(t *testing.T) {
	f, _ := newAccessFx(t, cas.ClassAccess)
	f.origin.getDelay = 300 * time.Millisecond
	good, bad := f.accessReq(1), f.accessReq(2)
	bad.Checksums = []federation.ArchivalChecksum{{Algorithm: "sha512", Value: strings.Repeat("0", 128)}}
	var wg sync.WaitGroup
	var eGood, eBad error
	wg.Add(2)
	go func() { defer wg.Done(); _, eGood = f.client.Fetch(context.Background(), good) }()
	go func() { defer wg.Done(); _, eBad = f.client.Fetch(context.Background(), bad) }()
	wg.Wait()
	if eGood != nil {
		t.Fatalf("version 1: %v", eGood)
	}
	wantReason(t, eBad, casfetch.ReasonFixityMismatch)
	if f.ingestPins() != 1 {
		t.Fatalf("ingest pins %d, want version 1's only", f.ingestPins())
	}
	f.noLeftovers()
}

// RecheckAccess (#1597 access worker): one fresh signed HEAD of the exact
// version-bound request at the registered origin, after the receiver's
// current peer record and its live access grant; nothing fetched or held.
func TestRecheckAccess_ExactSignedHEAD(t *testing.T) {
	f, g := newAccessFx(t, cas.ClassAccess)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	f.origin.pub = pub
	r := f.accessReq(7)
	r.Signer = casfetch.InstanceSigner(&outbox.IdentitySigner{PrivateKey: priv, KeyURL: localBase + "/federation/instance#main-key"})
	if err := f.client.RecheckAccess(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	reqs := f.origin.requests()
	if len(reqs) != 1 || reqs[0].Method != http.MethodHead || reqs[0].Path != f.accessPath(7) || reqs[0].RawQuery != f.sizeQuery() ||
		reqs[0].VerifyErr != nil || reqs[0].KeyID != r.Signer.KeyID {
		t.Fatalf("requests %+v, want one verified HEAD of %s?%s", reqs, f.accessPath(7), f.sizeQuery())
	}
	if g.count(false, cas.ClassAccess) != 1 || g.count(true, cas.ClassAccess) != 0 || g.total() != 1 {
		t.Fatalf("grant questions %+v, want one Allows(access)", g.calls)
	}
	f.nothingHeld()
}

// Refused before any contact: not an access request, an invalid one, a
// peer without the four profiles, or no access grant (an original grant
// is not one).
func TestRecheckAccess_RefusedBeforeContact(t *testing.T) {
	cases := map[string]struct {
		classes []string
		caps    []string
		mut     func(f *fx) casfetch.Request
		want    string
	}{
		"an original request": {[]string{cas.ClassAccess, cas.ClassOriginal}, federation.DerivedTransferProfiles,
			func(f *fx) casfetch.Request { return f.req() }, casfetch.ReasonClassNotAvailable},
		"version 0": {[]string{cas.ClassAccess}, federation.DerivedTransferProfiles,
			func(f *fx) casfetch.Request { return f.accessReq(0) }, casfetch.ReasonInvalidRequest},
		"restricted": {[]string{cas.ClassAccess}, federation.DerivedTransferProfiles,
			func(f *fx) casfetch.Request { r := f.accessReq(1); r.AnnouncedSensitivity = "restricted"; return r }, casfetch.ReasonSensitivity},
		"derived-transfer-1 not negotiated": {[]string{cas.ClassAccess}, without(federation.DerivedTransferProfiles, federation.ProfileDerivedTransfer1),
			func(f *fx) casfetch.Request { return f.accessReq(1) }, casfetch.ReasonProfile},
		"original grant only": {[]string{cas.ClassOriginal}, federation.DerivedTransferProfiles,
			func(f *fx) casfetch.Request { return f.accessReq(1) }, casfetch.ReasonNoInboundGrant},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f, g := newAccessFx(t, tc.classes...)
			f.setCaps(tc.caps)
			err := f.client.RecheckAccess(context.Background(), tc.mut(f))
			wantReason(t, err, tc.want)
			if n := len(f.origin.requests()); n != 0 || f.origin.heads.Load()+f.origin.gets.Load() != 0 {
				t.Fatalf("contacted the origin (%d)", n)
			}
			if tc.want != casfetch.ReasonNoInboundGrant && g.total() != 0 {
				t.Fatalf("asked the inbound record %d times", g.total())
			}
			f.nothingHeld()
		})
	}
}

// The origin's refusal and a redirect are the origin's answer.
func TestRecheckAccess_OriginRefusalAndRedirect(t *testing.T) {
	t.Run("refused", func(t *testing.T) {
		f, _ := newAccessFx(t, cas.ClassAccess)
		f.origin.headDeny = func(r *http.Request) bool { return strings.Contains(r.URL.Path, "/versions/") }
		wantReason(t, f.client.RecheckAccess(context.Background(), f.accessReq(3)), casfetch.ReasonOriginRefused)
		if f.origin.headsOn(f.accessPath(3)) != 1 || f.origin.gets.Load() != 0 {
			t.Fatal("want exactly one HEAD and no GET")
		}
	})
	t.Run("redirect", func(t *testing.T) {
		f, _ := newAccessFx(t, cas.ClassAccess)
		f.origin.headCode.Store(302)
		wantReason(t, f.client.RecheckAccess(context.Background(), f.accessReq(3)), casfetch.ReasonRedirectRefused)
		if f.origin.followed.Load() != 0 {
			t.Fatal("a redirect was followed")
		}
	})
}

// An original of the same peer, asset, bytes and signer completes while an
// access transfer of them is stalled mid-GET: the access transfer holds
// its lease in its own namespace, never the original's. Then the access
// transfer completes too (its import finds the bytes stored).
func TestFetch_OriginalNotBlockedByStalledAccessTransfer(t *testing.T) {
	f, _ := newAccessFx(t, cas.ClassAccess, cas.ClassOriginal)
	ctx := context.Background()
	// The first GET (the access one) waits for release; every later GET
	// passes at once. (A sync.Once gate would block later callers too,
	// for as long as the first is inside it.)
	var first atomic.Bool
	in, release := make(chan struct{}), make(chan struct{})
	var released sync.Once
	open := func() { released.Do(func() { close(release) }) }
	t.Cleanup(open)
	f.origin.beforeGET = func() {
		if first.CompareAndSwap(false, true) {
			close(in)
			<-release
		}
	}
	accessDone := make(chan error, 1)
	go func() { _, err := f.client.Fetch(ctx, f.accessReq(1)); accessDone <- err }()
	awaitOrFail(t, in, "the access GET")
	rows, err := f.pool.Query(ctx, `SELECT signer FROM federation_cas_fetch_leases WHERE object_hash = $1`, f.hash)
	if err != nil {
		t.Fatal(err)
	}
	var signers []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		signers = append(signers, s)
	}
	rows.Close()
	if len(signers) != 1 || signers[0] != "access-class|"+f.signer.KeyID {
		t.Fatalf("lease rows during the stall %q, want the access namespace only", signers)
	}
	origDone := make(chan error, 1)
	go func() {
		res, err := f.client.Fetch(ctx, f.req())
		if err == nil && (res.Reused || res.Pin.SubjectType != casfetch.IngestPinType) {
			err = fmt.Errorf("original result %+v", res)
		}
		origDone <- err
	}()
	select {
	case err := <-origDone:
		if err != nil {
			t.Fatalf("original: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the original waited behind the stalled access transfer")
	}
	select {
	case err := <-accessDone:
		t.Fatalf("the access transfer finished while stalled: %v", err)
	default:
	}
	if n := f.origin.getsOn(f.originalPath()); n != 1 {
		t.Fatalf("original GETs %d", n)
	}
	open()
	if err := <-accessDone; err != nil {
		t.Fatalf("access after the stall: %v", err)
	}
	if f.ingestPins() != 2 {
		t.Fatalf("ingest pins %d, want one per caller", f.ingestPins())
	}
	f.noLeftovers()
}

// The access lease namespace is never accepted as a signer's key id.
func TestFetch_AccessLeaseNamespaceIsNotASigner(t *testing.T) {
	f, _ := newAccessFx(t, cas.ClassAccess, cas.ClassOriginal)
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	for _, r := range []casfetch.Request{f.req(), f.accessReq(1)} {
		r.Signer = casfetch.InstanceSigner(&outbox.IdentitySigner{PrivateKey: priv, KeyURL: "access-class|" + localBase + "/federation/instance#main-key"})
		_, err := f.client.Fetch(context.Background(), r)
		wantReason(t, err, casfetch.ReasonForeignSigner)
	}
	if len(f.origin.requests()) != 0 {
		t.Fatal("contacted the origin")
	}
}
