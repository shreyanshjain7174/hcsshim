//go:build windows && lcow

package vm

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	runhcsoptions "github.com/Microsoft/hcsshim/cmd/containerd-shim-runhcs-v1/options"
	"github.com/Microsoft/hcsshim/internal/builder/vm/lcow"
	"github.com/Microsoft/hcsshim/internal/controller/device/scsi/disk"
	"github.com/Microsoft/hcsshim/internal/hcs/schema1"
	hcsschema "github.com/Microsoft/hcsshim/internal/hcs/schema2"
	hcs "github.com/Microsoft/hcsshim/internal/hcs/v2"
	"github.com/Microsoft/hcsshim/internal/vm/guestmanager"
	"github.com/Microsoft/hcsshim/internal/vm/vmmanager"

	"github.com/Microsoft/go-winio/pkg/guid"
	"github.com/containerd/errdefs"
	"google.golang.org/protobuf/types/known/anypb"
)

type retryTransportFactory struct {
	closeCalls int
	closeErrs  []error
}

func (*retryTransportFactory) ListenService(guid.GUID) (net.Listener, error) { return nil, nil }
func (*retryTransportFactory) ListenPort(uint32) (net.Listener, error)       { return nil, nil }
func (*retryTransportFactory) Paths() []string                               { return nil }
func (f *retryTransportFactory) Close() error {
	f.closeCalls++
	if len(f.closeErrs) == 0 {
		return nil
	}
	err := f.closeErrs[0]
	f.closeErrs = f.closeErrs[1:]
	return err
}

func TestColdCreateWithoutDirectCreatorUsesHCSDocument(t *testing.T) {
	c := New()
	err := c.CreateVM(context.Background(), &CreateOptions{ID: "no-creator"})
	if err == nil {
		t.Fatal("expected parse/config error from HCS builder, not a live HCS create")
	}
	if !strings.Contains(err.Error(), "failed to build VM config") && !strings.Contains(err.Error(), "no options provided") {
		t.Fatalf("a controller without a direct creator must stay on buildHCSConfig, got %v", err)
	}
	if c.State() != StateNotCreated || c.hcsDocument != nil || c.uvm != nil {
		t.Fatal("a failed HCS create must not mutate controller state")
	}
}

func TestColdCreateInvokesDirectCreatorAndDoesNotFallBack(t *testing.T) {
	called := 0
	c := newDirectControllerForTest(t, func(context.Context, *CreateOptions) (*DirectCreateResult, error) {
		called++
		return nil, errors.New("direct create boom")
	})
	err := c.CreateVM(context.Background(), &CreateOptions{
		ID:       "direct-fail",
		ShimOpts: &runhcsoptions.Options{SandboxPlatform: "linux/amd64"},
	})
	if err == nil || !strings.Contains(err.Error(), "direct create boom") {
		t.Fatalf("expected callback error, got %v", err)
	}
	if called != 1 {
		t.Fatalf("callback calls = %d", called)
	}
	if strings.Contains(err.Error(), "failed to build VM config") {
		t.Fatal("a direct-create controller must not fall back to HCS")
	}
	if c.State() != StateNotCreated || c.hcsDocument != nil || c.uvm != nil || c.guest != nil {
		t.Fatal("callback failure must leave the controller uninstalled")
	}
}

func TestColdCreateDirectTransportFailureClosesCreatedSystem(t *testing.T) {
	cs := newRecordingComputeSystem("")
	c := newDirectControllerForTest(t, func(context.Context, *CreateOptions) (*DirectCreateResult, error) {
		return &DirectCreateResult{
			ComputeSystem:  cs,
			RuntimeID:      guid.GUID{Data1: 1},
			SandboxOptions: &lcow.SandboxOptions{Architecture: "amd64"},
		}, nil
	})
	err := c.CreateVM(context.Background(), &CreateOptions{ID: "empty-base"})
	if err == nil {
		t.Fatal("empty transport base must fail")
	}
	if cs.closed == 0 {
		t.Fatal("wrap/transport failure must Close the created compute system")
	}
	if c.State() != StateNotCreated || c.hcsDocument != nil || c.uvm != nil {
		t.Fatal("transport failure must not install controller state")
	}
}

