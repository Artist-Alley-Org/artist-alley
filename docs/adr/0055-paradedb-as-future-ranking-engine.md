---
id: "0055"
title: pg_search / ParadeDB as a future ranking-engine option (record-only research snapshot)
status: proposed
date: 2026-07-02
area: architecture
phases: []
supersedes: []
related:
  - "0049"
tags:
  - search
  - postgres
  - extensions
  - licensing
  - research-record
excerpt: >-
  Record-only ADR documenting current state of ParadeDB / pg_search as a potential BM25 upgrade over Postgres native ts_rank_cd, backed by 2026-07 web research. NOT a commitment — captures maturity, licensing, managed-provider support, and benchmark evidence so a future revisit has facts, not stale assumptions.
---

## Status

**Proposed — record-only.** This ADR captures a research snapshot to support a future decision. AA is NOT adopting pg_search at this time. The current ranking layer (Postgres `ts_rank_cd` with A/B/C/D field weights + GIN indexes on `tsvector`, plus `pgvector` for embeddings via 1.14.B and hybrid ranking via Phase 1.16.B-3) stays in place until at least one of the revisit triggers below fires.

## Context

Phase 1.16.B-1 through B-5 shipped the search arc using Postgres native full-text search — `ts_rank_cd` with per-entity `A/B/C/D` field weights against `tsvector` columns, GIN indexes, `plainto_tsquery`/`to_tsquery` for query compilation, `pg_trgm` for autocomplete similarity, and `pgvector` for CLIP embeddings with app-layer BM25+cosine score merging. The 1.16.B-1 brief explicitly locked this choice with the note "real BM25 remains a future upgrade path; the query pipeline is designed so the ranking-function swap is a single-package change."

The operator asked in 2026-07-02 planning conversation whether AA should adopt `pg_search` (a Postgres extension by ParadeDB Inc. that implements BM25 full-text, faceted, and hybrid search over Postgres tables using Tantivy + pgrx). Two paths considered: adopt now, or maintain both engines with a cutover. Before either path, the operator asked for a research-backed record so future sessions revisit with facts.

This ADR is that record. It is deliberately NOT a commitment.

## Research findings (2026-07-02 web research; 21 confirmed claims from 23 sources)

### 1. Maturity + trajectory

- **Latest stable**: v0.24.1, released 2026-06-20 [1][2]. **Still pre-1.0** after 234 total releases and 3,141 commits on main [1][2].
- **Release cadence**: ~2-4 weeks for patches; 1-2 months for minors [1]. Five releases in the ~2.5-month window Apr 2 → Jun 20, 2026 [2].
- **Company viability**: ParadeDB Inc. closed a $12M Series A led by Craft Ventures in July 2025 with YC participation, bringing total capital to $14M [3][4]. Self-reports 7K+ GitHub stars and 100K+ installs at funding time (current repo shows ~9K stars, growing) [3][5].
- **Named production users**: Alibaba Cloud (first customer, May 2024) [4], Modern Treasury (migrated off Elasticsearch clusters) [3], Bilt Rewards, UnifyGTM, TCDI [3][4]. Sweetspot runs pg_search in production at 15M documents + 2M vectors with hybrid P99 latency of ~50ms [6]. Customer disclosures — no independently audited case studies.

### 2. Managed-Postgres provider support

**Materially constrained.** pg_search requires superuser access to install; on Postgres older than 17 it typically requires `shared_preload_libraries` [7]. This blocks most turnkey managed-Postgres offerings.

Confirmed status:

- **Supabase**: NOT supported on the primary Postgres. Documented pattern is a logical replica to a separate ParadeDB instance, with pg_search activated on the replica; primary remains system of record, ParadeDB acts as read-optimised search layer [8]. Open feature request for native primary support (Supabase issue #34709).
- **Crunchy Bridge**: Not listed in the official extensions catalog [9]. Search-related extensions offered: `pg_trgm`, `fuzzystrmatch`, `dict_int`, `dict_xsyn`, `similarity`. Whether installation is possible via support request was not verified.
- **Neon**: **DEPRECATED for new projects as of 2026-03-19** [10]. Existing projects retain access. Was AWS-region-only, never available in Azure regions. Neon steers users to `tsvector`/`tsquery` (FTS), `pg_trgm` (fuzzy), `pgvector` (semantic), or Neon's new `lakebase_text` (tsvector-compatible BM25 index) [10].
- **AWS RDS, Google Cloud SQL, Azure Database for Postgres, Timescale Cloud, Fly.io Postgres, Railway, Render**: NOT verified by this research. Original prompt asked; no primary-source data surfaced. Treat as "assume no unless confirmed."

The Neon deprecation is the strongest single signal — a paradedb-friendly managed provider walked away from primary support and pointed at native Postgres FTS as the replacement.

### 3. AGPL v3 licensing in practice

- **Dual licensing**: pg_search / ParadeDB Community is AGPL-3.0 [2][5]. A commercial, non-AGPL license (bundled with closed-source Enterprise features: index-on-JOINs, partitioned-table indexes, RLS-in-index) is available via ParadeDB sales [11].
- **ParadeDB does NOT interpret how AGPL applies to ordinary SQL-over-wire use** of pg_search [12]. Their AGPL rationale post cites MinIO, Grafana, Citus, and Quickwit as precedent-by-example rather than providing a derivative-work analysis for downstream apps [12]. This means: **the AGPL-vs-SaaS question for AA must be resolved against general AGPL interpretation and counsel, not ParadeDB guidance.**
- **MinIO parallel is contested**: the canonical MinIO GitHub discussions on whether AGPL applies to client apps talking over standard protocols (#21296, #12156) end with maintainer Klaus Post punting to legal teams; community counter-argument is that clients using standard protocols are not derivative works [13][14].
- **Community sentiment** (opencoreventures blog): "AGPL license is a non-starter for most companies" — argues most enterprise procurement teams reject AGPL dependencies out of hand, regardless of how the license technically applies [15]. Bias applies (blog by a competing commercial-license vendor), but reflects a real market pattern.

**Practical read for AA**: The legal question of whether AA's use of pg_search creates derivative-work obligations under AGPL is UNRESOLVED and would need counsel to answer definitively. The commercial escape hatch (non-AGPL license from ParadeDB) exists but requires a paid contract.

### 4. Ranking-quality + performance benchmarks

- **No published precision/recall/MRR/NDCG comparisons** of pg_search BM25 vs Postgres native `ts_rank_cd` survived research verification. This is a gap — the "BM25 is better ranking than ts_rank_cd" argument rests on theoretical IDF-inclusion, not measured relevance improvements.
- **Latency benchmarks** (from third-party sources, not head-to-head against tsvector for relevance):
  - **10M-row corpus** (Tembo benchmark): GIN index build 282.93s vs pg_search BM25 build 44.72s — ~6× faster index creation [16].
  - **2.68M-article corpus** (Cloudberry / mixed-benchmark comparison): High-frequency term query — pg_search BM25 71.07s vs GIN-trigram 99.15s; medium-frequency term — pg_search 23.55s vs GIN-trigram 92.80s [17]. Note: this compares BM25 to GIN-TRIGRAM (`pg_trgm`), not GIN-tsvector; different index type + different query pattern.
  - **Sweetspot production**: 15M documents + 2M vectors, hybrid BM25+vector query P99 latency ~50ms [6]. This is the strongest at-scale operational data point.

**Missing**: no measured quality-of-results improvement for typical DAM corpora (10k-100k assets). The performance wins scale up with corpus size; at AA's target range they may not be materially detectable.

### 5. Meaningful alternatives (partial coverage)

- **PGroonga**: Independent full-text extension. PGroonga's own benchmark claims ~698ms for a sample query vs textsearch ~602ms and pg_trgm ~33s — but this specific finding was refuted in verification (1-2 vote) so treat as unconfirmed [18][refuted]. PGroonga is mature but has a smaller commercial ecosystem than ParadeDB.
- **ZomboDB**: Postgres extension backed by Elasticsearch — mentioned in the mixed-corpus benchmark [17] but not deeply researched. Adds an Elasticsearch operational dependency, which contradicts AA's "one binary, three containers" architecture.
- **Postgres FTS + pg_trgm (current)**: The status quo. Native, no extension load. Faster autocomplete via trigrams. `ts_rank_cd` is well understood by operators.
- **RUM index type**: Not verified in research. Was in the original prompt; no surviving claims. Skip.
- **Neon `lakebase_text`**: New tsvector-compatible BM25 index Neon shipped as an alternative to pg_search in the deprecation window [10]. Neon-specific — not a portable alternative.

### 6. Hybrid BM25 + vector story

- ParadeDB published "Hybrid Search in PostgreSQL: The Missing Manual" describing BM25+pgvector hybrid search inside a single SQL query using Reciprocal Rank Fusion (RRF) as the fusion primitive [19]. Compares to application-layer merges (AA's current pattern).
- **Uncertain**: whether hybrid BM25+vector is fully shipped in Community edition or partially deferred to Enterprise is NOT conclusively resolved by this research. The claim "hybrid marked coming soon" was refuted in verification (1-2 vote) — suggesting it IS shipped, but the primary source was not clean.
- Sweetspot's 15M-doc / 2M-vector production deployment operates hybrid at P99 50ms [6], suggesting the feature works at scale in some form.

**Read**: AA's current app-layer merge in 1.16.B-3 is a legitimate alternative to pg_search's in-SQL RRF. Either works; ParadeDB's version is more concentrated but locks the merge algorithm.

### 7. Feature-parity gap with Postgres native FTS

Not conclusively benchmarked, but generally attributed to pg_search based on Tantivy's feature set:

- Real BM25 with IDF (Postgres `ts_rank_cd` uses cover-density scoring; different mathematical basis)
- Better fuzzy matching + snippet highlighting than Postgres `ts_headline`
- Fast facet aggregations in-index
- Query-time boost profiles + per-field weights + phrase slop
- Synonym expansion

For AA specifically: **field weights (A/B/C/D) via `setweight` already work in Postgres FTS** (shipped in 1.16.B-2 migration 00022). The gap for AA reduces to: IDF-aware ranking, better highlighting, fuzzy match (partially handled by `pg_trgm`), and faster facets at scale (not measured for our corpus range).

## Decision

**Defer adoption. Keep the current Postgres FTS + pgvector + app-layer merge stack. Do NOT commit to pg_search.**

Rationale, ranked by weight:

1. **Managed-provider drift** is going the WRONG direction. Neon deprecation (2026-03-19) is the strongest signal — a technically-capable managed provider walked away. Supabase requires a two-instance architecture. Crunchy Bridge doesn't offer it. Betting on wider managed-provider adoption is speculative.
2. **AGPL legal uncertainty is real and unresolved.** ParadeDB itself declines to interpret AGPL for SQL-over-wire use. The commercial-license escape hatch exists but requires a paid contract, which conflicts with AA's operator-friendly OSS positioning.
3. **No measured relevance improvement** for AA's target corpus size (thousands to low millions). All the benchmark wins found are latency at 10M+ rows and index-build speed. AA's users would not detect the quality difference.
4. **AA's current stack is not identified as a bottleneck.** No operator has complained about ranking quality. Solving hypothetical problems while real B-5 work is briefed violates the "don't ship half-finished implementations" bar.
5. **Pre-1.0 project risk.** After 234 releases, still no 1.0 tag; index-format stability across paradedb versions is a real upgrade-migration risk for federated peers.
6. **Federation-neutral** (per prior analysis): pg_search wouldn't help federation and adopting it wouldn't hurt it. Not a reason to move either way.

## Revisit triggers

Reassess this decision when ANY of the following fires:

1. **ParadeDB ships 1.0** with index-format stability guarantees.
2. **AWS RDS, Google Cloud SQL, and Azure Postgres all support pg_search natively** as trusted extensions (would close the managed-Postgres gap that Neon just widened).
3. **Operators complain about ranking quality** on corpora >100k assets — specific, actionable complaints, not general "search feels off."
4. **A feature request materialises for query-time synonyms, boost profiles, or richer highlighting** that Postgres FTS + `pg_trgm` can't deliver.
5. **AA's own corpus size in real deployments crosses ~1M assets** — the point where Postgres FTS latency starts showing on typical hardware per the 2.68M-article benchmark [17].
6. **ParadeDB clarifies AGPL applicability** to SQL-over-wire use via primary legal statement, OR clear industry consensus emerges from the MinIO / Grafana / Citus precedent chain.
7. **A major AA competitor moves to pg_search** and demonstrates a marketing/feature advantage.

Absent any of the above, defer indefinitely; re-review this ADR on the next annual planning pass.

## Alternatives considered

- **Adopt pg_search now (single ranking engine)** — rejected per Decision rationale.
- **Ship dual support: pg_search primary, tsvector fallback** — rejected. Doubles maintenance surface indefinitely; every future search feature has to answer "which engine?"; two ranking test suites; support conversations become "which engine are you on?" See 2026-07-02 planning conversation for full analysis.
- **Ship dual support with a hard cutover in Phase 2.0** — considered viable, but blocked on the AGPL and managed-provider risks that would still be present at cutover. Would create a forced Postgres-provider migration for early adopters on Cloud SQL / Azure Postgres who cannot install the extension.
- **Migrate to Elasticsearch or OpenSearch** — rejected in 2026-07-02 planning conversation. New operational surface (JVM, cluster health), separate index-drift bug class, doesn't help federation, no reason to move without corpus scale AA doesn't have.
- **Switch to `pgroonga`** — plausible alternative if BM25-shaped ranking becomes a need but AGPL is a blocker; PGroonga is BSD-licensed. Not deeply researched here; would justify its own ADR at revisit time.
- **Stay on `ts_rank_cd` + `pg_trgm` + `pgvector` + app-layer merge (current)** — **adopted.** Clean composition with existing federation, cache, visibility, and observability layers. All B-5 work continues to build on this stack.

## Caveats + open questions

Copied verbatim from the underlying research so future revisits know what's still unresolved:

1. Actual pg_search support state on AWS RDS, Google Cloud SQL, Azure Database for Postgres, Timescale Cloud, Fly.io Postgres, Railway, and Render — beyond the confirmed gaps on Supabase, Crunchy Bridge, and Neon. NOT VERIFIED here.
2. Published head-to-head **relevance-quality** benchmarks (precision/recall, MRR, NDCG) for pg_search BM25 vs Postgres native `ts_rank_cd`. NOT FOUND — only latency benchmarks exist.
3. Whether hybrid BM25+pgvector search is fully shipped in ParadeDB Community edition today (vs partially in Enterprise). UNCLEAR from primary sources; the "coming soon" claim was refuted in verification.
4. Current community/legal consensus on whether ordinary SQL-over-wire use of an AGPL Postgres extension creates derivative-work obligations for downstream apps. UNRESOLVED — MinIO precedent is invoked as social proof, not legal analog.

Time-sensitivity: release version, star count, Neon deprecation, and Supabase integration architecture are all weeks-old data points and will drift. Next revisit should refresh these facts.

## Reference

Primary sources cited in this record (fetched 2026-07-02):

1. https://github.com/paradedb/paradedb/releases — release cadence + latest version
2. https://pgxn.org/dist/pg_search/0.22.5/ — extension metadata + license
3. https://www.paradedb.com/blog/series-a-announcement — funding + customer list
4. https://techcrunch.com/2025/07/15/paradedb-takes-on-elasticsearch-as-interest-in-postgres-explodes-amid-ai-boom/ — third-party confirmation of funding
5. https://github.com/paradedb/paradedb — repo activity metrics
6. https://www.paradedb.com/blog/case-study-sweetspot — Sweetspot production deployment
7. https://docs.paradedb.com/deploy/self-hosted/extension — installation requirements
8. https://supabase.com/partners/integrations/paradedb — Supabase integration architecture
9. https://docs.crunchybridge.com/extensions-and-languages — Crunchy Bridge extension catalog
10. https://neon.com/docs/extensions/pg_search — Neon deprecation notice
11. https://docs.paradedb.com/deploy/enterprise — commercial + Enterprise features
12. https://www.paradedb.com/blog/agpl — ParadeDB's AGPL rationale (declines to interpret SQL-over-wire)
13. https://github.com/minio/minio/discussions/21296 — MinIO AGPL discussion (canonical unresolved)
14. https://github.com/minio/minio/discussions/12156 — earlier MinIO AGPL discussion
15. https://www.opencoreventures.com/blog/agpl-license-is-a-non-starter-for-most-companies — dissenting-market-view blog
16. https://legacy.tembo.io/blog/paradedb-search/ — Tembo 10M-row benchmark
17. https://medium.com/@lexuanf1212/full-text-search-solutions-comparation-on-apache-cloudberry-paradedb-bm25-vs-gin-vs-zombodb-08b83adf0e09 — mixed-corpus benchmark (2.68M articles)
18. https://pgroonga.github.io/reference/pgroonga-versus-textsearch-and-pg-trgm.html — PGroonga self-benchmark (specific claim refuted in verification)
19. https://www.paradedb.com/blog/hybrid-search-in-postgresql-the-missing-manual — RRF hybrid search inside SQL

Related ADRs:

- ADR 0049 — Encrypted federation + dogfood (federation-neutral for this decision)
- ADR 0043 — Federation walled-garden protocol (context for cross-instance implications)
- ADR 0052 — Optimistic-concurrency edit-safety (unaffected; edit safety and ranking engine are orthogonal)
