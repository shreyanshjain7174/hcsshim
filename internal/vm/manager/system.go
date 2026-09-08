//go:build windows && (lcow || wcow) && openvmm_prototype

package manager

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/Microsoft/hcsshim/internal/hcs/schema1"
	hcsschema "github.com/Microsoft/hcsshim/internal/hcs/schema2"
	hcs "github.com/Microsoft/hcsshim/internal/hcs/v2"
	"github.com/Microsoft/hcsshim/internal/log"
	"github.com/Microsoft/hcsshim/internal/vm/vmmanager"
	"github.com/Microsoft/hcsshim/internal/vmservice"

	"github.com/Microsoft/go-winio/pkg/guid"
	"github.com/containerd/errdefs"
	"google.golang.org/protobuf/types/known/emptypb"
)

// System is the vmservice-backed compute system.
var (
	_ vmmanager.ComputeSystem     = (*System)(nil)
	_ vmmanager.TransportProvider = (*System)(nil)
)

var (
	// systemQuitTimeout bounds the Quit RPC. Quit only asks the vmservice host to leave;
	// the launcher termination ladder is what guarantees it, so a host that never answers
	// must not hold up the rungs that follow.
	systemQuitTimeout = 5 * time.Second
	// systemCleanupTimeout bounds every other external cleanup call, so neither Terminate
	// nor CloseCtx can wait on a wedged backend forever.
	systemCleanupTimeout = 30 * time.Second
)

type System struct {
	id string
	// runtimeID is assigned by the shim, not reported by the virtstack: vmservice
	// exposes no VM GUID anywhere in its surface.
	runtimeID     guid.GUID
	transportBase string

	client   vmservice.VMClient
	launcher VMLauncher

	// serialClose and connClose run their resource's Close on a worker, so a wedged
	// stream cannot keep CloseCtx from reaching the launcher termination ladder. Either
	// is nil when the system has no such resource.
	serialClose *asyncCloser
	connClose   *asyncCloser

	binder EndpointPortBinder

	networkTxnMu    sync.Mutex
	networkMu       sync.Mutex
	networkBindings map[string]networkBinding

	// cleanupMu serializes Terminate and CloseCtx. It is held across their RPCs;
	// lifecycleMu never is.
	cleanupMu sync.Mutex

	// operationGate admits one mutation at a time. It is a one-token channel rather than
	// a mutex so a waiter can abandon the queue: a mutation whose caller is cancelled, or
	// that is queued behind an operation which ignores its cancelled context, must not be
	// stuck until that operation decides to leave.
	operationGate chan struct{}
	// closingNotify is closed exactly once, when CloseCtx begins. It is what lets a queued
	// mutation learn the system is closing without holding the gate.
	closingNotify chan struct{}

	lifecycleMu        sync.Mutex
	operationCancel    context.CancelFunc
	started            bool
	paused             bool
	terminated         bool
	closing            bool
	closed             bool
	quitAttempted      bool
	connClosed         bool
	serialClosed       bool
	launcherTerminated bool
	networkReleased    bool
	networkReleaseDone chan struct{}
	networkReleaseErr  error
	startedTime        time.Time

	waitMu      sync.Mutex
	stoppedTime time.Time
	exitErr     error

	waitCtx    context.Context
	waitCancel context.CancelFunc
	waitStart  sync.Once
	waitFinish sync.Once
	waitDone   chan struct{}

	// migrationNotifications is non-nil and closed: returning nil would make a range
	// over it block forever.
	migrationNotifications chan hcsschema.OperationSystemMigrationNotificationInfo
}

// beginOperation admits one mutation. Admission is refused - not queued - once close has
// begun, and a caller waiting for the gate is released by its own cancellation or by the
// closing notification, whichever comes first. The closure check is repeated after the
// token is held, because close may have begun while this caller waited.
func (s *System) beginOperation(ctx context.Context) (context.Context, func(), error) {
	if !s.operationStillAllowed() {
		return nil, nil, fmt.Errorf("compute system %s is closed: %w", s.id, hcs.ErrAlreadyClosed)
	}

	select {
	case s.operationGate <- struct{}{}:
	case <-s.closingNotify:
		return nil, nil, fmt.Errorf("compute system %s is closed: %w", s.id, hcs.ErrAlreadyClosed)
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}

	s.lifecycleMu.Lock()
	if s.closing || s.closed {
		s.lifecycleMu.Unlock()
		<-s.operationGate
		return nil, nil, fmt.Errorf("compute system %s is closed: %w", s.id, hcs.ErrAlreadyClosed)
	}
	opCtx, cancel := context.WithCancel(ctx)
	s.operationCancel = cancel
	s.lifecycleMu.Unlock()
	return opCtx, func() {
		s.lifecycleMu.Lock()
		s.operationCancel = nil
		s.lifecycleMu.Unlock()
		cancel()
		<-s.operationGate
	}, nil
}

