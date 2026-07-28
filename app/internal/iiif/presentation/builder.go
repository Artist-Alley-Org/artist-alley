// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package presentation

import (
	"strings"
	"time"

	"github.com/google/uuid"
)

// BuilderConfig captures instance-level identity + URL bases
// injected at boot from sysconfig. Every manifest carries these
// verbatim; caching keys ignore them since the whole cache
// invalidates on sysconfig write anyway.
type BuilderConfig struct {
	// SiteBaseURL is the front-of-house URL (e.g.,
	// "https://art.example.com"). All /iiif/3/... URLs are
	// rooted here.
	SiteBaseURL string

	// Provider is the IIIF Provider block. When Label empty,
	// falls back to SiteBaseURL as the label so a manifest is
	// never emitted with a blank provider.
	Provider Provider
}

// Builder is the manifest-assembler. Stateless; one per process.
// Callers supply per-request EntityRef + resolved federation
// URIs; Build returns a serialisable Manifest / CollectionManifest.
type Builder struct {
	Config BuilderConfig
}

// NewBuilder constructs a Builder with the sysconfig-derived
// config. Boot wires this once + shares across requests.
func NewBuilder(cfg BuilderConfig) *Builder {
	if cfg.Provider.Type == "" {
		cfg.Provider.Type = "Agent"
	}
	if cfg.Provider.ID == "" {
		cfg.Provider.ID = strings.TrimRight(cfg.SiteBaseURL, "/") + "/about"
	}
	if len(cfg.Provider.Label) == 0 {
		cfg.Provider.Label = EN(strings.TrimPrefix(strings.TrimPrefix(cfg.SiteBaseURL, "https://"), "http://"))
	}
	return &Builder{Config: cfg}
}

// ForRequest returns a shallow-copied Builder whose SiteBaseURL is
// overridden with the per-request public origin. Matches 1.54.A's
// publicBaseURL(r) verbatim so both the Image API and Presentation
// API emit URLs consistent with the request that came in.
//
// The receiver is unchanged; callers use the returned pointer for
// the current request only.
func (b *Builder) ForRequest(siteBaseURL string) *Builder {
	cp := *b
	cp.Config.SiteBaseURL = siteBaseURL
	return &cp
}

// BuildAssetManifest emits a full asset manifest OR a stub
// manifest when the asset is under active embargo.
//
// Anonymous callers (caller == AnonymousCaller sentinel) hit the
// IIIF-layer sensitivity gate BEFORE the manifest is assembled:
// restricted assets return nil, ErrRestricted; the HTTP wrapper
// maps to 404 (per spec + brief; 403 would leak existence).
// Embargo returns a stub manifest.
func (b *Builder) BuildAssetManifest(entity EntityRef, isAnonymous bool) (*Manifest, error) {
	if isAnonymous {
		if entity.Sensitivity == SensitivityRestricted || entity.Sensitivity == SensitivityTeam {
			return nil, ErrRestricted
		}
	}

	// Embargo shape: label + provider + requiredStatement only.
	// No canvases, no metadata leak. Per ADR 0020 + brief
	// decision 10.
	if isEmbargoActive(entity.EmbargoUntil) {
		return b.stubManifest(entity), nil
	}

	m := &Manifest{
		Context:  b.chooseContext(entity),
		ID:       b.assetManifestURL(entity.ID),
		Type:     "Manifest",
		Label:    EN(nonEmpty(entity.Title, entity.ID.String())),
		Provider: []Provider{b.Config.Provider},
		Homepage: []Homepage{
			{
				ID:     b.assetHomepageURL(entity.ID),
				Type:   "Text",
				Label:  EN("Open in artist-alley"),
				Format: "text/html",
			},
		},
	}

	if entity.LicenseURI != "" {
		m.Rights = entity.LicenseURI
	}
	if entity.AttributionHTML != "" {
		m.RequiredStatement = &MetadataPair{
			Label: EN("Attribution"),
			Value: EN(entity.AttributionHTML),
		}
	}
	if len(entity.Metadata) > 0 {
		m.Metadata = entity.Metadata
	}
	m.Thumbnail = []Thumbnail{b.thumbnailFor(entity)}
	m.Items = []Canvas{b.canvasFor(entity)}

	if entity.Latitude != nil && entity.Longitude != nil {
		np := b.navPlaceFor(entity)
		if np != nil {
			m.NavPlace = np
		}
	}

	m.SeeAlso = append(m.SeeAlso, SeeAlso{
		ID:      b.contentSearchURL(EntityAsset, entity.ID),
		Type:    "SearchService2",
		Label:   EN("Search within this asset"),
		Profile: "http://iiif.io/api/search/2/search",
	})

	// Multi-page assets (page_count > 1): per pre-audit Q3
	// finding, the 1.54.A Image API does NOT yet route
	// /pages/{n}/. Ship the manifest as single-canvas with a
	// metadata note surfacing the page count for now; per-page
	// canvas emission ships when the Image API grows the URL
	// grammar in a follow-up.
	if entity.PageCount != nil && *entity.PageCount > 1 {
		m.Metadata = append(m.Metadata, MetadataPair{
			Label: EN("Pages"),
			Value: EN(itoa(*entity.PageCount) + " (per-page tile routing pending — see follow-up)"),
		})
	}
	return m, nil
}

