// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package sysconfig

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"io"

	// Decoder registrations for image.DecodeConfig / image.Decode. The
	// blank imports ARE the allowlist's teeth: a format with no decoder
	// linked in cannot be identified, so it cannot pass validation.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	_ "golang.org/x/image/webp"
)

// Instance-logo limits.
//
// A navbar mark renders at ~40 CSS px and a login card at ~80, so
// 1024px on the long edge is already generous for a 2x/3x display.
// The caps exist to bound what an operator-supplied file costs us:
// MaxLogoBytes bounds the buffer we hold while validating, and
// MaxLogoDim bounds the pixel budget of the decode itself, which is
// what stops a decompression bomb (a 40KB PNG can declare 30000x30000
// and cost gigabytes to rasterize).
const (
	// MaxLogoBytes caps the uploaded file. We buffer the whole thing
	// in memory to validate before writing it anywhere, so this is a
	// per-request allocation ceiling, not just a policy number.
	MaxLogoBytes = 2 << 20 // 2 MiB

	// MinLogoDim rejects tracking-pixel-sized uploads, which are far
	// more likely to be a mistake than a logo.
	MinLogoDim = 16

	// MaxLogoDim caps the longest edge.
	MaxLogoDim = 1024
)

// Logo validation failures. Callers map these to a 400 with the
// error text; they are distinguished so tests can assert the reason
// rather than string-matching a message that may be reworded.
var (
	ErrLogoTooLarge   = errors.New("sysconfig: logo exceeds the maximum upload size")
	ErrLogoNotAnImage = errors.New("sysconfig: logo is not a supported image")
	ErrLogoDimensions = errors.New("sysconfig: logo dimensions out of range")
)

// logoFormatMIME is the allowlist, keyed by the format name the
// standard library's registry reports after it has actually parsed
// the header. Mapping FROM the decoded format (rather than to it) is
// the point: the content type we later serve is derived from bytes we
// parsed ourselves, never from anything the client told us.
//
// SVG is deliberately absent. It is the obvious thing to want for a
// logo and the obvious thing to get wrong: an SVG is an XML document
// that can carry <script>, <foreignObject>, event handlers, and
// external entity references, so accepting one means either shipping a
// sanitiser (a real, ongoing security commitment — the allowlist has
// to track every new SVG feature) or serving it in a way that can
// never be treated as a document. Neither is a subtask of "let the
// operator set a logo", so this issue ships raster only and an
// operator who wants vector art rasterizes it first. See the PR for
// the full reasoning.
var logoFormatMIME = map[string]string{
	"png":  "image/png",
	"jpeg": "image/jpeg",
	"gif":  "image/gif",
	"webp": "image/webp",
}

// allowedLogoMIME is logoFormatMIME inverted — the set of content
// types that may be persisted and, therefore, served. [SetAppearance]
// re-checks against it so a stored value can never name a type the
// validator would not have produced, including on a config row that
// was written by an older or future version of this code.
var allowedLogoMIME = func() map[string]struct{} {
	out := make(map[string]struct{}, len(logoFormatMIME))
	for _, m := range logoFormatMIME {
		out[m] = struct{}{}
	}
	return out
}()

// ValidateLogo reads at most MaxLogoBytes+1 from r, proves the bytes
// are a decodable image in an allowlisted raster format, and returns
// the buffered bytes alongside the metadata to persist.
//
// Everything about the returned LogoConfig is derived from the bytes.
// The caller MUST NOT substitute a client-supplied content type: the
// serve path sets Content-Type from this value, and a client that
// could choose it could serve text/html from our own origin.
//
// Validation is deliberately two-pass. DecodeConfig identifies the
// format and dimensions from the header alone, which is what lets us
// reject an oversized image before paying to rasterize it. Only once
// the dimensions are known to be sane do we run a full Decode, which
// is what proves the file is a real image rather than a valid header
// glued to arbitrary trailing data.
func ValidateLogo(r io.Reader) ([]byte, LogoConfig, error) {
	// +1 so a file of exactly MaxLogoBytes+1 is distinguishable from
	// one that just happens to end on the limit.
	buf, err := io.ReadAll(io.LimitReader(r, MaxLogoBytes+1))
	if err != nil {
		return nil, LogoConfig{}, fmt.Errorf("sysconfig: read logo: %w", err)
	}
	if len(buf) > MaxLogoBytes {
		return nil, LogoConfig{}, fmt.Errorf("%w (max %d bytes)", ErrLogoTooLarge, MaxLogoBytes)
	}
	if len(buf) == 0 {
		return nil, LogoConfig{}, fmt.Errorf("%w: empty body", ErrLogoNotAnImage)
	}

	cfg, format, err := image.DecodeConfig(bytes.NewReader(buf))
	if err != nil {
		return nil, LogoConfig{}, fmt.Errorf("%w: %v", ErrLogoNotAnImage, err)
	}
	mime, ok := logoFormatMIME[format]
	if !ok {
		return nil, LogoConfig{}, fmt.Errorf("%w: %s is not an accepted format (want PNG, JPEG, GIF or WebP)",
			ErrLogoNotAnImage, format)
	}
	if cfg.Width < MinLogoDim || cfg.Height < MinLogoDim {
		return nil, LogoConfig{}, fmt.Errorf("%w: %dx%d is smaller than %dpx",
			ErrLogoDimensions, cfg.Width, cfg.Height, MinLogoDim)
	}
	if cfg.Width > MaxLogoDim || cfg.Height > MaxLogoDim {
		return nil, LogoConfig{}, fmt.Errorf("%w: %dx%d exceeds %dpx on the longest edge",
			ErrLogoDimensions, cfg.Width, cfg.Height, MaxLogoDim)
	}

	// Full decode, now that the pixel budget is known-bounded. This is
	// what catches a truncated or corrupt body whose header parsed
	// fine, so we never persist something the browser will refuse to
	// draw.
	if _, _, err := image.Decode(bytes.NewReader(buf)); err != nil {
		return nil, LogoConfig{}, fmt.Errorf("%w: %v", ErrLogoNotAnImage, err)
	}

	return buf, LogoConfig{
		ContentType: mime,
		Width:       cfg.Width,
		Height:      cfg.Height,
		SizeBytes:   int64(len(buf)),
	}, nil
}
