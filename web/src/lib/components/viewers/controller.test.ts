// Kind-routing tests for the viewer controller. Every component
// that mounts an AssetViewer reads kindForExtension / kindForAsset
// to pick the right view body — a regression here renders the
// wrong UI for an asset (audiobook in audio player, sprite in
// generic image viewer, etc.).
//
// Mirrors the backend's assetTypeFor + jobTypeForExt coverage on
// the assets package so a desync between the two layers shows up
// in CI on either side.

import { describe, expect, it } from 'vitest';
import {
  kindForExtension, kindForAsset,
  isImageExt, isVideoExt, is3DExt, isDocExt, isAudiobookExt, isArchiveExt,
  type ViewKind,
} from './controller';

describe('kindForExtension', () => {
  it.each<[string, ViewKind]>([
    // Images
    ['jpg', 'image'], ['JPG', 'image'], ['.png', 'image'],
    ['gif', 'image'], ['webp', 'image'], ['svg', 'image'],
    // Photoshop sources route to image (the AssetViewer's ImageView
    // is the smart fallback for editor-source files).
    ['psd', 'image'], ['eps', 'image'],
    // Video
    ['mp4', 'video'], ['mov', 'video'], ['webm', 'video'], ['mkv', 'video'],
    // Audio
    ['mp3', 'audio'], ['wav', 'audio'], ['flac', 'audio'],
    // Audiobook (must NOT collapse to audio)
    ['m4b', 'audiobook'],
    // 3D
    ['glb', '3d'], ['gltf', '3d'], ['obj', '3d'], ['fbx', '3d'], ['blend', '3d'],
    ['stl', '3d'], ['usd', '3d'], ['mview', '3d'],
    // Doc — text/code via CodeMirror
    ['md', 'doc'], ['txt', 'doc'], ['json', 'doc'], ['yaml', 'doc'],
    ['py', 'doc'], ['go', 'doc'],
    // Note: `.ts` resolves to 'video' (MPEG transport stream wins
    // over TypeScript in VIDEO_EXTS). The backend dispatcher
    // (assets/handler.go assetTypeFor) is the inverse — it picks
    // Code 14 over Video 3 for the same extension. Drift documented
    // in the controller-vs-handler ambiguity tests below; pinned
    // here so a future "fix" makes an explicit product decision.
    // PDF — its own kind
    ['pdf', 'pdf'],
    // Font
    ['ttf', 'font'], ['otf', 'font'], ['woff2', 'font'],
    // EPUB
    ['epub', 'ebook'],
    // Archive (must NOT collapse to image for cbz/cbr — those are
    // wired through asset_type override at the kindForAsset layer.)
    ['zip', 'archive'], ['7z', 'archive'], ['rar', 'archive'],
    ['tar', 'archive'], ['tgz', 'archive'],
    // Unknown / null falls through to placeholder.
    ['weirdformat', 'placeholder'],
    ['', 'placeholder'],
  ])('routes %s to %s', (ext, want) => {
    expect(kindForExtension(ext)).toBe(want);
  });

  it('handles null + undefined as placeholder', () => {
    expect(kindForExtension(null)).toBe('placeholder');
    expect(kindForExtension(undefined)).toBe('placeholder');
  });

  it('strips a leading dot before matching', () => {
    expect(kindForExtension('.mp4')).toBe('video');
    expect(kindForExtension('.PDF')).toBe('pdf');
  });

  // Front/back drift acknowledgements. The backend's assets/handler.go
  // assetTypeFor and the frontend's kindForExtension are NOT
  // canonical mirrors today — they disagree on a few ambiguous
  // extensions. Pinning the current frontend behaviour keeps any
  // future alignment work explicit instead of accidental.
  it('.ts resolves to video on the frontend even though backend says Code', () => {
    expect(kindForExtension('ts')).toBe('video');
  });

  it('.ai resolves to placeholder on the frontend even though backend says Image', () => {
    expect(kindForExtension('ai')).toBe('placeholder');
  });
});

describe('kindForAsset', () => {
  it('asset_type override beats extension', () => {
    // A PNG uploaded as a Sprite (asset_type=13) must route to
    // SpriteView, not the generic image kind.
    expect(kindForAsset({ asset_type: 13, file_extension: 'png' })).toBe('sprite');
    expect(kindForAsset({ asset_type: 11, file_extension: 'mp3' })).toBe('audiobook');
    expect(kindForAsset({ asset_type: 6, file_extension: 'unknown' })).toBe('archive');
  });

  it('falls through to extension when asset_type does not override', () => {
    // asset_type 1 (Image) is not in the override map; extension wins.
    expect(kindForAsset({ asset_type: 1, file_extension: 'mp4' })).toBe('video');
  });

  it('handles null asset_type + missing extension gracefully', () => {
    expect(kindForAsset({ asset_type: null, file_extension: null })).toBe('placeholder');
    expect(kindForAsset({})).toBe('placeholder');
  });

  it('honours extension when asset_type is unknown (not in override map)', () => {
    expect(kindForAsset({ asset_type: 999, file_extension: 'mp4' })).toBe('video');
  });
});

describe('is{Image,Video,3D,Doc,Audiobook,Archive}Ext predicates', () => {
  // Each predicate must round-trip through kindForExtension. Spot-
  // check the easy positives; the comprehensive surface is covered
  // by the kindForExtension table above.
  it('isImageExt', () => {
    expect(isImageExt('jpg')).toBe(true);
    expect(isImageExt('mp4')).toBe(false);
    expect(isImageExt(null)).toBe(false);
  });

  it('isVideoExt', () => {
    expect(isVideoExt('mp4')).toBe(true);
    expect(isVideoExt('mp3')).toBe(false);
  });

  it('is3DExt', () => {
    expect(is3DExt('glb')).toBe(true);
    expect(is3DExt('png')).toBe(false);
  });

  it('isDocExt', () => {
    expect(isDocExt('md')).toBe(true);
    expect(isDocExt('mp4')).toBe(false);
  });

  it('isAudiobookExt', () => {
    expect(isAudiobookExt('m4b')).toBe(true);
    // Audiobook must NOT match generic audio extensions.
    expect(isAudiobookExt('mp3')).toBe(false);
  });

  it('isArchiveExt', () => {
    expect(isArchiveExt('zip')).toBe(true);
    expect(isArchiveExt('7z')).toBe(true);
    expect(isArchiveExt('rar')).toBe(true);
    expect(isArchiveExt('txz')).toBe(true);
    expect(isArchiveExt('mp4')).toBe(false);
  });
});
