---
id: "0037"
title: Caption & subtitle artifacts — portable, editable, reusable tracks
status: accepted
date: 2026-05-30
area: architecture
phases:
  - "1.44"
supersedes: []
related:
  - "0011"
  - "0017"
  - "0018"
  - "0020"
  - "0024"
  - "0031"
  - "0034"
  - "0036"
tags:
  - architecture
  - captions
  - ai
  - accessibility
  - video
  - audio
excerpt: >-
  Caption and subtitle tracks are first-class artifacts on audio /
  video assets — produced by the Whisper transcription add-on, edited
  by humans through a timeline UI, stored as canonical WebVTT,
  served as portable VTT / SRT / TTML downloads, and auto-attached
  to the video viewer as track sidecars.
---
## Context

Phase 1.42 ships a Whisper add-on that satisfies the `transcription`
capability slot (ADR 0034). Phase 1.18.B-3 ships a video viewer that
honours `<track>` elements for subtitles + lets the user pick a
language. Both of those are pieces. What's missing is the artifact
layer in between:

- **Where do the captions live?** Whisper outputs text + word-level
  timestamps. Today the design implicitly assumes "the viewer
  somehow gets them" — but there's no table, no versioning, no
  per-language model, no download endpoint, no editor.
- **How do humans fix Whisper?** Whisper is good. It is not perfect
  — proper nouns, jargon, mid-sentence punctuation, speaker turns,
  and translation choices all need a person. A "AI ran once, output
  is final" model gives reviewers no path to correct the obvious
  mistakes.
- **How are captions reused?** Studios want subtitle files they can
  bundle in a download, send to YouTube, hand to an NLE, attach to
  a deliverable, or surface on a federated peer. WebVTT is the
  modern web standard; SRT is the universal fallback; broadcast
  workflows want TTML / IMSC. None of these exist as exports today.
- **How are multiple languages handled?** One English source track
  + N translated tracks per asset is the common shape. The data
  model has to support that natively, not as a hack.

ResourceSpace has nothing in this space — captions / subtitles are
not part of its asset model. The closest precedent in the broader
ecosystem is YouTube Studio's captions tab (auto + manual + edit +
download), Frame.io's caption viewer (read-only display only), and
Aegisub (offline editor, no asset-management context). None of those
gives us the full shape: AI producer + human editor + portable
artifact + viewer sidecar + federation / commerce / share-link
distribution.

The relevant context from the rest of Artist Alley:

- **ADR 0011 (Asset entity)** — every caption track attaches to one
  asset; the asset is the unit of identity.
