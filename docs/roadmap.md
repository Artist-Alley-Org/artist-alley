# Roadmap

Artist Alley is growing from a place to keep and review studio art into a place where teams can work through a review together, even across studios. This page shows the direction. It is not a delivery calendar: the order and scope may change as people use the app.

## What works today

You can upload and organize artwork, make posts and collections, browse and search what you are allowed to see, and leave threaded comments where the work lives. Drafts, scheduled posts, studio-defined metadata, and bulk metadata edits help teams keep that work legible. The viewer handles images, video, audio, documents, sprite sheets, and 3D models. Operators can run their own instance and choose local or S3-compatible storage.

Two Artist Alley instances can pair through ArchivePub and exchange likes and comments. Wider cross-studio sharing is still ahead of us. Review is asynchronous today; shared live sessions and frame-scoped feedback are planned.

The current tagged release is [v0.11.1](https://github.com/Artist-Alley-Org/artist-alley/releases/tag/v0.11.1). It made the project's checks easier to reproduce from public sample libraries. For the story of a release, use the [release notes](https://github.com/Artist-Alley-Org/artist-alley/releases); this roadmap does not repeat the full version history.

## Up next

### Records that travel well

The next release scope, [v0.11.2](https://github.com/Artist-Alley-Org/artist-alley/milestone/75), focuses on ArchivePub and the records that describe an asset: its checksums, formats, rights, and preservation history. Those details matter when art moves between systems or needs to be understood years later.

### Finding and maintaining an instance

We plan an optional way for operators to hear about new releases and a directory that instances can choose to join. Neither should require a fresh install to phone home. The [milestones](https://github.com/Artist-Alley-Org/artist-alley/milestones) show the exact scope as these pieces are planned.

### Reviewing together

The next large product arc, [v0.12.0](https://github.com/Artist-Alley-Org/artist-alley/milestone/69), is shared review: presenter sessions, a collaborative canvas, and closer inspection of creative work. The goal is to make feedback feel attached to the image, frame, or moment that prompted it, while keeping the review useful later.

## Further out

The longer path includes articles and 3D animation, safer ways to share work outside a studio, community and moderation tools, stronger privacy and audit controls, optional creative tools, and easier ways to install and run the app. More of ArchivePub will connect independent libraries without making one studio give up control of its own.

These are themes, not a promise that every item lands in one release. The [open milestones](https://github.com/Artist-Alley-Org/artist-alley/milestones) and [issue tracker](https://github.com/Artist-Alley-Org/artist-alley/issues) hold the current work. Older, phase-numbered [planning notes](roadmap-planning-notes.md) are kept for contributors who need the design trail; they may not describe today's priorities.

## Boundaries

Artist Alley is for studio art review and archiving. It is not trying to replace a production tracker or become a general-purpose asset system. Self-hosting stays central even as more ways to install or operate it are explored.
