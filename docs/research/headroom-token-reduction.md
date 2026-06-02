# Headroom — token-reduction proxy evaluation

**Status:** investigation, not a decision. Revisit at Phase 1.46 (cloud-bridge add-ons) implementation time.
**Date:** 2026-06-02
**Owner:** TBD

Tejas Chopra's [Headroom](https://github.com/chopratejas/headroom) is an OSS proxy that sits between an LLM client and the upstream API, claiming to compress prompts and cache redundant blobs to reduce token spend. This note evaluates it against Artist Alley's specific AI integration points so we don't have to re-do the research when Phase 1.46 lands.

> The Register's "Netflix engineer slashes AI bills" framing overplays it — this is Chopra's personal project, not a Netflix release. No Wiz involvement. 2k★, 120 forks, v0.22 as of January 2026.

## What Headroom claims to do

Three components on a local proxy at `:8787`:

1. **CacheAligner.** Detects unchanged regions across repeated inputs so the upstream provider's prompt cache doesn't get invalidated by trivial drift (whitespace shifts, timestamps moving, etc.).
2. **Content-specific compressors.**
   - AST-based for code (Python AST round-trip + dictionary substitution).
   - JSON/DOM structural compression for API responses / scraped HTML.
   - Statistical "squashers" for prose (NLP n-gram replacement).
3. **Compress-Cache-Retrieve (CCR).** Replaces large blobs with opaque markers (`<<HEADROOM:abc123>>`); originals live in Redis or SQLite local to the proxy. The LLM is taught (via system prompt + a tool definition) to call a retrieval tool when it needs the underlying content.

Claimed numbers (from the README and the original blog post):

- 90% of input tokens redundant across calls
- 90% compression on server logs
- 70% compression on MCP tool outputs
- $700k saved across all users, 200B tokens "freed"

These numbers are user-self-reported; no independent benchmark exists at the time of this note.

## Where Artist Alley would actually use LLMs

LLM-touching surfaces, sorted by likely token volume:

| Surface | Phase | LLM role | Token shape |
|---|---|---|---|
| Auto-tagging at upload | 1.14 | Vision model → caption + tag list per asset | Few-shot prompt + image, output ~50-200 tokens. Bulk over many assets. |
| Translation (caption tracks) | 1.44.C / 1.46 | Text-to-text with cue-timing preserved | Repeated system prompt + per-cue text. Long-form bulk. |
| Transcription | 1.42 / 1.46 | Audio → text + timestamps (Whisper) | Audio in, no LLM prompt overhead. **Not in Headroom's scope.** |
| Image embedding | 1.14 / 1.42 | CLIP / future models | Image → vector. **Not in Headroom's scope.** |
| Creative editing — inpaint / variation | 1.34 | Diffusion model | Image + prompt → image. **Not in Headroom's scope.** |
| Operator-side LLM agent (future) | speculative | Tool-use over AA's own APIs | High variance; could be huge if it lands. |

**Headroom only addresses LLM text prompts** — prose-to-prose calls. The transcription, embedding, and image-generation paths bypass it entirely because they're not text-token-billed in the same way.

So the realistic target surface for Headroom is:

- **Auto-tagging** (1.14) — if it uses a multimodal LLM rather than CLIP-only
- **Translation** (1.44.C / 1.46) — high-volume, prompt-heavy
- **Future operator agent** — speculative but potentially huge

That's a narrow band, not the project-wide AI bill saver the marketing implies.

## Where Headroom would help us

### 1. Repeated system prompts in bulk jobs

The auto-tagging and translation pipelines both ship the same system prompt + format instructions per call. Headroom's CacheAligner would normalize trivial drift so the upstream cache stays warm. Plausibly a 30–50% reduction in input tokens on cold runs; smaller on warm runs because the upstream cache already handles much of it.

### 2. JSON tool-output compression

If we build an operator-side LLM agent that consumes AA's API responses (which are JSON), the JSON structural compressor could help. Asset metadata, search results, audit-event payloads — all moderately repetitive structure with varying string content.