- **ADR 0034 (Capability add-ons)** — the Whisper add-on is the
  producer. The `transcription` slot interface stays narrow ("audio
  → text + timestamps"); this ADR owns "what we do with that output".
- **ADR 0036 (External imports)** — imported video assets may already
  carry sidecar `.srt` / `.vtt` / `.ass` files in the source tree.
  The import framework's mapping rules need to recognise those and
  attach them as caption tracks on the resulting asset, not as
  separate orphan assets.
- **ADR 0018 (Share links)** — a share link to a video should be
  able to include the caption sidecar in the download bundle.
- **ADR 0031 (Commerce)** — a digital download bundle should be
  able to include caption files alongside the video.
- **ADR 0020 (Asset gating)** — caption tracks inherit the parent
  asset's sensitivity. A restricted asset's caption text is just as
  restricted as the bytes.
- **ADR 0024 (Privacy & consent)** — captions can leak speaker
  identity. They follow the asset's visibility rules; no separate
  caption-visibility surface.

This ADR commits Artist Alley to caption tracks as a first-class
artifact that the viewer is one consumer of, not the canonical home.

## Decision

Introduce **caption tracks** as first-class artifacts attached to
audio / video assets. WebVTT is the canonical storage format; SRT,
TTML, and IMSC are produced on-the-fly at download time. A timeline
+ cue-list editor in the asset viewer lets reviewers fix AI output
in place. Tracks are versioned, multi-language, and ship with the
asset through every distribution channel (viewer, download, share
link, commerce bundle, federation peer).

### Data model

```sql
CREATE TABLE caption_track (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    asset_id            UUID NOT NULL REFERENCES assets(id) ON DELETE CASCADE,

    -- BCP-47 language tag: "en", "en-US", "es-419", "ja-JP".
    -- Drives <track srclang>, download filename, viewer label.
    language            TEXT NOT NULL,

    -- HTML5 <track> kind values. Captions include non-speech audio
    -- cues (`[door slams]`); subtitles are dialogue only;
    -- descriptions are audio-described narration for the visually
    -- impaired; chapters are navigable section markers; metadata
    -- is non-displayed cues (for tooling).
    kind                TEXT NOT NULL DEFAULT 'subtitles'
                        CHECK (kind IN ('subtitles', 'captions', 'descriptions', 'chapters', 'metadata')),

    -- Lifecycle status. raw = straight off the AI; draft = a human
    -- is editing; final = signed off, served by the viewer + bundled
    -- in downloads.
    status              TEXT NOT NULL DEFAULT 'raw'
                        CHECK (status IN ('raw', 'draft', 'final', 'archived')),

    -- Who produced this track. Drives the badge in the editor and
    -- the per-track audit trail.
    source              TEXT NOT NULL
                        CHECK (source IN ('ai-whisper', 'ai-translation', 'manual', 'imported', 'derived')),
    source_add_on_id    UUID NULL,    -- if AI, which capability add-on (ADR 0034)
    source_track_id     UUID NULL REFERENCES caption_track(id),  -- if derived/translated, the parent
    import_item_id      UUID NULL REFERENCES import_item(id),    -- if imported, the source row (ADR 0036)

    -- Canonical storage is WebVTT. SRT / TTML / IMSC are produced
    -- on download from this column. Stored inline rather than as a
    -- filestore object because tracks are small (<1 MB even for
    -- long-form), versioned per row, and we want them queryable.
    vtt_text            TEXT NOT NULL,

    -- Default track for its language. The viewer auto-attaches the
    -- default per language; non-default tracks are picker-only.
    is_default          BOOLEAN NOT NULL DEFAULT FALSE,

    -- Versioning. Each save creates a new row pointing at the prior
    -- as predecessor; the editor exposes a track-history view.
    version             INTEGER NOT NULL DEFAULT 1,
    previous_track_id   UUID NULL REFERENCES caption_track(id),

    -- Optional translation provenance.
    translated_from_language TEXT NULL,

    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_user_ref BIGINT NULL,
    updated_by_user_ref BIGINT NULL,

    -- Federation-stable identifier; mirrors `origin_server_id` on
    -- other tables.
    origin_server_id    UUID NULL
);

CREATE UNIQUE INDEX caption_track_asset_lang_default_idx
    ON caption_track (asset_id, language, kind)
    WHERE is_default = TRUE AND status = 'final';

CREATE INDEX caption_track_asset_idx     ON caption_track (asset_id);
CREATE INDEX caption_track_status_idx    ON caption_track (asset_id, status);
CREATE INDEX caption_track_lang_idx      ON caption_track (asset_id, language);
```

The `is_default + status = 'final'` partial unique index guarantees
exactly one "served by the viewer" track per `(asset, language, kind)`.

WebVTT is the only on-disk format. Conversions to SRT / TTML / IMSC
happen in the download path — they're text transforms, fast, and
avoid stale-derivative bookkeeping.

### Producer pipeline (auto path)

```
asset.upload (video/audio) ─┐
                            ▼
                 job: extract_audio_for_caption (if video)
                            ▼
                 job: transcribe (transcription capability slot)
                            │ — dispatches to whichever add-on is bound:
                            │     ai-whisper (local GPU/CPU)
                            │     ai-whisper-cloud (cloud bridge, Pro+)
                            │     future add-ons that satisfy the same slot
                            ▼
                 caption_track row written:
                   status = 'raw'
                   source = 'ai-whisper' (or whatever satisfied the slot)
                   language = detected
                   vtt_text = converted from Whisper segments
                   is_default = TRUE (if no existing final track in lang)
                            ▼
            audit: caption.track.created (correlation_id = job_id)
            metric: captions_tracks_produced_total{source}
```

The transcription slot interface stays exactly as ADR 0034 declares
it ("audio → text + timestamps"). This ADR owns the part *after* the
slot — turning the structured Whisper output into a WebVTT row,
handling chunking for long-form audio (Whisper has practical limits
around 30 minutes per call), and storing the result.

For video, the audio extraction step uses the existing ffmpeg
binding to pull a downmixed mono WAV at Whisper's preferred sample
rate. Audio assets skip the extraction step.

Long-form audio is chunked at silence boundaries (VAD-style) and
merged on the WebVTT side — the chunk boundary becomes a cue
boundary, but the timestamps stay continuous against the original
asset's timeline.

Failure modes: any job-queue failure marks the asset's "auto-caption"
flag as `failed`; a reviewer can re-trigger from the editor. The
add-on being uninstalled produces `unavailable`. Cloud-bridge quota
exhaustion produces `quota_exhausted`. Each is a distinct audit
event.

### Editor

A timeline + cue list lives inside the asset viewer's right-rail in
review mode. Three panes:

1. **Waveform / video scrubber** (top) — the cue under playhead is
   highlighted; click-to-seek; drag cue boundaries on the timeline.
2. **Cue list** (left) — every cue as a row with start / end / text.
   Splits and merges are first-class operations. Speaker labels
   (`>> ALICE:`) are detected and surfaced as a per-cue speaker
   field.
3. **Cue editor** (right) — the focused cue's text, formatting
   (italic / bold for `<i>` / `<b>` tags inside VTT), and timing
   precision (`mm:ss.mmm`).

The editor writes a **draft** track row on first edit (clone-on-write
from the `raw` version, with `previous_track_id` set). Subsequent
saves update the same draft row. "Publish" promotes the draft to
`final` and flips its `is_default` to TRUE, atomically demoting the
previously-default track in the same language.

Keyboard model is intentionally Aegisub-shaped (Enter = next cue,
Shift-Enter = previous, Ctrl-Enter = insert cue, Ctrl-K = split,
Ctrl-M = merge) because that's what existing subtitle editors expect.

The editor is the same Svelte component for audio and video — the
playhead source differs but the cue model is identical.

### Serving

Final tracks are served via a stable URL:

```
GET /assets/{id}/captions/{lang}.{ext}
```

Where `{lang}` is BCP-47 and `{ext}` is one of `vtt`, `srt`, `ttml`,
`imsc.xml`. Format conversion happens on-the-fly; the canonical row
is always WebVTT. Cache headers respect the parent asset's ACL —
private assets' tracks are private too.

The video viewer (Phase 1.18.B-3) auto-attaches every `final` track
as a `<track>` element:

```html
<track kind="subtitles"
       srclang="en"
       label="English"
       src="/assets/{id}/captions/en.vtt"
       default>
<track kind="subtitles"
       srclang="es-419"
       label="Spanish (Latin America)"
       src="/assets/{id}/captions/es-419.vtt">
```

Browser `<track>` handling does the picker, the rendering, the
positioning. We don't reinvent that. Custom track styling (font,
size, position, background opacity) lives in per-user preferences
(Phase 1.18.B-3 already specifies this).

