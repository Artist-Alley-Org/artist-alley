# Licensing

artist-alley is licensed under the **GNU Affero General Public License,
version 3.0** (AGPL-3.0-only), and is free to self-host.

Commercial licensing and paid tiers are planned but are **not currently
offered**. That direction is recorded in
[ADR 0016](docs/adr/0016-license-direction.md) (license direction) and
[ADR 0017](docs/adr/0017-monetization-and-licensing.md) (monetization model);
see section 2.

## 1. Open-source use: AGPL-3.0-only

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

## 2. Commercial licensing (planned, not currently offered)

A separate commercial license from the copyright holder, Kenneth Blossom, is
the accepted long-term direction for anyone who wants to **embed,
redistribute, or offer artist-alley (or a derivative) as a service without
the AGPL's source-sharing obligations**. It is not available today: no
commercial license, paid tier or premium add-on can be obtained, and until
one is offered, AGPL-3.0-only is the only license artist-alley is available
under.

For questions about future commercial terms, contact:

> **licensing@artist-alley.org**

## Contributions

Contributions are accepted under AGPL-3.0-only. If commercial licensing is
introduced later, the necessary contributor agreement or permission must be
established before third-party contributions are commercially relicensed. No
contributor agreement exists today; that future work is tracked in
[#263](https://github.com/Artist-Alley-Org/artist-alley/issues/263).

## Third-party components

Bundled third-party dependencies retain their own licenses; the AGPL-3.0-only
license covers artist-alley's own source. No AGPL-incompatible code is shipped
in the tracked source tree (attribution audit, Phase 1.55.S).
