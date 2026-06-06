-- artist-alley baseline seeds migration 00002 — catalog data.
--
-- Pairs with 00001_baseline.sql. The baseline ships schema only
-- (pg_dump --schema-only). This file restores the catalog data
-- that the prior 00001..00056 migration sequence's seeding
-- INSERT statements populated:
--
--   - asset_types: 13 rows (Image, Video, Audio, 3D Object, Archive, etc.)
--   - capabilities: 35 capability codes
--   - roles: 3 (Admin, Editor, etc.) plus role_capabilities mappings
--   - workflow_states + workflow_transitions: lifecycle config
--   - field_definition: 7 seed field defs
--
-- Order chosen to respect FK dependencies (capabilities → roles →
-- role_capabilities; workflow_states → workflow_transitions). The
-- pg_dump order works because Postgres defers FK checks within a
-- transaction by default for these tables.
--
-- Pre-MVP: idempotency NOT required (baseline runs once on fresh
-- install).

-- +goose Up
INSERT INTO public.asset_types VALUES (2, 'Document', NULL, 20, NULL, NULL, NULL, 'file-text', NULL, NULL);
INSERT INTO public.asset_types VALUES (3, 'Video', NULL, 30, NULL, NULL, NULL, 'video', NULL, NULL);
INSERT INTO public.asset_types VALUES (4, 'Audio', NULL, 40, NULL, NULL, NULL, 'music', NULL, NULL);
INSERT INTO public.asset_types VALUES (5, '3D Object', NULL, 50, NULL, NULL, NULL, 'box', NULL, NULL);
INSERT INTO public.asset_types VALUES (6, 'Archive', NULL, 60, NULL, NULL, NULL, 'archive', NULL, NULL);
INSERT INTO public.asset_types VALUES (7, 'Font', NULL, 70, NULL, NULL, NULL, 'type', NULL, NULL);
INSERT INTO public.asset_types VALUES (1, 'Image', NULL, 10, NULL, NULL, NULL, 'image', NULL, NULL);
INSERT INTO public.asset_types VALUES (8, 'Comic', NULL, 80, NULL, NULL, NULL, 'book-open', NULL, NULL);
INSERT INTO public.asset_types VALUES (10, 'Ebook', NULL, 100, NULL, NULL, NULL, 'book', NULL, NULL);
INSERT INTO public.asset_types VALUES (11, 'Audiobook', NULL, 110, NULL, NULL, NULL, 'headphones', NULL, NULL);
INSERT INTO public.asset_types VALUES (12, 'Texture', NULL, 120, NULL, NULL, NULL, 'grid-3x3', NULL, NULL);
INSERT INTO public.asset_types VALUES (13, 'Sprite', NULL, 130, NULL, NULL, NULL, 'grid-2x2', NULL, NULL);
INSERT INTO public.asset_types VALUES (14, 'Code', NULL, 140, NULL, NULL, NULL, 'file-code-2', NULL, NULL);

