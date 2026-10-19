// Package vmservice is the generated gRPC client for the OpenVMM vmservice API.
//
// Source: openvmm/openvmm_ttrpc_vmservice/src/vmservice.proto at OpenVMM revision
// 1f28f042175ee98fe2781253602062d445f0870f on branch user/shsancheti/openvmm-virtiofs-shares
// of github.com/shreyanshjain7174/openvmm. Re-pin to an upstream OpenVMM commit before merge.
//
// Toolchain: protoc libprotoc 34.1, protoc-gen-go v1.36.12 and protoc-gen-go-grpc 1.6.2
// (both from the go.mod tool dependencies).
//
// To regenerate, copy vmservice.proto to a temporary directory, remove its unused
// `import "google/protobuf/struct.proto";` line, and run there:
//
//	protoc --plugin=protoc-gen-go=$(go tool -n protoc-gen-go) \
//	  --plugin=protoc-gen-go-grpc=$(go tool -n protoc-gen-go-grpc) \
//	  --go_out=. --go_opt=paths=source_relative \
//	  --go_opt=Mvmservice.proto=github.com/Microsoft/hcsshim/internal/vmservice \
//	  --go-grpc_out=. --go-grpc_opt=paths=source_relative \
//	  --go-grpc_opt=Mvmservice.proto=github.com/Microsoft/hcsshim/internal/vmservice \
//	  vmservice.proto
//
// Then copy vmservice.pb.go and vmservice_grpc.pb.go into this package.
package vmservice
