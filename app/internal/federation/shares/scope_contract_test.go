// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1524 - the share scope contract: view < comment < annotate < remix,
// and `edit` (editing the original on its own instance) is reserved,
// refused and never aliased to remix.
//
// Until erratum E7 the normative spec (v1 §7.5.1) listed
// `view | comment | annotate | edit` while the Go catalogue, the
// database CHECK and the OpenAPI enums used `remix`. These tests read
// each of the four sources rather than a copy, so the next drift fails
// here, and they check every grant/required pair against the real gate.
//
// The spec file lives outside app/: scripts/test.sh mounts it, and a
// missing mount FAILS (it does not skip; #956's lesson).

package shares_test

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/shares"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

const specPath = "../../../../docs/spec/federation/v1.md"
const openapiPath = "../../../api/openapi.yaml"

func ladder() []string {
	out := make([]string, 0, len(federation.ShareScopeOrdered))
	for _, s := range federation.ShareScopeOrdered {
		out = append(out, string(s))
	}
	return out
}

func TestShareScopeContract_SpecGoDBOpenAPIAgree(t *testing.T) {
	want := ladder()

	// 1. The normative spec, §7.5.1's aa:Share `scope` line.
	raw, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("cannot read the normative spec at %s (scripts/test.sh must mount it): %v", specPath, err)
	}
	m := regexp.MustCompile(`"scope":[^\n]*REQUIRED: ([a-z]+(?: \| [a-z]+)*)`).FindSubmatch(raw)
	if m == nil {
		t.Fatalf("no `\"scope\": ... // REQUIRED: a | b ...` line in %s §7.5.1", specPath)
	}
	if got := strings.Split(string(m[1]), " | "); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("spec §7.5.1 scope set %v, Go ladder %v", got, want)
	}

	// 2. The database CHECK on federation_shares.scope.
	fx := newGateFixture(t)
	var def string
	if err := fx.pool.QueryRow(context.Background(),
		`SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = 'federation_shares_scope_check'`).Scan(&def); err != nil {
		t.Fatalf("read scope CHECK: %v", err)
	}
	var dbSet []string
	for _, mm := range regexp.MustCompile(`'([a-z]+)'::text`).FindAllStringSubmatch(def, -1) {
		dbSet = append(dbSet, mm[1])
	}
	if strings.Join(dbSet, ",") != strings.Join(want, ",") {
		t.Fatalf("DB CHECK %v (%s), Go ladder %v", dbSet, def, want)
	}

	// 3. The OpenAPI scope enums (FederationShare + FederationShareCreate)
	// as written in the spec file, and as generated.
	api, err := os.ReadFile(openapiPath)
	if err != nil {
		t.Fatalf("read %s: %v", openapiPath, err)
	}
	// Each schema block runs from its `    Name:` line to the next schema
	// at the same indent. Other schemas also have a `scope` enum
	// (PromoBand, FeaturedItemInput: public/org), so match by name.
	enumRe := regexp.MustCompile(`(?m)^\s+scope:\n\s+type: string\n\s+enum: \[([a-z, ]+)\]`)
	nextSchema := regexp.MustCompile(`(?m)^    [A-Za-z0-9_]+:\s*$`)
	for _, name := range []string{"FederationShare", "FederationShareCreate"} {
		start := strings.Index(string(api), "\n    "+name+":\n")
		if start < 0 {
			t.Fatalf("schema %s not found in %s", name, openapiPath)
		}
		block := api[start+1:]
		if loc := nextSchema.FindIndex(block[len(name)+6:]); loc != nil {
			block = block[:len(name)+6+loc[0]]
		}
		e := enumRe.FindSubmatch(block)
		if e == nil {
			t.Fatalf("schema %s has no scope enum", name)
		}
		if got := strings.Split(string(e[1]), ", "); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("OpenAPI %s scope enum %v, Go ladder %v", name, got, want)
		}
	}
	for _, s := range want {
		if !openapi.FederationShareScope(s).Valid() || !openapi.FederationShareCreateScope(s).Valid() {
			t.Errorf("generated OpenAPI enum rejects %q", s)
		}
	}
	if openapi.FederationShareScope("edit").Valid() || openapi.FederationShareCreateScope("edit").Valid() {
		t.Error(`generated OpenAPI enum accepts the reserved "edit"`)
	}
}

