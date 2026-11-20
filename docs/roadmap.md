# Roadmap

Artist Alley is growing from a place to keep and review studio art into a place where teams can work through a review together, even across studios. This page shows the direction. It is not a delivery calendar: the order and scope may change as people use the app.

## What works today

You can upload and organize artwork, make posts and collections, browse and search what you are allowed to see, and leave threaded comments where the work lives. Drafts, scheduled posts, studio-defined metadata, and bulk metadata edits help teams keep that work legible. The viewer handles images, video, audio, documents, sprite sheets, and 3D models. Operators can run their own instance and choose local or S3-compatible storage.

Two Artist Alley instances can pair through ArchivePub and exchange likes and comments. Wider cross-studio sharing is still ahead of us. Review is asynchronous today; shared live sessions and frame-scoped feedback are planned.

The current tagged release is [v0.11.1](https://github.com/Artist-Alley-Org/artist-alley/releases/tag/v0.11.1). It made the project's checks easier to reproduce from public sample libraries. For the story of a release, use the [release notes](https://github.com/Artist-Alley-Org/artist-alley/releases); this roadmap does not repeat the full version history.

## Up next

### Federation between studios

The next release, [v0.12.0](https://github.com/Artist-Alley-Org/artist-alley/milestone/75), makes federation between two Artist Alley studios work end to end. Pairing still shares nothing by itself: a studio decides what to share and with whom, and can take it back. The plan covers trusted pairing with explicit grants, likes and comments that work in both directions, and shared assets that arrive intact with their versions and the records that describe them: checksums, formats, rights, and preservation history. Those details matter when art moves between systems or needs to be understood years later. Operators also get moderation for what arrives, and the release is tested with two instances on different versions before it ships. Anything a partner studio can't apply is reported back rather than silently dropped.

### Finding and maintaining an instance

v0.12.0 also brings in four smaller pieces. An optional update notice ([#1518](https://github.com/Artist-Alley-Org/artist-alley/issues/1518)) tells operators when a newer release is out, and a separate optional install count ([#1519](https://github.com/Artist-Alley-Org/artist-alley/issues/1519)) adds an instance to public totals. They are two distinct settings, and a fresh install sends neither request until an operator turns one on. An official directory ([#1520](https://github.com/Artist-Alley-Org/artist-alley/issues/1520), [#74](https://github.com/Artist-Alley-Org/artist-alley/issues/74)) lists only instances whose operators choose to be listed; a listing helps studios find each other, but it isn't pairing and it isn't trust. The project's code and the libraries it carries its own copies of move into the Artist-Alley-Org organization ([#865](https://github.com/Artist-Alley-Org/artist-alley/issues/865)), with no change to how the app works.

### Reviewing together

The next large product arc after that, [v0.13.0](https://github.com/Artist-Alley-Org/artist-alley/milestone/69), is shared review: presenter sessions, a collaborative canvas, and closer inspection of creative work. The goal is to make feedback feel attached to the image, frame, or moment that prompted it, while keeping the review useful later.

## Further out

The longer path includes articles and 3D animation, safer ways to share work outside a studio, community and moderation tools, stronger privacy and audit controls, optional creative tools, and easier ways to install and run the app. Later federation work, such as live sessions across studios, automatic sync, and discovery by the wider fediverse, will connect more independent libraries without making one studio give up control of its own.

These are themes, not a promise that every item lands in one release. The [open milestones](https://github.com/Artist-Alley-Org/artist-alley/milestones) and [issue tracker](https://github.com/Artist-Alley-Org/artist-alley/issues) hold the current work. Older, phase-numbered [planning notes](roadmap-planning-notes.md) are kept for contributors who need the design trail; they may not describe today's priorities.

## Boundaries

Artist Alley is for studio art review and archiving. It is not trying to replace a production tracker or become a general-purpose asset system. Self-hosting stays central even as more ways to install or operate it are explored.
