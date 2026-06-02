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
	for i, ir := range root.Spine.Itemrefs {
		if ir.Item == nil {
			continue
		}
		// Per-chapter label: prefer the NCX/TOC label goreader
		// populated, fall back to the manifest id, fall back to
		// "Chapter N" so the picker is never empty.
		label := strings.TrimSpace(ir.Item.Label)
		if label == "" {
			label = strings.TrimSpace(ir.Item.ID)
		}
		if label == "" {
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