### 3. Self-hosted operator workloads

For operators running their own LLM via local add-on (e.g. a future `ai-llm-local` slot), Headroom-style compression has different economics — it trades local compute for local compute, no upstream bill. Could still help by fitting more useful context into a smaller window.

## Where Headroom wouldn't help (or might regress)

### 1. Anthropic / OpenAI prompt caching already does most of this

Anthropic ships [prompt caching](https://docs.claude.com/en/docs/build-with-claude/prompt-caching) with 5-minute and 1-hour TTLs; cached input tokens are billed at **10% of the normal rate**. OpenAI has equivalent. **For the system-prompt-redundancy case Headroom optimises, the provider-side cache is already taking that money off the bill before Headroom sees it.** Stacking Headroom on top yields diminishing returns — possibly ~10–20% additional savings, possibly zero if your prompts are already cache-friendly.

The honest framing: Headroom is most valuable when your prompts are **not** cache-friendly (timestamps in the system prompt, dynamic per-call instructions, user-specific context shuffled around). The fix for that is usually "structure your prompts for the upstream cache," not "add a proxy."

### 2. Marker / retrieve adds a tool-use roundtrip

The CCR pattern (replace blob with `<<HEADROOM:abc>>`, model calls a retrieval tool when it needs the blob) is a tool-use roundtrip every time the model actually needs the content. That:

- **Adds latency** — extra request/response per retrieval.
- **Can regress accuracy** on tasks where the model needs the full blob in one pass (the model decides whether to retrieve; if it guesses wrong, you get worse output).
- **Doesn't compose with streaming** — if you need first-token latency to be low, the retrieve roundtrip kills it.

For auto-tagging and translation (where we want a single in-one-pass call), this pattern is the wrong shape. It's better suited to long-running agent loops where the model can plan retrievals.

### 3. We're not a high-volume LLM consumer (yet)

Our token volume in v1 is small. The economics of running a proxy (operational cost: Redis instance, Python/Node service, latency overhead, debugging surface) only pay back at high volume. For an operator running a few hundred asset uploads a day, the engineering cost dominates.

### 4. Compression for non-text modalities is out of scope

Half of our AI surface (images, audio, embeddings) doesn't route through a text-token-billed API in the way Headroom assumes.

## Overlap with our existing architecture

Phase 1.46 (cloud-bridge add-ons) plans **per-token / per-image metering** with operator-side budget caps in `/admin/billing`. The metering is the lever that surfaces token economics to the operator. Two ways Headroom could compose with 1.46:

### Option A — Operator-side opt-in proxy
Operators concerned about their cloud-bridge bill can run Headroom in front of their own AA instance. The cloud-bridge add-on talks to `localhost:8787`; Headroom forwards to our hosted endpoint. We do nothing.

**Pros:** zero engineering on our side; operators self-serve.
**Cons:** awkward setup for the operator; we lose visibility into what the proxy is doing, which complicates support.

### Option B — Bake Headroom into the cloud-bridge add-on container
Ship `aa-cloud-ai` / `aa-cloud-translation` / `aa-cloud-transcription` with Headroom built in, transparent to the operator, exposed as a per-add-on toggle ("enable token compression").

**Pros:** turn-key for operators; we control the integration; usage data feeds back into our metering.
**Cons:** real engineering, real ops surface, our brand is on the savings claim. Need to benchmark honestly before committing.

### Option C — Skip Headroom; do the parts ourselves
The genuinely valuable subset (CacheAligner-style prompt normalisation, JSON output compression for our specific shape) is small. Implementing those ourselves in the cloud-bridge add-on avoids the dependency, avoids the marker/retrieve latency tax, and lets us tune for our specific prompt shapes.

**Pros:** no third-party dependency; tuned for our case; we keep token reduction as a tangible operator-facing knob without the speculative parts.
**Cons:** more in-house engineering; we don't get the AST/code-compression "free" bits (which we don't need anyway).

**Provisional recommendation:** Option C, with the explicit caveat that we re-evaluate Headroom at Phase 1.46 implementation time after running the experiments below.

