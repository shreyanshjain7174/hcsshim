//go:build windows && lcow && openvmm_prototype

package manager

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	runhcsoptions "github.com/Microsoft/hcsshim/cmd/containerd-shim-runhcs-v1/options"
	iannotations "github.com/Microsoft/hcsshim/internal/annotations"
	"github.com/Microsoft/hcsshim/internal/builder/vm/lcow"
	"github.com/Microsoft/hcsshim/internal/controller/device/scsi/disk"
	controllervm "github.com/Microsoft/hcsshim/internal/controller/vm"
	"github.com/Microsoft/hcsshim/internal/oci"
	"github.com/Microsoft/hcsshim/internal/processorinfo"
	"github.com/Microsoft/hcsshim/internal/vm/vmutils"
	"github.com/Microsoft/hcsshim/internal/vmservice"
	"github.com/Microsoft/hcsshim/osversion"
	shimannotations "github.com/Microsoft/hcsshim/pkg/annotations"
	vmsandbox "github.com/Microsoft/hcsshim/sandbox-spec/vm/v2"
)

var (
	errNilConfig                = errors.New("no OpenVMM configuration was supplied")
	errUnsupportedDocumentField = errors.New("the requested VM configuration is not supported by the OpenVMM runtime")
	ErrInitrdNotSupported       = errors.New("PreferredRootFSType=initrd is not supported by the OpenVMM prototype backend")
	errDMVerityNotSupported     = errors.New("a dm-verity root filesystem is not supported by the OpenVMM prototype backend")
)

// BuildCreateVMRequestFromOptions builds a vmservice create request from the
// existing CreateOptions inputs. It performs no process launch and imports no
// HCS schema types.
func BuildCreateVMRequestFromOptions(
	ctx context.Context,
	opts *runhcsoptions.Options,
	spec *vmsandbox.Spec,
	cfg *Config,
	logID string,
) (*vmservice.CreateVMRequest, *lcow.SandboxOptions, []controllervm.RootfsReservation, error) {
	if opts == nil {
		return nil, nil, nil, fmt.Errorf("no options provided")
	}
	if spec == nil {
		return nil, nil, nil, fmt.Errorf("no sandbox spec provided")
	}
	if cfg == nil {
		return nil, nil, nil, fmt.Errorf("cannot build a CreateVMRequest: %w", errNilConfig)
	}
	if cfg.HybridVsockBase == "" {
		return nil, nil, nil, fmt.Errorf("Config.HybridVsockBase is empty: %w", errUnsupportedDocumentField)
	}
	if spec.Annotations == nil {
		spec.Annotations = map[string]string{}
	}

	if err := processAnnotations(ctx, opts, spec.Annotations); err != nil {
		return nil, nil, nil, fmt.Errorf("failed to process annotations: %w", err)
	}

	platform := strings.ToLower(opts.SandboxPlatform)
	if platform != "linux/amd64" {
		return nil, nil, nil, fmt.Errorf("unsupported sandbox platform: %s", opts.SandboxPlatform)
	}

	if err := rejectUnsupported(ctx, spec); err != nil {
		return nil, nil, nil, err
	}

	sandboxOptions := &lcow.SandboxOptions{
		Architecture:            platform[strings.IndexByte(platform, '/')+1:],
		PolicyBasedRouting:      oci.ParseAnnotationsBool(ctx, spec.Annotations, iannotations.NetworkingPolicyBasedRouting, false),
		NoWritableFileShares:    oci.ParseAnnotationsBool(ctx, spec.Annotations, shimannotations.DisableWritableFileShares, false),
		EnableScratchEncryption: oci.ParseAnnotationsBool(ctx, spec.Annotations, shimannotations.LCOWEncryptedScratchDisk, false),
	}

	cpuCount, err := parseCPU(ctx, opts, spec.Annotations)
	if err != nil {
		return nil, nil, nil, err
	}

	memorySize := oci.ParseAnnotationsUint64(ctx, spec.Annotations, shimannotations.MemorySizeInMB, uint64(opts.VmMemorySizeInMb))
	if memorySize == 0 {
		memorySize = 1024
	}
	memorySize = vmutils.NormalizeMemorySize(ctx, "", memorySize)

	boot, rootFsFullPath, err := parseBoot(ctx, opts, spec.Annotations)
	if err != nil {
		return nil, nil, nil, err
	}

	consolePipe := oci.ParseAnnotationsString(spec.Annotations, iannotations.UVMConsolePipe, "")
	serial, err := parseSerial(consolePipe, cfg)
	if err != nil {
		return nil, nil, nil, err
	}

	boot.KernelCmdline = buildKernelArgs(
		ctx,
		opts,
		spec.Annotations,
		cpuCount,
		consolePipe != "",
		filepath.Base(rootFsFullPath),
	)

	return &vmservice.CreateVMRequest{
			LogId: logID,
			Config: &vmservice.VMConfig{
				MemoryConfig:    &vmservice.MemoryConfig{MemoryMb: memorySize, AllowOvercommit: true},
				ProcessorConfig: &vmservice.ProcessorConfig{ProcessorCount: cpuCount},
				DevicesConfig: &vmservice.DevicesConfig{ScsiDisks: []*vmservice.SCSIDisk{{
					Controller: 0,
					Lun:        0,
					HostPath:   rootFsFullPath,
					Type:       vmservice.DiskType_SCSI_DISK_TYPE_VHD1,
					ReadOnly:   true,
				}}},
				SerialConfig:   serial,
				BootConfig:     &vmservice.VMConfig_DirectBoot{DirectBoot: boot},
				HvsocketConfig: &vmservice.HVSocketConfig{Path: cfg.HybridVsockBase},
			},
		}, sandboxOptions, []controllervm.RootfsReservation{{
			Controller: 0,
			Lun:        0,
			Config: disk.Config{
				HostPath: rootFsFullPath,
				ReadOnly: true,
				Type:     disk.TypeVirtualDisk,
			},
		}}, nil
}

