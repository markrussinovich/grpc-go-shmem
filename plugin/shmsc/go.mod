module google.golang.org/grpc/plugin/shmsc

go 1.25.0

// Build the self-contained plugin against the local grpc-go tree (which carries
// the experimental/transport API), matching the convention used by the other
// nested modules in this repository (gcp/observability, security/advancedtls,
// ...). The required version below is the released grpc-go this module targets;
// the replace keeps the monorepo build honest until the experimental/transport
// API appears in a release.
replace google.golang.org/grpc => ../..

require (
	golang.org/x/net v0.53.0
	golang.org/x/sys v0.43.0
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260414002931-afd174a4e478
	google.golang.org/grpc v1.80.0
	google.golang.org/protobuf v1.36.11
)

require golang.org/x/text v0.36.0 // indirect