func TestTerminateVMAfterNaturalExitClosesResourcesAndRetriesTransport(t *testing.T) {
	ctx := context.Background()
	computeSystem := newRecordingComputeSystem("")
	utilityVM, err := vmmanager.WrapCreatedSystem(ctx, "natural-exit", computeSystem, guid.GUID{Data1: 1})
	if err != nil {
		t.Fatalf("WrapCreatedSystem: %v", err)
	}
	transportFactory := &retryTransportFactory{closeErrs: []error{errors.New("transport close failed"), nil}}
	controller := New()
	controller.vmID = "natural-exit"
	controller.uvm = utilityVM
	controller.transport = transportFactory
	controller.guest = guestmanager.New(ctx, utilityVM, transportFactory)
	controller.vmState = StateRunning

	controller.waitForVMExit(ctx)
	if controller.State() != StateTerminated {
		t.Fatalf("state after natural exit = %s, want %s", controller.State(), StateTerminated)
	}
	if err := controller.TerminateVM(ctx); err != nil {
		t.Fatalf("TerminateVM after natural exit: %v", err)
	}
	if computeSystem.closed != 1 {
		t.Fatalf("utility VM close calls = %d, want 1", computeSystem.closed)
	}
	if transportFactory.closeCalls != 2 {
		t.Fatalf("transport close calls = %d, want failed natural-exit attempt plus TerminateVM retry", transportFactory.closeCalls)
	}
	if err := controller.TerminateVM(ctx); err != nil {
		t.Fatalf("second TerminateVM: %v", err)
	}
	if computeSystem.closed != 1 || transportFactory.closeCalls != 2 {
		t.Fatalf("second TerminateVM repeated cleanup: utility VM=%d transport=%d", computeSystem.closed, transportFactory.closeCalls)
	}
}

func TestColdCreateDirectReservationFailureClosesCreatedSystem(t *testing.T) {
	cs := newRecordingComputeSystem(`C:\ov`)
	duplicate := RootfsReservation{
		Controller: 0,
		Lun:        0,
		Config: disk.Config{
			HostPath: `C:\rootfs.vhd`,
			ReadOnly: true,
			Type:     disk.TypeVirtualDisk,
		},
	}
	c := newDirectControllerForTest(t, func(context.Context, *CreateOptions) (*DirectCreateResult, error) {
		return &DirectCreateResult{
			ComputeSystem:      cs,
			RuntimeID:          guid.GUID{Data1: 1},
			SandboxOptions:     &lcow.SandboxOptions{Architecture: "amd64"},
			RootfsReservations: []RootfsReservation{duplicate, duplicate},
		}, nil
	})
	err := c.CreateVM(context.Background(), &CreateOptions{ID: "duplicate-rootfs"})
	if err == nil || !strings.Contains(err.Error(), "failed to prepare platform controller state") {
		t.Fatalf("CreateVM error = %v", err)
	}
	if cs.closed != 1 {
		t.Fatalf("compute system close calls = %d, want 1", cs.closed)
	}
	if c.State() != StateNotCreated || c.hcsDocument != nil || c.uvm != nil || c.guest != nil {
		t.Fatal("reservation failure must leave the controller uninstalled")
	}
}

