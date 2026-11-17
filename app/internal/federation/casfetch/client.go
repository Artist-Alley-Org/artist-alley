// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package casfetch receives a peer's original file (#72, spec v1 §8.5,
// §7.5.7, §17.6) for ingest (#1574).
//
// # Two authorities, checked on every use
//
// Bytes are held only when BOTH say yes, for THIS caller:
//
//   - the receiver's own inbound grant record (InboundGrants; #1571),
//     checked on every fetch and on every reuse of bytes already held;
//   - the origin, asked with a signed HEAD for this exact caller, asset,
//     hash and size, on every fetch and every reuse.
//
// Identical bytes already in local storage grant nothing. Until #1571
// provides the inbound model, production uses FailClosedInboundGrants,
// which refuses everything (no_inbound_grant): receiving is inert.
//
// # State machine (Fetch)
//
//  1. Validate the request: the signer is this instance or one of its
//     actors; class original; the announced tier is not restricted or
//     embargo; size within the cap.
//  2. Authorise, as THIS caller, against CURRENT state: the receiver's
//     peer row re-read (enabled, connected, original-transfer-1, the
//     asset URI on its origin), the inbound grant, the origin's HEAD.
//  3. Local reuse: bytes of this size already held are re-verified
//     against THIS caller's size and checksums, then pinned with THIS
//     caller's own ingest pin. No GET.
//  4. Otherwise claim the download: in process, one caller per key
//     (peer, asset, hash, signer) downloads while the others wait; across
//     workers, the database lease. A caller that waited goes back to 2:
//     it never inherits another caller's result, it authorises itself,
//     and finds the bytes at 3.
//  5. The downloader: signed GET, no redirects, connect / idle / overall
//     deadlines, exactly 200, Content-Length (if any) equal to size,
//     streamed to a temp file through VerifyFileFixity (reads at most
//     size+1 bytes). The lease heartbeat runs from acquisition until the
//     import has committed or failed; a failed or lost renewal cancels
//     the work and nothing is recorded.
//  6. Re-authorise (2) after the download. The download and the import
//     each have their own deadline (Config); the lease-checked temp
//     sweep never removes a file whose lease is live.
//  7. storage.ImportVerified (live-import lock, intent, Put, one
//     transaction for object, variant, ingest pin, intent delete and
//     lease release). Inside that transaction the lease must still be
//     this caller's and the peer row is re-checked FOR SHARE, so a
//     concurrent disable or downgrade either commits first (and this
//     import records nothing) or waits for this commit.
//
// The result carries the ingest pin. Nothing here creates an asset row
// or shows anything to a user: that is #1574.
package casfetch

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/cas"
	"github.com/mscrnt/artist-alley/app/internal/federation/httpsig"
	"github.com/mscrnt/artist-alley/app/internal/federation/peer"
	"github.com/mscrnt/artist-alley/app/internal/storage"
)

// IngestPinType is the pin subject a fetch holds bytes under. It is
// never an asset pin; ingest converts it.
const IngestPinType = "federation_ingest"

// Failure reasons (typed, audited).
const (
	ReasonInvalidRequest     = "invalid_request"
	ReasonPeerUnavailable    = "peer_unavailable"
	ReasonProfile            = "profile_not_negotiated"
	ReasonForeignOrigin      = "foreign_origin"
	ReasonForeignSigner      = "foreign_signer"
	ReasonClassNotAvailable  = "class_not_available"
	ReasonSensitivity        = "sensitivity_refused"
	ReasonTooLarge           = "too_large"
	ReasonNoInboundGrant     = "no_inbound_grant"
	ReasonOriginRefused      = "origin_refused"
	ReasonRedirectRefused    = "redirect_refused"
	ReasonHTTPStatus         = "http_status"
	ReasonLengthMismatch     = "length_mismatch"
	ReasonFixityMismatch     = "fixity_mismatch"
	ReasonTimeout            = "timeout"
	ReasonTransport          = "transport_error"
	ReasonRevokedDuringFetch = "revoked_during_fetch"
	ReasonLeaseLost          = "lease_lost"
	ReasonStorage            = "storage_error"
)

// Error is a typed fetch failure.
type Error struct {
	Reason       string
	OriginReason string // the origin's reason, for origin_refused
	Status       int    // HTTP status, where one was received
	Err          error
}

func (e *Error) Error() string {
	s := "casfetch: " + e.Reason
	if e.OriginReason != "" {
		s += " (" + e.OriginReason + ")"
	}
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}

func (e *Error) Unwrap() error { return e.Err }

func fail(reason string, err error) *Error { return &Error{Reason: reason, Err: err} }

// ReasonOf returns the typed reason of a Fetch error, or "".
func ReasonOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Reason
	}
	return ""
}

// GrantQuery asks the receiver's inbound grant record.
type GrantQuery struct {
	PeerID         uuid.UUID
	SignerKeyID    string
	ActorURI       string // "" for an instance-signed fetch
	OriginAssetURI string
	Class          string
}