func processAnnotations(ctx context.Context, opts *runhcsoptions.Options, annotations map[string]string) error {
	for key, value := range opts.DefaultContainerAnnotations {
		if _, exists := annotations[key]; !exists {
			annotations[key] = value
		}
	}
	if err := oci.ProcessAnnotations(ctx, annotations); err != nil {
		return fmt.Errorf("failed to process OCI annotations: %w", err)
	}
	for _, key := range []string{
		shimannotations.NetworkConfigProxy,
		shimannotations.VPMemNoMultiMapping,
		shimannotations.VirtualMachineKernelDrivers,
	} {
		if v := oci.ParseAnnotationsString(annotations, key, ""); v != "" {
			return fmt.Errorf("%s annotation is not supported", key)
		}
	}
	return nil
}

func rejectUnsupported(ctx context.Context, spec *vmsandbox.Spec) error {
	annotations := spec.Annotations
	for _, key := range []string{
		shimannotations.LCOWSecurityPolicy,
		shimannotations.LCOWSecurityPolicyEnforcer,
		shimannotations.LCOWGuestStateFile,
		shimannotations.LCOWHclEnabled,
		shimannotations.LCOWReferenceInfoFile,
		shimannotations.UVMHashEnvelopeReferenceInfoFile,
		shimannotations.DmVerityRootFsVhd,
		shimannotations.CPUGroupID,
		shimannotations.ResourcePartitionID,
		shimannotations.LiveMigrationSupportEnabled,
		iannotations.ExtraVSockPorts,
	} {
		if oci.ParseAnnotationsString(annotations, key, "") != "" {
			return fmt.Errorf("%s is not supported by the OpenVMM builder", key)
		}
	}
	if oci.ParseAnnotationsBool(ctx, annotations, shimannotations.DmVerityMode, false) {
		return fmt.Errorf("%s is not supported by the OpenVMM builder: %w", shimannotations.DmVerityMode, errDMVerityNotSupported)
	}
	if oci.ParseAnnotationsString(annotations, shimannotations.DmVerityCreateArgs, "") != "" {
		return fmt.Errorf("%s is not supported by the OpenVMM builder", shimannotations.DmVerityCreateArgs)
	}
	if oci.ParseAnnotationsInt32(ctx, annotations, shimannotations.StorageQoSIopsMaximum, 0) > 0 {
		return fmt.Errorf("%s is not supported by the OpenVMM builder", shimannotations.StorageQoSIopsMaximum)
	}
	if oci.ParseAnnotationsInt32(ctx, annotations, shimannotations.StorageQoSBandwidthMaximum, 0) > 0 {
		return fmt.Errorf("%s is not supported by the OpenVMM builder", shimannotations.StorageQoSBandwidthMaximum)
	}
	if oci.ParseAnnotationsUint32(ctx, annotations, shimannotations.VPMemCount, 0) > 0 {
		return fmt.Errorf("%s is not supported by the OpenVMM builder", shimannotations.VPMemCount)
	}
	// The paired vmservice consumes only processor_count and memory_mb: it fixes the
	// memory policy and layout and drops every other compute-topology control. Only a
	// non-default request is rejected, so defaults keep working. These values are parsed
	// strictly instead of through the shared OCI parsers, which fall back to the default
	// on a malformed value and would let an unsupported request through.
	for _, key := range []string{shimannotations.ProcessorLimit, shimannotations.ProcessorWeight} {
		raw, ok := annotations[key]
		if !ok {
			continue
		}
		value, err := strconv.ParseInt(raw, 10, 32)
		if err != nil {
			return fmt.Errorf("%s has an invalid int32 value %q: %w", key, raw, errUnsupportedDocumentField)
		}
		if value != 0 {
			return fmt.Errorf("%s is not supported by the OpenVMM builder: %w", key, errUnsupportedDocumentField)
		}
	}
	allowOvercommit, err := strictAnnotationBool(annotations, shimannotations.AllowOvercommit, true)
	if err != nil {
		return err
	}
	if !allowOvercommit {
		return fmt.Errorf("%s=false is not supported by the OpenVMM builder: %w", shimannotations.AllowOvercommit, errUnsupportedDocumentField)
	}
	for _, key := range []string{
		shimannotations.EnableDeferredCommit,
		shimannotations.EnableColdDiscardHint,
		shimannotations.FullyPhysicallyBacked,
	} {
		value, err := strictAnnotationBool(annotations, key, false)
		if err != nil {
			return err
		}
		if value {
			return fmt.Errorf("%s=true is not supported by the OpenVMM builder: %w", key, errUnsupportedDocumentField)
		}
	}
	for _, key := range []string{
		shimannotations.MemoryLowMMIOGapInMB,
		shimannotations.MemoryHighMMIOBaseInMB,
		shimannotations.MemoryHighMMIOGapInMB,
	} {
		raw, ok := annotations[key]
		if !ok {
			continue
		}
		value, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			return fmt.Errorf("%s has an invalid uint64 value %q: %w", key, raw, errUnsupportedDocumentField)
		}
		if value != 0 {
			return fmt.Errorf("%s is not supported by the OpenVMM builder: %w", key, errUnsupportedDocumentField)
		}
	}
	for _, key := range []string{
		shimannotations.NumaMaximumProcessorsPerNode,
		shimannotations.NumaMaximumMemorySizePerNode,
		shimannotations.NumaPreferredPhysicalNodes,
		shimannotations.NumaMappedPhysicalNodes,
		shimannotations.NumaCountOfProcessors,
		shimannotations.NumaCountOfMemoryBlocks,
	} {
		if oci.ParseAnnotationsString(annotations, key, "") != "" {
			return fmt.Errorf("%s is not supported by the OpenVMM builder", key)
		}
	}
	for key := range annotations {
		if strings.HasPrefix(key, iannotations.UVMHyperVSocketConfigPrefix) {
			return fmt.Errorf("%s is not supported by the OpenVMM builder", iannotations.UVMHyperVSocketConfigPrefix)
		}
	}
	if len(spec.Devices) > 0 {
		return fmt.Errorf("vPCI assignment is not supported by the OpenVMM builder")
	}
	return nil
}

