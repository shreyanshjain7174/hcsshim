//go:build windows && lcow && openvmm_prototype

package manager

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	hcsschema "github.com/Microsoft/hcsshim/internal/hcs/schema2"
	hcs "github.com/Microsoft/hcsshim/internal/hcs/v2"

	"github.com/Microsoft/go-winio/pkg/guid"
)

func TestSystemCloseOwnsResourcesOnceWithoutRemovingTransportBase(t *testing.T) {
	transportBase := filepath.Join(t.TempDir(), "hybrid.sock")
	const transportBaseContents = "transport base owned by the configured environment"
	if err := os.WriteFile(transportBase, []byte(transportBaseContents), 0600); err != nil {
		t.Fatalf("create transport artifact: %v", err)
	}

	serialPath := shortSerialSocketPath(t)
	serial, err := newSerialRelay(context.Background(), serialPath, "close-system")
	if err != nil {
		t.Fatalf("newSerialRelay: %v", err)
	}
	t.Cleanup(func() { _ = serial.Close() })

	client := &fakeModifyVMClient{}
	connection := &recordingCloser{}
	launcher := &fakeDirectLauncher{}
	system := newSystem("close-system", guid.GUID{}, transportBase, client, connection, launcher, serial, nil)

	if err := system.CloseCtx(context.Background()); err != nil {
		t.Fatalf("first CloseCtx: %v", err)
	}
	if client.quitCallCount() != 1 {
		t.Fatalf("Quit calls = %d, want 1", client.quitCallCount())
	}
	if connection != nil && connection.closeCalls != 1 {
		t.Fatalf("connection close calls = %d, want 1", connection.closeCalls)
	}
	if launcher.terminateCalls != 1 {
		t.Fatalf("launcher terminate calls = %d, want 1", launcher.terminateCalls)
	}
	contents, err := os.ReadFile(transportBase)
	if err != nil {
		t.Fatalf("transport base was removed by CloseCtx: %v", err)
	}
	if string(contents) != transportBaseContents {
		t.Fatalf("transport base contents = %q, want %q", contents, transportBaseContents)
	}
	if _, err := os.Stat(serialPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("serial socket survived CloseCtx: %v", err)
	}
	if !system.closed || !serial.closed {
		t.Fatalf("system.closed=%v serial.closed=%v, want both true", system.closed, serial.closed)
	}

	if err := system.CloseCtx(context.Background()); err != nil {
		t.Fatalf("second CloseCtx: %v", err)
	}
	if client.quitCallCount() != 1 || connection.closeCalls != 1 || launcher.terminateCalls != 1 {
		t.Fatalf("second CloseCtx repeated cleanup: quit=%d connection=%d launcher=%d", client.quitCallCount(), connection.closeCalls, launcher.terminateCalls)
	}
}

// newCloseTestSystem builds a System whose only external dependencies are the fakes the
// close tests drive. transportBase is a naming prefix, not a filesystem object System owns.
func newCloseTestSystem(t *testing.T, id string, client *fakeModifyVMClient, connection io.Closer, launcher *fakeDirectLauncher) *System {
	t.Helper()
	transportBase := filepath.Join(t.TempDir(), "hybrid.sock")
	return newSystem(id, guid.GUID{}, transportBase, client, connection, launcher, nil, nil)
}

// blockingCloser is an io.Closer that parks in Close until released, so a test can prove
// a wedged resource does not hold up the rungs that follow it.
type blockingCloser struct {
	mu      sync.Mutex
	calls   int
	entered chan struct{}
	release <-chan struct{}
}

func (c *blockingCloser) Close() error {
	c.mu.Lock()
	c.calls++
	entered, release := c.entered, c.release
	c.entered = nil
	c.mu.Unlock()
	if entered != nil {
		close(entered)
	}
	if release != nil {
		<-release
	}
	return nil
}

