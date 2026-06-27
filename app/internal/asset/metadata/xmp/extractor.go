package xmp

import (
	"context"
	"errors"
	"io"

	"github.com/mscrnt/artist-alley/app/internal/asset/metadata"
)

// Extractor implements metadata.Extractor for Adobe XMP packets
// embedded in JPEG (APP1 / adobe.com/xap) + PNG (iTXt /
// XML:com.adobe.xmp). TIFF and WebP land in a follow-up — their
// XMP carriers each need a separate walker; the parser itself
// is carrier-agnostic so the extension is a small one-method
// add.
type Extractor struct{}

// New returns the singleton extractor. Register AFTER EXIF + IPTC
// so when two extractors emit the same CanonicalField (e.g.
// dc:title vs IPTC ObjectName) the per-field extraction-config
// picker is the operator's tie-breaker — neither extractor
// silently wins by registration order.
func New() *Extractor { return &Extractor{} }

// Name implements metadata.Extractor.
func (Extractor) Name() string { return "xmp" }

// Supports implements metadata.Extractor.
func (Extractor) Supports(mime string) bool {
	switch mime {
	case "image/jpeg", "image/jpg", "image/png":
		return true
	}
	return false
}

// Extract implements metadata.Extractor. Picks the carrier
// walker based on MIME, extracts the XMP packet, parses, returns
// the typed Result. metadata.ErrNoMetadata when the file has
// no XMP payload.
func (e Extractor) Extract(_ context.Context, r io.Reader, mime string) (metadata.Result, error) {
	bytes, err := io.ReadAll(r)
	if err != nil {
		return metadata.Result{Format: mime}, metadata.ErrMalformedFile
	}

	var packet []byte
	if err := safe(func() error {
		var ferr error
		switch mime {
		case "image/jpeg", "image/jpg":
			packet, ferr = FindJPEGXMPPacket(bytes)
		case "image/png":
			packet, ferr = FindPNGXMPPacket(bytes)
		default:
			ferr = ErrNoXMP
		}
		return ferr
	}); err != nil {
		if errors.Is(err, ErrNoXMP) {
			return metadata.Result{Format: mime}, metadata.ErrNoMetadata
		}
		return metadata.Result{Format: mime}, metadata.ErrLibraryPanic
	}

	res, err := ParseXMPPacket(packet)
	if err != nil {
		if errors.Is(err, ErrNoXMP) {
			return metadata.Result{Format: mime}, metadata.ErrNoMetadata
		}
		return metadata.Result{Format: mime}, metadata.ErrMalformedFile
	}
	return metadata.Result{
		Format: mime,
		Fields: res.Fields,
	}, nil
}

func safe(fn func() error) (out error) {
	defer func() {
		if r := recover(); r != nil {
			out = errors.New("xmp: panic recovered")
		}
	}()
	return fn()
}

// Compile-time interface check.
var _ metadata.Extractor = (*Extractor)(nil)
