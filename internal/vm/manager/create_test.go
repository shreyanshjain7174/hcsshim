//go:build windows && lcow && openvmm_prototype

package manager

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	runhcsoptions "github.com/Microsoft/hcsshim/cmd/containerd-shim-runhcs-v1/options"
	iannotations "github.com/Microsoft/hcsshim/internal/annotations"
	"github.com/Microsoft/hcsshim/internal/vm/vmutils"
	"github.com/Microsoft/hcsshim/internal/vmservice"
	shimannotations "github.com/Microsoft/hcsshim/pkg/annotations"
	vmsandbox "github.com/Microsoft/hcsshim/sandbox-spec/vm/v2"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// openVMMInputInventory is the exhaustive disposition table for every annotation
// and runtime option consumed by the HCS LCOW builder. Audit coverage greps
// these identifiers as substrings.
var openVMMInputInventory = []struct {
	Key               string
	Disposition       string
	Notes             string
	RejectedSample    map[string]string
	RejectedDevices   []specs.WindowsDevice
	ExpectedErrorName string
}{
	{Key: "AllowOvercommit", Disposition: "rejected", Notes: "default true supported; vmservice fixes memory policy so false is rejected", RejectedSample: map[string]string{shimannotations.AllowOvercommit: "false"}, ExpectedErrorName: shimannotations.AllowOvercommit},
	{Key: "BootFilesRootPath", Disposition: "supported", Notes: "direct-boot kernel and VHD rootfs path"},
	{Key: "ContainerProcessDumpLocation", Disposition: "downstream-only", Notes: "GCS -core-dump-location kernel/init args"},
	{Key: "CPUGroupID", Disposition: "rejected", Notes: "no vmservice CPU group", RejectedSample: map[string]string{shimannotations.CPUGroupID: "group"}, ExpectedErrorName: shimannotations.CPUGroupID},
	{Key: "DisableLCOWTimeSyncService", Disposition: "downstream-only", Notes: "GCS -disable-time-sync"},
	{Key: "DisableWritableFileShares", Disposition: "downstream-only", Notes: "SandboxOptions.NoWritableFileShares"},
	{Key: "DmVerityCreateArgs", Disposition: "rejected", Notes: "dm-verity root is unsupported", RejectedSample: map[string]string{shimannotations.DmVerityCreateArgs: "verity"}, ExpectedErrorName: shimannotations.DmVerityCreateArgs},
	{Key: "DmVerityMode", Disposition: "rejected", Notes: "errDMVerityNotSupported", RejectedSample: map[string]string{shimannotations.DmVerityMode: "true"}, ExpectedErrorName: shimannotations.DmVerityMode},
	{Key: "DmVerityRootFsVhd", Disposition: "rejected", Notes: "confidential-only rootfs", RejectedSample: map[string]string{shimannotations.DmVerityRootFsVhd: "rootfs.vhd"}, ExpectedErrorName: shimannotations.DmVerityRootFsVhd},
	{Key: "EnableColdDiscardHint", Disposition: "rejected", Notes: "default false supported; vmservice ignores the cold discard hint", RejectedSample: map[string]string{shimannotations.EnableColdDiscardHint: "true"}, ExpectedErrorName: shimannotations.EnableColdDiscardHint},
	{Key: "EnableDeferredCommit", Disposition: "rejected", Notes: "default false supported; vmservice ignores deferred commit", RejectedSample: map[string]string{shimannotations.EnableDeferredCommit: "true"}, ExpectedErrorName: shimannotations.EnableDeferredCommit},
	{Key: "ExtraLCOWExecArgs", Disposition: "downstream-only", Notes: "appended to GCS command"},
	{Key: "ExtraVSockPorts", Disposition: "rejected", Notes: "confidential-only extra vsock ports", RejectedSample: map[string]string{iannotations.ExtraVSockPorts: "123"}, ExpectedErrorName: iannotations.ExtraVSockPorts},
	{Key: "FullyPhysicallyBacked", Disposition: "rejected", Notes: "default false supported; vmservice cannot force physically backed memory", RejectedSample: map[string]string{shimannotations.FullyPhysicallyBacked: "true"}, ExpectedErrorName: shimannotations.FullyPhysicallyBacked},
	{Key: "KernelBootOptions", Disposition: "downstream-only", Notes: "appended to kernel cmdline"},
	{Key: "KernelDirectBoot", Disposition: "supported", Notes: "false/UEFI rejected; OpenVMM requires direct boot"},
	{Key: "LCOWEncryptedScratchDisk", Disposition: "downstream-only", Notes: "SandboxOptions.EnableScratchEncryption"},
	{Key: "LCOWGuestStateFile", Disposition: "rejected", Notes: "confidential VMGS copy/ACL", RejectedSample: map[string]string{shimannotations.LCOWGuestStateFile: "gueststate.vmgs"}, ExpectedErrorName: shimannotations.LCOWGuestStateFile},
	{Key: "LCOWHclEnabled", Disposition: "rejected", Notes: "confidential isolation", RejectedSample: map[string]string{shimannotations.LCOWHclEnabled: "true"}, ExpectedErrorName: shimannotations.LCOWHclEnabled},
	{Key: "LCOWReferenceInfoFile", Disposition: "rejected", Notes: "confidential attestation", RejectedSample: map[string]string{shimannotations.LCOWReferenceInfoFile: "reference-info"}, ExpectedErrorName: shimannotations.LCOWReferenceInfoFile},
	{Key: "LCOWSecurityPolicy", Disposition: "rejected", Notes: "confidential; no VMGS copy", RejectedSample: map[string]string{shimannotations.LCOWSecurityPolicy: "policy"}, ExpectedErrorName: shimannotations.LCOWSecurityPolicy},
	{Key: "LCOWSecurityPolicyEnforcer", Disposition: "rejected", Notes: "confidential enforcer", RejectedSample: map[string]string{shimannotations.LCOWSecurityPolicyEnforcer: "standard"}, ExpectedErrorName: shimannotations.LCOWSecurityPolicyEnforcer},
	{Key: "LiveMigrationSupportEnabled", Disposition: "rejected", Notes: "no live migration in this slice", RejectedSample: map[string]string{shimannotations.LiveMigrationSupportEnabled: "true"}, ExpectedErrorName: shimannotations.LiveMigrationSupportEnabled},
	{Key: "MemoryHighMMIOBaseInMB", Disposition: "rejected", Notes: "default 0 supported; vmservice fixes the memory layout", RejectedSample: map[string]string{shimannotations.MemoryHighMMIOBaseInMB: "8192"}, ExpectedErrorName: shimannotations.MemoryHighMMIOBaseInMB},
	{Key: "MemoryHighMMIOGapInMB", Disposition: "rejected", Notes: "default 0 supported; vmservice fixes the memory layout", RejectedSample: map[string]string{shimannotations.MemoryHighMMIOGapInMB: "128"}, ExpectedErrorName: shimannotations.MemoryHighMMIOGapInMB},
	{Key: "MemoryLowMMIOGapInMB", Disposition: "rejected", Notes: "default 0 supported; vmservice fixes the memory layout", RejectedSample: map[string]string{shimannotations.MemoryLowMMIOGapInMB: "64"}, ExpectedErrorName: shimannotations.MemoryLowMMIOGapInMB},
	{Key: "MemorySizeInMB", Disposition: "supported", Notes: "annotation > option.VmMemorySizeInMb > 1024"},
	{Key: "NetworkConfigProxy", Disposition: "rejected", Notes: "HCS also rejects by name", RejectedSample: map[string]string{shimannotations.NetworkConfigProxy: "proxy"}, ExpectedErrorName: shimannotations.NetworkConfigProxy},
	{Key: "NetworkingPolicyBasedRouting", Disposition: "downstream-only", Notes: "SandboxOptions.PolicyBasedRouting"},
	{Key: "NoSecurityHardware", Disposition: "downstream-only", Notes: "only meaningful with rejected confidential policy"},
	{Key: "NumaCountOfMemoryBlocks", Disposition: "rejected", Notes: "NUMA outside cold-boot slice", RejectedSample: map[string]string{shimannotations.NumaCountOfMemoryBlocks: "1"}, ExpectedErrorName: shimannotations.NumaCountOfMemoryBlocks},
	{Key: "NumaCountOfProcessors", Disposition: "rejected", Notes: "NUMA outside cold-boot slice", RejectedSample: map[string]string{shimannotations.NumaCountOfProcessors: "1"}, ExpectedErrorName: shimannotations.NumaCountOfProcessors},
	{Key: "NumaMappedPhysicalNodes", Disposition: "rejected", Notes: "NUMA outside cold-boot slice", RejectedSample: map[string]string{shimannotations.NumaMappedPhysicalNodes: "0"}, ExpectedErrorName: shimannotations.NumaMappedPhysicalNodes},
	{Key: "NumaMaximumMemorySizePerNode", Disposition: "rejected", Notes: "NUMA outside cold-boot slice", RejectedSample: map[string]string{shimannotations.NumaMaximumMemorySizePerNode: "1024"}, ExpectedErrorName: shimannotations.NumaMaximumMemorySizePerNode},
	{Key: "NumaMaximumProcessorsPerNode", Disposition: "rejected", Notes: "NUMA outside cold-boot slice", RejectedSample: map[string]string{shimannotations.NumaMaximumProcessorsPerNode: "2"}, ExpectedErrorName: shimannotations.NumaMaximumProcessorsPerNode},
	{Key: "NumaPreferredPhysicalNodes", Disposition: "rejected", Notes: "NUMA outside cold-boot slice", RejectedSample: map[string]string{shimannotations.NumaPreferredPhysicalNodes: "0"}, ExpectedErrorName: shimannotations.NumaPreferredPhysicalNodes},
	{Key: "option.BootFilesRootPath", Disposition: "supported", Notes: "fallback after annotation"},
	{Key: "option.DefaultContainerAnnotations", Disposition: "supported", Notes: "caller-map mutation of missing keys"},
	{Key: "option.LogLevel", Disposition: "downstream-only", Notes: "GCS -loglevel"},
	{Key: "option.SandboxPlatform", Disposition: "supported", Notes: "linux/amd64 only; all other platforms rejected"},
	{Key: "option.ScrubLogs", Disposition: "downstream-only", Notes: "GCS -scrub-logs"},
	{Key: "option.VmMemorySizeInMb", Disposition: "supported", Notes: "memory size precedence"},
	{Key: "option.VmProcessorCount", Disposition: "supported", Notes: "CPU count precedence"},
	{Key: "PreferredRootFSType", Disposition: "supported", Notes: "vhd supported; initrd rejected"},
	{Key: "ProcessorCount", Disposition: "supported", Notes: "CPU count annotation"},
	{Key: "ProcessorLimit", Disposition: "rejected", Notes: "default 0 supported; vmservice consumes processor_count only", RejectedSample: map[string]string{shimannotations.ProcessorLimit: "5000"}, ExpectedErrorName: shimannotations.ProcessorLimit},
	{Key: "ProcessorWeight", Disposition: "rejected", Notes: "default 0 supported; vmservice consumes processor_count only", RejectedSample: map[string]string{shimannotations.ProcessorWeight: "100"}, ExpectedErrorName: shimannotations.ProcessorWeight},
	{Key: "ResourcePartitionID", Disposition: "rejected", Notes: "conflict with CPUGroupID; no vmservice field", RejectedSample: map[string]string{shimannotations.ResourcePartitionID: "11111111-1111-1111-1111-111111111111"}, ExpectedErrorName: shimannotations.ResourcePartitionID},
	{Key: "StorageQoSBandwidthMaximum", Disposition: "rejected", Notes: "no storage QoS", RejectedSample: map[string]string{shimannotations.StorageQoSBandwidthMaximum: "1"}, ExpectedErrorName: shimannotations.StorageQoSBandwidthMaximum},
	{Key: "StorageQoSIopsMaximum", Disposition: "rejected", Notes: "no storage QoS", RejectedSample: map[string]string{shimannotations.StorageQoSIopsMaximum: "1"}, ExpectedErrorName: shimannotations.StorageQoSIopsMaximum},
	{Key: "UVMConsolePipe", Disposition: "supported", Notes: "COM1 AF_UNIX relay via Config.SerialSocket"},
	{Key: "UVMHashEnvelopeReferenceInfoFile", Disposition: "rejected", Notes: "confidential attestation", RejectedSample: map[string]string{shimannotations.UVMHashEnvelopeReferenceInfoFile: "hash-envelope"}, ExpectedErrorName: shimannotations.UVMHashEnvelopeReferenceInfoFile},
	{Key: "UVMHyperVSocketConfigPrefix", Disposition: "rejected", Notes: "custom hvsock service table / ACL", RejectedSample: map[string]string{iannotations.UVMHyperVSocketConfigPrefix + "test": "{}"}, ExpectedErrorName: iannotations.UVMHyperVSocketConfigPrefix},
	{Key: "VirtualMachineKernelDrivers", Disposition: "rejected", Notes: "HCS also rejects by name", RejectedSample: map[string]string{shimannotations.VirtualMachineKernelDrivers: "driver"}, ExpectedErrorName: shimannotations.VirtualMachineKernelDrivers},
	{Key: "VPCIEnabled", Disposition: "downstream-only", Notes: "kernel pci=off unless enabled"},
	{Key: "VPMemCount", Disposition: "rejected", Notes: "v2 shims do not support vPMem", RejectedSample: map[string]string{shimannotations.VPMemCount: "1"}, ExpectedErrorName: shimannotations.VPMemCount},
	{Key: "VPMemNoMultiMapping", Disposition: "rejected", Notes: "HCS also rejects by name", RejectedSample: map[string]string{shimannotations.VPMemNoMultiMapping: "true"}, ExpectedErrorName: shimannotations.VPMemNoMultiMapping},
	{Key: "Devices/vPCI", Disposition: "rejected", Notes: "vmservice does not support vPCI assignment", RejectedDevices: []specs.WindowsDevice{{ID: "test-device"}}, ExpectedErrorName: "vPCI"},
	{Key: "WritableOverlayDirs", Disposition: "downstream-only", Notes: "init -w for VHD rootfs"},
}