// strictAnnotationBool returns def when key is absent and errors on a value that
// oci.ParseAnnotationsBool would silently discard in favour of def.
func strictAnnotationBool(annotations map[string]string, key string, def bool) (bool, error) {
	raw, ok := annotations[key]
	if !ok {
		return def, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s has an invalid boolean value %q: %w", key, raw, errUnsupportedDocumentField)
	}
	return value, nil
}

func parseCPU(ctx context.Context, opts *runhcsoptions.Options, annotations map[string]string) (uint32, error) {
	count := oci.ParseAnnotationsInt32(ctx, annotations, shimannotations.ProcessorCount, opts.VmProcessorCount)
	if count <= 0 {
		count = vmutils.DefaultProcessorCountForUVM()
	}
	topology, err := processorinfo.HostProcessorInfo(ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to get host processor information: %w", err)
	}
	return uint32(vmutils.NormalizeProcessorCount(ctx, "", count, topology)), nil
}

func parseBoot(ctx context.Context, opts *runhcsoptions.Options, annotations map[string]string) (*vmservice.DirectBoot, string, error) {
	bootFilesPath, err := resolveBootFilesPath(opts, annotations)
	if err != nil {
		return nil, "", err
	}
	fileExists := func(filename string) bool {
		_, err := os.Stat(filepath.Join(bootFilesPath, filename))
		return err == nil
	}
	rootFsFile := vmutils.InitrdFile
	if fileExists(vmutils.VhdFile) {
		rootFsFile = vmutils.VhdFile
	}
	kernelDirectBootSupported := osversion.Build() >= 18286
	if runtime.GOARCH == "arm64" {
		kernelDirectBootSupported = false
	}
	useKernelDirect := oci.ParseAnnotationsBool(ctx, annotations, shimannotations.KernelDirectBoot, kernelDirectBootSupported)
	if !useKernelDirect {
		return nil, "", fmt.Errorf("%s is not supported by the OpenVMM builder", shimannotations.KernelDirectBoot)
	}
	if !kernelDirectBootSupported {
		return nil, "", fmt.Errorf("KernelDirectBoot is not supported on builds older than 18286")
	}
	kernelFileName := vmutils.KernelFile
	if fileExists(vmutils.UncompressedKernelFile) {
		kernelFileName = vmutils.UncompressedKernelFile
	} else if !fileExists(vmutils.KernelFile) {
		return nil, "", fmt.Errorf("kernel file not found in boot files path for kernel direct boot")
	}
	if preferred := oci.ParseAnnotationsString(annotations, shimannotations.PreferredRootFSType, ""); preferred != "" {
		switch preferred {
		case "initrd":
			return nil, "", fmt.Errorf("%s=initrd is not supported by the OpenVMM builder: %w", shimannotations.PreferredRootFSType, ErrInitrdNotSupported)
		case "vhd":
			rootFsFile = vmutils.VhdFile
		default:
			return nil, "", fmt.Errorf("invalid PreferredRootFSType: %s", preferred)
		}
	}
	if rootFsFile != vmutils.VhdFile {
		return nil, "", fmt.Errorf("%s=initrd is not supported by the OpenVMM builder: %w", shimannotations.PreferredRootFSType, ErrInitrdNotSupported)
	}
	if !fileExists(rootFsFile) {
		return nil, "", fmt.Errorf("%q not found in boot files path", rootFsFile)
	}
	return &vmservice.DirectBoot{
		KernelPath: filepath.Join(bootFilesPath, kernelFileName),
	}, filepath.Join(bootFilesPath, rootFsFile), nil
}