func (s *System) operationStillAllowed() bool {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	return !s.closing && !s.closed
}

func newSystem(id string, runtimeID guid.GUID, transportBase string, client vmservice.VMClient, conn io.Closer, launcher VMLauncher, serial *serialRelay, binder EndpointPortBinder) *System {
	notifications := make(chan hcsschema.OperationSystemMigrationNotificationInfo)
	close(notifications)
	waitCtx, waitCancel := context.WithCancel(context.Background())

	if binder == nil {
		binder = hcnEndpointPortBinder{}
	}

	system := &System{
		id:                     id,
		runtimeID:              runtimeID,
		transportBase:          transportBase,
		client:                 client,
		launcher:               launcher,
		binder:                 binder,
		networkBindings:        make(map[string]networkBinding),
		operationGate:          make(chan struct{}, 1),
		closingNotify:          make(chan struct{}),
		waitCtx:                waitCtx,
		waitCancel:             waitCancel,
		waitDone:               make(chan struct{}),
		migrationNotifications: notifications,
	}
	if serial != nil {
		system.serialClose = newAsyncCloser(serial.Close)
	}
	if conn != nil {
		system.connClose = newAsyncCloser(conn.Close)
	}
	return system
}

// asyncCloser runs one resource's Close on a worker goroutine so a caller can bound its
// wait and move on. At most one worker exists at a time, so a retry that arrives while
// the close is still blocked joins the existing attempt rather than starting a second.
// A close that returned an error is retryable - the resource was never proven released,
// which is the same rule the other CloseCtx rungs follow - and a close that returned nil
// is terminal, so the resource is closed exactly once on the successful path.
type asyncCloser struct {
	closeFn func() error

	mu       sync.Mutex
	done     chan struct{}
	err      error
	released bool
}

func newAsyncCloser(closeFn func() error) *asyncCloser {
	return &asyncCloser{closeFn: closeFn}
}

// release starts or joins the close worker and waits up to budget for it. It reports
// whether the resource is proven released, so a caller never records a close it did not
// observe finish. A worker that outlives the budget keeps running: its outcome is what a
// later release call observes.
func (a *asyncCloser) release(budget time.Duration) (bool, error) {
	a.mu.Lock()
	if a.released {
		a.mu.Unlock()
		return true, nil
	}
	done := a.done
	if done == nil {
		done = make(chan struct{})
		a.done = done
		go func() {
			err := a.closeFn()
			a.mu.Lock()
			a.err = err
			a.released = err == nil
			if err != nil {
				a.done = nil
			}
			a.mu.Unlock()
			close(done)
		}()
	}
	a.mu.Unlock()

	waitCtx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	select {
	case <-done:
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.released, a.err
	case <-waitCtx.Done():
		return false, waitCtx.Err()
	}
}

// notImplemented names the method that has no vmservice answer, so a caller sees why
// rather than a silent no-op.
func (s *System) notImplemented(method string) error {
	return fmt.Errorf("%s is not available on the %s backend for compute system %s: %w", method, BackendName, s.id, errdefs.ErrNotImplemented)
}

// Start resumes the created-but-paused VM. CreateVM leaves the VM paused, so ResumeVM is
// what starts it. Start is legal exactly once.
func (s *System) Start(ctx context.Context) error {
	opCtx, finish, err := s.beginOperation(ctx)
	if err != nil {
		return err
	}
	defer finish()

	s.lifecycleMu.Lock()
	if s.terminated {
		s.lifecycleMu.Unlock()
		return fmt.Errorf("compute system %s is terminated", s.id)
	}
	if s.started {
		s.lifecycleMu.Unlock()
		return fmt.Errorf("compute system %s is already started", s.id)
	}
	s.lifecycleMu.Unlock()

	if _, err := s.client.ResumeVM(opCtx, &emptypb.Empty{}); err != nil {
		return fmt.Errorf("failed to start compute system %s: %w", s.id, err)
	}

	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.closing || s.closed {
		return fmt.Errorf("compute system %s closed while starting: %w", s.id, hcs.ErrAlreadyClosed)
	}
	s.started = true
	if s.startedTime.IsZero() {
		s.startedTime = time.Now()
	}
	return nil
}

