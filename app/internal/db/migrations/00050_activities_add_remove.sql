-- artist-alley migration 00050 — extend activities.activity_type
-- CHECK to include standard AS2 Add + Remove per AP §6.6 / §6.7.
-- Phase 1.22.A-bis-4, feat/user-surfaces.
--
-- These were omitted from migration 00049 because the initial
-- handler-wiring sweep (1.22.A-bis-2) didn't touch collection
-- membership endpoints. 1.22.A-bis-4 wires
-- collections.AddCollectionResource + RemoveCollectionResource
-- (which need Add + Remove respectively) so the catalogue now
-- needs the constants.
--
-- Per ADR 0042 §3 the typed Go constants + DB CHECK move together;
-- federation.ActivityType (vocab.go) gains ActivityAdd +
-- ActivityRemove in the same PR.

-- +goose Up

ALTER TABLE activities DROP CONSTRAINT activities_activity_type_check;

ALTER TABLE activities ADD CONSTRAINT activities_activity_type_check
    CHECK (activity_type IN (
        -- Standard AS2 activities (per W3C AS2 Vocabulary).
        'Create', 'Update', 'Delete',
        'Follow', 'Accept', 'Reject',
        'Undo', 'Like', 'Announce', 'Block',
        'Add', 'Remove',
        -- Custom artist-alley activities (per ADR 0043).
        'aa:Share', 'aa:Unshare',
        'aa:Approve', 'aa:RequestChanges', 'aa:MarkReviewed',
        'aa:Annotation', 'aa:WorkflowTransition', 'aa:AssetVersion',
        'aa:Subscribe', 'aa:Mention'
    ));

-- +goose Down

ALTER TABLE activities DROP CONSTRAINT activities_activity_type_check;

ALTER TABLE activities ADD CONSTRAINT activities_activity_type_check
    CHECK (activity_type IN (
        'Create', 'Update', 'Delete',
        'Follow', 'Accept', 'Reject',
        'Undo', 'Like', 'Announce', 'Block',
        'aa:Share', 'aa:Unshare',
        'aa:Approve', 'aa:RequestChanges', 'aa:MarkReviewed',
        'aa:Annotation', 'aa:WorkflowTransition', 'aa:AssetVersion',
        'aa:Subscribe', 'aa:Mention'
    ));
