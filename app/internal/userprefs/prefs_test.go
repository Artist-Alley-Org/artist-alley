// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package userprefs

import (
	"encoding/json"
	"testing"
)

// ValidatePreferences must reject anything that could survive a
// hand-crafted PATCH from a sloppy client and pollute the DB. The
// load-bearing properties:
//   - Unknown event types are refused (we don't want garbage keys
//     piling up in user_preferences.notification_channels).
//   - Unknown channel names are refused (same reasoning; also we
//     don't want a writer crash later when it tries to dispatch).
//   - Duplicate channels in a single event's list are refused
//     (preserves "list = set" invariant).
//   - Empty event list is the "deliver nothing for this event"
//     escape valve and MUST validate.
//   - Empty prefs object (no overrides) MUST validate — fresh users.

func TestValidatePreferences_AllowsEmpty(t *testing.T) {
	if err := ValidatePreferences(Preferences{}); err != nil {
		t.Errorf("empty prefs must validate, got %v", err)
	}
}

func TestValidatePreferences_AllowsKnownEventsAndChannels(t *testing.T) {
	p := Preferences{
		NotificationChannels: NotificationChannels{
			EventCommentOnMyPost: {ChannelInApp, ChannelEmail},
			EventNewFollower:     {ChannelInApp},
			EventLicenseExpired:  {}, // explicit mute — valid
		},
	}
	if err := ValidatePreferences(p); err != nil {
		t.Errorf("expected valid, got %v", err)
	}
}

func TestValidatePreferences_RejectsUnknownEvent(t *testing.T) {
	p := Preferences{
		NotificationChannels: NotificationChannels{
			"some_made_up_event": {ChannelInApp},
		},
	}
	if err := ValidatePreferences(p); err == nil {
		t.Error("expected error for unknown event type, got nil")
	}
}

func TestValidatePreferences_RejectsUnknownChannel(t *testing.T) {
	p := Preferences{
		NotificationChannels: NotificationChannels{
			EventCommentOnMyPost: {"telegram"},
		},
	}
	if err := ValidatePreferences(p); err == nil {
		t.Error("expected error for unknown channel, got nil")
	}
}

func TestValidatePreferences_RejectsDuplicateChannel(t *testing.T) {
	p := Preferences{
		NotificationChannels: NotificationChannels{
			EventCommentOnMyPost: {ChannelInApp, ChannelInApp},
		},
	}
	if err := ValidatePreferences(p); err == nil {
		t.Error("expected error for duplicate channel, got nil")
	}
}

// ChannelsFor resolves the effective channel list a notification
// writer will use. Three cases the writer relies on:
//   - Unset key → system default.
//   - Explicit empty array → empty (the "mute" semantic).
//   - Explicit non-empty array → as-is.
//
// A regression that flipped "explicit empty" to "fall back to
// default" would silently re-enable notifications a user had muted.

func TestChannelsFor_FallsBackToSystemDefault(t *testing.T) {
	p := &Preferences{NotificationChannels: NotificationChannels{}}
	got := p.ChannelsFor(EventCommentOnMyPost)
	want := SystemDefaultChannels(EventCommentOnMyPost)
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (default)", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestChannelsFor_ExplicitEmptyIsMute(t *testing.T) {
	p := &Preferences{
		NotificationChannels: NotificationChannels{
			EventDirectMessageReceived: {},
		},
	}
	got := p.ChannelsFor(EventDirectMessageReceived)
	if len(got) != 0 {
		t.Errorf("explicit empty must mean mute, got %v", got)
	}
}

func TestChannelsFor_ExplicitOverridesDefault(t *testing.T) {
	// Override "comment on my post" to email-only — the system
	// default is in-app only, so this isn't subsumed by defaults.
	p := &Preferences{
		NotificationChannels: NotificationChannels{
			EventCommentOnMyPost: {ChannelEmail},
		},
	}
	got := p.ChannelsFor(EventCommentOnMyPost)
	if len(got) != 1 || got[0] != ChannelEmail {
		t.Errorf("override ignored — got %v, want [email]", got)
	}
}

