package assets

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	openapi_types "github.com/oapi-codegen/runtime/types"
	"github.com/taylorskalyo/goreader/epub"
	"golang.org/x/net/html"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/storage"
)

// EPUB reader endpoints.
//
// Surface (three operations, one per use case):
//
//   GET /assets/{id}/epub/spine                  → ordered chapter list
//   GET /assets/{id}/epub/chapters/{idx}         → chapter HTML (rewritten)
//   GET /assets/{id}/epub/resources/{*path}      → resource bytes from the zip
//
// The frontend EpubView mounts an iframe per chapter, fed from
// /chapters/{idx}, which streams the chapter XHTML with all
// relative resource references rewritten to /resources/{path} so
// the iframe loads them through us. Resources include CSS, images,
// fonts — everything the chapter renders against.
//
// Per-call cost: open the .epub as a bytes.Reader (zip parser
// needs io.ReaderAt) and parse the rootfile / manifest / spine.
// Typical EPUBs are 1–10 MB so reading into memory once per
// request is acceptable; the cache layer absorbs repeat reads.
//
// Caching strategy (ABC):
//   * Spine list — small (KB range), per-asset. Cache 5_000.
//   * Rendered chapter HTML — bigger (~10–50 KB per chapter, but
//     ebooks routinely have hundreds of chapters). Cache 2_000
//     keyed on (assetId, idx). LRU evicts cold entries; the
//     reader's "next chapter" call always hits cache after the
//     first read-through.
//   * Resources NOT cached — they're served direct from the zip;
//     the browser's cache + our VariantCache middleware handle
//     repeat hits cheaply enough.
//
// Invalidation: companion writes don't affect EPUB bodies (the
// .epub is the source). The only mutation that matters is an
// asset re-upload, which generates a new asset id; old caches
// stale-out naturally.

// maxEPUBBytes caps the in-memory read size. Real-world EPUBs
// almost never exceed 50 MB (massive scanned ones aside); 200 MB
// gives plenty of headroom while preventing an upload abuse path.
const maxEPUBBytes = 200 * 1024 * 1024

// ---------------------------------------------------------------------------
// GET /assets/{id}/epub/spine
// ---------------------------------------------------------------------------

func (h *Handler) GetEpubSpine(
	ctx context.Context,
	req openapi.GetEpubSpineRequestObject,
) (openapi.GetEpubSpineResponseObject, error) {
	if auth.IdentityFromContext(ctx) == nil {
		return openapi.GetEpubSpine401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}

	cacheKey := req.Id.String()
	if h.epubSpine != nil {
		if v, ok := h.epubSpine.Get(cacheKey); ok {
			return openapi.GetEpubSpine200JSONResponse(v), nil
		}
	}

	r, _, _, err := h.openEPUB(ctx, req.Id)
	if err != nil {
		return mapEPUBErrToSpineResponse(err), nil
	}

	root := r.Container.DefaultRendition()
	if root == nil {
		return openapi.GetEpubSpine404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "epub has no rendition"},
		}, nil
	}
	entries := make([]openapi.EpubSpineEntry, 0, len(root.Spine.Itemrefs))
	// Build href → label maps from the EPUB 3 nav.xhtml AND
	// EPUB 2 toc.ncx. goreader parses both but doesn't expose a
	// lookup helper (Item.Label is declared but never populated).
	// We do the lookup ourselves so the picker shows real chapter
	// titles instead of opaque manifest IDs like "id1".
	navLabels := buildNavLabels(root.NavDoc.Nav)
	ncxLabels := buildNCXLabels(root.NCX.NavPoints)

	for i, ir := range root.Spine.Itemrefs {
		if ir.Item == nil {
			continue
		}
		// Resolve label, best signal first:
		//   1. EPUB 3 nav.xhtml entry matching this href
		//   2. EPUB 2 toc.ncx navPoint matching this href
		//   3. Chapter HTML <title> tag (sniffed on demand)
		//   4. Manifest id (last-resort — usually "id1" / "p001")
		//   5. "Chapter N"
		href := ir.Item.HREF
		label := navLabels[href]
		if label == "" {
			label = ncxLabels[href]
		}
		if label == "" {
			label = sniffChapterTitle(ir.Item)
		}
		if label == "" {
			label = strings.TrimSpace(ir.Item.ID)
		}
		if label == "" || looksLikeOpaqueID(label) {
			label = fmt.Sprintf("Chapter %d", i+1)
		}
		entries = append(entries, openapi.EpubSpineEntry{
			Idx:       i,
			Label:     label,
			Href:      ir.Item.HREF,
			MediaType: ir.Item.MediaType,
		})
	}
	if h.epubSpine != nil {
		h.epubSpine.Add(cacheKey, entries)
	}
	return openapi.GetEpubSpine200JSONResponse(entries), nil
}

