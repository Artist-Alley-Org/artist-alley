package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// HTTPHandler exposes the queue over HTTP for external workers and
// federated peers. Each endpoint maps 1:1 onto a Service method; the
// handler is a thin marshaller.
type HTTPHandler struct {
	Service *Service
	Logger  *slog.Logger
}

func NewHTTPHandler(svc *Service, logger *slog.Logger) *HTTPHandler {
	return &HTTPHandler{Service: svc, Logger: logger}
}

// ---------------------------------------------------------------------------
// ClaimJobs (POST /jobs/claim)
// ---------------------------------------------------------------------------

func (h *HTTPHandler) ClaimJobs(ctx context.Context, req openapi.ClaimJobsRequestObject) (openapi.ClaimJobsResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil {
		return openapi.ClaimJobs401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	limit := 1
	var scopeTypes []JobType
	workerID := defaultWorkerID(caller)
	if req.Body != nil {
		if req.Body.Limit != nil {
			limit = *req.Body.Limit
		}
		if req.Body.WorkerId != nil && *req.Body.WorkerId != "" {
			workerID = *req.Body.WorkerId
		}
		if req.Body.Types != nil {
			for _, t := range *req.Body.Types {
				scopeTypes = append(scopeTypes, JobType(t))
			}
		}
	}
	claims, err := h.Service.ClaimBatch(ctx, workerID, scopeTypes, limit)
	if err != nil {
		return nil, err
	}
	out := make([]openapi.JobClaim, 0, len(claims))
	for _, c := range claims {
		var payload map[string]any
		if len(c.Payload) > 0 {
			_ = json.Unmarshal(c.Payload, &payload)
		}
		out = append(out, openapi.JobClaim{
			Id:           openapi_types.UUID(c.ID),
			Type:         string(c.Type),
			Payload:      payload,
			Attempts:     int(c.Attempts),
			MaxAttempts:  int(c.MaxAttempts),
			LeaseSeconds: h.Service.LeaseSeconds,
		})
	}
	return openapi.ClaimJobs200JSONResponse{Jobs: out}, nil
}

// ---------------------------------------------------------------------------
// GetJob (GET /jobs/{id})
// ---------------------------------------------------------------------------

func (h *HTTPHandler) GetJob(ctx context.Context, req openapi.GetJobRequestObject) (openapi.GetJobResponseObject, error) {
	if auth.IdentityFromContext(ctx) == nil {
		return openapi.GetJob401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	row, err := h.Service.GetByID(ctx, req.Id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.GetJob404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "job not found"},
			}, nil
		}
		return nil, err
	}
	return openapi.GetJob200JSONResponse(rowToJobAPI(row)), nil
}

// ---------------------------------------------------------------------------
// HeartbeatJob (POST /jobs/{id}/heartbeat)
// ---------------------------------------------------------------------------

func (h *HTTPHandler) HeartbeatJob(ctx context.Context, req openapi.HeartbeatJobRequestObject) (openapi.HeartbeatJobResponseObject, error) {
	if auth.IdentityFromContext(ctx) == nil {
		return openapi.HeartbeatJob401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if req.Body == nil || req.Body.WorkerId == "" {
		return openapi.HeartbeatJob401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "worker_id required"},
		}, nil
	}
	if err := h.Service.Heartbeat(ctx, req.Id, req.Body.WorkerId); err != nil {
		if errors.Is(err, ErrLeaseLost) {
			return openapi.HeartbeatJob409Response{}, nil
		}
		return nil, err
	}
	return openapi.HeartbeatJob204Response{}, nil
}

// ---------------------------------------------------------------------------
// CompleteJob (POST /jobs/{id}/complete)
// ---------------------------------------------------------------------------

func (h *HTTPHandler) CompleteJob(ctx context.Context, req openapi.CompleteJobRequestObject) (openapi.CompleteJobResponseObject, error) {
	if auth.IdentityFromContext(ctx) == nil {
		return openapi.CompleteJob401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if req.Body == nil || req.Body.WorkerId == "" {
		return openapi.CompleteJob401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "worker_id required"},
		}, nil
	}
	var result json.RawMessage
	if req.Body.Result != nil {
		b, err := json.Marshal(req.Body.Result)
		if err == nil {
			result = b
		}
	}
	if err := h.Service.Complete(ctx, req.Id, req.Body.WorkerId, result); err != nil {
		if errors.Is(err, ErrLeaseLost) {
			return openapi.CompleteJob409Response{}, nil
		}
		return nil, err
	}
	return openapi.CompleteJob204Response{}, nil
}

// ---------------------------------------------------------------------------
// FailJob (POST /jobs/{id}/fail)
// ---------------------------------------------------------------------------

func (h *HTTPHandler) FailJob(ctx context.Context, req openapi.FailJobRequestObject) (openapi.FailJobResponseObject, error) {
	if auth.IdentityFromContext(ctx) == nil {
		return openapi.FailJob401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if req.Body == nil || req.Body.WorkerId == "" {
		return openapi.FailJob401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "worker_id required"},
		}, nil
	}
	terminal := false
	if req.Body.Terminal != nil {
		terminal = *req.Body.Terminal
	}
	if err := h.Service.Fail(ctx, req.Id, req.Body.WorkerId, req.Body.Error, terminal); err != nil {
		if errors.Is(err, ErrLeaseLost) {
			return openapi.FailJob409Response{}, nil
		}
		return nil, err
	}
	return openapi.FailJob204Response{}, nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func defaultWorkerID(id *auth.Identity) string {
	return "user:" + strconv.FormatInt(id.UserRef, 10)
}

// rowToJobAPI converts the sqlc Job row into the openapi.Job shape.
func rowToJobAPI(r Job) openapi.Job {
	payload := unmarshalAny(r.Payload)
	j := openapi.Job{
		Id:          openapi_types.UUID(r.ID.Bytes),
		Type:        r.Type,
		Payload:     &payload,
		Status:      openapi.JobStatus(r.Status),
		Priority:    int(r.Priority),
		Attempts:    int(r.Attempts),
		MaxAttempts: int(r.MaxAttempts),
		EnqueuedAt:  r.EnqueuedAt.Time,
	}
	if r.ClaimedBy != nil {
		j.ClaimedBy = r.ClaimedBy
	}
	if r.ClaimedAt.Valid {
		t := r.ClaimedAt.Time
		j.ClaimedAt = &t
	}
	if r.LeaseExpiresAt.Valid {
		t := r.LeaseExpiresAt.Time
		j.LeaseExpiresAt = &t
	}
	if r.LastError != nil {
		j.LastError = r.LastError
	}
	if len(r.Result) > 0 {
		m := unmarshalAny(r.Result)
		j.Result = &m
	}
	if r.OriginServerID.Valid {
		u := openapi_types.UUID(r.OriginServerID.Bytes)
		j.OriginServerId = &u
	}
	if r.ScheduledFor.Valid {
		t := r.ScheduledFor.Time
		j.ScheduledFor = &t
	}
	if r.StartedAt.Valid {
		t := r.StartedAt.Time
		j.StartedAt = &t
	}
	if r.FinishedAt.Valid {
		t := r.FinishedAt.Time
		j.FinishedAt = &t
	}
	return j
}

func unmarshalAny(b []byte) map[string]any {
	if len(b) == 0 {
		return nil
	}
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}

// _ keeps `time` imported even if every reference above gets refactored.
var _ = time.Now