// InboundGrants is the receiver's own record of what peers granted it
// (#1571). Allows returns nil only for a live grant from that peer, for
// that asset (or a covering container), applicable to the signer, with
// allow_original for the original class.
type InboundGrants interface {
	Allows(ctx context.Context, q GrantQuery) error
}

// ErrNoInboundGrant is the inbound record's refusal.
var ErrNoInboundGrant = errors.New("casfetch: no live inbound grant")

// FailClosedInboundGrants is the production InboundGrants until #1571
// provides the inbound model: every query is refused.
type FailClosedInboundGrants struct{}

// Allows always refuses.
func (FailClosedInboundGrants) Allows(context.Context, GrantQuery) error {
	return fmt.Errorf("%w: the inbound grant model is not implemented yet (#1571)", ErrNoInboundGrant)
}

// Signer is the local identity a fetch is made as.
type Signer struct {
	KeyID    string
	ActorURI string // "" for the instance key
	sign     func(req *http.Request) error
}

// InstanceSigner signs as this instance (outbox.IdentitySigner or the
// server's deferred signer).
func InstanceSigner(s interface {
	Sign(*http.Request, []byte) error
	KeyID() string
}) Signer {
	return Signer{KeyID: s.KeyID(), sign: func(req *http.Request) error { return s.Sign(req, []byte{}) }}
}

// ActorSigner signs as a local actor with its #1526 key. A key of the
// wrong length makes every request fail, never panic.
func ActorSigner(actorURI string, priv ed25519.PrivateKey) Signer {
	keyID := actorURI + "#main-key"
	return Signer{KeyID: keyID, ActorURI: actorURI, sign: func(req *http.Request) error {
		if len(priv) != ed25519.PrivateKeySize {
			return fmt.Errorf("casfetch: actor key for %s is %d bytes, not an Ed25519 private key", actorURI, len(priv))
		}
		if req.Header.Get("Host") == "" {
			req.Header.Set("Host", req.URL.Host)
		}
		return httpsig.SignAndAttach(req, []byte{}, keyID, priv)
	}}
}

// Peer is what the client needs about a paired peer.
type Peer struct {
	ID           uuid.UUID
	InstanceURL  string
	Enabled      bool
	Connected    bool
	Capabilities []string
}

// PeerLookup resolves a peer by id from the receiver's database.
type PeerLookup interface {
	ByID(ctx context.Context, id uuid.UUID) (Peer, error)
}

// Auditor records federation.cas.* events.
type Auditor interface {
	FederationCAS(ctx context.Context, eventType string, meta map[string]any)
}

// Config bounds every transfer.
type Config struct {
	MaxBytes       int64         // largest file accepted (too_large above it)
	ConnectTimeout time.Duration // TCP + TLS
	IdleTimeout    time.Duration // no progress for this long is a timeout
	MinRate        int64         // bytes/s the overall deadline assumes
	MinOverall     time.Duration // the download (GET) phase: max(MinOverall, size/MinRate)
	MaxOverall     time.Duration // caps each phase
	// The import phase (local re-hash, backend Put, commit) has its own
	// budget, max(MinImport, size/ImportMinRate), capped at MaxOverall,
	// started after the download and its re-authorisation.
	MinImport     time.Duration
	ImportMinRate int64 // bytes/s
	LeaseTTL      time.Duration
	Heartbeat     time.Duration
	WaitPoll      time.Duration // a waiter's first poll interval
	TempDir       string
	Owner         string // this worker, for the lease row
	// LocalBaseURL is this instance's origin; signers must belong to it.
	LocalBaseURL func(ctx context.Context) string
}

// DefaultConfig returns production bounds.
func DefaultConfig() Config {
	host, _ := os.Hostname()
	return Config{
		MaxBytes:       8 << 30,
		ConnectTimeout: 10 * time.Second,
		IdleTimeout:    60 * time.Second,
		MinRate:        1 << 20,
		MinOverall:     60 * time.Second,
		MaxOverall:     2 * time.Hour,
		MinImport:      60 * time.Second,
		ImportMinRate:  4 << 20,
		LeaseTTL:       60 * time.Second,
		Heartbeat:      20 * time.Second,
		WaitPoll:       250 * time.Millisecond,
		Owner:          host + ":" + strconv.Itoa(os.Getpid()),
	}
}

// Deps wires the client.
type Deps struct {
	Pool    *pgxpool.Pool
	Storage *storage.Service
	Peers   PeerLookup
	Grants  InboundGrants
	Audit   Auditor
	Logger  *slog.Logger
	// Transport, if set, is used for every request (tests: a trust
	// root for httptest). Redirects are never followed regardless.
	Transport http.RoundTripper
	Config    Config
}