// BuildCollectionManifest emits a Collection listing member
// entities in the canonical (sort_order, added_at) order the
// callers hand us.
//
// Anonymous callers hit the content-plane sensitivity gate (ADR
// 0064). Since #661 the members list IS pre-filtered by
// visibility.Filter (LoadCollectionMembers splices the EntityAsset
// predicate), so the per-member check below is a content-plane
// supplement rather than the sole gate — but it is still required,
// because the AUTHENTICATED branch of that predicate is soft-delete
// only and admits every sensitivity tier. See #432, #661.
func (b *Builder) BuildCollectionManifest(entity EntityRef, members []EntityRef, isAnonymous bool) (*CollectionManifest, error) {
	if isAnonymous {
		if entity.Sensitivity == SensitivityRestricted || entity.Sensitivity == SensitivityTeam {
			return nil, ErrRestricted
		}
	}

	cm := &CollectionManifest{
		Context:  PresentationContext,
		ID:       b.collectionManifestURL(entity.ID),
		Type:     "Collection",
		Label:    EN(nonEmpty(entity.Title, entity.ID.String())),
		Provider: []Provider{b.Config.Provider},
		Homepage: []Homepage{
			{
				ID:     b.collectionHomepageURL(entity.ID),
				Type:   "Text",
				Label:  EN("Open in artist-alley"),
				Format: "text/html",
			},
		},
	}

	if len(entity.Metadata) > 0 {
		cm.Metadata = entity.Metadata
	}

	// Filter restricted/team members out of an anonymous manifest.
	// Since #661 LoadCollectionMembers DOES splice the EntityAsset
	// predicate, whose anonymous branch already requires
	// sensitivity='public', so for an anonymous caller this loop is
	// now belt-and-braces. It stays because it is the content-plane
	// rule (ADR 0064) and the two planes are maintained separately:
	// if the authenticated sensitivity rule ever lands (#210) this is
	// where the manifest's answer comes from.
	items := make([]CollectionMember, 0, len(members))
	for _, mem := range members {
		if isAnonymous && (mem.Sensitivity == SensitivityRestricted || mem.Sensitivity == SensitivityTeam) {
			continue
		}
		items = append(items, CollectionMember{
			ID:    b.assetManifestURL(mem.ID),
			Type:  "Manifest",
			Label: EN(nonEmpty(mem.Title, mem.ID.String())),
		})
	}
	cm.Items = items

	cm.SeeAlso = append(cm.SeeAlso, SeeAlso{
		ID:      b.contentSearchURL(EntityCollection, entity.ID),
		Type:    "SearchService2",
		Label:   EN("Search within this collection"),
		Profile: "http://iiif.io/api/search/2/search",
	})
	return cm, nil
}

