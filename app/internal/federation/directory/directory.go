// Package directory implements the subscriber side of the
// federation directory protocol per
// docs/spec/federation-directory/v1.md. The reference SERVER
// lives at cmd/aa-directory/; this package is what the artist-
// alley app does when it subscribes to one or more directories.
//
// # What's here
//
//   - Registry: CRUD + cache around the federation_directories
//     and federation_directory_entries tables.
//   - Client: HTTP client for fetching /v1/operator + /v1/listing,
//     verifying signatures, and persisting entries.
//   - Poller: background goroutine that walks enabled directories
//     and triggers Client.Poll on each whose interval has elapsed.
//
// # Caching
//
// Two tiers per the spec:
//
//   1. **Per-directory snapshot cache.** ListEntries hits an in-
//      process LRU keyed by directory_id; cold reads fall through
//      to Postgres. Invalidated by Poll on every successful
//      refresh (the snapshot represents a whole directory; any
//      change drops it).
//
//   2. **On-disk persistence.** federation_directory_entries
//      survives directory outages — admin still sees discovered
//      peers when the directory's down. Per the spec
//      §"Local caching", cached entries don't auto-expire; they
//      stay until the next successful poll overwrites them or
//      the directory is unsubscribed.

package directory

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/federation"
)

const (
	cacheDomainEntries = "directory.entries"

	// httpTimeout: a directory operator that's slower than this
	// is unhealthy; subscriber moves on rather than blocking.
	httpTimeout = 20 * time.Second

	// maxBodyBytes: response body limit. The reference server's
	// /v1/listing payload for a few hundred entries is ~50 KB;
	// 4 MB is a generous ceiling that still defends against an
	// OOM attack from a hostile directory.
	maxBodyBytes = 4 * 1024 * 1024
)

// PollStatus mirrors federation_directories.last_poll_status CHECK
// per migration 00053.
type PollStatus string

const (
	PollStatusNeverPolled          PollStatus = "never_polled"
	PollStatusOK                   PollStatus = "ok"
	PollStatusUnreachable          PollStatus = "unreachable"
	PollStatusSignatureFailed      PollStatus = "signature_failed"
	PollStatusMalformed            PollStatus = "malformed"
	PollStatusSpecVersionMismatch  PollStatus = "spec_version_mismatch"
)

// Valid reports whether s is in the closed catalogue.
func (s PollStatus) Valid() bool {
	switch s {
	case PollStatusNeverPolled, PollStatusOK, PollStatusUnreachable,
		PollStatusSignatureFailed, PollStatusMalformed, PollStatusSpecVersionMismatch:
		return true
	}
	return false
}

// Directory is the in-memory shape of one subscription.
type Directory struct {
	ID                   uuid.UUID
	URL                  string
	OperatorName         string
	OperatorPublicKey    string
	OperatorFingerprint  string
	OperatorContact      string
	SubscribedAt         pgtype.Timestamptz
	SubscribedByUserRef  int64
	Enabled              bool
	LastPolledAt         pgtype.Timestamptz
	LastPollStatus       PollStatus
	LastPollError        string
	PollIntervalSeconds  int32
	Notes                string
}

// Entry is the in-memory shape of one cached directory entry.
type Entry struct {
	ID                 uuid.UUID
	DirectoryID        uuid.UUID
	InstanceURL        string
	DisplayName        string
	InstancePublicKey  string
	Fingerprint        string
	Region             string
	Description        string
	Tags               []string
	VerifiedAt         pgtype.Timestamptz
	VerifiedVia        string
	ListingID          string
	CachedAt           pgtype.Timestamptz
}

// Errors callers may distinguish on.
var (
	ErrDirectoryNotFound = errors.New("directory: not found")
	ErrAlreadySubscribed = errors.New("directory: already subscribed to this URL")
	ErrInvalidURL        = errors.New("directory: URL must be https:// with no trailing slash")
	ErrOperatorFetch     = errors.New("directory: could not fetch /v1/operator")
	ErrSpecMismatch      = errors.New("directory: spec_version mismatch (only aa-directory/v1 supported)")
)

