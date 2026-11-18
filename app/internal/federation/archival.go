// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Profile archival-1 (docs/spec/federation/v1.md §17; #1513).
//
// Pure, side-effect-free validators for the three archival-1 envelope
// members and the receiver's byte-stream fixity check. Nothing in the
// reference implementation emits these members (WireMembers projects
// none of them), and the profile is not advertised (peer.CapArchival1 is
// deliberately absent from KnownCapabilities). Since #1580 the inbox
// calls ValidateArchivalMembers through ValidateMembers, before storing
// and again after decryption, so every peer's archival-1 member or
// aa:Provenance is refused with profile_not_negotiated. The byte fetch
// that feeds VerifyFileFixity is #72, and applying a received asset or
// tombstone is #1574 / #1575; they reuse these validators.
//
// Every validator is strict: unknown keys are rejected at every level
// (§3.2), so a member either matches archival-1 exactly or fails with
// ErrArchivalInvalid.

package federation

import (
	"bytes"
	"crypto/md5" //nolint:gosec // MD5 is accepted only as additional legacy fixity metadata (§17.2.3), never as the required checksum
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// ProfileArchival1 is the profile token of §17, exchanged as a
// capability in the handshake's supported_capabilities (§16).
const ProfileArchival1 = "archival-1"

// The three top-level envelope members archival-1 defines (§17.1).
const (
	MemberArchivalAsset     = "aa:asset"
	MemberArchivalTombstone = "aa:tombstone"
	MemberArchivalEvent     = "aa:event"
	// MemberArchivalAnnotation is a W3C Web Annotation on aa:Annotation
	// (§17.5).
	MemberArchivalAnnotation = "aa:annotation"
)

// Bounds (§17.2).
const (
	ArchivalMaxFiles          = 256
	ArchivalMaxChecksums      = 3
	ArchivalMaxIdentifiers    = 16
	ArchivalMaxAgents         = 8
	ArchivalMaxFilenameBytes  = 255
	ArchivalMaxShortText      = 256
	ArchivalMaxLongText       = 4096
	ArchivalMaxListEntries    = 64
	ArchivalMaxSafeInteger    = 9007199254740991 // 2^53 - 1, the largest exact JSON integer
	archivalMaxURILength      = 2048
	archivalMaxMediaTypeBytes = 255
)

var (
	// ErrArchivalInvalid: an archival-1 member does not match §17.
	// Maps to inbox status archival_invalid.
	ErrArchivalInvalid = errors.New("federation: archival-1 member invalid")
	// ErrProfileNotNegotiated: a profile member (archival-1,
	// original-transfer-1) from (or to) a peer whose negotiated
	// capabilities lack that profile (§16.3), or, on the sending side,
	// a profile-gated activity (share-lifecycle-1) for such a peer.
	// Maps to inbox status profile_not_negotiated; the wrapping error
	// names the profile.
	ErrProfileNotNegotiated = errors.New("federation: profile_not_negotiated")
	// ErrFixityMismatch: received bytes do not match the listed
	// size or a listed checksum (§17.6). Maps to fixity_mismatch.
	ErrFixityMismatch = errors.New("federation: received bytes do not match the listed fixity")
)

func invalid(path, format string, args ...any) error {
	return fmt.Errorf("%w: %s: %s", ErrArchivalInvalid, path, fmt.Sprintf(format, args...))
}

// --- types --------------------------------------------------------------

// ArchivalAsset is the aa:asset member (§17.2).
type ArchivalAsset struct {
	ID                string               `json:"id"`
	Version           ArchivalVersion      `json:"version"`
	Files             []ArchivalFile       `json:"files"`
	Sensitivity       string               `json:"sensitivity"`
	Mature            *bool                `json:"mature"`
	AIProvenance      string               `json:"aiProvenance,omitempty"`
	Rights            string               `json:"rights,omitempty"`
	RequiredStatement *ArchivalLabelValue  `json:"requiredStatement,omitempty"`
	Identifiers       []ArchivalIdentifier `json:"identifiers,omitempty"`
	AccessionNumber   string               `json:"accessionNumber,omitempty"`
	Description       *ArchivalDescription `json:"description,omitempty"`
	IIIFManifest      string               `json:"iiifManifest,omitempty"`
}

// ArchivalVersion identifies one immutable version (§17.2.1).
type ArchivalVersion struct {
	ID          string  `json:"id"`
	Number      int64   `json:"number"`
	Predecessor *string `json:"predecessor"`
	Created     string  `json:"created"`
}

// ArchivalFile is one entry of the file set (§17.2.2).
type ArchivalFile struct {
	Role             string                  `json:"role"`
	ContentURI       string                  `json:"contentUri"`
	Checksums        []ArchivalChecksum      `json:"checksums"`
	Size             *int64                  `json:"size"`
	MediaType        string                  `json:"mediaType"`
	Identification   *ArchivalIdentification `json:"identification,omitempty"`
	OriginalFilename string                  `json:"originalFilename,omitempty"`
}

// ArchivalChecksum is one fixity value (§17.2.3).
type ArchivalChecksum struct {
	Algorithm string `json:"algorithm"`
	Value     string `json:"value"`
}

// ArchivalIdentification records a format identification (§17.2.4).
type ArchivalIdentification struct {
	PUID string       `json:"puid"`
	Tool ArchivalTool `json:"tool"`
}

// ArchivalTool names the software that produced a claim.
type ArchivalTool struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ArchivalLabelValue is an IIIF-style requiredStatement: language
// tag to one or more strings, for both label and value.
type ArchivalLabelValue struct {
	Label map[string][]string `json:"label"`
	Value map[string][]string `json:"value"`
}

// ArchivalIdentifier is a persistent identifier (§17.2.5).
type ArchivalIdentifier struct {
	Scheme string `json:"scheme"`
	Value  string `json:"value"`
}

// ArchivalDescription is the DCMI-terms subset (§17.2.6).
type ArchivalDescription struct {
	Title       string   `json:"title,omitempty"`
	Description string   `json:"description,omitempty"`
	Creator     []string `json:"creator,omitempty"`
	Subject     []string `json:"subject,omitempty"`
	Date        string   `json:"date,omitempty"`
	Language    string   `json:"language,omitempty"`
}

// ArchivalTombstone is the aa:tombstone member (§17.3).
type ArchivalTombstone struct {
	ID          string                  `json:"id"`
	FormerType  string                  `json:"formerType"`
	Deleted     string                  `json:"deleted"`
	Reason      string                  `json:"reason,omitempty"`
	LastVersion string                  `json:"lastVersion"`
	Files       []ArchivalTombstoneFile `json:"files"`
}

// ArchivalTombstoneFile keeps the identity and fixity of a withdrawn
// file (§17.3).
type ArchivalTombstoneFile struct {
	Role       string             `json:"role"`
	ContentURI string             `json:"contentUri"`
	Checksums  []ArchivalChecksum `json:"checksums"`
}

// ArchivalEvent is the aa:event member of aa:Provenance, a PREMIS 3
// event (§17.4).
type ArchivalEvent struct {
	ID            string               `json:"id"`
	EventType     string               `json:"eventType"`
	DateTime      string               `json:"dateTime"`
	Detail        string               `json:"detail,omitempty"`
	Outcome       string               `json:"outcome"`
	OutcomeDetail string               `json:"outcomeDetail,omitempty"`
	Object        ArchivalEventObject  `json:"object"`
	Agents        []ArchivalEventAgent `json:"agents"`
}

// ArchivalEventObject links an event to what it is about.
type ArchivalEventObject struct {
	Asset      string `json:"asset"`
	Version    string `json:"version,omitempty"`
	ContentURI string `json:"contentUri,omitempty"`
}

// ArchivalAnnotation is the aa:annotation member: a W3C Web
// Annotation whose target is an IIIF canvas, optionally narrowed by a
// Media Fragments region or time (§17.5).
type ArchivalAnnotation struct {
	Type       string                   `json:"type"`
	Motivation string                   `json:"motivation"`
	Body       ArchivalAnnotationBody   `json:"body"`
	Target     ArchivalAnnotationTarget `json:"target"`
}

// ArchivalAnnotationBody is a plain-text TextualBody.
type ArchivalAnnotationBody struct {
	Type     string `json:"type"`
	Value    string `json:"value"`
	Format   string `json:"format"`
	Language string `json:"language,omitempty"`
}

// ArchivalAnnotationTarget is a SpecificResource on a canvas.
type ArchivalAnnotationTarget struct {
	Source   string                      `json:"source"`
	Selector *ArchivalAnnotationSelector `json:"selector,omitempty"`
}

// ArchivalAnnotationSelector is a FragmentSelector conforming to
// Media Fragments: xywh= (canvas pixels) or t= (seconds).
type ArchivalAnnotationSelector struct {
	Type       string `json:"type"`
	ConformsTo string `json:"conformsTo"`
	Value      string `json:"value"`
}

// ArchivalEventAgent is a person (actor URI) or software (§17.4).
type ArchivalEventAgent struct {
	Role     string        `json:"role"`
	Actor    string        `json:"actor,omitempty"`
	Software *ArchivalTool `json:"software,omitempty"`
}

// --- closed vocabularies (§17) -------------------------------------------

var (
	archivalRoles        = map[string]struct{}{"original": {}, "preservation": {}, "access": {}}
	archivalSensitivity  = map[string]struct{}{"public": {}, "team": {}, "restricted": {}, "embargo": {}}
	archivalAIProvenance = map[string]struct{}{"none": {}, "assisted": {}, "generated": {}}
	archivalIDSchemes    = map[string]struct{}{"ark": {}, "doi": {}, "handle": {}, "local": {}}
	// Algorithm -> hex digest length. sha256 is REQUIRED; md5 is
	// legacy, additional only.
	archivalAlgorithms = map[string]int{"sha256": 64, "sha512": 128, "md5": 32}
	// PREMIS event types this profile carries, by token; each maps to
	// the LoC Preservation Events Controlled Vocabulary (2017) term with
	// the same label; the mapping is by label, not by identifier URI
	// (§17.4 table).
	ArchivalEventTypes = map[string]string{
		"ingestion":             "ingestion",
		"fixity-check":          "fixity check",
		"format-identification": "format identification",
		"virus-check":           "virus check",
		"migration":             "migration",
		"replication":           "replication",
		"transfer":              "transfer",
		"deaccession":           "deaccession",
	}
	archivalOutcomes    = map[string]struct{}{"success": {}, "failure": {}, "warning": {}}
	archivalMotivations = map[string]struct{}{"commenting": {}, "highlighting": {}, "describing": {}, "tagging": {}}
	xywhPattern         = regexp.MustCompile(`^xywh=(0|[1-9][0-9]{0,8}),(0|[1-9][0-9]{0,8}),([1-9][0-9]{0,8}),([1-9][0-9]{0,8})$`)
	timePattern         = regexp.MustCompile(`^t=(0|[1-9][0-9]{0,8})(\.[0-9]{1,3})?(,(0|[1-9][0-9]{0,8})(\.[0-9]{1,3})?)?$`)
	archivalAgentRoles  = map[string]struct{}{"implementer": {}, "executing-program": {}, "authorizer": {}}

	casURIPattern      = regexp.MustCompile(`^cas://([0-9a-f]{64})(\?[^#\s]*)?$`)
	lowerHexPattern    = regexp.MustCompile(`^[0-9a-f]+$`)
	mediaTypePattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9!#$&^_.+-]{0,126}/[a-z0-9][a-z0-9!#$&^_.+-]{0,126}$`)
	puidPattern        = regexp.MustCompile(`^(x-)?fmt/[0-9]{1,6}$`)
	ccRightsPattern    = regexp.MustCompile(`^https?://creativecommons\.org/(licenses/[a-z-]+/[0-9.]+|publicdomain/(zero|mark)/[0-9.]+)/([a-z]{2}/)?$`)
	rsRightsPattern    = regexp.MustCompile(`^https?://rightsstatements\.org/vocab/[A-Za-z-]+/1\.0/$`)
	languageTagPattern = regexp.MustCompile(`^([a-zA-Z]{2,8}(-[a-zA-Z0-9]{1,8})*|none)$`)
)

// --- decode -------------------------------------------------------------

// strictDecode decodes raw into v rejecting unknown keys at every
// level, and trailing data.
func strictDecode(raw json.RawMessage, v any, path string) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return invalid(path, "%v", err)
	}
	if dec.More() {
		return invalid(path, "trailing data")
	}
	return nil
}

