// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Profile asset-metadata-1 (docs/spec/federation/v1.md §18; #1577).
//
// Pure, side-effect-free validators for the profile's one member,
// aa:metadata, and for its placement on an asset Update. They are the
// executable form of §18.1, §18.2 and §18.5, tested by
// testdata/conformance/asset-metadata-1/.
//
// The inbox's ValidateMembers calls ValidateAssetMetadataMembers for an
// aa:metadata member (at receipt and at dispatch), and the asset worker
// (assetingest) stores what it admits; the received-asset detail API
// shows the current snapshot. No sender emits the member, no page shows
// it, and the token is absent from peer.KnownCapabilities (§18.6 is
// later #1577 work).
//
// A nil error from these functions is a statement about the bytes and
// the stored profile intersection only. It is not a signature check, a
// peer or grant authorization, an admission, or a write.

package federation

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

// ProfileAssetMetadata1 is the profile token of §18. It is in effect
// only together with ProfileArchival1 and ProfileOriginalTransfer1.
const ProfileAssetMetadata1 = "asset-metadata-1"

// MemberAssetMetadata is the profile's one member (§18.1), on an Update
// of an origin-owned asset only.
const MemberAssetMetadata = "aa:metadata"

// Bounds of §18.2, in UTF-8 bytes.
const (
	// AssetMetadataMaxMemberBytes bounds the raw member value from its
	// opening { through its closing }, whitespace inside it included.
	AssetMetadataMaxMemberBytes      = 32768
	AssetMetadataMaxTitleBytes       = 1024
	AssetMetadataMaxDescriptionBytes = 4096
	AssetMetadataMaxRightsBytes      = 2048
)

// ErrMetadataInvalid: an aa:metadata member, or its placement, does not
// match §18 (the profile's own reason, metadata_invalid, §12.1 and
// §16.3). No running receiver emits it yet (Implementation status S3).
// Its messages name a fixed member path and rule, never a received value
// or a received key.
var ErrMetadataInvalid = errors.New("federation: asset-metadata-1 member invalid")

// AssetMetadataSnapshot is a validated aa:metadata member: the complete
// four-claim snapshot of §18.2. After ParseAssetMetadata a nil field is
// an explicit JSON null (the claim is cleared), never a missing key.
// Every field is marshalled, so a snapshot always serializes with all
// four keys.
type AssetMetadataSnapshot struct {
	Title             *string             `json:"title"`
	Description       *string             `json:"description"`
	Rights            *string             `json:"rights"`
	RequiredStatement *ArchivalLabelValue `json:"requiredStatement"`
}

// The snapshot's keys, in wire order (§18.2).
const (
	metadataKeyTitle             = "title"
	metadataKeyDescription       = "description"
	metadataKeyRights            = "rights"
	metadataKeyRequiredStatement = "requiredStatement"
)

var (
	assetMetadataKeys     = []string{metadataKeyTitle, metadataKeyDescription, metadataKeyRights, metadataKeyRequiredStatement}
	requiredStatementKeys = []string{"label", "value"}

	// metadataForbiddenCompanion are the aa:asset members an Update
	// carrying aa:metadata must not carry, even as null (§18.2).
	metadataForbiddenCompanion = []string{"description", "rights", "requiredStatement", "identifiers", "accessionNumber", "iiifManifest"}
)

// metadataInvalid builds an ErrMetadataInvalid with a fixed path and
// rule. Callers pass only constant text.
func metadataInvalid(path, rule string) error {
	return fmt.Errorf("%w: %s: %s", ErrMetadataInvalid, path, rule)
}

