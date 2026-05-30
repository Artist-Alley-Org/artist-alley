# ADR 0022: Chat platform integrations — Slack first, abstracted

- Date: 2026-05-30
- Status: Accepted

## Context

Studios live in Slack (most), Microsoft Teams (some), and Discord
(indies + community-facing studios). Notifications from Artist Alley
need to flow into those channels — review-request announcements,
upload notifications, approval state changes, sensitivity-tier
transitions, share-link views, comment mentions.

Email + in-app notifications are not enough. A reviewer in Slack who
sees "Kenneth pushed character_v3 for review" wants a one-click route
into the post-detail modal. Conversely, posting an Artist Alley link
into a Slack channel should unfurl with the cover thumbnail + post
title + author + state — like Linear / Figma / GitHub do.

## Decision

Add Phase 1.30 — Chat platform integrations — starting with Slack and
designed for trivial extension to Teams + Discord via the same
provider abstraction used in ADR 0021 for content platforms. Different
interface (notification-class, not content-class) but the same
package-per-provider shape.

### Provider model

```go
type ChatProvider interface {
    Name() string                                   // "slack", "teams", "discord"
    AuthFlow() AuthFlow                             // OAuth + bot install
    SendMessage(ctx, target, payload) error
    HandleSlashCommand(ctx, cmd) (Response, error)
    Unfurl(ctx, url) (UnfurlPayload, error)         // link-preview when our URLs appear
    HandleWebhook(ctx, payload) ([]Event, error)
}
```

### Slack first

- **Slack App** distributed through the Slack app directory once
  stable. During v0.1 era: distributed as a self-install workspace
  app keyed to the customer's signing secret.
- **Outbound:** rich Block Kit messages for every notifiable event
  (review request, approval state change, sensitivity reveal, share-
  link view, mention in comment). Each message carries deep-link
  buttons.
- **Inbound:** slash commands.
  - `/aa search <query>` — search Artist Alley, returns top results
    in an ephemeral message.
  - `/aa post <post-id>` — post a public link into the channel,
    unfurled with cover + metadata.
  - `/aa upload` — opens the upload modal in a new tab (deep link).
- **Unfurl:** when an Artist Alley URL appears in any Slack message
  the bot is in, the workspace shows a rich preview without
  divulging restricted content (a blur badge if sensitivity gates
  the URL).
- **DM notifications:** per-user opt-in. Users connect their Slack
  account in account settings.

### Channel routing rules

Admin surface defines per-event channel routing:

```
event:upload.completed in team:Concept Art → #concept-art-feed
event:review.requested for user:art-director → DM art-director
event:share-link.opened where target.sensitivity=embargo → #security-audit
```

The rules engine is a small DSL that mirrors the existing
notification-rule shape coming in Phase 1.18 — chat is a delivery
channel within that subsystem.

### Auth

- Slack: standard OAuth 2.0 with the `chat:write`, `commands`,
  `links:write`, `links:read`, `app_mentions:read` scopes.
- Workspace tokens stored encrypted at rest in the local DB.
- Signing secret validates every inbound Slack request.

### Teams + Discord later

The interface is designed so Teams + Discord providers can land in
follow-up sub-phases (1.30.B Teams, 1.30.C Discord) without touching
the call sites. Each adds a package + an OAuth client.

### Federation interaction

A federated peer notifies its own Slack workspace for events that
happen on the local instance. Cross-instance chat routing would be a
v2 enhancement.

### Per-tier behaviour

- **Community:** Slack only, single workspace, default channel routing.
- **Pro:** Slack + Teams + Discord, multiple workspaces, custom
  channel routing rules.
- **Enterprise:** Pro plus organization-owned Slack app (using their
  own signing secret instead of the artist-alley-issued shared app).

Enforced via the tangled-derivation model from ADR 0017.

## Consequences

**Positive**

- Brings Artist Alley into the studio's actual operating surface
  without forcing a context switch.
- Slash-command + unfurl pattern is what every modern dev tool
  (Linear, Figma, GitHub) does — reviewers already know how to use it.
- The chat-provider abstraction parallels the content-platform
  abstraction in ADR 0021; adding new chat platforms is bounded work.

**Negative**

- Slack app distribution + the app directory submission is real ops
  work, not just code. Mitigation: pre-directory, ship as a self-
  install workspace app with operator-supplied secrets.
- Slack API rate limits constrain notification bursts. Mitigation:
  outbound message queue + per-channel rate-limit awareness in the
  delivery layer.

## Alternatives considered

- **Pure webhook out (no Slack app).** Loses slash commands, unfurl,
  and DM routing. Rejected — half-measure.
- **One opinionated platform.** Studios use whichever is in their
  Microsoft / Google / Slack contract. Multi-provider is table stakes.
- **Build internal user-to-user messaging within Artist Alley.**
  Already rejected in the audit: studios use Slack/Discord/Teams;
  building a chat surface is rebuilding what every customer already
  has. Confirmed via user override 2026-05-30 — they want Slack
  *integration*, not internal messaging.

## Reference

- Phase 1.30 in [`docs/roadmap.md`](../roadmap.md).
- Notification rules (Phase 1.18 Integrations) consume chat providers
  as one of the delivery channels alongside email + in-app + webhook.
- ADR 0021 (external platform integrations) for the parallel
  content-platform abstraction.