// Client fetches original files from paired peers.
type Client struct {
	d      Deps
	cfg    Config
	http   *http.Client
	leases leaseStore
	claims claims
	// hbFault, if set, is consulted on every heartbeat (tests only, see
	// export_test.go): a non-nil error is a failed renewal.
	hbFault func() error
	// beforeReusePin, if set, runs after reuse's final re-authorisation
	// and before its pin transaction (tests only).
	beforeReusePin func()
	// afterFinalCheck, if set, runs after the before-commit
	// re-authorisation, before the import's commit (tests only).
	afterFinalCheck func()
}

// claims is the in-process download claim: one downloader per key, the
// others wait for it to finish and then start over on their own.
type claims struct {
	mu sync.Mutex
	m  map[string]chan struct{}
}

// claim returns (nil, true) when the caller now owns the download for
// key, or (done, false) with a channel closed when the owner finishes.
func (c *claims) claim(key string) (<-chan struct{}, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]chan struct{}{}
	}
	if ch, ok := c.m[key]; ok {
		return ch, false
	}
	c.m[key] = make(chan struct{})
	return nil, true
}

func (c *claims) release(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ch, ok := c.m[key]; ok {
		close(ch)
		delete(c.m, key)
	}
}

// New builds a client. Zero Config fields take DefaultConfig values.
func New(d Deps) *Client {
	cfg := d.Config
	def := DefaultConfig()
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = def.MaxBytes
	}
	if cfg.ConnectTimeout <= 0 {
		cfg.ConnectTimeout = def.ConnectTimeout
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = def.IdleTimeout
	}
	if cfg.MinRate <= 0 {
		cfg.MinRate = def.MinRate
	}
	if cfg.MinOverall <= 0 {
		cfg.MinOverall = def.MinOverall
	}
	if cfg.MaxOverall <= 0 {
		cfg.MaxOverall = def.MaxOverall
	}
	if cfg.MinImport <= 0 {
		cfg.MinImport = def.MinImport
	}
	if cfg.ImportMinRate <= 0 {
		cfg.ImportMinRate = def.ImportMinRate
	}
	if cfg.LeaseTTL <= 0 {
		cfg.LeaseTTL = def.LeaseTTL
	}
	if cfg.Heartbeat <= 0 || cfg.Heartbeat >= cfg.LeaseTTL {
		cfg.Heartbeat = cfg.LeaseTTL / 3
	}
	if cfg.WaitPoll <= 0 {
		cfg.WaitPoll = def.WaitPoll
	}
	if cfg.Owner == "" {
		cfg.Owner = def.Owner
	}
	if d.Grants == nil {
		d.Grants = FailClosedInboundGrants{}
	}
	rt := d.Transport
	if rt == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.DialContext = (&net.Dialer{Timeout: cfg.ConnectTimeout, KeepAlive: 30 * time.Second}).DialContext
		tr.TLSHandshakeTimeout = cfg.ConnectTimeout
		tr.ResponseHeaderTimeout = cfg.IdleTimeout
		rt = tr
	}
	return &Client{
		d:   d,
		cfg: cfg,
		http: &http.Client{
			Transport: rt,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		leases: leaseStore{pool: d.Pool, ttl: cfg.LeaseTTL},
	}
}

// Request names one original file to receive.
type Request struct {
	PeerID         uuid.UUID
	OriginAssetURI string // the asset's URI on the origin
	AssetID        uuid.UUID
	SHA256         string
	Size           int64
	ContentType    string
	// Checksums lists additional fixity values from the announcing
	// aa:asset (sha512, md5); sha256 is always checked.
	Checksums []federation.ArchivalChecksum
	// AnnouncedSensitivity is the tier the announcing envelope states;
	// restricted and embargo are refused before anything is fetched.
	AnnouncedSensitivity string
	Class                string // "" means original
	Signer               Signer
}

// Result is what a successful Fetch holds.
type Result struct {
	Hash     string
	Size     int64
	Deduped  bool // bytes were already held; no GET was made
	IngestID uuid.UUID
	Pin      storage.PinRef
}

// Fetch runs the state machine in the package header.
func (c *Client) Fetch(ctx context.Context, req Request) (*Result, error) {
	res, err := c.fetch(ctx, req)
	meta := map[string]any{
		"peer_id": req.PeerID.String(), "asset_id": req.AssetID.String(),
		"hash": req.SHA256, "size": req.Size, "signer": req.Signer.KeyID,
	}
	if err != nil {
		var e *Error
		if errors.As(err, &e) {
			meta["reason"] = e.Reason
			if e.OriginReason != "" {
				meta["origin_reason"] = e.OriginReason
			}
			if e.Status != 0 {
				meta["status"] = e.Status
			}
		}
		meta["error"] = err.Error()
		c.audit(ctx, audit.EventFederationCASFetchFailed, meta)
		return nil, err
	}
	meta["deduped"] = res.Deduped
	meta["ingest_id"] = res.IngestID.String()
	c.audit(ctx, audit.EventFederationCASFetched, meta)
	return res, nil
}

