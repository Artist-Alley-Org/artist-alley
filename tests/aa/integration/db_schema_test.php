<?php
/**
 * Integration tests for the Postgres schema emitter — i.e., the rewritten
 * CheckDBStruct() and friends that translate RS's MySQL-flavoured
 * dbstruct/*.txt files into Postgres-native DDL.
 *
 * The tests build a temporary dbstruct directory and verify that the
 * emitter creates the expected tables, columns, indexes, and seed data
 * without errors.
 */

declare(strict_types=1);

require __DIR__ . '/../bootstrap.php';

sql_connect();

// --- Per-test fixture helpers ---------------------------------------------

/**
 * Build a fresh dbstruct directory containing the given files and return
 * its path. Files are an associative array of "filename" => "csv contents".
 */
function aa_make_dbstruct(string $tag, array $files): string
{
    $dir = sys_get_temp_dir() . '/aa_dbstruct_' . $tag . '_' . bin2hex(random_bytes(4));
    mkdir($dir, 0777, true);
    foreach ($files as $name => $contents) {
        file_put_contents("{$dir}/{$name}", $contents);
    }
    return $dir;
}

/**
 * Drop tables we may have created in earlier test runs so the suite is
 * idempotent against an already-populated DB.
 */
function aa_drop_test_tables(array $tables): void
{
    foreach ($tables as $t) {
        ps_query('DROP TABLE IF EXISTS ' . aa_quote_ident($t) . ' CASCADE', [], '', -1, false);
    }
}

// --- Tests ----------------------------------------------------------------

aa_test('Type mapping: tinyint(1) -> SMALLINT (not BOOLEAN, for legacy compat)', function () {
    // We deliberately do NOT use Postgres BOOLEAN for tinyint(1) during
    // the PHP-on-Postgres transition because RS code uses `WHERE x = 1`
    // / `WHERE x = 0` constructs that BOOLEAN rejects. See ADR 0006 +
    // aa_mysql_to_pg_type for context. When Go owns the schema, BOOLEAN
    // comes back.
    $info = aa_mysql_to_pg_type('tinyint(1)');
    aa_assert_eq('SMALLINT', $info['type']);
    aa_assert(!$info['is_boolean'], 'is_boolean must be false during transition');
    return 'SMALLINT';
});

aa_test('Type mapping: int(11) -> BIGINT', function () {
    $info = aa_mysql_to_pg_type('int(11)');
    aa_assert_eq('BIGINT', $info['type']);
    aa_assert(!$info['is_boolean'], 'flag wrongly set');
    return 'BIGINT';
});

aa_test('Type mapping: datetime -> TIMESTAMPTZ', function () {
    aa_assert_eq('TIMESTAMPTZ', aa_mysql_to_pg_type('datetime')['type']);
    aa_assert_eq('TIMESTAMPTZ', aa_mysql_to_pg_type('timestamp')['type']);
    return 'datetime, timestamp';
});

aa_test('Type mapping: varchar(N) preserved', function () {
    aa_assert_eq('VARCHAR(200)', aa_mysql_to_pg_type('varchar(200)')['type']);
    return 'VARCHAR(200)';
});

aa_test('Type mapping: text variants -> TEXT', function () {
    foreach (['text', 'mediumtext', 'longtext', 'tinytext'] as $t) {
        aa_assert_eq('TEXT', aa_mysql_to_pg_type($t)['type'], "{$t} mapping");
    }
    return 'TEXT for all text variants';
});

aa_test('DEFAULT: CURRENT_TIMESTAMP passes through unquoted', function () {
    aa_drop_test_tables(['aa_ct']);
    $dir = aa_make_dbstruct('ct', [
        'table_aa_ct.txt' =>
            "ref,int(11),NO,PRI,,auto_increment\n"
          . "when,datetime,NO,,CURRENT_TIMESTAMP,\n",
    ]);
    CheckDBStruct($dir);
    // Insert a row, leaving "when" unset so the default fires.
    ps_query("INSERT INTO aa_ct (ref) VALUES (DEFAULT)", [], '', -1, false);
    $rows = ps_query('SELECT "when" AS value FROM aa_ct ORDER BY ref', [], '', -1, false);
    aa_assert(!empty($rows[0]['value']), '"when" should be populated by CURRENT_TIMESTAMP');
    return 'when=' . $rows[0]['value'];
});

aa_test('DEFAULT: NULL keyword passes through unquoted', function () {
    aa_drop_test_tables(['aa_dn']);
    $dir = aa_make_dbstruct('dn', [
        'table_aa_dn.txt' =>
            "ref,int(11),NO,PRI,,auto_increment\n"
          . "note,text,YES,,NULL,\n",
    ]);
    CheckDBStruct($dir); // should not throw
    return 'CREATE TABLE accepted DEFAULT NULL';
});

aa_test('Type mapping: decimal with § comma escape', function () {
    aa_assert_eq('NUMERIC(17,4)', aa_mysql_to_pg_type('decimal(17§4)')['type']);
    return 'NUMERIC(17,4)';
});