INSERT INTO public.capabilities VALUES ('system.admin', 'Superpower — bypasses every capability check', '2026-06-06 18:04:06.825478+00', NULL);
INSERT INTO public.capabilities VALUES ('users.read', 'Read other users'' profiles and metadata', '2026-06-06 18:04:06.825478+00', NULL);
INSERT INTO public.capabilities VALUES ('users.write', 'Modify other users (role, capability grants/revokes)', '2026-06-06 18:04:06.825478+00', NULL);
INSERT INTO public.capabilities VALUES ('roles.read', 'List available roles and their capabilities', '2026-06-06 18:04:06.825478+00', NULL);
INSERT INTO public.capabilities VALUES ('caps.read', 'List defined capability codes', '2026-06-06 18:04:06.825478+00', NULL);
INSERT INTO public.capabilities VALUES ('teams.read', 'List teams and view team membership', '2026-06-06 18:04:07.512666+00', NULL);
INSERT INTO public.capabilities VALUES ('teams.create', 'Create new teams', '2026-06-06 18:04:07.512666+00', NULL);
INSERT INTO public.capabilities VALUES ('teams.admin', 'Edit any team (rename, re-parent, delete, manage members)', '2026-06-06 18:04:07.512666+00', NULL);
INSERT INTO public.capabilities VALUES ('workflow.admin', 'Manage workflow_states and workflow_transitions', '2026-06-06 18:04:07.602388+00', NULL);
INSERT INTO public.capabilities VALUES ('posts.publish', 'Move a post into the published state', '2026-06-06 18:04:07.602388+00', NULL);
INSERT INTO public.capabilities VALUES ('assets.submit', 'Submit an asset for review (draft → pending_review)', '2026-06-06 18:04:07.602388+00', NULL);
INSERT INTO public.capabilities VALUES ('assets.review', 'Approve or reject an asset in review (pending_review → published)', '2026-06-06 18:04:07.602388+00', NULL);
INSERT INTO public.capabilities VALUES ('assets.publish', 'Publish an asset directly without review (draft → published)', '2026-06-06 18:04:07.602388+00', NULL);
INSERT INTO public.capabilities VALUES ('assets.archive', 'Archive a published asset (published → archived)', '2026-06-06 18:04:07.602388+00', NULL);
INSERT INTO public.capabilities VALUES ('assets.unarchive', 'Restore an archived asset (archived → published)', '2026-06-06 18:04:07.602388+00', NULL);
INSERT INTO public.capabilities VALUES ('posts.comment', 'Write comments on posts', '2026-06-06 18:04:07.650956+00', NULL);
INSERT INTO public.capabilities VALUES ('posts.like', 'Like (and unlike) posts and comments', '2026-06-06 18:04:07.650956+00', NULL);
INSERT INTO public.capabilities VALUES ('comments.delete.own', 'Delete a comment you authored', '2026-06-06 18:04:07.650956+00', NULL);
INSERT INTO public.capabilities VALUES ('comments.delete.any', 'Delete any comment (moderator)', '2026-06-06 18:04:07.650956+00', NULL);
INSERT INTO public.capabilities VALUES ('users.profile.edit.any', 'Edit any user''s profile (moderator)', '2026-06-06 18:04:07.681466+00', NULL);
INSERT INTO public.capabilities VALUES ('system.config.read', 'View system configuration (site, SMTP, auth, AI providers).', '2026-06-06 18:04:07.700808+00', NULL);
INSERT INTO public.capabilities VALUES ('system.config.write', 'Modify system configuration.', '2026-06-06 18:04:07.700808+00', NULL);
INSERT INTO public.capabilities VALUES ('system.auth.write', 'Modify authentication / SSO configuration.', '2026-06-06 18:04:07.700808+00', NULL);
INSERT INTO public.capabilities VALUES ('system.ai.write', 'Modify AI provider configuration.', '2026-06-06 18:04:07.700808+00', NULL);
INSERT INTO public.capabilities VALUES ('system.appearance.write', 'Modify the per-install brand and typography settings.', '2026-06-06 18:04:07.703148+00', NULL);
INSERT INTO public.capabilities VALUES ('users.approve', 'Approve, suspend, or restore user accounts (lifecycle state machine)', '2026-06-06 18:04:07.809993+00', NULL);
INSERT INTO public.capabilities VALUES ('users.password.reset', 'Issue a one-shot password reset for any user (admin helpdesk action)', '2026-06-06 18:04:07.81184+00', NULL);
INSERT INTO public.capabilities VALUES ('system.asset_types.admin', 'Edit asset_type definitions and manage their per-type ACLs', '2026-06-06 18:04:07.818966+00', NULL);
INSERT INTO public.capabilities VALUES ('system.audit.read', 'Read the system-wide audit event log via the admin viewer', '2026-06-06 18:04:07.833355+00', NULL);
INSERT INTO public.capabilities VALUES ('system.sso.ldap.read', 'View LDAP/AD identity-provider configuration', '2026-06-06 18:04:07.836887+00', 'sso_ldap');
INSERT INTO public.capabilities VALUES ('system.sso.ldap.write', 'Configure LDAP/AD identity-provider connections', '2026-06-06 18:04:07.836887+00', 'sso_ldap');
INSERT INTO public.capabilities VALUES ('system.sso.saml.read', 'View SAML 2.0 IdP trust configuration', '2026-06-06 18:04:07.836887+00', 'sso_saml');
INSERT INTO public.capabilities VALUES ('system.sso.saml.write', 'Configure SAML 2.0 IdP trust + service-provider metadata', '2026-06-06 18:04:07.836887+00', 'sso_saml');
INSERT INTO public.capabilities VALUES ('system.tenancy.read', 'View multi-tenant deployment configuration', '2026-06-06 18:04:07.836887+00', 'multi_tenant');
INSERT INTO public.capabilities VALUES ('system.tenancy.write', 'Manage tenants, quotas, and per-tenant administration', '2026-06-06 18:04:07.836887+00', 'multi_tenant');

