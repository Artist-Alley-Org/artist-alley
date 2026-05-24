# artist-alley tests

These are *artist-alley's* tests, separate from ResourceSpace's own
`tests/` infrastructure at the repo root. They run against the live
PostgreSQL container.

## Layout

```
tests/aa/
├── bootstrap.php                  # shared setup (env, stubs, framework)
├── integration/                   # tests that hit real Postgres
│   └── *_test.php
└── README.md                      # this file
```

Future categories (unit, e2e) can sit as siblings to `integration/`
when they're warranted.

## Running

Local — the container stack must be up (`docker compose up -d`):

```bash
./scripts/test.sh                                          # every test file
./scripts/test.sh tests/aa/integration/db_connection_test.php  # one file
```

CI runs the same script on every push to `main`, `dev`, and `feat/**`
branches (see `.github/workflows/ci.yml`).

## Adding a new test file

Three steps:

1. Create `tests/aa/integration/<area>_test.php`.

2. Boilerplate is one line:

   ```php
   <?php
   require __DIR__ . '/../bootstrap.php';

   sql_connect();

   aa_test('description of the case', function () {
       // ... do work ...
       aa_assert(...);
       aa_assert_eq(expected, actual, 'what failed');
       return 'optional detail string shown next to PASS';
   });

   // No summary call needed — bootstrap.php registers a shutdown handler
   // that prints "N passed, M failed" and exits non-zero on failure.
   ```

3. Run `./scripts/test.sh` and confirm green.

The bootstrap pulls connection settings from these environment variables,
falling back to sensible defaults:

| Variable        | Default        |
|-----------------|----------------|
| `AA_DB_HOST`    | `postgres`     |
| `AA_DB_PORT`    | `5432`         |
| `AA_DB_USER`    | `artist_alley` |
| `AA_DB_PASSWORD`| _(empty)_      |
| `AA_DB_NAME`    | `artist_alley` |

`scripts/test.sh` exports them from your `.env` automatically.

## Conventions

- **Test files end in `_test.php`.** The runner discovers them by glob.
- **Tests should be isolated.** Use `CREATE TEMP TABLE` (session-scoped,
  auto-dropped) rather than `CREATE TABLE`. If you need persistent fixtures
  across tests in a file, set them up once at the top of the file after
  `sql_connect()`.
- **No external dependencies.** The framework is just `aa_test`,
  `aa_assert`, `aa_assert_eq`. No PHPUnit, no Composer dev requires.
  Keep it that way unless we hit something the trio can't express.
- **Return a short detail string from each test closure.** It appears
  next to PASS in the runner output, making failures and regressions
  easier to skim.

## Why these tests exist

Phase 0.5 of artist-alley is a substantial fork of ResourceSpace's
database layer (MySQL → PostgreSQL). Without tests, every dialect
porting commit risks silent regressions in unrelated code paths.
Anchoring an integration test to each rewritten primitive (connection,
ps_query, transactions, insert id, limit/offset, identifier quoting)
keeps that risk bounded.
