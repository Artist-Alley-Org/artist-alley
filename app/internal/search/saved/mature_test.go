// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1147 — every saved search on the install ran as the disqualified
// viewer.
//
// `Executor.Run` built its `search.Query` with the owner as caller and
// no `Mature`, so the zero MatureViewer reached the Engine — and that
// struct's zero value is deliberately the viewer who qualifies for
// nothing. Fail-closed, so nothing leaked; but it was silent and it was
// permanent. An owner who had opted in simply never got a mature hit in
// a digest again, and no error said why.
//
// # Why these assert on the QUERY
//
// Because the observable IS the Query. Zero hits is what a working gate
// and an absent one both produce, so an end-to-end assertion cannot tell
// them apart — the same argument the IIIF sibling's file makes. What
// separates them is the viewer handed to the Engine, and whether it was
// resolved for the search's OWNER.
//
// Needs no database: the Engine and the resolver are both seams the
// package already declares for exactly this.

package saved_test

import (
	"context"
	"errors"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/search"
	"github.com/mscrnt/artist-alley/app/internal/search/saved"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// smFakeEngine records the Query it was handed.
type smFakeEngine struct{ got search.Query }

func (f *smFakeEngine) Run(_ context.Context, q search.Query) (search.QueryResult, error) {
	f.got = q
	return search.QueryResult{}, nil
}

// smResolver answers for one specific user ref and refuses every other,
// which is what makes "resolved for the OWNER" an assertion rather than
// a hope: an executor that resolved for nobody, or for some ambient
// caller, gets the error arm and lands on the disqualified viewer.
type smResolver struct {
	wantRef int64
	answer  visibility.MatureViewer
	sawRef  int64
	calls   int
}

func (r *smResolver) ResolveMature(_ context.Context, c visibility.Caller) (visibility.MatureViewer, error) {
	r.calls++
	r.sawRef = c.UserRef
	if c.UserRef != r.wantRef {
		return visibility.AnonymousMatureViewer, errors.New("saved: resolver asked about the wrong caller")
	}
	return r.answer, nil
}

// smRun wires an executor with both seams and runs one saved search.
// The DSL is a bare term so the Engine takes the BM25 path and no
// vector.Fetcher is needed.
func smRun(t *testing.T, owner int64, r visibility.MatureResolver) search.Query {
	t.Helper()
	eng := &smFakeEngine{}
	ex := saved.NewExecutor(nil, eng, nil)
	if r != nil {
		ex.SetMatureResolver(r)
	}
	if _, err := ex.Run(context.Background(), saved.Row{
		OwnerUserRef: owner,
		DSL:          "sunset",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if eng.got.CallerUserRef == nil {
		t.Fatal("the Engine was never handed a caller — the fixture stopped short of the " +
			"line under test")
	}
	return eng.got
}

// TestSavedSearch_RunsAsTheOwnersResolvedViewer is the control arm, and
// it is the one that fails against the shipped code.
//
// It asserts two separate things, because the fix has two halves that
// can each be wrong on their own: that a viewer was resolved AT ALL, and
// that it was resolved for the search's OWNER rather than for whoever
// the job runner happens to be.
func TestSavedSearch_RunsAsTheOwnersResolvedViewer(t *testing.T) {
	const owner int64 = 4242
	qualified := visibility.MatureViewer{SignedIn: true, OptedIn: true, InstanceAllows: true}
	res := &smResolver{wantRef: owner, answer: qualified}

	got := smRun(t, owner, res)

	if res.calls != 1 {
		t.Errorf("the resolver was called %d times, want exactly 1. It is resolved once "+
			"per RUN and never per hit — visibility.MatureViewer's own contract", res.calls)
	}
	if res.sawRef != owner {
		t.Errorf("the resolver was asked about ref %d, want the saved search's owner %d. "+
			"A saved search runs as its owner; resolving for anybody else would mail "+
			"them somebody else's answer", res.sawRef, owner)
	}
	if got.Mature != qualified {
		t.Errorf("Query.Mature = %+v, want %+v — an owner who signed in, opted in and is "+
			"on an instance that allows mature content was having their own saved "+
			"search run as somebody who had done none of those things (#1147)",
			got.Mature, qualified)
	}
}

// TestSavedSearch_OptedOutOwnerStaysDisqualified is the withheld arm.
// Same executor, same wiring, opposite preference.
func TestSavedSearch_OptedOutOwnerStaysDisqualified(t *testing.T) {
	const owner int64 = 4243
	res := &smResolver{
		wantRef: owner,
		answer:  visibility.MatureViewer{SignedIn: true, InstanceAllows: true},
	}
	if got := smRun(t, owner, res); visibility.QualifiesForMature(got.Mature) {
		t.Errorf("Query.Mature = %+v qualifies, but this owner never opted in", got.Mature)
	}
}

// TestSavedSearch_UnwiredResolverFailsClosed pins the direction a
// missing wire must fail in.
//
// This is not a theoretical branch: the executor is constructed in
// api.go before the resolver exists, so an ordering change or a dropped
// line puts every saved search on this path. It must land on the
// DISQUALIFIED viewer — the pre-#1147 behaviour, which showed too little
// — and never on a permissive default.
//
// visibility.ResolveMatureOr owns that decision; this test is what stops
// a future edit from answering it locally and differently.
func TestSavedSearch_UnwiredResolverFailsClosed(t *testing.T) {
	if got := smRun(t, 4244, nil); visibility.QualifiesForMature(got.Mature) {
		t.Errorf("Query.Mature = %+v with NO resolver wired. A gate that has lost its "+
			"inputs must refuse rather than widen", got.Mature)
	}

	// And an erroring resolver, which is the live version of the same
	// case: a preferences-table blip must shorten the digest, not widen
	// it.
	broken := &smResolver{wantRef: -1}
	if got := smRun(t, 4245, broken); visibility.QualifiesForMature(got.Mature) {
		t.Errorf("Query.Mature = %+v after the resolver returned an error", got.Mature)
	}
}