// ParseArchivalAsset strictly decodes and validates an aa:asset
// member (§17.2).
func ParseArchivalAsset(raw json.RawMessage) (*ArchivalAsset, error) {
	var a ArchivalAsset
	if err := strictDecode(raw, &a, MemberArchivalAsset); err != nil {
		return nil, err
	}
	if err := a.Validate(); err != nil {
		return nil, err
	}
	return &a, nil
}

// ParseArchivalTombstone strictly decodes and validates an
// aa:tombstone member (§17.3).
func ParseArchivalTombstone(raw json.RawMessage) (*ArchivalTombstone, error) {
	var t ArchivalTombstone
	if err := strictDecode(raw, &t, MemberArchivalTombstone); err != nil {
		return nil, err
	}
	if err := t.Validate(); err != nil {
		return nil, err
	}
	return &t, nil
}

// ParseArchivalAnnotation strictly decodes and validates an
// aa:annotation member (§17.5).
func ParseArchivalAnnotation(raw json.RawMessage) (*ArchivalAnnotation, error) {
	var a ArchivalAnnotation
	if err := strictDecode(raw, &a, MemberArchivalAnnotation); err != nil {
		return nil, err
	}
	if err := a.Validate(); err != nil {
		return nil, err
	}
	return &a, nil
}

