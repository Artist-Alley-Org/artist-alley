// Package strictservershim provides PanicShim, a generated stub
// implementation of openapi.StrictServerInterface where every
// method panics. Tests embed *PanicShim and override only the
// methods exercised by their fixture, eliminating the
// historical "7 panic stubs in 7 files per new openapi method"
// boilerplate (~1,200 lines of pure stub code).
//
// Usage:
//
//	type myShim struct {
//	    *strictservershim.PanicShim
//	    h *mydomain.Handler
//	}
//
//	// Only override what this test exercises:
//	func (s *myShim) ListThings(ctx context.Context, req openapi.ListThingsRequestObject) (openapi.ListThingsResponseObject, error) {
//	    return s.h.ListThings(ctx, req)
//	}
//
// Go's method resolution promotes embedded methods, so the
// shim satisfies StrictServerInterface without enumerating every
// untouched operation. Calls into untouched ops panic with a
// clear "PanicShim: X called without override in test fixture"
// message so tests that accidentally route through an
// unstubbed method fail loudly instead of silently no-oping.
//
// Regenerate with `./scripts/generate.sh` after every
// openapi.yaml change — the generator script reads
// app/internal/openapi/openapi.gen.go to enumerate the
// interface methods, so it stays in sync automatically.
package strictservershim