// Registry owns the CRUD + cache for directory subscriptions.
type Registry struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger

	entries *cache.Cache[entriesSnapshot]
}

// entriesSnapshot is the cached list of entries for ONE directory.
// Stored under the directory_id as cache key so each directory's
// snapshot lives in its own LRU slot.
type entriesSnapshot struct {
	DirectoryID uuid.UUID
	Entries     []Entry
}

// NewRegistry wires the package. cacheReg may be nil for tests
// that don't want the LISTEN goroutine.
func NewRegistry(pool *pgxpool.Pool, logger *slog.Logger, cacheReg *cache.Registry) *Registry {
	r := &Registry{Pool: pool, Logger: logger}
	if cacheReg != nil {
		// 200 directories is generous; subscribing to >50 would be
		// unusual. The LRU caps the working set; eviction just means
		// the next ListEntries refetches from Postgres.
		r.entries = cache.Register[entriesSnapshot](cacheReg, cacheDomainEntries, 200)
	}
	return r
}

// --- registry: read --------------------------------------------------------

// List returns all subscribed directories ordered newest-first.
func (r *Registry) List(ctx context.Context) ([]Directory, error) {
	rows, err := New(r.Pool).ListDirectories(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Directory, len(rows))
	for i, row := range rows {
		out[i] = *rowToDirectory(row)
	}
	return out, nil
}

// ByID looks up one subscription.
func (r *Registry) ByID(ctx context.Context, id uuid.UUID) (*Directory, error) {
	row, err := New(r.Pool).GetDirectoryByID(ctx, pgtype.UUID{Bytes: id, Valid: true})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrDirectoryNotFound
		}
		return nil, err
	}
	return rowToDirectory(row), nil
}

// ListEntries returns the cached entries for one directory.
// Cache-first; cold misses hit Postgres + populate.
func (r *Registry) ListEntries(ctx context.Context, directoryID uuid.UUID, limit int32) ([]Entry, error) {
	if r.entries != nil {
		if hit, ok := r.entries.Get(directoryID.String()); ok {
			out := hit.Entries
			if limit > 0 && int32(len(out)) > limit {
				out = out[:limit]
			}
			return append([]Entry(nil), out...), nil
		}
	}
	if limit <= 0 {
		limit = 500
	}
	rows, err := New(r.Pool).ListDirectoryEntries(ctx, ListDirectoryEntriesParams{
		DirectoryID: pgtype.UUID{Bytes: directoryID, Valid: true},
		Limit:       limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]Entry, len(rows))
	for i, row := range rows {
		out[i] = *rowToEntry(row)
	}
	if r.entries != nil {
		r.entries.Add(directoryID.String(), entriesSnapshot{
			DirectoryID: directoryID,
			Entries:     append([]Entry(nil), out...),
		})
	}
	return out, nil
}

// --- registry: write -------------------------------------------------------

// Subscribe persists a new directory subscription. URL is
// normalized + de-duplicated.
func (r *Registry) Subscribe(ctx context.Context, in SubscribeInput) (*Directory, error) {
	url, err := normalizeDirectoryURL(in.URL)
	if err != nil {
		return nil, err
	}
	if existing, err := New(r.Pool).GetDirectoryByURL(ctx, url); err == nil {
		_ = existing
		return nil, ErrAlreadySubscribed
	}
	row, err := New(r.Pool).InsertDirectory(ctx, InsertDirectoryParams{
		DirectoryUrl:         url,
		OperatorName:         in.OperatorName,
		OperatorPublicKey:    in.OperatorPublicKey,
		OperatorFingerprint:  in.OperatorFingerprint,
		OperatorContact:      in.OperatorContact,
		SubscribedByUserRef:  in.SubscribedByUserRef,
		Notes:                in.Notes,
	})
	if err != nil {
		return nil, err
	}
	return rowToDirectory(row), nil
}

