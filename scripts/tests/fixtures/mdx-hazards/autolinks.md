# Autolink fixture: autolinks are hazards, ordinary links are not

MDX rejects a CommonMark autolink: it reads `https:` as a namespaced
tag name and chokes on the `/`. PR #1533's autolinks in ADR 0100
broke every site build while the scanner reported 0 hazards, because
it stripped them before scanning. Exactly the two lines below that
start with "Unsafe" must be reported. Every other line must pass.

Unsafe http: then open <http://localhost:8080>.

Unsafe https: see <https://example.com/a?b=1> for more.

Safe link: then open [localhost:8080](http://localhost:8080).

Safe inline code: the form `<https://example.com>` is quoted as code.

Safe escaped bracket: \<https://example.com> is a literal bracket.

Safe entity: &lt;https://example.com&gt; is written as an entity.

```
Safe fenced block: <https://example.com/in/a/fence>
```
