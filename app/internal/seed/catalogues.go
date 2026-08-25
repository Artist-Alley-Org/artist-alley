// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Catalogue + manifest loading for the `aa seed` DB-direct loader.
// Profiles (users/teams/collections/fields) come from seed/profiles;
// the site-specific asset manifest + posts come from the populated
// site root. Mirrors apply.py's Catalogues.load + apply_extension_limit.

package seed

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/mscrnt/artist-alley/app/internal/metadata"
)

type catUser struct {
	Username    string `json:"username"`
	FullName    string `json:"full_name"`
	Email       string `json:"email"`
	PrimaryTeam string `json:"primary_team"`
}

type catTeam struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type catCollection struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Featured places a collection on the PUBLIC rail + /admin/featured
	// (#380, ADR 0065). Absent/false leaves it unplaced.
	//
	// Still a boolean in the catalogue, but it no longer maps to a
	// boolean in the database: applyFeatured writes one featured_items
	// placement at scope='public' per flagged entry. collections.featured
	// is gone.
	Featured bool `json:"featured"`

	// Visibility is the collection's own visibility tier. Absent means
	// "org-only", which is what every entry got before this field
	// existed — so leaving it out preserves the previous behaviour
	// exactly.
	//
	// It has to exist as a field, not just as a key in the JSON: the
	// seeder reads this struct, so an unmodelled key is silently
	// ignored. Placing a collection publicly while it stays org-only
	// produces a rail that renders nothing to an anonymous visitor —
	// correctly, since featuring never widens access (ADR 0065), but
	// invisibly. The placement and the tier have to move together.
	Visibility string `json:"visibility"`
}

type catField struct {
	Name  string `json:"name"` // federation-stable code
	Label string `json:"label"`
	Type  string `json:"type"`
	// Options is the field's controlled vocabulary, in exactly the
	// model the API accepts (metadata.FieldOption): either a bare slug
	// string or the full object, and nested via children.
	//
	// It used to be []string, which is the entry shape all fourteen
	// pre-existing definitions use and which round-trips unchanged
	// through FieldOption's Unmarshal/Marshal pair. But bare strings
	// cannot express hierarchy, so a `tree` definition was not
	// WRITABLE from this catalogue at all — which is why no seeded
	// instance had ever had one, and why the three-way disagreement
	// about where a tree value is stored (#778) survived undetected:
	// the fixture that would have caught it could not be built.
	Options []metadata.FieldOption `json:"options"`
	// Extraction wiring (#618). extraction_source must be one of the
	// extractor's CanonicalField names or the definition routes nothing:
	// the mapping query filters WHERE extraction_source != '', so an
	// unwired technical field is indistinguishable from a missing one —
	// the exact defect that kept IIIF's info.json 404ing after the
	// definitions notionally "existed". Empty = operator-managed field,
	// which is right for the studio-fiction set.
	ExtractionSource string `json:"extraction_source"`
	ExtractionMode   string `json:"extraction_mode"`
}