INSERT INTO public.field_definition VALUES ('7cf56f14-f68b-43ef-9b52-ca349bb836b5', 'title', 'Title', 'Primary display title for the asset.', 'text', '{}', true, true, '{}', NULL, NULL, NULL, 10, 'core', '{"tag": "ObjectName", "type": "iptc"}', 'active', NULL, NULL, '2026-06-06 18:04:07.364619+00', '2026-06-06 18:04:07.364619+00', NULL, NULL);
INSERT INTO public.field_definition VALUES ('42b3bb29-8807-4946-abc8-fdaa3d890f50', 'description', 'Description', 'Long-form description of the work.', 'longtext', '{}', false, true, '{}', NULL, NULL, NULL, 20, 'core', NULL, 'active', NULL, NULL, '2026-06-06 18:04:07.364619+00', '2026-06-06 18:04:07.364619+00', NULL, NULL);
INSERT INTO public.field_definition VALUES ('e86aa34e-820f-4a29-93cb-01d5d6a2141f', 'credit', 'Credit', 'Person or studio credited for the work.', 'text', '{}', false, true, '{}', NULL, NULL, NULL, 10, 'rights', '{"tag": "Credit", "type": "iptc"}', 'active', NULL, NULL, '2026-06-06 18:04:07.364619+00', '2026-06-06 18:04:07.364619+00', NULL, NULL);
INSERT INTO public.field_definition VALUES ('ccbdecf3-bddc-4316-928b-3e7333378cd9', 'copyright', 'Copyright', 'Copyright notice / rights statement.', 'text', '{}', false, true, '{}', NULL, NULL, NULL, 20, 'rights', '{"tag": "dc:rights", "type": "xmp"}', 'active', NULL, NULL, '2026-06-06 18:04:07.364619+00', '2026-06-06 18:04:07.364619+00', NULL, NULL);
INSERT INTO public.field_definition VALUES ('e4bc6773-025a-4277-a0b8-a0b71163134a', 'capture_date', 'Capture date', 'When the original was captured (EXIF).', 'datetime', '{}', false, true, '{}', NULL, NULL, NULL, 10, 'technical', '{"tag": "DateTimeOriginal", "type": "exif"}', 'active', NULL, NULL, '2026-06-06 18:04:07.364619+00', '2026-06-06 18:04:07.364619+00', NULL, NULL);
INSERT INTO public.field_definition VALUES ('7d8c0ca9-aff8-482f-8d20-348304aeec75', 'keywords', 'Keywords', 'Multi-value tagging.', 'multi_select', '{}', false, true, '{}', NULL, NULL, NULL, 30, 'core', '{"tag": "Keywords", "type": "iptc"}', 'active', NULL, NULL, '2026-06-06 18:04:07.364619+00', '2026-06-06 18:04:07.364619+00', NULL, NULL);
INSERT INTO public.field_definition VALUES ('da8dc72c-3b1d-4d90-b0a6-c38f53d87e46', 'country', 'Country', 'Country / region / city tree.', 'tree', '{}', false, true, '{}', NULL, NULL, NULL, 40, 'general', '{"tag": "Country-PrimaryLocationName", "type": "iptc"}', 'active', NULL, NULL, '2026-06-06 18:04:07.364619+00', '2026-06-06 18:04:07.364619+00', NULL, NULL);

INSERT INTO public.roles VALUES ('80ec6003-7fd5-4dac-9415-d26d39169d42', NULL, 'Base', 'Minimal sign-in user; can read public catalogs', NULL, '2026-06-06 18:04:06.825478+00', '2026-06-06 18:04:06.825478+00');
INSERT INTO public.roles VALUES ('aa6b632d-5bef-4924-93d4-aba070dfe503', '80ec6003-7fd5-4dac-9415-d26d39169d42', 'Admin', 'Full administrative access', NULL, '2026-06-06 18:04:06.825478+00', '2026-06-06 18:04:06.825478+00');
INSERT INTO public.roles VALUES ('a09769d4-968f-4df8-881f-d5b0822fa62d', NULL, 'Anonymous', 'Synthetic role for unauthenticated requests; caps gate which public surfaces anonymous users may read', NULL, '2026-06-06 18:04:07.649058+00', '2026-06-06 18:04:07.649058+00');

