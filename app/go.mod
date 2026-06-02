module github.com/mscrnt/artist-alley/app

go 1.26

require (
	github.com/aws/aws-sdk-go-v2 v1.41.7
	github.com/aws/aws-sdk-go-v2/config v1.32.18
	github.com/aws/aws-sdk-go-v2/credentials v1.19.17
	github.com/aws/aws-sdk-go-v2/service/s3 v1.101.0
	github.com/chai2010/webp v1.4.0
	github.com/getkin/kin-openapi v0.139.0
	github.com/go-chi/chi/v5 v5.3.0
	github.com/google/uuid v1.6.0
	github.com/hashicorp/golang-lru/v2 v2.0.7
	github.com/jackc/pgx/v5 v5.9.2
	github.com/kaitai-io/kaitai_struct_go_runtime v0.11.0
	github.com/mscrnt/mviewer/go v0.0.0-20260529200211-fe5325066d66
	github.com/oapi-codegen/runtime v1.4.1
	github.com/pressly/goose/v3 v3.27.1
	github.com/qmuntal/gltf v0.28.0
	github.com/srwiley/oksvg v0.0.0-20221011165216-be6e8873101c
	github.com/srwiley/rasterx v0.0.0-20220730225603-2ab79fcdd4ef
	github.com/taylorskalyo/goreader v0.0.0-00010101000000-000000000000
	go.n16f.net/thumbhash v1.1.0
	golang.org/x/crypto v0.52.0
	golang.org/x/image v0.32.0
	golang.org/x/net v0.54.0
	golang.org/x/text v0.37.0
)

require (
	github.com/apapsch/go-jsonmerge/v2 v2.0.0 // indirect
	github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream v1.7.10 // indirect
	github.com/aws/aws-sdk-go-v2/feature/ec2/imds v1.18.23 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.4.23 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.7.23 // indirect
	github.com/aws/aws-sdk-go-v2/internal/v4a v1.4.24 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.13.9 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/checksum v1.9.15 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.13.23 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/s3shared v1.19.23 // indirect
	github.com/aws/aws-sdk-go-v2/service/signin v1.0.11 // indirect
	github.com/aws/aws-sdk-go-v2/service/sso v1.30.17 // indirect
	github.com/aws/aws-sdk-go-v2/service/ssooidc v1.36.0 // indirect
	github.com/aws/aws-sdk-go-v2/service/sts v1.42.1 // indirect
	github.com/aws/smithy-go v1.25.1 // indirect
	github.com/go-openapi/jsonpointer v0.21.0 // indirect
	github.com/go-openapi/swag v0.23.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/josharian/intern v1.0.0 // indirect
	github.com/mailru/easyjson v0.7.7 // indirect
	github.com/mfridman/interpolate v0.0.2 // indirect
	github.com/mohae/deepcopy v0.0.0-20170929034955-c48cc78d4826 // indirect
	github.com/oasdiff/yaml v0.1.0 // indirect
	github.com/oasdiff/yaml3 v0.0.13 // indirect
	github.com/perimeterx/marshmallow v1.1.5 // indirect
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.2 // indirect
	github.com/sethvargo/go-retry v0.3.0 // indirect
	github.com/woodsbury/decimal128 v1.3.0 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

// Supply-chain forks — pinned to mscrnt/* mirrors so a small-
// maintainer upstream can't disappear under us. See
// memory/project_dep_fork_audit.md for the rule set + risk
// rationale. Add a new entry here whenever a fork is created;
// `go mod tidy` will resolve the version against the fork.
replace (
	github.com/chai2010/webp => github.com/mscrnt/webp v1.4.0
	github.com/qmuntal/gltf => github.com/mscrnt/gltf v0.28.0
	github.com/srwiley/oksvg => github.com/mscrnt/oksvg v0.0.0-20221011165216-be6e8873101c
	github.com/srwiley/rasterx => github.com/mscrnt/rasterx v0.0.0-20220730225603-2ab79fcdd4ef
	// goreader's epub package — pure-Go EPUB parser. We only
	// import the `epub/` subpackage (terminal UI half is dead
	// weight Go tree-shaking drops at link time).
	github.com/taylorskalyo/goreader => github.com/mscrnt/goreader v0.0.0-20250314214816-f9256af1ef9f
	go.n16f.net/thumbhash => github.com/mscrnt/go-thumbhash v1.1.0
)