// stubManifest is the embargoed-asset variant per brief decision
// 10. Label + provider + requiredStatement only.
func (b *Builder) stubManifest(entity EntityRef) *Manifest {
	stmt := "This item is under embargo."
	if entity.EmbargoUntil != nil {
		stmt = "This item is under embargo until " + entity.EmbargoUntil.UTC().Format(time.RFC3339) + "."
	}
	return &Manifest{
		Context:  PresentationContext,
		ID:       b.assetManifestURL(entity.ID),
		Type:     "Manifest",
		Label:    EN(nonEmpty(entity.Title, entity.ID.String())),
		Provider: []Provider{b.Config.Provider},
		RequiredStatement: &MetadataPair{
			Label: EN("Notice"),
			Value: EN(stmt),
		},
		// Items REQUIRED by spec even in the embargo-stub shape;
		// initialise to an empty slice so JSON emits [] not null.
		Items: []Canvas{},
	}
}

// defaultCanvasWidth / defaultCanvasHeight are the placeholder
// dimensions emitted when the loader hasn't populated real ones.
// IIIF Presentation 3.0 §5.7 requires Canvas to carry BOTH width
// and height (or a duration) for viewers to compute layout;
// Mirador's OpenSeadragon integration crashes with a `null` deref
// when either is missing. Real dimensions come from the extractor
// pipeline (asset_field_value rows for image_width/image_height),
// but the loader doesn't populate EntityRef.Width/Height yet —
// that's a follow-up (tracked as part of the metadata / asset
// dimensions read-through). Defaults are chosen to make the aspect
// ratio close to landscape 4:3 which viewers handle gracefully.
const (
	defaultCanvasWidth  = 1200
	defaultCanvasHeight = 900
)

// canvasFor renders the asset's single canvas. Federated assets
// surface as remote actor URIs per ADR 0043 — local instance
// never proxies tiles.
func (b *Builder) canvasFor(entity EntityRef) Canvas {
	base := b.canvasIDBase(entity)
	canvasID := base + "/canvas/1"
	imageID := b.imageAPIURL(entity) + "/full/max/0/default.jpg"
	infoID := b.imageAPIURL(entity) + "/info.json"
	w, h := defaultCanvasWidth, defaultCanvasHeight
	if entity.Width > 0 && entity.Height > 0 {
		w, h = entity.Width, entity.Height
	}
	return Canvas{
		ID:     canvasID,
		Type:   "Canvas",
		Label:  EN(nonEmpty(entity.Title, entity.ID.String())),
		Width:  w,
		Height: h,
		Items: []AnnotationPage{
			{
				ID:   canvasID + "/page/1",
				Type: "AnnotationPage",
				Items: []Annotation{
					{
						ID:         canvasID + "/annotation/1",
						Type:       "Annotation",
						Motivation: "painting",
						Body: ImageBody{
							ID:     imageID,
							Type:   "Image",
							Format: "image/jpeg",
							Width:  w,
							Height: h,
							Service: []ImageService{
								{
									ID:      infoID,
									Type:    "ImageService3",
									Profile: "level0",
								},
							},
						},
						Target: canvasID,
					},
				},
			},
		},
	}
}

// thumbnailFor emits an Image API-backed thumbnail. Follows the
// Image API grammar (size 256 as a reasonable default; viewers
// re-request if they need a different size).
func (b *Builder) thumbnailFor(entity EntityRef) Thumbnail {
	base := b.imageAPIURL(entity)
	return Thumbnail{
		ID:     base + "/full/,256/0/default.jpg",
		Type:   "Image",
		Format: "image/jpeg",
	}
}

// navPlaceFor renders the geo-tagged point. Returns nil for
// out-of-range coordinates (validated at extraction time but
// belt-and-braces here).
func (b *Builder) navPlaceFor(entity EntityRef) *NavPlace {
	if entity.Latitude == nil || entity.Longitude == nil {
		return nil
	}
	lat := *entity.Latitude
	lon := *entity.Longitude
	if lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return nil
	}
	// (0, 0) is technically a valid coordinate (Null Island) but
	// far more often means "GPS wasn't populated + defaulted to
	// zero". Treat as missing per brief decision 4 note.
	if lat == 0 && lon == 0 {
		return nil
	}
	return &NavPlace{
		Context: NavPlaceContext,
		Type:    "FeatureCollection",
		Features: []NavPlaceFeature{
			{
				Type: "Feature",
				Geometry: NavPlaceGeometry{
					Type:        "Point",
					Coordinates: []float64{lon, lat}, // [lon, lat] per GeoJSON
				},
				Properties: NavPlaceProperties{
					Label: EN(nonEmpty(entity.Title, entity.ID.String())),
				},
			},
		},
	}
}

