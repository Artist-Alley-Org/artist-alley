---
id: "0081"
title: Operators override shipped strings, field defaults, and email templates — the last over a restricted context
status: accepted
date: 2026-07-30
area: architecture
phases: []
supersedes: []
related:
  - "0012"
  - "0013"
  - "0024"
  - "0077"
tags:
  - admin
  - metadata
  - security
  - i18n
excerpt: >-
  Three locked admin tiles — site text, email templates and upload defaults —
  are the same question wearing three hats: what may an operator change about
  content that ships inside the binary? All three become operator-editable.
  Templates were rejected outright at first and reinstated by the 2026-07-31
  amendment: the hazard is Go templates invoking methods, which is fixed by
  rendering against a flat typed view-model rather than by prohibition.
---

## Context

Epic #519 tracks six locked `/admin/content/*` tiles. Three of them —
`site_text`, `email_templates`, `defaults` — carried `adr-needed` because none has a table,
an endpoint, or a prior decision. A full-text sweep of the ADR corpus found nothing covering
any of them.

They look like three features. They are one question asked three times: **what may an operator
change about content that ships inside the binary, and by what mechanism?**

Current state, verified:

- **Strings** live in `web/src/lib/i18n/en.json`, pulled in by a static
  `import enDict from './en.json'` — bundled at build time, with no runtime layer.
- **Email templates** are 18 files (6 events × subject/txt/html) under
  `app/internal/email/templates/`, loaded from an `embed.FS` at package `init()` into a
  package-level `registry` map.
- **Defaults** do not exist in any form. There is no table and no column.
- `system_config` already holds `site` → `{"name": …, "base_url": …}`, so a small amount of
  operator-editable copy has precedent.

### What the prior art shows, including by omission

A mature DAM in this space carries a `site_text` table — `page`, `name`, `text`, `language`,
`specific_to_group`, `custom`. Two things are informative. It is keyed by *page and name*
because it has no global string-key namespace; and it carries a per-user-group axis, which
means someone genuinely wanted site copy to vary by audience.

**It has no email-template table at all.** Twenty years of a product built for exactly this
operator persona, and operator-authored mail bodies were never built. Absence in a mature
product is weak evidence, but it is evidence, and it points the same way as the security
analysis below.

**It has no `default_value` column either.** Its field-definition table instead carries
`autocomplete_macro` and `onchange_macro` — **executable code stored as text in a
configuration column**. That is the mechanism we are choosing against, not the one we are
copying.

## Decision

### 1. Site text — yes, as an override layer keyed by the existing string key

