// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1577 receiver adversarial gate (spec v1 §18.2, §18.4, §18.5, in part
// §18.6): hostile, ordering and authority cases against the destination's
// production inbox, dispatcher, asset worker and API, in BOTH directions.
//
// The baseline is real: a source upload, seal, grant and automatic
// delivery (newAdvGate). Every case after it is a MANUAL ADVERSARIAL WIRE
// ENVELOPE (built and signed by the test with the source's real keys,
// held in memory only; see the helpers) or a TEST-ONLY RECEIVER POLICY
// FAULT FIXTURE (Authority). None of it proves production source
// emission, sender refusal, real renegotiation, source recorder
// concurrency, rejected-fetch activation, queued withdrawal or a live UI.
// The profiles stay unadvertised.

package twostack_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/federation"
)

func bp(b bool) *bool { return &b }

// 1. Strict snapshot shape, bounds and atomic refusal.
func TestTwoStack_MetadataAdversarial_Snapshots(t *testing.T) {
	flowDirections(t, func(t *testing.T, g *flowGate) {
		a := newAdvGate(t, g)
		flip := !a.declaredMature()

		t.Run("valid controls are kept as sent", func(t *testing.T) {
			for _, c := range []struct {
				name string
				snap snap
			}{
				{"hostile but valid text", want(sp(`<img src=x onerror="alert(1)"> & "quotes"`), sp("javascript:alert(1) https://evil.example/x\n<script>"),
					sp(advRights), sp(`<a href="javascript:alert(2)">Studio</a>`))},
				{"exact UTF-8 limits", want(sp(strings.Repeat("é", 512)), sp(strings.Repeat("d", 4096)), nil, nil)},
				{"four nulls", want(nil, nil, nil, nil)},
			} {
				md, _ := json.Marshal(c.snap)
				env, body := a.build(upd{metadata: md})
				rev := a.accepted(c.name, env, a.post(body, "", nil))
				a.checkKept(c.name, env, rev, true)
			}
			// 32,768 raw bytes, whitespace inside the member: accepted.
			env, body := a.build(upd{metadata: meta(nil, nil, nil, nil)})
			body = padMetadata(t, body, federation.AssetMetadataMaxMemberBytes)
			rev := a.accepted("32,768 raw bytes", env, a.post(body, "", nil))
			a.checkKept("32,768 raw bytes", env, rev, true)
		})

		t.Run("invalid snapshots are refused whole", func(t *testing.T) {
			stmt := func(s string) string {
				return `{"title":"Valid sibling","description":null,"rights":null,"requiredStatement":` + s + `}`
			}
			for _, c := range []struct {
				name   string
				raw    string
				mutate func(map[string]json.RawMessage)
				extra  map[string]json.RawMessage
				status int
				reason federation.InboxStatus
			}{
				{"an absent key", `{"title":"x","description":null,"rights":null}`, nil, nil, 400, federation.InboxStatusMetadataInvalid},
				{"a differently cased key", `{"Title":"x","description":null,"rights":null,"requiredStatement":null}`, nil, nil, 400, federation.InboxStatusMetadataInvalid},
				{"an unknown key", `{"title":"x","description":null,"rights":null,"requiredStatement":null,"subject":"x"}`, nil, nil, 400, federation.InboxStatusMetadataInvalid},
				{"a whole-member null", `null`, nil, nil, 400, federation.InboxStatusMetadataInvalid},
				{"an empty title", `{"title":"","description":null,"rights":null,"requiredStatement":null}`, nil, nil, 400, federation.InboxStatusMetadataInvalid},
				{"a control character", `{"title":"a\rb","description":null,"rights":null,"requiredStatement":null}`, nil, nil, 400, federation.InboxStatusMetadataInvalid},
				{"a title of 1,025 bytes", `{"title":"` + strings.Repeat("é", 512) + `x","description":null,"rights":null,"requiredStatement":null}`, nil, nil, 400, federation.InboxStatusMetadataInvalid},
				{"a description of 4,097 bytes", `{"title":null,"description":"` + strings.Repeat("d", 4097) + `","rights":null,"requiredStatement":null}`, nil, nil, 400, federation.InboxStatusMetadataInvalid},
				{"a valid title beside a javascript: rights", `{"title":"Valid sibling","description":null,"rights":"javascript:alert(1)","requiredStatement":null}`, nil, nil, 400, federation.InboxStatusMetadataInvalid},
				{"a valid title beside a rights on another host", `{"title":"Valid sibling","description":null,"rights":"https://example.org/licenses/by/4.0/","requiredStatement":null}`, nil, nil, 400, federation.InboxStatusMetadataInvalid},
				{"a statement that is not a label/value", stmt(`{"label":"Attribution","value":{"none":["S"]}}`), nil, nil, 400, federation.InboxStatusMetadataInvalid},
				{"a language map that is not lists", stmt(`{"label":{"en":"Attribution"},"value":{"none":["S"]}}`), nil, nil, 400, federation.InboxStatusMetadataInvalid},
				{"a descriptive companion member, even null", `{"title":"x","description":null,"rights":null,"requiredStatement":null}`,
					func(c map[string]json.RawMessage) { c["rights"] = json.RawMessage(`null`) }, nil, 400, federation.InboxStatusMetadataInvalid},
				{"a malformed aa:asset", `{"title":"x","description":null,"rights":null,"requiredStatement":null}`,
					func(c map[string]json.RawMessage) { delete(c, "files") }, nil, 400, federation.InboxStatusArchivalInvalid},
				{"an unrelated top-level member", `{"title":"x","description":null,"rights":null,"requiredStatement":null}`, nil,
					map[string]json.RawMessage{"content": json.RawMessage(`"x"`)}, 400, federation.InboxStatusUnknownField},
			} {
				env, body := a.build(upd{metadata: json.RawMessage(c.raw), mature: bp(flip), mutate: c.mutate, extra: c.extra})
				a.sendRefused(c.name, env.ID, body, "", nil, c.status, c.reason)
			}
			// Repeated names fail the strict envelope parse first.
			for name, raw := range map[string]string{
				"a repeated key":                  `{"title":"a","title":"b","description":null,"rights":null,"requiredStatement":null}`,
				"a repeated language tag":         `{"title":null,"description":null,"rights":null,"requiredStatement":{"label":{"en":["A"],"en":["B"]},"value":{"none":["S"]}}}`,
				"a repeated key inside the value": `{"title":null,"description":null,"rights":null,"requiredStatement":{"label":{"en":["A"]},"value":{"none":["S"]},"value":{"none":["T"]}}}`,
			} {
				env, body := a.build(upd{metadata: meta(sp("Valid"), nil, nil, nil), mature: bp(flip)})
				body = replaceMetadata(t, body, raw)
				a.sendRefused(name, env.ID, body, "", nil, 400, federation.InboxStatusInvalidMember)
			}
			// 32,769 raw bytes: over the member bound.
			env, body := a.build(upd{metadata: meta(nil, nil, nil, nil), mature: bp(flip)})
			body = padMetadata(t, body, federation.AssetMetadataMaxMemberBytes+1)
			a.sendRefused("32,769 raw bytes", env.ID, body, "", nil, 400, federation.InboxStatusMetadataInvalid)
		})
		a.servedOriginal("at the end")
	})
}

