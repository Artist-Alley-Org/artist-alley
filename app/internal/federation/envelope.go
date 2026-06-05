// Activity envelope — the wire-format unit of federation. Every
// activity is one envelope; the envelope wraps the type-specific
// fields, embeds a signed proof of authorship, and may carry an
// encryption envelope (NaCl-box, see nacl/) in place of plaintext
// type fields.
//
// Strict parsing
//
// Unmarshal rejects any top-level field not in the schema below
// (the "reject-on-unknown-fields" policy in
// docs/spec/federation/v1.md §3.2). This is the v1 protocol's
// defence against silent schema drift between implementations.
// New fields must wait for a `@context` version bump.
//
// Spec reference: docs/spec/federation/v1.md §3.

package federation

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Envelope is the typed wire-format unit. JSON-tagged for both
// emit and parse paths; the parser uses DisallowUnknownFields to
// catch silent extensions per §3.2.
//
// Field-presence rules:
//
//   - Context, Type, ID, Actor, Published are REQUIRED for any
//     envelope.
//   - Signature is REQUIRED unless this is the input to Sign
//     (which produces it).
//   - To and CC are OPTIONAL. Their canonical form is "field
//     omitted" when empty — see §3.4 (and the Marshaling notes
//     below).
//   - Object is conditionally required by activity type
//     (validated outside this struct by the type's handler).
//   - Encrypted is mutually exclusive with type-specific plaintext
//     fields; presence flips the envelope into encrypted mode
//     (the signature then covers the encrypted envelope, not the
//     plaintext within).
//   - Extra holds the type-specific fields (e.g. aa:comment,
//     aa:reviewedAt, object_kind). Encoded by Marshal as
//     top-level keys; decoded by Unmarshal as a residual map
//     after the named fields have been extracted.
type Envelope struct {
	Context   string             `json:"@context"`
	Type      ActivityType       `json:"type"`
	ID        string             `json:"id"`
	Actor     string             `json:"actor"`
	Published time.Time          `json:"published"`
	To        []string           `json:"to,omitempty"`
	CC        []string           `json:"cc,omitempty"`
	Object    string             `json:"object,omitempty"`
	Encrypted *EncryptedEnvelope `json:"encrypted,omitempty"`
	Signature *Signature         `json:"signature,omitempty"`

	// Extra carries the activity-type-specific fields. The known
	// named fields above are excluded from Extra both at marshal
	// time (we wouldn't double-emit them) and at unmarshal time
	// (they're consumed by the named fields). Extra is also the
	// channel by which the strict-parse "unknown_field" check
	// finds violations — Unmarshal scans Extra after extraction
	// and rejects unknown top-level keys outside the per-type
	// allowlist.
	//
	// Type-specific allowlists live alongside each activity in
	// activities/*.go (Phase 1.22.E). For 1.22.A the parser
	// accepts ANY extra field — strict validation per type lands
	// when the activity handlers do.
	Extra map[string]json.RawMessage `json:"-"`
}

// EncryptedEnvelope is the multi-recipient NaCl-box payload that
// replaces type-specific fields when an activity is end-to-end
// encrypted. See nacl/box.go for the encrypt + decrypt primitives
// and docs/spec/federation/v1.md §6 for the wire shape.
type EncryptedEnvelope struct {
	Alg          EncryptionAlgorithm `json:"alg"`
	EphemeralKey string              `json:"ephemeralKey"` // base64url-no-padding, 32 bytes X25519 pubkey
	Recipients   []EncryptedRecipient `json:"recipients"`
}

// EncryptedRecipient is one per-recipient entry inside an
// EncryptedEnvelope.
type EncryptedRecipient struct {
	Actor      string `json:"actor"`
	Nonce      string `json:"nonce"`      // base64url-no-padding, 24 bytes
	Ciphertext string `json:"ciphertext"` // base64url-no-padding
}

// Signature is the embedded signed-proof block on every envelope.
// See docs/spec/federation/v1.md §5.
type Signature struct {
	Type      SignatureAlgorithm `json:"type"`      // v1: must equal SignatureAlgEd25519
	PublicKey string             `json:"publicKey"` // URL of the actor's published key, e.g. "...#main-key"
	Value     string             `json:"value"`     // base64url-no-padding(64 bytes)
}

// Parse errors. These map to InboxStatus values at the inbox
// dispatch layer (Phase 1.22.D wires the mapping); for 1.22.A the
// envelope code is the only thing that emits them, so we just
// expose them as sentinel errors.
var (
	ErrInvalidContext   = errors.New("federation: @context is not the artist-alley v1 string")
	ErrUnknownField     = errors.New("federation: envelope contains an unknown top-level field")
	ErrMissingField     = errors.New("federation: envelope missing a required field")
	ErrInvalidType      = errors.New("federation: type is not in the known activity-type catalogue")
	ErrInvalidPublished = errors.New("federation: published is not a valid RFC 3339 timestamp")
	ErrUnsigned         = errors.New("federation: envelope has no signature block")
	ErrUnsupportedAlg   = errors.New("federation: signature algorithm not in the allowlist")
)

