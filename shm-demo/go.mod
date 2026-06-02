module shmdemo

go 1.25.0

require (
	golang.org/x/sys v0.43.0
	google.golang.org/grpc v1.81.1
	google.golang.org/protobuf v1.36.11
)

require (
	golang.org/x/net v0.53.0 // indirect
	golang.org/x/text v0.36.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260414002931-afd174a4e478 // indirect
)

// Build against the enclosing grpc-go-shmem checkout (this demo lives inside it
// as a nested module, mirroring the examples/ module's `replace => ../`).
replace google.golang.org/grpc => ../