// manifestAsset is the subset of a MANIFEST.json entry the seeder uses.
type manifestAsset struct {
	ID            string `json:"id"`
	AssetType     string `json:"asset_type"`
	Title         string `json:"title"`
	Description   string `json:"description"`
	FilePath      string `json:"file_path"`
	FileExtension string `json:"file_extension"`
	// FileSizeBytes is advisory only — the seeder writes the size it
	// measures off disk, not this. Coverage selection reads it to
	// prefer cheap assets over expensive ones (#768).
	FileSizeBytes   int64  `json:"file_size_bytes"`
	SensitivityTier string `json:"sensitivity_tier"`
	// Mature is the catalogue's own content LABEL, and it is a second
	// axis rather than a tier inside SensitivityTier (ADR 0090): a
	// public artwork can be mature and a restricted one need not be, so
	// the two travel separately from the manifest all the way to the
	// two columns. Absent/false is the honest default for a catalogue
	// entry nobody has labelled.
	//
	// It has to be modelled HERE to be seeded at all — an unmodelled
	// JSON key is silently dropped by the decoder, which is how the
	// mature axis came to be unexercised on every seeded instance
	// (#1217) even though the schema, the predicate and the UI had
	// shipped.
	Mature bool `json:"mature"`
	// AiProvenance is the MAKER'S DECLARATION about generative-AI
	// involvement (#1167, ADR 0094): `none`, `assisted`, `generated`,
	// or ABSENT, which means UNDECLARED — nobody was asked.
	//
	// ⚠️ A POINTER, AND NOT A `string`, BECAUSE ABSENT IS A VALUE HERE.
	// `assets.ai_provenance` is nullable and unbackfilled precisely so
	// the rows predating the feature do not assert a disclaimer their
	// makers never made, and a plain `string` would decode a missing key
	// to `""` — which this seeder would then have to invent a rule for.
	// Nil writes NULL, which is the honest answer for a catalogue entry
	// nobody has declared, and it is what every entry says today bar the
	// handful #1251 slice 3 declares on purpose.
	//
	// It is modelled HERE for the reason `Mature` above it records in as
	// many words: an unmodelled JSON key is silently dropped by the
	// decoder. #1217 is the bill for learning that the hard way — the
	// mature axis shipped its schema, its predicate and its UI and then
	// sat unexercised on every seeded instance because this struct did
	// not carry the field.
	//
	// ⛔ IT DOES NOT REPLACE `metadata.acquisition_source` AND MUST NOT.
	// The fixture sweep partitions the asset table on that key alone
	// (fixturesweep.Rules, ADR 0095) — an asset the seeder wrote without
	// it is indistinguishable from real uploaded content and becomes
	// sweep-bait. A declared seeded asset carries BOTH.
	AiProvenance     *string         `json:"ai_provenance"`
	ArchiveState     string          `json:"archive_state"`
	OwnerUsername    string          `json:"owner_username"`
	CollectionName   string          `json:"collection_name"`
	TeamName         string          `json:"team_name"`
	Tags             []string        `json:"tags"`
	WorkflowState    string          `json:"workflow_state"`
	Metadata         json.RawMessage `json:"metadata"`
	FieldValues      map[string]any  `json:"field_values"`
	ReviewNotes      string          `json:"review_notes"`
	ReviewerUsername string          `json:"reviewer_username"`
	CreatedAt        string          `json:"created_at"`
	UpdatedAt        string          `json:"updated_at"`
}

// manifestPost is the subset of a posts.json entry the seeder uses.
type manifestPost struct {
	ID             string   `json:"id"`
	Title          string   `json:"title"`
	Description    string   `json:"description"`
	AssetIDs       []string `json:"asset_ids"`
	AuthorUsername string   `json:"author_username"`
	CollectionName string   `json:"collection_name"`
	TeamName       string   `json:"team_name"`
	WorkflowState  string   `json:"workflow_state"`
	Tags           []string `json:"tags"`
	CreatedAt      string   `json:"created_at"`
	UpdatedAt      string   `json:"updated_at"`

	// Coverage-selection inputs (#768), and — for SensitivityTier —
	// the seeder's visibility input as well (#1176).
	//
	// applyPosts still hardcodes state 'published', but visibility is no
	// longer hardcoded 'org-only': postVisibility reads SensitivityTier
	// (with the cover asset's tier) to decide which posts go public, so
	// an instance in public mode has something for an anonymous visitor
	// to look at. The CI coverage profile selects on these too, so the
	// shrunk fixture keeps the same tier mix.
	PostKind        string `json:"post_kind"`
	SensitivityTier string `json:"sensitivity_tier"`
	IsMixedType     bool   `json:"is_mixed_type"`
}

// catFixtures is dataset.fixtures.json: the substrate the dogfood suite
// used to build for itself on every fresh database (#1270).
//
// It is a CATALOGUE and not a hardcoded list because the credentials
// have to have exactly one home — scripts/dogfood/ui/helpers/
// seeded-principal.ts reads this same file, so a password changed here
// reaches the suite and a password changed there reaches nothing.
//
// Loaded only when `aa seed --fixtures` asks for it; the demo path never
// creates these accounts. See the file's own `_why` block.
type catFixtures struct {
	Principals []catFixturePrincipal `json:"principals"`
	Admin      catFixtureAdmin       `json:"admin_uploads"`
}