aa_test('CREATE TABLE: basic with PRIMARY KEY + IDENTITY', function () {
    aa_drop_test_tables(['aa_t1']);
    $dir = aa_make_dbstruct('basic', [
        'table_aa_t1.txt' =>
            "ref,int(11),NO,PRI,,auto_increment\n"
          . "title,varchar(200),YES,,,\n"
          . "active,tinyint(1),NO,,1,\n"
          . "created_at,datetime,YES,,,\n",
    ]);

    CheckDBStruct($dir);

    $cols = aa_pg_list_columns('aa_t1');
    aa_assert_eq(['ref', 'title', 'active', 'created_at'], $cols);

    // ref is BIGINT IDENTITY.
    $info = ps_query(
        "SELECT data_type AS value, is_identity AS is_identity FROM information_schema.columns
         WHERE table_name = 'aa_t1' AND column_name = 'ref'",
        [], '', -1, false
    );
    aa_assert_eq('bigint', $info[0]['value'], 'ref data_type');
    aa_assert_eq('YES', $info[0]['is_identity'], 'ref is_identity');

    // active is SMALLINT (transitional choice — see ADR 0006) with default 1.
    $info = ps_query(
        "SELECT data_type AS value, column_default AS def FROM information_schema.columns
         WHERE table_name = 'aa_t1' AND column_name = 'active'",
        [], '', -1, false
    );
    aa_assert_eq('smallint', $info[0]['value'], 'active data_type');
    aa_assert_eq('1', $info[0]['def'], 'active default');

    return 'aa_t1 created with PK, IDENTITY, SMALLINT default';
});

aa_test('CREATE TABLE: idempotent re-run', function () {
    // Running CheckDBStruct twice should NOT error.
    $dir = aa_make_dbstruct('idem', [
        'table_aa_t2.txt' => "ref,int(11),NO,PRI,,auto_increment\nname,varchar(50),YES,,,\n",
    ]);
    aa_drop_test_tables(['aa_t2']);
    CheckDBStruct($dir);
    CheckDBStruct($dir); // second run should be a no-op for existing tables
    $cols = aa_pg_list_columns('aa_t2');
    aa_assert_eq(['ref', 'name'], $cols);
    return 'two runs, no error';
});

aa_test('ALTER TABLE: add column to existing table', function () {
    aa_drop_test_tables(['aa_t3']);
    $dir = aa_make_dbstruct('alter1', [
        'table_aa_t3.txt' => "ref,int(11),NO,PRI,,auto_increment\nname,varchar(50),YES,,,\n",
    ]);
    CheckDBStruct($dir);

    // Add a new column to the dbstruct and re-run.
    file_put_contents(
        "{$dir}/table_aa_t3.txt",
        "ref,int(11),NO,PRI,,auto_increment\n"
      . "name,varchar(50),YES,,,\n"
      . "description,text,YES,,,\n"
    );
    CheckDBStruct($dir);

    $cols = aa_pg_list_columns('aa_t3');
    aa_assert_eq(['ref', 'name', 'description'], $cols);
    return 'description column added';
});

aa_test('Seed data: loaded from data_*.txt', function () {
    aa_drop_test_tables(['aa_t4']);
    $dir = aa_make_dbstruct('seed', [
        'table_aa_t4.txt' =>
            "ref,int(11),NO,PRI,,auto_increment\n"
          . "name,varchar(50),YES,,,\n"
          . "is_default,tinyint(1),NO,,0,\n",
        'data_aa_t4.txt' =>
            "1,Photo,1\n"
          . "2,Document,0\n"
          . "3,,1\n", // empty name -> NULL
    ]);
    CheckDBStruct($dir);

    $rows = ps_query('SELECT ref, name, is_default FROM aa_t4 ORDER BY ref', [], '', -1, false);
    aa_assert_eq(3, count($rows));
    aa_assert_eq('Photo', $rows[0]['name']);
    aa_assert_eq(1,       (int) $rows[0]['is_default'], 'is_default round-trips as 1');
    aa_assert_eq(0,       (int) $rows[1]['is_default'], 'is_default round-trips as 0');
    aa_assert_eq(null,    $rows[2]['name'], 'empty CSV cell -> NULL');
    return '3 seed rows, empty -> NULL, tinyint(1) preserved as 0/1';
});

aa_test('CREATE TABLE: NOT NULL without explicit DEFAULT gets implicit one', function () {
    // Mirrors RS plugin INSERTs that omit columns RS expects MySQL to
    // backfill with empty/0. Postgres enforces NOT NULL strictly; the
    // schema emitter inserts an implicit DEFAULT for such columns.
    aa_drop_test_tables(['aa_impd']);
    $dir = aa_make_dbstruct('impd', [
        'table_aa_impd.txt' =>
            "ref,int(11),NO,PRI,,auto_increment\n"
          . "title,varchar(50),NO,,,\n"        // NOT NULL, no default
          . "count,int(11),NO,,,\n"            // NOT NULL, no default
          . "active,tinyint(1),NO,,,\n",       // NOT NULL, no default
    ]);
    CheckDBStruct($dir);
    // INSERT only the columns we care about. The others should backfill
    // to '' / 0 / 0 via the implicit DEFAULTs we emitted.
    ps_query("INSERT INTO aa_impd (title) VALUES (?)", ['s', 'hi'], '', -1, false);
    $rows = ps_query("SELECT title, count, active FROM aa_impd", [], '', -1, false);
    aa_assert_eq('hi', $rows[0]['title']);
    aa_assert_eq(0,    (int) $rows[0]['count']);
    aa_assert_eq(0,    (int) $rows[0]['active']);
    return 'NOT NULL backfilled with type defaults';
});