// 2. Authentication, origin and object binding, held-version conflict.
func TestTwoStack_MetadataAdversarial_IdentityAndVersion(t *testing.T) {
	flowDirections(t, func(t *testing.T, g *flowGate) {
		a := newAdvGate(t, g)
		valid := func() json.RawMessage { return meta(sp("Identity "+uuid.NewString()[:8]), nil, sp(advRights), nil) }

		// A control under the same keys is kept.
		env, body := a.build(upd{metadata: valid()})
		a.checkKept("the control", env, a.accepted("the control", env, a.post(body, "", nil)), true)

		env, body = a.build(upd{metadata: valid()})
		a.sendRefused("an unpaired origin's key", env.ID, body, "https://unpaired-"+uuid.NewString()[:8]+".example/federation/instance#main-key", randomKey(t), http.StatusUnauthorized, federation.InboxStatusUnknownPeer)
		env, body = a.build(upd{metadata: valid()})
		a.sendRefused("the source's key id, another key", env.ID, body, a.instKeyID, randomKey(t), http.StatusUnauthorized, federation.InboxStatusSigInvalid)
		for name, actor := range map[string]string{
			"an actor of the destination":     a.dst.url + "/users/someone",
			"an actor of an unrelated origin": "https://unrelated-" + uuid.NewString()[:8] + ".example/users/someone",
		} {
			env, body := a.build(upd{metadata: valid(), actor: actor})
			a.sendRefused(name, env.ID, body, "", nil, http.StatusForbidden, federation.InboxStatusUnknownActor)
		}
		env, body = a.build(upd{metadata: valid(), keyID: a.actor + "#another-key"})
		a.sendRefused("a signature key that is not the actor's #main-key", env.ID, body, "", nil, http.StatusUnauthorized, federation.InboxStatusUnknownKey)

		// Altered after signing: the candidate verified before the change.
		env, _ = a.build(upd{metadata: valid()})
		if err := env.Verify(a.pub); err != nil {
			t.Fatalf("setup: the candidate does not verify: %v", err)
		}
		env.Extra[federation.MemberAssetMetadata] = meta(sp("Altered after signing"), nil, nil, nil)
		altered, err := env.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		a.sendRefused("metadata altered after the actor signed", env.ID, altered, "", nil, http.StatusUnauthorized, federation.InboxStatusSigInvalid)

		// An object (and its companion) on another origin: ValidateMembers
		// first runs the metadata profile's checks (its prerequisite
		// profiles and the companion's validity, which this consistent
		// companion passes), then the existing base rule for an asset
		// Update's object (#1575, checkPriorActivityObject), whose reason,
		// invalid_member, is kept (§18.5).
		other := a.dst.url + "/assets/" + uuid.NewString()
		env, body = a.build(upd{metadata: valid(), object: other,
			mutate: func(c map[string]json.RawMessage) { b, _ := json.Marshal(other); c["id"] = b }})
		a.sendRefused("an object on another origin", env.ID, body, "", nil, http.StatusBadRequest, federation.InboxStatusInvalidMember)
		// The companion alone on another origin, the object on the source:
		// the archival member does not name the envelope's object.
		env, body = a.build(upd{metadata: valid(),
			mutate: func(c map[string]json.RawMessage) { b, _ := json.Marshal(other); c["id"] = b }})
		a.sendRefused("a companion on another origin", env.ID, body, "", nil, http.StatusBadRequest, federation.InboxStatusArchivalInvalid)

		// The held version with another, self-consistent original.
		otherHash := strings.Repeat("ab", 32)
		env, body = a.build(upd{metadata: valid(), mutate: func(c map[string]json.RawMessage) {
			var files []map[string]any
			if err := json.Unmarshal(c["files"], &files); err != nil || len(files) == 0 {
				t.Fatalf("setup: the companion's files: %v", err)
			}
			for _, f := range files {
				if f["role"] == "original" {
					f["contentUri"] = "cas://" + otherHash
					f["checksums"] = []map[string]string{{"algorithm": "sha256", "value": otherHash}}
				}
			}
			c["files"], _ = json.Marshal(files)
		}})
		a.sendRefusedLate("the held version with another original", env.ID, body, federation.InboxStatusError, "version_conflict")
		a.servedOriginal("at the end")
	})
}