// ParseAssetMetadata strictly decodes and validates a raw aa:metadata
// member (§18.2):
//
//  1. valid UTF-8, a JSON object, and at most AssetMetadataMaxMemberBytes
//     from { through }, measured on the raw bytes (only JSON whitespace
//     outside the object is excluded);
//  2. strict JSON: no trailing value, no invalid escape or unpaired
//     surrogate, and no repeated name at any depth (the RFC 8785
//     canonicalizer is the guard; its output is discarded);
//  3. exactly the four keys, case-sensitively, each present, and
//     requiredStatement with exactly label and value;
//  4. each value null or a valid claim: nonempty text within its bound
//     with no C0 control character other than tab and line feed, a
//     Creative Commons or RightsStatements.org rights URI, and language
//     maps under the archival-1 rules.
//
// Values are kept exactly as decoded: nothing is trimmed, truncated,
// normalized or coerced.
func ParseAssetMetadata(raw json.RawMessage) (*AssetMetadataSnapshot, error) {
	if err := checkMetadataRawSpan(raw); err != nil {
		return nil, err
	}
	if err := checkMetadataStrictJSON(raw); err != nil {
		return nil, err
	}
	members, err := decodeExactObject(raw, MemberAssetMetadata, assetMetadataKeys)
	if err != nil {
		return nil, err
	}
	var s AssetMetadataSnapshot
	if s.Title, err = metadataText(members[metadataKeyTitle], MemberAssetMetadata+".title", AssetMetadataMaxTitleBytes,
		"must be null or nonempty text of at most 1024 bytes with no control character other than tab or line feed"); err != nil {
		return nil, err
	}
	if s.Description, err = metadataText(members[metadataKeyDescription], MemberAssetMetadata+".description", AssetMetadataMaxDescriptionBytes,
		"must be null or nonempty text of at most 4096 bytes with no control character other than tab or line feed"); err != nil {
		return nil, err
	}
	const rightsRule = "must be null or a Creative Commons or RightsStatements.org URI of at most 2048 bytes"
	if s.Rights, err = metadataText(members[metadataKeyRights], MemberAssetMetadata+".rights", AssetMetadataMaxRightsBytes, rightsRule); err != nil {
		return nil, err
	}
	if s.Rights != nil && !ArchivalRightsURIValid(*s.Rights) {
		return nil, metadataInvalid(MemberAssetMetadata+".rights", rightsRule)
	}
	if s.RequiredStatement, err = metadataRequiredStatement(members[metadataKeyRequiredStatement]); err != nil {
		return nil, err
	}
	return &s, nil
}

// checkMetadataRawSpan: valid UTF-8 (before encoding/json could replace
// a bad byte), an object, and the raw object span within the bound.
func checkMetadataRawSpan(raw []byte) error {
	if !utf8.Valid(raw) {
		return metadataInvalid(MemberAssetMetadata, "is not valid UTF-8")
	}
	span := bytes.Trim(raw, " \t\r\n")
	if len(span) == 0 || span[0] != '{' {
		return metadataInvalid(MemberAssetMetadata, "must be a JSON object")
	}
	if len(span) > AssetMetadataMaxMemberBytes {
		return metadataInvalid(MemberAssetMetadata, "is longer than 32768 bytes")
	}
	return nil
}

// checkMetadataStrictJSON uses the canonicalizer as a strict parser: it
// refuses trailing data, invalid escapes, unpaired surrogates and a
// repeated name (compared after unescaping) at any depth.
func checkMetadataStrictJSON(raw []byte) error {
	if _, err := Canonicalize(raw); err != nil {
		return metadataInvalid(MemberAssetMetadata, "is not strict JSON (syntax, Unicode escape or a repeated name)")
	}
	return nil
}

// decodeExactObject decodes a JSON object whose keys must be exactly
// want: compared case-sensitively, none missing, none extra. encoding/json
// alone would match keys case-insensitively and could not tell a
// missing key from a null one.
func decodeExactObject(raw json.RawMessage, path string, want []string) (map[string]json.RawMessage, error) {
	if !isJSONKind(raw, '{') {
		return nil, metadataInvalid(path, "must be a JSON object")
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, metadataInvalid(path, "must be a JSON object")
	}
	for k := range m {
		if !slices.Contains(want, k) {
			return nil, metadataInvalid(path, "has a member this profile does not define")
		}
	}
	for _, k := range want {
		if _, ok := m[k]; !ok {
			return nil, metadataInvalid(path+"."+k, "is required (null clears it)")
		}
	}
	return m, nil
}

// metadataText decodes a null or a string claim and checks it with the
// archival-1 text rule (checkText), translating any failure into the
// profile's own error with a fixed rule.
func metadataText(raw json.RawMessage, path string, max int, rule string) (*string, error) {
	if isJSONNull(raw) {
		return nil, nil
	}
	if !isJSONKind(raw, '"') {
		return nil, metadataInvalid(path, rule)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, metadataInvalid(path, rule)
	}
	if err := checkText(path, s, max, true); err != nil {
		return nil, metadataInvalid(path, rule)
	}
	return &s, nil
}

// metadataRequiredStatement decodes a null or a requiredStatement with
// exactly label and value, each a language map of string lists, and
// checks it with the archival-1 rules (§17.2.5).
func metadataRequiredStatement(raw json.RawMessage) (*ArchivalLabelValue, error) {
	const path = MemberAssetMetadata + ".requiredStatement"
	const rule = "must be null or label and value language maps: 1 to 64 BCP 47 tags (or none), each with 1 to 64 nonempty texts of at most 4096 bytes"
	if isJSONNull(raw) {
		return nil, nil
	}
	parts, err := decodeExactObject(raw, path, requiredStatementKeys)
	if err != nil {
		return nil, err
	}
	var lv ArchivalLabelValue
	if lv.Label, err = metadataLanguageMap(parts["label"], path+".label", rule); err != nil {
		return nil, err
	}
	if lv.Value, err = metadataLanguageMap(parts["value"], path+".value", rule); err != nil {
		return nil, err
	}
	if err := ValidateArchivalRequiredStatement(lv); err != nil {
		return nil, metadataInvalid(path, rule)
	}
	return &lv, nil
}

