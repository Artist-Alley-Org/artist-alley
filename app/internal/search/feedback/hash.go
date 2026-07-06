package feedback

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// CanonicalizeDSL returns a canonical string form of a DSL query
// suitable for aggregation-key hashing. Same-semantic variants
// collapse via three normalizations:
//
//  1. Leading/trailing whitespace stripped.
//  2. Interior runs of whitespace collapsed to a single space
//     (strings.Fields does both 1 + 2).
//  3. Lowercased.
//
// **This is not full AST canonicalization.** Semantically-equivalent
// queries with different operator ordering (`cat AND dog` vs
// `dog AND cat`) still hash to distinct keys. Full canonicalization
// would require parsing the DSL + sorting AND/OR child lists; that's
// a larger refactor deferred to a future arc. For the MVP feedback
// aggregation, "same string modulo whitespace + case" is enough
// resolution — admins can spot ranking issues at the query-family
// level even if a few permutations don't collapse.
func CanonicalizeDSL(dsl string) string {
	return strings.ToLower(strings.Join(strings.Fields(dsl), " "))
}

// HashDSL returns the SHA-256 hex digest of the canonicalized DSL.
// Used as the aggregation key in the search_feedback table + the
// (user_ref, hit_asset_id, query_hash) unique constraint.
//
// 64-char hex output. Not truncated — the DB column is TEXT so
// storage is cheap and the extra collision resistance is trivial.
func HashDSL(dsl string) string {
	sum := sha256.Sum256([]byte(CanonicalizeDSL(dsl)))
	return hex.EncodeToString(sum[:])
}