func TestColdCreateDirectInstallsControllerState(t *testing.T) {
	runtimeID := guid.GUID{Data1: 0x11, Data2: 0x22}
	hostPath := `C:\openvmm\rootfs.vhd`
	cs := newRecordingComputeSystem(`C:\ov`)
	sandbox := &lcow.SandboxOptions{
		Architecture:          "amd64",
		FullyPhysicallyBacked: true,
		PolicyBasedRouting:    true,
		NoWritableFileShares:  true,
	}
	c := newDirectControllerForTest(t, func(context.Context, *CreateOptions) (*DirectCreateResult, error) {
		return &DirectCreateResult{
			ComputeSystem:  cs,
			RuntimeID:      runtimeID,
			SandboxOptions: sandbox,
			RootfsReservations: []RootfsReservation{{
				Controller: 0,
				Lun:        0,
				Config: disk.Config{
					HostPath: hostPath,
					ReadOnly: true,
					Type:     disk.TypeVirtualDisk,
				},
			}},
		}, nil
	})
	if err := c.CreateVM(context.Background(), &CreateOptions{ID: "installed"}); err != nil {
		t.Fatalf("CreateVM: %v", err)
	}
	if c.State() != StateCreated {
		t.Fatalf("state=%s", c.State())
	}
	if c.hcsDocument != nil {
		t.Fatal("direct create must leave hcsDocument nil")
	}
	if c.VM() == nil || c.VM().ID() != "installed" || c.VM().RuntimeID() != runtimeID {
		t.Fatalf("UVM identity = %+v", c.VM())
	}
	if c.RuntimeID() != runtimeID.String() {
		t.Fatalf("controller RuntimeID=%q", c.RuntimeID())
	}
	got := c.SandboxOptions()
	if got == nil || got.Architecture != "amd64" || !got.FullyPhysicallyBacked || !got.PolicyBasedRouting || !got.NoWritableFileShares {
		t.Fatalf("SandboxOptions=%+v", got)
	}
	if c.Guest() == nil {
		t.Fatal("guest manager must be installed")
	}
	scsiCtrl, err := c.SCSIController(context.Background())
	if err != nil {
		t.Fatalf("SCSIController: %v", err)
	}
	disks := scsiCtrl.Disks()
	if len(disks) != 1 || disks[0].HostPath != hostPath || !disks[0].ReadOnly || disks[0].Type != disk.TypeVirtualDisk {
		t.Fatalf("SCSI disks = %+v", disks)
	}
}

func TestColdCreateDirectRejectsDuplicateCreateWithoutRelaunch(t *testing.T) {
	calls := 0
	cs := newRecordingComputeSystem(`C:\ov`)
	c := newDirectControllerForTest(t, func(context.Context, *CreateOptions) (*DirectCreateResult, error) {
		calls++
		return &DirectCreateResult{ComputeSystem: cs, RuntimeID: guid.GUID{Data1: 1}}, nil
	})
	opts := &CreateOptions{ID: "duplicate-create"}
	if err := c.CreateVM(context.Background(), opts); err != nil {
		t.Fatalf("first CreateVM: %v", err)
	}
	err := c.CreateVM(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "incorrect state") {
		t.Fatalf("second CreateVM error = %v", err)
	}
	if calls != 1 || c.State() != StateCreated || c.VM() == nil {
		t.Fatalf("calls=%d state=%s VM=%v", calls, c.State(), c.VM())
	}
}

func TestDirectControllerRejectsSaveImportAndMigration(t *testing.T) {
	sentinel := &hcsschema.ComputeSystem{Owner: "must-not-read"}
	c := newDirectControllerForTest(t, func(context.Context, *CreateOptions) (*DirectCreateResult, error) { return nil, nil })
	c.hcsDocument = sentinel
	ctx := context.Background()

	assertNI := func(name string, err error) {
		t.Helper()
		if err == nil || !errors.Is(err, errdefs.ErrNotImplemented) {
			t.Fatalf("%s: want ErrNotImplemented, got %v", name, err)
		}
		if c.hcsDocument != sentinel {
			t.Fatalf("%s mutated hcsDocument", name)
		}
	}

	_, err := c.Save(ctx)
	assertNI("Save", err)
	assertNI("Import", c.Import(ctx, &anypb.Any{}))
	assertNI("Patch", c.Patch(ctx))
	assertNI("Resume", c.Resume(ctx, false))
	assertNI("InitializeLiveMigrationOnSource", c.InitializeLiveMigrationOnSource(ctx, &hcsschema.MigrationInitializeOptions{}))
	_, err = c.CompatibilityInfo(ctx)
	assertNI("CompatibilityInfo", err)
	_, err = c.MigrationNotifications()
	assertNI("MigrationNotifications", err)
	assertNI("StartWithMigrationOptions", c.StartWithMigrationOptions(ctx, &hcs.MigrationConfig{}))
	assertNI("StartLiveMigrationOnSource", c.StartLiveMigrationOnSource(ctx, &hcs.MigrationConfig{}))
	assertNI("StartLiveMigrationTransfer", c.StartLiveMigrationTransfer(ctx, &hcsschema.MigrationTransferOptions{}))
	assertNI("FinalizeLiveMigration", c.FinalizeLiveMigration(ctx, &hcsschema.MigrationFinalizedOptions{}))
	assertNI("CancelLiveMigration", c.CancelLiveMigration(ctx, &hcsschema.MigrationCancelOptions{}))
}