func resolveBootFilesPath(opts *runhcsoptions.Options, annotations map[string]string) (string, error) {
	bootFilesRootPath := oci.ParseAnnotationsString(annotations, shimannotations.BootFilesRootPath, opts.BootFilesRootPath)
	if bootFilesRootPath == "" {
		bootFilesRootPath = vmutils.DefaultLCOWOSBootFilesPath()
	}
	if p, err := filepath.Abs(bootFilesRootPath); err == nil {
		bootFilesRootPath = p
	}
	if _, err := os.Stat(bootFilesRootPath); err != nil {
		return "", fmt.Errorf("boot_files_root_path %q not found: %w", bootFilesRootPath, err)
	}
	return bootFilesRootPath, nil
}

func parseSerial(consolePipe string, cfg *Config) (*vmservice.SerialConfig, error) {
	if consolePipe == "" {
		return nil, nil
	}
	if !strings.HasPrefix(consolePipe, `\\.\pipe\`) {
		return nil, fmt.Errorf("listener for serial console is not a named pipe")
	}
	if cfg.SerialSocket == "" {
		return nil, fmt.Errorf("Config.SerialSocket is empty for a document that requests COM1: %w", errUnsupportedDocumentField)
	}
	return &vmservice.SerialConfig{Ports: []*vmservice.SerialConfig_Config{{
		Port:       0,
		SocketPath: cfg.SerialSocket,
		Connect:    true,
	}}}, nil
}

func buildKernelArgs(
	ctx context.Context,
	opts *runhcsoptions.Options,
	annotations map[string]string,
	processorCount uint32,
	hasConsole bool,
	rootFsFile string,
) string {
	vpciEnabled := oci.ParseAnnotationsBool(ctx, annotations, shimannotations.VPCIEnabled, false)
	disableTimeSyncService := oci.ParseAnnotationsBool(ctx, annotations, shimannotations.DisableLCOWTimeSyncService, false)
	writableOverlayDirs := oci.ParseAnnotationsBool(ctx, annotations, iannotations.WritableOverlayDirs, false)
	processDumpLocation := oci.ParseAnnotationsString(annotations, shimannotations.ContainerProcessDumpLocation, "")

	args := []string{"root=/dev/sda ro rootwait init=/init", "initcall_blacklist=virtio_vsock_init"}
	if hasConsole {
		args = append(args, "8250_core.nr_uarts=1", "8250_core.skip_txen_test=1", "console=ttyS0,115200")
	} else {
		args = append(args, "8250_core.nr_uarts=0", "panic=-1", "quiet")
	}
	if kernelBootOptions := oci.ParseAnnotationsString(annotations, shimannotations.KernelBootOptions, ""); kernelBootOptions != "" {
		args = append(args, kernelBootOptions)
	}
	if !vpciEnabled {
		args = append(args, "pci=off")
	}
	args = append(args, fmt.Sprintf("nr_cpus=%d", processorCount), "brd.rd_nr=0", "pmtmr=0")
	args = append(args, "--", buildInitArgs(ctx, opts, annotations, writableOverlayDirs, disableTimeSyncService, processDumpLocation, rootFsFile, hasConsole))
	return strings.Join(args, " ")
}

func buildInitArgs(
	ctx context.Context,
	opts *runhcsoptions.Options,
	annotations map[string]string,
	writableOverlayDirs bool,
	disableTimeSyncService bool,
	processDumpLocation string,
	rootFsFile string,
	hasConsole bool,
) string {
	gcsCmd := buildGCSCommand(ctx, opts, annotations, disableTimeSyncService, processDumpLocation)
	initArgsList := []string{fmt.Sprintf("-e %d", vmutils.LinuxEntropyVsockPort)}
	if writableOverlayDirs && rootFsFile == vmutils.VhdFile {
		initArgsList = append(initArgsList, "-w")
	}
	if hasConsole {
		initArgsList = append(initArgsList, `sh -c"`+gcsCmd+`& exec sh"`)
	} else {
		initArgsList = append(initArgsList, gcsCmd)
	}
	return strings.Join(initArgsList, " ")
}

func buildGCSCommand(
	ctx context.Context,
	opts *runhcsoptions.Options,
	annotations map[string]string,
	disableTimeSyncService bool,
	processDumpLocation string,
) string {
	logLevel := "info"
	if opts.LogLevel != "" {
		logLevel = opts.LogLevel
	}
	gcsParts := []string{"/bin/gcs", "-v4", "-log-format json", "-loglevel " + logLevel}
	if disableTimeSyncService {
		gcsParts = append(gcsParts, "-disable-time-sync")
	}
	if opts.ScrubLogs != nil {
		gcsParts = append(gcsParts, fmt.Sprintf("-scrub-logs=%s", strconv.FormatBool(*opts.ScrubLogs)))
	}
	if processDumpLocation != "" {
		gcsParts = append(gcsParts, "-core-dump-location", processDumpLocation)
	}
	if s := oci.ParseAnnotationsString(annotations, iannotations.ExtraLCOWExecArgs, ""); s != "" {
		gcsParts = append(gcsParts, s)
	}
	return strings.Join(append([]string{vmutils.LinuxLogForwarderCommand(false)}, gcsParts...), " ")
}
