//go:build windows && (lcow || wcow) && openvmm_prototype

package manager

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Microsoft/hcsshim/internal/hcs/resourcepaths"
	"github.com/Microsoft/hcsshim/internal/hcs/schema1"
	hcsschema "github.com/Microsoft/hcsshim/internal/hcs/schema2"
	hcs "github.com/Microsoft/hcsshim/internal/hcs/v2"
	"github.com/Microsoft/hcsshim/internal/log"
	"github.com/Microsoft/hcsshim/internal/protocol/guestrequest"
	"github.com/Microsoft/hcsshim/internal/vm/vmmanager"
	"github.com/Microsoft/hcsshim/internal/vmservice"

	"github.com/Microsoft/go-winio/pkg/guid"
	"github.com/containerd/errdefs"
	"google.golang.org/protobuf/types/known/emptypb"
)

var (
	_ vmmanager.ComputeSystem     = (*System)(nil)
	_ vmmanager.TransportProvider = (*System)(nil)
)

var (
	// Quit must not hold up the launcher's later cleanup rungs.
	systemQuitTimeout = 5 * time.Second
	// Cleanup must remain bounded even when the backend is wedged.
	systemCleanupTimeout = 30 * time.Second
)

type System struct {
	id string
	// vmservice exposes no VM GUID; the shim supplies this identity.
	runtimeID     guid.GUID
	transportBase string

	client   vmservice.VMClient
	launcher VMLauncher

	// A wedged stream must not prevent the launcher termination ladder from running.
	serialClose *asyncCloser
	connClose   *asyncCloser

	binder EndpointPortBinder

	networkTxnMu    sync.Mutex
	networkMu       sync.Mutex
	networkBindings map[string]networkBinding

	// Held across cleanup RPCs; lifecycleMu never is.
	cleanupMu sync.Mutex

	// Unlike a mutex, this lets waiters abandon a queue behind a stuck operation.
	operationGate chan struct{}
	// Closing wakes queued mutations without waiting for the operation gate.
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

	// A nil notification channel would make range block forever.
	migrationNotifications chan hcsschema.OperationSystemMigrationNotificationInfo
}

// Close can begin while admission waits, so recheck after acquiring the token.
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

// Bound caller waits without duplicating a blocked Close or losing failed-close retries.
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

// A timed-out worker stays live so a later release can observe its result.
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

func (s *System) notImplemented(method string) error {
	return fmt.Errorf("%s is not available on the %s backend for compute system %s: %w", method, BackendName, s.id, errdefs.ErrNotImplemented)
}

// CreateVM leaves the VM paused, so starting it requires ResumeVM.
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