func (c *Client) fetch(ctx context.Context, req Request) (*Result, error) {
	if err := c.validate(ctx, &req); err != nil {
		return nil, err
	}
	// Room for one wait on another holder's whole transfer, then this
	// caller's own; each phase below has its own deadline.
	ctx, cancel := context.WithTimeout(ctx, c.total(req.Size))
	defer cancel()
	key := leaseKey{PeerID: req.PeerID, AssetID: req.AssetID, Hash: req.SHA256, Signer: req.Signer.KeyID}
	poll := c.cfg.WaitPoll
	for {
		// 2: this caller, current state.
		p, err := c.authorize(ctx, req)
		if err != nil {
			return nil, err
		}
		// 3: bytes already held, re-verified for this caller.
		if res, ok, err := c.reuse(ctx, req); err != nil || ok {
			return res, err
		}
		// 4: one downloader per key in this process.
		done, owner := c.claims.claim(key.String())
		if !owner {
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return nil, fail(ReasonTimeout, ctx.Err())
			}
		}
		res, retry, err := c.lead(ctx, req, p, key, &poll)
		c.claims.release(key.String())
		if retry {
			continue
		}
		return res, err
	}
}

// validate checks the request itself; peer state is checked on every
// authorisation (authorize).
func (c *Client) validate(ctx context.Context, req *Request) error {
	if req.Class == "" {
		req.Class = cas.ClassOriginal
	}
	if req.Class != cas.ClassOriginal {
		return fail(ReasonClassNotAvailable, nil)
	}
	if !validHash(req.SHA256) || req.Size < 0 || req.AssetID == uuid.Nil || req.Signer.KeyID == "" || req.Signer.sign == nil {
		return fail(ReasonInvalidRequest, nil)
	}
	if req.Size > c.cfg.MaxBytes {
		return fail(ReasonTooLarge, nil)
	}
	switch req.AnnouncedSensitivity {
	case "", "public", "team":
	default:
		return fail(ReasonSensitivity, nil)
	}
	if req.ContentType == "" {
		req.ContentType = "application/octet-stream"
	}
	req.ContentType = strings.ToLower(req.ContentType)
	u, err := url.Parse(req.OriginAssetURI)
	if err != nil || origin(u) == "" || u.Path != "/assets/"+req.AssetID.String() || u.RawQuery != "" || u.Fragment != "" {
		return fail(ReasonForeignOrigin, nil)
	}
	// The signer is this instance or one of its actors.
	if c.cfg.LocalBaseURL != nil {
		local := originOf(c.cfg.LocalBaseURL(ctx))
		ku, err := url.Parse(req.Signer.KeyID)
		if err != nil || local == "" || origin(ku) != local {
			return fail(ReasonForeignSigner, nil)
		}
	}
	return nil
}

// checkPeer validates the receiver's CURRENT record of the peer for req.
func checkPeer(p Peer, req Request) error {
	if !p.Enabled || !p.Connected {
		return fail(ReasonPeerUnavailable, nil)
	}
	if !contains(p.Capabilities, string(peer.CapOriginalTransfer1)) {
		return fail(ReasonProfile, nil)
	}
	u, _ := url.Parse(req.OriginAssetURI)
	if o := originOf(p.InstanceURL); o == "" || origin(u) != o {
		return fail(ReasonForeignOrigin, nil)
	}
	return nil
}