// ParseArchivalEvent strictly decodes and validates an aa:event
// member (§17.4).
func ParseArchivalEvent(raw json.RawMessage) (*ArchivalEvent, error) {
	var e ArchivalEvent
	if err := strictDecode(raw, &e, MemberArchivalEvent); err != nil {
		return nil, err
	}
	if err := e.Validate(); err != nil {
		return nil, err
	}
	return &e, nil
}

// --- envelope placement (§16.3, §17.1) ------------------------------------

// ValidateArchivalMembers applies the profile rule to one parsed
// envelope: no archival-1 member unless archival-1 is negotiated with
// the other peer (ErrProfileNotNegotiated), each member only on its
// activity types and naming the envelope's object, and the members an
// activity requires present. Pure: the caller supplies whether the
// profile is in the stored, negotiated capability intersection.
// ValidateMembers (members.go) calls it on every inbound envelope
// (#1580). Nothing emits archival-1 members yet; the sender that does
// must check them here before sending.
func ValidateArchivalMembers(env *Envelope, negotiated bool) error {
	asset, hasAsset := env.Extra[MemberArchivalAsset]
	tomb, hasTomb := env.Extra[MemberArchivalTombstone]
	event, hasEvent := env.Extra[MemberArchivalEvent]
	annotation, hasAnnotation := env.Extra[MemberArchivalAnnotation]
	if !hasAsset && !hasTomb && !hasEvent && !hasAnnotation {
		// The activities that require a member are themselves
		// profile-gated.
		if env.Type == ActivityAAProvenance {
			if !negotiated {
				return fmt.Errorf("%w: %s is an archival-1 activity", ErrProfileNotNegotiated, env.Type)
			}
			return invalid(string(env.Type), "requires %s", MemberArchivalEvent)
		}
		if env.Type == ActivityAAAssetVersion && negotiated {
			return invalid(string(env.Type), "requires %s under archival-1", MemberArchivalAsset)
		}
		if env.Type == ActivityAAAnnotation && negotiated {
			return invalid(string(env.Type), "requires %s under archival-1", MemberArchivalAnnotation)
		}
		return nil
	}
	members := 0
	for _, has := range []bool{hasAsset, hasTomb, hasEvent, hasAnnotation} {
		if has {
			members++
		}
	}
	if members > 1 {
		if !negotiated {
			return fmt.Errorf("%w: %s carries archival-1 members", ErrProfileNotNegotiated, env.Type)
		}
		return invalid(string(env.Type), "carries more than one archival-1 member")
	}
	if !negotiated {
		return fmt.Errorf("%w: %s carries archival-1 members", ErrProfileNotNegotiated, env.Type)
	}
	switch env.Type {
	case ActivityCreate, ActivityUpdate, ActivityAAAssetVersion:
		if !hasAsset {
			return invalid(string(env.Type), "carries only %s", MemberArchivalAsset)
		}
		a, err := ParseArchivalAsset(asset)
		if err != nil {
			return err
		}
		if a.ID != env.Object {
			return invalid(MemberArchivalAsset+".id", "does not equal the envelope object")
		}
	case ActivityDelete:
		if !hasTomb {
			return invalid(string(env.Type), "carries only %s", MemberArchivalTombstone)
		}
		t, err := ParseArchivalTombstone(tomb)
		if err != nil {
			return err
		}
		if t.ID != env.Object {
			return invalid(MemberArchivalTombstone+".id", "does not equal the envelope object")
		}
	case ActivityAAProvenance:
		if !hasEvent {
			return invalid(string(env.Type), "carries only %s", MemberArchivalEvent)
		}
		e, err := ParseArchivalEvent(event)
		if err != nil {
			return err
		}
		if e.Object.Asset != env.Object {
			return invalid(MemberArchivalEvent+".object.asset", "does not equal the envelope object")
		}
	case ActivityAAAnnotation:
		if !hasAnnotation {
			return invalid(string(env.Type), "carries only %s", MemberArchivalAnnotation)
		}
		a, err := ParseArchivalAnnotation(annotation)
		if err != nil {
			return err
		}
		if !sameOrigin(a.Target.Source, env.Object) {
			return invalid(MemberArchivalAnnotation+".target.source", "must be a canvas on the annotated asset's origin")
		}
	default:
		return invalid(string(env.Type), "carries no archival-1 member")
	}
	return nil
}

