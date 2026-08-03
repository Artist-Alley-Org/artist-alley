<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // /admin/email-templates — operator-authored email templates
  // (#795, ADR 0081 §2 as amended).
  //
  // Every email the instance sends has three faces — subject, plain
  // text, and HTML. This page shows what each ships as, lets an operator
  // replace any of them with their own wording, lists the fields that
  // are in scope for that email, and previews the draft against sample
  // values inside a SANDBOXED IFRAME. The preview never uses the app's
  // {@html} path — ADR 0085 keeps rich text the only in-app HTML
  // surface, so a template body an operator is editing is shown through
  // an <iframe sandbox srcdoc> instead.
  //
  // A reference to a field the email does not carry is refused at save
  // with the field named (fail-loud, ADR 0081 §2); the 422 message is
  // shown verbatim.

  import { onMount } from 'svelte';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';

  type Field = { name: string; kind: string; description: string };
  type Collection = { name: string; description: string; fields: Field[] };
  type Part = { part: string; shipped: string; overridden: boolean; body?: string; updated_at?: string };
  type Event = {
    name: string;
    description: string;
    parts: Part[];
    fields: { scalars: Field[]; collections: Collection[] };
  };

  const PART_ORDER = ['subject', 'text', 'html'];

  let events = $state<Event[]>([]);
  let selectedName = $state('');
  /** `${name}/${part}` → the text currently in that part's editor. */
  let drafts = $state<Record<string, string>>({});
  let busy = $state<string | null>(null);
  let loading = $state(true);
  let toast = $state<{ kind: 'ok' | 'err'; text: string } | null>(null);

  const selected = $derived(events.find((e) => e.name === selectedName));

  onMount(() => { void load(); });

  function flash(kind: 'ok' | 'err', text: string) {
    toast = { kind, text };
    setTimeout(() => { toast = null; }, 4000);
  }

  function key(name: string, part: string): string {
    return `${name}/${part}`;
  }

  async function load() {
    loading = true;
    try {
      const r = await api.GET('/email-templates');
      const list = (r.data?.templates ?? []) as Event[];
      events = list;
      // Seed drafts: the override body when present, else the shipped
      // body, so the editor opens on what actually renders today.
      const seeded: Record<string, string> = {};
      for (const ev of list) {
        for (const p of ev.parts) {
          seeded[key(ev.name, p.part)] = p.overridden ? (p.body ?? '') : p.shipped;
        }
      }
      drafts = seeded;
      if (!selectedName && list.length) selectedName = list[0].name;
    } finally {
      loading = false;
    }
  }

  function orderedParts(ev: Event): Part[] {
    return [...ev.parts].sort(
      (a, b) => PART_ORDER.indexOf(a.part) - PART_ORDER.indexOf(b.part),
    );
  }

  function partLabel(part: string): string {
    if (part === 'subject') return t('admin.email_templates.part_subject');
    if (part === 'text') return t('admin.email_templates.part_text');
    return t('admin.email_templates.part_html');
  }

  function draftFor(name: string, part: string): string {
    return drafts[key(name, part)] ?? '';
  }

  function dirty(ev: Event, p: Part): boolean {
    const cur = draftFor(ev.name, p.part);
    const base = p.overridden ? (p.body ?? '') : p.shipped;
    return cur !== base;
  }

  async function save(ev: Event, p: Part) {
    if (busy) return;
    busy = key(ev.name, p.part);
    try {
      const r = await api.PUT('/email-templates/{template}/{part}', {
        params: { path: { template: ev.name, part: p.part } },
        body: { body: draftFor(ev.name, p.part) } as never,
      });
      if (r.error) {
        const status = (r.response as Response | undefined)?.status;
        const msg = (r.error as { error?: string }).error;
        // A 422 names the offending field server-side; show it verbatim
        // — knowing WHICH field was refused is the whole point of the
        // fail-loud rule (ADR 0081 §2).
        if (status === 422) flash('err', msg || t('admin.email_templates.err_unknown_field'));
        else if (status === 403) flash('err', t('admin.email_templates.err_forbidden'));
        else flash('err', msg || t('admin.email_templates.err_generic'));
        return;
      }
      // Reflect the new server truth locally.
      p.overridden = true;
      p.body = draftFor(ev.name, p.part);
      events = [...events];
      flash('ok', t('admin.email_templates.saved'));
    } finally {
      busy = null;
    }
  }

  async function revert(ev: Event, p: Part) {
    if (busy) return;
    busy = key(ev.name, p.part);
    try {
      const r = await api.DELETE('/email-templates/{template}/{part}', {
        params: { path: { template: ev.name, part: p.part } },
      });
      if (r.error) {
        const status = (r.response as Response | undefined)?.status;
        if (status === 403) flash('err', t('admin.email_templates.err_forbidden'));
        else flash('err', t('admin.email_templates.err_generic'));
        return;
      }
      p.overridden = false;
      p.body = undefined;
      drafts = { ...drafts, [key(ev.name, p.part)]: p.shipped };
      events = [...events];
      flash('ok', t('admin.email_templates.reverted'));
    } finally {
      busy = null;
    }
  }

  // --- preview ---------------------------------------------------------
  // Rough client-side render against sample values, safe by construction:
  // the result only ever goes into an <iframe sandbox srcdoc>, never the
  // app DOM. It substitutes scalar + collection fields, expands one
  // iteration of a {{range}}, and strips {{if}}/{{end}} wrappers. It is
  // a preview, not the mail engine — the server renders the real thing.
  const SAMPLES: Record<string, string> = {
    site_name: 'Your Site',
    site_url: 'https://example.org',
    recipient_name: 'Alex',
    triggered_by: 'an operator',
    triggered_at: '2026-08-02T12:00:00Z',
    verb: 'comment.created',
    target_kind: 'asset',
    target_id: 'a1b2c3d4',
    search_name: 'Blue skies',
    results_url: 'https://example.org/search',
    cadence_label: 'daily',
    unsubscribe_url: 'https://example.org/unsubscribe',
    verify_url: 'https://example.org/verify?token=sample',
    expires_in: '24 hours',
    title: 'A sample result',
    summary: 'A short summary of the result.',
    url: 'https://example.org/item',
    headline: 'Alex commented on your asset',
    when: '2 hours ago',
    added_count: '2',
    removed_count: '2',
    count: '2',
  };
  function sampleFor(name: string): string {
    return SAMPLES[name] ?? name;
  }
  function substituteScalars(s: string): string {
    return s.replace(/\{\{\s*\.(\w+)\s*\}\}/g, (_m, f: string) => sampleFor(f));
  }
  function renderPreview(body: string): string {
    // 1. one iteration of each range, substituting the row's fields.
    let out = body.replace(
      /\{\{\s*range\s+\.(\w+)\s*\}\}([\s\S]*?)\{\{\s*end\s*\}\}/g,
      (_m, _coll: string, inner: string) => substituteScalars(inner),
    );
    // 2. keep the if-branch, drop else-branch + the if/else/end tokens.
    out = out.replace(/\{\{\s*else\s*\}\}[\s\S]*?(?=\{\{\s*end\s*\}\})/g, '');
    out = out.replace(/\{\{\s*if[^}]*\}\}/g, '').replace(/\{\{\s*end\s*\}\}/g, '');
    // 3. remaining scalars, then anything left in braces.
    out = substituteScalars(out);
    return out.replace(/\{\{[^}]*\}\}/g, '');
  }
  function textPreviewDoc(body: string): string {
    const esc = renderPreview(body)
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;');
    return `<pre style="white-space:pre-wrap;font-family:ui-monospace,monospace;padding:12px;margin:0">${esc}</pre>`;
  }