// chooseContext returns the top-level @context — either the plain
// Presentation string or an array including navPlace when the
// manifest carries a geo-tagged feature.
func (b *Builder) chooseContext(entity EntityRef) any {
	if entity.Latitude != nil && entity.Longitude != nil {
		return []string{NavPlaceContext, PresentationContext}
	}
	return PresentationContext
}

// canvasIDBase returns the URL prefix for canvas + annotation
// IDs. For local assets this is the asset's manifest URL; for
// federated assets, this is the remote actor's canonical URI
// per ADR 0043 — the resolver hands the ORIGIN_SERVER_ID to a
// peer directory lookup that returns the peer's site base URL.
func (b *Builder) canvasIDBase(entity EntityRef) string {
	if entity.OriginServerID != nil {
		if base := b.remoteCanvasBase(entity); base != "" {
			return base
		}
	}
	return b.assetManifestURL(entity.ID)
}

// imageAPIURL returns the Image API base for the asset. Same
// federation gate as canvas IDs — federated assets get remote
// resolution or fall through to local.
func (b *Builder) imageAPIURL(entity EntityRef) string {
	if entity.OriginServerID != nil {
		if base := b.remoteImageBase(entity); base != "" {
			return base
		}
	}
	return strings.TrimRight(b.Config.SiteBaseURL, "/") + "/iiif/3/" + entity.ID.String()
}

// remoteCanvasBase / remoteImageBase return pre-resolved federated
// URL bases stashed on the EntityRef by the http-layer applyFederation
// helper. Empty string means "no peer resolution" — either the asset
// is local (OriginServerID nil) or the peer directory lookup missed;
// callers fall through to the local URL.
func (b *Builder) remoteCanvasBase(entity EntityRef) string { return entity.RemoteCanvasBase }
func (b *Builder) remoteImageBase(entity EntityRef) string  { return entity.RemoteImageBase }

// assetManifestURL, collectionManifestURL, assetHomepageURL,
// collectionHomepageURL, contentSearchURL are the shape-fixed URL
// builders. Every manifest ID / homepage / seeAlso in the entire
// package routes through these so a sysconfig SiteBaseURL change
// takes effect on the next request.
func (b *Builder) assetManifestURL(id uuid.UUID) string {
	return strings.TrimRight(b.Config.SiteBaseURL, "/") + "/iiif/3/asset/" + id.String() + "/manifest.json"
}
func (b *Builder) collectionManifestURL(id uuid.UUID) string {
	return strings.TrimRight(b.Config.SiteBaseURL, "/") + "/iiif/3/collection/" + id.String() + "/manifest.json"
}
func (b *Builder) assetHomepageURL(id uuid.UUID) string {
	return strings.TrimRight(b.Config.SiteBaseURL, "/") + "/assets/" + id.String()
}
func (b *Builder) collectionHomepageURL(id uuid.UUID) string {
	return strings.TrimRight(b.Config.SiteBaseURL, "/") + "/collections/" + id.String()
}
func (b *Builder) contentSearchURL(kind EntityType, id uuid.UUID) string {
	return strings.TrimRight(b.Config.SiteBaseURL, "/") + "/iiif/3/" + string(kind) + "/" + id.String() + "/search"
}

// --- helpers ----------------------------------------------------

func isEmbargoActive(until *time.Time) bool {
	if until == nil {
		return false
	}
	return until.After(time.Now())
}

func nonEmpty(a, fallback string) string {
	if strings.TrimSpace(a) == "" {
		return fallback
	}
	return a
}

// itoa avoids strconv import for a single call site.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+(n%10))) + digits
		n /= 10
	}
	if neg {
		return "-" + digits
	}
	return digits
}
