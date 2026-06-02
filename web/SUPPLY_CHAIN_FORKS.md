# Frontend supply-chain forks

We mirror small-maintainer npm deps to `mscrnt/*` on GitHub so the
project doesn't break if an upstream maintainer disappears. The
forks live as frozen GitHub mirrors at pinned SHAs; we keep using
the upstream npm packages day-to-day because git-URL installs add
CI complexity for repos that aren't shaped as standalone packages.

If upstream disappears, swap the package.json entry to point at
the fork — instructions per dep below.

| npm package        | Upstream                             | Fork                              | Pinned SHA |
|--------------------|--------------------------------------|-----------------------------------|------------|
| `gifenc`           | mattdesl/gifenc                      | mscrnt/gifenc                     | `27db5b9`  |
| `perfect-freehand` | steveruizok/perfect-freehand         | mscrnt/perfect-freehand           | `176e00f`  |
| `thumbhash`        | evanw/thumbhash                      | mscrnt/thumbhash                  | `a652ce6`  |

## Swap procedure

### gifenc (easy — dist/ committed at repo root)
```json
"gifenc": "github:mscrnt/gifenc#27db5b982dba701ca440b55ea36fad3999040973"
```
Drop in, `npm install`, done.

### perfect-freehand (monorepo — package is in packages/perfect-freehand)
The fork is a yarn workspaces monorepo; the published package
lives under `packages/perfect-freehand`. Options when needed:
1. **Publish to npm under @mscrnt scope** (cleanest):
   `cd packages/perfect-freehand && npm publish --access public`
   then `"perfect-freehand": "npm:@mscrnt/perfect-freehand@1.2.3"`.
2. **Vendor as a workspace package**: copy
   `packages/perfect-freehand/` into `web/vendor/perfect-freehand/`
   and reference via `"perfect-freehand": "file:./vendor/perfect-freehand"`.
3. **Use a gitpkg.now.sh URL** (third-party proxy that resolves
   monorepo subpaths): `"perfect-freehand": "https://gitpkg.now.sh/mscrnt/perfect-freehand/packages/perfect-freehand?176e00f"`
   — fast but adds a runtime dep on gitpkg's availability.

### thumbhash (multi-language repo — js/ subdir)
Same options as perfect-freehand. The npm package code lives in
`js/`. Recommended: republish to `@mscrnt/thumbhash`.

## Backend Go forks

Go's `replace` directive in `app/go.mod` handles these cleanly —
no CI complexity, the build resolves through the fork. Mappings:

```
github.com/chai2010/webp     => github.com/mscrnt/webp v1.4.0
github.com/qmuntal/gltf      => github.com/mscrnt/gltf v0.28.0
github.com/srwiley/oksvg     => github.com/mscrnt/oksvg v0.0.0-20221011165216-be6e8873101c
github.com/srwiley/rasterx   => github.com/mscrnt/rasterx v0.0.0-20220730225603-2ab79fcdd4ef
go.n16f.net/thumbhash        => github.com/mscrnt/go-thumbhash v1.1.0
```

Already in effect — `go mod tidy` resolves to the forks. To bump
to a newer fork SHA, change the version in the replace directive.

See `memory/project_dep_fork_audit.md` for the decision rules and
the future-fork list (rardecode + sevenzip when we add CBR/CB7).