func (c *blockingCloser) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// assertBlockedCloseRungKeepsLauncherProgress is the shared body of the serial and
// connection variants: the blocked rung times out, every later rung still runs, an
// immediate retry starts no second worker, and the eventual completion is observed.
func assertBlockedCloseRungKeepsLauncherProgress(t *testing.T, blocked *blockingCloser, entered <-chan struct{}, release chan struct{}, system *System, launcher *fakeDirectLauncher, connection *recordingCloser) {
	t.Helper()

	oldTimeout := systemCleanupTimeout
	systemCleanupTimeout = 25 * time.Millisecond
	restoreTimeout := func() { systemCleanupTimeout = oldTimeout }
	t.Cleanup(restoreTimeout)

	if err := system.CloseCtx(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first CloseCtx = %v, want a deadline for the blocked rung", err)
	}
	<-entered
	if launcher.terminateCalls != 1 {
		t.Fatalf("launcher terminate calls = %d, want 1: a blocked close must not starve the launcher", launcher.terminateCalls)
	}
	if connection != nil && connection.closeCalls != 1 {
		t.Fatalf("connection close calls = %d, want 1", connection.closeCalls)
	}
	if system.closed {
		t.Fatal("system.closed = true while a close rung is still blocked")
	}

	if err := system.CloseCtx(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("retry CloseCtx = %v, want a deadline", err)
	}
	if blocked.callCount() != 1 {
		t.Fatalf("blocked close calls = %d, want exactly one in-flight worker", blocked.callCount())
	}
	if launcher.terminateCalls != 1 {
		t.Fatalf("launcher terminate calls = %d, want 1: a completed rung must not repeat", launcher.terminateCalls)
	}

	restoreTimeout()
	close(release)
	if err := system.CloseCtx(context.Background()); err != nil {
		t.Fatalf("final CloseCtx: %v", err)
	}
	if !system.closed {
		t.Fatal("system.closed = false after the blocked close completed")
	}
	if blocked.callCount() != 1 {
		t.Fatalf("blocked close calls = %d, want 1 across all three attempts", blocked.callCount())
	}
}

func TestSystemCloseBlockedSerialCloseStillTerminatesLauncher(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	blocked := &blockingCloser{entered: entered, release: release}
	connection := &recordingCloser{}
	launcher := &fakeDirectLauncher{}
	system := newCloseTestSystem(t, "blocked-serial", &fakeModifyVMClient{}, connection, launcher)
	system.serialClose = newAsyncCloser(blocked.Close)

	assertBlockedCloseRungKeepsLauncherProgress(t, blocked, entered, release, system, launcher, connection)
}

func TestSystemCloseBlockedConnCloseStillTerminatesLauncher(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	connection := &blockingCloser{entered: entered, release: release}
	launcher := &fakeDirectLauncher{}
	system := newCloseTestSystem(t, "blocked-conn", &fakeModifyVMClient{}, connection, launcher)

	assertBlockedCloseRungKeepsLauncherProgress(t, connection, entered, release, system, launcher, nil)
}

func TestSystemCloseRunsLaterRungsWhenQuitHangs(t *testing.T) {
	blocked := make(chan struct{})
	t.Cleanup(func() { close(blocked) })

	client := &fakeModifyVMClient{quitBlock: blocked}
	connection := &recordingCloser{}
	launcher := &fakeDirectLauncher{}
	system := newCloseTestSystem(t, "hung-quit", client, connection, launcher)

	done := make(chan error, 1)
	go func() { done <- system.CloseCtx(context.Background()) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("CloseCtx with a hung Quit: want an error, got nil")
		}
	case <-time.After(60 * time.Second):
		t.Fatal("CloseCtx never returned while Quit was hung")
	}

	if connection.closeCalls != 1 {
		t.Fatalf("connection close calls = %d, want 1", connection.closeCalls)
	}
	if launcher.terminateCalls != 1 {
		t.Fatalf("launcher terminate calls = %d, want 1", launcher.terminateCalls)
	}
	if !system.closed {
		t.Fatal("system.closed = false, want true: a hung Quit must not block the terminal state")
	}
}

func TestSystemCloseIgnoresCallerCancellation(t *testing.T) {
	client := &fakeModifyVMClient{}
	connection := &recordingCloser{}
	launcher := &fakeDirectLauncher{}
	system := newCloseTestSystem(t, "cancelled-close", client, connection, launcher)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := system.CloseCtx(ctx); err != nil {
		t.Fatalf("CloseCtx with a cancelled caller context: %v", err)
	}
	if client.quitCallCount() != 1 || connection.closeCalls != 1 || launcher.terminateCalls != 1 {
		t.Fatalf("cleanup skipped: quit=%d connection=%d launcher=%d", client.quitCallCount(), connection.closeCalls, launcher.terminateCalls)
	}
	if !system.closed {
		t.Fatal("system.closed = false, want true")
	}
}

