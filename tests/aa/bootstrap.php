<?php
/**
 * tests/aa/bootstrap.php
 *
 * Shared setup for every artist-alley test file. A test file just does:
 *
 *     require __DIR__ . '/../bootstrap.php';
 *     sql_connect();
 *
 *     aa_test('something', function () {
 *         aa_assert(...);
 *         return 'detail string for the row';
 *     });
 *
 * Summary + exit code happen automatically via a shutdown handler.
 */

declare(strict_types=1);

define('AA_REPO_ROOT', dirname(__DIR__, 2));

// --- Connection config from env -------------------------------------------
// scripts/test.sh exports these; CI sets them in the workflow file.
$GLOBALS['db_server']   = getenv('AA_DB_HOST')     ?: 'postgres';
$GLOBALS['db_port']     = (int) (getenv('AA_DB_PORT') ?: 5432);
$GLOBALS['db_username'] = getenv('AA_DB_USER')     ?: 'artist_alley';
$GLOBALS['db_password'] = getenv('AA_DB_PASSWORD') ?: '';
$GLOBALS['db_name']     = getenv('AA_DB_NAME')     ?: 'artist_alley';

// --- Minimum globals that include/database_functions.php expects ----------
$GLOBALS['debug_log']                      = false;
$GLOBALS['query_cache_enabled']            = false;
$GLOBALS['use_db_transaction']             = true;
$GLOBALS['config_show_performance_footer'] = false;
$GLOBALS['prepared_statement_cache']       = [];
$GLOBALS['storagedir']                     = '/tmp';
$GLOBALS['tempdir']                        = '/tmp';
$GLOBALS['scramble_key']                   = 'aa_test';
$GLOBALS['query_cache_expires_minutes']    = 30;
$GLOBALS['config_error_reporting']         = E_ALL;
$GLOBALS['plugins']                        = [];

if (!defined('SYSTEM_DATABASE_MAX_RETRIES')) {
    define('SYSTEM_DATABASE_MAX_RETRIES', 0);
}

// --- Stubs for helpers normally provided by other RS includes -------------
// Use function_exists so individual test files can override if they ever need
// richer behaviour.
if (!function_exists('debug'))                  { function debug($s) {} }
if (!function_exists('hook'))                   { function hook($n,$p='',$a=[],$l=false) { return false; } }
if (!function_exists('check_debug_log_override')) { function check_debug_log_override() {} }
if (!function_exists('is_int_loose'))           { function is_int_loose($v) { return is_numeric($v) && (int)$v == $v; } }
if (!function_exists('is_process_lock'))        { function is_process_lock($n) { return false; } }
if (!function_exists('set_process_lock'))       { function set_process_lock($n) { return true; } }
if (!function_exists('clear_process_lock'))     { function clear_process_lock($n) { return true; } }
if (!function_exists('show_upgrade_in_progress')) { function show_upgrade_in_progress($exit=false) {} }

// --- Subject under test ---------------------------------------------------
require AA_REPO_ROOT . '/include/database_functions.php';

// --- Minimal test framework -----------------------------------------------
// Three primitives — aa_test, aa_assert, aa_assert_eq — and an automatic
// summary at script end. No PHPUnit dependency; tests can run with nothing
// but PHP + pdo_pgsql on PATH.

$GLOBALS['aa_test_passed'] = 0;
$GLOBALS['aa_test_failed'] = 0;
$GLOBALS['aa_test_headline_printed'] = false;

function aa_test(string $name, callable $fn): void
{
    if (!$GLOBALS['aa_test_headline_printed']) {
        $u = $GLOBALS['db_username'];
        $h = $GLOBALS['db_server'];
        $p = $GLOBALS['db_port'];
        $d = $GLOBALS['db_name'];
        echo "\n  Target: {$u}@{$h}:{$p}/{$d}\n\n";
        $GLOBALS['aa_test_headline_printed'] = true;
    }
    try {
        $detail = $fn();
        echo sprintf("  \033[32mPASS\033[0m  %-50s  %s\n", $name, is_string($detail) ? $detail : '');
        $GLOBALS['aa_test_passed']++;
    } catch (Throwable $e) {
        echo sprintf("  \033[31mFAIL\033[0m  %-50s  %s\n", $name, $e->getMessage());
        $GLOBALS['aa_test_failed']++;
    }
}

function aa_assert(bool $cond, string $msg): void
{
    if (!$cond) {
        throw new RuntimeException($msg);
    }
}

function aa_assert_eq($expected, $actual, string $what = 'value'): void
{
    if ($expected !== $actual) {
        throw new RuntimeException(
            "{$what}: expected " . var_export($expected, true)
            . ", got " . var_export($actual, true)
        );
    }
}

register_shutdown_function(function () {
    $p = $GLOBALS['aa_test_passed'];
    $f = $GLOBALS['aa_test_failed'];
    echo "\n  {$p} passed, {$f} failed\n";
    if ($f > 0) {
        exit(1);
    }
});
