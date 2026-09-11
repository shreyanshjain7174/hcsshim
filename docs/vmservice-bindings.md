# OpenVMM vmservice Go bindings

The files in `internal/vmservice` are generated from OpenVMM's `vmservice.proto`. The proto is not copied into this repository.

## Source

- Repository: `https://github.com/microsoft/openvmm`
- Revision: `61fd38fe6114d0955a082aa717a8e16e722b8490`
- Path: `openvmm/openvmm_ttrpc_vmservice/src/vmservice.proto`
- SHA-256: `D3A03C6DBCB633579A7F78535DE8EDD2CA73B6B61546E269D7C1ABD3B8A4DDD0`
- Generation input SHA-256: `9D8876BEBF102B095593BB4CF512FFD53CBA0F2867C06F0E2FE761B48C776859`

## Toolchain

- `protoc`: `libprotoc 34.1` (recorded by generated files as `v7.34.1`)
- `protoc-gen-go`: `v1.36.11`
- `protoc-gen-go-grpc`: `1.6.0`

The upstream proto declares `option go_package = "vmservice"`, which current `protoc-gen-go` rejects as a Go import path. Generation therefore maps the source file explicitly to this package.

## Regeneration

Run these commands from a temporary directory, not the repository root:

```powershell
$revision = '61fd38fe6114d0955a082aa717a8e16e722b8490'
$source = "https://raw.githubusercontent.com/microsoft/openvmm/$revision/openvmm/openvmm_ttrpc_vmservice/src/vmservice.proto"
Invoke-WebRequest -Uri $source -OutFile vmservice.proto

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
| `vmservice.pb.go` | `3759ABBD392E9382A80DB618B2CC3E2728CE81928F321A58E64681F5667B89BF` |
| `vmservice_grpc.pb.go` | `979F14C257C4391B6C31867C54335C7CA8A0DCD47DD93BE924331E1910D3FE0F` |

Copy the generated files to `internal/vmservice` only when both hashes match. A source revision or toolchain update must update this document and the expected hashes in the same change.
