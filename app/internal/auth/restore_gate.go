// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package auth

// CanRestoreDeleted decides who may undo a soft delete, for any of the
// three soft-deletable entities. The rule is deliberately about the
// DELETER, not about the caller's standing authority (#931):
//
//	"users should be able to recover their own deleted files, unless
//	 deleted by an admin. Then they would need to request for
//	 restoration."
//
// So: you may undo your own delete, and system.admin may undo any.
// Nothing else. That single rule satisfies both halves at once —
//
//   - Restore authority matches delete authority. Whoever could delete
//     it can undo it, because the deleter is by construction someone
//     who passed the delete gate. Before this, delete was open to every
//     authenticated user and restore was system.admin only, which is
//     the asymmetry #931 objects to; conditioning restore on the
//     caller's authority INSTEAD of on the deleter would just move that
//     asymmetry one level up.
//
//   - An admin's delete is not silently reversible by the owner. If it
//     were "owner OR deleter", an owner could undo a moderation action
//     the instant it landed. Asking for restoration is the intended
//     path there; the request flow itself is #931's other half and is
//     not built yet.
//
// deletedBy is nil for a row deleted before migration 00037 and for a
// system-scheduled retention delete (scheduled_actions.created_by is
// nullable). Both mean "we do not know who did this", and both fail
// closed to system.admin.
//
// WHY IT LIVES IN `auth` (#665). It began as assets.canRestoreDeleted
// with two hand-copies — collections.canRestoreCollection and
// posts.canRestorePost — each carrying a comment saying "mirrors
// assets.canRestoreDeleted exactly". Three copies of one rule is three
// places to forget, and #937 added a FOURTH consumer: GET
// /account/trash renders `restorable_by_caller` per row and must agree
// with what the restore endpoints will actually do. A boolean the
// listing computes independently is a listing that can lie — offer a
// Restore button that 403s, or hide one that would have worked. So the
// rule has exactly one home, and every consumer obtains it rather than
// restating it. `auth` is that home because the rule's whole input is
// an *Identity plus a user ref, and every domain package already
// imports auth (no domain can import another without a cycle).
func CanRestoreDeleted(id *Identity, deletedBy *int64) bool {
	if id == nil || id.IsAnonymous() {
		return false
	}
	if id.Can(SuperAdminCapability) {
		return true
	}
	return deletedBy != nil && *deletedBy != 0 && id.UserRef != 0 && *deletedBy == id.UserRef
}