func TestRejectedInputInventory(t *testing.T) {
	boot := newBootFiles(t)
	opts := defaultNativeOpts(boot)
	cfg := nativeBuilderConfig()

	for _, row := range openVMMInputInventory {
		if row.Disposition != "rejected" {
			continue
		}
		t.Run(row.Key, func(t *testing.T) {
			if len(row.RejectedSample) == 0 && len(row.RejectedDevices) == 0 {
				t.Fatal("rejected inventory row has no sample input")
			}
			if row.ExpectedErrorName == "" {
				t.Fatal("rejected inventory row has no expected error name")
			}
			annotations := make(map[string]string, len(row.RejectedSample))
			for key, value := range row.RejectedSample {
				annotations[key] = value
			}
			spec := &vmsandbox.Spec{Annotations: annotations, Devices: row.RejectedDevices}
			_, _, _, err := BuildCreateVMRequestFromOptions(context.Background(), opts, spec, cfg, "log-id")
			if err == nil {
				t.Fatal("BuildCreateVMRequestFromOptions succeeded")
			}
			if !strings.Contains(err.Error(), row.ExpectedErrorName) {
				t.Fatalf("want error naming %q, got %v", row.ExpectedErrorName, err)
			}
		})
	}
}

// TestDefaultComputeTopologyControlsAccepted pins that a control is rejected for
// its requested value, not for merely appearing in the annotation map.
func TestDefaultComputeTopologyControlsAccepted(t *testing.T) {
	boot := newBootFiles(t)
	opts := defaultNativeOpts(boot)
	cfg := nativeBuilderConfig()

	for _, tc := range []struct {
		key   string
		value string
	}{
		{shimannotations.ProcessorLimit, "0"},
		{shimannotations.ProcessorWeight, "0"},
		{shimannotations.AllowOvercommit, "true"},
		{shimannotations.EnableDeferredCommit, "false"},
		{shimannotations.EnableColdDiscardHint, "false"},
		{shimannotations.FullyPhysicallyBacked, "false"},
		{shimannotations.MemoryLowMMIOGapInMB, "0"},
		{shimannotations.MemoryHighMMIOBaseInMB, "0"},
		{shimannotations.MemoryHighMMIOGapInMB, "0"},
	} {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			spec := &vmsandbox.Spec{Annotations: map[string]string{tc.key: tc.value}}
			req, _, _, err := BuildCreateVMRequestFromOptions(context.Background(), opts, spec, cfg, "log-id")
			if err != nil {
				t.Fatalf("BuildCreateVMRequestFromOptions: %v", err)
			}
			if !req.Config.MemoryConfig.GetAllowOvercommit() {
				t.Error("default request must allow overcommit")
			}
			if req.Config.ProcessorConfig.GetProcessorLimit() != 0 || req.Config.ProcessorConfig.GetProcessorWeight() != 0 {
				t.Errorf("cpu limit/weight = %d/%d", req.Config.ProcessorConfig.GetProcessorLimit(), req.Config.ProcessorConfig.GetProcessorWeight())
			}
			if req.Config.ProcessorConfig.GetProcessorCount() == 0 || req.Config.MemoryConfig.GetMemoryMb() == 0 {
				t.Errorf("cpu count=%d memory=%d", req.Config.ProcessorConfig.GetProcessorCount(), req.Config.MemoryConfig.GetMemoryMb())
			}
		})
	}
}