Operators may override any shipped UI string. The override is **data**, stored per
`(key, language)`, resolved at read time over the shipped dictionary, and cached
(ADR 0013's `cache.Registry` pattern, invalidated on write).

**We key by the i18n key, not by page and name.** Our strings already have a global dotted
namespace (`admin.collection_fields.section_title`); that key *is* the identity. The prior-art
composite key exists to compensate for not having one. This is a place where our existing model
is simply better and we should not import the workaround.

**`language` is part of the key.** Translation is a live epic (#289); an override layer that
is locale-blind would silently apply an English override to every locale.

**Per-group site text is deliberately not built.** It is a real thing someone once wanted, and
it is also a combinatorial maintenance surface for a need no operator here has expressed. A
column is cheap to add later and expensive to remove once written against.

**An override that names a key that does not exist must fail loudly.** This is not a
hypothetical: the entire `collection_fields.*` namespace resolves to nothing today (#774) and
renders raw key names to operators, because a missing key silently returns its own name.
Layering operator overrides on top of a resolver that fails silently would let an operator
"override" a string and see no effect, with nothing to tell them why. **Fixing that resolution
gap is a prerequisite, not a follow-up.**

**Site text does not federate.** It is instance identity — how *this* installation speaks. A
peer receiving content must not receive the operator's wording.

### 2. Email templates — no. Operators configure the variables, never the template

> ⚠️ **SUPERSEDED by the 2026-07-31 amendment at the end of this document.** Templates are
> reinstated over a restricted context. The analysis below is kept because its third leg — Go
> templates *invoking methods* — is what the replacement design is built around; the first two
> legs were overstated. Do not implement from this section.

**Operator-authored email templates are not built, and this is a decision rather than a
deferral.**

The reason is not escaping. Go's `html/template` contextually auto-escapes, so the naive XSS
case is already handled. The reason is *reach*: a template is evaluated against a data map, and
an operator-authored template can walk that map. Email is the one channel that leaves the
instance and arrives somewhere we do not control. A configuration surface that can render
arbitrary fields of the data passed to a notification is an exfiltration primitive wearing the
costume of a settings page — and the operator role that would edit templates is not
automatically the role that should read every value a notification is built from. ADR 0077
established that a capability to *write* configuration is not a capability to *read* whatever
happens to sit next to it; the same reasoning applies here.

**What the operator actually wants is branding and wording, not control flow.** So we expose
those directly: instance name and logo, colours, a per-event custom message block, and footer
and signature text — all typed configuration fields, all escaped on render, none of them
capable of introducing a template action.

If a genuine need for structural template editing ever appears, it arrives as its own ADR with
a sandbox story, an explicit capability, and an audit trail. It does not arrive as a text area.

### 3. Upload defaults — yes, declarative only, never an expression language

> ⚠️ **The precedence paragraph below was wrong, and the "target collection" context value does
> not exist. Both are corrected by the 2026-07-31 defaults amendment at the end of this
> document.** Everything else in this section shipped as written.

A field may carry a default that is applied when an asset is created, and a team may override
that default for its own uploads. **This is the highest-value of the three tiles**, because it
is the wedge: every default correctly applied is a decision an artist does not have to make at
upload time.

A default is one of exactly two things:

1. **A literal value** valid for the field's type — and for `select` / `multi_select`, a slug
   that exists in that field's `options` and is not `deprecated` or `archived` (ADR 0012's
   amendment). Defaulting to a retired term would quietly spread it.
2. **A named context value** from a small closed set the server resolves — the uploading user,
   the current date, the target team or collection. A closed enumeration, not an expression.

**There is no macro column, and no expression language.** Storing executable code in a
configuration column is the single clearest thing to avoid here: it is a code-injection surface,
it cannot be validated on write, it makes the field definition unportable across a federation
boundary, and it turns every future change to the host language into a migration of user data.

**When a value genuinely must be computed, the extraction pipeline already exists.**
`field_definition` carries `extraction_source` and `extraction_mode`; deriving a value from the
file is a solved problem with a home. Defaults answer "what should this be when nothing else
says," not "what can be computed."

~~**Precedence is fixed and shallow**, so a value's origin is always explainable:
extracted > team default > field default > empty. A default never overwrites a value that is
already set, and never overwrites an extracted one.~~ **Superseded — see the amendment.**

**Defaults federate with their field**, because they are part of the field definition. A team
override does not: teams are local.

## Consequences

- `#519` can be decomposed. `site_text` and `defaults` become buildable tiles; the
  `email_templates` tile is **closed as decided-against** and replaced by a narrower
  "email branding and wording" tile.
- **#774 is promoted to a prerequisite for `site_text`.** An override layer over a silently
  failing resolver is worse than no override layer, because the failure mode moves from
  "developer sees a raw key" to "operator's change does nothing and nothing says why."
- The `defaults` tile needs `field_definition` to carry a default, and a team-scoped override
  table. Neither exists; this is the largest of the three.
- ~~Operators wanting genuinely custom mail will be told no.~~ **Withdrawn 2026-07-31** — see the
  amendment. The branding fields remain the common case and stay worth having.
- Three surfaces now resolve shipped-content-plus-override at read time (strings, field
  defaults, email branding). Each must cache and invalidate on write; none may resolve per row
  in a loop.
- **We diverge from the prior art on all three.** Keyed by string key rather than page/name;
  no operator templates where they have none either; declarative defaults where they use
  executable macros. Only the first is a case of us being better by construction — the other
  two are cases of choosing a smaller surface deliberately.

## Amendment 2026-07-31 — email templates are reinstated, over a restricted context

**The decision above rejected operator-authored email templates outright. That was
overstated, and the owner was right to push on it.** This amendment replaces the rejection
with a narrower design.

### What the original argument got wrong

Two of the three legs were weaker than the text implied.

- **"An operator-authored template can walk the data map."** True, but *we control what is in
  the map*. The reach objection is a property of passing raw notification internals to the
  template, not a property of letting operators write templates. Change the context and the
  objection largely dissolves.
- **"A capability to write is not a capability to read what sits beside it."** The ADR 0077
  parallel is real but weaker here: the operator role that edits mail bodies is already highly
  privileged and can very likely read the database directly. The marginal escalation is small,
  and the original text presented it as decisive.

The absence of an equivalent feature in a mature competitor was cited as supporting evidence.
It is weak evidence about demand and none at all about safety, and it was allowed to carry more
weight than it should.

### What the original argument got right, and under-weighted

**Go templates invoke methods.** `{{.Thing.Method}}` does not read a field, it *executes*. That
is a materially different hazard from reading data, and the original text buried it under the
exfiltration framing. It is also the leg that survives scrutiny — and it points at the fix
rather than at a prohibition.

### Decision (supersedes the rejection above)

**Operators may author email templates, and templates render against a restricted context.**

- The context is a **flat, typed view-model of strings and simple scalars** assembled per
  event — never a domain object, never a struct carrying methods, never the raw notification
  payload. `{{.Thing.Method}}` has nothing to reach because nothing in scope has methods.
- The available fields per event are **documented and finite**, and the editing surface shows
  them. An operator writing a template should not have to guess what is in scope.
- A reference to something absent fails **visibly at edit time**, not silently at send time.
  This is the same rule #774 established for strings: a lookup that quietly resolves to nothing
  is worse than one that fails loudly.
- Output stays contextually escaped. That was never in question.

### Consequences

- The `email_templates` tile is **reinstated** and is real work, not a rename. It needs the
  per-event view-model defined and documented before any editing surface is built — that
  definition *is* the security boundary.
- The email-branding fields from the original decision remain worth having; they are the common
  case and should not require writing a template to change a logo.
- **The view-model is now a compatibility surface.** Renaming a field in it breaks operator
  templates silently, so it needs the same lifecycle care as a public API — which is an argument
  for keeping it small.
- The consequence above reading *"operators wanting genuinely custom mail will be told no"* is
  **withdrawn**.

## Amendment 2026-07-31 — the defaults precedence chain, corrected against the mechanism (#793)

Section 3 stated a precedence the existing machinery delivers **backwards**, and named a context
value the creation path cannot see. Both are corrected here. The rest of §3 — declarative only,
two shapes, no macro column, active terms only, federate with the field but not the override —
shipped exactly as written.

### What §3 got wrong, and why it mattered

§3 said: `extracted > team default > field default > empty`, and *"a default never overwrites a
value that is already set, and never overwrites an extracted one."*

The second half was already true and stayed true. The first half was not implementable as
written, because **the extraction applier's `skip_if_set` rule tests presence, not provenance**:

```go
// app/internal/asset/metadata/apply.go, before #793
if fc.Mode == ExtractionModeSkipIfSet && present { skip }
```

Write a default at asset creation and the value is *present*. Extraction then skips it. So the
default would have outranked the extraction it is supposed to yield to — and **thirteen of the
fifteen live field definitions are `skip_if_set`**, so that inversion would have been the normal
case rather than a corner. ADR 0012 always specified this skip in terms of provenance ("skip if
`set_by = 'manual'`"); what shipped read only "is a value there", and nothing noticed because
until now every value on an asset had been put there by a person or by an extractor.

Two related documents also need reading with this in mind. Migration `00020` had already
recorded the same gap in passing — *"the applier's only mode check is 'is a value present',
never `set_by`"* — as a contributing cause of the pixel-dimension defect, without generalising
it. And `#799` was filed as *"provenance is wrong AND the manual-skip rule is unimplemented"*;
its claim that the applier "never skips" is not right — `skip_if_set` does skip, it is simply
**coarser** than ADR 0012 specifies, never consulting `set_by`. #793 owns the precedence chain;
#799 keeps the narrower question of which *extractor name* gets recorded.

### Decision: a default carries its own provenance

`asset_field_value.set_by` gains **`default`**, and the applier's skip becomes a provenance
check:

```go
if fc.Mode == ExtractionModeSkipIfSet && present && !current.isPlaceholder() { skip }
```

A value marked `default` is the one thing nobody chose. Extraction may improve on it;
everything else present — `manual`, `exif`, `iptc`, `xmp`, `api`, `import`, `computed` — stays
protected exactly as before. The skip's exemption is *widened by one value*, not removed.

**The alternative — apply defaults after extraction — was considered and rejected.** It changes
ordering rather than comparison, so it needs no migration, and it fails for a plainer reason:
extraction is an async job enqueued post-commit and **only for six image extensions**
(`jpg jpeg png tif tiff webp`). A default on a `.glb`, a `.pdf`, a `.wav` or a `.txt` would
never be applied at all, because nothing downstream ever runs. It would also make the upload
modal unable to say truthfully what an asset is about to carry, which is the artist-facing half
of the feature. A default has to be there when nothing else says otherwise — and "nothing else
ever runs" is the most common form of that.

The equal-value short-circuit is exempted for placeholders too. A default that happens to match
what the file says is still labelled "nobody chose this"; letting one write through relabels it,
and every pass after that takes the short-circuit normally, so backfill idempotency survives
after a single converging write.

### The corrected chain

```
a value already on the row      never touched — the defaults writer is
                                INSERT … ON CONFLICT DO NOTHING, so this
                                holds regardless of call order
extraction (skip_if_set)        may overwrite a `default`, nothing else
the uploader's team override    when exactly one applies
the field's own default
nothing
```

`replace`-mode extraction is unchanged: it overwrites regardless of provenance, which is what it
has always meant.

Note what this makes explicit that §3 left out: **a human's edit outranks extraction** under
`skip_if_set`. §3's chain started at "extracted" and never mentioned manual values, which is
part of why the mechanism's actual shape went unexamined.

### `target collection` is removed from the closed set

§3 listed "the target team or collection" as context values. The collection is not
implementable and is dropped: `POST /assets` has **no collection in scope at all** — collection
membership is a separate, later write. A menu entry the resolver can never answer is a setting
that appears to work and does nothing, which is the failure mode #774 established we do not
ship.

The set that exists is `uploading_user`, `uploading_team`, `current_date`. Each names one
storage column, and a context whose column does not match the field's type is rejected on write
with a message naming the ones that would fit.

### The team of an upload

`assets.team_id` exists but **nothing on the upload path writes it** — `AssetCreate` has no
`team_id` and `CreateAsset` never sets one. So a team override is resolved from the uploader's
**direct** team memberships, not from the asset's team and not through `team_closure` (an
ancestor team is a permission scope, not a place someone uploads to).

When more than one of the uploader's teams overrides the same field there is no correct answer,
so none is invented: both overrides are discarded and the field's own default applies. That is
deliberately not resolved by an `ORDER BY` on team name or creation date — a rule that picks
confidently and unpredictably is worse for an operator than one that steps back to the value
everyone agreed on. `uploading_team` follows the same rule and simply does not resolve when the
team is ambiguous; an unresolvable context applies **nothing**, never a blank or a zero.

### Storage

The default document is `jsonb` on `field_definition.default_value`, plus a
`field_default_override (field_id, team_id)` table with real FKs and `ON DELETE CASCADE`.

The apply path renders a default into the same `AssetFieldValueWrite` the manual `PUT` takes and
hands it to the same `buildUpsertParams`, so **the column a default lands in cannot diverge from
the column a manual value lands in** — by construction, not by a second switch statement kept in
step by hand. `app/internal/metadata/valuecolumn_test.go` pins it for all eleven field types and
compares the two writers' output byte for byte, which is the guard that stops a future
"simplification" from reintroducing the #778 / #791 class.

### Consequences

- `asset_field_value_set_by_check` grows a value, so this is a migration on a CHECK constraint
  (`00021`). `asset_field_value_history.set_by` carries no constraint and needed none;
  `collection_field_value` is untouched, because collections are not created by upload.
- `AssetFieldValue.set_by` on the wire grows `default`. `AssetFieldValueWrite.set_by`
  deliberately does **not**: a value a caller chose to send is by definition not one nobody
  chose, so `default` is the one provenance a client cannot claim.
- The upload modal **shows** a field's default and never pre-fills it. Pre-filling would send
  the value back as `set_by='manual'`, telling the pipeline a person chose it and stopping
  anything from ever improving on it — the same inversion by a different route.
- **§3's precedence sentence is retracted, not merely annotated.** Leaving it standing would be
  worse than the bug it described, because the next person would implement from it.

**Implementation note (2026-08-02):** §1 shipped as specified in PR #857 — per-row `site_text` table keyed `(key, language)`, read-time resolution in the client language store, cache invalidated on write with cross-instance NOTIFY, unknown keys refused against a build-embedded catalogue. The #774 prerequisite was fixed beforehand. §2 (as amended) and §3 remain the open tiles.
