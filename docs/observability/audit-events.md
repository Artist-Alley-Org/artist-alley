# Audit event authoring guide

The audit_events table is the operator-facing log of "who did what,
when". Each event has a typed string constant in
`app/internal/audit/events.go` and a corresponding method on
`*audit.Recorder`. This doc is the convention any new event author
follows so the log stays grep-able + the changeset shape stays
uniform across surfaces.

## Two write paths

There are two ways to emit an audit event. Pick based on the
operator question the event answers.

### Path A — typed flat-metadata methods

For events that represent a single discrete action with a few
parameters. Examples: `CapabilityGranted`, `UserStatusChanged`,
`FederationOutboxRequeued`.

```go
// Add the const in events.go.
EventCapabilityGranted = "user.capability_granted"

// Add the typed Recorder method with explicit parameters.
func (r *Recorder) CapabilityGranted(ctx context.Context, req *http.Request,
    subjectUserRef, actorUserRef int64, capability, teamID, note string) {
    r.write(ctx, EventCapabilityGranted, &subjectUserRef, &actorUserRef,
        ctxFromRequest(req), map[string]any{
            "capability": capability,
            "team_id":    teamID,
            "note":       note,
        })
}
```

Use this when the parameter list is closed + small (≤ 5 fields)
and the operator question is "did this specific action happen, and
with what arguments?". The metadata stays flat — easy to query via
`metadata->>'capability' = 'system.admin'`.

### Path B — `RecordChange` reflective changeset

For events that represent a field-level edit of a structured
object. Examples: site config saves, SMTP rotations, user profile
updates. Phase 1.17.D introduced this path.

```go
// Add the const in events.go.
EventAdminSiteConfigUpdated = "admin.system.site_config_updated"

// In the handler:
before, _ := h.Store.GetSite(ctx)
// ... apply patch + write ...
h.Audit.RecordChange(ctx, auth.RequestFromContext(ctx),
    audit.EventAdminSiteConfigUpdated,
    nil /* subject */, &actorRef,
    &before, &after, nil /* extra */)
```

Use this when the operator question is "what changed?" — they
want a per-field diff, not the full new state.

#### Changeset shape

The diff lands as a nested `changeset` key inside `metadata`:

```json
{
  "changeset": {
    "SiteName":  {"before": "Old", "after": "New"},
    "BaseURL":   {"before": "https://a/", "after": "https://b/"}
  }
}
```

- Only changed fields appear. Unchanged fields are omitted.
- If every field equals between `before` and `after`, the
  `changeset` key is omitted entirely — but the event still
  emits. "Admin saved the form, nothing changed" is itself a
  meaningful audit signal.

#### Sensitive-field stripping

Two mechanisms strip values that must never appear in audit:

1. **Primary — `audit:"-"` struct tag.** Explicit per-field opt-out:
   ```go
   type AIConfig struct {
       DefaultProviderID string `json:"default_provider_id"`
       Providers []AIProvider `json:"providers" audit:"-"` // strip
   }
   ```
   Use this when the field is structurally sensitive (slice of
   secret-bearing structs, opaque secret blob, etc.). This is
   the primary mechanism; reach for it whenever you author a
   new struct destined for `RecordChange`.

2. **Backstop — name-pattern match.** Field names containing
   any of:
   ```
   password, hash, secret, privatekey, token,
   apikey, masterkey, encryption, signing
   ```
   (case-insensitive) get stripped even without the tag. Defense
   in depth.

   Conservative on purpose: a field named `PasswordPolicy` or
   `EncryptionMode` is also stripped. Operators lose the per-field
   diff signal for those fields; they still see the event row
   and can read the new value via the API. If a legit field
   really needs to appear, prefer **renaming** over loosening
   the pattern — the pattern catches a lot of real-world
   sensitive cases without per-field author discipline.

#### Known limitations

- **Slice-element fields are diffed via `reflect.DeepEqual` at
  the slice level.** A single change in a `[]AIProvider` dumps
  the whole `before`/`after` slices, including any embedded
  `APIKey` strings. Tag the slice field `audit:"-"` (precedent:
  `AIConfig.Providers`, `AuthConfig.SSOProviders`).
- **Nested structs are reported as a single field.** The diff
  doesn't flatten to dotted paths; if `SubStruct.X` changes,
  the entry is `"SubStruct": {"before": <whole struct>, "after":
  <whole struct>}`. Operators see the change in context;
  reduces grep noise. Acceptable for MVP.
- **Per-event custom diff strategies** aren't supported. One
  helper fits all callers. If a real use case surfaces, revisit.

## Naming convention

| Surface          | Naming pattern                                    |
|------------------|---------------------------------------------------|
| Login / sessions | `login.<verb>` / `session.<verb>` / `logout`     |
| User lifecycle   | `user.<noun>` (e.g. `user.status_changed`)       |
| User-admin verbs | `admin.users.<verb>` (e.g. `admin.users.approved`) |
| System config    | `admin.system.<surface>_config_updated`           |
| Capability ops   | `user.capability_<verb>` (granted / revoked / etc.) |
| Federation       | `federation.<surface>.<verb>`                    |

Past tense for completed actions (`approved`, `granted`,
`config_updated`). Present-tense-imperative reserved for action
verbs the system rejected (none today — would be
`admin.users.refused_*`).

## Best-effort write semantics

Every audit emit is best-effort:

- A DB failure logs at WARN and the calling operation still
  completes successfully. The domain write is the source of
  truth; the audit log is observability.
- A serialization failure (the metadata `map[string]any` can't
  marshal to JSONB) writes `{}` instead of skipping the row,
  so the event_type + actor + timestamp survive.

Never wrap the audit emit in error-propagation code — the
operator's request must not fail because the audit pipeline did.

## Federation pass-through

Audit events are local-instance state. They do NOT federate.
Adding a new audit event does NOT add a new federation activity;
the two systems are orthogonal.

If you find yourself wanting to "log this remote action to audit"
for a federation activity, that's the existing
`federation.inbox.*` and `federation.outbox.*` event family —
add a new event in that namespace, don't reach across systems.