// Pause transitions a running VM to paused.
func (s *System) Pause(ctx context.Context) error {
	opCtx, finish, err := s.beginOperation(ctx)
	if err != nil {
		return err
	}
	defer finish()

	s.lifecycleMu.Lock()
	if s.terminated {
		s.lifecycleMu.Unlock()
		return fmt.Errorf("compute system %s is terminated", s.id)
	}
	if !s.started {
		s.lifecycleMu.Unlock()
		return fmt.Errorf("compute system %s is not started", s.id)
	}
	if s.paused {
		s.lifecycleMu.Unlock()
		return fmt.Errorf("compute system %s is already paused", s.id)
	}
	s.lifecycleMu.Unlock()

	if _, err := s.client.PauseVM(opCtx, &emptypb.Empty{}); err != nil {
		return fmt.Errorf("failed to pause compute system %s: %w", s.id, err)
	}

	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.closing || s.closed {
		return fmt.Errorf("compute system %s closed while pausing: %w", s.id, hcs.ErrAlreadyClosed)
	}
	s.paused = true
	return nil
}

// Resume transitions a paused VM back to running. It uses the same RPC as Start and is
// reached only after Pause.
func (s *System) Resume(ctx context.Context) error {
	opCtx, finish, err := s.beginOperation(ctx)
	if err != nil {
		return err
	}
	defer finish()

	s.lifecycleMu.Lock()
	if s.terminated {
		s.lifecycleMu.Unlock()
		return fmt.Errorf("compute system %s is terminated", s.id)
	}
	if !s.paused {
		s.lifecycleMu.Unlock()
		return fmt.Errorf("compute system %s is not paused", s.id)
	}
	s.lifecycleMu.Unlock()

	if _, err := s.client.ResumeVM(opCtx, &emptypb.Empty{}); err != nil {
		return fmt.Errorf("failed to resume compute system %s: %w", s.id, err)
	}

	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.closing || s.closed {
		return fmt.Errorf("compute system %s closed while resuming: %w", s.id, hcs.ErrAlreadyClosed)
	}
	s.paused = false
	return nil
}

// Terminate releases the VM's resources and unblocks WaitVM. It is idempotent.
func (s *System) Terminate(ctx context.Context) error {
	s.cleanupMu.Lock()
	defer s.cleanupMu.Unlock()

	s.lifecycleMu.Lock()
	done := s.closing || s.closed || s.terminated
	s.lifecycleMu.Unlock()
	if done {
		return nil
	}

	// Detached and bounded: a cancelled caller must not skip the teardown that unblocks
	// WaitVM, and a wedged backend must not make this call the new infinite wait.
	teardownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), systemCleanupTimeout)
	defer cancel()

	if _, err := s.client.TeardownVM(teardownCtx, &emptypb.Empty{}); err != nil {
		return fmt.Errorf("failed to terminate compute system %s: %w", s.id, err)
	}

	s.lifecycleMu.Lock()
	s.terminated = true
	s.lifecycleMu.Unlock()
	return nil
}

// quit asks the vmservice host to exit. The RPC runs on its own goroutine so a host that
// never answers is abandoned at the deadline rather than stranding the later rungs.
func (s *System) quit(ctx context.Context) error {
	quitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), systemQuitTimeout)
	defer cancel()
	_, err := s.client.Quit(quitCtx, &emptypb.Empty{})
	return err
}

func (s *System) networkRelease() <-chan struct{} {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.networkReleased {
		return nil
	}
	if s.networkReleaseDone != nil {
		return s.networkReleaseDone
	}
	done := make(chan struct{})
	s.networkReleaseDone = done
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), systemCleanupTimeout)
		err := s.releaseNetworkBindings(ctx)
		cancel()
		s.lifecycleMu.Lock()
		s.networkReleaseErr = err
		s.networkReleased = err == nil
		close(done)
		s.lifecycleMu.Unlock()
	}()
	return done
}

func (s *System) networkReleaseResult(done <-chan struct{}) error {
	if done == nil {
		return nil
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), systemCleanupTimeout)
	defer cancel()
	select {
	case <-done:
		s.lifecycleMu.Lock()
		defer s.lifecycleMu.Unlock()
		err := s.networkReleaseErr
		if err != nil && s.networkReleaseDone == done {
			s.networkReleaseDone = nil
			s.networkReleaseErr = nil
		}
		return err
	case <-waitCtx.Done():
		return waitCtx.Err()
	}
}

