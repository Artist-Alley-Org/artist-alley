---
id: "0015"
title: PHP as legacy backend — `aa_api/` JSON wrappers behind `/api/v1/legacy/*`
status: accepted
date: 2026-05-26
area: architecture
phases: 
  - "1.13.H"
supersedes: []
related: 
  - "0003"
  - "0006"
  - "0014"
tags:
  - architecture
  - ai
excerpt: >-
  ADR 0014 retires RS PHP as the user-facing frontend in favour of the new SvelteKit app. But RS still owns a large body of business logic the Go side hasn't ported yet (asset search variants, resource-share URL generation, plugin hooks, the upload pipeline, etc.). Porting every…
---
## Context

ADR 0014 retires RS PHP as the user-facing frontend in favour of the
new SvelteKit app. But RS still owns a large body of business logic
the Go side hasn't ported yet (asset search variants, resource-share
URL generation, plugin hooks, the upload pipeline, etc.). Porting
every RS function before the new frontend can ship would block
1.13.D–H for weeks.

The pragmatic shape, which the user called out explicitly: keep RS's
PHP **functions** available behind a small JSON wrapper layer, treat
RS as a backend-gap-filler service, and delete each wrapper as the
corresponding Go feature lands. RS's HTML-rendering pages are not
used — only its callable functions.

This ADR specifies the wrapper convention, the routing topology, the
auth contract, and the deletion discipline.

## Decision

### URL surface

```
GET /api/v1/legacy/<wrapper>?<query>
```

`<wrapper>` is a `[A-Za-z0-9_][A-Za-z0-9_/\-]*` path that maps onto
the filesystem at `aa_api/<wrapper>.php`. Nested wrappers
(`/api/v1/legacy/asset/related`) are allowed; nginx and the
filesystem follow the same path separators.

The frontend treats `/api/v1/legacy/*` as part of the regular API
surface — component code does not branch on whether a feature is
Go-backed or PHP-backed. When a wrapper is replaced by a native Go
endpoint at, e.g., `/api/v1/asset/{id}/related`, the frontend's
fetch URL changes in one place and the wrapper is deleted.

### Routing

nginx splits the API namespace at the routing layer:

```
location ~ ^/api/v1/legacy/(?<aa_wrapper>...)$
    → fastcgi_pass php:9000
    → SCRIPT_FILENAME = /var/www/html/aa_api/$aa_wrapper.php

location /api/v1/
    → proxy_pass http://app:8080  (Go binary)
```

Regex locations win over prefix locations in nginx, so the legacy
pattern matches before the catch-all `/api/v1/` proxy. The Go binary
never sees `/api/v1/legacy/*` requests at all — there's no Go-side
proxy code to maintain, and no FastCGI client dependency to add.

In dev, Vite proxies `/api/*` to nginx (`http://nginx:80`); nginx
then performs the split. From the frontend's perspective every API
call has the same upstream.

When PHP is removed (end of Phase 1.13.H or later), the legacy
location block deletes and nginx becomes a plain Go-only proxy. When
nginx itself is removed (post-strangler-fig), the Go binary serves
both `/api/v1/*` and the embedded static frontend directly.

### Wrapper convention

```php
<?php
// aa_api/<wrapper>.php
//
// Ported-in-phase: 1.X (will be replaced by GET /api/v1/<native-path>)

declare(strict_types=1);
require_once __DIR__ . '/_bootstrap.php';

// $userref is set by the bootstrap. Call RS functions, shape JSON.
$result = some_rs_function($userref, aa_query('q', 'string', ''));
aa_json(['items' => $result]);
```

`_bootstrap.php` is the only place that knows about RS's includes,
the cookie auth flow, content-type negotiation, and error handling.
Wrappers stay short — typically 5–15 lines of function call + JSON
shaping.

The `Ported-in-phase` comment at the top of every wrapper is the
deletion contract: when that phase lands, the wrapper file (and the
matching frontend fetch URL) goes.

### Authentication

The frontend's session cookie (`user=<token>`) is the same one RS's
own pages have always read. nginx forwards the cookie verbatim via
FastCGI; `_bootstrap.php` requires `include/authenticate.php`,
which validates the cookie against the `user.session` column and
populates `$userref` + `$usergroup` globals.

