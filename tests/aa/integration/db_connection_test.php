<?php
/**
 * Integration tests for include/database_functions.php — exercises the
 * PDO+pgsql connection layer against a live Postgres.
 *
 * To run:  ./scripts/test.sh
 * To add cases:  see tests/aa/README.md
 */

declare(strict_types=1);

require __DIR__ . '/../bootstrap.php';

sql_connect();

aa_test('sql_connect()', function () {
    aa_assert(isset($GLOBALS['db']['read_write']), 'read_write connection missing');
    aa_assert($GLOBALS['db']['read_write'] instanceof PDO, 'read_write is not a PDO');
    return 'ok';
});

aa_test('ps_query: SELECT version()', function () {
    $r = ps_query('SELECT version() AS value');
    aa_assert(stripos($r[0]['value'], 'PostgreSQL') !== false, 'no PostgreSQL in version string');
    return explode(' ', $r[0]['value'])[1];
});

aa_test('ps_query: parameter binding (int + string)', function () {
    $r = ps_query('SELECT ? AS n, ? AS s', ['i', 42, 's', 'hello']);
    aa_assert_eq(42, (int) $r[0]['n'], 'int param');
    aa_assert_eq('hello', $r[0]['s'], 'string param');
    return 'n=42 s=hello';
});

aa_test('ps_value: scalar shortcut', function () {
    aa_assert_eq(7, (int) ps_value('SELECT 7 AS value', [], -1));
    return '7';
});

aa_test('ps_value: default on empty result', function () {
    aa_assert_eq('fallback', ps_value('SELECT 1 AS value WHERE FALSE', [], 'fallback'));
    return 'fallback';
});

aa_test('Transaction: BEGIN -> INSERT -> COMMIT', function () {
    db_begin_transaction('tx_test');
    ps_query('CREATE TEMP TABLE aa_tx_test (id INT)', [], '', -1, false);
    ps_query('INSERT INTO aa_tx_test (id) VALUES (?)', ['i', 1], '', -1, false);
    ps_query('INSERT INTO aa_tx_test (id) VALUES (?)', ['i', 2], '', -1, false);
    db_end_transaction('tx_test');
    $n = (int) ps_value('SELECT COUNT(*) AS value FROM aa_tx_test', [], 0);
    aa_assert_eq(2, $n, 'row count after commit');
    return "rows={$n}";
});

aa_test('Transaction: BEGIN -> INSERT -> ROLLBACK', function () {
    ps_query('CREATE TEMP TABLE aa_tx_rb (id INT)', [], '', -1, false);
    db_begin_transaction('rb_test');
    ps_query('INSERT INTO aa_tx_rb (id) VALUES (?)', ['i', 1], '', -1, false);
    db_rollback_transaction('rb_test');
    $n = (int) ps_value('SELECT COUNT(*) AS value FROM aa_tx_rb', [], 0);
    aa_assert_eq(0, $n, 'row count after rollback');
    return "rows={$n}";
});

aa_test('Savepoint: nested rollback preserves outer work', function () {
    ps_query('CREATE TEMP TABLE aa_sp (id INT)', [], '', -1, false);
    db_begin_transaction('outer');
    ps_query('INSERT INTO aa_sp (id) VALUES (1)', [], '', -1, false);
    db_begin_transaction('inner');
    ps_query('INSERT INTO aa_sp (id) VALUES (2)', [], '', -1, false);
    db_rollback_transaction('inner');
    ps_query('INSERT INTO aa_sp (id) VALUES (3)', [], '', -1, false);
    db_end_transaction('outer');
    $ids = array_map('intval', ps_array('SELECT id AS value FROM aa_sp ORDER BY id'));
    aa_assert_eq([1, 3], $ids, 'outer kept 1+3; inner dropped 2');
    return 'kept 1,3; dropped 2';
});

aa_test('sql_insert_id: works with BIGSERIAL', function () {
    ps_query('CREATE TEMP TABLE aa_seq (id BIGSERIAL PRIMARY KEY, x TEXT)', [], '', -1, false);
    ps_query('INSERT INTO aa_seq (x) VALUES (?)', ['s', 'first'], '', -1, false);
    $first = sql_insert_id();
    ps_query('INSERT INTO aa_seq (x) VALUES (?)', ['s', 'second'], '', -1, false);
    $second = sql_insert_id();
    aa_assert_eq(1, $first, 'first id');
    aa_assert_eq(2, $second, 'second id');
    return 'ids=1,2';
});

aa_test('sql_limit emits Postgres LIMIT/OFFSET syntax', function () {
    aa_assert_eq('LIMIT 20 OFFSET 10', sql_limit(10, 20));
    return 'LIMIT 20 OFFSET 10';
});

aa_test('Backtick identifiers are translated to double-quoted', function () {
    $r = ps_query('SELECT 99 AS `space col`', [], '', -1, false);
    aa_assert_eq(99, (int) $r[0]['space_col']);
    return 'space_col=99';
});

aa_test('Dialect translation: IFNULL -> COALESCE', function () {
    $r = ps_query("SELECT IFNULL(NULL, 'fallback') AS value", [], '', -1, false);
    aa_assert_eq('fallback', $r[0]['value']);
    return "IFNULL(NULL,'fallback') -> 'fallback'";
});

aa_test('Dialect translation: LIMIT a,b -> LIMIT b OFFSET a', function () {
    ps_query("CREATE TEMP TABLE aa_lim (id INT)", [], '', -1, false);
    for ($i = 1; $i <= 5; $i++) {
        ps_query("INSERT INTO aa_lim VALUES (?)", ['i', $i], '', -1, false);
    }
    // MySQL-style: offset=2, count=2 -> rows 3 and 4
    $r = ps_query("SELECT id AS value FROM aa_lim ORDER BY id LIMIT 2, 2", [], '', -1, false);
    aa_assert_eq(2, count($r), 'two rows');
    aa_assert_eq(3, (int) $r[0]['value']);
    aa_assert_eq(4, (int) $r[1]['value']);
    return 'LIMIT 2,2 -> rows 3,4';
});

aa_test('Column names with spaces are normalised to underscores', function () {
    $r = ps_query('SELECT 1 AS "spaced col"', [], '', -1, false);
    aa_assert(isset($r[0]['spaced_col']), 'no spaced_col key in ' . json_encode(array_keys($r[0])));
    return 'spaced col -> spaced_col';
});
