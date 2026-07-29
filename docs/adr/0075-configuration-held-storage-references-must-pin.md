---
id: "0075"
title: A configuration that references a storage object must pin it for as long as it references it
status: accepted
date: 2026-07-28
area: storage
phases: []
supersedes: []
related:
  - "0008"
  - "0017"
tags:
  - storage
  - retention
  - sysconfig
  - garbage-collection
excerpt: >-
  Pins have always been entity-owned — an asset, a companion, a job output.
  The instance logo introduced the first configuration-owned reference, and
  content-addressing does not retain: `RemovePin` marks an object GC-eligible
  the moment its last pin drops. Any config holding a reference must hold a
  pin, with a bounded count, or the reference rots the first time the sweeper
  runs.
---

# A configuration that references a storage object must pin it for as long as it references it

## Context

Storage retention works on **pins**. A pin is a `(SubjectType, SubjectID, object_hash)`
triple; `RemovePin` deletes one and then calls `MarkGCEligibleIfOrphaned`, which
stamps `gc_eligible_at` only if no pins remain. A fresh upload of the same bytes
clears any pending `gc_eligible_at`.

Every pinner up to now has been **entity-owned**: an asset, an alternate, a
companion, a transcription output, an AI-edit result, a subtitle track, seeded
fixtures. In each case the thing holding the reference is a row that the storage
layer can reason about, and its lifecycle is the object's lifecycle.

The instance logo (#517) introduced something new: a reference held by
**configuration**, not by an entity. `system_config` stores the hash of the
current logo plus a most-recently-used history of previous ones, so an operator
can switch back if an image is lost.

Two facts made this a decision rather than an implementation detail.

**Content-addressing does not retain.** It deduplicates. If the config is the
only thing referring to a blob, nothing else keeps it alive, and the moment its
last pin drops it becomes GC-eligible. A "remembered" logo would rot into
precisely the broken thumbnail the history feature exists to prevent.

**The sweeper is not implemented yet.** `service.go` says so in as many words:
the sweeper "actually deletes the bytes after the grace window expires" and does
not exist. So today a config-held reference *appears* to work — nothing is
collecting anything. The failure is latent, and it lands in a single batch the
first time the sweeper ships. That is the worst possible discovery moment, and
it is why this rule is being written now rather than when it next bites.

## Decision

**Any configuration that stores a reference to a storage object MUST hold a pin
for that object for exactly as long as it holds the reference.**

Concretely:

1. **Acquire on reference.** Writing a hash into config takes a pin, using a
   `PinRef` whose subject identifies the configuration, not an entity.
2. **Release on dereference.** Removing the hash — overwrite, eviction from a
   history list, reset to default — releases that pin. Normal GC then applies:
   if another pin remains the object lives, otherwise it becomes eligible.
3. **Bound the count.** A configuration holding N references pins N blobs, and
   an unbounded history is an unbounded retention leak. Every such list carries
   an explicit cap. The logo history caps at five.

The invariant this buys is: **listed implies resolvable.** The storage layer
maintains it, so no consuming surface has to defensively re-check whether the
thing it was told about still exists.

## Pinning is necessary but not sufficient

Pins protect against *our own* garbage collection. They do not protect against
loss outside the pin system, and the motivating requirement was explicitly
"in case an image was lost":

- a database restored against a fresh or different bucket
- an object removed directly from the backend
- backend migration that misses objects nothing else references

So a configuration surface that **displays** its references must also degrade
honestly. An entry whose object cannot be resolved is shown as unavailable and
disabled — never rendered as an image element pointing at a URL known to 404.
The logo admin card probes each history entry and greys the dead ones.

The two mechanisms cover different failures and neither substitutes for the
other: pinning stops *us* deleting it, probing copes with *something else*
having deleted it.

## A configuration reference is not a capability to read arbitrary bytes

Storing a hash in config means an operator can point a **public, unauthenticated**
route at whatever that hash names. The logo is served at `/appearance/logo` with
`security: []`.

So the write path must constrain which hashes may be referenced. Selection
resolves only against objects the configuration itself uploaded and still lists;
selecting an arbitrary hash returns 404. The reference fields are `readOnly` on
the general config PATCH, so the only way a hash enters config is through the
dedicated upload path.

Without this, an admin could aim a public route at any object on the install,
including another user's private asset — a privilege escalation dressed as a
settings change. **Any future config-held reference inherits this requirement.**

## Consequences

- `sysconfig` becomes a pin subject alongside the entity pinners, and the storage
  layer's notion of "who holds this" widens beyond rows-that-own-bytes.
- Retention is bounded and predictable per configuration: five blobs for the logo.
- The rule is in place **before** the sweeper ships, so the first sweep cannot
  silently invalidate config-held references.
- Every future feature of this shape — a custom favicon, a per-team brand mark,
  an operator-supplied email header image — has a rule to follow rather than a
  precedent to guess at.
- Cost: a config write now performs a storage write, so config mutation can fail
  for storage reasons. Callers must handle that rather than assuming config
  writes are cheap and local.

## References

- ADR 0008 — storage architecture (content-addressed, pluggable backends)
- ADR 0017 — license-derived limits, the existing precedent for bounded resources
- #517 — instance logo, the first configuration-held storage reference
