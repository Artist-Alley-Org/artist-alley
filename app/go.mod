module github.com/mscrnt/artist-alley/app

go 1.26

require (
	github.com/aws/aws-sdk-go-v2 v1.43.0
	github.com/aws/aws-sdk-go-v2/config v1.32.31
	github.com/aws/aws-sdk-go-v2/credentials v1.19.30
	github.com/aws/aws-sdk-go-v2/service/s3 v1.106.0
	github.com/bodgit/sevenzip v1.6.5
	github.com/chai2010/webp v1.4.0
	github.com/dsoprea/go-exif/v3 v3.0.1
	github.com/getkin/kin-openapi v0.145.0
	github.com/go-chi/chi/v5 v5.3.1
	github.com/google/uuid v1.6.0
	github.com/gowebpki/jcs v1.0.1
	github.com/hashicorp/golang-lru/v2 v2.0.7
	github.com/jackc/pgx/v5 v5.10.0
	github.com/kaitai-io/kaitai_struct_go_runtime v0.11.0
	github.com/mscrnt/mviewer/go v0.0.0-20260529200211-fe5325066d66
	github.com/nwaples/rardecode/v2 v2.3.0
	github.com/oapi-codegen/runtime v1.6.0
	github.com/pdfcpu/pdfcpu v0.13.0
	github.com/pgvector/pgvector-go v0.4.0
	github.com/pressly/goose/v3 v3.27.3
	github.com/qmuntal/gltf v0.28.0
	github.com/srwiley/oksvg v0.0.0-20221011165216-be6e8873101c
	github.com/srwiley/rasterx v0.0.0-20220730225603-2ab79fcdd4ef
	github.com/taylorskalyo/goreader v0.0.0-00010101000000-000000000000
	github.com/ulikunitz/xz v0.5.16
	go.n16f.net/thumbhash v1.1.0
	golang.org/x/crypto v0.54.0
	golang.org/x/image v0.44.0
	golang.org/x/net v0.57.0
	golang.org/x/text v0.40.0
	golang.org/x/time v0.15.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/andybalholm/brotli v1.2.2 // indirect
	github.com/apapsch/go-jsonmerge/v2 v2.0.0 // indirect
	github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream v1.7.14 // indirect
	github.com/aws/aws-sdk-go-v2/feature/ec2/imds v1.18.31 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.4.31 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.7.31 // indirect
	github.com/aws/aws-sdk-go-v2/internal/v4a v1.4.32 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.13.13 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/checksum v1.9.24 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.13.31 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/s3shared v1.19.32 // indirect
	github.com/aws/aws-sdk-go-v2/service/signin v1.5.0 // indirect
	github.com/aws/aws-sdk-go-v2/service/sso v1.33.0 // indirect
	github.com/aws/aws-sdk-go-v2/service/ssooidc v1.38.0 // indirect
	github.com/aws/aws-sdk-go-v2/service/sts v1.45.0 // indirect
	github.com/aws/smithy-go v1.27.3 // indirect
	github.com/bodgit/plumbing v1.3.0 // indirect
	github.com/bodgit/windows v1.0.1 // indirect
	github.com/clipperhouse/uax29/v2 v2.7.0 // indirect
	github.com/dsoprea/go-logging v0.0.0-20200710184922-b02d349568dd // indirect
	github.com/dsoprea/go-utility/v2 v2.0.0-20221003172846-a3e1774ef349 // indirect
	github.com/go-errors/errors v1.4.2 // indirect
	github.com/go-openapi/jsonpointer v0.22.5 // indirect
	github.com/go-openapi/swag/jsonname v0.25.5 // indirect
	github.com/go-test/deep v1.0.8 // indirect
	github.com/golang/geo v0.0.0-20210211234256-740aa86cb551 // indirect
	github.com/hhrutter/lzw v1.0.0 // indirect
	github.com/hhrutter/pkcs7 v0.2.2 // indirect
	github.com/hhrutter/tiff v1.0.3 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/klauspost/compress v1.19.1 // indirect
	github.com/mattn/go-runewidth v0.0.24 // indirect
	github.com/mfridman/interpolate v0.0.2 // indirect
	github.com/oasdiff/yaml v0.1.1 // indirect
	github.com/oasdiff/yaml3 v0.0.14 // indirect
	github.com/pierrec/lz4/v4 v4.1.27 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/rogpeppe/go-internal v1.15.0 // indirect
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.2 // indirect
	github.com/sethvargo/go-retry v0.4.0 // indirect
	github.com/spf13/afero v1.15.0 // indirect
	github.com/stangelandcl/ppmd v0.1.1 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	go4.org v0.0.0-20260112195520-a5071408f32f // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	gopkg.in/yaml.v2 v2.4.0 // indirect
)

// Supply-chain forks — pinned to Artist-Alley-Org/* mirrors so a small-
// maintainer upstream can't disappear under us. See
// memory/project_dep_fork_audit.md for the rule set + risk
// rationale. Add a new entry here whenever a fork is created;
// `go mod tidy` will resolve the version against the fork.
replace (
	github.com/chai2010/webp => github.com/Artist-Alley-Org/webp v1.4.0
	github.com/qmuntal/gltf => github.com/Artist-Alley-Org/gltf v0.28.0
	github.com/srwiley/oksvg => github.com/Artist-Alley-Org/oksvg v0.0.0-20221011165216-be6e8873101c
	github.com/srwiley/rasterx => github.com/Artist-Alley-Org/rasterx v0.0.0-20220730225603-2ab79fcdd4ef
	// goreader's epub package — pure-Go EPUB parser. We only
	// import the `epub/` subpackage (terminal UI half is dead
	// weight Go tree-shaking drops at link time).
	github.com/taylorskalyo/goreader => github.com/Artist-Alley-Org/goreader v0.0.0-20250314214816-f9256af1ef9f
	go.n16f.net/thumbhash => github.com/Artist-Alley-Org/go-thumbhash v1.1.0
)