// Marshal encodes the envelope as wire-format JSON.
//
// Extra fields are merged into the top-level object alongside the
// named fields. Marshal does not canonicalize — that's a separate
// step (Canonicalize) and only applied when signing or verifying.
func (e *Envelope) Marshal() ([]byte, error) {
	// Build the on-the-wire map by emitting the named fields
	// first then overlaying Extra. Named fields win on collision
	// (defence in depth — Extra shouldn't carry the named keys).
	out := make(map[string]json.RawMessage, len(e.Extra)+11)
	for k, v := range e.Extra {
		out[k] = v
	}
	// Required.
	if err := setRaw(out, "@context", e.Context); err != nil {
		return nil, err
	}
	if err := setRaw(out, "type", string(e.Type)); err != nil {
		return nil, err
	}
	if err := setRaw(out, "id", e.ID); err != nil {
		return nil, err
	}
	if err := setRaw(out, "actor", e.Actor); err != nil {
		return nil, err
	}
	if err := setRaw(out, "published", e.Published.UTC().Format(time.RFC3339Nano)); err != nil {
		return nil, err
	}
	// Optional / conditional.
	if len(e.To) > 0 {
		if err := setRaw(out, "to", e.To); err != nil {
			return nil, err
		}
	}
	if len(e.CC) > 0 {
		if err := setRaw(out, "cc", e.CC); err != nil {
			return nil, err
		}
	}
	if e.Object != "" {
		if err := setRaw(out, "object", e.Object); err != nil {
			return nil, err
		}
	}
	if e.Encrypted != nil {
		if err := setRaw(out, "encrypted", e.Encrypted); err != nil {
			return nil, err
		}
	}
	if e.Signature != nil {
		if err := setRaw(out, "signature", e.Signature); err != nil {
			return nil, err
		}
	}
	return json.Marshal(out)
}

// Unmarshal decodes a wire-format envelope, validating the
// required fields and the @context version string. Strict parsing
// catches unknown top-level fields per §3.2.
//
// Unmarshal does NOT verify the signature — that's a separate
// step (VerifyOn) so callers can choose how to obtain the
// verifying key. Unmarshal does not validate that the signature's
// algorithm is in the allowlist, either; the verifier does.
func Unmarshal(data []byte) (*Envelope, error) {
	// Two-pass decode: first into a raw map to enforce
	// known-field-only at the top level, then into the typed
	// fields. Spec §3.2's "reject-on-unknown-fields" rule needs
	// the raw map to see what's there.
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("federation: unmarshal envelope: %w", err)
	}

	known := map[string]bool{
		"@context": true, "type": true, "id": true, "actor": true,
		"published": true, "to": true, "cc": true, "object": true,
		"encrypted": true, "signature": true,
	}

	env := &Envelope{Extra: map[string]json.RawMessage{}}

	if v, ok := raw["@context"]; ok {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return nil, fmt.Errorf("federation: @context: %w", err)
		}
		if s != ContextV1 {
			return nil, fmt.Errorf("%w: %q", ErrInvalidContext, s)
		}
		env.Context = s
	} else {
		return nil, fmt.Errorf("%w: @context", ErrMissingField)
	}

	if v, ok := raw["type"]; ok {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return nil, fmt.Errorf("federation: type: %w", err)
		}
		t := ActivityType(s)
		if !t.Valid() {
			return nil, fmt.Errorf("%w: %q", ErrInvalidType, s)
		}
		env.Type = t
	} else {
		return nil, fmt.Errorf("%w: type", ErrMissingField)
	}

	if v, ok := raw["id"]; ok {
		var s string
		if err := json.Unmarshal(v, &s); err != nil || s == "" {
			return nil, fmt.Errorf("federation: id: %w", errOr(err, ErrMissingField))
		}
		env.ID = s
	} else {
		return nil, fmt.Errorf("%w: id", ErrMissingField)
	}

	if v, ok := raw["actor"]; ok {
		var s string
		if err := json.Unmarshal(v, &s); err != nil || s == "" {
			return nil, fmt.Errorf("federation: actor: %w", errOr(err, ErrMissingField))
		}
		env.Actor = s
	} else {
		return nil, fmt.Errorf("%w: actor", ErrMissingField)
	}

	if v, ok := raw["published"]; ok {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return nil, fmt.Errorf("federation: published: %w", err)
		}
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return nil, fmt.Errorf("%w: %q", ErrInvalidPublished, s)
		}
		env.Published = t
	} else {
		return nil, fmt.Errorf("%w: published", ErrMissingField)
	}

	if v, ok := raw["to"]; ok {
		if err := json.Unmarshal(v, &env.To); err != nil {
			return nil, fmt.Errorf("federation: to: %w", err)
		}
	}
	if v, ok := raw["cc"]; ok {
		if err := json.Unmarshal(v, &env.CC); err != nil {
			return nil, fmt.Errorf("federation: cc: %w", err)
		}
	}
	if v, ok := raw["object"]; ok {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return nil, fmt.Errorf("federation: object: %w", err)
		}
		env.Object = s
	}
	if v, ok := raw["encrypted"]; ok {
		var ee EncryptedEnvelope
		// Strict-parse the encrypted subobject too — same policy.
		ed := json.NewDecoder(bytes.NewReader(v))
		ed.DisallowUnknownFields()
		if err := ed.Decode(&ee); err != nil {
			return nil, fmt.Errorf("federation: encrypted: %w", err)
		}
		env.Encrypted = &ee
	}
	if v, ok := raw["signature"]; ok {
		var sig Signature
		sd := json.NewDecoder(bytes.NewReader(v))
		sd.DisallowUnknownFields()
		if err := sd.Decode(&sig); err != nil {
			return nil, fmt.Errorf("federation: signature: %w", err)
		}
		env.Signature = &sig
	}

	// Reject any unknown top-level fields (§3.2). Anything left
	// in raw that isn't in `known` is a violation.
	for k, v := range raw {
		if known[k] {
			continue
		}
		// For 1.22.A we don't validate per-activity-type extra
		// fields; we accept them into Extra so per-type handlers
		// (1.22.E) can validate. The Marshal side echoes them
		// back, so round-trip is intact.
		env.Extra[k] = v
	}

	return env, nil
}

