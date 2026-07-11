# AI inference subsystem — operator setup

Phase 1.14.A introduces the AI inference subsystem: a provider
abstraction with 5 implementations (OpenAI, Claude, Gemini,
Ollama, vLLM), a router with privacy + budget gating, and the
plumbing for ai.tag + ai.caption job handlers. This document
covers what an operator needs to know to enable AI features on a
fresh install.

## Fresh-install state

On a clean install the AI subsystem is **off by default**:

- `ai.enabled` is `false` — no inference call will fire even if
  routing + providers are configured.
- Every cloud provider's per-month hard cap is `$0` — the first
  attempted call returns `cloud_budget_not_configured` until you
  explicitly raise the cap.
- `ai.privacy.lock_sensitive_to_local` is `true` — assets at
  sensitivity tier `restricted` or `embargo` route only to
  providers in the local list (default
  `[ollama, vllm, whisper_local, clip_local]`). Cloud providers
  never see their bytes unless you flip this off (with a confirm
  dialog).

These defaults are deliberately conservative. The first three
things you'll do as an operator:

1. Configure at least one provider with an API key (or local
   endpoint URL).
2. Raise the default budget cap to something non-zero so cloud
   calls can fire.
3. Flip `ai.enabled` to `true`.

## Per-task routing

The router picks a provider per task. Defaults (from migration
00009 seed):

| Task        | Default provider | Notes                                         |
|-------------|------------------|-----------------------------------------------|
| `complete`  | `claude`         | Chat-style completion + reasoning.            |
| `tag`       | `ollama`         | Image → tag list. Private + free by default.  |
| `caption`   | `claude`         | Image → free-text caption.                    |
| `embed`     | `clip_local`     | Text/image → vector. Deferred to 1.14.B.      |
| `transcribe`| `whisper_local`  | Audio → text. Deferred to 1.14.C.             |

Edit at `/admin/ai/config` → **Per-task routing**. Each value is
a provider name. The router falls through the fallback chain on
transient + rate-limit errors; permanent / budget / privacy
errors short-circuit (no fallback would succeed).

## Fallback chains

Per task, ordered list of provider names walked when the primary
fails. Defaults again from migration 00009:

```
complete:   claude, openai, ollama
embed:      clip_local, ollama, openai
transcribe: whisper_local, openai
tag:        ollama, gemini, openai
caption:    claude, openai, ollama
```

Edit at `/admin/ai/config` → **Fallback chains**, comma-separated.

## Privacy

Two settings under `/admin/ai/config` → **Privacy**:

- **Lock sensitive assets to local providers** — when on
  (default), restricted + embargo assets clamp to the local
  provider list. Off means cloud providers can see their bytes.
- **Local provider names** — which names are considered local.
  Defaults: `ollama, vllm, whisper_local, clip_local`.

When the gate empties the candidate set (lock on + no local
provider eligible for the concern), the call fails with
`ErrClassPrivacy` and shows up in the usage dashboard's
per-status breakdown as `privacy_blocked`.

## Budgets

Per-provider monthly budgets enforce a hard ceiling on cloud
spend. The structure is:

- **Soft warning (USD)** — crossing this threshold within a
  billing period fires an audit event. Doesn't block calls.
- **Hard cap (USD)** — calls that would push past this fail
  with `ai_budget_exhausted`. The router skips the provider for
  the rest of the period.

The fresh-install default is **$0 hard cap** for every provider.
First cloud call fails with `cloud_budget_not_configured` until
you raise the cap explicitly. This is the fail-closed default;
edit at `/admin/ai/config` → **Default budget**.

Per-provider overrides land in a follow-up phase; for 1.14.A the
default applies to every provider.

## Cost dashboard

`/admin/ai/usage` shows per-provider call count + spend + status
breakdown for one billing period (defaults to the current UTC
month). Source of truth is the `ai_provider_call` table; rollup
runs on demand. Use the period picker to inspect past months.

## What's NOT in 1.14.A

- **Per-provider config UI** — the existing `/admin/system/ai`
  page (Phase 1.16) still holds the raw provider list (API keys,
  base URLs, models). The new typed inference config at
  `/admin/ai/config` drives the router; the two will reconcile
  in a follow-up slice once the operator-facing design is
  settled.
- **Auto-fanout on asset upload** — the ai.tag + ai.caption job
  handlers are implemented but not yet wired to the assets
  package post-upload pipeline. The `AssetLookup` adapter that
  bridges them lands in a follow-up.
- **CLIP embeddings + similarity search** — Phase 1.14.B.
- **Whisper transcription** — Phase 1.14.C.
- **Image editing via ComfyUI** — Phase 1.34 (ADR 0026).

## Federation

AI calls are local-instance only. No AI activity federates
cross-instance — the AI-generated tags / captions ride along
with their parent asset as ordinary metadata. The receiving
instance MAY run its own AI on top (provenance is preserved via
the audit row's `provider` + `model` fields).
