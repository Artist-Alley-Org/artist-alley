package email

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/jobs"
)

// JobTypeNotificationEmail is the canonical job type the
// notifications package enqueues on every Notify() where the
// recipient's prefs include "email". The handler renders + sends
// via the configured Sender.
const JobTypeNotificationEmail jobs.JobType = "notification.email"

// NotificationJobPayload mirrors notifications.emailJobPayload —
// kept here so the email package doesn't import notifications
// (would invert the dependency edge). The notifications writer
// marshals from its own struct; both sides agree on JSON tags.
type NotificationJobPayload struct {
	RecipientUserRef int64          `json:"recipient_user_ref"`
	Verb             string         `json:"verb"`
	TargetKind       string         `json:"target_kind,omitempty"`
	TargetID         string         `json:"target_id,omitempty"`
	Payload          map[string]any `json:"payload,omitempty"`
}

// SiteContextProvider returns the rendering-time site context
// every template merges into its data (site name + URL). Kept
// behind an interface so the email package doesn't import
// sysconfig directly — boot wires a closure over sysconfig.Store.
type SiteContextProvider func(ctx context.Context) (SiteContext, error)

// SiteContext is the small per-instance struct templates expect
// to find as `{{.site_name}}` / `{{.site_url}}`.
type SiteContext struct {
	Name string
	URL  string
}

// NotificationJobHandler implements [jobs.Handler] for
// notification.email. Stateless; one per process.
type NotificationJobHandler struct {
	pool   *pgxpool.Pool
	sender Sender
	site   SiteContextProvider
	logger *slog.Logger
}

// NewNotificationJobHandler wires the handler. Any nil dep
// degrades gracefully:
//
//   - nil sender → returns terminal "no sender wired" on Handle
//   - nil site → empty site context (templates render with empty
//     placeholders rather than failing — operators see misconfigured
//     emails go out and fix them)
func NewNotificationJobHandler(pool *pgxpool.Pool, sender Sender, site SiteContextProvider, logger *slog.Logger) *NotificationJobHandler {
	return &NotificationJobHandler{
		pool:   pool,
		sender: sender,
		site:   site,
		logger: logger,
	}
}

// Type implements [jobs.Handler].
func (h *NotificationJobHandler) Type() jobs.JobType { return JobTypeNotificationEmail }

// Handle implements [jobs.Handler]. Errors classify per
// [jobs.IsTerminal]:
//
//   - bad payload, unknown recipient, no email on file →
//     TerminalError (no retry; nothing the queue can do to fix).
//   - SMTP transient (network, 4xx) → plain error → retry per
//     the job framework's backoff.
//   - ErrNotConfigured → terminal, since retries won't configure
//     the relay.
func (h *NotificationJobHandler) Handle(ctx context.Context, job *jobs.Claim) (json.RawMessage, error) {
	if h.sender == nil {
		return nil, &jobs.TerminalError{Err: errors.New("notification.email: no sender wired")}
	}
	var p NotificationJobPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return nil, &jobs.TerminalError{Err: fmt.Errorf("notification.email: parse payload: %w", err)}
	}

	recipient, err := h.lookupRecipient(ctx, p.RecipientUserRef)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, &jobs.TerminalError{Err: fmt.Errorf("notification.email: recipient %d not found", p.RecipientUserRef)}
		}
		return nil, fmt.Errorf("notification.email: lookup recipient: %w", err)
	}
	if recipient.Email == "" {
		// User has no email on file — nothing to do, no retries.
		if h.logger != nil {
			h.logger.LogAttrs(ctx, slog.LevelInfo,
				"notification.email.skip_no_email",
				slog.Int64("recipient_user_ref", p.RecipientUserRef),
				slog.String("verb", p.Verb),
			)
		}
		return jsonMarshal(map[string]any{"sent": false, "reason": "no_email"}), nil
	}

	// Merge site context into the template data. Caller payload
	// wins when a key collides.
	data := map[string]any{
		"recipient_name": recipient.Name,
		"verb":           p.Verb,
		"target_kind":    p.TargetKind,
		"target_id":      p.TargetID,
	}
	if h.site != nil {
		if sc, err := h.site(ctx); err == nil {
			data["site_name"] = sc.Name
			data["site_url"] = sc.URL
		}
	}
	for k, v := range p.Payload {
		data[k] = v
	}

	msg, err := Render(templateForVerb(p.Verb), []string{recipient.Email}, data)
	if err != nil {
		return nil, &jobs.TerminalError{Err: fmt.Errorf("notification.email: render: %w", err)}
	}

	if err := h.sender.Send(ctx, msg); err != nil {
		if errors.Is(err, ErrNotConfigured) {
			// SMTP not configured — terminal, no retry.
			return jsonMarshal(map[string]any{"sent": false, "reason": "not_configured"}),
				&jobs.TerminalError{Err: err}
		}
		// Transient: let the job framework retry.
		return nil, fmt.Errorf("notification.email: send: %w", err)
	}
	if h.logger != nil {
		h.logger.LogAttrs(ctx, slog.LevelInfo,
			"notification.email.sent",
			slog.Int64("recipient_user_ref", p.RecipientUserRef),
			slog.String("verb", p.Verb),
		)
	}
	return jsonMarshal(map[string]any{"sent": true, "verb": p.Verb}), nil
}

// recipientInfo is the small projection the handler needs.
type recipientInfo struct {
	Email string
	Name  string
}

// lookupRecipient pulls email + a display name for the user.
// Display fall-back chain: fullname → username → "you".
func (h *NotificationJobHandler) lookupRecipient(ctx context.Context, ref int64) (recipientInfo, error) {
	var email, fullname, username *string
	err := h.pool.QueryRow(ctx, `
		SELECT email, fullname, username FROM "user" WHERE ref = $1
	`, ref).Scan(&email, &fullname, &username)
	if err != nil {
		return recipientInfo{}, err
	}
	out := recipientInfo{}
	if email != nil {
		out.Email = strings.TrimSpace(*email)
	}
	switch {
	case fullname != nil && strings.TrimSpace(*fullname) != "":
		out.Name = strings.TrimSpace(*fullname)
	case username != nil && strings.TrimSpace(*username) != "":
		out.Name = strings.TrimSpace(*username)
	default:
		out.Name = "you"
	}
	return out, nil
}

// templateForVerb maps a notification verb to a template name.
// Per-verb templates land incrementally; absent ones fall back
// to TemplateNotificationGeneric so every verb has SOMETHING to
// render.
func templateForVerb(verb string) string {
	candidate := "notification_" + strings.ReplaceAll(strings.ToLower(verb), ".", "_")
	if _, ok := registry[candidate]; ok {
		return candidate
	}
	return TemplateNotificationGeneric
}

func jsonMarshal(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// Compile-time assertion.
var _ jobs.Handler = (*NotificationJobHandler)(nil)