// CloseCtx shuts down the vmservice host and releases its resources.
func (s *System) CloseCtx(ctx context.Context) error {
	s.cleanupMu.Lock()
	defer s.cleanupMu.Unlock()

	s.lifecycleMu.Lock()
	if s.closed {
		s.lifecycleMu.Unlock()
		return nil
	}
	// Closing is irreversible, so the notification is published once and never withdrawn:
	// a close that fails a rung still leaves admission shut.
	if !s.closing {
		s.closing = true
		close(s.closingNotify)
	}
	if s.operationCancel != nil {
		s.operationCancel()
	}
	quitAttempted := s.quitAttempted
	connClosed := s.connClosed
	serialClosed := s.serialClosed
	launcherTerminated := s.launcherTerminated
	s.lifecycleMu.Unlock()

	var closeErr error
	fail := func(err error) {
		if closeErr == nil {
			closeErr = err
			return
		}
		log.G(context.Background()).WithError(err).Warn("additional OpenVMM cleanup failure")
	}

	if err := s.networkReleaseResult(s.networkRelease()); err != nil {
		fail(fmt.Errorf("failed to release network bindings for compute system %s: %w", s.id, err))
	}
	if !serialClosed {
		if s.serialClose == nil {
			serialClosed = true
		} else {
			released, err := s.serialClose.release(systemCleanupTimeout)
			if err != nil {
				fail(fmt.Errorf("failed to close serial relay for compute system %s: %w", s.id, err))
			}
			serialClosed = released
		}
	}

	if !quitAttempted {
		quitAttempted = true
		if err := s.quit(ctx); err != nil {
			fail(fmt.Errorf("failed to quit the vmservice host for compute system %s: %w", s.id, err))
		}
	}
	if !connClosed {
		if s.connClose == nil {
			connClosed = true
		} else {
			released, err := s.connClose.release(systemCleanupTimeout)
			if err != nil {
				fail(fmt.Errorf("failed to close the vmservice connection for compute system %s: %w", s.id, err))
			}
			connClosed = released
		}
	}
	if !launcherTerminated {
		if s.launcher == nil {
			launcherTerminated = true
		} else {
			launcherCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), systemCleanupTimeout)
			err := s.launcher.Terminate(launcherCtx)
			cancel()
			if err != nil {
				fail(fmt.Errorf("failed to terminate the VM host for compute system %s: %w", s.id, err))
			} else {
				launcherTerminated = true
			}
		}
	}
	s.lifecycleMu.Lock()
	s.quitAttempted = quitAttempted
	s.connClosed = connClosed
	s.serialClosed = serialClosed
	s.launcherTerminated = launcherTerminated
	closed := s.networkReleased && serialClosed && connClosed && launcherTerminated
	s.closed = closed
	if closed {
		s.terminated = true
	}
	s.lifecycleMu.Unlock()

	if closed {
		s.finishWait(hcs.ErrAlreadyClosed)
		s.waitCancel()
	}
	return closeErr
}

