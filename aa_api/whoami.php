<?php
/**
 * GET /api/v1/legacy/whoami
 *
 * Returns the authenticated user's RS-side identity. Useful as a
 * smoke test of the legacy-proxy pipeline (auth + cookie forwarding
 * + RS function call + JSON shape) and as a way for the frontend to
 * inspect the user's RS usergroup permissions, which aren't yet
 * mirrored into the Go-side capability model.
 *
 * Ported-in-phase: never. This endpoint is the smoke test for
 * ADR 0015 and stays as long as the legacy proxy exists. Delete
 * with the rest of aa_api/ once PHP is gone.
 */

declare(strict_types=1);

require_once __DIR__ . '/_bootstrap.php';

// $userref is set by the bootstrap; get_user() is an RS function
// that returns the joined user + usergroup row.
$user = get_user($userref);
if (!is_array($user)) {
    aa_error('user not found', 404);
}

aa_json([
    'ref'        => (int) $user['ref'],
    'username'   => $user['username'] ?? null,
    'fullname'   => $user['fullname'] ?? null,
    'email'      => $user['email'] ?? null,
    'usergroup'  => isset($user['usergroup']) ? (int) $user['usergroup'] : null,
    'groupname'  => $user['groupname'] ?? null,
    // RS encodes permissions as a comma-separated codes list; the
    // frontend treats it as opaque, but exposing it lets us debug.
    'rs_permissions' => $user['permissions'] ?? null,
]);
