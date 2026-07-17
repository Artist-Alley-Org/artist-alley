// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Publish-side flow — Phase 1.22.B-c-bis. The mirror of the
// subscriber: the operator wants THIS instance to appear in the
// directory's /v1/listing for other instances to discover.
//
// State machine (federation.PublishStatus):
//
//   not_published
//       │ admin clicks "publish here"
//       ▼
//   pending_dns        ←──── challenge issued by directory; admin
//       │                    must add the TXT record we show them
//       │ admin clicks "register" (claiming DNS is live)
//       ▼
//   pending_register   ←──── /v1/register POST in flight
//       │
//   ┌───┴───┐
//   ▼       ▼
//  listed  failed     ←──── any step can land in failed; admin
//                           reads publish_last_error + retries
//
// Note on tokens: the directory issues a single-use token bound
// to our instance URL with a 1-hour expiry (per the spec). We
// persist it on the row so the admin UI shows the same record
// across page reloads. If a register attempt fails and the
// token has expired, we re-issue a fresh challenge automatically
// on the next attempt.

package directory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/identity"
)

// PublishMetadata is the operator-chosen subset of the directory
// entry — what shows up on /v1/listing once we're listed.
type PublishMetadata struct {
	DisplayName string
	Region      string
	Description string
	Tags        []string
}

// Errors callers may distinguish on.
var (
	ErrPublishNotPending   = errors.New("publish: directory not in pending_dns; request a challenge first")
	ErrPublishNoToken      = errors.New("publish: no pending token on this directory")
	ErrPublishTokenExpired = errors.New("publish: token expired; request a fresh challenge")
)

