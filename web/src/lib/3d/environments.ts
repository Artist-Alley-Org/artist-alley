// Procedural environments for the 3D model viewer.
//
// Rather than ship a megabyte of HDRI files we generate equi-
// rectangular gradient textures on the fly + feed them through
// PMREMGenerator. Six presets cover the common product / sculpt /
// look-dev moods a user reaches for:
//
//   studio  — Three.js's RoomEnvironment (neutral box lighting,
//             the look 99 % of viewers default to)
//   park    — outdoor: blue sky, green horizon, dark ground
//   sunset  — warm: orange sky, pink horizon, brown ground
//   city    — neutral grays + subtle warm windows on horizon
//   night   — deep blue sky, near-black ground
//   none    — no env (PBR materials show flat color; key light
//             still illuminates form)
//
// All gradients are 1024×512 — equirectangular convention. The
// PMREMGenerator filters them down to whatever face size three
// needs for the indirect-light convolution. Generation is one-shot
// per preset per session; ModelView caches the resulting texture
// and disposes on unmount.
//
// Matcap textures are similarly procedural (small radial gradient
// disk) so the Normals/Matcap render modes don't pull a binary asset.

import * as THREE from 'three';

export type EnvPresetId = 'studio' | 'park' | 'sunset' | 'city' | 'night' | 'none';

interface GradientStop { offset: number; color: string }

const GRADIENTS: Record<Exclude<EnvPresetId, 'studio' | 'none'>, GradientStop[]> = {
  park: [
    { offset: 0.0,  color: '#9ec5e8' },
    { offset: 0.45, color: '#cfe2c0' },
    { offset: 0.55, color: '#6b8f4f' },
    { offset: 1.0,  color: '#2a3a1c' },
  ],
  sunset: [
    { offset: 0.0,  color: '#3b2055' },
    { offset: 0.4,  color: '#e98a4a' },
    { offset: 0.55, color: '#ffb27a' },
    { offset: 1.0,  color: '#2a1310' },
  ],
  city: [
    { offset: 0.0,  color: '#8a98a8' },
    { offset: 0.45, color: '#b6bdc6' },
    { offset: 0.52, color: '#c2a474' }, // warm window band
    { offset: 0.58, color: '#5f6873' },
    { offset: 1.0,  color: '#1f242b' },
  ],
  night: [
    { offset: 0.0,  color: '#101627' },
    { offset: 0.5,  color: '#1a223a' },
    { offset: 1.0,  color: '#020409' },
  ],
};

function buildGradientCanvas(stops: GradientStop[]): HTMLCanvasElement {
  const c = document.createElement('canvas');
  c.width = 1024;
  c.height = 512;
  const ctx = c.getContext('2d');
  if (!ctx) return c;
  const g = ctx.createLinearGradient(0, 0, 0, c.height);
  for (const s of stops) g.addColorStop(s.offset, s.color);
  ctx.fillStyle = g;
  ctx.fillRect(0, 0, c.width, c.height);
  return c;
}

function buildGradientEquirect(stops: GradientStop[]): THREE.Texture {
  const tex = new THREE.CanvasTexture(buildGradientCanvas(stops));
  tex.mapping = THREE.EquirectangularReflectionMapping;
  tex.colorSpace = THREE.SRGBColorSpace;
  tex.needsUpdate = true;
  return tex;
}

/** Build the IBL + background pair for a given preset.
 *  Caller owns disposal of both. */
export async function buildEnvironment(
  id: EnvPresetId,
  pmrem: THREE.PMREMGenerator,
): Promise<{ env: THREE.Texture | null; background: THREE.Texture | THREE.Color | null }> {
  if (id === 'none') {
    return { env: null, background: new THREE.Color(0x0b0b0c) };
  }
  if (id === 'studio') {
    const { RoomEnvironment } = await import('three/examples/jsm/environments/RoomEnvironment.js');
    const env = pmrem.fromScene(new RoomEnvironment(), 0.04).texture;
    // Studio's "background" is a flat tone so the IBL details
    // don't read as visual noise behind the model.
    return { env, background: new THREE.Color(0x202024) };
  }
  const equirect = buildGradientEquirect(GRADIENTS[id]);
  const env = pmrem.fromEquirectangular(equirect).texture;
  // Reuse the same equirect texture as the rendered background so
  // tone-mapping makes the env-vs-background match exactly. Caller
  // disposes both.
  return { env, background: equirect };
}

/** Tiny matcap — a soft diagonal radial gradient that reads as a
 *  generic clay sphere lit from the upper-left. 256² is plenty
 *  for the normals visualisation mode. */
export function buildDefaultMatcap(): THREE.Texture {
  const c = document.createElement('canvas');
  c.width = 256;
  c.height = 256;
  const ctx = c.getContext('2d');
  if (ctx) {
    // Background dark
    ctx.fillStyle = '#1a1a1f';
    ctx.fillRect(0, 0, 256, 256);
    // Sphere highlight
    const g = ctx.createRadialGradient(96, 96, 8, 128, 128, 140);
    g.addColorStop(0.0, '#ffffff');
    g.addColorStop(0.25, '#c8cdd4');
    g.addColorStop(0.7, '#5a6068');
    g.addColorStop(1.0, '#1a1a1f');
    ctx.beginPath();
    ctx.arc(128, 128, 124, 0, Math.PI * 2);
    ctx.closePath();
    ctx.fillStyle = g;
    ctx.fill();
  }
  const tex = new THREE.CanvasTexture(c);
  tex.colorSpace = THREE.SRGBColorSpace;
  return tex;
}

/** Tone-mapping ids the UI exposes. Maps to three's constants in
 *  the viewer; declared here so the session + the dropdown agree
 *  on the wire shape. */
export type ToneMappingId =
  | 'none' | 'linear' | 'reinhard' | 'cineon' | 'aces' | 'neutral';

export function toneMappingValue(id: ToneMappingId): THREE.ToneMapping {
  switch (id) {
    case 'linear':   return THREE.LinearToneMapping;
    case 'reinhard': return THREE.ReinhardToneMapping;
    case 'cineon':   return THREE.CineonToneMapping;
    case 'aces':     return THREE.ACESFilmicToneMapping;
    // Three's "Neutral" tone mapping landed in r166; available in
    // our pinned r182. It's the modern default Khronos pushes for
    // glTF — gentle, no saturation pumping at clipping.
    case 'neutral':  return (THREE as unknown as { NeutralToneMapping: THREE.ToneMapping }).NeutralToneMapping ?? THREE.ACESFilmicToneMapping;
    case 'none':
    default:         return THREE.NoToneMapping;
  }
}
