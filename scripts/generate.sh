#!/usr/bin/env bash
# scripts/generate.sh
#
# Regenerate every machine-generated Go file in app/. Run this after
# editing any *.sql or *.yaml spec under app/.
#
# Outputs:
#   app/internal/<feature>/queries.sql.go, models.go, db.go  (sqlc)
#   app/internal/openapi/openapi.gen.go                       (oapi-codegen)
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
docker run --rm \
    -v "${ROOT}/app:/src" \
    -w /src/api \
    golang:1.26 \
    sh -c '
        go install github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@latest >/dev/null 2>&1
        /go/bin/oapi-codegen -config oapi-codegen.yaml openapi.yaml
    '

step "gen-panicshim: regenerating strictservershim.PanicShim from openapi.gen.go"
"${ROOT}/scripts/gen-panicshim.sh" \
    "${ROOT}/app/internal/openapi/openapi.gen.go" \
    "${ROOT}/app/internal/openapi/strictservershim/panicshim_gen.go"

step "Generated files now match the specs."
echo "Verify with: cd app && go build ./... && ./scripts/test.sh --go"
