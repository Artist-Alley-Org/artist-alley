# Licensing

artist-alley is **dual-licensed**. You may use it under **either**:

1. the **GNU Affero General Public License, version 3.0** (AGPL-3.0-only), **or**
2. a separate **commercial license** from the copyright holder.

Choose whichever fits your use. This is the licensing direction set in
[ADR 0016](docs/adr/0016-license-direction.md) and the monetization model in
[ADR 0017](docs/adr/0017-monetization-and-licensing.md).

## 1. Open-source use — AGPL-3.0-only

The default license is the AGPL-3.0, in full at [`LICENSE`](LICENSE) and
identified in every source file by the SPDX tag:

```
SPDX-License-Identifier: AGPL-3.0-only
```

The AGPL is a strong copyleft license. In particular — and unlike the GPL —
its **§13 network clause** means that if you run a modified version of
artist-alley and let users interact with it **over a network**, you must
offer those users the **complete corresponding source** of your modified
version under the same AGPL terms. Self-hosting the unmodified project for
your own community is fine; running a modified fork as a network service
obligates you to publish your changes.

If those obligations work for you, use artist-alley under the AGPL at no
cost. You owe nothing but the copyleft.

## 2. Commercial use — separate paid license

If you want to **embed, redistribute, or offer artist-alley (or a derivative)
as a service without the AGPL's source-sharing obligations** — for example,
bundling it into a closed-source product or running a modified fork as a
commercial service without publishing your changes — a **commercial license**
is available. It grants those rights without the AGPL copyleft, and its terms
forbid removing or bypassing the license check for commercial deployment
(per [ADR 0017](docs/adr/0017-monetization-and-licensing.md)).

**To obtain a commercial license, contact:**

> **licensing@artist-alley.org**

_(A self-serve purchase/management portal is planned; until it lands, reach
out by email.)_

## Contributions

Contributions are accepted under the AGPL-3.0 (the project's open-source
license). By submitting a contribution you agree it may be distributed under
both the AGPL and the commercial license above.

## Third-party components

Bundled third-party dependencies retain their own licenses; this dual license
covers artist-alley's own source. No AGPL-incompatible code is shipped in the
tracked source tree (attribution audit, Phase 1.55.S).