// --- validation -----------------------------------------------------------

// Validate checks an aa:asset against §17.2.
func (a *ArchivalAsset) Validate() error {
	p := MemberArchivalAsset
	if err := checkAbsURI(p+".id", a.ID, true); err != nil {
		return err
	}
	if err := a.Version.validate(p + ".version"); err != nil {
		return err
	}
	if len(a.Files) == 0 || len(a.Files) > ArchivalMaxFiles {
		return invalid(p+".files", "must list 1 to %d files, got %d", ArchivalMaxFiles, len(a.Files))
	}
	originals := 0
	seen := map[string]struct{}{}
	for i, f := range a.Files {
		fp := fmt.Sprintf("%s.files[%d]", p, i)
		if err := f.validate(fp); err != nil {
			return err
		}
		key := f.Role + " " + f.ContentURI
		if _, dup := seen[key]; dup {
			return invalid(fp, "duplicate role and contentUri")
		}
		seen[key] = struct{}{}
		if f.Role == "original" {
			originals++
		}
	}
	if originals == 0 {
		return invalid(p+".files", "must include at least one original")
	}
	if _, ok := archivalSensitivity[a.Sensitivity]; !ok {
		return invalid(p+".sensitivity", "must be public, team, restricted or embargo")
	}
	if a.Mature == nil {
		return invalid(p+".mature", "is required")
	}
	if a.AIProvenance != "" {
		if _, ok := archivalAIProvenance[a.AIProvenance]; !ok {
			return invalid(p+".aiProvenance", "must be none, assisted or generated")
		}
	}
	if a.Rights != "" && !ccRightsPattern.MatchString(a.Rights) && !rsRightsPattern.MatchString(a.Rights) {
		return invalid(p+".rights", "must be a Creative Commons or RightsStatements.org URI")
	}
	if a.RequiredStatement != nil {
		if err := checkLanguageMap(p+".requiredStatement.label", a.RequiredStatement.Label); err != nil {
			return err
		}
		if err := checkLanguageMap(p+".requiredStatement.value", a.RequiredStatement.Value); err != nil {
			return err
		}
	}
	if len(a.Identifiers) > ArchivalMaxIdentifiers {
		return invalid(p+".identifiers", "at most %d", ArchivalMaxIdentifiers)
	}
	for i, id := range a.Identifiers {
		ip := fmt.Sprintf("%s.identifiers[%d]", p, i)
		if _, ok := archivalIDSchemes[id.Scheme]; !ok {
			return invalid(ip+".scheme", "must be ark, doi, handle or local")
		}
		if err := checkText(ip+".value", id.Value, ArchivalMaxShortText, true); err != nil {
			return err
		}
	}
	if err := checkText(p+".accessionNumber", a.AccessionNumber, ArchivalMaxShortText, false); err != nil {
		return err
	}
	if a.Description != nil {
		if err := a.Description.validate(p + ".description"); err != nil {
			return err
		}
	}
	if a.IIIFManifest != "" {
		if err := checkAbsURI(p+".iiifManifest", a.IIIFManifest, true); err != nil {
			return err
		}
		if !sameOrigin(a.IIIFManifest, a.ID) {
			return invalid(p+".iiifManifest", "must be on the asset's own origin")
		}
	}
	return nil
}

