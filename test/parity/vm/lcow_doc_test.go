//go:build windows

package vm

import (
	"context"
	"maps"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/opencontainers/runtime-spec/specs-go"

	runhcsopts "github.com/Microsoft/hcsshim/cmd/containerd-shim-runhcs-v1/options"
	iannotations "github.com/Microsoft/hcsshim/internal/annotations"
	shimannotations "github.com/Microsoft/hcsshim/pkg/annotations"
	vm "github.com/Microsoft/hcsshim/sandbox-spec/vm/v2"
)

// TestLCOWDocumentParity feeds identical annotations, devices, and shim options
// to both the legacy and v2 LCOW pipelines and verifies the resulting HCS
// ComputeSystem documents are structurally identical.
func TestLCOWDocumentParity(t *testing.T) {
	bootDir := setupBootFiles(t)

	tests := []struct {
		name        string
		annotations map[string]string
		devices     []specs.WindowsDevice
		shimOpts    func() *runhcsopts.Options
	}{
		{
			name: "CPU + memory + QoS + MMIO + CPUGroup",
			annotations: map[string]string{
				shimannotations.ProcessorCount:             "2",
				shimannotations.ProcessorLimit:             "50000",
				shimannotations.ProcessorWeight:            "500",
				shimannotations.CPUGroupID:                 "test-cpu-group-id-123",
				shimannotations.MemorySizeInMB:             "2048",
				shimannotations.AllowOvercommit:            "true",
				shimannotations.EnableColdDiscardHint:      "true",
				shimannotations.MemoryLowMMIOGapInMB:       "256",
				shimannotations.MemoryHighMMIOBaseInMB:     "1024",
				shimannotations.MemoryHighMMIOGapInMB:      "512",
				shimannotations.StorageQoSIopsMaximum:      "5000",
				shimannotations.StorageQoSBandwidthMaximum: "1000000",
			},
		},
		{
			name: "memory + MMIO + QoS (overcommit off)",
			annotations: map[string]string{
				shimannotations.MemorySizeInMB:             "4096",
				shimannotations.AllowOvercommit:            "false",
				shimannotations.MemoryLowMMIOGapInMB:       "256",
				shimannotations.MemoryHighMMIOBaseInMB:     "1024",
				shimannotations.MemoryHighMMIOGapInMB:      "512",
				shimannotations.CPUGroupID:                 "test-cpu-group-id-456",
				shimannotations.StorageQoSIopsMaximum:      "3000",
				shimannotations.StorageQoSBandwidthMaximum: "500000",
			},
		},
		{
			name: "shim options CPU/memory + annotation QoS",
			shimOpts: func() *runhcsopts.Options {
				return &runhcsopts.Options{
					SandboxPlatform:   "linux/amd64",
					BootFilesRootPath: bootDir,
					VmProcessorCount:  2,
					VmMemorySizeInMb:  2048,
				}
			},
			annotations: map[string]string{
				shimannotations.CPUGroupID:                 "test-cpu-group-id-789",
				shimannotations.StorageQoSIopsMaximum:      "5000",
				shimannotations.StorageQoSBandwidthMaximum: "1000000",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			shimOpts := &runhcsopts.Options{
				SandboxPlatform:   "linux/amd64",
				BootFilesRootPath: bootDir,
			}
			if tc.shimOpts != nil {
				shimOpts = tc.shimOpts()
			}

			// Legacy path: OCI spec with annotations and devices.
			legacySpec := specs.Spec{
				Annotations: maps.Clone(tc.annotations),
				Linux:       &specs.Linux{},
				Windows: &specs.Windows{
					HyperV:  &specs.WindowsHyperV{},
					Devices: tc.devices,
				},
			}
			if legacySpec.Annotations == nil {
				legacySpec.Annotations = map[string]string{}
			}
			legacyDoc, legacyOpts, err := buildLegacyLCOWDocument(ctx, legacySpec, shimOpts, bootDir)
			if err != nil {
				t.Fatalf("failed to build legacy LCOW document: %v", err)
			}

			// V2 path: vm.Spec with the same annotations and devices.
			v2Spec := &vm.Spec{
				Annotations: maps.Clone(tc.annotations),
				Devices:     tc.devices,
			}
			if v2Spec.Annotations == nil {
				v2Spec.Annotations = map[string]string{}
			}
			v2Doc, sandboxOpts, err := buildV2LCOWDocument(ctx, shimOpts, v2Spec, bootDir)
			if err != nil {
				t.Fatalf("failed to build v2 LCOW document: %v", err)
			}

			if testing.Verbose() {
				t.Logf("Legacy options: %+v", legacyOpts)
				t.Logf("V2 sandbox options: %+v", sandboxOpts)
			}

			if diff := cmp.Diff(legacyDoc, v2Doc); diff != "" {
				// Check if the only difference is the legacy kernel cmdline
				// leading space quirk. If so, warn instead of failing.
				if isOnlyKernelCmdLineWhitespaceDiff(legacyDoc, v2Doc) {
					t.Logf("WARNING: kernel cmdline has leading whitespace difference (legacy quirk): %s", diff)
				} else {
					t.Logf("Legacy document:\n%s", jsonToString(legacyDoc))
					t.Logf("V2 document:\n%s", jsonToString(v2Doc))
					t.Errorf("LCOW HCS document mismatch (-legacy +v2):\n%s", diff)
				}
			}
		})
	}
}