aa_test('Seed data: empty cell into NOT NULL column uses type-appropriate default', function () {
    // Mirrors RS's `collection.keywords` situation: TEXT NOT NULL, no
    // explicit DEFAULT, empty cell in data_*.txt. Upstream MySQL with
    // sql_mode="" silently coerces NULL -> ''; we do the same coercion
    // explicitly so Postgres accepts the row.
    aa_drop_test_tables(['aa_notnull']);
    $dir = aa_make_dbstruct('notnull', [
        'table_aa_notnull.txt' =>
            "ref,int(11),NO,PRI,,auto_increment\n"
          . "title,text,NO,,,\n"
          . "count,int(11),NO,,,\n"
          . "active,tinyint(1),NO,,,\n",
        'data_aa_notnull.txt' =>
            "1,Real,5,1\n"
          . "2,,,\n", // all-empty after ref should coerce to '', 0, false
    ]);

    CheckDBStruct($dir);

    $rows = ps_query('SELECT ref, title, count, active FROM aa_notnull ORDER BY ref', [], '', -1, false);
    aa_assert_eq(2, count($rows));
    aa_assert_eq('Real', $rows[0]['title']);
    aa_assert_eq(5,      (int) $rows[0]['count']);
    aa_assert_eq(1,      (int) $rows[0]['active']);
    aa_assert_eq('',     $rows[1]['title'],          'NOT NULL text -> empty string');
    aa_assert_eq(0,      (int) $rows[1]['count'],    'NOT NULL int -> 0');
    aa_assert_eq(0,      (int) $rows[1]['active'],   'NOT NULL tinyint(1) -> 0');
    return 'NULL coerced to type-appropriate defaults';
});

aa_test('Indexes: unique + non-unique', function () {
    aa_drop_test_tables(['aa_t5']);
    $dir = aa_make_dbstruct('idx', [
        'table_aa_t5.txt' =>
            "ref,int(11),NO,PRI,,auto_increment\n"
          . "code,varchar(20),NO,,,\n"
          . "owner,int(11),YES,,,\n",
        'index_aa_t5.txt' =>
            "aa_t5,0,aa_t5_code_uniq,1,code,A,0,,,,BTREE,\n"
          . "aa_t5,1,aa_t5_owner,1,owner,A,0,,,YES,BTREE,\n",
    ]);
    CheckDBStruct($dir);

    $idx = aa_pg_list_indexes('aa_t5');
    aa_assert(in_array('aa_t5__aa_t5_code_uniq', $idx, true), 'unique idx missing: ' . json_encode($idx));
    aa_assert(in_array('aa_t5__aa_t5_owner', $idx, true), 'non-unique idx missing: ' . json_encode($idx));

    // Confirm uniqueness. ps_query routes errors through the global
    // errorhandler which exit()s, so for expected-to-fail queries we go
    // directly through the PDO connection.
    ps_query("INSERT INTO aa_t5 (code, owner) VALUES (?, ?)", ['s', 'A', 'i', 1], '', -1, false);
    $raw = $GLOBALS['db']['read_write'];
    $threw = false;
    try {
        $stmt = $raw->prepare('INSERT INTO aa_t5 (code, owner) VALUES (?, ?)');
        $stmt->execute(['A', 2]);
    } catch (PDOException $e) {
        $threw = true;
        aa_assert_eq('23505', $e->errorInfo[0], 'unique-violation SQLSTATE');
    }
    aa_assert($threw, 'duplicate insert should have raised PDOException');
    return 'unique + non-unique created; uniqueness enforced (SQLSTATE 23505)';
});

aa_test('Indexes: FULLTEXT skipped with debug note', function () {
    aa_drop_test_tables(['aa_t6']);
    $dir = aa_make_dbstruct('fts', [
        'table_aa_t6.txt' => "ref,int(11),NO,PRI,,auto_increment\nbody,text,YES,,,\n",
        'index_aa_t6.txt' => "aa_t6,1,fts_body,1,body,,0,,,YES,FULLTEXT,,\n",
    ]);
    CheckDBStruct($dir); // should NOT throw on the FULLTEXT row

    $idx = aa_pg_list_indexes('aa_t6');
    aa_assert(!in_array('aa_t6__fts_body', $idx, true), 'FULLTEXT should NOT have been created: ' . json_encode($idx));
    return 'FULLTEXT skipped (no DDL emitted)';
});

aa_test('CheckDBStruct: returns false on missing path', function () {
    $r = CheckDBStruct('/tmp/definitely_does_not_exist_aa_' . bin2hex(random_bytes(3)));
    aa_assert_eq(false, $r);
    return 'false on missing path';
});