// authorize re-reads the peer, then asks the inbound record and the
// origin (signed HEAD), all for this caller.
func (c *Client) authorize(ctx context.Context, req Request) (Peer, error) {
	p, err := c.d.Peers.ByID(ctx, req.PeerID)
	if err != nil {
		return Peer{}, fail(ReasonPeerUnavailable, err)
	}
	if err := checkPeer(p, req); err != nil {
		return Peer{}, err
	}
	if err := c.d.Grants.Allows(ctx, GrantQuery{
		PeerID: p.ID, SignerKeyID: req.Signer.KeyID, ActorURI: req.Signer.ActorURI,
		OriginAssetURI: req.OriginAssetURI, Class: req.Class,
	}); err != nil {
		return Peer{}, fail(ReasonNoInboundGrant, err)
	}
	hctx, cancel := context.WithTimeout(ctx, c.cfg.ConnectTimeout+c.cfg.IdleTimeout)
	defer cancel()
	resp, err := c.do(hctx, http.MethodHead, p, req)
	if err != nil {
		return Peer{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Peer{}, refusal(resp)
	}
	return p, nil
}

// reuse pins bytes already held, after re-verifying them against this
// caller's size and checksums. Only reached after authorize said yes.
// The verify read can be long (a large file), so the caller is
// authorised AGAIN after it, and the pin is written in one transaction
// that re-checks the peer row FOR SHARE and the object row: a revoke,
// disable, downgrade or origin change during the read records no pin.
// No transaction is held over the file read or the network.
//
// The receiver's inbound grant (#1571) is asked over the network-free
// InboundGrants interface; when it becomes a database model it must also
// be re-checked inside the pin transaction, like the peer row, to close
// the race between that check and the commit.
func (c *Client) reuse(ctx context.Context, req Request) (*Result, bool, error) {
	obj, err := storage.New(c.d.Pool).FindObject(ctx, req.SHA256)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, fail(ReasonStorage, err)
	}
	if obj.SizeBytes != req.Size {
		return nil, false, nil
	}
	info, err := c.d.Storage.Stat(ctx, req.SHA256, storage.VariantOriginal)
	if err != nil || info == nil || info.Size != req.Size {
		return nil, false, nil
	}
	rc, _, err := c.d.Storage.Download(ctx, req.SHA256, storage.VariantOriginal)
	if err != nil {
		return nil, false, nil
	}
	verr := federation.VerifyFileFixity(c.fileOf(req), rc)
	rc.Close()
	if verr != nil {
		if errors.Is(verr, federation.ErrFixityMismatch) {
			return nil, false, fail(ReasonFixityMismatch, verr)
		}
		return nil, false, fail(ReasonStorage, verr)
	}
	// Again, after the (possibly long) read.
	p, err := c.authorize(ctx, req)
	if err != nil {
		return nil, false, reauthFailure(err)
	}
	if c.beforeReusePin != nil {
		c.beforeReusePin()
	}
	ingest := uuid.New()
	pin := storage.PinRef{SubjectType: IngestPinType, SubjectID: ingest.String()}
	err = pgx.BeginFunc(ctx, c.d.Pool, func(tx pgx.Tx) error {
		if err := checkPeerTx(ctx, tx, p.ID, req); err != nil {
			return err
		}
		// One statement: the object still exists at this size, and it is
		// locked and taken off any pending GC (no lock upgrade, so
		// concurrent pins of one object serialise instead of deadlocking).
		tag, err := tx.Exec(ctx, `UPDATE storage_objects SET gc_eligible_at = NULL
			WHERE hash = $1 AND size_bytes = $2`, req.SHA256, req.Size)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return errObjectGone
		}
		return storage.New(tx).AddPin(ctx, storage.AddPinParams{ObjectHash: req.SHA256, PinSubjectType: pin.SubjectType, PinSubjectID: pin.SubjectID})
	})
	if err != nil {
		var e *Error
		switch {
		case errors.As(err, &e):
			return nil, false, e
		case errors.Is(err, errObjectGone):
			return nil, false, nil // fetch instead
		}
		return nil, false, fail(ReasonStorage, err)
	}
	return &Result{Hash: req.SHA256, Size: req.Size, Deduped: true, IngestID: ingest, Pin: pin}, true, nil
}

var errObjectGone = errors.New("casfetch: held object disappeared before the pin")

// checkPeerTx re-reads the peer row FOR SHARE inside tx and checks it,
// so a concurrent change either commits first or waits for tx.
func checkPeerTx(ctx context.Context, tx pgx.Tx, peerID uuid.UUID, req Request) error {
	var cur Peer
	var status string
	if err := tx.QueryRow(ctx, `SELECT id, instance_url, enabled, status,
		    ARRAY(SELECT jsonb_array_elements_text(capabilities))
		  FROM federation_peers WHERE id = $1 FOR SHARE`, peerID).
		Scan(&cur.ID, &cur.InstanceURL, &cur.Enabled, &status, &cur.Capabilities); err != nil {
		return fail(ReasonPeerUnavailable, err)
	}
	cur.Connected = status == "connected"
	return checkPeer(cur, req)
}

// reauthFailure turns a failed re-authorisation into revoked_during_fetch
// with the cause kept.
func reauthFailure(err error) error {
	var e *Error
	if errors.As(err, &e) {
		origin := e.OriginReason
		if origin == "" {
			origin = e.Reason
		}
		return &Error{Reason: ReasonRevokedDuringFetch, OriginReason: origin, Err: err}
	}
	return fail(ReasonRevokedDuringFetch, err)
}

// fileOf is the listed file this caller expects.
func (c *Client) fileOf(req Request) federation.ArchivalFile {
	size := req.Size
	return federation.ArchivalFile{
		Role:       "original",
		ContentURI: "cas://" + req.SHA256,
		Checksums:  append([]federation.ArchivalChecksum{{Algorithm: "sha256", Value: req.SHA256}}, req.Checksums...),
		Size:       &size,
		MediaType:  mediaType(req.ContentType),
	}
}

// lead takes the database lease and downloads, or, when another worker
// holds the lease, waits for it and asks the caller to start over
// (retry=true).
func (c *Client) lead(ctx context.Context, req Request, p Peer, key leaseKey, poll *time.Duration) (*Result, bool, error) {
	leaseID, ok, err := c.leases.acquire(ctx, key, c.cfg.Owner)
	if err != nil {
		return nil, false, fail(ReasonStorage, err)
	}
	if ok {
		res, err := c.download(ctx, req, p, key, leaseID)
		return res, false, err
	}
	for {
		held, err := c.leases.held(ctx, key)
		if err != nil {
			return nil, false, fail(ReasonStorage, err)
		}
		if !held {
			return nil, true, nil
		}
		select {
		case <-ctx.Done():
			return nil, false, fail(ReasonTimeout, ctx.Err())
		case <-time.After(*poll):
		}
		if *poll < time.Second {
			*poll *= 2
		}
	}
}