// ---------------------------------------------------------------------------
// GET /assets/{id}/epub/chapters/{idx}
// ---------------------------------------------------------------------------

func (h *Handler) GetEpubChapter(
	ctx context.Context,
	req openapi.GetEpubChapterRequestObject,
) (openapi.GetEpubChapterResponseObject, error) {
	if auth.IdentityFromContext(ctx) == nil {
		return openapi.GetEpubChapter401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}

	cacheKey := fmt.Sprintf("%s:%d", req.Id, req.Idx)
	if h.epubChapters != nil {
		if v, ok := h.epubChapters.Get(cacheKey); ok {
			return openapi.GetEpubChapter200TexthtmlResponse{
				Body:          io.NopCloser(bytes.NewReader(v)),
				ContentLength: int64(len(v)),
			}, nil
		}
	}

	r, opfDir, _, err := h.openEPUB(ctx, req.Id)
	if err != nil {
		return mapEPUBErrToChapterResponse(err), nil
	}

	root := r.Container.DefaultRendition()
	if root == nil || req.Idx < 0 || req.Idx >= len(root.Spine.Itemrefs) {
		return openapi.GetEpubChapter404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "chapter index out of range"},
		}, nil
	}
	ref := root.Spine.Itemrefs[req.Idx]
	if ref.Item == nil {
		return openapi.GetEpubChapter404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "chapter manifest entry missing"},
		}, nil
	}
	rc, err := ref.Item.Open()
	if err != nil {
		return openapi.GetEpubChapter404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "chapter body unreadable: " + err.Error()},
		}, nil
	}
	defer rc.Close()

	raw, err := io.ReadAll(io.LimitReader(rc, maxEPUBBytes))
	if err != nil {
		return nil, fmt.Errorf("assets: epub chapter read: %w", err)
	}

	// Chapter href is relative to the OPF directory. We need it
	// absolute (zip-internal) so rewritten resource URLs resolve.
	chapterAbs := path.Join(opfDir, ref.Item.HREF)
	rewritten, err := rewriteChapter(raw, req.Id.String(), path.Dir(chapterAbs))
	if err != nil {
		return nil, fmt.Errorf("assets: epub chapter rewrite: %w", err)
	}

	if h.epubChapters != nil {
		h.epubChapters.Add(cacheKey, rewritten)
	}
	return openapi.GetEpubChapter200TexthtmlResponse{
		Body:          io.NopCloser(bytes.NewReader(rewritten)),
		ContentLength: int64(len(rewritten)),
	}, nil
}

// ---------------------------------------------------------------------------
// GET /assets/{id}/epub/resources/{path}
// ---------------------------------------------------------------------------