`include/authenticate.php` was patched (one location) to recognise
the `AA_API_JSON_REQUEST` constant that `_bootstrap.php` defines.
With the flag set, auth failure exits with a JSON 401/403 body
instead of the HTML redirect intended for browser navigation.

There is no separate token, no JWT, no service-to-service auth
layer. The session cookie *is* the auth — it's already trusted by
both RS and the Go binary, and nothing about adding the wrapper
changes the trust boundary.

### Direct-access blocking

nginx denies `^/aa_api/(/|$)` URLs at the location level — wrappers
are reachable *only* through `/api/v1/legacy/<wrapper>`. This keeps
the supported API surface single-shaped and prevents someone from
discovering and depending on the internal filesystem path.

### Content negotiation

Wrappers always emit `Content-Type: application/json; charset=utf-8`.
`_bootstrap.php` sets the header *after* `include/boot.php` runs
because boot.php unconditionally sets `text/html`. Bootstrap also
turns off `display_errors` so PHP notices never corrupt the JSON
body; errors continue to flow to the PHP error log.

Uncaught exceptions in wrappers are caught by a bootstrap
`set_exception_handler` and surface as `{"error":"internal error"}`
with HTTP 500 — the exception details are logged but never returned
to the client.

### Wrapper-side helpers

`_bootstrap.php` exports three helpers so wrappers don't reinvent
boilerplate:

| Helper | Use |
|---|---|
| `aa_json($data, $status = 200)` | Emit a JSON body with the given status and exit |
| `aa_error($msg, $status = 400)` | Emit `{"error":"..."}` with the given status and exit |
| `aa_query($key, $type, $default)` | Type-cast a `$_GET` parameter (string / int / bool) |

These exist to keep wrappers explicit about expected input types
without growing a full schema/validation layer that RS doesn't use
elsewhere.

## Consequences

**Wins**

- Frontend phases 1.13.C–H stop being blocked on Go ports. Each
  page calls native Go endpoints where they exist, legacy wrappers
  where they don't, with a uniform API surface.
- No new Go dependency. nginx already speaks FastCGI; adding a Go
  FastCGI client (gofast or similar) was the only alternative.
- The "delete the wrapper" step is a checklist item per phase, with
  a per-file comment recording the target phase. Avoids the
  "compatibility shim that lives forever" failure mode.
- RS plugins continue to work (they're still hooked into the same
  PHP function calls that the wrappers invoke).

**Costs**

- Two PHP-vs-Postgres patch surfaces remain: any wrapper we add
  exercises another corner of RS code that may hit the same
  MySQL→PG semantic gaps we patched for the home page. Each new
  wrapper carries that risk.
- One more PHP file to author per legacy feature, with manual JSON
  shaping. Acceptable since wrappers are short.
- nginx must be in the request path during this phase. The "one Go
  binary, three containers" target architecture (per memory) has
  nginx as one of the three containers, so this is on-plan.

## Smoke test

`aa_api/whoami.php` — returns the authenticated user's RS-side
identity (ref, username, fullname, email, usergroup, groupname, RS
permissions string). Used to verify the wrapper pipeline end-to-end:

```bash
SESSION=$(psql -tA -c "SELECT session FROM \"user\" WHERE ref=1;")
curl -H "Cookie: user=$SESSION" /api/v1/legacy/whoami
# → {"ref":1,"username":"admin","groupname":"Super Admin",...}

curl /api/v1/legacy/whoami
# → 403 {"error":"authentication required"}

curl -H "Cookie: user=bogus" /api/v1/legacy/whoami
# → 401 {"error":"session expired"}
```

whoami stays as long as the legacy proxy exists; it is the
canonical smoke test. Other wrappers come and go.

## References

- ADR 0003 — Strangler fig (the original plan; this ADR's wrapper
  pattern is its surviving piece)
- ADR 0014 — Frontend stack (this ADR's companion; together they
  describe how the new UI talks to both the new Go backend and the
  retained RS PHP functions)
- ADR 0006 — Go as target backend (the destination — wrappers
  decrement toward zero as native Go ports land)