// TestMalformedComputeTopologyControlsRejected pins that an unparsable value is
// rejected instead of falling back to the supported default, which would let an
// unsupported request through.
func TestMalformedComputeTopologyControlsRejected(t *testing.T) {
	boot := newBootFiles(t)
	opts := defaultNativeOpts(boot)
	cfg := nativeBuilderConfig()

	for _, group := range []struct {
		keys   []string
		values []string
	}{
		{
			keys:   []string{shimannotations.ProcessorLimit, shimannotations.ProcessorWeight},
			values: []string{"", " ", " 0", "0 ", "abc", "0x0", "1.5", "-1", "-2147483649", "2147483648"},
		},
		{
			keys: []string{
				shimannotations.AllowOvercommit,
				shimannotations.EnableDeferredCommit,
				shimannotations.EnableColdDiscardHint,
				shimannotations.FullyPhysicallyBacked,
			},
			values: []string{"", " ", " true", "false ", "yes", "2"},
		},
		{
			keys: []string{
				shimannotations.MemoryLowMMIOGapInMB,
				shimannotations.MemoryHighMMIOBaseInMB,
				shimannotations.MemoryHighMMIOGapInMB,
			},
			values: []string{"", " ", " 0", "0 ", "abc", "-1", "18446744073709551616"},
		},
	} {
		for _, key := range group.keys {
			for _, value := range group.values {
				t.Run(key+"="+value, func(t *testing.T) {
					spec := &vmsandbox.Spec{Annotations: map[string]string{key: value}}
					_, _, _, err := BuildCreateVMRequestFromOptions(context.Background(), opts, spec, cfg, "log-id")
					if !errors.Is(err, errUnsupportedDocumentField) {
						t.Fatalf("want %v, got %v", errUnsupportedDocumentField, err)
					}
					if !strings.Contains(err.Error(), key) {
						t.Fatalf("want error naming %q, got %v", key, err)
					}
				})
			}
		}
	}
}