// 3. Stored receipts, total order, flags-only Updates, concurrent admission.
func TestTwoStack_MetadataAdversarial_ReplayAndOrder(t *testing.T) {
	flowDirections(t, func(t *testing.T, g *flowGate) {
		a := newAdvGate(t, g)
		flip := !a.declaredMature()
		storedEnvelope := func(id string) string {
			var s string
			if err := a.dst.pool.QueryRow(t.Context(), `SELECT envelope_json::text FROM federation_inbox WHERE peer_id = $1 AND activity_uri = $2`,
				a.peerAonB, id).Scan(&s); err != nil {
				t.Fatal(err)
			}
			return s
		}

		t.Run("a stored id answers its stored receipt", func(t *testing.T) {
			first, body := a.build(upd{metadata: meta(sp("First"), nil, sp(advRights), nil), mature: bp(flip)})
			rev := a.accepted("the first delivery", first, a.post(body, "", nil))
			a.checkKept("the first delivery", first, rev, true)
			stored := storedEnvelope(first.ID)
			// Each receipt: observe, THEN send, THEN compare row for row.
			receipt := func(name string, b []byte) {
				t.Helper()
				before := a.observe(true)
				r := a.post(b, "", nil)
				if r.err != nil || r.status != http.StatusOK {
					t.Fatalf("%s: %s, want the stored 200 receipt", name, r)
				}
				if storedEnvelope(first.ID) != stored || a.keptFor(first.ID) != 1 {
					t.Fatalf("%s changed the stored activity", name)
				}
				a.same(name, before)
				a.checkKept(name, first, rev, true)
				if a.declaredMature() != flip {
					t.Fatalf("%s changed the flags", name)
				}
			}
			receipt("the exact redelivery", body)
			_, other := a.build(upd{id: first.ID, metadata: meta(sp("Second payload"), nil, nil, nil), mature: bp(!flip)})
			receipt("another payload under the stored id", other)
			_, bare := a.build(upd{id: first.ID, noMeta: true, mature: bp(!flip)})
			receipt("no metadata under the stored id", bare)
		})

		t.Run("the newest signed time wins, whatever arrives first", func(t *testing.T) {
			early, eb := a.build(upd{metadata: meta(sp("Older"), nil, nil, nil)})
			late, lb := a.build(upd{metadata: meta(sp("Newer"), nil, nil, nil)})
			a.accepted("the later first", late, a.post(lb, "", nil))
			a.accepted("the earlier second", early, a.post(eb, "", nil))
			if c := a.current(); c == nil || c.ActivityURI != late.ID {
				t.Fatalf("the current snapshot is %+v, want the later one", c)
			}
			if r := a.post(eb, "", nil); r.status != http.StatusOK {
				t.Fatalf("redelivering the older one: %s", r)
			}
			if c := a.current(); c == nil || c.ActivityURI != late.ID {
				t.Fatal("the older delivery restored its claims")
			}

			// One nanosecond apart in one microsecond; the URI order points
			// the other way.
			base := a.next()
			prefix := a.src.url + "/activities/" + uuid.NewString()
			n0, b0 := a.build(upd{id: prefix + "-z", published: base, metadata: meta(sp("Nanosecond 0"), nil, nil, nil)})
			n1, b1 := a.build(upd{id: prefix + "-a", published: base.Add(time.Nanosecond), metadata: meta(sp("Nanosecond 1"), nil, nil, nil)})
			a.accepted("nanosecond 1 first", n1, a.post(b1, "", nil))
			a.accepted("nanosecond 0 second", n0, a.post(b0, "", nil))
			rev1 := a.revisionOf(n1.ID)
			a.checkKept("the later nanosecond", n1, rev1, true)
		})

		t.Run("equal times choose the bytewise greatest URI", func(t *testing.T) {
			at := a.next()
			prefix := a.src.url + "/activities/" + uuid.NewString() + "-"
			envs := map[string]*federation.Envelope{}
			bodies := map[string][]byte{}
			for _, s := range []string{"tie-B", "tie-a", "tie-_", "tie-A"} {
				md := meta(sp("Tie "+s), sp("Not the winner"), sp(advRights), nil)
				if s == "tie-a" {
					md = meta(nil, nil, nil, nil)
				}
				envs[s], bodies[s] = a.build(upd{id: prefix + s, published: at, metadata: md})
			}
			for _, s := range []string{"tie-_", "tie-a", "tie-A", "tie-B"} {
				a.accepted(s, envs[s], a.post(bodies[s], "", nil))
			}
			a.checkKept("the tie winner", envs["tie-a"], a.revisionOf(envs["tie-a"].ID), true)

			// A newer flags-only Update keeps one revision and no snapshot.
			revs, snaps := a.counts()
			before := a.declaredMature()
			flags, fb := a.build(upd{noMeta: true, mature: bp(!before)})
			rev := a.accepted("a flags-only Update", flags, a.post(fb, "", nil))
			a.checkKept("a flags-only Update", flags, rev, false)
			if r2, s2 := a.counts(); r2 != revs+1 || s2 != snaps {
				t.Fatalf("flags-only: revisions %d->%d, snapshots %d->%d", revs, r2, snaps, s2)
			}
			if c := a.current(); c == nil || c.ActivityURI != envs["tie-a"].ID {
				t.Fatal("a flags-only Update moved the current snapshot")
			}
			if a.declaredMature() != !before {
				t.Fatal("the flags-only Update's flag is not shown")
			}
		})

		t.Run("six simultaneous deliveries are admitted once", func(t *testing.T) {
			env, body := a.build(upd{metadata: meta(sp("Concurrent"), nil, nil, nil)})
			revs, snaps := a.counts()
			start := make(chan struct{})
			results := make(chan wireResp, 6)
			var wg sync.WaitGroup
			for range 6 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					results <- a.post(body, "", nil)
				}()
			}
			close(start)
			joined := make(chan struct{})
			go func() { wg.Wait(); close(joined) }()
			select {
			case <-joined:
			case <-time.After(60 * time.Second):
				t.Fatal("the deliveries did not return")
			}
			close(results)
			fresh, receipts := 0, 0
			for r := range results {
				switch {
				case r.err != nil:
					t.Fatalf("a delivery failed: %v", r.err)
				case r.status == http.StatusAccepted:
					fresh++
				case r.status == http.StatusOK:
					receipts++
				default:
					t.Fatalf("a delivery answered %s", r)
				}
			}
			if fresh != 1 || receipts != 5 {
				t.Fatalf("%d fresh acceptances and %d receipts, want 1 and 5", fresh, receipts)
			}
			if n := a.countOnB(`SELECT count(*) FROM federation_inbox WHERE peer_id = $1 AND activity_uri = $2`, a.peerAonB, env.ID); n != 1 {
				t.Fatalf("%d inbox rows", n)
			}
			rev := a.accepted("the concurrent delivery", env, wireResp{status: http.StatusAccepted})
			a.checkKept("the concurrent delivery", env, rev, true)
			if r2, s2 := a.counts(); r2 != revs+1 || s2 != snaps+1 {
				t.Fatalf("concurrent: revisions %d->%d, snapshots %d->%d", revs, r2, snaps, s2)
			}
		})
		a.servedOriginal("at the end")
	})
}

