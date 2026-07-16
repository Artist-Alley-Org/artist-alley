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
}

type catField struct {
	Name    string   `json:"name"` // federation-stable code
	Label   string   `json:"label"`
	Type    string   `json:"type"`
	Options []string `json:"options"`
}

// manifestAsset is the subset of a MANIFEST.json entry the seeder uses.
type manifestAsset struct {
	ID               string          `json:"id"`
	AssetType        string          `json:"asset_type"`
	Title            string          `json:"title"`
	Description      string          `json:"description"`
	FilePath         string          `json:"file_path"`
	FileExtension    string          `json:"file_extension"`
	SensitivityTier  string          `json:"sensitivity_tier"`
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
}

type catalogues struct {
	Users       []catUser
	Teams       []catTeam
	Collections []catCollection
	Fields      []catField
	Assets      []manifestAsset
	Posts       []manifestPost
}

func loadCatalogues(catalogueRoot, siteRoot string) (*catalogues, error) {
	c := &catalogues{}
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