func TestCanPeerAccess_EveryGrantAgainstEveryRequiredScope(t *testing.T) {
	fx := newGateFixture(t)
	ctx := context.Background()
	for _, grant := range federation.ShareScopeOrdered {
		objectID := uuid.New()
		fx.insertShare(t, shares.InsertInput{
			GrantorUserRef: fx.grantorRef, ObjectKind: federation.ShareObjectKindPost,
			ObjectID: objectID, PeerID: fx.peerID, Scope: grant,
		})
		check := func(required federation.ShareScope) shares.AccessDecision {
			d, err := fx.reg.CanPeerAccess(ctx, shares.AccessRequest{
				PeerID: fx.peerID, ObjectKind: federation.ShareObjectKindPost, ObjectID: objectID,
				RequiredScope: required, PeerEnabled: true, PeerConnected: true,
			})
			if err != nil {
				t.Fatalf("grant %s, required %s: %v", grant, required, err)
			}
			return d
		}
		for _, required := range federation.ShareScopeOrdered {
			d := check(required)
			want := grant.Rank() >= required.Rank()
			if d.Allowed != want {
				t.Errorf("grant %s, required %s: allowed=%v, want %v (%s)", grant, required, d.Allowed, want, d.Reason)
			}
			if !want && d.Reason != shares.RejectReasonInsufficientScope {
				t.Errorf("grant %s, required %s: reason %q, want insufficient_scope", grant, required, d.Reason)
			}
		}
		// Editing the original: denied under every grant, remix included.
		if d := check(federation.ShareScopeEditReserved); d.Allowed || d.Reason != shares.RejectReasonInsufficientScope {
			t.Errorf("grant %s, required edit (original): %+v, want denied insufficient_scope", grant, d)
		}
	}
}

func TestActivitiesThatModifyTheOriginal_DeniedEvenWithRemix(t *testing.T) {
	fx := newGateFixture(t)
	objectID := uuid.New()
	fx.insertShare(t, shares.InsertInput{
		GrantorUserRef: fx.grantorRef, ObjectKind: federation.ShareObjectKindPost,
		ObjectID: objectID, PeerID: fx.peerID, Scope: federation.ShareScopeRemix,
	})
	decide := func(verb federation.ActivityType) shares.AccessDecision {
		t.Helper()
		required, ok := shares.ActivityRequiredScope(verb)
		if !ok {
			t.Fatalf("%s: mapping says skip the share gate", verb)
		}
		d, err := fx.reg.CanPeerAccess(context.Background(), shares.AccessRequest{
			PeerID: fx.peerID, ObjectKind: federation.ShareObjectKindPost, ObjectID: objectID,
			RequiredScope: required, PeerEnabled: true, PeerConnected: true,
		})
		if err != nil {
			t.Fatalf("%s: %v", verb, err)
		}
		return d
	}
	for _, verb := range []federation.ActivityType{
		federation.ActivityUpdate, federation.ActivityDelete, federation.ActivityAAWorkflowTransition,
		federation.ActivityAdd, federation.ActivityRemove,
	} {
		if d := decide(verb); d.Allowed {
			t.Errorf("%s on the original was admitted under a remix share: %+v", verb, d)
		}
	}
	// Reuse and interaction stay admitted under remix.
	for _, verb := range []federation.ActivityType{
		federation.ActivityLike, federation.ActivityCreate, federation.ActivityAAAnnotation, federation.ActivityAAApprove,
	} {
		if d := decide(verb); !d.Allowed {
			t.Errorf("%s under a remix share was denied: %+v", verb, d)
		}
	}
}

func TestGrant_EditScopeIsRefusedNotAliased(t *testing.T) {
	fx := newAdminFixture(t)
	objectID := uuid.New()
	body := func(scope string) *openapi.FederationShareCreate {
		return &openapi.FederationShareCreate{
			ObjectKind: openapi.FederationShareCreateObjectKind(federation.ShareObjectKindPost),
			ObjectId:   objectID, PeerId: fx.peerID,
			Scope: openapi.FederationShareCreateScope(scope),
		}
	}
	resp, err := fx.handler.GrantFederationShare(fx.ctx(), openapi.GrantFederationShareRequestObject{Body: body("edit")})
	if err != nil {
		t.Fatalf("grant edit: %v", err)
	}
	bad, ok := resp.(openapi.GrantFederationShare400JSONResponse)
	if !ok {
		t.Fatalf("grant edit: want 400, got %T %+v", resp, resp)
	}
	if !strings.Contains(bad.Error, `"edit"`) || !strings.Contains(bad.Error, "reserved") {
		t.Errorf("grant edit: 400 %q does not say edit is reserved", bad.Error)
	}
	var rows int
	_ = fx.pool.QueryRow(context.Background(), `SELECT count(*) FROM federation_shares WHERE object_id=$1`, objectID).Scan(&rows)
	if rows != 0 {
		t.Fatalf("grant edit stored %d row(s); it must store nothing (and never a remix row)", rows)
	}

	resp, err = fx.handler.GrantFederationShare(fx.ctx(), openapi.GrantFederationShareRequestObject{Body: body("remix")})
	if err != nil {
		t.Fatalf("grant remix: %v", err)
	}
	created, ok := resp.(openapi.GrantFederationShare201JSONResponse)
	if !ok || created.Scope != openapi.FederationShareScopeRemix {
		t.Fatalf("grant remix: %T %+v, want 201 with scope remix", resp, resp)
	}
}