// TestLCOWSandboxOptionsFieldParity verifies that configuration fields carried
// by the legacy OptionsLCOW have matching values in the v2 SandboxOptions when
// both pipelines receive the same default inputs.
func TestLCOWSandboxOptionsFieldParity(t *testing.T) {
	bootDir := setupBootFiles(t)

	tests := []struct {
		name        string
		annotations map[string]string
	}{
		{
			name: "default config",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			shimOpts := &runhcsopts.Options{
				SandboxPlatform:   "linux/amd64",
				BootFilesRootPath: bootDir,
			}

			legacySpec := specs.Spec{
				Annotations: maps.Clone(tc.annotations),
				Linux:       &specs.Linux{},
				Windows:     &specs.Windows{HyperV: &specs.WindowsHyperV{}},
			}
			if legacySpec.Annotations == nil {
				legacySpec.Annotations = map[string]string{}
			}
			_, legacyOpts, err := buildLegacyLCOWDocument(ctx, legacySpec, shimOpts, bootDir)
			if err != nil {
				t.Fatalf("failed to build legacy LCOW document: %v", err)
			}

			v2Spec := &vm.Spec{Annotations: maps.Clone(tc.annotations)}
			if v2Spec.Annotations == nil {
				v2Spec.Annotations = map[string]string{}
			}
			_, sandboxOpts, err := buildV2LCOWDocument(ctx, shimOpts, v2Spec, bootDir)
			if err != nil {
				t.Fatalf("failed to build v2 LCOW document: %v", err)
			}

			checks := []struct {
				name   string
				legacy interface{}
				v2     interface{}
			}{
				{"NoWritableFileShares", legacyOpts.NoWritableFileShares, sandboxOpts.NoWritableFileShares},
				{"EnableScratchEncryption", legacyOpts.EnableScratchEncryption, sandboxOpts.EnableScratchEncryption},
				{"PolicyBasedRouting", legacyOpts.PolicyBasedRouting, sandboxOpts.PolicyBasedRouting},
				{"FullyPhysicallyBacked", legacyOpts.FullyPhysicallyBacked, sandboxOpts.FullyPhysicallyBacked},
				{"VPMEMMultiMapping", !legacyOpts.VPMemNoMultiMapping, sandboxOpts.VPMEMMultiMapping},
			}

			for _, c := range checks {
				t.Run(c.name, func(t *testing.T) {
					if c.legacy != c.v2 {
						t.Errorf("sandbox option %s mismatch: legacy=%v, v2=%v", c.name, c.legacy, c.v2)
					}
				})
			}
		})
	}
}