// SubscribeInput is the typed argument to Registry.Subscribe.
type SubscribeInput struct {
	URL                 string
	OperatorName        string
	OperatorPublicKey   string // PEM
	OperatorFingerprint string
	OperatorContact     string
	SubscribedByUserRef int64
	Notes               string
}

// SetEnabled flips the enabled flag.
func (r *Registry) SetEnabled(ctx context.Context, id uuid.UUID, enabled bool) (*Directory, error) {
	row, err := New(r.Pool).SetDirectoryEnabled(ctx, SetDirectoryEnabledParams{
		ID:      pgtype.UUID{Bytes: id, Valid: true},
		Enabled: enabled,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrDirectoryNotFound
		}
		return nil, err
	}
	r.invalidateEntries(id)
	return rowToDirectory(row), nil
}

// Unsubscribe removes the directory + cascades to entries.
func (r *Registry) Unsubscribe(ctx context.Context, id uuid.UUID) error {
	if err := New(r.Pool).DeleteDirectory(ctx, pgtype.UUID{Bytes: id, Valid: true}); err != nil {
		return err
	}
	r.invalidateEntries(id)
	return nil
}

// recordPollOutcome is called by the polling worker on success or
// failure to update last_poll_status + last_polled_at.
func (r *Registry) recordPollOutcome(ctx context.Context, id uuid.UUID, status PollStatus, errMsg string) error {
	if !status.Valid() {
		return fmt.Errorf("directory: invalid poll status %q", status)
	}
	return New(r.Pool).UpdateDirectoryPollOutcome(ctx, UpdateDirectoryPollOutcomeParams{
		ID:              pgtype.UUID{Bytes: id, Valid: true},
		LastPollStatus:  string(status),
		LastPollError:   errMsg,
	})
}

// persistEntries replaces this directory's cached entries with
// the freshly-fetched set in one transaction.
func (r *Registry) persistEntries(ctx context.Context, directoryID uuid.UUID, entries []Entry) error {
	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := New(tx)
	keepURLs := make([]string, len(entries))
	for i, e := range entries {
		keepURLs[i] = e.InstanceURL
		tagsJSON, _ := json.Marshal(e.Tags)
		if err := q.UpsertDirectoryEntry(ctx, UpsertDirectoryEntryParams{
			DirectoryID:        pgtype.UUID{Bytes: directoryID, Valid: true},
			InstanceUrl:        e.InstanceURL,
			DisplayName:        e.DisplayName,
			InstancePublicKey:  e.InstancePublicKey,
			Fingerprint:        e.Fingerprint,
			Region:             e.Region,
			Description:        e.Description,
			Tags:               tagsJSON,
			VerifiedAt:         e.VerifiedAt,
			VerifiedVia:        e.VerifiedVia,
			ListingID:          e.ListingID,
		}); err != nil {
			return err
		}
	}
	keepJSON, _ := json.Marshal(keepURLs)
	if err := q.DeleteDirectoryEntriesNotIn(ctx, DeleteDirectoryEntriesNotInParams{
		DirectoryID: pgtype.UUID{Bytes: directoryID, Valid: true},
		KeepUrls:    keepJSON,
	}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	r.invalidateEntries(directoryID)
	return nil
}

func (r *Registry) invalidateEntries(directoryID uuid.UUID) {
	if r.entries == nil {
		return
	}
	if err := r.entries.Invalidate(context.Background(), directoryID.String()); err != nil && r.Logger != nil {
		r.Logger.LogAttrs(context.Background(), slog.LevelWarn, "directory.cache.invalidate.error",
			slog.String("err", err.Error()),
		)
	}
}

// --- HTTP client + Poller --------------------------------------------------

// Client fetches /v1/operator + /v1/listing responses, verifies
// signatures, persists entries. Stateless per-call; safe for
// concurrent use.
type Client struct {
	HTTP   *http.Client
	Logger *slog.Logger
}

// NewClient wires the default HTTP client + logger.
func NewClient(logger *slog.Logger) *Client {
	return &Client{
		HTTP:   &http.Client{Timeout: httpTimeout},
		Logger: logger,
	}
}

// FetchOperator hits GET /v1/operator. Called once at subscribe
// time to capture the operator's pubkey + fingerprint; the admin
// reviews the fingerprint out-of-band before confirming the
// subscription.
type OperatorDoc struct {
	Name         string `json:"name"`
	OperatorURL  string `json:"operator_url"`
	Contact      string `json:"contact"`
	SpecVersion  string `json:"spec_version"`
	PublicKeyPEM string `json:"public_key_pem"`
	Fingerprint  string `json:"fingerprint"`
}

func (c *Client) FetchOperator(ctx context.Context, directoryURL string) (*OperatorDoc, error) {
	url := strings.TrimRight(directoryURL, "/") + "/v1/operator"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrOperatorFetch, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("%w: %s: %s", ErrOperatorFetch, resp.Status, string(body))
	}
	var op OperatorDoc
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBodyBytes)).Decode(&op); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrOperatorFetch, err)
	}
	if op.SpecVersion != "aa-directory/v1" {
		return nil, fmt.Errorf("%w: got %q", ErrSpecMismatch, op.SpecVersion)
	}
	return &op, nil
}

