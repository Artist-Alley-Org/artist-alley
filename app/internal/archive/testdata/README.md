# archive/testdata

Tiny binary fixtures for parsers that can't be built in-memory from
the test code (neither bodgit/sevenzip nor nwaples/rardecode ships
a writer).

## sample.7z

Generated with p7zip from an Alpine container so the recipe is
reproducible without depending on host tools:

```bash
mkdir -p tmp/payload/sub
printf 'hello from sevenzip\n' > tmp/payload/hello.txt
printf 'nested entry\n'        > tmp/payload/sub/file2.txt

docker run --rm -v "$PWD/tmp:/work" -w /work alpine:3 sh -c '
  apk add --no-cache p7zip >/dev/null &&
  7z a -mx=1 -t7z fixture.7z payload/ >/dev/null
'
mv tmp/fixture.7z sample.7z && rm -rf tmp
```

Layout:

```
payload/hello.txt        ("hello from sevenzip\n", 20 bytes)
payload/sub/file2.txt    ("nested entry\n",         13 bytes)
```

## sample.rar

Generated with the official rarlab build of `rar` (proprietary,
not packaged for Alpine), pulled into a container on demand so the
recipe is reproducible and the host doesn't need rar installed:

```bash
mkdir -p tmp/payload/sub
printf 'hello from sevenzip\n' > tmp/payload/hello.txt
printf 'nested entry\n'        > tmp/payload/sub/file2.txt

docker run --rm -v "$PWD/tmp:/work" -w /work/payload alpine:3 sh -c '
  apk add --no-cache curl tar gcompat libstdc++ >/dev/null &&
  cd /tmp &&
  curl -fsSL https://www.rarlab.com/rar/rarlinux-x64-712.tar.gz -o r.tgz &&
  tar xf r.tgz >/dev/null &&
  cd /work/payload &&
  /tmp/rar/rar a -r -m1 -ep1 /work/fixture.rar . >/dev/null
'
mv tmp/fixture.rar sample.rar && rm -rf tmp
```

Layout (the `-ep1` flag strips the leading `./`):

```
hello.txt        ("hello from sevenzip\n", 20 bytes)
sub/file2.txt    ("nested entry\n",         13 bytes)
sub              (directory)
```

## Regenerating

Tests skip when a fixture is missing, so re-running these recipes
is the only step needed to bring fresh fixtures online. Keep the
payloads identical to the constants in `sevenzip_test.go` /
`rar_test.go` or update those alongside.