// revisionOf is the kept revision id of activity id.
func (a *advGate) revisionOf(id string) uuid.UUID {
	a.t.Helper()
	var rev uuid.UUID
	if err := a.dst.pool.QueryRow(a.t.Context(), `SELECT id FROM federation_received_asset_revisions WHERE peer_id = $1 AND activity_uri = $2`,
		a.peerAonB, id).Scan(&rev); err != nil {
		a.t.Fatal(err)
	}
	return rev
}

// 4. The destination's stored profiles, peer state and grant decide.
func TestTwoStack_MetadataAdversarial_Authority(t *testing.T) {
	flowDirections(t, func(t *testing.T, g *flowGate) {
		a := newAdvGate(t, g)
		control, cb := a.build(upd{metadata: meta(sp("Authority control"), nil, sp(advRights), nil)})
		controlRev := a.accepted("the control", control, a.post(cb, "", nil))
		a.checkKept("the control", control, controlRev, true)
		p := a.policy()
		flip := !a.declaredMature()

		// Each case: observe through the API before the fault, observe
		// through read-only SQL (the API may refuse) BEFORE sending, then
		// compare; after the restore the API view must be as before.
		refused := func(name, id string, before advState) {
			t.Helper()
			if a.keptFor(id) != 0 {
				t.Fatalf("%s: a revision of the refused activity was kept", name)
			}
			a.same(name, before)
		}
		reads := func() (detail, original int, d []byte) {
			detail, d = a.getOnB(a.member, "/federation/received-assets/"+a.recordID)
			original, _ = a.getOnB(a.member, "/federation/received-assets/"+a.recordID+"/original")
			return
		}
		restored := func(name string, apiBefore advState) {
			t.Helper()
			p.restored(name)
			a.same(name+", restored", apiBefore)
			a.servedOriginal(name + ", restored")
		}

		t.Run("a missing stored profile refuses new metadata", func(t *testing.T) {
			for _, token := range []string{federation.ProfileAssetMetadata1, federation.ProfileArchival1, federation.ProfileOriginalTransfer1} {
				apiBefore := a.observe(true)
				p.exec(`UPDATE federation_peers SET capabilities = capabilities - $2 WHERE id = $1`, a.peerAonB, token)
				env, body := a.build(upd{metadata: meta(sp("Without "+token), nil, nil, nil), mature: bp(flip)})
				before := a.observe(false)
				r := a.post(body, "", nil)
				switch r.status {
				case http.StatusUnprocessableEntity:
					if r.reason != string(federation.InboxStatusProfileNotNegotiated) {
						t.Fatalf("without %s: %s", token, r)
					}
					if _, _, _, ok := a.inboxRow(env.ID); ok {
						t.Fatalf("without %s: an inbox row was stored", token)
					}
				case http.StatusAccepted:
					if st, rs, le := a.settled(env.ID); st != "rejected" || rs != string(federation.InboxStatusProfileNotNegotiated) {
						t.Fatalf("without %s: settled %s/%s (%q)", token, st, rs, le)
					}
				default:
					t.Fatalf("without %s: %s", token, r)
				}
				t.Logf("%s: without %s the destination answered %s", a.dir, token, r)
				refused("without "+token, env.ID, before)
				restored("without "+token, apiBefore)
			}
		})

		t.Run("a disabled peer is refused and its records are not readable", func(t *testing.T) {
			apiBefore := a.observe(true)
			p.exec(`UPDATE federation_peers SET enabled = FALSE WHERE id = $1`, a.peerAonB)
			env, body := a.build(upd{metadata: meta(sp("While disabled"), nil, nil, nil), mature: bp(flip)})
			before := a.observe(false)
			r := a.post(body, "", nil)
			switch r.status {
			case http.StatusForbidden:
				if r.reason != string(federation.InboxStatusPeerDisabled) {
					t.Fatalf("disabled: %s", r)
				}
				if _, _, _, ok := a.inboxRow(env.ID); ok {
					t.Fatal("disabled: an inbox row was stored")
				}
			case http.StatusAccepted:
				if st, rs, le := a.settled(env.ID); st != "rejected" || rs != string(federation.InboxStatusPeerDisabled) ||
					!strings.Contains(le, "peer_unavailable") {
					t.Fatalf("disabled: settled %s/%s (%q)", st, rs, le)
				}
			default:
				t.Fatalf("disabled: %s", r)
			}
			refused("while disabled", env.ID, before)
			if d, o, _ := reads(); d != http.StatusNotFound || o != http.StatusNotFound {
				t.Fatalf("while disabled: detail %d, original %d", d, o)
			}
			restored("disabled", apiBefore)
		})

		t.Run("a view-only grant keeps old metadata visible and refuses new", func(t *testing.T) {
			apiBefore := a.observe(true)
			p.exec(`UPDATE federation_inbound_grants SET allow_original = FALSE WHERE id = $1`, p.grantID)
			env, body := a.build(upd{metadata: meta(sp("View only"), nil, nil, nil), mature: bp(flip)})
			// The record stays readable under a view grant: observe the API too.
			a.sendRefusedLate("under a view-only grant", env.ID, body, federation.InboxStatusError, "no_inbound_grant")
			d, o, raw := reads()
			var detail struct {
				OriginalAvailable *bool    `json:"original_available"`
				CurrentMetadata   *current `json:"current_metadata"`
			}
			if err := json.Unmarshal(raw, &detail); err != nil {
				t.Fatalf("view only: detail %d does not decode: %v", d, err)
			}
			if d != http.StatusOK || detail.OriginalAvailable == nil || *detail.OriginalAvailable || o != http.StatusNotFound ||
				detail.CurrentMetadata == nil || detail.CurrentMetadata.ActivityURI != control.ID {
				t.Fatalf("view only: detail %d (%s), original %d", d, raw, o)
			}
			restored("view only", apiBefore)
		})

		for _, c := range []struct {
			name, sql string
		}{
			{"an expired grant", `UPDATE federation_inbound_grants SET expires_at = now() - interval '1 hour' WHERE id = $1`},
			{"a revoked grant", `UPDATE federation_inbound_grants SET state = 'ended', end_reason = 'revoked', ended_at = now() WHERE id = $1`},
		} {
			t.Run(c.name+" refuses new metadata; a receipt restores nothing", func(t *testing.T) {
				apiBefore := a.observe(true)
				p.exec(c.sql, p.grantID)
				env, body := a.build(upd{metadata: meta(sp("After "+c.name), nil, nil, nil), mature: bp(flip)})
				before := a.observe(false)
				r := a.post(body, "", nil)
				if r.err != nil || r.status != http.StatusAccepted {
					t.Fatalf("%s: %s", c.name, r)
				}
				if st, rs, le := a.settled(env.ID); st != "rejected" || rs != string(federation.InboxStatusError) || !strings.Contains(le, "no_inbound_grant") {
					t.Fatalf("%s: settled %s/%s (%q)", c.name, st, rs, le)
				}
				refused(c.name, env.ID, before)
				if d, o, _ := reads(); d != http.StatusNotFound || o != http.StatusNotFound {
					t.Fatalf("%s: detail %d, original %d", c.name, d, o)
				}
				// The stored accepted control again, while authority is absent.
				before = a.observe(false)
				if r := a.post(cb, "", nil); r.err != nil || r.status != http.StatusOK {
					t.Fatalf("%s: the stored receipt: %s", c.name, r)
				}
				if a.keptFor(control.ID) != 1 {
					t.Fatalf("%s: the receipt changed the control's revisions", c.name)
				}
				a.same(c.name+" receipt", before)
				if d, o, _ := reads(); d != http.StatusNotFound || o != http.StatusNotFound {
					t.Fatalf("%s: after the receipt, detail %d, original %d", c.name, d, o)
				}
				restored(c.name, apiBefore)
			})
		}

		t.Run("restored authority admits new metadata again", func(t *testing.T) {
			env, body := a.build(upd{metadata: meta(sp("Authority restored"), nil, nil, nil)})
			a.checkKept("after restoring", env, a.accepted("after restoring", env, a.post(body, "", nil)), true)
			a.servedOriginal("at the end")
		})
	})
}