## Specific experiments to run at 1.46 implementation time

Before deciding whether to integrate Headroom (Options A/B) or roll our own (Option C):

1. **Baseline tokens-per-asset for auto-tagging.** Run 100 representative assets through our chosen vision LLM with the planned prompt. Measure input + output token counts; compute the per-asset cost at provider list price.
2. **Baseline with Anthropic prompt caching.** Same workload with explicit cache control on the system prompt. Compute savings. This is the floor Headroom has to beat.
3. **Translation bulk job baseline.** 1 hour of caption tracks (~600 cues) translated to one target language with provider cache. Same measurement.
4. **Headroom on top.** Plug Headroom into the same workload, measure delta over baseline-with-cache.
5. **Latency regression.** Measure p50/p95 first-token-latency with vs. without Headroom. Marker/retrieve roundtrips show up here.
6. **Accuracy regression.** Sample-judge 50 outputs with vs. without Headroom for both auto-tagging and translation. Headroom's compressors are lossy; some tasks tolerate it, some don't.

Pass criteria for adopting Headroom (Option B): **>20% additional cost savings over baseline-with-provider-cache, <50ms additional p95 latency, no statistically detectable accuracy regression.** Lower than 20% extra savings doesn't justify the operational complexity.

## Open questions to verify before adopting

- **License compatibility.** Headroom's license needs to be checked against Artist Alley's plan to ship cloud-bridge add-ons as proprietary artifacts (ADR 0038). If it's GPL'd, subprocess-only invocation (the Blender pattern, per ADR 0040) is the safe-harbour. If it's AGPL'd, even subprocess gets fraught for a hosted service. If permissive (MIT / Apache / BSD), we can vendor.
- **Maturity.** v0.22 with 2k stars and 120 forks suggests young-but-active. Production reliability story is unclear; CI/test coverage on the repo needs review.
- **Provider compatibility.** Headroom claims wraps "any LLM CLI." Verify it works against the specific Anthropic + OpenAI SDKs we'll use, not just `curl` against the bare API.
- **Cache invalidation under concurrent writes.** The Redis/SQLite blob store has to handle concurrent operator workloads. What happens when two parallel auto-tagging jobs reference the same marker? Not obvious from the README.
- **Privacy posture.** The proxy intercepts every prompt — at a minimum, that's where credentials route. The operator has to trust the binary. For our cloud-bridge add-on threat model (ADR 0038 § Premium add-ons), this composes — but for self-hosted operators running their own LLM, Headroom's proxy is one more attack surface to keep current.
- **Streaming.** Most LLM calls are streamed in 2026. Does Headroom's marker/retrieve pattern compose with streamed responses, or does it force buffering?

## See also

- ADR 0026 — AI creative editing (`image-inpaint` + `image-generation` slots; bypasses Headroom's scope).
- ADR 0034 — Capability add-ons (where the AI providers plug in).
- ADR 0037 — Caption & subtitle artifacts (translation slot is the highest-value Headroom target).
- ADR 0038 — Premium add-on layer (cloud-bridge pricing model; metered billing is the lever Headroom would compose with).
- ADR 0040 — Clean-room reverse-engineering methodology (governs how we'd vendor or integrate Headroom code if we go that direction).
- Phase 1.46 — Cloud-bridge add-ons — the natural decision point.
- [Headroom repository](https://github.com/chopratejas/headroom)
- [Anthropic prompt caching documentation](https://docs.claude.com/en/docs/build-with-claude/prompt-caching) — the prior art Headroom partially overlaps with.

## What to do now

**Nothing implementation-side.** This is a parking-lot research note. Revisit when:

- Phase 1.46 (cloud-bridge add-ons) reaches implementation, OR
- An operator hits a real cloud-bridge cost ceiling and Headroom is the proposed mitigation, OR
- The Headroom project ships a 1.0 with production case studies.

Until then: track the project for major version bumps + production deployments; keep this note current with any provider-side prompt-cache changes that move the baseline.
