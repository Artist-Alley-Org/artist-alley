package presentation

import (
	"time"

	"github.com/google/uuid"
)

// EntityType identifies which entity a manifest is being built for.
type EntityType string

const (
	EntityAsset      EntityType = "asset"
	EntityCollection EntityType = "collection"
)

// Sensitivity mirrors the enum on the assets.sensitivity column.
// Kept as a package-local type so the presentation layer isn't
// tied to the metadata subsystem's imports.
type Sensitivity string

const (
	SensitivityPublic     Sensitivity = "public"
	SensitivityTeam       Sensitivity = "team"
	SensitivityRestricted Sensitivity = "restricted"
	SensitivityEmbargo    Sensitivity = "embargo"
)

// PresentationContext is the JSON-LD context URI for Presentation
// API 3.0. Emitted as the top-level @context on every manifest.
const PresentationContext = "http://iiif.io/api/presentation/3/context.json"

// NavPlaceContext is the JSON-LD context URI for the navPlace
// extension. Emitted alongside the presentation context when the
// manifest carries a geo-tagged feature.
const NavPlaceContext = "http://iiif.io/api/extension/navplace/context.json"

// ImageAPIContext is the JSON-LD context URI for Image API 3.0.
// Referenced by canvas item Image resources so viewers know the
// tile-endpoint speaks the level-0 grammar.
const ImageAPIContext = "http://iiif.io/api/image/3/context.json"

// LangString is the IIIF Presentation 3.0 language-tagged value
// shape: {"en": ["value"]}. Every user-facing string in the
// manifest uses this form so viewers can localise.
type LangString map[string][]string

// EN is a helper for the common single-en case.
func EN(v string) LangString {
	if v == "" {
		return LangString{"en": {""}}
	}
	return LangString{"en": {v}}
}

// MetadataPair is one label/value row rendered in the viewer's
// sidebar. Both sides are language-tagged.
type MetadataPair struct {
	Label LangString `json:"label"`
	Value LangString `json:"value"`
}

// Provider identifies who publishes the manifest. Configured per
// instance via sysconfig; defaults to a synthetic "artist-alley"
// identity when unset.
type Provider struct {
	ID       string     `json:"id"`
	Type     string     `json:"type"`
	Label    LangString `json:"label"`
	Homepage []Homepage `json:"homepage,omitempty"`
}

// Homepage is the browser-facing URL for the provider (or the
// manifest's target entity). Type is "Text" per spec since it's
// clickable HTML, not an image.
type Homepage struct {
	ID     string     `json:"id"`
	Type   string     `json:"type"`
	Label  LangString `json:"label"`
	Format string     `json:"format,omitempty"`
}

// Thumbnail is a compact preview image reference. body content-
// type omitted for the common image/jpeg case; explicit when the
// source is SVG or WebP.
type Thumbnail struct {
	ID     string     `json:"id"`
	Type   string     `json:"type"`
	Format string     `json:"format,omitempty"`
	Width  int        `json:"width,omitempty"`
	Height int        `json:"height,omitempty"`
	Label  LangString `json:"label,omitempty"`
}

// NavPlaceFeature is one geo-tagged Point in the navPlace
// extension's FeatureCollection.
type NavPlaceFeature struct {
	Type       string             `json:"type"`
	Geometry   NavPlaceGeometry   `json:"geometry"`
	Properties NavPlaceProperties `json:"properties,omitempty"`
}

// NavPlaceGeometry is a GeoJSON Point with [lon, lat] ordering per
// GeoJSON spec (NOT [lat, lon] — this trips up implementers who
// think in Google Maps coordinate order).
type NavPlaceGeometry struct {
	Type        string    `json:"type"`
	Coordinates []float64 `json:"coordinates"`
}

// NavPlaceProperties carries the human-readable label the viewer
// renders on the map pin.
type NavPlaceProperties struct {
	Label LangString `json:"label,omitempty"`
}

// NavPlace is the extension block emitted at manifest top level
// when the asset has GPS coordinates.
type NavPlace struct {
	Context  string             `json:"@context"`
	Type     string             `json:"type"`
	Features []NavPlaceFeature `json:"features"`
}

// Canvas is one page/frame in the manifest. Single-canvas
// manifests have a length-1 items list; multi-canvas (post +
// future PDF multi-page) get one canvas per source unit.
type Canvas struct {
	ID     string         `json:"id"`
	Type   string         `json:"type"`
	Label  LangString     `json:"label"`
	Height int            `json:"height,omitempty"`
	Width  int            `json:"width,omitempty"`
	Items  []AnnotationPage `json:"items,omitempty"`
}

// AnnotationPage wraps the canvas's content annotations. In the
// single-image case, one annotation with motivation="painting"
// pointing at the Image API tile source.
type AnnotationPage struct {
	ID    string       `json:"id"`
	Type  string       `json:"type"`
	Items []Annotation `json:"items,omitempty"`
}

