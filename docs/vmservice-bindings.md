# OpenVMM vmservice Go bindings

Last verified: 2026-09-14

The files in `internal/vmservice` are generated from OpenVMM's `vmservice.proto`. The proto is not copied into this repository.

## Source

- Repository: `https://github.com/microsoft/openvmm`
- Revision: `2a0db026c788fdcde91a40cbb7a04d1f852fb1fa`
- Status: pushed to branch `user/shsancheti/openvmm-lcow-mvp` on `https://github.com/shreyanshjain7174/openvmm`, not upstream
- Path: `openvmm/openvmm_ttrpc_vmservice/src/vmservice.proto`
- SHA-256: `E936C7B258E6B2A65A88B392D459A2C3A6A9FE163BD04B132E7F0FA32CE7EBAD`
- Generation input SHA-256: `50142DD6F7AEBACD9F9202E953651EDE0A2EBBAF30182AB6AA47A53CA2C2B610`

## VirtioFS compatibility

`VirtioFSConfig` includes the additive proto3 field `bool read_only = 3`. Its absent or
`false` default remains writable. A value of `true` maps in the compatible OpenVMM server
to the canonical host mount option `ro`, which enforces read-only behavior in the backend.
Guest mount flags remain `0`; callers must not depend on guest `MS_RDONLY` for this contract.

Proto3 unknown-field behavior makes version skew security-relevant here. A server built from
an older schema ignores field 3 and handles the share as writable. Pin compatible OpenVMM
source, generated bindings, and server binaries whenever `read_only=true` is required.

The generated-field contract is covered by
`internal/vmservice.TestVirtioFSConfigReadOnlyWireEncoding`. OpenVMM separately
tests the `true` to `ro` mapping and direct read-only and writable backend behavior.

Before merging, re-pin to the merged upstream OpenVMM commit and regenerate the bindings.

## Toolchain

- `protoc`: `libprotoc 34.1` (recorded by generated files as `v7.34.1`)
- `protoc-gen-go`: `v1.36.12` (from the hcsshim `go.mod` tool dependency)
- `protoc-gen-go-grpc`: `1.6.2` (from the hcsshim `go.mod` tool dependency)

The upstream proto declares `option go_package = "vmservice"`, which current `protoc-gen-go` rejects as a Go import path. Generation therefore maps the source file explicitly to this package.

## Regeneration

Set `OPENVMM_ROOT` to the OpenVMM checkout, then run these commands from the hcsshim
repository root. Generated files are written to a temporary directory.

```powershell
$openvmm = $env:OPENVMM_ROOT
if ([string]::IsNullOrWhiteSpace($openvmm)) {
  throw 'OPENVMM_ROOT must point to an OpenVMM checkout'
}
if ((git -C $openvmm rev-parse --is-inside-work-tree 2>$null).Trim() -ne 'true') {
  throw "OPENVMM_ROOT is not a Git repository: $openvmm"
}

$repository = (git rev-parse --show-toplevel).Trim()
$revision = '2a0db026c788fdcde91a40cbb7a04d1f852fb1fa'
if ((git -C $openvmm rev-parse HEAD).Trim() -ne $revision) {
  throw "OpenVMM HEAD does not match $revision"
}
$source = Join-Path $openvmm 'openvmm\openvmm_ttrpc_vmservice\src\vmservice.proto'
if (-not (Test-Path -LiteralPath $source -PathType Leaf)) {
  throw "vmservice.proto not found under OPENVMM_ROOT: $source"
}
$sourceHash = 'E936C7B258E6B2A65A88B392D459A2C3A6A9FE163BD04B132E7F0FA32CE7EBAD'
if ((Get-FileHash $source -Algorithm SHA256).Hash -ne $sourceHash) {
  throw "vmservice.proto does not match source hash $sourceHash"
}

$work = Join-Path ([IO.Path]::GetTempPath()) "hcsshim-vmservice-$([guid]::NewGuid())"
New-Item -ItemType Directory $work | Out-Null
$input = Join-Path $work 'vmservice.proto'
Copy-Item $source $input

# The proto does not reference Struct. Remove the import so the descriptor does
# not create an otherwise-unused structpb dependency in the generated package.
$proto = Get-Content $input -Raw
$normalized = $proto.Replace("import `"google/protobuf/struct.proto`";`n", '')
if ($normalized -eq $proto) {
  throw 'vmservice.proto normalization did not remove the Struct import'
}
[IO.File]::WriteAllText($input, $normalized, [Text.UTF8Encoding]::new($false))
$inputHash = '50142DD6F7AEBACD9F9202E953651EDE0A2EBBAF30182AB6AA47A53CA2C2B610'
if ((Get-FileHash $input -Algorithm SHA256).Hash -ne $inputHash) {
  throw "normalized vmservice.proto does not match generation input hash $inputHash"
}

$protocGenGo = (go tool -n protoc-gen-go).Trim()
$protocGenGoGrpc = (go tool -n protoc-gen-go-grpc).Trim()

Push-Location $work
try {
  protoc `
    "--plugin=protoc-gen-go=$protocGenGo" `
    "--plugin=protoc-gen-go-grpc=$protocGenGoGrpc" `
    --go_out=. `
    --go_opt=paths=source_relative `
    --go_opt=Mvmservice.proto=github.com/Microsoft/hcsshim/internal/vmservice `
    --go-grpc_out=. `
    --go-grpc_opt=paths=source_relative `
    --go-grpc_opt=Mvmservice.proto=github.com/Microsoft/hcsshim/internal/vmservice `
    vmservice.proto
  if ($LASTEXITCODE -ne 0) {
    throw "protoc failed with exit code $LASTEXITCODE"
  }
} finally {
  Pop-Location
}

$outputs = @{
  'vmservice.pb.go' = '54474D676E163D4DFA9F0F21F5324A6A717B2A11EF256C4FFF2F4367A35B7E10'
  'vmservice_grpc.pb.go' = '05F87D56A0978F6F2B83FA800667CE7F368CBA9B4AFE05A2BEE263E963D80E1F'
}
foreach ($output in $outputs.GetEnumerator()) {
  $generated = Join-Path $work $output.Key
  if ((Get-FileHash $generated -Algorithm SHA256).Hash -ne $output.Value) {
    throw "$($output.Key) does not match expected hash $($output.Value)"
  }
}
foreach ($output in $outputs.GetEnumerator()) {
  $generated = Join-Path $work $output.Key
  Copy-Item $generated (Join-Path $repository "internal\vmservice\$($output.Key)")
}
```

## Expected outputs

| File | SHA-256 |
|---|---|
| `vmservice.pb.go` | `54474D676E163D4DFA9F0F21F5324A6A717B2A11EF256C4FFF2F4367A35B7E10` |
| `vmservice_grpc.pb.go` | `05F87D56A0978F6F2B83FA800667CE7F368CBA9B4AFE05A2BEE263E963D80E1F` |

The script copies the generated files to `internal/vmservice` only when both hashes match. A source revision or toolchain update must update this document and the expected hashes in the same change.