func TestSystemCloseRetriesFailedRungsAndPreservesRealWaitResult(t *testing.T) {
	exited := errors.New("vm exited")
	released := make(chan struct{})
	client := &fakeModifyVMClient{waitBlock: released, waitErr: exited}
	connection := &recordingCloser{}
	launcher := &fakeDirectLauncher{terminateErrs: []error{errors.New("terminate refused")}}
	system := newCloseTestSystem(t, "retry-close", client, connection, launcher)

	waitResult := make(chan error, 1)
	go func() { waitResult <- system.WaitCtx(context.Background()) }()

	if err := system.CloseCtx(context.Background()); err == nil {
		t.Fatal("CloseCtx with a failing launcher: want an error, got nil")
	}
	if system.closed {
		t.Fatal("system.closed = true after a failed launcher rung, want false")
	}
	select {
	case err := <-waitResult:
		t.Fatalf("WaitCtx returned %v before the VM reported an exit", err)
	case <-time.After(200 * time.Millisecond):
	}

	close(released)
	if err := <-waitResult; !errors.Is(err, exited) {
		t.Fatalf("WaitCtx = %v, want %v", err, exited)
	}

	if err := system.CloseCtx(context.Background()); err != nil {
		t.Fatalf("second CloseCtx: %v", err)
	}
	if !system.closed {
		t.Fatal("system.closed = false after a successful retry, want true")
	}
	if launcher.terminateCalls != 2 {
		t.Fatalf("launcher terminate calls = %d, want 2: the failed rung must be retried", launcher.terminateCalls)
	}
	if client.quitCallCount() != 1 || connection.closeCalls != 1 {
		t.Fatalf("successful rungs repeated: quit=%d connection=%d", client.quitCallCount(), connection.closeCalls)
	}
	if err := system.ExitError(); !errors.Is(err, exited) {
		t.Fatalf("ExitError = %v, want %v: CloseCtx must not overwrite a real WaitVM result", err, exited)
	}
}

func TestSystemCloseSerializesConcurrentCallers(t *testing.T) {
	client := &fakeModifyVMClient{}
	connection := &recordingCloser{}
	launcher := &fakeDirectLauncher{}
	system := newCloseTestSystem(t, "concurrent-close", client, connection, launcher)

	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = system.CloseCtx(context.Background())
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent CloseCtx %d: %v", i, err)
		}
	}
	if client.quitCallCount() != 1 || connection.closeCalls != 1 || launcher.terminateCalls != 1 {
		t.Fatalf("concurrent CloseCtx repeated cleanup: quit=%d connection=%d launcher=%d", client.quitCallCount(), connection.closeCalls, launcher.terminateCalls)
	}
}

func TestSystemTerminateIgnoresCallerCancellation(t *testing.T) {
	client := &fakeModifyVMClient{}
	system := newCloseTestSystem(t, "cancelled-terminate", client, &recordingCloser{}, &fakeDirectLauncher{})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := system.Terminate(ctx); err != nil {
		t.Fatalf("Terminate with a cancelled caller context: %v", err)
	}
	if client.teardownCallCount() != 1 {
		t.Fatalf("TeardownVM calls = %d, want 1", client.teardownCallCount())
	}
	if !system.terminated {
		t.Fatal("system.terminated = false, want true")
	}

	if err := system.Terminate(context.Background()); err != nil {
		t.Fatalf("second Terminate: %v", err)
	}
	if client.teardownCallCount() != 1 {
		t.Fatalf("second Terminate repeated teardown: TeardownVM calls = %d, want 1", client.teardownCallCount())
	}
}

func TestSystemCloseCancelsInFlightStartAndRunsLaterRungs(t *testing.T) {
	entered := make(chan struct{})
	blocked := make(chan struct{})
	client := &fakeModifyVMClient{resumeEntered: entered, resumeBlock: blocked}
	connection := &recordingCloser{}
	launcher := &fakeDirectLauncher{}
	system := newCloseTestSystem(t, "close-start", client, connection, launcher)

	startDone := make(chan error, 1)
	go func() { startDone <- system.Start(context.Background()) }()
	<-entered

	if err := system.CloseCtx(context.Background()); err != nil {
		t.Fatalf("CloseCtx: %v", err)
	}
	if err := <-startDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("Start = %v, want context cancellation", err)
	}
	if connection.closeCalls != 1 || launcher.terminateCalls != 1 || !system.closed {
		t.Fatalf("cleanup: connection=%d launcher=%d closed=%v", connection.closeCalls, launcher.terminateCalls, system.closed)
	}
}

func TestSystemFailedCloseKeepsMutationAdmissionClosed(t *testing.T) {
	system := newCloseTestSystem(t, "failed-close", &fakeModifyVMClient{}, &recordingCloser{}, &fakeDirectLauncher{
		terminateErrs: []error{errors.New("terminate failed")},
	})

	if err := system.CloseCtx(context.Background()); err == nil {
		t.Fatal("first CloseCtx succeeded")
	}
	if !system.closing || system.closed {
		t.Fatalf("closing=%v closed=%v", system.closing, system.closed)
	}
	if err := system.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "is closed") {
		t.Fatalf("Start after failed CloseCtx = %v, want closed precondition", err)
	}
	if err := system.CloseCtx(context.Background()); err != nil {
		t.Fatalf("retry CloseCtx: %v", err)
	}
}