// heartbeat renews the lease every Heartbeat until stopped. A failed or
// lost renewal records the loss and cancels the work.
type heartbeat struct {
	stop   context.CancelFunc
	done   chan struct{}
	mu     sync.Mutex
	lost   error
	cancel context.CancelFunc
}

func (c *Client) startHeartbeat(ctx context.Context, key leaseKey, leaseID uuid.UUID, cancelWork context.CancelFunc) *heartbeat {
	hctx, stop := context.WithCancel(context.WithoutCancel(ctx))
	hb := &heartbeat{stop: stop, done: make(chan struct{}), cancel: cancelWork}
	go func() {
		defer close(hb.done)
		t := time.NewTicker(c.cfg.Heartbeat)
		defer t.Stop()
		for {
			select {
			case <-hctx.Done():
				return
			case <-t.C:
			}
			var err error
			if c.hbFault != nil {
				err = c.hbFault()
			}
			if err == nil {
				var ok bool
				ok, err = c.leases.heartbeat(hctx, key, leaseID)
				if err == nil && !ok {
					err = errors.New("lease no longer held")
				}
			}
			if err != nil {
				if hctx.Err() != nil {
					return // stopped while renewing
				}
				hb.mu.Lock()
				hb.lost = err
				hb.mu.Unlock()
				cancelWork()
				return
			}
		}
	}()
	return hb
}

func (hb *heartbeat) finish() { hb.stop(); <-hb.done }

func (hb *heartbeat) err() error {
	hb.mu.Lock()
	defer hb.mu.Unlock()
	return hb.lost
}

// errLeaseNotHeld aborts the import transaction when this caller no
// longer owns the lease.
var errLeaseNotHeld = errors.New("casfetch: lease no longer held at commit")

// download is the downloader's part, holding the lease: GET, verify,
// re-authorise, import. The heartbeat covers all of it.
func (c *Client) download(ctx context.Context, req Request, p Peer, key leaseKey, leaseID uuid.UUID) (res *Result, err error) {
	committed := false
	workCtx, cancelWork := context.WithCancel(ctx)
	defer cancelWork()
	hb := c.startHeartbeat(ctx, key, leaseID, cancelWork)
	defer func() {
		hb.finish()
		if !committed {
			_ = c.leases.release(context.WithoutCancel(ctx), key, leaseID)
		}
		if err != nil && hb.err() != nil {
			err = &Error{Reason: ReasonLeaseLost, Err: errors.Join(hb.err(), err)}
		}
	}()

	tmpDir := c.cfg.TempDir
	if tmpDir == "" {
		tmpDir = os.TempDir()
	}
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return nil, fail(ReasonStorage, err)
	}
	tmpPath := filepath.Join(tmpDir, tempPrefix+leaseID.String()+tempSuffix)
	defer os.Remove(tmpPath)

	dctx, cancelD := context.WithTimeout(workCtx, c.overall(req.Size))
	err = c.get(dctx, req, p, tmpPath)
	cancelD()
	if err != nil {
		return nil, err
	}
	// 6: re-authorise against current state before anything is recorded.
	if _, err := c.authorize(workCtx, req); err != nil {
		return nil, reauthFailure(err)
	}
	if hb.err() != nil {
		return nil, fail(ReasonLeaseLost, hb.err())
	}
	// 7: import; the lease and the peer are checked inside the commit.
	ingest := uuid.New()
	pin := storage.PinRef{SubjectType: IngestPinType, SubjectID: ingest.String()}
	// The import phase has its own budget (re-hash, Put, commit).
	ictx, cancelI := context.WithTimeout(workCtx, c.importBudget(req.Size))
	defer cancelI()
	ir, err := c.d.Storage.ImportVerified(ictx, storage.ImportRequest{
		TempPath:       tmpPath,
		Hash:           req.SHA256,
		Size:           req.Size,
		ContentType:    req.ContentType,
		OriginServerID: p.ID,
		IntentID:       leaseID,
		Pin:            pin,
		// After the (possibly long) Put, before the commit: this caller
		// is authorised once more, over the network, outside any
		// transaction, with the lease and the import lock still held.
		// The HEAD and the commit are separate steps: a revoke at the
		// origin after this HEAD is not seen by this import (no recall,
		// the same boundary as the origin's own request-start check).
		BeforeCommit: func(ctx context.Context) error {
			if err := hb.err(); err != nil {
				return fail(ReasonLeaseLost, err)
			}
			if _, err := c.authorize(ctx, req); err != nil {
				return reauthFailure(err)
			}
			if c.afterFinalCheck != nil {
				c.afterFinalCheck()
			}
			return nil
		},
		InTx: func(ctx context.Context, tx pgx.Tx) error {
			// The lease is still this caller's (locked until commit).
			var held uuid.UUID
			if err := tx.QueryRow(ctx, `SELECT lease_id FROM federation_cas_fetch_leases
				WHERE peer_id = $1 AND asset_id = $2 AND object_hash = $3 AND signer = $4 AND lease_id = $5
				FOR UPDATE`, key.PeerID, key.AssetID, key.Hash, key.Signer, leaseID).Scan(&held); err != nil {
				return errLeaseNotHeld
			}
			// The peer as it is now, held until commit. (#1571's
			// database-backed inbound grant belongs here too.)
			if err := checkPeerTx(ctx, tx, p.ID, req); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, releaseSQL, key.PeerID, key.AssetID, key.Hash, key.Signer, leaseID)
			return err
		},
	})
	if err != nil {
		var e *Error
		switch {
		case errors.As(err, &e):
			return nil, e
		case errors.Is(err, errLeaseNotHeld):
			return nil, fail(ReasonLeaseLost, err)
		case errors.Is(err, storage.ErrImportMismatch):
			return nil, fail(ReasonFixityMismatch, err)
		}
		return nil, fail(ReasonStorage, err)
	}
	committed = true
	return &Result{Hash: ir.Hash, Size: ir.Size, Deduped: ir.Deduped, IngestID: ingest, Pin: pin}, nil
}

