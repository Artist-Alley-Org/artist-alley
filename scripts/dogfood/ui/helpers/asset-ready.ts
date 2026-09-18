// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Wait for one asset's preview pipeline to settle (#1401).
//
// `POST /assets` commits the row, enqueues the preview job and answers
// 201. The worker may already be running by the time the response is
// read, and on a successful text preview it writes the row at least
// three more times, each with `updated_at = now()`: MarkAssetProcessing,
// MergeAssetMetadata and the final MarkAssetReady (preview/text.go). So
// an editor opened straight off the 201 is built on a baseline the
// pipeline is still moving, and `PATCH /assets/{id}` refuses the save
// with a 409 exactly as the optimistic-concurrency guard (#549) says it
// should. The product is right; the fixture was handing it a stale
// baseline.
//
// `processing_status = ready` is the terminal state of that write
// history for a successful preview. After it, nothing on the pipeline
// touches `updated_at` again, which is what makes a baseline read AFTER
// readiness a settled one.
//
// The contract is READY ONLY:
//   * `ready`   resolves with the last observed asset representation.
//   * `failed`  throws at once. A failed preview has a DIFFERENT write
//               history (MarkAssetFailed, and none of the later writers),
//               so it is not a valid subject for a test that reasons
//               about the settled row; accepting it would make the
//               fixture pass on a broken worker.
//   * timeout   throws at the deadline, naming the last observed state.
// A non-OK answer inside the deadline is tolerated and polling continues,
// which is the convention the inline pollers already follow
// (kind-vocabulary-1417.spec.ts). Nothing retries as a fix and nothing
// sleeps beyond the poll interval.
//
// The request parameter is shaped, not typed as APIRequestContext, so a
// test can hand it a scripted double and assert the polling itself.

export interface AssetReadyResponse {
  ok(): boolean;
  status(): number;
  json(): Promise<unknown>;
}

export interface AssetReadyRequest {
  get(url: string): Promise<AssetReadyResponse>;
}

export interface AssetReadyOptions {
  /** Interval between polls. Default 2 s. */
  pollMs?: number;
  /** Total wait bound. Default 180 s, the bound the inline pollers use. */
  timeoutMs?: number;
  /** Prefix for error messages, so a failure names the fixture that waited. */
  label?: string;
}

const sleep = (ms: number) => new Promise<void>((r) => setTimeout(r, ms));

/**
 * Poll `GET /api/v1/assets/{id}` until `processing_status` is `ready`,
 * and resolve with that representation. Throws on `failed` immediately,
 * and at the deadline otherwise; both messages carry the last observed
 * state.
 */
export async function waitForAssetReady(
  request: AssetReadyRequest,
  id: string,
  opts: AssetReadyOptions = {},
): Promise<Record<string, unknown>> {
  const pollMs = opts.pollMs ?? 2_000;
  const timeoutMs = opts.timeoutMs ?? 180_000;
  const label = opts.label ?? 'asset';
  const deadline = Date.now() + timeoutMs;

  let last: Record<string, unknown> | undefined;
  let lastStatus: number | undefined;
  let polls = 0;

  for (;;) {
    polls += 1;
    const res = await request.get(`/api/v1/assets/${id}`);
    lastStatus = res.status();
    if (res.ok()) {
      last = (await res.json()) as Record<string, unknown>;
      const state = last.processing_status;
      if (state === 'ready') return last;
      if (state === 'failed') {
        throw new Error(
          `${label} ${id} preview FAILED after ${polls} poll(s); ` +
            `last observed state ${describe(last, lastStatus)}`,
        );
      }
    }
    if (Date.now() + pollMs > deadline) {
      throw new Error(
        `${label} ${id} never reached processing_status=ready within ${timeoutMs}ms ` +
          `(${polls} poll(s)); last observed state ${describe(last, lastStatus)}`,
      );
    }
    await sleep(pollMs);
  }
}

function describe(last: Record<string, unknown> | undefined, status: number | undefined): string {
  if (!last) return `(no OK response yet; last HTTP status ${status ?? 'none'})`;
  return `processing_status: ${String(last.processing_status)} ${JSON.stringify(last)}`;
}