func (v ArchivalVersion) validate(p string) error {
	if err := checkAbsURI(p+".id", v.ID, true); err != nil {
		return err
	}
	if v.Number < 1 || v.Number > ArchivalMaxSafeInteger {
		return invalid(p+".number", "must be 1 or more")
	}
	switch {
	case v.Number == 1 && v.Predecessor != nil:
		return invalid(p+".predecessor", "must be null for version 1")
	case v.Number > 1 && v.Predecessor == nil:
		return invalid(p+".predecessor", "is required after version 1")
	case v.Predecessor != nil:
		if err := checkAbsURI(p+".predecessor", *v.Predecessor, true); err != nil {
			return err
		}
		if *v.Predecessor == v.ID {
			return invalid(p+".predecessor", "must not be the version itself")
		}
	}
	return checkTime(p+".created", v.Created)
}

func (f ArchivalFile) validate(p string) error {
	if _, ok := archivalRoles[f.Role]; !ok {
		return invalid(p+".role", "must be original, preservation or access")
	}
	casHash, err := casHashOf(p+".contentUri", f.ContentURI)
	if err != nil {
		return err
	}
	if err := checkChecksums(p+".checksums", f.Checksums, casHash); err != nil {
		return err
	}
	if f.Size == nil {
		return invalid(p+".size", "is required")
	}
	if *f.Size < 0 || *f.Size > ArchivalMaxSafeInteger {
		return invalid(p+".size", "out of range")
	}
	if len(f.MediaType) > archivalMaxMediaTypeBytes || !mediaTypePattern.MatchString(f.MediaType) {
		return invalid(p+".mediaType", "must be a lowercase type/subtype")
	}
	if f.Identification != nil {
		if !puidPattern.MatchString(f.Identification.PUID) {
			return invalid(p+".identification.puid", "must be a PRONOM PUID (fmt/N or x-fmt/N)")
		}
		if err := f.Identification.Tool.validate(p + ".identification.tool"); err != nil {
			return err
		}
	}
	if f.OriginalFilename != "" {
		if err := checkBasename(p+".originalFilename", f.OriginalFilename); err != nil {
			return err
		}
	}
	return nil
}

