# v0.11.1 compatibility goldens (#1580)

Read-only evidence for the cross-version rows of #1580. Not conformance
vectors: they pin how this implementation meets the previous release.
The live two-stack gate is #1582.

- `sender_*.json`: envelopes as a v0.11.1 sender produced them. Its
  delivery worker merged the local activity payload into the envelope as
  is and sent a placeholder signature; the emitters in
  `app/internal/activities/emit` are unchanged since v0.11.1
  (`c3f6ef2e`). A current receiver refuses each one (`sig_malformed`
  since #1526), and `members_test.go` shows its members would be refused
  too.
- `current_sender_*.json`: for the same local payloads, the members this
  release sends (`federation.WireMembers`), and what a v0.11.1 receiver
  reads from them. A v0.11.1 receiver keeps unknown members and checks
  the signature for shape only, so it accepts these. `members_test.go`
  checks the members it reads are present and the `applies` claim: at
  v0.11.1 `inbox/handler_stub.go` `BuildRegistry` had real handlers only
  for `Like` and `Create` (a comment); every other verb went to a stub
  that marked the row processed without applying it. This models the
  v0.11.1 source; it does not run the old binary.
