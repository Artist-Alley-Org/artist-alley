# Safe fixture: constructs the MDX compiler accepts

Every construct below was compiled through @mdx-js/mdx 3.1.1 and
compiles clean. The scanner must report ZERO hazards here.

Inline code spans carrying the same comparisons that break in prose:
bounds `A <= B`, the `size < A` case, and `A <= size <= B` for both.

A fenced block carrying them:

```
A <= B
size < A
A <= size <= B
1<2
<AssetCard>
```

A fenced block with a language tag:

```text
A <= size <= B
```

A bare `<` followed by whitespace is LITERAL TEXT in MDX, not a tag.
`startAfter()` in micromark-extension-mdx-jsx bails out on
`markdownLineEndingOrSpace`, deliberately deviating from JSX. So all
of these compile and must NOT be flagged:

bounds A < B here, and size < A on its own, and A < 5 too.

An escaped bracket compiles: bounds A \<= B here.

The HTML entity compiles: bounds A &lt;= B here, and size &lt; A.

A brace not followed by an identifier is left alone: {1, 2}.