// WaitCtx blocks until the VM halts or its resources are released. The exit latch is set
// once: a second call returns the same result without a second RPC.
func (s *System) WaitCtx(ctx context.Context) error {
	select {
	case <-s.waitDone:
		return s.ExitError()
	default:
	}

	s.waitStart.Do(func() {
		select {
		case <-s.waitDone:
			return
		default:
		}
		go func() {
			_, err := s.client.WaitVM(s.waitCtx, &emptypb.Empty{})
			s.finishWait(err)
		}()
	})

	select {
	case <-s.waitDone:
		return s.ExitError()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *System) finishWait(err error) {
	s.waitFinish.Do(func() {
		s.waitMu.Lock()
		s.exitErr = err
		s.stoppedTime = time.Now()
		s.waitMu.Unlock()
		close(s.waitDone)
	})
}

// ExitError reports why the VM stopped. It is local state set by the exit latch.
func (s *System) ExitError() error {
	select {
	case <-s.waitDone:
	default:
		return fmt.Errorf("container not exited")
	}

	s.waitMu.Lock()
	defer s.waitMu.Unlock()
	return s.exitErr
}

// StartedTime is set once, on the successful start.
func (s *System) StartedTime() time.Time {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	return s.startedTime
}

// StoppedTime is set once, when the exit latch fires.
func (s *System) StoppedTime() time.Time {
	s.waitMu.Lock()
	defer s.waitMu.Unlock()
	return s.stoppedTime
}

// Save has no vmservice counterpart: the surface exposes no save or restore RPC.
func (s *System) Save(context.Context, interface{}) error {
	return s.notImplemented("Save")
}

// Modify translates a hot-plug SCSI add/remove to a vmservice ModifyResource call. Every
// other resource path, and any config of the wrong type, is a named not-implemented error:
// BuildModifyResourceRequest never returns a partial request beside an error, so this
// method never issues an RPC for a request it could not fully translate.
func (s *System) Modify(ctx context.Context, config interface{}) error {
	opCtx, finish, err := s.beginOperation(ctx)
	if err != nil {
		return err
	}
	defer finish()

	var req *hcsschema.ModifySettingRequest
	switch value := config.(type) {
	case *hcsschema.ModifySettingRequest:
		req = value
	case hcsschema.ModifySettingRequest:
		req = &value
	default:
		return fmt.Errorf("Modify requires a *hcsschema.ModifySettingRequest, got %T: %w", config, ErrModifyNotSupported)
	}

	if req != nil && isNetworkResourcePath(req.ResourcePath) {
		err := s.modifyNetwork(opCtx, req)
		if err == nil && !s.operationStillAllowed() {
			return fmt.Errorf("compute system %s closed while modifying network: %w", s.id, hcs.ErrAlreadyClosed)
		}
		return err
	}

	modifyReq, err := BuildModifyResourceRequest(req)
	if err != nil {
		return err
	}

	if _, err := s.client.ModifyResource(opCtx, modifyReq); err != nil {
		return fmt.Errorf("failed to modify compute system %s at resource path %q: %w", s.id, req.ResourcePath, err)
	}
	if !s.operationStillAllowed() {
		return fmt.Errorf("compute system %s closed while modifying resource path %q: %w", s.id, req.ResourcePath, hcs.ErrAlreadyClosed)
	}
	return nil
}

// Properties returns identity only. With no property types it answers the shim-assigned
// runtime GUID and issues no RPC; with any type it refuses rather than inventing a
// statistic vmservice does not expose.
func (s *System) Properties(_ context.Context, types ...schema1.PropertyType) (*schema1.ContainerProperties, error) {
	if len(types) != 0 {
		return nil, fmt.Errorf("schema1 property type %q is not available on the %s backend for compute system %s: %w", string(types[0]), BackendName, s.id, errdefs.ErrNotImplemented)
	}
	return &schema1.ContainerProperties{
		ID:        s.id,
		RuntimeID: s.runtimeID,
	}, nil
}

// PropertiesV2 is deferred, not silently stubbed: vmservice reports only memory and
// processor statistics and no VM GUID, and translating them has no consumer on this path.
func (s *System) PropertiesV2(context.Context, ...hcsschema.PropertyType) (*hcsschema.Properties, error) {
	return nil, s.notImplemented("PropertiesV2")
}

// PropertiesV3's only caller is the migration path, which is off on this backend.
func (s *System) PropertiesV3(context.Context, *hcsschema.PropertyQuery) (*hcsschema.Properties, error) {
	return nil, s.notImplemented("PropertiesV3")
}

func (s *System) StartWithMigrationOptions(context.Context, *hcs.MigrationConfig) error {
	return s.notImplemented("StartWithMigrationOptions")
}

func (s *System) InitializeLiveMigrationOnSource(context.Context, *hcsschema.MigrationInitializeOptions) error {
	return s.notImplemented("InitializeLiveMigrationOnSource")
}

func (s *System) StartLiveMigrationOnSource(context.Context, *hcs.MigrationConfig) error {
	return s.notImplemented("StartLiveMigrationOnSource")
}

func (s *System) StartLiveMigrationTransfer(context.Context, *hcsschema.MigrationTransferOptions) error {
	return s.notImplemented("StartLiveMigrationTransfer")
}

func (s *System) CancelLiveMigration(context.Context, *hcsschema.MigrationCancelOptions) error {
	return s.notImplemented("CancelLiveMigration")
}

func (s *System) FinalizeLiveMigration(context.Context, *hcsschema.MigrationFinalizedOptions) error {
	return s.notImplemented("FinalizeLiveMigration")
}

// MigrationNotifications returns a non-nil closed channel, so a range terminates.
func (s *System) MigrationNotifications() <-chan hcsschema.OperationSystemMigrationNotificationInfo {
	return s.migrationNotifications
}

// TransportBase returns the hybrid-vsock base this system handed to vmservice.
func (s *System) TransportBase() string {
	return s.transportBase
}