type catFixturePrincipal struct {
	Username string `json:"username"`
	FullName string `json:"full_name"`
	Email    string `json:"email"`
	Password string `json:"password"`
	// ConsumedBy and Why are documentation carried in the data, so the
	// answer to "what is this account for" travels with the account
	// rather than living in a comment two directories away.
	ConsumedBy string `json:"consumed_by"`
	Why        string `json:"why"`
}

type catFixtureAdmin struct {
	Count           int    `json:"count"`
	TitlePrefix     string `json:"title_prefix"`
	PostTitlePrefix string `json:"post_title_prefix"`
	CreatedAt       string `json:"created_at"`
	ConsumedBy      string `json:"consumed_by"`
	Why             string `json:"why"`
	SweepNote       string `json:"sweep_note"`
}

type catalogues struct {
	Users       []catUser
	Teams       []catTeam
	Collections []catCollection
	Fields      []catField
	Assets      []manifestAsset
	Posts       []manifestPost

	// Fixtures is nil when the catalogue directory ships no
	// dataset.fixtures.json. Absent is not an error HERE — the file is
	// only meaningful to `--fixtures`, and applyTestFixtures is the one
	// place that can say what its absence costs.
	Fixtures *catFixtures

	// SiteRoot is kept so coverage selection can reach the bytes:
	// whether a model declares external companions is a property of the
	// FILE, not of the manifest row (#750), so it has to be read.
	SiteRoot string
}

func loadCatalogues(catalogueRoot, siteRoot string) (*catalogues, error) {
	c := &catalogues{SiteRoot: siteRoot}
	if err := loadJSON(filepath.Join(catalogueRoot, "dataset.users.json"), &c.Users); err != nil {
		return nil, err
	}
	if err := loadJSON(filepath.Join(catalogueRoot, "dataset.teams.json"), &c.Teams); err != nil {
		return nil, err
	}
	if err := loadJSON(filepath.Join(catalogueRoot, "dataset.collections.json"), &c.Collections); err != nil {
		return nil, err
	}
	if err := loadJSON(filepath.Join(catalogueRoot, "dataset.field_definitions.json"), &c.Fields); err != nil {
		return nil, err
	}
	if err := loadJSON(filepath.Join(siteRoot, "MANIFEST.json"), &c.Assets); err != nil {
		return nil, err
	}
	if err := loadJSON(filepath.Join(siteRoot, "posts.json"), &c.Posts); err != nil {
		return nil, err
	}
	fixPath := filepath.Join(catalogueRoot, "dataset.fixtures.json")
	if _, err := os.Stat(fixPath); err == nil {
		var f catFixtures
		if err := loadJSON(fixPath, &f); err != nil {
			return nil, err
		}
		c.Fixtures = &f
	}
	return c, nil
}

// guessContentType maps a file extension to a MIME type (port of
// apply.py._guess_content_type). Falls back to octet-stream.
func guessContentType(extension string) string {
	ext := strings.ToLower(strings.TrimPrefix(extension, "."))
	switch ext {
	case "jpg", "jpeg":
		return "image/jpeg"
	case "png":
		return "image/png"
	case "gif":
		return "image/gif"
	case "webp":
		return "image/webp"
	case "svg":
		return "image/svg+xml"
	case "hdr":
		return "image/vnd.radiance"
	case "mp4":
		return "video/mp4"
	case "webm":
		return "video/webm"
	case "mov":
		return "video/quicktime"
	case "avi":
		return "video/x-msvideo"
	case "mkv":
		return "video/x-matroska"
	case "ogv":
		return "video/ogg"
	case "mp3":
		return "audio/mpeg"
	case "ogg":
		return "audio/ogg"
	case "wav":
		return "audio/wav"
	case "m4a":
		return "audio/mp4"
	case "flac":
		return "audio/flac"
	case "pdf":
		return "application/pdf"
	case "epub":
		return "application/epub+zip"
	case "txt":
		return "text/plain"
	case "md":
		return "text/markdown"
	case "json":
		return "application/json"
	case "yaml", "yml":
		return "text/yaml"
	case "otf":
		return "font/otf"
	case "ttf":
		return "font/ttf"
	case "woff":
		return "font/woff"
	case "woff2":
		return "font/woff2"
	case "fbx":
		return "model/fbx"
	case "glb":
		return "model/gltf-binary"
	case "gltf":
		return "model/gltf+json"
	case "obj":
		return "model/obj"
	case "cbz":
		return "application/vnd.comicbook+zip"
	case "zip":
		return "application/zip"
	default:
		return "application/octet-stream"
	}
}