INSERT INTO public.role_capabilities VALUES ('80ec6003-7fd5-4dac-9415-d26d39169d42', 'caps.read');
INSERT INTO public.role_capabilities VALUES ('80ec6003-7fd5-4dac-9415-d26d39169d42', 'roles.read');
INSERT INTO public.role_capabilities VALUES ('aa6b632d-5bef-4924-93d4-aba070dfe503', 'users.read');
INSERT INTO public.role_capabilities VALUES ('aa6b632d-5bef-4924-93d4-aba070dfe503', 'users.write');
INSERT INTO public.role_capabilities VALUES ('aa6b632d-5bef-4924-93d4-aba070dfe503', 'system.admin');
INSERT INTO public.role_capabilities VALUES ('80ec6003-7fd5-4dac-9415-d26d39169d42', 'teams.read');
INSERT INTO public.role_capabilities VALUES ('aa6b632d-5bef-4924-93d4-aba070dfe503', 'teams.create');
INSERT INTO public.role_capabilities VALUES ('aa6b632d-5bef-4924-93d4-aba070dfe503', 'teams.admin');
INSERT INTO public.role_capabilities VALUES ('80ec6003-7fd5-4dac-9415-d26d39169d42', 'assets.submit');
INSERT INTO public.role_capabilities VALUES ('aa6b632d-5bef-4924-93d4-aba070dfe503', 'posts.publish');
INSERT INTO public.role_capabilities VALUES ('aa6b632d-5bef-4924-93d4-aba070dfe503', 'assets.review');
INSERT INTO public.role_capabilities VALUES ('aa6b632d-5bef-4924-93d4-aba070dfe503', 'assets.publish');
INSERT INTO public.role_capabilities VALUES ('aa6b632d-5bef-4924-93d4-aba070dfe503', 'assets.archive');
INSERT INTO public.role_capabilities VALUES ('aa6b632d-5bef-4924-93d4-aba070dfe503', 'assets.unarchive');
INSERT INTO public.role_capabilities VALUES ('aa6b632d-5bef-4924-93d4-aba070dfe503', 'workflow.admin');
INSERT INTO public.role_capabilities VALUES ('80ec6003-7fd5-4dac-9415-d26d39169d42', 'posts.comment');
INSERT INTO public.role_capabilities VALUES ('80ec6003-7fd5-4dac-9415-d26d39169d42', 'posts.like');
INSERT INTO public.role_capabilities VALUES ('80ec6003-7fd5-4dac-9415-d26d39169d42', 'comments.delete.own');
INSERT INTO public.role_capabilities VALUES ('aa6b632d-5bef-4924-93d4-aba070dfe503', 'comments.delete.any');
INSERT INTO public.role_capabilities VALUES ('aa6b632d-5bef-4924-93d4-aba070dfe503', 'users.profile.edit.any');
INSERT INTO public.role_capabilities VALUES ('aa6b632d-5bef-4924-93d4-aba070dfe503', 'system.config.read');
INSERT INTO public.role_capabilities VALUES ('aa6b632d-5bef-4924-93d4-aba070dfe503', 'system.config.write');
INSERT INTO public.role_capabilities VALUES ('aa6b632d-5bef-4924-93d4-aba070dfe503', 'system.auth.write');
INSERT INTO public.role_capabilities VALUES ('aa6b632d-5bef-4924-93d4-aba070dfe503', 'system.ai.write');
INSERT INTO public.role_capabilities VALUES ('aa6b632d-5bef-4924-93d4-aba070dfe503', 'system.appearance.write');