// listingResponse is the on-wire /v1/listing shape, mirrored
// from cmd/aa-directory/server.go. Kept local so the subscriber
// doesn't import directory-server types.
//
// Entries is intentionally json.RawMessage (not []listingEntry).
// The directory server signs the RFC 8785 canonical form of the
// emitted `entries` JSON; if we round-tripped through a typed
// struct here, omitempty drift on optional fields would break
// the signature verify. Canonicalizing the raw bytes is the only
// way to be deterministic across server + subscriber.
type listingResponse struct {
	Directory  listingDirHeader `json:"directory"`
	Entries    json.RawMessage  `json:"entries"`
	NextCursor *string          `json:"next_cursor"`
}

type listingDirHeader struct {
	Name         string    `json:"name"`
	OperatorURL  string    `json:"operator_url"`
	SpecVersion  string    `json:"spec_version"`
	GeneratedAt  time.Time `json:"generated_at"`
	Signature    string    `json:"signature"`
	PublicKeyPEM string    `json:"public_key_pem"`
}

type listingEntry struct {
	InstanceURL          string    `json:"instance_url"`
	DisplayName          string    `json:"display_name"`
	InstancePublicKeyPEM string    `json:"instance_public_key_pem"`
	Fingerprint          string    `json:"fingerprint"`
	Region               string    `json:"region,omitempty"`
	Description          string    `json:"description,omitempty"`
	Tags                 []string  `json:"tags,omitempty"`
	VerifiedAt           time.Time `json:"verified_at"`
	VerifiedVia          string    `json:"verified_via"`
	ListingID            string    `json:"listing_id"`
}

