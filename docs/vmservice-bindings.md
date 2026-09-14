# OpenVMM vmservice Go bindings

The files in `internal/vmservice` are generated from OpenVMM's `vmservice.proto`. The proto is not copied into this repository.

## Source

- Repository: `https://github.com/microsoft/openvmm`
- Revision: `14d607e4d7117df2f6934707c0de56110e7fb8a9`
- Path: `openvmm/openvmm_ttrpc_vmservice/src/vmservice.proto`
- SHA-256: `E936C7B258E6B2A65A88B392D459A2C3A6A9FE163BD04B132E7F0FA32CE7EBAD`
- Generation input SHA-256: `50142DD6F7AEBACD9F9202E953651EDE0A2EBBAF30182AB6AA47A53CA2C2B610`

## Toolchain

- `protoc`: `libprotoc 34.1` (recorded by generated files as `v7.34.1`)
- `protoc-gen-go`: `v1.36.11`
- `protoc-gen-go-grpc`: `1.6.0`

The upstream proto declares `option go_package = "vmservice"`, which current `protoc-gen-go` rejects as a Go import path. Generation therefore maps the source file explicitly to this package.

## Regeneration

Run these commands from a temporary directory, not the repository root:

```powershell
$openvmm = 'Q:\openvmm-openvmm-lcow-mvp'
$revision = '14d607e4d7117df2f6934707c0de56110e7fb8a9'
if ((git -C $openvmm rev-parse HEAD).Trim() -ne $revision) {
  throw "OpenVMM HEAD does not match $revision"
}
$source = Join-Path $openvmm 'openvmm\openvmm_ttrpc_vmservice\src\vmservice.proto'
Copy-Item $source vmservice.proto

# The pinned upstream proto imports Struct but never references it. Remove that
# import so the generated client does not add an unused structpb vendor package.
$proto = (Get-Content vmservice.proto -Raw).Replace("import `"google/protobuf/struct.proto`";`n", '')
[IO.File]::WriteAllText((Join-Path $PWD vmservice.proto), $proto, [Text.UTF8Encoding]::new($false))

protoc `
  --go_out=. `
  --go_opt=paths=source_relative `
  --go_opt=Mvmservice.proto=github.com/Microsoft/hcsshim/internal/vmservice `
  --go-grpc_out=. `
  --go-grpc_opt=paths=source_relative `
  --go-grpc_opt=Mvmservice.proto=github.com/Microsoft/hcsshim/internal/vmservice `
  vmservice.proto
```

## Expected outputs

| File | SHA-256 |
|---|---|
| `vmservice.pb.go` | `4BA501AD19B85CBD7D7ABDB026EAF5CF7145BE94974A40CA0DA5088B76A970E2` |
| `vmservice_grpc.pb.go` | `979F14C257C4391B6C31867C54335C7CA8A0DCD47DD93BE924331E1910D3FE0F` |

Copy the generated files to `internal/vmservice` only when both hashes match. A source revision or toolchain update must update this document and the expected hashes in the same change.