// TestLCOWDocumentParityPermutations exercises annotation and option combinations
// that trigger different document construction branches. Each test populates all
// fields it depends on so the comparison checks real values, not defaults.
func TestLCOWDocumentParityPermutations(t *testing.T) {
	bootDir := setupBootFiles(t)

	tests := []struct {
		name              string
		annotations       map[string]string
		devices           []specs.WindowsDevice
		shimOpts          func() *runhcsopts.Options
		expectedDiffField string // for gap tests: the HCS field path expected in the diff
	}{
		// --- CPU partial combinations ---

		{
			name: "CPU: count only",
			annotations: map[string]string{
				shimannotations.ProcessorCount:             "2",
				shimannotations.CPUGroupID:                 "cpu-only-group",
				shimannotations.StorageQoSIopsMaximum:      "1000",
				shimannotations.StorageQoSBandwidthMaximum: "100000",
			},
		},
		{
			name: "CPU: limit only",
			annotations: map[string]string{
				shimannotations.ProcessorLimit:             "50000",
				shimannotations.CPUGroupID:                 "limit-only-group",
				shimannotations.StorageQoSIopsMaximum:      "1000",
				shimannotations.StorageQoSBandwidthMaximum: "100000",
			},
		},
		{
			name: "CPU: weight only",
			annotations: map[string]string{
				shimannotations.ProcessorWeight:            "500",
				shimannotations.CPUGroupID:                 "weight-only-group",
				shimannotations.StorageQoSIopsMaximum:      "1000",
				shimannotations.StorageQoSBandwidthMaximum: "100000",
			},
		},

		// --- Memory partial combinations ---

		{
			name: "memory: overcommit disabled",
			annotations: map[string]string{
				shimannotations.MemorySizeInMB:             "2048",
				shimannotations.AllowOvercommit:            "false",
				shimannotations.CPUGroupID:                 "mem-nocommit-group",
				shimannotations.StorageQoSIopsMaximum:      "1000",
				shimannotations.StorageQoSBandwidthMaximum: "100000",
			},
		},
		{
			name: "memory: cold discard hint",
			annotations: map[string]string{
				shimannotations.MemorySizeInMB:             "1024",
				shimannotations.EnableColdDiscardHint:      "true",
				shimannotations.CPUGroupID:                 "cold-discard-group",
				shimannotations.StorageQoSIopsMaximum:      "1000",
				shimannotations.StorageQoSBandwidthMaximum: "100000",
			},
		},

		// --- Boot mode interactions ---

		{
			name: "boot: kernel direct + VHD rootfs",
			annotations: map[string]string{
				shimannotations.KernelDirectBoot:           "true",
				shimannotations.PreferredRootFSType:        "vhd",
				shimannotations.CPUGroupID:                 "vhd-boot-group",
				shimannotations.StorageQoSIopsMaximum:      "1000",
				shimannotations.StorageQoSBandwidthMaximum: "100000",
			},
		},

		// --- Feature flag combinations ---

		{
			name: "feature: scratch encryption + disable writable shares",
			annotations: map[string]string{
				shimannotations.LCOWEncryptedScratchDisk:   "true",
				shimannotations.DisableWritableFileShares:  "true",
				shimannotations.CPUGroupID:                 "scratch-enc-group",
				shimannotations.StorageQoSIopsMaximum:      "1000",
				shimannotations.StorageQoSBandwidthMaximum: "100000",
			},
		},
		{
			name: "feature: writable overlay dirs (VHD rootfs)",
			annotations: map[string]string{
				shimannotations.PreferredRootFSType:        "vhd",
				shimannotations.KernelDirectBoot:           "true",
				iannotations.WritableOverlayDirs:           "true",
				shimannotations.CPUGroupID:                 "overlay-group",
				shimannotations.StorageQoSIopsMaximum:      "1000",
				shimannotations.StorageQoSBandwidthMaximum: "100000",
			},
		},

		// --- Device interactions ---

		{
			name: "VPMem disabled (4 SCSI controllers)",
			annotations: map[string]string{
				shimannotations.VPMemCount:                 "0",
				shimannotations.CPUGroupID:                 "no-vpmem-group",
				shimannotations.StorageQoSIopsMaximum:      "1000",
				shimannotations.StorageQoSBandwidthMaximum: "100000",
			},
		},

		// --- Cross-group interactions ---

		{
			name: "cross: physically backed + VPMem disabled + scratch encryption",
			annotations: map[string]string{
				shimannotations.FullyPhysicallyBacked:      "true",
				shimannotations.VPMemCount:                 "0",
				shimannotations.LCOWEncryptedScratchDisk:   "true",
				shimannotations.MemorySizeInMB:             "4096",
				shimannotations.CPUGroupID:                 "phys-backed-group",
				shimannotations.StorageQoSIopsMaximum:      "5000",
				shimannotations.StorageQoSBandwidthMaximum: "1000000",
			},
		},
		{
			name: "cross: CPU + memory + MMIO + QoS + cold discard + VHD boot",
			annotations: map[string]string{
				shimannotations.ProcessorCount:             "2",
				shimannotations.ProcessorLimit:             "80000",
				shimannotations.ProcessorWeight:            "300",
				shimannotations.CPUGroupID:                 "full-combo-group",
				shimannotations.MemorySizeInMB:             "4096",
				shimannotations.AllowOvercommit:            "true",
				shimannotations.EnableColdDiscardHint:      "true",
				shimannotations.MemoryLowMMIOGapInMB:       "512",
				shimannotations.MemoryHighMMIOBaseInMB:     "2048",
				shimannotations.MemoryHighMMIOGapInMB:      "1024",
				shimannotations.StorageQoSIopsMaximum:      "10000",
				shimannotations.StorageQoSBandwidthMaximum: "2000000",
				shimannotations.KernelDirectBoot:           "true",
				shimannotations.PreferredRootFSType:        "vhd",
			},
		},

		// --- Shim options override vs annotation priority ---

		{
			name: "override: annotation CPU overrides shim option CPU",
			shimOpts: func() *runhcsopts.Options {
				return &runhcsopts.Options{
					SandboxPlatform:   "linux/amd64",
					BootFilesRootPath: bootDir,
					VmProcessorCount:  1,
				}
			},
			annotations: map[string]string{
				shimannotations.ProcessorCount:             "2",
				shimannotations.CPUGroupID:                 "override-cpu-group",
				shimannotations.StorageQoSIopsMaximum:      "1000",
				shimannotations.StorageQoSBandwidthMaximum: "100000",
			},
		},
		{
			name: "override: annotation memory overrides shim option memory",
			shimOpts: func() *runhcsopts.Options {
				return &runhcsopts.Options{
					SandboxPlatform:   "linux/amd64",
					BootFilesRootPath: bootDir,
					VmMemorySizeInMb:  1024,
				}
			},
			annotations: map[string]string{
				shimannotations.MemorySizeInMB:             "4096",
				shimannotations.CPUGroupID:                 "override-mem-group",
				shimannotations.StorageQoSIopsMaximum:      "1000",
				shimannotations.StorageQoSBandwidthMaximum: "100000",
			},
		},
		// --- Kernel args combinations ---

		{
			name: "kernel args: VPCIEnabled + custom boot options",
			annotations: map[string]string{
				shimannotations.VPCIEnabled:                "true",
				shimannotations.KernelBootOptions:          "loglevel=7 debug",
				shimannotations.CPUGroupID:                 "vpci-boot-group",
				shimannotations.StorageQoSIopsMaximum:      "1000",
				shimannotations.StorageQoSBandwidthMaximum: "100000",
			},
		},
		{
			name: "kernel args: disable time sync + process dump + writable overlay (VHD)",
			annotations: map[string]string{
				shimannotations.KernelDirectBoot:             "true",
				shimannotations.PreferredRootFSType:          "vhd",
				shimannotations.DisableLCOWTimeSyncService:   "true",
				shimannotations.ContainerProcessDumpLocation: "/tmp/dumps",
				iannotations.WritableOverlayDirs:             "true",
				shimannotations.CPUGroupID:                   "kargs-combo-group",
				shimannotations.StorageQoSIopsMaximum:        "1000",
				shimannotations.StorageQoSBandwidthMaximum:   "100000",
			},
		},
		{
			name: "kernel args: initrd boot (kernel cmdline whitespace warning)",
			annotations: map[string]string{
				shimannotations.PreferredRootFSType:        "initrd",
				shimannotations.KernelDirectBoot:           "true",
				shimannotations.CPUGroupID:                 "initrd-kargs-group",
				shimannotations.StorageQoSIopsMaximum:      "1000",
				shimannotations.StorageQoSBandwidthMaximum: "100000",
			},
		},

		// --- Console pipe ---

		{
			name: "console: named pipe enables COM port + nr_uarts=1",
			annotations: map[string]string{
				iannotations.UVMConsolePipe:                `\\.\pipe\test-console`,
				shimannotations.CPUGroupID:                 "console-group",
				shimannotations.StorageQoSIopsMaximum:      "1000",
				shimannotations.StorageQoSBandwidthMaximum: "100000",
			},
		},

		// --- Memory: deferred commit ---

		{
			name: "memory: deferred commit enabled (requires overcommit)",
			annotations: map[string]string{
				shimannotations.AllowOvercommit:            "true",
				shimannotations.EnableDeferredCommit:       "true",
				shimannotations.MemorySizeInMB:             "2048",
				shimannotations.CPUGroupID:                 "deferred-commit-group",
				shimannotations.StorageQoSIopsMaximum:      "1000",
				shimannotations.StorageQoSBandwidthMaximum: "100000",
			},
		},

		// --- VPMem size ---

		{
			name: "device: non-default VPMem size",
			annotations: map[string]string{
				shimannotations.VPMemSize:                  "536870912",
				shimannotations.PreferredRootFSType:        "vhd",
				shimannotations.KernelDirectBoot:           "true",
				shimannotations.CPUGroupID:                 "vpmem-size-group",
				shimannotations.StorageQoSIopsMaximum:      "1000",
				shimannotations.StorageQoSBandwidthMaximum: "100000",
			},
		},

		// --- DmVerity boot ---

		{
			name: "boot: dm-verity mode with SCSI rootfs",
			annotations: map[string]string{
				shimannotations.PreferredRootFSType:        "vhd",
				shimannotations.KernelDirectBoot:           "true",
				shimannotations.DmVerityMode:               "true",
				shimannotations.DmVerityCreateArgs:         `dm-test linear 0 1024 /dev/sda 0`,
				shimannotations.VPMemCount:                 "0",
				shimannotations.CPUGroupID:                 "dmverity-group",
				shimannotations.StorageQoSIopsMaximum:      "1000",
				shimannotations.StorageQoSBandwidthMaximum: "100000",
			},
		},

		// --- Cross-group: console + deferred commit + VHD boot ---

		{
			name: "cross: console pipe + deferred commit + VHD boot",
			annotations: map[string]string{
				iannotations.UVMConsolePipe:                `\\.\pipe\cross-console`,
				shimannotations.AllowOvercommit:            "true",
				shimannotations.EnableDeferredCommit:       "true",
				shimannotations.MemorySizeInMB:             "2048",
				shimannotations.PreferredRootFSType:        "vhd",
				shimannotations.KernelDirectBoot:           "true",
				shimannotations.CPUGroupID:                 "cross-console-deferred-group",
				shimannotations.StorageQoSIopsMaximum:      "1000",
				shimannotations.StorageQoSBandwidthMaximum: "100000",
			},
		},

		// --- VPCI device passthrough ---

		{
			name: "device: single VPCI device assignment",
			annotations: map[string]string{
				shimannotations.VPCIEnabled:                "true",
				shimannotations.CPUGroupID:                 "vpci-device-group",
				shimannotations.StorageQoSIopsMaximum:      "1000",
				shimannotations.StorageQoSBandwidthMaximum: "100000",
			},
			devices: []specs.WindowsDevice{
				{ID: "vpci://a0a0a0a0-b1b1-c2c2-d3d3-e4e4e4e4e4e4/0", IDType: "vpci"},
			},
		},
		{
			name: "device: multiple VPCI devices",
			annotations: map[string]string{
				shimannotations.VPCIEnabled:                "true",
				shimannotations.CPUGroupID:                 "multi-vpci-group",
				shimannotations.StorageQoSIopsMaximum:      "1000",
				shimannotations.StorageQoSBandwidthMaximum: "100000",
			},
			devices: []specs.WindowsDevice{
				{ID: "vpci://a0a0a0a0-b1b1-c2c2-d3d3-e4e4e4e4e4e4/0", IDType: "vpci"},
				{ID: "vpci://b1b1b1b1-c2c2-d3d3-e4e4-f5f5f5f5f5f5/0", IDType: "vpci"},
			},
		},

		// --- Extra vsock ports ---

		{
			name: "hvsocket: extra vsock ports",
			annotations: map[string]string{
				iannotations.ExtraVSockPorts:               "5000,5001",
				shimannotations.CPUGroupID:                 "vsock-ports-group",
				shimannotations.StorageQoSIopsMaximum:      "1000",
				shimannotations.StorageQoSBandwidthMaximum: "100000",
			},
		},

		// --- NUMA topology (implicit) ---

		{
			name: "NUMA: implicit topology (max processors/memory per node)",
			annotations: map[string]string{
				shimannotations.AllowOvercommit:              "false",
				shimannotations.MemorySizeInMB:               "4096",
				shimannotations.ProcessorCount:               "4",
				shimannotations.NumaMaximumProcessorsPerNode: "2",
				shimannotations.NumaMaximumMemorySizePerNode: "2048",
				shimannotations.CPUGroupID:                   "numa-implicit-group",
				shimannotations.StorageQoSIopsMaximum:        "1000",
				shimannotations.StorageQoSBandwidthMaximum:   "100000",
			},
		},

		// --- NUMA topology (explicit) ---

		{
			name: "NUMA: explicit topology (mapped physical nodes + processor/memory counts)",
			annotations: map[string]string{
				shimannotations.AllowOvercommit:            "false",
				shimannotations.MemorySizeInMB:             "4096",
				shimannotations.ProcessorCount:             "4",
				shimannotations.NumaMappedPhysicalNodes:    "0,1",
				shimannotations.NumaCountOfProcessors:      "2,2",
				shimannotations.NumaCountOfMemoryBlocks:    "2048,2048",
				shimannotations.CPUGroupID:                 "numa-explicit-group",
				shimannotations.StorageQoSIopsMaximum:      "1000",
				shimannotations.StorageQoSBandwidthMaximum: "100000",
			},
		},

		// --- ResourcePartitionID (mutually exclusive with CPUGroupID) ---

		{
			name: "CPU: resource partition ID instead of CPUGroupID",
			annotations: map[string]string{
				shimannotations.ResourcePartitionID:        "12345678-1234-1234-1234-123456789abc",
				shimannotations.ProcessorCount:             "2",
				shimannotations.MemorySizeInMB:             "2048",
				shimannotations.StorageQoSIopsMaximum:      "1000",
				shimannotations.StorageQoSBandwidthMaximum: "100000",
			},
		},

		// --- HvSocket service table via annotation prefix ---

		{
			name: "hvsocket: custom service table entry via annotation prefix",
			annotations: map[string]string{
				iannotations.UVMHyperVSocketConfigPrefix + "12345678-1234-1234-1234-123456789abc": `{"AllowWildcardBinds":true,"BindSecurityDescriptor":"D:P(A;;FA;;;WD)"}`,
				shimannotations.CPUGroupID:                 "hvsocket-prefix-group",
				shimannotations.StorageQoSIopsMaximum:      "1000",
				shimannotations.StorageQoSBandwidthMaximum: "100000",
			},
		},

		// --- Cases that expose known differences between legacy and v2 ---
		// These document real parity gaps for the v2 builder team to fix.

		{
			// No CPUGroupID set — legacy produces CpuGroup=nil,
			// v2 produces CpuGroup=&{Id:""} (unconditional init in topology.go).
			name: "gap: no CPUGroupID (nil vs empty CpuGroup)",
			annotations: map[string]string{
				shimannotations.ProcessorCount:             "2",
				shimannotations.MemorySizeInMB:             "2048",
				shimannotations.StorageQoSIopsMaximum:      "1000",
				shimannotations.StorageQoSBandwidthMaximum: "100000",
			},
			expectedDiffField: "CpuGroup",
		},
		{
			// No QoS set — legacy produces StorageQoS=nil,
			// v2 may produce StorageQoS=&{} depending on builder.
			name: "gap: no StorageQoS (nil vs empty)",
			annotations: map[string]string{
				shimannotations.ProcessorCount: "2",
				shimannotations.MemorySizeInMB: "2048",
				shimannotations.CPUGroupID:     "no-qos-group",
			},
			expectedDiffField: "StorageQoS",
		},
		{
			// Initrd preferred — legacy allocates VPMem controller with no
			// devices, v2 sets VirtualPMem=nil.
			name: "gap: initrd boot (VPMem nil vs empty controller)",
			annotations: map[string]string{
				shimannotations.PreferredRootFSType:        "initrd",
				shimannotations.KernelDirectBoot:           "true",
				shimannotations.CPUGroupID:                 "initrd-group",
				shimannotations.StorageQoSIopsMaximum:      "1000",
				shimannotations.StorageQoSBandwidthMaximum: "100000",
			},
			expectedDiffField: "VirtualPMem",
		}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			shimOpts := &runhcsopts.Options{
				SandboxPlatform:   "linux/amd64",
				BootFilesRootPath: bootDir,
			}
			if tc.shimOpts != nil {
				shimOpts = tc.shimOpts()
			}

			legacySpec := specs.Spec{
				Annotations: maps.Clone(tc.annotations),
				Linux:       &specs.Linux{},
				Windows: &specs.Windows{
					HyperV:  &specs.WindowsHyperV{},
					Devices: tc.devices,
				},
			}
			if legacySpec.Annotations == nil {
				legacySpec.Annotations = map[string]string{}
			}
			legacyDoc, legacyOpts, err := buildLegacyLCOWDocument(ctx, legacySpec, shimOpts, bootDir)
			if err != nil {
				t.Fatalf("failed to build legacy LCOW document: %v", err)
			}

			v2Spec := &vm.Spec{
				Annotations: maps.Clone(tc.annotations),
				Devices:     tc.devices,
			}
			if v2Spec.Annotations == nil {
				v2Spec.Annotations = map[string]string{}
			}
			v2Doc, sandboxOpts, err := buildV2LCOWDocument(ctx, shimOpts, v2Spec, bootDir)
			if err != nil {
				t.Fatalf("failed to build v2 LCOW document: %v", err)
			}

			if testing.Verbose() {
				t.Logf("Legacy options: %+v", legacyOpts)
				t.Logf("V2 sandbox options: %+v", sandboxOpts)
			}

			// Normalize VPCI map keys before comparison — both builders
			// generate random VMBus GUIDs that can never match.
			normalizeVirtualPci(legacyDoc)
			normalizeVirtualPci(v2Doc)

			diff := cmp.Diff(legacyDoc, v2Doc)

			// Gap tests document known v2 builder bugs. They expect a
			// diff and only fail if the documents unexpectedly match,
			// signaling the bug was fixed and the gap test should be
			// removed.
			if strings.HasPrefix(tc.name, "gap:") {
				if diff == "" {
					t.Errorf("gap test unexpectedly passed: v2 builder bug may be fixed; remove from gaps")
				} else if tc.expectedDiffField != "" && !strings.Contains(diff, tc.expectedDiffField) {
					t.Errorf("gap test diff does not contain expected field %q (unexpected regression?):\n%s", tc.expectedDiffField, diff)
				} else {
					t.Logf("expected gap diff on field %q (-legacy +v2):\n%s", tc.expectedDiffField, diff)
				}
				return
			}

			if diff != "" {
				if isOnlyKernelCmdLineWhitespaceDiff(legacyDoc, v2Doc) {
					t.Logf("WARNING: kernel cmdline has leading whitespace difference (legacy quirk): %s", diff)
				} else {
					t.Logf("Legacy document:\n%s", jsonToString(legacyDoc))
					t.Logf("V2 document:\n%s", jsonToString(v2Doc))
					t.Errorf("LCOW HCS document mismatch (-legacy +v2):\n%s", diff)
				}
			}
		})
	}
}