func TestChannelsFor_NilReceiverSafe(t *testing.T) {
	var p *Preferences
	got := p.ChannelsFor(EventCommentOnMyPost)
	want := SystemDefaultChannels(EventCommentOnMyPost)
	if len(got) != len(want) {
		t.Errorf("nil receiver must fall back to default, got %v", got)
	}
}

// UnmarshalPreferencesRow is the bridge between the sqlc-generated
// raw []byte columns and the typed Preferences struct. Empty bytes
// must produce a zero-value struct (not an error) because that's
// what the first-visit-no-row case looks like upstream.
func TestUnmarshalPreferencesRow_EmptyBytesProduceZero(t *testing.T) {
	p, err := UnmarshalPreferencesRow(nil, nil, nil)
	if err != nil {
		t.Fatalf("empty bytes should not error, got %v", err)
	}
	if p.NotificationChannels == nil {
		t.Error("NotificationChannels should be a non-nil empty map after unmarshal")
	}
}

func TestUnmarshalPreferencesRow_RoundTrip(t *testing.T) {
	want := Preferences{
		NotificationChannels: NotificationChannels{
			EventCommentOnMyPost: {ChannelInApp, ChannelEmail},
		},
		DefaultViews: DefaultViews{
			HomeTab:      "following",
			BrowseLayout: "masonry",
		},
	}
	channelsJSON, _ := MarshalNotificationChannels(want.NotificationChannels)
	viewsJSON, _ := MarshalDefaultViews(want.DefaultViews)
	got, err := UnmarshalPreferencesRow(channelsJSON, viewsJSON, nil)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.DefaultViews.HomeTab != want.DefaultViews.HomeTab {
		t.Errorf("HomeTab mismatch: %q vs %q", got.DefaultViews.HomeTab, want.DefaultViews.HomeTab)
	}
	if len(got.NotificationChannels[EventCommentOnMyPost]) != 2 {
		t.Errorf("channel list mismatch: %v", got.NotificationChannels[EventCommentOnMyPost])
	}
}

// MarshalDefaultViews must use omitempty so empty fields don't ship
// over the wire as "". The frontend treats "" as "no preference,"
// but explicit-empty in JSONB on disk and absent-field-on-wire are
// the same semantic — verify the marshaler keeps it small.
func TestMarshalDefaultViews_OmitsEmpty(t *testing.T) {
	out, err := MarshalDefaultViews(DefaultViews{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(out) != "{}" {
		t.Errorf("empty views should marshal to {}, got %s", out)
	}
	// And with one set field, only that field is present.
	out, _ = MarshalDefaultViews(DefaultViews{HomeTab: "following"})
	var got map[string]string
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	if len(got) != 1 || got["home_tab"] != "following" {
		t.Errorf("expected only home_tab, got %v", got)
	}
}

func TestCadenceFor_DefaultsImmediate(t *testing.T) {
	p := Preferences{EmailCadence: EmailCadences{EventMentionOfMe: CadenceDaily}}
	if got := p.CadenceFor(EventMentionOfMe); got != CadenceDaily {
		t.Fatalf("explicit cadence: got %q want daily", got)
	}
	if got := p.CadenceFor(EventCommentOnMyPost); got != CadenceImmediate {
		t.Fatalf("unset cadence must default immediate, got %q", got)
	}
}

func TestValidatePreferences_RejectsBadCadence(t *testing.T) {
	p := Preferences{EmailCadence: EmailCadences{EventMentionOfMe: "fortnightly"}}
	if err := ValidatePreferences(p); err == nil {
		t.Fatal("expected unknown cadence to be rejected")
	}
	p2 := Preferences{EmailCadence: EmailCadences{"not_a_real_event": CadenceDaily}}
	if err := ValidatePreferences(p2); err == nil {
		t.Fatal("expected unknown event type in email_cadence to be rejected")
	}
}