func (t ArchivalTool) validate(p string) error {
	if err := checkText(p+".name", t.Name, ArchivalMaxShortText, true); err != nil {
		return err
	}
	return checkText(p+".version", t.Version, ArchivalMaxShortText, true)
}

func (d ArchivalDescription) validate(p string) error {
	if err := checkText(p+".title", d.Title, ArchivalMaxShortText*4, false); err != nil {
		return err
	}
	if err := checkText(p+".description", d.Description, ArchivalMaxLongText, false); err != nil {
		return err
	}
	for name, list := range map[string][]string{"creator": d.Creator, "subject": d.Subject} {
		if len(list) > ArchivalMaxListEntries {
			return invalid(p+"."+name, "at most %d entries", ArchivalMaxListEntries)
		}
		for i, s := range list {
			if err := checkText(fmt.Sprintf("%s.%s[%d]", p, name, i), s, ArchivalMaxShortText, true); err != nil {
				return err
			}
		}
	}
	if err := checkText(p+".date", d.Date, 64, false); err != nil {
		return err
	}
	if d.Language != "" && !languageTagPattern.MatchString(d.Language) {
		return invalid(p+".language", "must be a BCP 47 language tag")
	}
	return nil
}

// Validate checks an aa:tombstone against §17.3.
func (t *ArchivalTombstone) Validate() error {
	p := MemberArchivalTombstone
	if err := checkAbsURI(p+".id", t.ID, true); err != nil {
		return err
	}
	if t.FormerType != string(ObjectAAAsset) {
		return invalid(p+".formerType", "must be %s", ObjectAAAsset)
	}
	if err := checkTime(p+".deleted", t.Deleted); err != nil {
		return err
	}
	if err := checkText(p+".reason", t.Reason, ArchivalMaxLongText, false); err != nil {
		return err
	}
	if err := checkAbsURI(p+".lastVersion", t.LastVersion, true); err != nil {
		return err
	}
	if len(t.Files) == 0 || len(t.Files) > ArchivalMaxFiles {
		return invalid(p+".files", "must list 1 to %d files", ArchivalMaxFiles)
	}
	for i, f := range t.Files {
		fp := fmt.Sprintf("%s.files[%d]", p, i)
		if _, ok := archivalRoles[f.Role]; !ok {
			return invalid(fp+".role", "must be original, preservation or access")
		}
		casHash, err := casHashOf(fp+".contentUri", f.ContentURI)
		if err != nil {
			return err
		}
		if err := checkChecksums(fp+".checksums", f.Checksums, casHash); err != nil {
			return err
		}
	}
	return nil
}

// Validate checks an aa:event against §17.4.
func (e *ArchivalEvent) Validate() error {
	p := MemberArchivalEvent
	if err := checkAbsURI(p+".id", e.ID, true); err != nil {
		return err
	}
	if _, ok := ArchivalEventTypes[e.EventType]; !ok {
		return invalid(p+".eventType", "not an archival-1 event type")
	}
	if err := checkTime(p+".dateTime", e.DateTime); err != nil {
		return err
	}
	if err := checkText(p+".detail", e.Detail, ArchivalMaxLongText, false); err != nil {
		return err
	}
	if _, ok := archivalOutcomes[e.Outcome]; !ok {
		return invalid(p+".outcome", "must be success, failure or warning")
	}
	if err := checkText(p+".outcomeDetail", e.OutcomeDetail, ArchivalMaxLongText, false); err != nil {
		return err
	}
	if err := checkAbsURI(p+".object.asset", e.Object.Asset, true); err != nil {
		return err
	}
	if e.Object.Version != "" {
		if err := checkAbsURI(p+".object.version", e.Object.Version, true); err != nil {
			return err
		}
	}
	if e.Object.ContentURI != "" {
		if _, err := casHashOf(p+".object.contentUri", e.Object.ContentURI); err != nil {
			return err
		}
	}
	if len(e.Agents) == 0 || len(e.Agents) > ArchivalMaxAgents {
		return invalid(p+".agents", "must list 1 to %d agents", ArchivalMaxAgents)
	}
	for i, ag := range e.Agents {
		ap := fmt.Sprintf("%s.agents[%d]", p, i)
		if _, ok := archivalAgentRoles[ag.Role]; !ok {
			return invalid(ap+".role", "must be implementer, executing-program or authorizer")
		}
		switch {
		case ag.Actor != "" && ag.Software != nil, ag.Actor == "" && ag.Software == nil:
			return invalid(ap, "names exactly one of actor or software")
		case ag.Actor != "":
			if err := checkAbsURI(ap+".actor", ag.Actor, true); err != nil {
				return err
			}
		default:
			if err := ag.Software.validate(ap + ".software"); err != nil {
				return err
			}
		}
	}
	return nil
}

