// Sprite Phase 9 — alt-file client helpers.
//
// Thin wrappers around /api/v1/assets/{id}/alternates so the palette
// remap UI doesn't have to hand-roll fetch + header marshalling.
// Mirrors the companion helpers in shape so future sprite-tool work
// (Phase 11 trim output, Phase 13+ authored variants) can use the
// same surface.

export interface Alternate {
  id: string;
  asset_id: string;
  label: string;
  kind: string;
  content_type: string;
  size_bytes: number;
  created_at: string;
  created_by_user_ref: number | null;
  origin_server_id?: string | null;
  metadata: Record<string, unknown>;
}

export async function listAlternates(assetId: string): Promise<Alternate[]> {
  const r = await fetch(`/api/v1/assets/${assetId}/alternates`, { credentials: 'include' });
  if (!r.ok) throw new Error(`listAlternates: HTTP ${r.status}`);
  return (await r.json()) as Alternate[];
}

export async function addAlternate(opts: {
  assetId: string;
  label: string;
  kind?: string;
  contentType?: string;
  metadata?: Record<string, unknown>;
  body: Blob;
}): Promise<Alternate> {
  const headers: Record<string, string> = {
    'X-Alternate-Label': opts.label,
    'Content-Type': 'application/octet-stream',
  };
  if (opts.kind) headers['X-Alternate-Kind'] = opts.kind;
  if (opts.contentType) headers['X-Content-Type'] = opts.contentType;
  if (opts.metadata) headers['X-Alternate-Metadata'] = JSON.stringify(opts.metadata);
  const r = await fetch(`/api/v1/assets/${opts.assetId}/alternates`, {
    method: 'POST',
    credentials: 'include',
    headers,
    body: opts.body,
  });
  if (!r.ok) {
    const j = await r.json().catch(() => ({ error: `HTTP ${r.status}` }));
    throw new Error((j as { error?: string }).error ?? `addAlternate: HTTP ${r.status}`);
  }
  return (await r.json()) as Alternate;
}

export async function removeAlternate(assetId: string, alternateId: string): Promise<void> {
  const r = await fetch(`/api/v1/assets/${assetId}/alternates/${alternateId}`, {
    method: 'DELETE',
    credentials: 'include',
  });
  if (!r.ok) throw new Error(`removeAlternate: HTTP ${r.status}`);
}

/** Direct download URL for an alternate — handy for <a download>. */
export function alternateDownloadURL(assetId: string, alternateId: string): string {
  return `/api/v1/assets/${assetId}/alternates/${alternateId}`;
}