func (h *Handler) GetEpubResource(
	ctx context.Context,
	req openapi.GetEpubResourceRequestObject,
) (openapi.GetEpubResourceResponseObject, error) {
	if auth.IdentityFromContext(ctx) == nil {
		return openapi.GetEpubResource401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}

	// Path traversal guard. Any '..' segment is rejected outright —
	// resources are looked up by their zip-internal path, which
	// goreader populated from the manifest, so traversal can't
	// resolve to anything legitimate.
	clean := path.Clean("/" + req.Path)
	if clean == "/" || strings.Contains(req.Path, "..") {
		return openapi.GetEpubResource400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "bad resource path"},
		}, nil
	}
	want := strings.TrimPrefix(clean, "/")

	r, opfDir, _, err := h.openEPUB(ctx, req.Id)
	if err != nil {
		return mapEPUBErrToResourceResponse(err), nil
	}

	// Walk the default rendition's manifest looking for the item
	// whose resolved path matches. Resolution is the same as
	// goreader's setItems — path.Join(opfDir, item.HREF).
	root := r.Container.DefaultRendition()
	if root == nil {
		return openapi.GetEpubResource404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "epub has no rendition"},
		}, nil
	}
	var found *epub.Item
	for i := range root.Manifest.Items {
		it := &root.Manifest.Items[i]
		abs := path.Join(opfDir, it.HREF)
		if abs == want {
			found = it
			break
		}
	}
	if found == nil {
		return openapi.GetEpubResource404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "resource not in manifest"},
		}, nil
	}
	rc, err := found.Open()
	if err != nil {
		return openapi.GetEpubResource404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "resource unreadable: " + err.Error()},
		}, nil
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, maxEPUBBytes))
	if err != nil {
		return nil, fmt.Errorf("assets: epub resource read: %w", err)
	}

	contentType := found.MediaType
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	return openapi.GetEpubResource200ApplicationoctetStreamResponse{
		Body:          io.NopCloser(bytes.NewReader(data)),
		ContentLength: int64(len(data)),
		Headers: openapi.GetEpubResource200ResponseHeaders{
			ContentType: &contentType,
		},
	}, nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// openEPUB streams the asset's source bytes into memory and
// returns a parsed reader along with the OPF directory (needed
// for resolving relative paths to zip-internal absolutes). The
// reader doesn't need closing — it holds no OS handles, just
// references into the byte slice.
func (h *Handler) openEPUB(ctx context.Context, assetID openapi_types.UUID) (*epub.Reader, string, string, error) {
	q := New(h.Pool)
	row, err := q.GetAsset(ctx, pgtype.UUID{Bytes: uuid.UUID(assetID), Valid: true})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, "", "", errEPUBNotFound
		}
		return nil, "", "", fmt.Errorf("get asset: %w", err)
	}
	if row.FileHash == nil || *row.FileHash == "" {
		return nil, "", "", errEPUBNotFound
	}
	body, _, err := h.Storage.Download(ctx, *row.FileHash, storage.VariantOriginal)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, "", "", errEPUBNotFound
		}
		return nil, "", "", fmt.Errorf("download: %w", err)
	}
	defer body.Close()
	raw, err := io.ReadAll(io.LimitReader(body, maxEPUBBytes))
	if err != nil {
		return nil, "", "", fmt.Errorf("read source: %w", err)
	}
	r, err := epub.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, "", "", fmt.Errorf("epub parse: %w", err)
	}
	root := r.Container.DefaultRendition()
	if root == nil {
		return nil, "", "", errEPUBNotFound
	}
	return r, path.Dir(root.FullPath), root.FullPath, nil
}

// errEPUBNotFound is the sentinel mapped to 404 in each handler
// shim. Keeps the openEPUB signature single-error.
var errEPUBNotFound = errors.New("epub: asset / file / rendition not found")