func TestSystemNetworkReleaseTimeoutDoesNotStarveLaterRungsOrDuplicateWorker(t *testing.T) {
	oldTimeout := systemCleanupTimeout
	systemCleanupTimeout = 25 * time.Millisecond
	t.Cleanup(func() { systemCleanupTimeout = oldTimeout })

	entered := make(chan struct{})
	blocked := make(chan struct{})
	binder := &fakeEndpointPortBinder{unbindEntered: entered, unbindBlock: blocked}
	connection := &recordingCloser{}
	launcher := &fakeDirectLauncher{}
	system := newCloseTestSystem(t, "blocked-network", &fakeModifyVMClient{}, connection, launcher)
	system.binder = binder
	nicID := guid.GUID{Data1: 1}
	system.networkBindings[nicID.String()] = networkBinding{
		endpointID: "endpoint",
		portID:     guid.GUID{Data1: 2},
	}

	firstDone := make(chan error, 1)
	go func() { firstDone <- system.CloseCtx(context.Background()) }()
	<-entered
	if err := <-firstDone; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first CloseCtx = %v, want deadline", err)
	}
	if connection.closeCalls != 1 || launcher.terminateCalls != 1 {
		t.Fatalf("later rungs skipped: connection=%d launcher=%d", connection.closeCalls, launcher.terminateCalls)
	}
	if err := system.CloseCtx(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second CloseCtx = %v, want deadline", err)
	}
	if binder.unbindCallCount() != 1 {
		t.Fatalf("unbind calls=%d, want one in-flight worker", binder.unbindCallCount())
	}
	close(blocked)
	select {
	case <-system.networkReleaseDone:
	case <-time.After(time.Second):
		t.Fatal("network release worker did not finish")
	}
	if err := system.CloseCtx(context.Background()); err != nil {
		t.Fatalf("final CloseCtx: %v", err)
	}
	if !system.closed {
		t.Fatal("system did not close after network release completed")
	}
}

// TestSystemCloseRejectsQueuedMutationsWhileAnUncooperativeBindHoldsAdmission pins the
// admission rule an operation mutex cannot express: a current operation that ignores its
// cancelled context keeps its own admission slot, but it must not make a queued or a new
// mutation wait for it once close has begun.
func TestSystemCloseRejectsQueuedMutationsWhileAnUncooperativeBindHoldsAdmission(t *testing.T) {
	oldTimeout := systemCleanupTimeout
	systemCleanupTimeout = 25 * time.Millisecond
	t.Cleanup(func() { systemCleanupTimeout = oldTimeout })

	bindEntered := make(chan struct{})
	bindBlock := make(chan struct{})
	binder := &fakeEndpointPortBinder{
		bindEntered: bindEntered,
		bindBlock:   bindBlock,
		bindResults: []struct {
			bound BoundEndpoint
			err   error
		}{{err: errors.New("bind released")}},
	}
	launcher := &fakeDirectLauncher{}
	system := newCloseTestSystem(t, "closure-admission", &fakeModifyVMClient{}, &recordingCloser{}, launcher)
	system.binder = binder

	request := networkAddRequest(&hcsschema.NetworkAdapter{EndpointId: testEndpointID, MacAddress: testMAC})

	blockedDone := make(chan error, 1)
	go func() { blockedDone <- system.Modify(context.Background(), request) }()
	<-bindEntered

	queuedStarted := make(chan struct{})
	queuedDone := make(chan error, 1)
	go func() {
		close(queuedStarted)
		queuedDone <- system.Modify(context.Background(), request)
	}()
	<-queuedStarted

	// The network release worker cannot run while the blocked add holds the transaction
	// lock, so close reports that deadline - and still reaches the launcher rung.
	if err := system.CloseCtx(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("CloseCtx = %v, want the blocked network release deadline", err)
	}
	if launcher.terminateCalls != 1 {
		t.Fatalf("launcher terminate calls = %d, want close to progress through the launcher rung", launcher.terminateCalls)
	}

	select {
	case err := <-queuedDone:
		if !errors.Is(err, hcs.ErrAlreadyClosed) {
			t.Fatalf("queued Modify = %v, want the closed verdict", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the queued Modify waited on an operation that will not cooperate with cancellation")
	}

	if err := system.Modify(context.Background(), request); !errors.Is(err, hcs.ErrAlreadyClosed) {
		t.Fatalf("Modify after close = %v, want the closed verdict", err)
	}
	if binder.bindCallCount() != 1 {
		t.Fatalf("bind calls = %d, want only the operation that was admitted before close", binder.bindCallCount())
	}

	close(bindBlock)
	select {
	case err := <-blockedDone:
		if err == nil {
			t.Fatal("the released Modify reported success for a bind that failed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the released Modify never returned")
	}
	select {
	case <-system.networkReleaseDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the network release worker never finished")
	}
}
