# artist-alley — developer + release convenience targets.
#
# The release model is dev -> main -> tag (see RELEASING.md). This
# Makefile codifies the SAFE, AUTOMATABLE release PREP so it isn't
# hand-rolled each time. It deliberately stops short of the
# irreversible / outward-facing steps:
#
#   NOT done here (stay manual, on purpose):
#     * creating/pushing the vX.Y.Z tag — the tag is what fires the
#       release matrix; it's a deliberate post-merge human step.
#     * toggling branch protection — never.
#     * merging the dev->main PR — a human reviews + merges.
#
# `make release VERSION=x.y.z` does the prep + validation; the last
# thing it prints is the exact command to open the dev->main PR and a
# reminder of the manual tag step.

SHELL := /bin/bash
.DEFAULT_GOAL := help

# oapi-codegen version — single source of truth is scripts/generate.sh
# (#377). Read it here so the Makefile's regen can never drift from what
# generate.sh and CI's Codegen drift check use.
OAPI_CODEGEN_VERSION := $(shell sed -n 's/^OAPI_CODEGEN_VERSION=//p' scripts/generate.sh)

.PHONY: help
help: ## Show this help.
	@echo "artist-alley — make targets"
	@echo
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
	  | awk 'BEGIN {FS = ":.*?## "} {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'
	@echo
	@echo "Release: 'make release VERSION=x.y.z' preps the version bump +"
	@echo "regen + opens-the-PR command. It does NOT tag, merge, or touch"
	@echo "branch protection — those stay manual (see RELEASING.md)."

.PHONY: release
release: ## Prep a release: bump version, regen spec, validate. Needs VERSION=x.y.z
	@if [ -z "$(VERSION)" ]; then \
	  echo "ERROR: VERSION is required, e.g. make release VERSION=0.3.1" >&2; exit 2; fi
	@echo "$(VERSION)" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+([-+.0-9A-Za-z]*)?$$' \
	  || { echo "ERROR: VERSION '$(VERSION)' is not semver (x.y.z)" >&2; exit 2; }
	@branch=$$(git rev-parse --abbrev-ref HEAD); \
	if [ "$$branch" != "dev" ]; then \
	  echo "WARNING: on '$$branch', not 'dev' — releases are cut from dev->main." >&2; fi
	@echo "==> Bumping version to $(VERSION)"
	@# web/package.json — only the top-level "version" line.
	@sed -i -E '0,/^(  "version": )"[^"]*"/s//\1"$(VERSION)"/' web/package.json
	@# app/api/openapi.yaml — info.version (the first top-level `version:`).
	@sed -i -E '0,/^  version: .*/s//  version: $(VERSION)/' app/api/openapi.yaml
	@grep -q '"version": "$(VERSION)"' web/package.json || { echo "ERROR: web/package.json bump failed" >&2; exit 1; }
	@grep -q '^  version: $(VERSION)' app/api/openapi.yaml   || { echo "ERROR: openapi.yaml bump failed" >&2; exit 1; }
	@echo "    web/package.json + app/api/openapi.yaml -> $(VERSION)"
	@echo "==> Regenerating openapi.gen.go (oapi-codegen $(OAPI_CODEGEN_VERSION))"
	@# The spec is embedded in the generated server, so the info.version
	@# bump changes openapi.gen.go. Same pinned version CI uses (#377), so
	@# the Codegen drift check will match.
	@test -n "$(OAPI_CODEGEN_VERSION)" || { echo "ERROR: couldn't read OAPI_CODEGEN_VERSION from scripts/generate.sh" >&2; exit 1; }
	@docker run --rm -v "$(CURDIR)/app:/src" -w /src/api golang:1.26 sh -c '\
	  go install github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@$(OAPI_CODEGEN_VERSION) >/dev/null 2>&1 && \
	  /go/bin/oapi-codegen -config oapi-codegen.yaml openapi.yaml'
	@echo "==> Validating: a second regen must be byte-identical (drift check would pass)"
	@before=$$(sha256sum app/internal/openapi/openapi.gen.go | cut -d' ' -f1); \
	docker run --rm -v "$(CURDIR)/app:/src" -w /src/api golang:1.26 sh -c '\
	  go install github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@$(OAPI_CODEGEN_VERSION) >/dev/null 2>&1 && \
	  /go/bin/oapi-codegen -config oapi-codegen.yaml openapi.yaml'; \
	after=$$(sha256sum app/internal/openapi/openapi.gen.go | cut -d' ' -f1); \
	[ "$$before" = "$$after" ] || { echo "ERROR: regen is not deterministic — drift check would fail" >&2; exit 1; }
	@echo "    regen deterministic — Codegen drift check will pass"
	@echo
	@echo "==> Prep complete. Review the diff, commit, and push to dev:"
	@echo "      git add web/package.json app/api/openapi.yaml app/internal/openapi/openapi.gen.go"
	@echo "      git commit -m 'chore(release): v$(VERSION)'"
	@echo "      git push origin dev"
	@echo
	@echo "==> Then open the dev->main release PR:"
	@echo "      gh pr create --base main --head dev --title 'release: v$(VERSION)' \\"
	@echo "        --body 'Release v$(VERSION). Merge with a MERGE COMMIT (not squash).'"
	@echo
	@echo "==> MANUAL, after the PR merges (see RELEASING.md) — NOT done by this target:"
	@echo "      git checkout main && git pull"
	@echo "      git tag -s -a v$(VERSION) -m 'v$(VERSION)' && git push origin v$(VERSION)"
	@echo "    The tag fires the release matrix. Tagging, merging, and branch"
	@echo "    protection stay human decisions on purpose."
