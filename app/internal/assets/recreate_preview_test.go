package assets

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// RecreateAssetPreview shipped in 8bf4a75 (the AssetViewer's Edit-
// menu "Recreate previews" action). The DB-backed happy path lives
// in the integration tests; this file pins the guards that run
// before any DB work — they're what stops a missing-Identity request
// from leaking a 500 or a misconfigured handler from panicking on
// a nil Jobs deref.

func TestRecreateAssetPreview_Unauthenticated(t *testing.T) {
	h := &Handler{}
	resp, err := h.RecreateAssetPreview(context.Background(), openapi.RecreateAssetPreviewRequestObject{
		Id: openapi_types.UUID(uuid.New()),
	})
	if err != nil {
		t.Fatalf("RecreateAssetPreview: %v", err)
	}
	r, ok := resp.(openapi.RecreateAssetPreview401JSONResponse)
	if !ok {
		t.Fatalf("expected 401, got %T", resp)
	}
	if r.Error != "authentication required" {
		t.Errorf("error = %q", r.Error)
	}
}

// A handler that hasn't been wired with a Jobs service must surface
// the config bug as a 500, not nil-deref panic when the enqueue is
// attempted. Catching it before the DB call means we don't waste a
// query before failing.
func TestRecreateAssetPreview_NilJobsServiceIs500(t *testing.T) {
	h := &Handler{Jobs: nil}
	ctx := auth.WithIdentity(context.Background(), &auth.Identity{UserRef: 1})
	_, err := h.RecreateAssetPreview(ctx, openapi.RecreateAssetPreviewRequestObject{
		Id: openapi_types.UUID(uuid.New()),
	})
	if err == nil {
		t.Fatal("expected 500-style error for nil Jobs service")
	}
	if !strings.Contains(err.Error(), "jobs service not configured") {
		t.Errorf("error %q missing config hint", err.Error())
	}
}
