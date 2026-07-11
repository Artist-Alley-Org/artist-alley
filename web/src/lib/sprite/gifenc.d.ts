// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Local type shim for `gifenc` — the upstream package ships
// ESM only with no .d.ts. Only the surface we actually call is
// declared; full coverage isn't needed.
declare module 'gifenc' {
  export interface PaletteOptions {
    format?: 'rgb444' | 'rgba4444' | 'rgb565' | 'rgb888';
    oneBitAlpha?: boolean | number;
    clearAlpha?: boolean;
    clearAlphaThreshold?: number;
    clearAlphaColor?: number;
  }
  export function quantize(
    rgba: Uint8Array | Uint8ClampedArray,
    maxColors: number,
    options?: PaletteOptions,
  ): number[][];
  export function applyPalette(
    rgba: Uint8Array | Uint8ClampedArray,
    palette: number[][],
    format?: PaletteOptions['format'],
  ): Uint8Array;
  export interface WriteFrameOpts {
    palette?: number[][];
    delay?: number;
    transparent?: boolean;
    transparentIndex?: number;
    dispose?: number;
    first?: boolean;
    repeat?: number;
  }
  export interface Encoder {
    writeFrame(index: Uint8Array, width: number, height: number, opts?: WriteFrameOpts): void;
    finish(): void;
    bytesView(): Uint8Array;
    bytes(): Uint8Array;
  }
  export function GIFEncoder(): Encoder;
}