</script>

<svelte:head><title>{t('admin.email_templates.title')}</title></svelte:head>

<section class="flex flex-col gap-4 p-4 sm:p-6" data-testid="email-templates-page">
  <header>
    <h1 class="text-2xl font-semibold text-fg">{t('admin.email_templates.title')}</h1>
    <p class="mt-1 max-w-4xl text-sm text-fg-muted">{t('admin.email_templates.intro')}</p>
    <p class="mt-1 max-w-4xl text-xs text-fg-muted">{t('admin.email_templates.syntax_note')}</p>
  </header>

  {#if toast}
    <p role="status" data-testid="email-templates-toast"
       class={toast.kind === 'ok'
         ? 'rounded border border-success/40 bg-success/10 px-3 py-2 text-sm text-success'
         : 'rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger'}>{toast.text}</p>
  {/if}

  {#if loading}
    <p class="text-fg-muted">{t('admin.email_templates.loading')}</p>
  {:else}
    <label class="block max-w-md text-xs">
      <span class="mb-1 block text-fg-muted">{t('admin.email_templates.select_event')}</span>
      <select bind:value={selectedName}
              class="min-h-11 w-full rounded border border-border-strong bg-surface px-2 py-1 text-sm"
              data-testid="email-templates-select">
        {#each events as ev (ev.name)}
          <option value={ev.name}>{ev.name}</option>
        {/each}
      </select>
    </label>

    {#if selected}
      <p class="text-sm text-fg-muted" data-testid="email-templates-description">{selected.description}</p>

      <!-- Field list — what the operator may reference. -->
      <div class="rounded-lg border border-border bg-surface-elevated p-3" data-testid="email-templates-fields">
        <h2 class="text-sm font-semibold text-fg">{t('admin.email_templates.fields_available')}</h2>
        <ul class="mt-2 flex flex-wrap gap-x-4 gap-y-1">
          {#each selected.fields.scalars as f (f.name)}
            <li class="text-xs text-fg-muted">
              <code class="text-accent">&#123;&#123;.{f.name}&#125;&#125;</code>
              <span class="ml-1">{f.description}</span>
            </li>
          {/each}
        </ul>
        {#each selected.fields.collections as c (c.name)}
          <div class="mt-2">
            <p class="text-xs text-fg">
              <code class="text-accent">&#123;&#123;range .{c.name}&#125;&#125;</code>
              <span class="ml-1 text-fg-muted">{c.description}</span>
            </p>
            <ul class="ml-4 mt-1 flex flex-wrap gap-x-4 gap-y-1">
              {#each c.fields as f (f.name)}
                <li class="text-xs text-fg-muted">
                  <code class="text-accent">&#123;&#123;.{f.name}&#125;&#125;</code>
                  <span class="ml-1">{f.description}</span>
                </li>
              {/each}
            </ul>
          </div>
        {/each}
        <p class="mt-2 text-xs text-fg-muted">{t('admin.email_templates.fields_loop_note')}</p>
      </div>

      <!-- One editor block per part. -->
      {#each orderedParts(selected) as p (p.part)}
        <div class="rounded-lg border border-border bg-surface-elevated p-3" data-testid="email-templates-part-{p.part}">
          <div class="flex flex-wrap items-center gap-2">
            <h3 class="text-sm font-semibold text-fg">{partLabel(p.part)}</h3>
            {#if p.overridden}
              <span class="rounded bg-accent/15 px-1.5 py-0.5 text-[0.65rem] uppercase tracking-wide text-accent"
                    data-testid="email-templates-changed-{p.part}">{t('admin.email_templates.changed_badge')}</span>
            {/if}
          </div>

          <div class="mt-2 grid gap-3 lg:grid-cols-2">
            <div>
              <span class="mb-1 block text-xs uppercase tracking-wide text-fg-muted">{t('admin.email_templates.shipped')}</span>
              <pre class="max-h-56 overflow-auto rounded border border-border bg-surface p-2 text-xs text-fg-muted"
                   data-testid="email-templates-shipped-{p.part}">{p.shipped}</pre>
            </div>
            <div>
              <label class="mb-1 block text-xs uppercase tracking-wide text-fg-muted" for="ed-{p.part}">
                {t('admin.email_templates.your_version')}
              </label>
              <textarea id="ed-{p.part}"
                        value={draftFor(selected.name, p.part)}
                        oninput={(e) => { drafts = { ...drafts, [key(selected!.name, p.part)]: (e.currentTarget as HTMLTextAreaElement).value }; }}
                        rows={p.part === 'html' ? 10 : 4}
                        class="w-full rounded border border-border-strong bg-surface p-2 font-mono text-xs"
                        data-testid="email-templates-input-{p.part}"></textarea>
            </div>
          </div>

          <div class="mt-2 flex flex-wrap items-center gap-2">
            <button type="button" onclick={() => void save(selected!, p)}
                    disabled={busy === key(selected.name, p.part) || !dirty(selected, p)}
                    class="min-h-11 rounded border border-accent bg-accent/10 px-3 py-1 text-sm font-medium text-accent hover:bg-accent/20 disabled:opacity-40"
                    data-testid="email-templates-save-{p.part}">
              {busy === key(selected.name, p.part) ? t('admin.email_templates.saving') : t('admin.email_templates.save')}
            </button>
            {#if p.overridden}
              <button type="button" onclick={() => void revert(selected!, p)}
                      disabled={busy === key(selected.name, p.part)}
                      class="min-h-11 rounded border border-border-strong px-3 py-1 text-sm text-fg-muted hover:border-danger hover:text-danger disabled:opacity-40"
                      data-testid="email-templates-revert-{p.part}">
                {busy === key(selected.name, p.part) ? t('admin.email_templates.reverting') : t('admin.email_templates.revert')}
              </button>
            {/if}
          </div>

          <!-- Preview: sandboxed iframe, never {@html}. -->
          <div class="mt-3">
            <span class="mb-1 block text-xs uppercase tracking-wide text-fg-muted">{t('admin.email_templates.preview')}</span>
            <iframe title={partLabel(p.part) + ' preview'}
                    sandbox=""
                    srcdoc={p.part === 'html'
                      ? renderPreview(draftFor(selected.name, p.part))
                      : textPreviewDoc(draftFor(selected.name, p.part))}
                    class="h-56 w-full rounded border border-border bg-white"
                    data-testid="email-templates-preview-{p.part}"></iframe>
            <p class="mt-1 text-xs text-fg-muted">{t('admin.email_templates.preview_note')}</p>
          </div>
        </div>
      {/each}
    {/if}
  {/if}
</section>
