module google.golang.org/grpc/benchmark/shmsccmp

go 1.25.0

// Compares the in-tree ("monolithic") SHM transport against the
// self-contained plugin/shmsc transport. Kept as its own nested module so
// neither the root module nor plugin/shmsc has to gain a dependency on the
// other: the root module's build stays free of the plugin, and the plugin
// keeps its no-internal-imports guarantee.
replace google.golang.org/grpc => ../..

replace google.golang.org/grpc/plugin/shmsc => ../../plugin/shmsc

require (
	google.golang.org/grpc v1.80.0
	google.golang.org/grpc/plugin/shmsc v0.0.0-00010101000000-000000000000
)

require (
	golang.org/x/net v0.53.0 // indirect
	golang.org/x/sys v0.43.0 // indirect
	golang.org/x/text v0.36.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260414002931-afd174a4e478 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)