// AIDeclarableSourcePrefix is the provenance an asset must claim before
// it may claim `ai_provenance` (#1260).
//
// ⛔ WHY THE SEEDER CHECKS THIS AT ALL, when seed/scripts/apply_upgrade.py
// audits the same rule. The two read DIFFERENT FILES. The Python audit
// covers `seed/profiles/*`, which is the pipeline's INPUT and lives in
// the repo; the seeder reads `MANIFEST.json` off the archive share, which
// is its OUTPUT and does not. A false declaration written straight into
// the shipped manifest — which is how the last four got applied, by hand
// — passes every check in this repo and reaches the database anyway.
//
// The claim is not an ordinary catalogue value. `ai_provenance` says HOW
// THE WORK WAS MADE, on a row that also names its creator, and site_a is
// published to Kaggle: every asset in it is a real work by an
// identifiable third party (Kenney.nl 1,778 · Pexels 75 · Met Museum 18
// · NASA 6 · …). Four rows already carried `generated` over
// `attribution: "Kenney (kenney.nl)"` before #1260 removed them.
//
// Widening this constant publishes a claim about somebody. That is why
// it is a named constant and a hard failure rather than a warning: a
// seed log nobody reads is exactly how the mature axis sat dead for
// months (#1217).
const AIDeclarableSourcePrefix = "Generated in-house"

// validateAIDeclarations refuses a manifest that declares AI on work we
// did not make. Runs before any bytes move, so the failure costs
// seconds rather than the whole upload phase.
func (c *catalogues) validateAIDeclarations() error {
	for _, a := range c.Assets {
		if a.AiProvenance == nil || *a.AiProvenance == "" {
			continue
		}
		var meta map[string]any
		if len(a.Metadata) > 0 {
			_ = json.Unmarshal(a.Metadata, &meta)
		}
		src, _ := meta["acquisition_source"].(string)
		if strings.HasPrefix(src, AIDeclarableSourcePrefix) {
			continue
		}
		return fmt.Errorf(
			"asset %s (%q) declares ai_provenance=%q but its provenance is %q. "+
				"An AI declaration on work we did not generate is a false statement "+
				"about that creator, and this dataset is published — either the "+
				"declaration is wrong or the provenance is. If we really did make it, "+
				"say so in metadata.acquisition_source (it must start with %q)",
			a.ID, a.Title, *a.AiProvenance, src, AIDeclarableSourcePrefix)
	}
	return nil
}

func loadJSON(path string, dst any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read catalogue %s: %w", path, err)
	}
	if err := json.Unmarshal(b, dst); err != nil {
		return fmt.Errorf("parse catalogue %s: %w", path, err)
	}
	return nil
}

// applyExtensionLimit shrinks the asset set to at most n per distinct
// file_extension (order-preserving) and cascade-drops posts that
// reference any cut asset. Mirrors apply.py.apply_extension_limit —
// used to produce fast CI / dogfood seeds.
func (c *catalogues) applyExtensionLimit(n int, log *slog.Logger) {
	if n <= 0 {
		return
	}
	beforeA, beforeP := len(c.Assets), len(c.Posts)
	counts := map[string]int{}
	kept := c.Assets[:0:0]
	keptIDs := map[string]bool{}
	for _, a := range c.Assets {
		if counts[a.FileExtension] >= n {
			continue
		}
		counts[a.FileExtension]++
		kept = append(kept, a)
		keptIDs[a.ID] = true
	}
	var keptPosts []manifestPost
	for _, p := range c.Posts {
		all := len(p.AssetIDs) > 0
		for _, aid := range p.AssetIDs {
			if !keptIDs[aid] {
				all = false
				break
			}
		}
		if all {
			keptPosts = append(keptPosts, p)
		}
	}
	c.Assets = kept
	c.Posts = keptPosts
	log.Info("seed.limit_per_extension", "n", n,
		"assets_before", beforeA, "assets_after", len(c.Assets),
		"posts_before", beforeP, "posts_after", len(c.Posts))
}