func TestFreshHCSControllerMigrationMethodsDoNotRejectUnsupported(t *testing.T) {
	c := New()
	ctx := context.Background()

	_, saveErr := c.Save(ctx)
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "Save", err: saveErr},
		{name: "Patch", err: c.Patch(ctx)},
		{name: "CancelLiveMigration", err: c.CancelLiveMigration(ctx, &hcsschema.MigrationCancelOptions{})},
	} {
		if test.err == nil {
			t.Fatalf("%s: expected a fresh-controller precondition error", test.name)
		}
		if errors.Is(test.err, errdefs.ErrNotImplemented) {
			t.Fatalf("%s: must not reject an HCS-backed controller with ErrNotImplemented: %v", test.name, test.err)
		}
	}
}

func newDirectControllerForTest(t *testing.T, create DirectCreateFunc) *Controller {
	t.Helper()
	c, err := NewWithDirectCreate(create)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// recordingComputeSystem is a host-free ComputeSystem plus TransportProvider.
type recordingComputeSystem struct {
	closed int
	base   string
}

func newRecordingComputeSystem(base string) *recordingComputeSystem {
	return &recordingComputeSystem{base: base}
}

func (s *recordingComputeSystem) TransportBase() string { return s.base }

func (s *recordingComputeSystem) Start(context.Context) error { return nil }
func (s *recordingComputeSystem) Terminate(context.Context) error {
	return nil
}
func (s *recordingComputeSystem) CloseCtx(context.Context) error {
	s.closed++
	return nil
}
func (s *recordingComputeSystem) Pause(context.Context) error  { return nil }
func (s *recordingComputeSystem) Resume(context.Context) error { return nil }
func (s *recordingComputeSystem) Save(context.Context, interface{}) error {
	return nil
}
func (s *recordingComputeSystem) WaitCtx(context.Context) error { return nil }
func (s *recordingComputeSystem) PropertiesV2(context.Context, ...hcsschema.PropertyType) (*hcsschema.Properties, error) {
	return nil, nil
}
func (s *recordingComputeSystem) PropertiesV3(context.Context, *hcsschema.PropertyQuery) (*hcsschema.Properties, error) {
	return nil, nil
}
func (s *recordingComputeSystem) StartedTime() time.Time { return time.Time{} }
func (s *recordingComputeSystem) StoppedTime() time.Time { return time.Time{} }
func (s *recordingComputeSystem) ExitError() error       { return nil }
func (s *recordingComputeSystem) Modify(context.Context, interface{}) error {
	return nil
}
func (s *recordingComputeSystem) Properties(context.Context, ...schema1.PropertyType) (*schema1.ContainerProperties, error) {
	return &schema1.ContainerProperties{}, nil
}
func (s *recordingComputeSystem) StartWithMigrationOptions(context.Context, *hcs.MigrationConfig) error {
	return nil
}
func (s *recordingComputeSystem) InitializeLiveMigrationOnSource(context.Context, *hcsschema.MigrationInitializeOptions) error {
	return nil
}
func (s *recordingComputeSystem) StartLiveMigrationOnSource(context.Context, *hcs.MigrationConfig) error {
	return nil
}
func (s *recordingComputeSystem) StartLiveMigrationTransfer(context.Context, *hcsschema.MigrationTransferOptions) error {
	return nil
}
func (s *recordingComputeSystem) CancelLiveMigration(context.Context, *hcsschema.MigrationCancelOptions) error {
	return nil
}
func (s *recordingComputeSystem) FinalizeLiveMigration(context.Context, *hcsschema.MigrationFinalizedOptions) error {
	return nil
}
func (s *recordingComputeSystem) MigrationNotifications() <-chan hcsschema.OperationSystemMigrationNotificationInfo {
	ch := make(chan hcsschema.OperationSystemMigrationNotificationInfo)
	close(ch)
	return ch
}