// Validate checks an aa:annotation against §17.5. Whether the region
// lies inside the canvas needs the canvas size and is checked when the
// annotation is applied (#1578).
func (a *ArchivalAnnotation) Validate() error {
	p := MemberArchivalAnnotation
	if a.Type != "Annotation" {
		return invalid(p+".type", "must be Annotation")
	}
	if _, ok := archivalMotivations[a.Motivation]; !ok {
		return invalid(p+".motivation", "must be commenting, highlighting, describing or tagging")
	}
	if a.Body.Type != "TextualBody" || a.Body.Format != "text/plain" {
		return invalid(p+".body", "must be a text/plain TextualBody")
	}
	if err := checkText(p+".body.value", a.Body.Value, ArchivalMaxLongText, true); err != nil {
		return err
	}
	if a.Body.Language != "" && !languageTagPattern.MatchString(a.Body.Language) {
		return invalid(p+".body.language", "must be a BCP 47 language tag")
	}
	if err := checkAbsURI(p+".target.source", a.Target.Source, true); err != nil {
		return err
	}
	if sel := a.Target.Selector; sel != nil {
		if sel.Type != "FragmentSelector" || sel.ConformsTo != "http://www.w3.org/TR/media-frags/" {
			return invalid(p+".target.selector", "must be a FragmentSelector conforming to http://www.w3.org/TR/media-frags/")
		}
		switch {
		case xywhPattern.MatchString(sel.Value):
		case timePattern.MatchString(sel.Value):
			if m := timePattern.FindStringSubmatch(sel.Value); m[3] != "" && !timeOrdered(sel.Value) {
				return invalid(p+".target.selector.value", "time range must end after it starts")
			}
		default:
			return invalid(p+".target.selector.value", "must be xywh=x,y,w,h or t=start[,end]")
		}
	}
	return nil
}

func timeOrdered(v string) bool {
	parts := strings.SplitN(strings.TrimPrefix(v, "t="), ",", 2)
	var a, b float64
	if _, err := fmt.Sscanf(parts[0], "%g", &a); err != nil {
		return false
	}
	if _, err := fmt.Sscanf(parts[1], "%g", &b); err != nil {
		return false
	}
	return b > a
}

// --- receiver fixity (§17.6) ---------------------------------------------

// VerifyFileFixity reads the received bytes of one listed file and
// returns ErrFixityMismatch unless their length equals the listed size
// and every listed checksum (sha256 always, plus any sha512 / md5)
// matches. It never trusts the sender's claim: the comparison is
// against what was received.
//
// The listed file is validated in full first (§17.2.2, §17.2.3: role,
// a cas:// URI whose hash equals the listed sha256, lowercase hex, no
// duplicate or unknown algorithm, size within 0..2^53-1). Malformed
// metadata returns ErrArchivalInvalid without reading a byte, so a
// file whose sha256 claim differs from its CAS URI can never pass, and
// size+1 below cannot overflow. It then reads at most size+1 bytes, so
// an oversized stream is a mismatch without being read to the end.
func VerifyFileFixity(f ArchivalFile, r io.Reader) error {
	if err := f.validate("file"); err != nil {
		return err
	}
	hashers := map[string]hash.Hash{}
	writers := make([]io.Writer, 0, len(f.Checksums))
	for _, c := range f.Checksums {
		var h hash.Hash
		switch c.Algorithm {
		case "sha256":
			h = sha256.New()
		case "sha512":
			h = sha512.New()
		case "md5":
			h = md5.New() //nolint:gosec // additional legacy fixity only
		default:
			return invalid("file.checksums", "unknown algorithm %q", c.Algorithm)
		}
		hashers[c.Algorithm] = h
		writers = append(writers, h)
	}
	if _, ok := hashers["sha256"]; !ok {
		return invalid("file.checksums", "sha256 is required")
	}
	n, err := io.Copy(io.MultiWriter(writers...), io.LimitReader(r, *f.Size+1))
	if err != nil {
		return fmt.Errorf("federation: read received bytes: %w", err)
	}
	if n != *f.Size {
		return fmt.Errorf("%w: received %d bytes or more, listed %d", ErrFixityMismatch, n, *f.Size)
	}
	for _, c := range f.Checksums {
		if got := hex.EncodeToString(hashers[c.Algorithm].Sum(nil)); got != c.Value {
			return fmt.Errorf("%w: %s of received bytes is %s, listed %s", ErrFixityMismatch, c.Algorithm, got, c.Value)
		}
	}
	return nil
}