// Annotation is one content anchor. For image-on-canvas, body is
// the Image API endpoint; target is the canvas id.
type Annotation struct {
	ID         string      `json:"id"`
	Type       string      `json:"type"`
	Motivation string      `json:"motivation"`
	Body       ImageBody   `json:"body"`
	Target     string      `json:"target"`
}

// ImageBody is the tile-source reference. Service points at Image
// API level-0 for viewers that support tiled zoom.
type ImageBody struct {
	ID      string         `json:"id"`
	Type    string         `json:"type"`
	Format  string         `json:"format,omitempty"`
	Width   int            `json:"width,omitempty"`
	Height  int            `json:"height,omitempty"`
	Service []ImageService `json:"service,omitempty"`
}

// ImageService points at Image API info.json — viewers use this
// for tiled zoom + region requests.
type ImageService struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Profile string `json:"profile"`
}

// Manifest is the top-level JSON body for an asset or single-
// entity view. Collections use CollectionManifest below.
type Manifest struct {
	Context           any                `json:"@context"` // string OR []string when navPlace present
	ID                string             `json:"id"`
	Type              string             `json:"type"`
	Label             LangString         `json:"label"`
	Rights            string             `json:"rights,omitempty"`
	RequiredStatement *MetadataPair      `json:"requiredStatement,omitempty"`
	Metadata          []MetadataPair     `json:"metadata,omitempty"`
	Provider          []Provider         `json:"provider,omitempty"`
	Homepage          []Homepage         `json:"homepage,omitempty"`
	Thumbnail         []Thumbnail        `json:"thumbnail,omitempty"`
	NavPlace          *NavPlace          `json:"navPlace,omitempty"`
	Items             []Canvas           `json:"items,omitempty"`
	// SeeAlso surfaces the Content Search 2.0 endpoint so
	// viewers auto-discover in-manifest search per spec.
	SeeAlso []SeeAlso `json:"seeAlso,omitempty"`
}

// CollectionManifest is the collection variant. items contains
// references to member manifests rather than canvases.
type CollectionManifest struct {
	Context           any                     `json:"@context"`
	ID                string                  `json:"id"`
	Type              string                  `json:"type"`
	Label             LangString              `json:"label"`
	RequiredStatement *MetadataPair           `json:"requiredStatement,omitempty"`
	Metadata          []MetadataPair          `json:"metadata,omitempty"`
	Provider          []Provider              `json:"provider,omitempty"`
	Homepage          []Homepage              `json:"homepage,omitempty"`
	Thumbnail         []Thumbnail             `json:"thumbnail,omitempty"`
	Items             []CollectionMember      `json:"items,omitempty"`
	SeeAlso           []SeeAlso               `json:"seeAlso,omitempty"`
}

// CollectionMember is one reference to a child manifest.
type CollectionMember struct {
	ID    string     `json:"id"`
	Type  string     `json:"type"`
	Label LangString `json:"label"`
}

// SeeAlso links to companion resources — Content Search 2.0
// service, related manifests, or (future) annotation collections.
type SeeAlso struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Label   LangString `json:"label,omitempty"`
	Profile string `json:"profile,omitempty"`
	Format  string `json:"format,omitempty"`
}

// EntityRef is the plain-Go projection of an asset or collection
// row the builder consumes. Kept narrow so tests don't need to
// spin up the full sqlc-generated struct.
type EntityRef struct {
	ID             uuid.UUID
	Kind           EntityType
	Title          string
	Description    string
	Sensitivity    Sensitivity
	OwnerUserRef   *int64
	OriginServerID *uuid.UUID
	CreatedAt      time.Time
	UpdatedAt      time.Time
	PageCount      *int
	FileExtension  string
	Latitude       *float64
	Longitude      *float64
	// EmbargoUntil is optional; when non-nil and in the future,
	// the manifest builder emits a stub manifest instead of the
	// full canvas payload.
	EmbargoUntil *time.Time
	// LicenseURI is the copyright / rights URL if the asset has
	// one — feeds Manifest.Rights per spec.
	LicenseURI string
	// AttributionHTML is the required-statement text (usually a
	// short "© <holder>" line). Feeds RequiredStatement.
	AttributionHTML string
	// Metadata is the pre-filtered custom-field pair set the
	// builder emits verbatim. Filtering (public-only, non-empty,
	// respect field-definition visibility) happens at the
	// caller layer.
	Metadata []MetadataPair
	// RemoteCanvasBase / RemoteImageBase are populated by the
	// http-layer federation resolver when OriginServerID != nil.
	// Empty string means "fall back to local URL" (peer directory
	// missed or peer row deleted — see iiif/federation Resolver).
	RemoteCanvasBase string
	RemoteImageBase  string
}