INSERT INTO public.workflow_states VALUES ('48a7ec39-9ab8-463e-984a-9f0c3037fee1', 'post', 'published', 'Published', 0, true, false, true, '2026-06-06 18:04:07.602388+00', 'check-circle', '#16a34a', false);
INSERT INTO public.workflow_states VALUES ('a4fb6ed4-16f0-405b-a172-f0049a07feda', 'asset:1', 'draft', 'Draft', 0, true, false, false, '2026-06-06 18:04:07.602388+00', 'file-edit', '#64748b', false);
INSERT INTO public.workflow_states VALUES ('2c68c34f-fdbf-4080-a958-e8418b4e4def', 'asset:1', 'pending_review', 'Pending Review', 1, false, false, false, '2026-06-06 18:04:07.602388+00', 'clock', '#f59e0b', false);
INSERT INTO public.workflow_states VALUES ('daf8045b-0b32-49c9-87eb-e7ff72db206c', 'asset:1', 'published', 'Published', 2, false, false, true, '2026-06-06 18:04:07.602388+00', 'check-circle', '#16a34a', false);
INSERT INTO public.workflow_states VALUES ('def32f01-1912-4d43-8c51-95f527d163dd', 'asset:1', 'archived', 'Archived', 3, false, false, false, '2026-06-06 18:04:07.602388+00', 'archive', '#0ea5e9', false);
INSERT INTO public.workflow_states VALUES ('0489ffd2-9ec4-454f-9604-02f8c4390a7b', 'asset:1', 'deleted', 'Deleted', 4, false, true, false, '2026-06-06 18:04:07.602388+00', 'trash-2', '#ef4444', false);
INSERT INTO public.workflow_states VALUES ('3c318b8b-572c-4ed8-a87f-6f531ce42028', 'post', 'wip', 'WIP', -10, false, false, true, '2026-06-06 18:04:07.691197+00', 'pencil-line', '#f59e0b', false);

INSERT INTO public.workflow_transitions VALUES ('6af6c8d9-8d19-4976-b060-3121153f874c', 'a4fb6ed4-16f0-405b-a172-f0049a07feda', '2c68c34f-fdbf-4080-a958-e8418b4e4def', 'assets.submit', false);
INSERT INTO public.workflow_transitions VALUES ('036187b0-0065-4834-93d1-c5cc6a7b19c1', '2c68c34f-fdbf-4080-a958-e8418b4e4def', 'daf8045b-0b32-49c9-87eb-e7ff72db206c', 'assets.review', true);
INSERT INTO public.workflow_transitions VALUES ('f8c0b52b-ef0c-406d-be49-bcba2935bd58', '2c68c34f-fdbf-4080-a958-e8418b4e4def', 'a4fb6ed4-16f0-405b-a172-f0049a07feda', 'assets.review', true);
INSERT INTO public.workflow_transitions VALUES ('c5411fe5-bf35-487e-9c71-761ebb418e58', 'a4fb6ed4-16f0-405b-a172-f0049a07feda', 'daf8045b-0b32-49c9-87eb-e7ff72db206c', 'assets.publish', true);
INSERT INTO public.workflow_transitions VALUES ('6cd17ebe-c92b-4c1c-be81-d2080944985d', 'daf8045b-0b32-49c9-87eb-e7ff72db206c', 'def32f01-1912-4d43-8c51-95f527d163dd', 'assets.archive', true);
INSERT INTO public.workflow_transitions VALUES ('d6310dbd-8a3e-4289-9726-ea2e477f3fb6', 'def32f01-1912-4d43-8c51-95f527d163dd', 'daf8045b-0b32-49c9-87eb-e7ff72db206c', 'assets.unarchive', true);
INSERT INTO public.workflow_transitions VALUES ('db548e79-b233-4396-82bf-bdc4302f1112', NULL, 'a4fb6ed4-16f0-405b-a172-f0049a07feda', NULL, false);
INSERT INTO public.workflow_transitions VALUES ('13681f73-8ff7-4d69-a805-084365305467', NULL, '48a7ec39-9ab8-463e-984a-9f0c3037fee1', NULL, false);
INSERT INTO public.workflow_transitions VALUES ('a9d5c140-ba58-4bb9-9643-3b5880a93c41', NULL, '3c318b8b-572c-4ed8-a87f-6f531ce42028', NULL, false);
INSERT INTO public.workflow_transitions VALUES ('8b398b4a-4a35-49f8-922a-370910176ef3', '3c318b8b-572c-4ed8-a87f-6f531ce42028', '48a7ec39-9ab8-463e-984a-9f0c3037fee1', 'posts.publish', false);
INSERT INTO public.workflow_transitions VALUES ('23306b7c-c570-4d50-9c22-875f778111b2', '48a7ec39-9ab8-463e-984a-9f0c3037fee1', '3c318b8b-572c-4ed8-a87f-6f531ce42028', 'posts.publish', false);

--
-- Name: resource_type_ref_seq; Type: SEQUENCE SET; Schema: public; Owner: -
--

-- +goose Down
DELETE FROM public.role_capabilities;
DELETE FROM public.workflow_transitions;
DELETE FROM public.field_definition;
DELETE FROM public.workflow_states;
DELETE FROM public.roles;
DELETE FROM public.capabilities;
DELETE FROM public.asset_types;