const (
	tempPrefix = "aa-casfetch-"
	tempSuffix = ".part"
)

// get streams the body to tmpPath through the fixity check.
func (c *Client) get(ctx context.Context, req Request, p Peer, tmpPath string) error {
	gctx, cancel := context.WithCancel(ctx)
	defer cancel()
	resp, err := c.do(gctx, http.MethodGet, p, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return refusal(resp)
	}
	if resp.ContentLength >= 0 && resp.ContentLength != req.Size {
		return &Error{Reason: ReasonLengthMismatch, Status: resp.StatusCode,
			Err: fmt.Errorf("Content-Length %d, expected %d", resp.ContentLength, req.Size)}
	}
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fail(ReasonStorage, err)
	}
	body := &idleReader{r: resp.Body, timeout: c.cfg.IdleTimeout, cancel: cancel}
	body.arm()
	verr := federation.VerifyFileFixity(c.fileOf(req), io.TeeReader(body, f))
	body.stop()
	cerr := f.Sync()
	if err := f.Close(); cerr == nil {
		cerr = err
	}
	switch {
	case verr != nil && body.timedOut():
		return fail(ReasonTimeout, verr)
	case verr != nil && errors.Is(verr, federation.ErrFixityMismatch):
		return fail(ReasonFixityMismatch, verr)
	case verr != nil && errors.Is(ctx.Err(), context.DeadlineExceeded):
		return fail(ReasonTimeout, verr)
	case verr != nil:
		return fail(ReasonTransport, verr)
	case cerr != nil:
		return fail(ReasonStorage, cerr)
	}
	return nil
}

// do sends a signed HEAD or GET for req to the peer's registered origin.
func (c *Client) do(ctx context.Context, method string, p Peer, req Request) (*http.Response, error) {
	target := strings.TrimRight(p.InstanceURL, "/") + cas.FilePath(req.AssetID, req.Class, req.SHA256) +
		"?size=" + strconv.FormatInt(req.Size, 10)
	hr, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		return nil, fail(ReasonInvalidRequest, err)
	}
	hr.Header.Set("Accept", "application/octet-stream")
	if err := req.Signer.sign(hr); err != nil {
		return nil, fail(ReasonInvalidRequest, err)
	}
	resp, err := c.http.Do(hr)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
			return nil, fail(ReasonTimeout, err)
		}
		return nil, fail(ReasonTransport, err)
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		resp.Body.Close()
		return nil, &Error{Reason: ReasonRedirectRefused, Status: resp.StatusCode}
	}
	return resp, nil
}

// refusal reads the origin's {"reason"} (bounded) from a non-200.
func refusal(resp *http.Response) error {
	var body struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 4<<10)).Decode(&body)
	reason := sanitize(body.Reason)
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
		return &Error{Reason: ReasonOriginRefused, OriginReason: reason, Status: resp.StatusCode}
	}
	return &Error{Reason: ReasonHTTPStatus, OriginReason: reason, Status: resp.StatusCode}
}

func sanitize(s string) string {
	if len(s) > 64 {
		s = s[:64]
	}
	for _, r := range s {
		if !(r == '_' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			return "unrecognised"
		}
	}
	return s
}

// overall is the transfer deadline: max(MinOverall, size/MinRate),
// capped at MaxOverall.
func (c *Client) overall(size int64) time.Duration {
	d := time.Duration(size/c.cfg.MinRate) * time.Second
	if d < c.cfg.MinOverall {
		d = c.cfg.MinOverall
	}
	if d > c.cfg.MaxOverall {
		d = c.cfg.MaxOverall
	}
	return d
}

