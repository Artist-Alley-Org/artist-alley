#!/usr/bin/env bash
# scripts/generate.sh
#
# Regenerate every machine-generated Go file in app/. Run this after
# editing any *.sql or *.yaml spec under app/.
#
# Outputs:
#   app/internal/<feature>/queries.sql.go, models.go, db.go  (sqlc)
#   app/internal/openapi/openapi.gen.go                       (oapi-codegen)
#   app/internal/sitetext/catalogue.json                      (copy of en.json)
#
# Both generators run in throwaway containers — no host toolchain
# required beyond Docker.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

step() { printf '\n\033[1;36m==>\033[0m %s\n' "$*"; }

step "sqlc: regenerating query types"
docker run --rm \
    -v "${ROOT}/app:/src" \
    -w /src \
    sqlc/sqlc:latest \
    generate

step "oapi-codegen: regenerating OpenAPI types and server interface"
# Version pinned per §7.1 of docs/v0_1_readiness.md — using `@latest`
# silently drifted v2.7.1 → v2.7.2 on dev and broke Codegen check
# for a week (PR #217). Bump the pin here + regen when upgrading;
# never widen back to @latest.
OAPI_CODEGEN_VERSION=v2.7.2
docker run --rm \
    -v "${ROOT}/app:/src" \
    -w /src/api \
    -e OAPI_CODEGEN_VERSION="${OAPI_CODEGEN_VERSION}" \
    golang:1.26 \
    sh -c '
        go install "github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@${OAPI_CODEGEN_VERSION}" >/dev/null 2>&1
        /go/bin/oapi-codegen -config oapi-codegen.yaml openapi.yaml
    '

step "site-text catalogue: copying the shipped en.json into the Go tree"
# The write API for operator string overrides refuses any key the
# shipped catalogue does not contain (ADR 0081 §1: "an override that
# names a key that does not exist must fail loudly"). The server can
# only enforce that if it knows the key set — and the catalogue lives in
# the frontend tree, which the Go binary does not read at runtime.
#
# So the catalogue is copied here and `go:embed`ed, exactly like
# openapi.gen.go: a committed generated artifact, which is what puts it
# under CI's drift check. Edit en.json without re-running this and the
# check fails, rather than the server silently validating against a
# stale key set.
cp "${ROOT}/web/src/lib/i18n/en.json" "${ROOT}/app/internal/sitetext/catalogue.json"

step "gen-panicshim: regenerating strictservershim.PanicShim from openapi.gen.go"
"${ROOT}/scripts/gen-panicshim.sh" \
    "${ROOT}/app/internal/openapi/openapi.gen.go" \
    "${ROOT}/app/internal/openapi/strictservershim/panicshim_gen.go"

step "Generated files now match the specs."
echo "Verify with: cd app && go build ./... && ./scripts/test.sh --go"