// CanonicalSigningBytes returns the byte sequence to feed into
// Sign / Verify — the envelope minus the signature field,
// canonicalized per RFC 8785.
//
// The "minus the signature field" rule is in §4.3: delete-then-
// canonicalize is the single correct order. Implementations that
// canonicalize with the signature present and try to "subtract"
// it afterward admit subtle equality bugs.
func (e *Envelope) CanonicalSigningBytes() ([]byte, error) {
	cp := *e
	cp.Signature = nil
	wire, err := cp.Marshal()
	if err != nil {
		return nil, err
	}
	return Canonicalize(wire)
}

// Sign produces an envelope signature and attaches it. The
// envelope is mutated in place. Caller supplies the private key
// and the URL where the matching public key is published (the
// signature.publicKey hint).
//
// Sign canonicalizes per §4, signs the canonical bytes with
// Ed25519, base64url-no-padding-encodes the signature, and stores
// it in e.Signature. The signature algorithm is locked to
// Ed25519 per the v1 allowlist (§5.5).
func (e *Envelope) Sign(privPEM []byte, publicKeyURL string) error {
	priv, err := PrivateKeyFromPEM(privPEM)
	if err != nil {
		return err
	}
	// Temporarily drop any existing signature before computing the
	// canonical bytes (re-sign overwrites in place).
	prev := e.Signature
	e.Signature = nil
	msg, err := e.CanonicalSigningBytes()
	if err != nil {
		e.Signature = prev
		return err
	}
	sig := Sign(priv, msg)
	e.Signature = &Signature{
		Type:      SignatureAlgEd25519,
		PublicKey: publicKeyURL,
		Value:     base64.RawURLEncoding.EncodeToString(sig),
	}
	return nil
}

// Verify checks the envelope's signature under the supplied
// PEM-encoded public key. The caller is responsible for obtaining
// the correct public key — Verify does not consult the
// signature.publicKey URL.
//
// Returns nil on success or one of ErrUnsigned, ErrUnsupportedAlg,
// ErrSigMalformed, ErrSigInvalid on failure. Mapping to the
// InboxStatus catalogue is the caller's job (the inbox dispatcher
// in 1.22.D does this).
func (e *Envelope) Verify(publicPEM []byte) error {
	if e.Signature == nil {
		return ErrUnsigned
	}
	if !e.Signature.Type.Valid() {
		return ErrUnsupportedAlg
	}
	pub, err := PublicKeyFromPEM(publicPEM)
	if err != nil {
		return err
	}
	sigBytes, err := base64.RawURLEncoding.DecodeString(e.Signature.Value)
	if err != nil {
		return ErrSigMalformed
	}
	// Sign over the envelope WITHOUT its signature — match what
	// Sign signed.
	cp := *e
	cp.Signature = nil
	msg, err := cp.CanonicalSigningBytes()
	if err != nil {
		return err
	}
	return Verify(pub, msg, sigBytes)
}

// --- internal helpers -----------------------------------------------------

func setRaw(dst map[string]json.RawMessage, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("federation: marshal %s: %w", key, err)
	}
	dst[key] = b
	return nil
}

func errOr(a, b error) error {
	if a != nil {
		return a
	}
	return b
}
