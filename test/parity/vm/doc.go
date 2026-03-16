//go:build windows

// Package vm validates that the v2 VM document builders produce HCS
// ComputeSystem documents equivalent to the legacy shim pipelines.
//
// Currently covers LCOW parity between:
//   - Legacy: OCI spec → oci.UpdateSpecFromOptions → oci.ProcessAnnotations →
//     oci.SpecToUVMCreateOpts → uvm.MakeLCOWDoc → *hcsschema.ComputeSystem
//   - V2: vm.Spec + runhcsopts.Options → lcow.BuildSandboxConfig →
//     *hcsschema.ComputeSystem + *SandboxOptions
//
// WCOW parity will be added in a future PR.
package vm