// challengeResponse mirrors what /v1/challenge returns.
type challengeResponse struct {
	Token       string    `json:"token"`
	RecordName  string    `json:"record_name"`
	RecordValue string    `json:"record_value"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// registerResponse mirrors the listing the directory returns on
// successful /v1/register.
type registerResponse struct {
	ListingID string `json:"listing_id"`
}

// RequestChallenge POSTs /v1/challenge to the directory and
// persists the issued token + record on the directory row so the
// admin UI can show it.
//
// Returns the updated Directory (with status=pending_dns +
// publish_record_name + publish_record_value populated).
func (c *Client) RequestChallenge(
	ctx context.Context,
	reg *Registry,
	d *Directory,
	instanceURL string,
) (*Directory, error) {
	url := strings.TrimRight(d.URL, "/") + "/v1/challenge"
	body, _ := json.Marshal(map[string]string{"instance_url": instanceURL})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("publish.RequestChallenge: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("publish.RequestChallenge: HTTP %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	var ch challengeResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBodyBytes)).Decode(&ch); err != nil {
		return nil, fmt.Errorf("publish.RequestChallenge decode: %w", err)
	}
	if ch.Token == "" {
		return nil, errors.New("publish.RequestChallenge: directory returned empty token")
	}
	return reg.setPublishChallenge(ctx, d.ID, ch.Token, ch.ExpiresAt, ch.RecordName, ch.RecordValue)
}

// RegisterListing POSTs /v1/register with the previously-issued
// token + the operator-chosen metadata. Persists the result on
// the directory row (status=listed + listing_id on success;
// status=failed + publish_last_error on failure).
//
// Caller MUST ensure d.PublishStatus == pending_dns and
// d.PublishPendingToken is non-empty before calling — the
// registry returns ErrPublishNotPending / ErrPublishNoToken
// otherwise.
func (c *Client) RegisterListing(
	ctx context.Context,
	reg *Registry,
	d *Directory,
	idMgr *identity.Manager,
	instanceURL string,
	meta PublishMetadata,
) (*Directory, error) {
	if d.PublishStatus != federation.PublishStatusPendingDNS {
		return nil, ErrPublishNotPending
	}
	if d.PublishPendingToken == "" {
		return nil, ErrPublishNoToken
	}
	if d.PublishTokenExpiresAt.Valid && time.Now().UTC().After(d.PublishTokenExpiresAt.Time) {
		return nil, ErrPublishTokenExpired
	}
	id, err := idMgr.Get()
	if err != nil {
		return nil, fmt.Errorf("publish.RegisterListing: %w", err)
	}

	url := strings.TrimRight(d.URL, "/") + "/v1/register"
	payload := map[string]any{
		"instance_url":            instanceURL,
		"display_name":            meta.DisplayName,
		"instance_public_key_pem": string(id.PublicKeyPEM()),
		"region":                  meta.Region,
		"description":             meta.Description,
		"tags":                    meta.Tags,
		"dns_txt_token":           d.PublishPendingToken,
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		updated, _ := reg.setPublishFailed(ctx, d.ID, err.Error())
		return updated, fmt.Errorf("publish.RegisterListing: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if resp.StatusCode == http.StatusAccepted {
		// 202 = DNS not yet propagated. Keep status=pending_dns
		// + record the wait so the admin sees a clear "DNS not
		// visible yet — try again in a minute" message.
		updated, _ := reg.setPublishFailed(ctx, d.ID,
			fmt.Sprintf("DNS-TXT not yet propagated per directory: %s", strings.TrimSpace(string(raw))))
		// Status got flipped to 'failed' by setPublishFailed; flip
		// it back to pending_dns explicitly so the admin can retry
		// the register without re-doing the DNS step.
		if updated != nil {
			updated, _ = reg.setPublishStatus(ctx, d.ID, federation.PublishStatusPendingDNS)
		}
		return updated, errors.New(strings.TrimSpace(string(raw)))
	}
	if resp.StatusCode != http.StatusCreated {
		msg := fmt.Sprintf("HTTP %s: %s", resp.Status, strings.TrimSpace(string(raw)))
		updated, _ := reg.setPublishFailed(ctx, d.ID, msg)
		return updated, errors.New(msg)
	}
	// 201 — listed. Extract listing_id if present.
	var rr registerResponse
	_ = json.Unmarshal(raw, &rr)
	return reg.setPublishListed(ctx, d.ID, rr.ListingID)
}

// --- registry methods (publish-side) ------------------------------------

// SetPublishMetadata persists the operator-chosen display_name +
// region + description + tags WITHOUT changing status. Called
// from the admin form's "save metadata" path so values pre-fill
// next time + survive challenge expiry.
func (r *Registry) SetPublishMetadata(ctx context.Context, id uuid.UUID, m PublishMetadata) (*Directory, error) {
	tagsJSON, _ := json.Marshal(m.Tags)
	row, err := New(r.Pool).SetDirectoryPublishMetadata(ctx, SetDirectoryPublishMetadataParams{
		ID:                 pgtype.UUID{Bytes: id, Valid: true},
		PublishDisplayName: m.DisplayName,
		PublishRegion:      m.Region,
		PublishDescription: m.Description,
		PublishTags:        tagsJSON,
	})
	if err != nil {
		return nil, err
	}
	return rowToDirectory(row), nil
}

func (r *Registry) setPublishChallenge(ctx context.Context, id uuid.UUID, token string, expires time.Time, recName, recValue string) (*Directory, error) {
	row, err := New(r.Pool).SetDirectoryPublishChallenge(ctx, SetDirectoryPublishChallengeParams{
		ID:                    pgtype.UUID{Bytes: id, Valid: true},
		PublishPendingToken:   token,
		PublishTokenExpiresAt: pgtype.Timestamptz{Time: expires, Valid: true},
		PublishRecordName:     recName,
		PublishRecordValue:    recValue,
	})
	if err != nil {
		return nil, err
	}
	return rowToDirectory(row), nil
}

func (r *Registry) setPublishListed(ctx context.Context, id uuid.UUID, listingID string) (*Directory, error) {
	row, err := New(r.Pool).SetDirectoryPublishListed(ctx, SetDirectoryPublishListedParams{
		ID:               pgtype.UUID{Bytes: id, Valid: true},
		PublishListingID: listingID,
	})
	if err != nil {
		return nil, err
	}
	return rowToDirectory(row), nil
}

func (r *Registry) setPublishFailed(ctx context.Context, id uuid.UUID, errMsg string) (*Directory, error) {
	row, err := New(r.Pool).SetDirectoryPublishFailed(ctx, SetDirectoryPublishFailedParams{
		ID:               pgtype.UUID{Bytes: id, Valid: true},
		PublishLastError: errMsg,
	})
	if err != nil {
		return nil, err
	}
	return rowToDirectory(row), nil
}

// setPublishStatus is the bare flip used in the 202-retry path
// of RegisterListing. Not a public method because direct
// status-only flips can violate the state machine; only call
// from publish.go for the documented edge cases.
func (r *Registry) setPublishStatus(ctx context.Context, id uuid.UUID, status federation.PublishStatus) (*Directory, error) {
	if !status.Valid() {
		return nil, fmt.Errorf("publish.setPublishStatus: invalid status %q", status)
	}
	// Inline UPDATE — kept here rather than as a sqlc query
	// because it's the ONLY caller and exposing a public
	// SetPublishStatus query would invite misuse from the admin
	// HTTP layer.
	tag, err := r.Pool.Exec(ctx,
		`UPDATE federation_directories SET publish_status = $1, updated_at = NOW() WHERE id = $2`,
		string(status), id,
	)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrDirectoryNotFound
	}
	return r.ByID(ctx, id)
}