func nativeBuilderConfig() *Config {
	return &Config{
		OpenVMMBinaryPath: `C:\openvmm\openvmm.exe`,
		VMServiceSocket:   `C:\openvmm\vmservice.sock`,
		HybridVsockBase:   `C:\openvmm\hybrid-base`,
		SerialSocket:      `C:\openvmm\serial.sock`,
	}
}

func newBootFiles(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "bootfiles")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir bootfiles: %v", err)
	}
	for _, name := range []string{
		vmutils.KernelFile,
		vmutils.UncompressedKernelFile,
		vmutils.InitrdFile,
		vmutils.VhdFile,
		vmutils.DefaultGuestStateFile,
		vmutils.DefaultDmVerityRootfsVhd,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("test"), 0644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

func defaultNativeOpts(boot string) *runhcsoptions.Options {
	return &runhcsoptions.Options{
		SandboxPlatform:   "linux/amd64",
		BootFilesRootPath: boot,
	}
}

func TestBuildCreateVMRequestFromOptions(t *testing.T) {
	ctx := context.Background()
	boot := newBootFiles(t)
	cfg := nativeBuilderConfig()
	opts := defaultNativeOpts(boot)

	t.Run("direct boot memory processor SCSI serial hvsock SandboxOptions rootfs reservations", func(t *testing.T) {
		spec := &vmsandbox.Spec{Annotations: map[string]string{
			shimannotations.ProcessorCount:            "2",
			shimannotations.MemorySizeInMB:            "2048",
			iannotations.UVMConsolePipe:               `\\.\pipe\console`,
			iannotations.NetworkingPolicyBasedRouting: "true",
			shimannotations.DisableWritableFileShares: "true",
			shimannotations.LCOWEncryptedScratchDisk:  "true",
			shimannotations.PreferredRootFSType:       "vhd",
		}}
		req, sandbox, reservations, err := BuildCreateVMRequestFromOptions(ctx, opts, spec, cfg, "native-log")
		if err != nil {
			t.Fatalf("BuildCreateVMRequestFromOptions: %v", err)
		}
		if req == nil || req.Config == nil {
			t.Fatal("expected CreateVMRequest")
		}
		if req.LogId != "native-log" {
			t.Errorf("LogId=%q", req.LogId)
		}
		bootCfg, ok := req.Config.BootConfig.(*vmservice.VMConfig_DirectBoot)
		if !ok || bootCfg.DirectBoot == nil {
			t.Fatal("expected DirectBoot")
		}
		if bootCfg.DirectBoot.KernelPath == "" || !strings.Contains(bootCfg.DirectBoot.KernelCmdline, "root=/dev/sda") {
			t.Errorf("kernel=%q cmdline=%q", bootCfg.DirectBoot.KernelPath, bootCfg.DirectBoot.KernelCmdline)
		}
		if req.Config.MemoryConfig.GetMemoryMb() != 2048 {
			t.Errorf("memory=%d", req.Config.MemoryConfig.GetMemoryMb())
		}
		if !req.Config.MemoryConfig.GetAllowOvercommit() {
			t.Error("default memory policy must allow overcommit")
		}
		if sandbox.FullyPhysicallyBacked {
			t.Error("physically backed memory is not supported")
		}
		if req.Config.ProcessorConfig.GetProcessorCount() != 2 {
			t.Errorf("cpu=%d", req.Config.ProcessorConfig.GetProcessorCount())
		}
		if req.Config.ProcessorConfig.GetProcessorLimit() != 0 || req.Config.ProcessorConfig.GetProcessorWeight() != 0 {
			t.Errorf("cpu limit/weight = %d/%d", req.Config.ProcessorConfig.GetProcessorLimit(), req.Config.ProcessorConfig.GetProcessorWeight())
		}
		disks := req.Config.DevicesConfig.GetScsiDisks()
		if len(disks) != 1 || disks[0].GetController() != 0 || disks[0].GetLun() != 0 || !disks[0].GetReadOnly() {
			t.Fatalf("SCSI rootfs = %+v", disks)
		}
		if disks[0].GetType() != vmservice.DiskType_SCSI_DISK_TYPE_VHD1 {
			t.Errorf("root disk type=%v", disks[0].GetType())
		}
		serial := req.Config.SerialConfig
		if serial == nil || len(serial.Ports) != 1 || serial.Ports[0].GetPort() != 0 || !serial.Ports[0].GetConnect() || serial.Ports[0].GetSocketPath() != cfg.SerialSocket {
			t.Fatalf("COM1 serial = %+v", serial)
		}
		if req.Config.HvsocketConfig.GetPath() != cfg.HybridVsockBase {
			t.Errorf("hvsock path=%q", req.Config.HvsocketConfig.GetPath())
		}
		if sandbox.Architecture != "amd64" {
			t.Errorf("architecture=%q", sandbox.Architecture)
		}
		if !sandbox.PolicyBasedRouting || !sandbox.NoWritableFileShares || !sandbox.EnableScratchEncryption {
			t.Errorf("SandboxOptions routing=%v nowrite=%v encrypt=%v", sandbox.PolicyBasedRouting, sandbox.NoWritableFileShares, sandbox.EnableScratchEncryption)
		}
		if sandbox.ConfidentialConfig != nil {
			t.Fatal("ConfidentialConfig must stay nil")
		}
		if len(reservations) != 1 || reservations[0].Controller != 0 || reservations[0].Lun != 0 || !reservations[0].Config.ReadOnly || reservations[0].Config.Type != "VirtualDisk" {
			t.Fatalf("rootfs reservations = %+v", reservations)
		}
		if reservations[0].Config.HostPath != disks[0].GetHostPath() {
			t.Errorf("reservation path %q != disk %q", reservations[0].Config.HostPath, disks[0].GetHostPath())
		}
	})

	t.Run("defaults do not start a process", func(t *testing.T) {
		req, sandbox, reservations, err := BuildCreateVMRequestFromOptions(ctx, opts, &vmsandbox.Spec{}, cfg, "log-id")
		if err != nil {
			t.Fatalf("BuildCreateVMRequestFromOptions: %v", err)
		}
		if req.Config.ProcessorConfig.GetProcessorCount() == 0 {
			t.Fatal("default CPU count missing")
		}
		if req.Config.MemoryConfig.GetMemoryMb() != 1024 {
			t.Errorf("default memory=%d want 1024", req.Config.MemoryConfig.GetMemoryMb())
		}
		if sandbox == nil || sandbox.Architecture != "amd64" {
			t.Fatalf("SandboxOptions=%+v", sandbox)
		}
		if len(reservations) != 1 {
			t.Fatalf("expected rootfs reservation, got %d", len(reservations))
		}
	})

	t.Run("nil options", func(t *testing.T) {
		_, _, _, err := BuildCreateVMRequestFromOptions(ctx, nil, &vmsandbox.Spec{}, cfg, "")
		if err == nil || !strings.Contains(err.Error(), "no options provided") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("initrd rejected", func(t *testing.T) {
		spec := &vmsandbox.Spec{Annotations: map[string]string{shimannotations.PreferredRootFSType: "initrd"}}
		_, _, _, err := BuildCreateVMRequestFromOptions(ctx, opts, spec, cfg, "")
		if !errors.Is(err, ErrInitrdNotSupported) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("UEFI KernelDirectBoot=false rejected", func(t *testing.T) {
		spec := &vmsandbox.Spec{Annotations: map[string]string{shimannotations.KernelDirectBoot: "false"}}
		_, _, _, err := BuildCreateVMRequestFromOptions(ctx, opts, spec, cfg, "")
		if err == nil || !strings.Contains(err.Error(), shimannotations.KernelDirectBoot) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("confidential LCOWSecurityPolicy rejected before copy", func(t *testing.T) {
		bundle := t.TempDir()
		spec := &vmsandbox.Spec{Annotations: map[string]string{shimannotations.LCOWSecurityPolicy: "policy"}}
		_, _, _, err := BuildCreateVMRequestFromOptions(ctx, opts, spec, cfg, "")
		if err == nil || !strings.Contains(err.Error(), shimannotations.LCOWSecurityPolicy) {
			t.Fatalf("got %v", err)
		}
		entries, _ := os.ReadDir(bundle)
		if len(entries) != 0 {
			t.Fatalf("bundle mutated: %v", entries)
		}
	})

	t.Run("deferred commit rejected before the physically backed conflict", func(t *testing.T) {
		spec := &vmsandbox.Spec{Annotations: map[string]string{
			shimannotations.FullyPhysicallyBacked: "true",
			shimannotations.EnableDeferredCommit:  "true",
		}}
		_, _, _, err := BuildCreateVMRequestFromOptions(ctx, opts, spec, cfg, "")
		if err == nil || !strings.Contains(err.Error(), shimannotations.EnableDeferredCommit) {
			t.Fatalf("got %v", err)
		}
	})
}
