// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1580: an encrypted envelope's activity members are only visible
// after decryption, so the dispatcher validates them there, before any
// handler runs. A refusal marks the row rejected and applies nothing.

package inbox_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/inbox"
)

// memberDispatcher is newDecryptFx's dispatcher with stub Like and
// Create handlers that count how often they are applied.
func memberDispatcher(t *testing.T, fx *decryptFx, applied *atomic.Int32) *inbox.Dispatcher {
	t.Helper()
	apply := func(context.Context, *federation.Envelope, uuid.UUID, string, inbox.FederationInbox) (inbox.DispatchOutcome, federation.InboxStatus, uuid.UUID, error) {
		applied.Add(1)
		return inbox.OutcomeProcessed, "", uuid.UUID{}, nil
	}
	d := inbox.NewDispatcher(
		inbox.DispatcherConfig{Interval: time.Hour, BatchSize: 100, MaxAttempts: 5},
		inbox.New(fx.pool),
		func(_ context.Context, id uuid.UUID) (inbox.PeerInfo, error) {
			return inbox.PeerInfo{ID: id, InstanceURL: "https://studio-b.example", Enabled: true, Connected: true}, nil
		},
		map[federation.ActivityType]inbox.HandlerFn{federation.ActivityLike: apply, federation.ActivityCreate: apply},
		fx.logger,
	)
	d.SetRawPool(fx.pool)
	d.SetSenderEncKey(func(_ context.Context, actorURI string) ([]byte, int32, error) {
		if actorURI != fx.senderActor {
			return nil, 0, errors.New("synthetic: unknown sender")
		}
		return fx.senderPub, 1, nil
	})
	d.SetRecipientUserRef(func(ctx context.Context, actorURI string) (int64, error) {
		uname := actorURI[strings.LastIndex(actorURI, "/users/")+len("/users/"):]
		var ref int64
		err := fx.pool.QueryRow(ctx, `SELECT ref FROM "user" WHERE username = $1`, uname).Scan(&ref)
		return ref, err
	})
	d.SetAudit(fx.auditRec)
	return d
}

// sealed builds an encrypted envelope of verb whose sealed payload is
// exactly plaintext, stores it as an inbox row and dispatches it.
func dispatchSealed(t *testing.T, verb federation.ActivityType, plaintext string) (inboxRowState, int32) {
	t.Helper()
	fx := newDecryptFx(t)
	var applied atomic.Int32
	d := memberDispatcher(t, fx, &applied)
	recipientPub := fetchUserCurrentPub(t, fx.pool, fx.recipientRef)
	nonce, ct := fx.encryptPayload([]byte(plaintext), recipientPub)
	enc := &federation.EncryptionBlock{
		Algorithm:           federation.EncryptionAlgNaClBoxV1,
		SenderKeyID:         fx.senderActor + "#encryption-key",
		SenderKeyVersion:    1,
		RecipientKeyID:      fx.recipientURI + "#encryption-key",
		RecipientKeyVersion: 1,
		Nonce:               federation.Base64Bytes(nonce),
		Ciphertext:          federation.Base64Bytes(ct),
	}
	envelope, _ := fx.buildEnvelope(verb, nil, enc)
	id, row := fx.insertInbox(envelope, "", verb)
	d.DispatchRow(context.Background(), row)
	return fx.readRow(id), applied.Load()
}

func TestDispatcherMembers_SealedPayloadIsValidated(t *testing.T) {
	cases := []struct {
		name      string
		verb      federation.ActivityType
		plaintext string
		reason    federation.InboxStatus // "" = applied
	}{
		{"like_unknown_member", federation.ActivityLike, `{"post_title":"local only"}`, federation.InboxStatusUnknownField},
		{"like_display_hint", federation.ActivityLike, `{"actorDisplayName":"Alice"}`, federation.InboxStatusUnknownField},
		{"duplicate_member", federation.ActivityCreate, `{"object_type":"Note","content":"a","content":"b"}`, federation.InboxStatusInvalidMember},
		{"comment_without_content", federation.ActivityCreate, `{"object_type":"Note"}`, federation.InboxStatusInvalidMember},
		{"direct_message", federation.ActivityCreate, `{"object_type":"Note","content":"psst","directMessage":true}`, federation.InboxStatusUnsupportedActivity},
		{"post_create", federation.ActivityCreate, `{"object_type":"aa:Post","title":"New","visibility":"public"}`, federation.InboxStatusUnsupportedActivity},
		{"archival_member_unnegotiated", federation.ActivityCreate, `{"aa:asset":{}}`, federation.InboxStatusProfileNotNegotiated},
		{"like_conforming", federation.ActivityLike, `{}`, ""},
		{"comment_conforming", federation.ActivityCreate, `{"object_type":"Note","content":"sealed hello","inReplyTo":"https://studio-a.example/comments/` + uuid.NewString() + `"}`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st, applied := dispatchSealed(t, c.verb, c.plaintext)
			if c.reason == "" {
				if st.status != "processed" || applied != 1 {
					t.Fatalf("conforming sealed payload: status %q, applied %d; want processed once", st.status, applied)
				}
				return
			}
			if applied != 0 {
				t.Errorf("a refused sealed payload was applied %d times", applied)
			}
			if st.status != "rejected" || st.rejectReason == nil || *st.rejectReason != string(c.reason) {
				got := "<nil>"
				if st.rejectReason != nil {
					got = *st.rejectReason
				}
				t.Errorf("row %q / %s, want rejected / %s", st.status, got, c.reason)
			}
		})
	}
}