// rewriteChapter parses the chapter XHTML and rewrites every
// relative href/src/srcset that doesn't already start with a
// scheme or with our resource prefix. Resolved paths target
// /api/v1/assets/{id}/epub/resources/{absZipPath} so the iframe
// fetches resources through us (single-origin = no CORS issues +
// auth cookies travel).
func rewriteChapter(raw []byte, assetID, chapterDir string) ([]byte, error) {
	doc, err := html.Parse(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	rewriteNode(doc, assetID, chapterDir)
	var buf bytes.Buffer
	if err := html.Render(&buf, doc); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func rewriteNode(n *html.Node, assetID, chapterDir string) {
	if n.Type == html.ElementNode {
		for i := range n.Attr {
			a := &n.Attr[i]
			switch strings.ToLower(a.Key) {
			case "href", "src":
				a.Val = rewriteURL(a.Val, assetID, chapterDir)
			case "srcset":
				a.Val = rewriteSrcset(a.Val, assetID, chapterDir)
			}
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		rewriteNode(c, assetID, chapterDir)
	}
}

func rewriteURL(raw, assetID, chapterDir string) string {
	if raw == "" {
		return raw
	}
	// Leave absolute URLs and fragment-only refs alone. Mailto /
	// javascript / data URIs likewise pass through.
	if strings.HasPrefix(raw, "#") {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if u.Scheme != "" || u.Host != "" {
		return raw
	}
	if strings.HasPrefix(u.Path, "/") {
		return raw
	}
	// Resolve relative to the chapter's directory in the zip.
	abs := path.Join(chapterDir, u.Path)
	// URL-encode each path segment so non-ASCII filenames + spaces
	// survive the round-trip through the resource handler.
	parts := strings.Split(abs, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	out := "/api/v1/assets/" + assetID + "/epub/resources/" + strings.Join(parts, "/")
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	if u.Fragment != "" {
		out += "#" + u.Fragment
	}
	return out
}

func rewriteSrcset(raw, assetID, chapterDir string) string {
	// srcset format: "url1 1x, url2 2x" — rewrite the URL portion
	// of each comma-separated entry; keep the descriptor as-is.
	parts := strings.Split(raw, ",")
	for i, part := range parts {
		seg := strings.TrimSpace(part)
		if seg == "" {
			continue
		}
		fields := strings.Fields(seg)
		if len(fields) == 0 {
			continue
		}
		fields[0] = rewriteURL(fields[0], assetID, chapterDir)
		parts[i] = strings.Join(fields, " ")
	}
	return strings.Join(parts, ", ")
}

// ---------------------------------------------------------------------------
// GET /assets/{id}/epub/search?q=...
// ---------------------------------------------------------------------------

// snippetRadius — number of characters of context to render on
// each side of a match in the search snippet.
const snippetRadius = 60

func (h *Handler) SearchEpub(
	ctx context.Context,
	req openapi.SearchEpubRequestObject,
) (openapi.SearchEpubResponseObject, error) {
	if auth.IdentityFromContext(ctx) == nil {
		return openapi.SearchEpub401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	q := strings.TrimSpace(req.Params.Q)
	if len(q) < 2 {
		return openapi.SearchEpub200JSONResponse([]openapi.EpubSearchHit{}), nil
	}
	qLower := strings.ToLower(q)

	r, _, _, err := h.openEPUB(ctx, req.Id)
	if err != nil {
		if errors.Is(err, errEPUBNotFound) {
			return openapi.SearchEpub404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "asset not found"},
			}, nil
		}
		return nil, err
	}
	root := r.Container.DefaultRendition()
	if root == nil {
		return openapi.SearchEpub404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "epub has no rendition"},
		}, nil
	}

	hits := make([]openapi.EpubSearchHit, 0)
	// Scan every spine entry. Cheap on the typical novel (100s of
	// chapters × tens of KB); long enough to be I/O-bound on big
	// scientific or scanned EPUBs but those are rare and we cap
	// per-chapter read at maxEPUBBytes anyway.
	for i, ref := range root.Spine.Itemrefs {
		if ref.Item == nil {
			continue
		}
		body, err := chapterPlainText(ref.Item)
		if err != nil {
			// Skip unreadable chapters rather than failing the
			// whole search; common cause is binary-ish item types
			// (NCX / OPF / cover image) accidentally in the spine.
			continue
		}
		bodyLower := strings.ToLower(body)
		// Per-chapter hit cap so a single chapter can't drown the
		// results page. The user can refine the query to widen.
		const perChapterCap = 5
		hitsInChapter := 0
		start := 0
		for {
			off := strings.Index(bodyLower[start:], qLower)
			if off < 0 {
				break
			}
			absOff := start + off
			start = absOff + len(qLower)
			snip := snippetAround(body, absOff, len(q))
			label := strings.TrimSpace(ref.Item.Label)
			if label == "" {
				label = fmt.Sprintf("Chapter %d", i+1)
			}
			hits = append(hits, openapi.EpubSearchHit{
				ChapterIdx:   i,
				ChapterLabel: label,
				Snippet:      snip,
				CharOffset:   absOff,
			})
			hitsInChapter++
			if hitsInChapter >= perChapterCap {
				break
			}
		}
	}

	// Global cap so the JSON response stays sane.
	const maxHits = 200
	if len(hits) > maxHits {
		hits = hits[:maxHits]
	}
	return openapi.SearchEpub200JSONResponse(hits), nil
}

// ---------------------------------------------------------------------------
// TOC label resolution helpers
// ---------------------------------------------------------------------------

// buildNavLabels walks an EPUB 3 nav.xhtml tree into a flat
// href → label map. Nav links can carry a #fragment for sub-
// sections; we key on the pre-fragment href so a chapter file
// gets ONE label even when the nav links into it multiple times
// (intro paragraph, section A, section B). First-wins so the
// top-most nav entry takes priority.
func buildNavLabels(roots []epub.Nav) map[string]string {
	out := map[string]string{}
	var walk func(items []epub.ListItem)
	walk = func(items []epub.ListItem) {
		for _, it := range items {
			href := stripFragment(it.Link.Href)
			text := strings.TrimSpace(it.Link.Text)
			if href != "" && text != "" {
				if _, exists := out[href]; !exists {
					out[href] = text
				}
			}
			if it.SubItems != nil {
				walk(*it.SubItems)
			}
		}
	}
	for _, nav := range roots {
		walk(nav.Items)
	}
	return out
}

// buildNCXLabels does the same for EPUB 2 toc.ncx navPoints.
func buildNCXLabels(roots []epub.NavPoint) map[string]string {
	out := map[string]string{}
	var walk func(points []epub.NavPoint)
	walk = func(points []epub.NavPoint) {
		for _, p := range points {
			href := stripFragment(p.Content.Src)
			text := strings.TrimSpace(p.NavLabel.Text)
			if href != "" && text != "" {
				if _, exists := out[href]; !exists {
					out[href] = text
				}
			}
			if len(p.NavPoints) > 0 {
				walk(p.NavPoints)
			}
		}
	}
	walk(roots)
	return out
}

func stripFragment(href string) string {
	if i := strings.Index(href, "#"); i >= 0 {
		return href[:i]
	}
	return href
}

// sniffChapterTitle reads the first text content out of a
// chapter's <h1>, <h2>, <h3>, or <title> — whichever fires
// first. Strong signal when nav.xhtml + ncx come up empty
// (the manifest ID is usually opaque). Costs one extra zip-read
// per chapter on the spine call — the spine result is cached so
// subsequent hits are free.
func sniffChapterTitle(item *epub.Item) string {
	rc, err := item.Open()
	if err != nil {
		return ""
	}
	defer rc.Close()
	raw, err := io.ReadAll(io.LimitReader(rc, 256*1024))
	if err != nil {
		return ""
	}
	doc, err := html.Parse(bytes.NewReader(raw))
	if err != nil {
		return ""
	}
	// Prefer first heading inside <body> (chapter titles usually
	// live there). Fall back to <head><title> only when no
	// heading exists, since <title> often carries the book
	// title rather than the chapter's.
	if h := findFirstText(doc, "h1", "h2", "h3"); h != "" {
		return collapseSpaces(h)
	}
	if t := findFirstText(doc, "title"); t != "" {
		return collapseSpaces(t)
	}
	return ""
}

func findFirstText(n *html.Node, tags ...string) string {
	wanted := map[string]struct{}{}
	for _, t := range tags {
		wanted[t] = struct{}{}
	}
	var hit string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if hit != "" || n == nil {
			return
		}
		if n.Type == html.ElementNode {
			if _, ok := wanted[strings.ToLower(n.Data)]; ok {
				hit = innerText(n)
				if strings.TrimSpace(hit) != "" {
					return
				}
				hit = ""
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return hit
}

func innerText(n *html.Node) string {
	var sb strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			sb.WriteString(n.Data)
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return sb.String()
}

// looksLikeOpaqueID flags labels we'd rather render as "Chapter N"
// instead of as-is. Catches the common patterns: short alphanumeric
// identifiers ("id1", "p001", "htp01", "ch04"), bare filenames
// ("chapter01.xhtml"), and pure numbers. The user doesn't want to
// scan a TOC of "ch01 / ch02 / ch03"; they want "Chapter 1 / 2 / 3".
func looksLikeOpaqueID(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return true
	}
	// Filename — has a dot AND no space (real titles can have
	// dots in "Mr. So-and-So" but never without a space too).
	if strings.Contains(s, ".") && !strings.Contains(s, " ") {
		return true
	}
	// Short identifier — ≤ 8 chars AND no spaces AND no
	// non-ASCII (real titles often have punctuation / unicode).
	if len(s) <= 8 && !strings.ContainsAny(s, " \t-—:?!") {
		ascii := true
		for _, r := range s {
			if r > 127 {
				ascii = false
				break
			}
		}
		if ascii {
			return true
		}
	}
	return false
}

// chapterPlainText reads an EPUB item, strips HTML tags, collapses
// whitespace. Used by the search endpoint. Doesn't need to be
// pixel-perfect — we just need a searchable corpus + a way to pick
// snippets out of it.
func chapterPlainText(item *epub.Item) (string, error) {
	rc, err := item.Open()
	if err != nil {
		return "", err
	}
	defer rc.Close()
	raw, err := io.ReadAll(io.LimitReader(rc, maxEPUBBytes))
	if err != nil {
		return "", err
	}
	doc, err := html.Parse(bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	sb.Grow(len(raw) / 2)
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			sb.WriteString(n.Data)
			sb.WriteByte(' ')
			return
		}
		// Skip script + style — their text payloads aren't reader
		// content and would pollute search hits with CSS / JS.
		if n.Type == html.ElementNode {
			tag := strings.ToLower(n.Data)
			if tag == "script" || tag == "style" {
				return
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return collapseSpaces(sb.String()), nil
}

func collapseSpaces(s string) string {
	var out strings.Builder
	out.Grow(len(s))
	prevSpace := true // suppress leading whitespace
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\u00A0' {
			if !prevSpace {
				out.WriteByte(' ')
				prevSpace = true
			}
			continue
		}
		out.WriteRune(r)
		prevSpace = false
	}
	return strings.TrimSpace(out.String())
}

func snippetAround(text string, matchOff, matchLen int) string {
	start := matchOff - snippetRadius
	if start < 0 {
		start = 0
	}
	end := matchOff + matchLen + snippetRadius
	if end > len(text) {
		end = len(text)
	}
	snip := text[start:end]
	if start > 0 {
		snip = "…" + snip
	}
	if end < len(text) {
		snip = snip + "…"
	}
	return snip
}

// ── per-handler 404 mappers ────────────────────────────────────

func mapEPUBErrToSpineResponse(err error) openapi.GetEpubSpineResponseObject {
	if errors.Is(err, errEPUBNotFound) {
		return openapi.GetEpubSpine404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "asset not found"},
		}
	}
	return openapi.GetEpubSpine500JSONResponse{
		InternalErrorJSONResponse: openapi.InternalErrorJSONResponse{Error: err.Error()},
	}
}
func mapEPUBErrToChapterResponse(err error) openapi.GetEpubChapterResponseObject {
	if errors.Is(err, errEPUBNotFound) {
		return openapi.GetEpubChapter404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "asset not found"},
		}
	}
	return openapi.GetEpubChapter500JSONResponse{
		InternalErrorJSONResponse: openapi.InternalErrorJSONResponse{Error: err.Error()},
	}
}
func mapEPUBErrToResourceResponse(err error) openapi.GetEpubResourceResponseObject {
	if errors.Is(err, errEPUBNotFound) {
		return openapi.GetEpubResource404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "asset not found"},
		}
	}
	return openapi.GetEpubResource500JSONResponse{
		InternalErrorJSONResponse: openapi.InternalErrorJSONResponse{Error: err.Error()},
	}
}
