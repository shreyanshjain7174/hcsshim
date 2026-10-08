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
)

type retryTransportFactory struct {
	closeCalls int
	closeErrs  []error
}

func TestColdCreateWithoutDirectCreatorUsesHCSDocument(t *testing.T) {
	c := New()
	err := c.CreateVM(context.Background(), &CreateOptions{ID: "no-creator"})
	if err == nil || !strings.Contains(err.Error(), "failed to build VM config") {
		t.Fatalf("controller without direct creator did not use HCS builder: %v", err)
	}
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
	if calls != 1 {
		t.Fatalf("direct creator calls = %d, want 1", calls)
	}
}

func TestControllerSavePreservesBackendBoundary(t *testing.T) {
	direct := newDirectControllerForTest(t, func(context.Context, *CreateOptions) (*DirectCreateResult, error) { return nil, nil })
	_, directErr := direct.Save(context.Background())
	if !errors.Is(directErr, errdefs.ErrNotImplemented) {
		t.Fatalf("direct Save: want ErrNotImplemented, got %v", directErr)
	}
	_, hcsErr := New().Save(context.Background())
	if errors.Is(hcsErr, errdefs.ErrNotImplemented) {
		t.Fatalf("HCS Save was rejected as OpenVMM: %v", hcsErr)
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
