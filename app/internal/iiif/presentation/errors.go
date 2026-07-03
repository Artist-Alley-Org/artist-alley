package presentation

import "errors"

// Sentinels. HTTP layer maps to status codes:
//
//   - ErrRestricted → 404 (per brief decision 7 — 403 leaks
//     existence; spec-legal alternative is 404)
//   - ErrNotFound → 404
//   - ErrEmbargoed is NOT an error — the builder returns a stub
//     manifest instead. Kept as a variable for consistency with
//     downstream error-handling code that wants to check.
var (
	ErrRestricted = errors.New("presentation: entity restricted from caller")
	ErrNotFound   = errors.New("presentation: entity not found")
	ErrEmbargoed  = errors.New("presentation: entity embargoed")
)
