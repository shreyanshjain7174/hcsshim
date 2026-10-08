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

func TestBuildCreateVMRequestWiresBootAndRootDisk(t *testing.T) {
	cfg := nativeBuilderConfig()
	opts := defaultNativeOpts(newBootFiles(t))
	opts.VmProcessorCount = 1
	opts.VmMemorySizeInMb = 1024
	opts.LogLevel = "debug"
	opts.DefaultContainerAnnotations = map[string]string{
		shimannotations.ProcessorCount: "3",
		shimannotations.MemorySizeInMB: "2048",
	}
	spec := &vmsandbox.Spec{Annotations: map[string]string{
		shimannotations.ProcessorCount:            "2",
		shimannotations.KernelBootOptions:         "custom-kernel=1",
		shimannotations.DisableWritableFileShares: "true",
		shimannotations.LCOWEncryptedScratchDisk:  "true",
		iannotations.NetworkingPolicyBasedRouting: "true",
		iannotations.ExtraLCOWExecArgs:            "--custom-gcs",
		iannotations.UVMConsolePipe:               `\\.\pipe\console`,
	}}

	request, sandbox, reservations, err := BuildCreateVMRequestFromOptions(
		context.Background(), opts, spec, cfg, "native-log",
	)
	if err != nil {
		t.Fatalf("BuildCreateVMRequestFromOptions: %v", err)
	}
	boot, ok := request.Config.BootConfig.(*vmservice.VMConfig_DirectBoot)
	if !ok || boot.DirectBoot == nil || boot.DirectBoot.KernelPath == "" || !strings.Contains(boot.DirectBoot.KernelCmdline, "root=/dev/sda") {
		t.Fatalf("direct boot = %+v", request.Config.BootConfig)
	}
	for _, arg := range []string{"custom-kernel=1", "nr_cpus=2", "-loglevel debug", "--custom-gcs"} {
		if !strings.Contains(boot.DirectBoot.KernelCmdline, arg) {
			t.Fatalf("kernel command line %q is missing %q", boot.DirectBoot.KernelCmdline, arg)
		}
	}
	if request.GetLogId() != "native-log" || request.Config.GetProcessorConfig().GetProcessorCount() != 2 || request.Config.GetMemoryConfig().GetMemoryMb() != 2048 || !request.Config.GetMemoryConfig().GetAllowOvercommit() {
		t.Fatalf("request identity/resources = log=%q cpu=%d memory=%d overcommit=%v", request.GetLogId(), request.Config.GetProcessorConfig().GetProcessorCount(), request.Config.GetMemoryConfig().GetMemoryMb(), request.Config.GetMemoryConfig().GetAllowOvercommit())
	}
	if spec.Annotations[shimannotations.ProcessorCount] != "2" || spec.Annotations[shimannotations.MemorySizeInMB] != "2048" {
		t.Fatalf("default annotation merge = cpu=%q memory=%q", spec.Annotations[shimannotations.ProcessorCount], spec.Annotations[shimannotations.MemorySizeInMB])
	}
	serial := request.Config.GetSerialConfig()
	if serial == nil || len(serial.GetPorts()) != 1 || serial.GetPorts()[0].GetPort() != 0 || !serial.GetPorts()[0].GetConnect() || serial.GetPorts()[0].GetSocketPath() != cfg.SerialSocket {
		t.Fatalf("COM1 serial = %+v", serial)
	}
	if request.Config.GetHvsocketConfig().GetPath() != cfg.HybridVsockBase || sandbox == nil || sandbox.Architecture != "amd64" || !sandbox.PolicyBasedRouting || !sandbox.NoWritableFileShares || !sandbox.EnableScratchEncryption {
		t.Fatalf("transport/sandbox = hvsock=%q sandbox=%+v", request.Config.GetHvsocketConfig().GetPath(), sandbox)
	}
	disks := request.Config.DevicesConfig.GetScsiDisks()
	if len(disks) != 1 || disks[0].GetController() != 0 || disks[0].GetLun() != 0 || !disks[0].GetReadOnly() || disks[0].GetType() != vmservice.DiskType_SCSI_DISK_TYPE_VHD1 {
		t.Fatalf("root disk = %+v", disks)
	}
	if len(reservations) != 1 || reservations[0].Config.HostPath != disks[0].GetHostPath() {
		t.Fatalf("root reservation = %+v, disk = %+v", reservations, disks[0])
	}

	defaults, _, _, err := BuildCreateVMRequestFromOptions(
		context.Background(), defaultNativeOpts(newBootFiles(t)), &vmsandbox.Spec{}, cfg, "defaults",
	)
	if err != nil {
		t.Fatalf("BuildCreateVMRequestFromOptions defaults: %v", err)
	}
	if defaults.Config.GetProcessorConfig().GetProcessorCount() == 0 || defaults.Config.GetMemoryConfig().GetMemoryMb() != 1024 {
		t.Fatalf("default resources = cpu=%d memory=%d", defaults.Config.GetProcessorConfig().GetProcessorCount(), defaults.Config.GetMemoryConfig().GetMemoryMb())
	}
}

func TestBuildCreateVMRequestRejectsUnsupportedRuntimeContracts(t *testing.T) {
	boot := newBootFiles(t)
	for _, test := range []struct {
		name        string
		platform    string
		annotations map[string]string
		devices     []specs.WindowsDevice
		want        string
		wantErr     error
	}{
		{name: "confidential policy", annotations: map[string]string{shimannotations.LCOWSecurityPolicy: "policy"}, want: shimannotations.LCOWSecurityPolicy},
		{name: "live migration", annotations: map[string]string{shimannotations.LiveMigrationSupportEnabled: "true"}, want: shimannotations.LiveMigrationSupportEnabled},
		{name: "initrd root", annotations: map[string]string{shimannotations.PreferredRootFSType: "initrd"}, wantErr: ErrInitrdNotSupported},
		{name: "vPCI assignment", devices: []specs.WindowsDevice{{ID: "gpu"}}, want: "vPCI"},
		{name: "ARM64", platform: "linux/arm64", want: "linux/arm64"},
	} {
		t.Run(test.name, func(t *testing.T) {
			opts := defaultNativeOpts(boot)
			if test.platform != "" {
				opts.SandboxPlatform = test.platform
			}
			_, _, _, err := BuildCreateVMRequestFromOptions(
				context.Background(), opts, &vmsandbox.Spec{Annotations: test.annotations, Devices: test.devices}, nativeBuilderConfig(), "sandbox",
			)
			if test.wantErr != nil {
				if !errors.Is(err, test.wantErr) {
					t.Fatalf("want %v, got %v", test.wantErr, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("want rejection naming %q, got %v", test.want, err)
			}
		})
	}
}