// Poll fetches /v1/listing, verifies the signature against the
// PINNED operator pubkey (NOT the one in the response — we
// trust the pinned one captured at subscribe time), then
// persists entries + records the outcome on the directory row.
//
// On any failure the cached entries are LEFT IN PLACE per the
// spec's local-caching rule — a directory outage shouldn't
// drop discovered peers.
func (c *Client) Poll(ctx context.Context, reg *Registry, d *Directory) error {
	url := strings.TrimRight(d.URL, "/") + "/v1/listing"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		_ = reg.recordPollOutcome(ctx, d.ID, PollStatusMalformed, err.Error())
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		_ = reg.recordPollOutcome(ctx, d.ID, PollStatusUnreachable, err.Error())
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		msg := fmt.Sprintf("HTTP %s: %s", resp.Status, strings.TrimSpace(string(body)))
		_ = reg.recordPollOutcome(ctx, d.ID, PollStatusUnreachable, msg)
		return errors.New(msg)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		_ = reg.recordPollOutcome(ctx, d.ID, PollStatusUnreachable, err.Error())
		return err
	}
	var lr listingResponse
	if err := json.Unmarshal(raw, &lr); err != nil {
		_ = reg.recordPollOutcome(ctx, d.ID, PollStatusMalformed, err.Error())
		return err
	}
	if lr.Directory.SpecVersion != "aa-directory/v1" {
		msg := "spec_version=" + lr.Directory.SpecVersion
		_ = reg.recordPollOutcome(ctx, d.ID, PollStatusSpecVersionMismatch, msg)
		return errors.New(msg)
	}
	// Verify signature against the PINNED key.
	pinnedPub, err := federation.PublicKeyFromPEM([]byte(d.OperatorPublicKey))
	if err != nil {
		_ = reg.recordPollOutcome(ctx, d.ID, PollStatusSignatureFailed,
			"pinned operator key unparseable: "+err.Error())
		return err
	}
	// Canonicalize the RAW bytes of the entries field — see the
	// note on listingResponse.Entries about why we don't round-trip
	// through a typed struct here.
	canonical, err := federation.Canonicalize(lr.Entries)
	if err != nil {
		_ = reg.recordPollOutcome(ctx, d.ID, PollStatusMalformed, err.Error())
		return err
	}
	sig, err := base64.StdEncoding.DecodeString(lr.Directory.Signature)
	if err != nil {
		_ = reg.recordPollOutcome(ctx, d.ID, PollStatusSignatureFailed,
			"signature not base64: "+err.Error())
		return err
	}
	if err := federation.Verify(pinnedPub, canonical, sig); err != nil {
		_ = reg.recordPollOutcome(ctx, d.ID, PollStatusSignatureFailed, err.Error())
		return err
	}
	// All checks passed — parse the entries into typed shape for
	// persistence.
	var parsed []listingEntry
	if err := json.Unmarshal(lr.Entries, &parsed); err != nil {
		_ = reg.recordPollOutcome(ctx, d.ID, PollStatusMalformed, err.Error())
		return err
	}
	entries := make([]Entry, len(parsed))
	for i, e := range parsed {
		entries[i] = Entry{
			DirectoryID:        d.ID,
			InstanceURL:        e.InstanceURL,
			DisplayName:        e.DisplayName,
			InstancePublicKey:  e.InstancePublicKeyPEM,
			Fingerprint:        e.Fingerprint,
			Region:             e.Region,
			Description:        e.Description,
			Tags:               e.Tags,
			VerifiedAt:         pgtype.Timestamptz{Time: e.VerifiedAt, Valid: true},
			VerifiedVia:        e.VerifiedVia,
			ListingID:          e.ListingID,
		}
	}
	if err := reg.persistEntries(ctx, d.ID, entries); err != nil {
		_ = reg.recordPollOutcome(ctx, d.ID, PollStatusMalformed, "persist: "+err.Error())
		return err
	}
	_ = reg.recordPollOutcome(ctx, d.ID, PollStatusOK, "")
	return nil
}

// Poller is the background goroutine that walks enabled
// directories on a tick and polls those whose interval has
// elapsed. Stop via context cancellation.
type Poller struct {
	registry *Registry
	client   *Client
	logger   *slog.Logger
	tick     time.Duration

	stopped chan struct{}
	once    sync.Once
}

