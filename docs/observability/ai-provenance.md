# AI tag provenance

How artist-alley records *which* model generated *which* tag, why
operator-set data is sacred across AI re-runs, and how to audit the
catalogue when a stale model leaves bad suggestions behind.

Audience: operator running the install. The frontend hides most of
this behind the per-source badge; the SQL paths below are what you
reach for when the UI isn't enough.

## What lands on `asset_tag` after an AI run

Migration `00010_asset_tag_provenance` extended the `asset_tag` row
with four columns:

| Column                 | Type    | Meaning |
| ---------------------- | ------- | ------- |
| `source`               | text    | `manual` / `ai` / `import` — enforced by CHECK constraint. Defaults to `manual` (covers every pre-migration row). |
| `confidence`           | real    | `[0.0, 1.0]` when the provider supplied a score; `NULL` otherwise. Operator and import tags always store `NULL`. |
| `created_by_provider`  | text    | Provider slug (e.g. `openai`, `claude`, `ollama`). `NULL` for manual + import. |
| `created_by_model`     | text    | Model identifier (e.g. `gpt-4o`, `claude-3-5-sonnet`). `NULL` for manual + import. |

The bridge layer (`app/internal/ai/bridge.go`) defines the typed enum
the Go code uses; the values match the CHECK constraint verbatim.

## Merge semantics

The only mode shipped in 1.14.A-bridge is **preserve_manual** (the
default; the key `ai.tag.merge_semantics` in `system_config` records
it, room left for `replace_all` / `additive_only` later).

What that means in one transaction:

```sql
-- inside SetAITagsForAsset
BEGIN;
  DELETE FROM asset_tag WHERE asset_id = $1 AND source = 'ai';
  -- ... INSERT one row per new AI tag, source='ai', with provenance
COMMIT;
```

The `WHERE source = 'ai'` is the load-bearing clause. Manual + import
rows are physically untouched — an AI re-run can never clobber tags
the operator typed in the UI or rows that arrived through a bulk
import.

The whole pair runs in one tx so the asset never has zero tags
during the gap. A failure on any insert rolls the whole thing back —
the old AI tags survive intact rather than being half-replaced.

## How the UI surfaces source

`web/src/lib/components/AssetTagBadge.svelte` renders one tag with a
tiny coloured marker so the operator can tell at a glance which
came from where. The component takes a `source` prop and is
backwards-compatible with the current flat-`string[]` read API
(passes default `source='manual'`); 1.14.B will surface real source
in the JSON projection and consumers swap the prop in.

The colour palette:

| Source   | Border tint | Marker |
| -------- | ----------- | ------ |
| `manual` | neutral     | grey dot |
| `ai`     | accent      | accent dot, percentage shown if confidence > 0 |
| `import` | warning     | warning dot |

A low-confidence (< 50%) AI tag flips the percentage text to
`text-danger` — the operator's "this is a stretchy suggestion" cue.

## Audit recipes

### Find every AI tag from a specific model

When a provider's model spec is deprecated and you want to clear out
its suggestions:

```sql
SELECT a.id, a.title, t.tag, t.confidence, t.added_at
FROM asset_tag t
JOIN assets    a ON a.id = t.asset_id
WHERE t.source            = 'ai'
  AND t.created_by_model  = 'gpt-3.5-turbo'
ORDER BY t.added_at DESC
LIMIT 100;
```

The partial index `idx_asset_tag_ai_provenance` (on
`(created_by_model, added_at DESC) WHERE source = 'ai'`) backs this
query so even on a large catalogue the response stays bounded.

### Operator override audit — "what did I write?"

When a teammate asks "did I really tag this asset or did the AI?":

```sql
SELECT asset_id, tag, added_at
FROM asset_tag
WHERE asset_id = '<uuid>'
  AND source   = 'manual'
ORDER BY added_at DESC;
```

### Sweep low-confidence AI tags below the UI threshold

The UI hides AI tags whose `confidence < ai.tag.confidence_threshold`
(default `0.5`, persisted in `system_config`). To garbage-collect them
out of the DB rather than just hiding them:

```sql
DELETE FROM asset_tag
WHERE source     = 'ai'
  AND confidence IS NOT NULL
  AND confidence < (SELECT (value)::numeric
                      FROM system_config
                     WHERE key = 'ai.tag.confidence_threshold');
```

Run this off-peak — it's a destructive sweep, not a UI toggle.

### Cost rollback — which inference call wrote which tag?

The per-call audit row lives in `ai_provider_call` (Phase 1.14.A).
Join via the `created_by_provider` + `created_by_model` + `added_at`
window when you need to attribute spend back to specific tag writes:

```sql
SELECT t.asset_id, t.tag, t.confidence,
       c.id AS call_id, c.cost_usd_micros, c.created_at
FROM asset_tag t
JOIN ai_provider_call c
  ON c.provider = t.created_by_provider
 AND c.model    = t.created_by_model
 AND c.created_at <= t.added_at
WHERE t.source            = 'ai'
  AND t.asset_id          = '<uuid>'
ORDER BY t.added_at DESC, c.created_at DESC
LIMIT 50;
```

(There's no foreign key — the call may be GC'd before the tag — but
the chronological proximity is reliable enough for forensics.)

## Re-running a tag job by hand

The `ai.tag` job handler keys idempotency on `(asset_id, prompt_version)`.
Bumping the prompt version (in `ai.tag.prompt_version`) is the
operator-supported way to force a fresh run across the catalogue —
the existing in-flight jobs settle, then the next fanout uses the new
key and queues fresh work.

A single-asset re-run uses the same enqueue path the upload fanout
takes; the dedicated "regenerate tags for this asset" admin action
lives in the [AI providers](../../web/src/routes/admin/system/ai/+page.svelte)
hub.

## What's NOT in scope yet

- **Read-side exposure of source.** The asset detail JSON still ships
  tags as `string[]` for backwards-compat. The badge component is the
  primitive ready for 1.14.B's typed projection.
- **`replace_all` merge mode.** Only `preserve_manual` is implemented;
  the `ai.tag.merge_semantics` system_config key reserves the
  extension point.
- **Caption / embedding / transcript provenance.** Same per-column
  pattern applies, but the schema for each lands with the providing
  phase (caption schema follow-up; 1.14.B for embeddings; 1.14.C for
  transcripts).