func (s *System) Terminate(ctx context.Context) error {
	s.cleanupMu.Lock()
	defer s.cleanupMu.Unlock()

	s.lifecycleMu.Lock()
	done := s.closing || s.closed || s.terminated
	s.lifecycleMu.Unlock()
	if done {
		return nil
	}

	// Caller cancellation must not skip teardown or strand WaitVM.
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

func (s *System) CloseCtx(ctx context.Context) error {
	s.cleanupMu.Lock()
	defer s.cleanupMu.Unlock()

	s.lifecycleMu.Lock()
	if s.closed {
		s.lifecycleMu.Unlock()
		return nil
	}
	// A failed cleanup must not reopen mutation admission.
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

	var closeErr, quitErr error
	failures := 0
	fail := func(err error) {
		failures++
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
		quitStart := time.Now()
		err := s.quit(ctx)
		log.G(context.Background()).WithField("duration", time.Since(quitStart).String()).WithError(err).Info("OpenVMM Quit returned")
		if err != nil {
			quitErr = fmt.Errorf("failed to quit the vmservice host for compute system %s: %w", s.id, err)
			fail(quitErr)
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
	// containerd skips its own cleanup on stop errors; ignore failed Quit after release.
	if closed && failures == 1 && quitErr != nil {
		log.G(context.Background()).WithError(quitErr).Warn("OpenVMM host did not answer Quit, but it is terminated and released")
		return nil
	}
	return closeErr
}

// Caller cancellation must not cancel the VM-wide wait shared by other callers.
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

func (s *System) StartedTime() time.Time {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	return s.startedTime
}

func (s *System) StoppedTime() time.Time {
	s.waitMu.Lock()
	defer s.waitMu.Unlock()
	return s.stoppedTime
}

// Save has no vmservice counterpart: the surface exposes no save or restore RPC.
func (s *System) Save(context.Context, interface{}) error {
	return s.notImplemented("Save")
}

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

func (s *System) Properties(_ context.Context, types ...schema1.PropertyType) (*schema1.ContainerProperties, error) {
	if len(types) != 0 {
		return nil, fmt.Errorf("schema1 property type %q is not available on the %s backend for compute system %s: %w", string(types[0]), BackendName, s.id, errdefs.ErrNotImplemented)
	}
	return &schema1.ContainerProperties{
		ID:        s.id,
		RuntimeID: s.runtimeID,
	}, nil
}

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

func (s *System) MigrationNotifications() <-chan hcsschema.OperationSystemMigrationNotificationInfo {
	return s.migrationNotifications
}

func (s *System) TransportBase() string {
	return s.transportBase
}

// BackendName is the diagnostic name used in unsupported-operation errors.
const BackendName = "openvmm"

// runtimeIDNamespace is the fixed namespace the shim-assigned runtime GUID is derived
// under. Derivation is deterministic so the same compute-system id always yields the same
// runtime id in logs and in tests.
var runtimeIDNamespace = guid.GUID{
	Data1: 0x6f0c9d3b,
	Data2: 0x8f1a,
	Data3: 0x4f2e,
	Data4: [8]byte{0x9c, 0x2d, 0x1b, 0x77, 0x5a, 0x30, 0xd4, 0xe1},
}

// errLauncherNotConfigured is returned when a backend is constructed without a process
// launcher in its [Deps].
var errLauncherNotConfigured = errors.New("no VM launcher is configured on the openvmm backend")

// errDialerNotConfigured is returned when a backend is constructed without a vmservice dialer.
var errDialerNotConfigured = errors.New("no vmservice dialer is configured on the openvmm backend")

// errTransportBaseMismatch reports a request whose hvsocket path differs from the advertised hybrid-vsock base.
var errTransportBaseMismatch = errors.New("the request hvsocket path differs from the transport base the openvmm backend advertises")

const createCleanupTimeout = 30 * time.Second

// VMLauncher starts and terminates the OpenVMM process.
type VMLauncher interface {
	// Launch spawns the OpenVMM child, waits for vmservice readiness, and returns the
	// AF_UNIX path to dial. The backend dials no other socket path.
	Launch(ctx context.Context, id string) (socketPath string, err error)
	// Terminate runs the termination ladder. It is idempotent.
	Terminate(ctx context.Context) error
}

// Deps are the inputs used to construct the OpenVMM backend.
type Deps struct {
	// Launcher spawns the VM host process and reports the socket to dial.
	Launcher VMLauncher
	// NewLauncher builds the launcher for one VM's derived config when Launcher is nil.
	NewLauncher func(*Config) VMLauncher
	// Dial builds the vmservice client from that socket path.
	Dial func(ctx context.Context, socketPath string) (vmservice.VMClient, io.Closer, error)
	// TransportBase is the hybrid-vsock base the request's hvsocket path must equal. The
	// direct create path overwrites it with the per-VM base derived from the VM ID, so a
	// value set on the Deps passed to newDirectCreate is not used there.
	TransportBase string
}

type backend struct {
	deps Deps
}

func (b *backend) CreateFromRequest(ctx context.Context, id string, request *vmservice.CreateVMRequest) (vmmanager.ComputeSystem, guid.GUID, error) {
	if request == nil || request.Config == nil {
		return nil, guid.GUID{}, fmt.Errorf("cannot create compute system %s: no vmservice create request", id)
	}
	if request.Config.HvsocketConfig == nil || request.Config.HvsocketConfig.Path != b.deps.TransportBase {
		return nil, guid.GUID{}, fmt.Errorf("cannot create compute system %s: request hvsocket path does not match advertised transport base: %w", id, errTransportBaseMismatch)
	}
	if b.deps.Launcher == nil {
		return nil, guid.GUID{}, fmt.Errorf("cannot create compute system %s: %w", id, errLauncherNotConfigured)
	}
	if b.deps.Dial == nil {
		return nil, guid.GUID{}, fmt.Errorf("cannot create compute system %s: %w", id, errDialerNotConfigured)
	}

	runtimeID, err := guid.NewV5(runtimeIDNamespace, []byte(id))
	if err != nil {
		return nil, guid.GUID{}, fmt.Errorf("failed to derive a runtime id for compute system %s: %w", id, err)
	}

	socketPath, err := b.deps.Launcher.Launch(ctx, id)
	if err != nil {
		return nil, guid.GUID{}, fmt.Errorf("failed to launch the VM host for compute system %s: %w", id, errors.Join(err, b.terminateLauncher(ctx)))
	}

	client, conn, err := b.deps.Dial(ctx, socketPath)
	if err != nil {
		if conn != nil {
			_ = conn.Close()
		}
		return nil, guid.GUID{}, fmt.Errorf("failed to dial vmservice for compute system %s: %w", id, errors.Join(err, b.terminateLauncher(ctx)))
	}
	var serial *serialRelay
	if config := request.Config.GetSerialConfig(); config != nil && len(config.GetPorts()) != 0 && config.GetPorts()[0].GetConnect() {
		serial, err = newSerialRelay(ctx, config.GetPorts()[0].GetSocketPath(), id)
		if err != nil {
			connErr := conn.Close()
			return nil, guid.GUID{}, fmt.Errorf("failed to prepare COM1 serial listener for compute system %s: %w", id, errors.Join(err, connErr, b.terminateLauncher(ctx)))
		}
	}

	if _, err := client.CreateVM(ctx, request); err != nil {
		var serialErr error
		if serial != nil {
			serialErr = serial.Close()
		}
		connErr := conn.Close()
		return nil, guid.GUID{}, fmt.Errorf("failed to create compute system %s: %w", id, errors.Join(err, serialErr, connErr, b.terminateLauncher(ctx)))
	}

	return newSystem(id, runtimeID, b.deps.TransportBase, client, conn, b.deps.Launcher, serial, nil), runtimeID, nil
}

func (b *backend) terminateLauncher(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), createCleanupTimeout)
	defer cancel()
	return b.deps.Launcher.Terminate(ctx)
}

// ErrModifyNotSupported names every hot-plug ModifySettingRequest this backend has no
// vmservice translation for. It wraps errdefs.ErrNotImplemented so a caller can match
// either sentinel with errors.Is.
var ErrModifyNotSupported = fmt.Errorf("this ModifySettingRequest has no vmservice translation on the %s backend: %w", BackendName, errdefs.ErrNotImplemented)

var errMalformedModifyRequest = errors.New("no usable ModifySettingRequest was supplied to the openvmm modify translation")

// BuildModifyResourceRequest translates one hot-plug ModifySettingRequest into the
// vmservice request that performs it. A SCSI add or remove and a Plan9 share add or remove
// translate; Plan9 shares become children of the aggregate virtio-fs device. A SCSI update
// and every other non-network path (vPCI, vPMem, VSMB, memory, processor, ...) is a named
// not-implemented error with a nil request; System.Modify handles network paths before
// this. It never returns a partial request beside an error.
func BuildModifyResourceRequest(req *hcsschema.ModifySettingRequest) (*vmservice.ModifyResourceRequest, error) {
	if req == nil {
		return nil, fmt.Errorf("cannot build a ModifyResourceRequest: %w", errMalformedModifyRequest)
	}
	if req.ResourcePath == "" {
		return nil, fmt.Errorf("ModifySettingRequest.ResourcePath is empty: %w", errMalformedModifyRequest)
	}
	if req.ResourcePath == resourcepaths.Plan9ShareResourcePath {
		return buildVirtioFSShareRequest(req)
	}
	if !isSCSIResourcePath(req.ResourcePath) {
		return nil, fmt.Errorf("ModifySettingRequest.ResourcePath %q has no vmservice modify translation: %w", req.ResourcePath, ErrModifyNotSupported)
	}

	controller, lun, err := ParseSCSIResourcePath(req.ResourcePath)
	if err != nil {
		return nil, err
	}

	switch req.RequestType {
	case guestrequest.RequestTypeAdd:
		return buildSCSIAddRequest(req, controller, lun)
	case guestrequest.RequestTypeRemove:
		return &vmservice.ModifyResourceRequest{
			Type: vmservice.ModifyType_REMOVE,
			Resource: &vmservice.ModifyResourceRequest_ScsiDisk{
				ScsiDisk: &vmservice.SCSIDisk{
					Controller: controller,
					Lun:        lun,
				},
			},
		}, nil
	default:
		return nil, fmt.Errorf("RequestType %q on SCSI resource path %q is not supported: %w", req.RequestType, req.ResourcePath, ErrModifyNotSupported)
	}
}

// buildVirtioFSShareRequest maps a Plan9 share request onto a child of the aggregate virtio-fs device.
func buildVirtioFSShareRequest(req *hcsschema.ModifySettingRequest) (*vmservice.ModifyResourceRequest, error) {
	var share hcsschema.Plan9Share
	switch s := req.Settings.(type) {
	case hcsschema.Plan9Share:
		share = s
	case *hcsschema.Plan9Share:
		if s == nil {
			return nil, fmt.Errorf("nil Plan9 share settings: %w", errMalformedModifyRequest)
		}
		share = *s
	default:
		return nil, fmt.Errorf("Plan9 share settings have type %T: %w", req.Settings, errMalformedModifyRequest)
	}
	if share.Name == "" {
		return nil, fmt.Errorf("Plan9 share has no name: %w", errMalformedModifyRequest)
	}

	switch req.RequestType {
	case guestrequest.RequestTypeAdd:
		out := &vmservice.VirtioFSShare{
			Name:     share.Name,
			RootPath: share.Path,
			ReadOnly: share.Flags&hcsschema.Plan9ShareFlagsReadOnly != 0,
		}
		if share.Flags&hcsschema.Plan9ShareFlagsRestrictFileAccess != 0 {
			if len(share.AllowedFiles) == 0 {
				return nil, fmt.Errorf("restricted Plan9 share %s lists no files: %w", share.Name, errMalformedModifyRequest)
			}
			out.AllowedNames = share.AllowedFiles
		}
		return &vmservice.ModifyResourceRequest{
			Type:     vmservice.ModifyType_ADD,
			Resource: &vmservice.ModifyResourceRequest_VirtiofsShare{VirtiofsShare: out},
		}, nil
	case guestrequest.RequestTypeRemove:
		return &vmservice.ModifyResourceRequest{
			Type:     vmservice.ModifyType_REMOVE,
			Resource: &vmservice.ModifyResourceRequest_VirtiofsShare{VirtiofsShare: &vmservice.VirtioFSShare{Name: share.Name}},
		}, nil
	default:
		return nil, fmt.Errorf("RequestType %q on Plan9 share %s is not supported: %w", req.RequestType, share.Name, ErrModifyNotSupported)
	}
}

func buildSCSIAddRequest(req *hcsschema.ModifySettingRequest, controller, lun uint32) (*vmservice.ModifyResourceRequest, error) {
	attachment, ok := req.Settings.(hcsschema.Attachment)
	if !ok {
		return nil, fmt.Errorf("SCSI add at controller %d LUN %d carries Settings of type %T, want hcsschema.Attachment: %w", controller, lun, req.Settings, errInvalidSCSIAttachment)
	}
	diskType, err := scsiDiskType(attachment, controller, lun)
	if err != nil {
		return nil, err
	}
	return &vmservice.ModifyResourceRequest{
		Type: vmservice.ModifyType_ADD,
		Resource: &vmservice.ModifyResourceRequest_ScsiDisk{
			ScsiDisk: &vmservice.SCSIDisk{
				Controller: controller,
				Lun:        lun,
				HostPath:   attachment.Path,
				Type:       diskType,
				ReadOnly:   attachment.ReadOnly,
			},
		},
	}, nil
}

var errInvalidSCSIAttachment = errors.New("the SCSI attachment cannot be represented by the OpenVMM runtime")

func scsiControllerIndex(controllerID string) (uint32, error) {
	for index, knownID := range guestrequest.ScsiControllerGuids {
		if strings.EqualFold(controllerID, knownID) {
			return uint32(index), nil
		}
	}
	return 0, fmt.Errorf("unknown SCSI controller %q: %w", controllerID, errInvalidSCSIAttachment)
}

func scsiDiskType(attachment hcsschema.Attachment, controller, lun uint32) (vmservice.DiskType, error) {
	if attachment.Path == "" {
		return 0, fmt.Errorf("SCSI attachment at controller %d LUN %d has an empty path: %w", controller, lun, errInvalidSCSIAttachment)
	}
	switch attachment.Type_ {
	case "VirtualDisk":
		switch strings.ToLower(filepath.Ext(attachment.Path)) {
		case ".vhd":
			return vmservice.DiskType_SCSI_DISK_TYPE_VHD1, nil
		case ".vhdx":
			return vmservice.DiskType_SCSI_DISK_TYPE_VHDX, nil
		}
	case "PassThru":
		return vmservice.DiskType_SCSI_DISK_TYPE_PHYSICAL, nil
	}
	return 0, fmt.Errorf("SCSI attachment at controller %d LUN %d has unsupported type %q or path %q: %w", controller, lun, attachment.Type_, attachment.Path, errInvalidSCSIAttachment)
}

// isSCSIResourcePath reports whether path is shaped like resourcepaths.SCSIResourceFormat
// (VirtualMachine/Devices/Scsi/...), without requiring the controller and LUN segments to
// be well-formed. It routes any SCSI-shaped path, malformed or not, to ParseSCSIResourcePath
// for the named-error verdict, and every other path to the not-implemented arm.
func isSCSIResourcePath(path string) bool {
	segments := strings.SplitN(path, "/", 4)
	return len(segments) >= 3 && segments[0] == "VirtualMachine" && segments[1] == "Devices" && segments[2] == "Scsi"
}

// ParseSCSIResourcePath resolves a ModifySettingRequest.ResourcePath built from
// resourcepaths.SCSIResourceFormat back to the controller index the guest sees (via the
// shared scsiControllerIndex lookup) and the LUN. An unrecognised GUID, a non-numeric LUN,
// a wrong segment name, and a path with trailing segments are each a distinct named error;
// none of them fall back to controller 0, which would attach a disk to the wrong controller.
func ParseSCSIResourcePath(path string) (controller uint32, lun uint32, err error) {
	segments := strings.Split(path, "/")
	if len(segments) != 6 || segments[0] != "VirtualMachine" || segments[1] != "Devices" || segments[2] != "Scsi" || segments[4] != "Attachments" {
		return 0, 0, fmt.Errorf("SCSI resource path %q does not match the %q shape: %w", path, resourcepaths.SCSIResourceFormat, errInvalidSCSIAttachment)
	}

	controller, err = scsiControllerIndex(segments[3])
	if err != nil {
		return 0, 0, err
	}

	lunValue, err := strconv.ParseUint(segments[5], 10, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("SCSI resource path %q has a non-numeric LUN %q: %w", path, segments[5], errInvalidSCSIAttachment)
	}
	return controller, uint32(lunValue), nil
}