// NewPoller wires the background poller. tick is the cadence at
// which it scans for due directories (NOT the per-directory poll
// interval — that's read from the DB row).
func NewPoller(reg *Registry, client *Client, logger *slog.Logger, tick time.Duration) *Poller {
	if tick <= 0 {
		tick = 5 * time.Minute
	}
	return &Poller{
		registry: reg,
		client:   client,
		logger:   logger,
		tick:     tick,
		stopped:  make(chan struct{}),
	}
}

// Run blocks until ctx is cancelled. Safe to call once per
// process.
func (p *Poller) Run(ctx context.Context) {
	defer p.once.Do(func() { close(p.stopped) })
	t := time.NewTicker(p.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.sweep(ctx)
		}
	}
}

// sweep polls all due directories serially. Serial keeps
// implementation simple + load on each peer's directory
// bounded. For larger fleets we'd switch to a worker pool.
func (p *Poller) sweep(ctx context.Context) {
	dirs, err := p.registry.List(ctx)
	if err != nil {
		p.logger.Warn("directory.poller.list", slog.String("err", err.Error()))
		return
	}
	now := time.Now().UTC()
	for i := range dirs {
		d := dirs[i]
		if !d.Enabled {
			continue
		}
		due := !d.LastPolledAt.Valid ||
			now.Sub(d.LastPolledAt.Time) >= time.Duration(d.PollIntervalSeconds)*time.Second
		if !due {
			continue
		}
		if err := p.client.Poll(ctx, p.registry, &d); err != nil {
			p.logger.Warn("directory.poll.failed",
				slog.String("directory_url", d.URL),
				slog.String("err", err.Error()),
			)
		}
	}
}

// --- helpers ---------------------------------------------------------------

func rowToDirectory(r FederationDirectory) *Directory {
	return &Directory{
		ID:                  uuid.UUID(r.ID.Bytes),
		URL:                 r.DirectoryUrl,
		OperatorName:        r.OperatorName,
		OperatorPublicKey:   r.OperatorPublicKey,
		OperatorFingerprint: r.OperatorFingerprint,
		OperatorContact:     r.OperatorContact,
		SubscribedAt:        r.SubscribedAt,
		SubscribedByUserRef: r.SubscribedByUserRef,
		Enabled:             r.Enabled,
		LastPolledAt:        r.LastPolledAt,
		LastPollStatus:      PollStatus(r.LastPollStatus),
		LastPollError:       r.LastPollError,
		PollIntervalSeconds: r.PollIntervalSeconds,
		Notes:               r.Notes,
	}
}

func rowToEntry(r FederationDirectoryEntry) *Entry {
	var tags []string
	_ = json.Unmarshal(r.Tags, &tags)
	return &Entry{
		ID:                uuid.UUID(r.ID.Bytes),
		DirectoryID:       uuid.UUID(r.DirectoryID.Bytes),
		InstanceURL:       r.InstanceUrl,
		DisplayName:       r.DisplayName,
		InstancePublicKey: r.InstancePublicKey,
		Fingerprint:       r.Fingerprint,
		Region:            r.Region,
		Description:       r.Description,
		Tags:              tags,
		VerifiedAt:        r.VerifiedAt,
		VerifiedVia:       r.VerifiedVia,
		ListingID:         r.ListingID,
		CachedAt:          r.CachedAt,
	}
}

// normalizeDirectoryURL validates + cleans an https URL. Same
// rules as peer.normalizeInstanceURL: https only, no path, strip
// trailing slash.
func normalizeDirectoryURL(in string) (string, error) {
	s := strings.TrimSpace(in)
	if !strings.HasPrefix(s, "https://") {
		return "", ErrInvalidURL
	}
	rest := strings.TrimPrefix(s, "https://")
	rest = strings.TrimRight(rest, "/")
	if rest == "" {
		return "", ErrInvalidURL
	}
	if strings.Contains(rest, "/") {
		return "", ErrInvalidURL
	}
	return "https://" + rest, nil
}

// _ keeps the bytes import live for the future inline-canonical
// path; remove when no longer needed.
var _ = bytes.NewReader