### Reuse hooks

Caption tracks ride along with the parent asset everywhere bytes go:

- **Direct download** of a video asset offers an "Include captions"
  toggle in the bulk-actions modal; checked, the response is a
  ZIP containing the video + one file per final track in the
  reviewer's chosen export format (SRT default, VTT / TTML
  alternates).
- **Share links** (ADR 0018) — when a share-link target is an asset
  with caption tracks, the share-link viewer attaches them as
  `<track>` elements just like the authenticated viewer. The
  download endpoint behind a share link respects the same "include
  captions" toggle, gated on the share-link's `scope=download`.
- **Commerce digital deliveries** (ADR 0031) — purchasing a video
  asset includes the final caption tracks in the delivered bundle.
  Operators can require this at the listing level ("subtitles
  included" badge in the storefront).
- **Federation** (ADR 0007) — peers replicate caption tracks
  alongside the asset; `origin_server_id` mirrors the asset's
  origin. Translation tracks created at a peer push back to origin
  on the same federation channel.
- **External imports** (ADR 0036) — when an import connector
  encounters a sidecar `.srt` / `.vtt` / `.ass` next to a video,
  the importer attaches it as a `caption_track` row
  (`source = 'imported'`, `status = 'final'`) instead of creating
  a separate orphan text asset. The mapping rule that matches the
  sidecar is the standard match-suffix rule.
- **RSS / Atom feeds** (ADR 0023) — feed enclosures for video
  assets include a `<media:subTitle>` entry per final caption
  track. Standard syndication semantics, nothing custom.

### Translation

Translation is a second AI hop, modelled identically to the first.
The capability slot is `text-translation` (new; satisfied by future
add-ons — OpenAI / Anthropic / DeepL / Argos local). Input is a
WebVTT track + target language; output is a WebVTT track with the
same cue timings + translated text.

The translation flow:

```
final track in English ─► [translate to es-419]
                           ▼
              new caption_track row:
                source = 'ai-translation'
                source_track_id = <English track UUID>
                translated_from_language = 'en'
                status = 'raw'  (review before final)
                language = 'es-419'
```

Translation is **always** auto-promoted to `raw` (never `final`)
because cross-language nuance needs a reviewer. The same editor
handles translated tracks; the cue editor optionally shows the
source-language cue alongside for context.

### Manual creation

A reviewer can also create a track from scratch — no AI hop at all.
The editor opens with an empty cue list; the user adds cues against
the playhead. `source = 'manual'`. The path is identical from
`status = 'draft'` onward.

### Burn-in (deferred, v2)

Burning subtitles into the video pixel data ("hard subs") is useful
for platforms that don't honour soft tracks. Two options at v2:

- **Pre-render burn-in variant** — produces a new video file with
  subtitles baked in; stored as an asset variant alongside the
  original. Costly storage; useful for "deliver to YouTube" or
  "ship to a kiosk that has no subtitle support".
- **On-the-fly burn-in on download** — ffmpeg pipeline that
  burns + remuxes during the download response. Lower storage,
  higher CPU per download.

Neither ships at v1. The data model already supports them — they're
just new `kind` values (`hardsub`) or new variant kinds on the
asset, not schema changes. Decision deferred to whenever the demand
arrives.

## Consequences

### Positive

- Captions are real artifacts with a real lifecycle (raw → draft →
  final → archived) instead of an implicit byproduct of preview
  generation. Every distribution channel — viewer, download, share
  link, commerce bundle, federation, RSS — uses the same source.
- Whisper output is treatable as a draft; reviewers have a path to
  fix it without leaving the asset page. The fail mode of "AI gets
  it 90% right and there's no way to fix the 10%" is gone.
- WebVTT-as-canonical + on-the-fly conversion to SRT / TTML / IMSC
  keeps the storage shape simple while giving every downstream
  consumer its preferred format.
- Multi-language is native — one `language` column, one track per
  language, no schema gymnastics. Translation is a clean second hop
  satisfied by the same add-on machinery.
- Imports framework (ADR 0036) gains a clean place for sidecar
  subtitle files instead of having them land as orphan text assets.
- Accessibility story improves at the platform level: every video
  asset can carry captions without per-deployment hand-wiring.

### Negative

- New table + new editor surface area + new serving endpoints +
  new format conversions. Not a small phase.
- The editor is the meaty UI piece; bad caption editors are
  legion and reviewers will compare ours to Aegisub / Subtitle
  Edit / YouTube Studio. We have to be at least adequate at v1.
- WebVTT styling support is uneven across browsers. Custom font /
  size / position preferences may not render identically on Safari
  vs. Chrome vs. Firefox. We document the lowest common denominator
  and accept the rest.
- Translation as a separate AI hop doubles the compute cost per
  translated language. The cloud-bridge price story for Pro+
  operators using AI translation has to be transparent — surface
  per-track translation cost in the editor.
- Whisper for non-English is uneven; some languages return
  garbled or English-mistranscribed output. The status enum
  (`raw → draft → final`) protects users from this — `raw` never
  ships to viewers without human review — but the failure mode
  needs to be surfaced clearly in the editor.

## Alternatives considered

- **Store captions as a sidecar asset.** Treat the `.vtt` file as
  a regular asset with a relationship to the parent video. Reuses
  the asset model wholesale. Rejected because: relationships are
  modelled as plain edges with no semantics about "this is captions
  for that"; bulk operations / sensitivity / federation rules would
  need special-case handling anyway; the editor has nowhere
  natural to live; the relationship-as-captions hack would have to
  be re-discovered everywhere downstream.

- **Store as a column on `assets`.** `assets.caption_vtt` jsonb
  with `{lang: vtt_text}`. Smallest change. Rejected because: no
  versioning, no draft/final lifecycle, no producer attribution,
  no per-language audit, and conceptually wrong — captions are
  their own thing.

- **Defer captions to plugins.** Phase 1.23 (plugin ecosystem) +
  let third parties write a captions plugin. Rejected because: AI
  captions are valuable enough to be first-party; the artifact
  model (multi-language, lifecycle, serving) is platform-level not
  feature-level; deferring it leaves video assets without captions
  for years.

- **Outsource to YouTube Studio / Frame.io style hosting.** Use
  someone else's captioning UI; embed via iframe. Rejected because:
  vendor lock-in directly contradicts the self-hosted commitment
  (ADR 0006); the canonical artifact still has to live somewhere;
  every reuse channel (download, share link, federation, commerce)
  loses the data when the third party goes down.

- **Only auto-captions, no editor.** Whisper output is what it is;
  reviewers edit `.vtt` files outside the system if they care.
  Rejected because: the most common pain point reviewers will hit
  is "Whisper got the artist's name wrong" and there is no
  forgivable answer to "edit the database column directly". The
  editor is the load-bearing UX.

## Implementation

Sub-phases land sequentially; each is its own milestone with an
epic issue.

- **1.44.A — Data model + auto-caption pipeline (raw → served).**
  `caption_track` migration. Audio-extraction job for video assets.
  Transcription slot consumer that writes `caption_track` rows.
  WebVTT serialisation from Whisper word-level output (cue
  splitting at natural pauses + max-chars-per-cue rules).
  Serving endpoint with on-the-fly SRT conversion. Viewer auto-
  attachment of `<track>` elements (consumes Phase 1.18.B-3).
  Audit (`caption.*`) + metrics (`captions_*`) emission. No editor
  yet — `raw` tracks are served as soon as they're produced for
  the v1 demo loop.

- **1.44.B — Editor.** Timeline + cue list + cue editor Svelte
  component. Cue split / merge / re-time / re-text operations.
  Aegisub-shaped keyboard shortcuts. Draft track creation on first
  edit; publish-to-final flow. Per-track version history view.
  Speaker label detection + per-cue speaker field.

- **1.44.C — Translation.** `text-translation` capability slot
  added to the add-on registry. Translation job dispatcher.
  Source-language cue alongside in the cue editor for context.
  Locale picker in the editor. New tracks land as `raw` and route
  through the editor for human review.

- **1.44.D — Reuse hooks + extra formats.** Download bundling
  ("include captions" toggle). Share-link integration. Commerce
  bundle integration. TTML / IMSC export at the serving endpoint.
  Import-connector sidecar attachment (per ADR 0036's mapping
  rules — match `.srt` / `.vtt` / `.ass` next to a video file).

- **1.44.E (deferred / opt-in) — Burn-in variants.** Pre-render
  burn-in path producing an asset variant. Skipped at v1; lands
  when demand arrives.

## References

- [WebVTT specification](https://www.w3.org/TR/webvtt1/) — canonical
  storage format; serving endpoint maps to this exactly.
- [HTML Living Standard, `<track>` element](https://html.spec.whatwg.org/multipage/media.html#the-track-element)
  — viewer integration shape.
- [BCP-47](https://www.rfc-editor.org/info/bcp47) — language tag format.
- [TTML2 / IMSC1](https://www.w3.org/TR/ttml2/) — broadcast export
  format on the serving endpoint.
- [Whisper paper + repository](https://github.com/openai/whisper) —
  producer model; word-level timestamps are the input shape we
  serialise into WebVTT.
- ADR 0011 — Asset entity. Caption tracks are children of assets.
- ADR 0034 — Capability add-ons. The `transcription` and (new)
  `text-translation` slots are satisfied by add-ons; this ADR owns
  what happens to their output.
- ADR 0036 — External imports. Sidecar `.srt` / `.vtt` files in
  import sources attach as caption tracks instead of orphan assets.
- ADR 0018 — Share links. Share-linked videos serve their final
  caption tracks alongside.
- ADR 0031 — Commerce. Digital download bundles include final
  caption tracks.
