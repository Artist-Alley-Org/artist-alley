<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // ColorPicker — popover with a hue slider + S/V field + RGB/Hex
  // inputs + EyeDropper API button + recent-colors strip.
  //
  // Architecture:
  //   - The component is purely visual / interactive — it takes a
  //     `value` (current color hex) + `oninput` callback. Position-
  //     ing is the host's job (we render with absolute positioning
  //     anchored to a parent the host wraps us in).
  //   - HSV is the canonical interaction space (matches Photoshop /
  //     Figma): hue along the vertical slider, saturation × value
  //     in the 2D field. We convert HSV → RGB → hex on every
  //     change, emit hex, and also accept hex/RGB input boxes.
  //   - The EyeDropper API (Chromium ≥ 95) lets the user pick a
  //     pixel from anywhere on screen. Soft-fails to a tooltip on
  //     Firefox / Safari with a hint about Chrome/Edge support.
  //
  // Recent colors are stored in localStorage so they survive page
  // reloads — saves users from re-picking the same custom color
  // every session.

  import { onMount } from 'svelte';

  interface Props {
    value: string; // hex like '#ff6b00'
    oninput: (hex: string) => void;
    onclose?: () => void;
  }

  let { value, oninput, onclose }: Props = $props();

  // ── HSV state — initialised synchronously from `value` so the
  // picker opens at the user's actual current color (not a default).
  // Previously this lived in onMount and the $effect emitted "red"
  // before the mount ran, clobbering the prop.
  function hsvFromValue(hex: string) {
    const rgb = hexToRgb(hex) ?? { r: 255, g: 0, b: 0 };
    return rgbToHsv(rgb.r, rgb.g, rgb.b);
  }

  const _init = hsvFromValue(value);
  let h = $state(_init.h);
  let s = $state(_init.s);
  let v = $state(_init.v);
  // RGB inputs are derived from HSV but the user can type into them
  // to drive HSV the other way. Same with the hex box.
  let hexInput = $state(value);
  let suppressNextHex = false; // avoid re-parsing our own write-back

  // Recent colors (max 12 entries, newest first).
  const RECENT_KEY = 'whiteboard.colorPicker.recent';
  let recent = $state<string[]>([]);

  // ── Conversions ─────────────────────────────────────────────────

  function hexToRgb(hex: string): { r: number; g: number; b: number } | null {
    const m = /^#?([0-9a-f]{6})$/i.exec(hex.trim());
    if (!m) {
      const m3 = /^#?([0-9a-f]{3})$/i.exec(hex.trim());
      if (!m3) return null;
      const expanded = m3[1].split('').map((c) => c + c).join('');
      return hexToRgb('#' + expanded);
    }
    const n = parseInt(m[1], 16);
    return { r: (n >> 16) & 0xff, g: (n >> 8) & 0xff, b: n & 0xff };
  }

  function rgbToHex(r: number, g: number, b: number): string {
    const p = (n: number) => Math.max(0, Math.min(255, Math.round(n))).toString(16).padStart(2, '0');
    return `#${p(r)}${p(g)}${p(b)}`;
  }

  function rgbToHsv(r: number, g: number, b: number): { h: number; s: number; v: number } {
    const rr = r / 255, gg = g / 255, bb = b / 255;
    const max = Math.max(rr, gg, bb);
    const min = Math.min(rr, gg, bb);
    const d = max - min;
    let hue = 0;
    if (d !== 0) {
      if (max === rr) hue = ((gg - bb) / d + (gg < bb ? 6 : 0)) * 60;
      else if (max === gg) hue = ((bb - rr) / d + 2) * 60;
      else hue = ((rr - gg) / d + 4) * 60;
    }
    return { h: hue, s: max === 0 ? 0 : d / max, v: max };
  }

  function hsvToRgb(hue: number, sat: number, val: number): { r: number; g: number; b: number } {
    const c = val * sat;
    const x = c * (1 - Math.abs(((hue / 60) % 2) - 1));
    const m = val - c;
    let r1 = 0, g1 = 0, b1 = 0;
    if (hue < 60) { r1 = c; g1 = x; }
    else if (hue < 120) { r1 = x; g1 = c; }
    else if (hue < 180) { g1 = c; b1 = x; }
    else if (hue < 240) { g1 = x; b1 = c; }
    else if (hue < 300) { r1 = x; b1 = c; }
    else { r1 = c; b1 = x; }
    return { r: (r1 + m) * 255, g: (g1 + m) * 255, b: (b1 + m) * 255 };
  }

  const currentRgb = $derived(hsvToRgb(h, s, v));
  const currentHex = $derived(rgbToHex(currentRgb.r, currentRgb.g, currentRgb.b));

  // Push current color back to the host on every change. Also
  // refresh the hex input so the user sees it follow their drags.
  $effect(() => {
    const hex = currentHex;
    if (!suppressNextHex) hexInput = hex;
    suppressNextHex = false;
    oninput(hex);
  });

  // ── Recent colors hydrate on mount ─────────────────────────────

  onMount(() => {
    try {
      const raw = localStorage.getItem(RECENT_KEY);
      if (raw) {
        const arr = JSON.parse(raw);
        if (Array.isArray(arr)) recent = arr.filter((x) => typeof x === 'string').slice(0, 12);
      }
    } catch { /* ignore */ }
  });

  // ── Hex / RGB input handlers ───────────────────────────────────

  function onHexChange(next: string) {
    hexInput = next;
    const rgb = hexToRgb(next);
    if (!rgb) return;
    const hsv = rgbToHsv(rgb.r, rgb.g, rgb.b);
    suppressNextHex = true;
    h = hsv.h; s = hsv.s; v = hsv.v;
  }
  function onRgbChange(field: 'r' | 'g' | 'b', val: number) {
    const r = field === 'r' ? val : currentRgb.r;
    const g = field === 'g' ? val : currentRgb.g;
    const b = field === 'b' ? val : currentRgb.b;
    const hsv = rgbToHsv(r, g, b);
    suppressNextHex = true;
    h = hsv.h; s = hsv.s; v = hsv.v;
  }

  // ── S/V field pointer drag ─────────────────────────────────────

  let svFieldEl: HTMLDivElement | undefined = $state();
  function onSvPointer(e: PointerEvent) {
    if (!svFieldEl) return;
    if (e.type === 'pointerdown') svFieldEl.setPointerCapture(e.pointerId);
    if (e.buttons === 0 && e.type !== 'pointerdown') return;
    const rect = svFieldEl.getBoundingClientRect();
    const nx = Math.max(0, Math.min(1, (e.clientX - rect.left) / rect.width));
    const ny = Math.max(0, Math.min(1, (e.clientY - rect.top) / rect.height));
    s = nx;
    v = 1 - ny;
  }

  // ── EyeDropper API ─────────────────────────────────────────────

  // Type augmentation — EyeDropper is only in Chromium types and
  // not yet in stock lib.dom. Declare a local stub so TS compiles.
  interface EyeDropperResult { sRGBHex: string; }
  interface EyeDropperCtor {
    new (): { open: () => Promise<EyeDropperResult>; };
  }
  const hasEyeDropper = $derived(typeof window !== 'undefined' && 'EyeDropper' in window);
  async function pickWithEyeDropper() {
    if (!hasEyeDropper) return;
    try {
      const ED = (window as unknown as { EyeDropper: EyeDropperCtor }).EyeDropper;
      const dropper = new ED();
      const result = await dropper.open();
      onHexChange(result.sRGBHex);
      commitToRecent(result.sRGBHex);
    } catch {
      // User cancelled — silent.
    }
  }

  // ── Recent colors ──────────────────────────────────────────────

  function commitToRecent(hex: string) {
    const next = [hex.toLowerCase(), ...recent.filter((c) => c.toLowerCase() !== hex.toLowerCase())].slice(0, 12);
    recent = next;
    try { localStorage.setItem(RECENT_KEY, JSON.stringify(next)); } catch { /* ignore */ }
  }
  function pickRecent(hex: string) { onHexChange(hex); }

  // Close on Escape, commit-and-close on Enter inside the hex box.
  function onKey(e: KeyboardEvent) {
    if (e.key === 'Escape') { onclose?.(); e.preventDefault(); }
  }
