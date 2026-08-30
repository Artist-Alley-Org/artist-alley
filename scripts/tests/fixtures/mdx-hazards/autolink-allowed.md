# Autolink fixture: the scanner's long-standing allowance

The scanner strips `<http(s)://...>` before scanning and reports no
hazard for it. This is scanner POLICY, not parser safety: MDX itself
rejects an autolink (it reads `https:` as a namespaced tag name and
chokes on the `/`). The allowance is preserved because the only
autolink in the synced corpus lives in docs/install/README.md, which
the site consumes as a config partial rather than rendering as a
page. This fixture pins the behaviour so a future rule change does
not silently alter it.

Then open <http://localhost:8080>.

See <https://example.com/a?b=1> for more.
