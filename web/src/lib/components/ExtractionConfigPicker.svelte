<script lang="ts">
  // ExtractionConfigPicker — Phase 1.18.A-2 follow-up B (commit 5).
  //
  // Per-field-definition control for the metadata-extraction
  // pipeline. Operator picks (or clears) the CanonicalField source
  // + the apply mode (skip_if_set / replace / append / prepend).
  // Save triggers PUT /fields/{id}/extraction; on success the
  // dispatched `saved` callback re-loads the parent list.

  import { api } from '$api/client';

  let {
    fieldId,
    initialSource = '',
    initialMode = 'skip_if_set',
    onSaved = () => {},
  }: {
    fieldId: string;
    initialSource?: string;
    initialMode?: string;
    onSaved?: () => void;
  } = $props();

  // Canonical extraction sources the EXIF extractor + applier
  // currently know about. Stable enum — adding a new source means
  // adding a new entry to both ends. Kept inline (no remote enum
  // endpoint) so the picker is one click.
  const SOURCES = [
    { value: '',                 label: '— none (operator-managed) —' },
    { value: 'capture_datetime', label: 'Capture date / time' },
    { value: 'camera_make',      label: 'Camera make' },
    { value: 'camera_model',     label: 'Camera model' },
    { value: 'camera_make_model', label: 'Camera make + model' },
    { value: 'lens_model',       label: 'Lens model' },
    { value: 'gps_latitude',     label: 'GPS latitude' },
    { value: 'gps_longitude',    label: 'GPS longitude' },
    { value: 'gps_coordinates',  label: 'GPS coordinates (combined)' },
    { value: 'exposure_time',    label: 'Exposure time' },
    { value: 'f_number',         label: 'Aperture (f-number)' },
    { value: 'iso',              label: 'ISO speed' },
    { value: 'focal_length',     label: 'Focal length' },
    { value: 'artist',           label: 'Artist (creator)' },
    { value: 'copyright',        label: 'Copyright notice' },
    { value: 'image_description', label: 'Image description' },
    { value: 'orientation',      label: 'EXIF orientation' },
    { value: 'pixel_width',      label: 'Pixel width' },
    { value: 'pixel_height',     label: 'Pixel height' },
    // --- IPTC IIM (Phase 1.18.A-3) — JPEG only today ---
    { value: 'iptc_keywords',     label: 'IPTC keywords (joined)' },
    { value: 'iptc_byline',       label: 'IPTC by-line (photographer)' },
    { value: 'iptc_byline_title', label: 'IPTC by-line title' },
    { value: 'iptc_caption',      label: 'IPTC caption / abstract' },
    { value: 'iptc_headline',     label: 'IPTC headline' },
    { value: 'iptc_credit',       label: 'IPTC credit (agency)' },
    { value: 'iptc_source',       label: 'IPTC source' },
    { value: 'iptc_city',         label: 'IPTC city' },
    { value: 'iptc_state',        label: 'IPTC state / province' },
    { value: 'iptc_country',      label: 'IPTC country' },
    { value: 'iptc_object_name',  label: 'IPTC object name (title)' },
    { value: 'iptc_instructions', label: 'IPTC special instructions' },
    { value: 'iptc_copyright',    label: 'IPTC copyright notice' },
    // --- XMP (Phase 1.18.A-3) — JPEG + PNG ---
    { value: 'xmp_title',                label: 'XMP dc:title' },
    { value: 'xmp_description',          label: 'XMP dc:description' },
    { value: 'xmp_creator',              label: 'XMP dc:creator (joined)' },
    { value: 'xmp_subjects',             label: 'XMP dc:subject (tags)' },
    { value: 'xmp_rights',               label: 'XMP rights / usage terms' },
    { value: 'xmp_hierarchical_tags',    label: 'XMP Lightroom hierarchy' },
    { value: 'xmp_rating',               label: 'XMP rating (0-5)' },
    { value: 'xmp_label',                label: 'XMP color label' },
    { value: 'xmp_photoshop_headline',   label: 'XMP photoshop:Headline' },
    { value: 'xmp_instructions',         label: 'XMP photoshop:Instructions' },
  ];

  const MODES = [
    { value: 'skip_if_set', label: 'skip_if_set — only when target is empty' },
    { value: 'replace',     label: 'replace — overwrite always' },
    { value: 'append',      label: 'append — add to multi-value' },
    { value: 'prepend',     label: 'prepend — front-add to ordered text' },
  ];

  let source = $state(initialSource);
  let mode = $state(initialMode || 'skip_if_set');
  let saving = $state(false);
  let error = $state('');
  let savedMsg = $state('');

  const dirty = $derived(source !== initialSource || mode !== (initialMode || 'skip_if_set'));

  async function save() {
    if (saving) return;
    saving = true;
    error = '';
    savedMsg = '';
    try {
      const r = await api.PUT('/fields/{id}/extraction', {
        params: { path: { id: fieldId } },
        body: { source, mode } as never,
      });
      if (r.error) {
        error = (r.error as { error?: string }).error || 'save failed';
        return;
      }
      savedMsg = source ? `Wired to ${source} (${mode})` : 'Cleared';
      onSaved();
    } finally {
      saving = false;
    }
  }
</script>

<div class="space-y-3 rounded border border-border bg-bg-soft p-3 text-sm">
  <div class="flex flex-wrap items-end gap-3">
    <label class="flex flex-col gap-1">
      <span class="text-xs text-fg-muted">Source</span>
      <select
        bind:value={source}
        class="rounded border border-border bg-bg p-1.5 text-fg"
        data-testid="extraction-source"
      >
        {#each SOURCES as s (s.value)}
          <option value={s.value}>{s.label}</option>
        {/each}
      </select>
    </label>
    <label class="flex flex-col gap-1">
      <span class="text-xs text-fg-muted">Mode</span>
      <select
        bind:value={mode}
        disabled={!source}
        class="rounded border border-border bg-bg p-1.5 text-fg disabled:opacity-50"
        data-testid="extraction-mode"
      >
        {#each MODES as m (m.value)}
          <option value={m.value}>{m.label}</option>
        {/each}
      </select>
    </label>
    <button
      onclick={save}
      disabled={saving || !dirty}
      class="rounded bg-accent px-3 py-1.5 text-xs font-medium text-accent-fg disabled:opacity-50"
      data-testid="extraction-save"
    >{saving ? 'Saving…' : 'Save'}</button>
  </div>
  {#if error}
    <div class="rounded border border-danger/40 bg-danger/10 px-2 py-1 text-xs text-danger">{error}</div>
  {/if}
  {#if savedMsg}
    <div class="rounded border border-success/40 bg-success/10 px-2 py-1 text-xs text-success">{savedMsg}</div>
  {/if}
</div>