</script>

<svelte:window onkeydown={onKey} />

<div
  class="w-[22rem] rounded-md border border-border bg-surface-elevated p-3 text-xs text-fg shadow-xl"
  role="dialog"
  aria-label="Color picker"
>
  <!-- Two-column layout: left = S/V field + hue slider (square-
       ish color space); right = stacked HEX + R/G/B inputs. Keeps
       the dropdown compact horizontally while the inputs stay
       readable, and avoids the previous-layout overflow when the
       sidebar is narrow. -->
  <div class="grid grid-cols-[1fr_5rem] gap-3">
    <!-- LEFT column: S/V field + hue slider -->
    <div>
      <!-- svelte-ignore a11y_click_events_have_key_events a11y_no_static_element_interactions -->
      <div
        bind:this={svFieldEl}
        class="relative h-40 w-full cursor-crosshair touch-none rounded"
        style:background={`linear-gradient(to top, #000, transparent), linear-gradient(to right, #fff, hsl(${h}, 100%, 50%))`}
        onpointerdown={onSvPointer}
        onpointermove={onSvPointer}
      >
        <div
          class="pointer-events-none absolute h-3 w-3 -translate-x-1/2 -translate-y-1/2 rounded-full border-2 border-white shadow-[0_0_0_1px_rgba(0,0,0,0.5)]"
          style:left={`${s * 100}%`}
          style:top={`${(1 - v) * 100}%`}
        ></div>
      </div>
      <input
        type="range"
        min={0}
        max={360}
        step={1}
        value={h}
        oninput={(e) => (h = +(e.currentTarget as HTMLInputElement).value)}
        class="mt-2 w-full"
        style:background="linear-gradient(to right, #f00 0%, #ff0 17%, #0f0 33%, #0ff 50%, #00f 67%, #f0f 83%, #f00 100%)"
        style:appearance="none"
        style:height="12px"
        style:border-radius="6px"
        aria-label="Hue"
      />
    </div>

    <!-- RIGHT column: HEX + RGB inputs stacked. Each input is
         full-column width so digits never clip. -->
    <div class="flex flex-col gap-2">
      <!-- Live color preview swatch — bigger feedback than the
           tiny indicator dot inside the S/V field. -->
      <div
        class="h-10 w-full rounded border border-border"
        style:background-color={currentHex}
        title={currentHex}
        aria-label="Current color preview"
      ></div>
      <label class="block">
        <span class="block text-[10px] text-fg-muted">HEX</span>
        <input
          type="text"
          value={hexInput}
          oninput={(e) => onHexChange((e.currentTarget as HTMLInputElement).value)}
          class="w-full rounded border border-border bg-surface px-1 py-0.5 font-mono text-xs"
          maxlength={7}
        />
      </label>
      {#each ['r','g','b'] as ch (ch)}
        {@const cur = ch === 'r' ? currentRgb.r : ch === 'g' ? currentRgb.g : currentRgb.b}
        <label class="block">
          <span class="block text-[10px] text-fg-muted uppercase">{ch}</span>
          <input
            type="number"
            min={0} max={255} step={1}
            value={Math.round(cur)}
            oninput={(e) => onRgbChange(ch as 'r'|'g'|'b', +(e.currentTarget as HTMLInputElement).value)}
            class="w-full rounded border border-border bg-surface px-1 py-0.5 font-mono text-xs"
          />
        </label>
      {/each}
    </div>
  </div>

  <!-- ── EyeDropper + commit-to-recent + close ──────────────── -->
  <div class="mt-3 flex items-center gap-1">
    <button
      type="button"
      onclick={pickWithEyeDropper}
      disabled={!hasEyeDropper}
      class="inline-flex h-7 items-center gap-1 rounded border border-border px-2 text-xs text-fg disabled:opacity-40"
      title={hasEyeDropper ? 'Pick a color from anywhere on screen' : 'Eyedropper requires Chrome / Edge ≥ 95'}
    >
      <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
        <path d="M2 22l1-1h4l9-9-3-3-9 9v4z"/>
        <path d="M14 7l3 3"/>
        <path d="M17 4l3 3-3 3-3-3z"/>
      </svg>
      Eyedropper
    </button>
    <button
      type="button"
      onclick={() => commitToRecent(currentHex)}
      class="inline-flex h-7 items-center rounded border border-border px-2 text-xs text-fg-muted hover:text-fg"
      title="Pin this color to the recent strip"
    >
      Pin
    </button>
    <span class="flex-1"></span>
    {#if onclose}
      <button
        type="button"
        onclick={onclose}
        class="inline-flex h-7 items-center rounded bg-accent px-2 text-xs font-medium text-on-accent"
      >Done</button>
    {/if}
  </div>

  <!-- ── Recent colors strip ────────────────────────────────── -->
  {#if recent.length > 0}
    <div class="mt-3">
      <div class="mb-1 text-[10px] uppercase tracking-wide text-fg-muted">Recent</div>
      <div class="flex flex-wrap gap-1">
        {#each recent as c (c)}
          <button
            type="button"
            onclick={() => pickRecent(c)}
            class="h-5 w-5 rounded-sm ring-1 ring-border hover:ring-accent"
            style:background-color={c}
            title={c}
            aria-label={`Pick ${c}`}
          ></button>
        {/each}
      </div>
    </div>
  {/if}
</div>
