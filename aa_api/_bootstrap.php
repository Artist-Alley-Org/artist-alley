<?php
/**
 * aa_api/_bootstrap.php — shared bootstrap for JSON-wrapper endpoints
 * under `/api/v1/legacy/*`.
 *
 * Each wrapper script under aa_api/ requires this file first. The
 * bootstrap:
 *   1. Loads RS (include/boot.php) so RS functions are callable.
 *   2. Forces RS's auth path to return JSON 401 instead of an HTML
 *      redirect on auth failure (the AA_API_JSON_REQUEST flag, read
 *      by include/authenticate.php).
 *   3. Runs include/authenticate.php to populate $userref / $usergroup.
 *   4. Provides aa_json() / aa_error() helpers so wrappers stay short.
 *
 * Wrappers must NEVER emit any non-JSON output. Errors are surfaced
 * via aa_error() (sets HTTP status + JSON body) — never via echoed
 * HTML, never via PHP warning output.
 *
 * See ADR 0015 for the architectural rationale and the deletion
 * convention (each wrapper has a "ported in phase X" comment and
 * deletes when that phase lands).
 */

declare(strict_types=1);

// Suppress display of PHP warnings — they would corrupt the JSON body.
// Errors still get logged via the existing error log.
ini_set('display_errors', '0');
ini_set('log_errors', '1');

// Flag for include/authenticate.php so it returns 401 JSON instead
// of HTML redirect on auth failure.
define('AA_API_JSON_REQUEST', true);

require_once __DIR__ . '/../include/boot.php';
require_once __DIR__ . '/../include/authenticate.php';

// Set the JSON content type *after* the RS includes — include/boot.php
// unconditionally header()s text/html, which would otherwise overwrite
// anything we set above. Cache-Control is independent of body type so
// goes alongside.
header('Content-Type: application/json; charset=utf-8');
header('Cache-Control: no-store');

// authenticate.php exits with 401/403 + (RS lang string) when auth
// fails; we want JSON. If we reach here without $userref, it means
// the system_login bypass kicked in — for our endpoints that's a
// 401 too.
if (!isset($userref) || $userref <= 0) {
    http_response_code(401);
    echo json_encode(['error' => 'authentication required']);
    exit;
}

// Uncaught exceptions in wrappers become 500 JSON.
set_exception_handler(function (Throwable $e): void {
    error_log('[aa_api] uncaught ' . get_class($e) . ': ' . $e->getMessage()
        . ' at ' . $e->getFile() . ':' . $e->getLine());
    if (!headers_sent()) {
        http_response_code(500);
        header('Content-Type: application/json; charset=utf-8');
    }
    echo json_encode(['error' => 'internal error']);
    exit;
});

/**
 * Emit a JSON response with the given status code and exit.
 */
function aa_json(mixed $data, int $status = 200): never
{
    http_response_code($status);
    echo json_encode($data);
    exit;
}

/**
 * Emit an error JSON ({"error": "..."}) with the given status code
 * and exit. 400 is the default since most wrapper errors are client
 * input mistakes.
 */
function aa_error(string $msg, int $status = 400): never
{
    http_response_code($status);
    echo json_encode(['error' => $msg]);
    exit;
}

/**
 * Read a query string parameter, applying a type cast. Used in place
 * of RS's getval() to keep wrappers explicit about expected types.
 */
function aa_query(string $key, string $type = 'string', mixed $default = null): mixed
{
    if (!isset($_GET[$key])) {
        return $default;
    }
    $v = $_GET[$key];
    return match ($type) {
        'int'    => is_numeric($v) ? (int) $v : $default,
        'bool'   => filter_var($v, FILTER_VALIDATE_BOOLEAN, FILTER_NULL_ON_FAILURE) ?? $default,
        'string' => is_string($v) ? $v : $default,
        default  => $default,
    };
}
