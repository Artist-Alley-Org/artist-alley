# Unsafe fixture: prose forms the MDX compiler rejects

Every `<` below is followed by a NON-whitespace character, which is
exactly the condition under which micromark-extension-mdx-jsx enters
tag parsing. Each line was compiled through @mdx-js/mdx 3.1.1 and
each one fails. The scanner must report all of them.

The first two lines are the literal text sprint 18b merged into ADR
0093, which broke artist-alley-site's `Build verify` run 33286001571
at `0093-browse-and-search-compose-one-query.mdx:465:31`:

populations against bounds A <= B and compares hit ID **sets**:

| population | condition | satisfies |
|---|---|---|
| X | A <= size <= B | both |

Other non-whitespace followers, same failure class:

a digit follows: I <3 that idea
no spaces at all: when 1<2 holds
an operator follows: value <- assigned
an underscore follows: value <_name here
a bang follows: <!-- an HTML comment is not MDX -->

The old `<[a-zA-Z]` rule caught only this shape, and it must keep
being caught: mount the <AssetCard> component.

The brace hazard is unchanged. It parses (`up,down` is a valid JS
sequence expression) and then explodes at render on the undefined
identifiers, which is how PR #208 took the site down: the config
keys are {up,down}.