// metadataLanguageMap decodes a language map whose values are lists of
// strings only: a null map, list or entry is not coerced into an empty
// one.
func metadataLanguageMap(raw json.RawMessage, path, rule string) (map[string][]string, error) {
	if !isJSONKind(raw, '{') {
		return nil, metadataInvalid(path, rule)
	}
	var tags map[string]json.RawMessage
	if err := json.Unmarshal(raw, &tags); err != nil {
		return nil, metadataInvalid(path, rule)
	}
	out := make(map[string][]string, len(tags))
	for tag, listRaw := range tags {
		if !isJSONKind(listRaw, '[') {
			return nil, metadataInvalid(path, rule)
		}
		var entries []json.RawMessage
		if err := json.Unmarshal(listRaw, &entries); err != nil {
			return nil, metadataInvalid(path, rule)
		}
		list := make([]string, 0, len(entries))
		for _, e := range entries {
			if !isJSONKind(e, '"') {
				return nil, metadataInvalid(path, rule)
			}
			var s string
			if err := json.Unmarshal(e, &s); err != nil {
				return nil, metadataInvalid(path, rule)
			}
			list = append(list, s)
		}
		out[tag] = list
	}
	return out, nil
}

// ValidateAssetMetadataMembers checks the placement of aa:metadata on an
// envelope (§18.1, §18.2, §18.5). Without the member it does nothing:
// a flags-only Update, an aa:AssetVersion and every other activity need
// no new token. With it, in order:
//
//  1. all three profiles in the stored, negotiated intersection
//     (ErrProfileNotNegotiated), before any member is examined;
//  2. no member in clear beside an encryption block (ErrInvalidMember);
//  3. only on an Update, with an aa:asset companion (ErrMetadataInvalid);
//  4. no other member beside them (ErrUnknownField, as for a flags-only
//     Update);
//  5. the companion carries no descriptive member, even null
//     (ErrMetadataInvalid); a case-folded alias of one is an invalid
//     archival member (ErrArchivalInvalid);
//  6. the companion is a valid aa:asset naming the envelope object
//     (ErrArchivalInvalid), and the object is on the actor's origin
//     (ErrInvalidMember);
//  7. the member itself (ParseAssetMetadata).
//
// It does not verify the signature, that the actor belongs to the sending
// peer, or any grant. It reads env and changes nothing.
func ValidateAssetMetadataMembers(env *Envelope, mc MemberContext) error {
	raw, ok := env.Extra[MemberAssetMetadata]
	if !ok {
		return nil
	}
	for _, token := range []string{ProfileAssetMetadata1, ProfileArchival1, ProfileOriginalTransfer1} {
		if mc.Negotiated == nil || !mc.Negotiated(token) {
			return fmt.Errorf("%w: %s needs %s", ErrProfileNotNegotiated, MemberAssetMetadata, token)
		}
	}
	if env.Encryption != nil {
		return fmt.Errorf("%w: %s in clear beside an encryption block", ErrInvalidMember, MemberAssetMetadata)
	}
	if env.Type != ActivityUpdate {
		return metadataInvalid(MemberAssetMetadata, "is allowed only on an Update of an asset")
	}
	companion, ok := env.Extra[MemberArchivalAsset]
	if !ok {
		return metadataInvalid(MemberAssetMetadata, "requires an aa:asset member on the same Update")
	}
	for name := range env.Extra {
		if name != MemberAssetMetadata && name != MemberArchivalAsset && name != PropEncryptionPublicKey {
			return fmt.Errorf("%w: an Update carrying %s has a member it does not define", ErrUnknownField, MemberAssetMetadata)
		}
	}
	var companionKeys map[string]json.RawMessage
	if err := json.Unmarshal(companion, &companionKeys); err == nil {
		for name := range companionKeys {
			for _, forbidden := range metadataForbiddenCompanion {
				switch {
				case name == forbidden:
					return metadataInvalid(MemberArchivalAsset, "must not carry a descriptive member beside "+MemberAssetMetadata)
				case strings.EqualFold(name, forbidden):
					return invalid(MemberArchivalAsset, "has a member that is not an exact archival-1 name")
				}
			}
		}
	}
	if err := ValidateArchivalMembers(env, true); err != nil {
		return err
	}
	if err := checkPriorActivityObject(env); err != nil {
		return err
	}
	_, err := ParseAssetMetadata(raw)
	return err
}

func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// isJSONKind reports whether raw's first non-space byte opens the given
// JSON kind ('{', '[' or '"').
func isJSONKind(raw json.RawMessage, open byte) bool {
	t := bytes.TrimLeft(raw, " \t\r\n")
	return len(t) > 0 && t[0] == open
}