// --- helpers --------------------------------------------------------------

func casHashOf(p, uri string) (string, error) {
	m := casURIPattern.FindStringSubmatch(uri)
	if m == nil {
		return "", invalid(p, "must be cas://<lowercase sha256 hex>")
	}
	return m[1], nil
}

func checkChecksums(p string, list []ArchivalChecksum, casHash string) error {
	if len(list) == 0 || len(list) > ArchivalMaxChecksums {
		return invalid(p, "must list 1 to %d checksums", ArchivalMaxChecksums)
	}
	seen := map[string]bool{}
	for i, c := range list {
		cp := fmt.Sprintf("%s[%d]", p, i)
		want, ok := archivalAlgorithms[c.Algorithm]
		if !ok {
			return invalid(cp+".algorithm", "must be sha256, sha512 or md5")
		}
		if seen[c.Algorithm] {
			return invalid(cp+".algorithm", "listed twice")
		}
		seen[c.Algorithm] = true
		if len(c.Value) != want || !lowerHexPattern.MatchString(c.Value) {
			return invalid(cp+".value", "must be %d lowercase hex digits", want)
		}
		if c.Algorithm == "sha256" && c.Value != casHash {
			return invalid(cp+".value", "sha256 does not equal the contentUri hash")
		}
	}
	if !seen["sha256"] {
		return invalid(p, "sha256 is required (md5 or sha512 alone is not enough)")
	}
	return nil
}

func checkAbsURI(p, s string, required bool) error {
	if s == "" {
		if required {
			return invalid(p, "is required")
		}
		return nil
	}
	if len(s) > archivalMaxURILength {
		return invalid(p, "longer than %d", archivalMaxURILength)
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.Fragment != "" {
		return invalid(p, "must be an absolute http(s) URI without user info or fragment")
	}
	return nil
}

func sameOrigin(a, b string) bool {
	ua, errA := url.Parse(a)
	ub, errB := url.Parse(b)
	return errA == nil && errB == nil &&
		strings.EqualFold(ua.Scheme, ub.Scheme) && strings.EqualFold(ua.Host, ub.Host)
}

func checkTime(p, s string) error {
	if _, err := time.Parse(time.RFC3339Nano, s); err != nil {
		return invalid(p, "must be an RFC 3339 timestamp")
	}
	return nil
}

func checkText(p, s string, max int, required bool) error {
	if s == "" {
		if required {
			return invalid(p, "is required")
		}
		return nil
	}
	if len(s) > max || !utf8.ValidString(s) {
		return invalid(p, "must be valid UTF-8 of at most %d bytes", max)
	}
	for _, r := range s {
		if r < 0x20 && r != '\n' && r != '\t' {
			return invalid(p, "contains a control character")
		}
	}
	return nil
}

// checkBasename enforces §17.2.2: a bare file name, never a path.
func checkBasename(p, s string) error {
	if len(s) > ArchivalMaxFilenameBytes || !utf8.ValidString(s) {
		return invalid(p, "must be valid UTF-8 of at most %d bytes", ArchivalMaxFilenameBytes)
	}
	if s == "." || s == ".." || strings.ContainsAny(s, "/\\") {
		return invalid(p, "must be a base name, not a path")
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return invalid(p, "contains a control character")
		}
	}
	return nil
}

func checkLanguageMap(p string, m map[string][]string) error {
	if len(m) == 0 || len(m) > ArchivalMaxListEntries {
		return invalid(p, "must map 1 to %d language tags", ArchivalMaxListEntries)
	}
	for tag, vals := range m {
		if !languageTagPattern.MatchString(tag) {
			return invalid(p, "language tag %q is not BCP 47 or none", tag)
		}
		if len(vals) == 0 || len(vals) > ArchivalMaxListEntries {
			return invalid(p+"."+tag, "must list 1 to %d strings", ArchivalMaxListEntries)
		}
		for i, v := range vals {
			if err := checkText(fmt.Sprintf("%s.%s[%d]", p, tag, i), v, ArchivalMaxLongText, true); err != nil {
				return err
			}
		}
	}
	return nil
}
