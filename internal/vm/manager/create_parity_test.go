//go:build windows && lcow && openvmm_prototype

package manager

import (
	"context"
	"maps"
	"strings"
	"testing"

	runhcsoptions "github.com/Microsoft/hcsshim/cmd/containerd-shim-runhcs-v1/options"
	iannotations "github.com/Microsoft/hcsshim/internal/annotations"
	"github.com/Microsoft/hcsshim/internal/builder/vm/lcow"
	"github.com/Microsoft/hcsshim/internal/protocol/guestrequest"
	"github.com/Microsoft/hcsshim/internal/vmservice"
	shimannotations "github.com/Microsoft/hcsshim/pkg/annotations"
	vmsandbox "github.com/Microsoft/hcsshim/sandbox-spec/vm/v2"
	"google.golang.org/protobuf/proto"
)

func cloneAnnotations(src map[string]string) map[string]string {
	if src == nil {
		return map[string]string{}
	}
	return maps.Clone(src)
}

func cloneOpts(o *runhcsoptions.Options) *runhcsoptions.Options {
	return proto.Clone(o).(*runhcsoptions.Options)
}

func TestParity(t *testing.T) {
	ctx := context.Background()
	boot := newBootFiles(t)
	cfg := nativeBuilderConfig()

	compareSupported := func(t *testing.T, opts *runhcsoptions.Options, anns map[string]string) {
		t.Helper()
		hcsOpts := cloneOpts(opts)
		ovmmOpts := cloneOpts(opts)
		hcsSpec := &vmsandbox.Spec{Annotations: cloneAnnotations(anns)}
		ovmmSpec := &vmsandbox.Spec{Annotations: cloneAnnotations(anns)}

		doc, hcsSandbox, err := lcow.BuildSandboxConfig(ctx, "parity-owner", t.TempDir(), hcsOpts, hcsSpec)
		if err != nil {
			t.Fatalf("HCS BuildSandboxConfig: %v", err)
		}
		req, ovmmSandbox, reservations, err := BuildCreateVMRequestFromOptions(ctx, ovmmOpts, ovmmSpec, cfg, "parity")
		if err != nil {
			t.Fatalf("BuildCreateVMRequestFromOptions: %v", err)
		}

		mem := doc.VirtualMachine.ComputeTopology.Memory
		cpu := doc.VirtualMachine.ComputeTopology.Processor
		if req.Config.MemoryConfig.GetMemoryMb() != mem.SizeInMB {
			t.Errorf("memory HCS=%d OpenVMM=%d", mem.SizeInMB, req.Config.MemoryConfig.GetMemoryMb())
		}
		if req.Config.MemoryConfig.GetAllowOvercommit() != mem.AllowOvercommit {
			t.Errorf("overcommit HCS=%v OpenVMM=%v", mem.AllowOvercommit, req.Config.MemoryConfig.GetAllowOvercommit())
		}
		if req.Config.MemoryConfig.GetDeferredCommit() != mem.EnableDeferredCommit {
			t.Errorf("deferred HCS=%v OpenVMM=%v", mem.EnableDeferredCommit, req.Config.MemoryConfig.GetDeferredCommit())
		}
		if req.Config.MemoryConfig.GetColdDiscardHint() != mem.EnableColdDiscardHint {
			t.Errorf("cold discard HCS=%v OpenVMM=%v", mem.EnableColdDiscardHint, req.Config.MemoryConfig.GetColdDiscardHint())
		}
		if req.Config.MemoryConfig.GetLowMmioGapInMb() != mem.LowMMIOGapInMB ||
			req.Config.MemoryConfig.GetHighMmioBaseInMb() != mem.HighMMIOBaseInMB ||
			req.Config.MemoryConfig.GetHighMmioGapInMb() != mem.HighMMIOGapInMB {
			t.Errorf("MMIO mismatch HCS=%d/%d/%d OpenVMM=%d/%d/%d",
				mem.LowMMIOGapInMB, mem.HighMMIOBaseInMB, mem.HighMMIOGapInMB,
				req.Config.MemoryConfig.GetLowMmioGapInMb(), req.Config.MemoryConfig.GetHighMmioBaseInMb(), req.Config.MemoryConfig.GetHighMmioGapInMb())
		}
		if ovmmSandbox.FullyPhysicallyBacked != hcsSandbox.FullyPhysicallyBacked {
			t.Errorf("physical backing HCS=%v OpenVMM=%v", hcsSandbox.FullyPhysicallyBacked, ovmmSandbox.FullyPhysicallyBacked)
		}
		if req.Config.ProcessorConfig.GetProcessorCount() != cpu.Count {
			t.Errorf("CPU HCS=%d OpenVMM=%d", cpu.Count, req.Config.ProcessorConfig.GetProcessorCount())
		}
		if uint64(req.Config.ProcessorConfig.GetProcessorLimit()) != cpu.Limit {
			t.Errorf("CPU limit HCS=%d OpenVMM=%d", cpu.Limit, req.Config.ProcessorConfig.GetProcessorLimit())
		}
		if uint64(req.Config.ProcessorConfig.GetProcessorWeight()) != cpu.Weight {
			t.Errorf("CPU weight HCS=%d OpenVMM=%d", cpu.Weight, req.Config.ProcessorConfig.GetProcessorWeight())
		}

		if doc.VirtualMachine.Chipset.LinuxKernelDirect == nil {
			t.Fatal("HCS document is not LinuxKernelDirect; OpenVMM cannot match UEFI")
		}
		bootCfg, ok := req.Config.BootConfig.(*vmservice.VMConfig_DirectBoot)
		if !ok || bootCfg.DirectBoot == nil {
			t.Fatal("OpenVMM request is not DirectBoot")
		}
		if bootCfg.DirectBoot.KernelPath != doc.VirtualMachine.Chipset.LinuxKernelDirect.KernelFilePath {
			t.Errorf("kernel path HCS=%q OpenVMM=%q", doc.VirtualMachine.Chipset.LinuxKernelDirect.KernelFilePath, bootCfg.DirectBoot.KernelPath)
		}
		if bootCfg.DirectBoot.KernelCmdline != doc.VirtualMachine.Chipset.LinuxKernelDirect.KernelCmdLine {
			t.Errorf("kernel args HCS=%q OpenVMM=%q", doc.VirtualMachine.Chipset.LinuxKernelDirect.KernelCmdLine, bootCfg.DirectBoot.KernelCmdline)
		}

		root := doc.VirtualMachine.Devices.Scsi[guestrequest.ScsiControllerGuids[0]].Attachments["0"]
		disks := req.Config.DevicesConfig.GetScsiDisks()
		if len(disks) != 1 || disks[0].GetHostPath() != root.Path || disks[0].GetReadOnly() != root.ReadOnly {
			t.Errorf("SCSI HCS=%+v OpenVMM=%+v", root, disks)
		}
		if len(reservations) != 1 || reservations[0].Config.HostPath != root.Path {
			t.Errorf("rootfs reservations=%+v", reservations)
		}

		if ovmmSandbox.Architecture != hcsSandbox.Architecture {
			t.Errorf("architecture HCS=%q OpenVMM=%q", hcsSandbox.Architecture, ovmmSandbox.Architecture)
		}
		if ovmmSandbox.PolicyBasedRouting != hcsSandbox.PolicyBasedRouting ||
			ovmmSandbox.NoWritableFileShares != hcsSandbox.NoWritableFileShares ||
			ovmmSandbox.EnableScratchEncryption != hcsSandbox.EnableScratchEncryption {
			t.Errorf("SandboxOptions HCS=%+v OpenVMM=%+v", hcsSandbox, ovmmSandbox)
		}
		if hcsSandbox.ConfidentialConfig != nil || ovmmSandbox.ConfidentialConfig != nil {
			t.Errorf("confidential config leaked HCS=%v OpenVMM=%v", hcsSandbox.ConfidentialConfig, ovmmSandbox.ConfidentialConfig)
		}

		if _, ok := doc.VirtualMachine.Devices.ComPorts["0"]; ok {
			if req.Config.SerialConfig == nil || len(req.Config.SerialConfig.Ports) != 1 || req.Config.SerialConfig.Ports[0].GetSocketPath() != cfg.SerialSocket {
				t.Errorf("COM1 OpenVMM serial=%+v", req.Config.SerialConfig)
			}
		} else if req.Config.SerialConfig != nil {
			t.Errorf("unexpected COM1 serial %+v", req.Config.SerialConfig)
		}
		if req.Config.HvsocketConfig.GetPath() != cfg.HybridVsockBase {
			t.Errorf("hvsock path=%q", req.Config.HvsocketConfig.GetPath())
		}
	}

	t.Run("defaults", func(t *testing.T) {
		compareSupported(t, defaultNativeOpts(boot), nil)
	})

	t.Run("precedence annotation over options", func(t *testing.T) {
		opts := defaultNativeOpts(boot)
		opts.VmProcessorCount = 8
		opts.VmMemorySizeInMb = 4096
		opts.BootFilesRootPath = boot
		compareSupported(t, opts, map[string]string{
			shimannotations.ProcessorCount: "2",
			shimannotations.MemorySizeInMB: "2048",
		})
	})

	t.Run("kernel CPU memory SCSI COM1 hvsock SandboxOptions", func(t *testing.T) {
		opts := defaultNativeOpts(boot)
		opts.LogLevel = "debug"
		scrub := true
		opts.ScrubLogs = &scrub
		compareSupported(t, opts, map[string]string{
			shimannotations.ProcessorCount:               "2",
			shimannotations.MemorySizeInMB:               "2048",
			shimannotations.AllowOvercommit:              "true",
			shimannotations.KernelBootOptions:            "foo=bar",
			shimannotations.DisableLCOWTimeSyncService:   "true",
			shimannotations.ContainerProcessDumpLocation: "/tmp/dump",
			iannotations.ExtraLCOWExecArgs:               "-extra",
			iannotations.WritableOverlayDirs:             "true",
			shimannotations.VPCIEnabled:                  "false",
			iannotations.UVMConsolePipe:                  `\\.\pipe\console`,
			iannotations.NetworkingPolicyBasedRouting:    "true",
			shimannotations.DisableWritableFileShares:    "true",
			shimannotations.LCOWEncryptedScratchDisk:     "true",
			shimannotations.PreferredRootFSType:          "vhd",
		})
	})

	t.Run("caller-map mutation", func(t *testing.T) {
		opts := defaultNativeOpts(boot)
		opts.DefaultContainerAnnotations = map[string]string{
			shimannotations.ProcessorCount: "2",
			shimannotations.MemorySizeInMB: "2048",
		}
		hcsSpec := &vmsandbox.Spec{Annotations: map[string]string{shimannotations.ProcessorCount: "1"}}
		ovmmSpec := &vmsandbox.Spec{Annotations: map[string]string{shimannotations.ProcessorCount: "1"}}
		if _, _, err := lcow.BuildSandboxConfig(ctx, "o", t.TempDir(), cloneOpts(opts), hcsSpec); err != nil {
			t.Fatalf("HCS: %v", err)
		}
		if _, _, _, err := BuildCreateVMRequestFromOptions(ctx, cloneOpts(opts), ovmmSpec, cfg, "m"); err != nil {
			t.Fatalf("OpenVMM: %v", err)
		}
		if hcsSpec.Annotations[shimannotations.ProcessorCount] != "1" {
			t.Errorf("existing CPU annotation mutated: %q", hcsSpec.Annotations[shimannotations.ProcessorCount])
		}
		if ovmmSpec.Annotations[shimannotations.ProcessorCount] != "1" {
			t.Errorf("OpenVMM existing CPU annotation mutated: %q", ovmmSpec.Annotations[shimannotations.ProcessorCount])
		}
		if hcsSpec.Annotations[shimannotations.MemorySizeInMB] != "2048" || ovmmSpec.Annotations[shimannotations.MemorySizeInMB] != "2048" {
			t.Errorf("default memory not inserted HCS=%q OpenVMM=%q", hcsSpec.Annotations[shimannotations.MemorySizeInMB], ovmmSpec.Annotations[shimannotations.MemorySizeInMB])
		}
	})

	t.Run("conflict deferred+physically backed", func(t *testing.T) {
		opts := defaultNativeOpts(boot)
		anns := map[string]string{
			shimannotations.FullyPhysicallyBacked: "true",
			shimannotations.EnableDeferredCommit:  "true",
		}
		_, _, err := lcow.BuildSandboxConfig(ctx, "o", t.TempDir(), cloneOpts(opts), &vmsandbox.Spec{Annotations: cloneAnnotations(anns)})
		if err == nil || !strings.Contains(err.Error(), "enable_deferred_commit") {
			t.Fatalf("HCS conflict err=%v", err)
		}
		// OpenVMM rejects both annotations outright, so it fails earlier than the HCS conflict.
		_, _, _, err = BuildCreateVMRequestFromOptions(ctx, cloneOpts(opts), &vmsandbox.Spec{Annotations: cloneAnnotations(anns)}, cfg, "c")
		if err == nil || !strings.Contains(err.Error(), shimannotations.EnableDeferredCommit) {
			t.Fatalf("OpenVMM conflict err=%v", err)
		}
	})

	t.Run("conflict CPUGroupID+ResourcePartitionID", func(t *testing.T) {
		opts := defaultNativeOpts(boot)
		anns := map[string]string{
			shimannotations.CPUGroupID:          "group",
			shimannotations.ResourcePartitionID: "11111111-1111-1111-1111-111111111111",
		}
		_, _, err := lcow.BuildSandboxConfig(ctx, "o", t.TempDir(), cloneOpts(opts), &vmsandbox.Spec{Annotations: cloneAnnotations(anns)})
		if err == nil {
			t.Fatal("HCS expected conflict")
		}
		_, _, _, err = BuildCreateVMRequestFromOptions(ctx, cloneOpts(opts), &vmsandbox.Spec{Annotations: cloneAnnotations(anns)}, cfg, "c")
		if err == nil || !strings.Contains(err.Error(), shimannotations.CPUGroupID) {
			t.Fatalf("OpenVMM conflict err=%v", err)
		}
	})
}
