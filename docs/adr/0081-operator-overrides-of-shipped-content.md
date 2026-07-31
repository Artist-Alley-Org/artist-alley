---
id: "0081"
title: Operators override shipped strings and field defaults, but never shipped templates
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
  content that ships inside the binary? Strings and field defaults become
  operator-editable data; email templates do not, because an operator-authored
  template is executable content on the one channel that leaves the instance.
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

**Precedence is fixed and shallow**, so a value's origin is always explainable:
extracted > team default > field default > empty. A default never overwrites a value that is
already set, and never overwrites an extracted one.

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
- Operators wanting genuinely custom mail will be told no. That is the intended answer, and the
  branding fields are what makes it an acceptable one.
- Three surfaces now resolve shipped-content-plus-override at read time (strings, field
  defaults, email branding). Each must cache and invalidate on write; none may resolve per row
  in a loop.
- **We diverge from the prior art on all three.** Keyed by string key rather than page/name;
  no operator templates where they have none either; declarative defaults where they use
  executable macros. Only the first is a case of us being better by construction — the other
  two are cases of choosing a smaller surface deliberately.