// importBudget is the import phase deadline: max(MinImport,
// size/ImportMinRate), capped at MaxOverall.
func (c *Client) importBudget(size int64) time.Duration {
	d := time.Duration(size/c.cfg.ImportMinRate) * time.Second
	if d < c.cfg.MinImport {
		d = c.cfg.MinImport
	}
	if d > c.cfg.MaxOverall {
		d = c.cfg.MaxOverall
	}
	return d
}

// phase is one complete transfer: download plus import.
func (c *Client) phase(size int64) time.Duration {
	return c.overall(size) + c.importBudget(size)
}

// total bounds one Fetch: one wait on another holder's whole transfer,
// then this caller's own, plus lease slack.
func (c *Client) total(size int64) time.Duration {
	return 2*c.phase(size) + 3*c.cfg.LeaseTTL
}

func (c *Client) audit(ctx context.Context, event string, meta map[string]any) {
	if c.d.Audit != nil {
		c.d.Audit.FederationCAS(context.WithoutCancel(ctx), event, meta)
	}
}

// Reconcile is the periodic cleanup: import intents left by a crash,
// ingest pins ingest never converted, expired leases and stale temp
// files.
func (c *Client) Reconcile(ctx context.Context, intentStaleAfter, ingestPinTTL time.Duration) error {
	if _, err := c.d.Storage.ReconcileImportIntents(ctx, intentStaleAfter, 500); err != nil {
		return err
	}
	if _, err := c.d.Storage.ReleaseStalePins(ctx, IngestPinType, ingestPinTTL, 500); err != nil {
		return err
	}
	if _, err := c.leases.deleteExpired(ctx, c.cfg.LeaseTTL); err != nil {
		return err
	}
	return c.sweepTemp(ctx, c.phase(c.cfg.MaxBytes)+c.cfg.LeaseTTL)
}

// sweepTemp removes temp files older than olderThan whose lease is not
// live. A file is named by its lease id; while that lease is held (the
// heartbeat keeps it until the import has finished) the file is in use
// and is kept, whatever its age.
func (c *Client) sweepTemp(ctx context.Context, olderThan time.Duration) error {
	return sweepTempFiles(c.cfg.TempDir, olderThan, func(leaseID uuid.UUID) bool {
		var live bool
		err := c.d.Pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM federation_cas_fetch_leases WHERE lease_id = $1 AND expires_at > now())`,
			leaseID).Scan(&live)
		return err != nil || live // on a lookup error, keep the file
	})
}

// SweepTemp removes this package's temp files older than olderThan,
// without a lease check (for use when no client exists).
func SweepTemp(dir string, olderThan time.Duration) error {
	return sweepTempFiles(dir, olderThan, nil)
}

func sweepTempFiles(dir string, olderThan time.Duration, live func(uuid.UUID) bool) error {
	if dir == "" {
		dir = os.TempDir()
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	cutoff := time.Now().Add(-olderThan)
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasPrefix(n, tempPrefix) || !strings.HasSuffix(n, tempSuffix) {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		if live != nil {
			id, err := uuid.Parse(strings.TrimSuffix(strings.TrimPrefix(n, tempPrefix), tempSuffix))
			if err == nil && live(id) {
				continue
			}
		}
		_ = os.Remove(filepath.Join(dir, n))
	}
	return nil
}

// idleReader cancels the request when no bytes arrive for timeout.
type idleReader struct {
	r       io.Reader
	timeout time.Duration
	cancel  context.CancelFunc
	mu      sync.Mutex
	t       *time.Timer
	fired   bool
}

func (i *idleReader) arm() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.t = time.AfterFunc(i.timeout, func() {
		i.mu.Lock()
		i.fired = true
		i.mu.Unlock()
		i.cancel()
	})
}

func (i *idleReader) Read(p []byte) (int, error) {
	n, err := i.r.Read(p)
	if n > 0 {
		i.mu.Lock()
		if i.t != nil && !i.fired {
			i.t.Reset(i.timeout)
		}
		i.mu.Unlock()
	}
	return n, err
}

func (i *idleReader) stop() {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.t != nil {
		i.t.Stop()
	}
}

func (i *idleReader) timedOut() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.fired
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func validHash(h string) bool {
	if len(h) != 64 {
		return false
	}
	for _, r := range h {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func origin(u *url.URL) string {
	if u == nil || u.Scheme == "" || u.Host == "" || u.User != nil {
		return ""
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host)
}

func originOf(raw string) string {
	u, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil || u.Path != "" {
		return ""
	}
	return origin(u)
}

// mediaType returns a §17.2.2-valid media type (lowercase type/subtype
// without parameters), falling back to application/octet-stream.
func mediaType(ct string) string {
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	ct = strings.TrimSpace(strings.ToLower(ct))
	if ct == "" || !strings.Contains(ct, "/") {
		return "application/octet-stream"
	}
	return ct
}
