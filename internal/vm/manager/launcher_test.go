//go:build windows && (lcow || wcow) && openvmm_prototype

package manager

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Microsoft/hcsshim/internal/safefile"

	"golang.org/x/sys/windows"
)

func shortLauncherSocketPath(t *testing.T) string {
	t.Helper()
	path := filepath.Join(os.TempDir(), fmt.Sprintf("ovmm-launcher-%d.sock", time.Now().UnixNano()))
	t.Cleanup(func() { _ = os.Remove(path) })
	return path
}

func TestStartProcessAssignmentFailureReapsChild(t *testing.T) {
	want := errors.New("assignment failed")
	t.Setenv("GO_WANT_LAUNCHER_HELPER", "1")
	oldCreate := createKillOnCloseJob
	oldAssign := assignProcessToJob
	var process *os.Process
	createKillOnCloseJob = func() (windows.Handle, error) { return windows.Handle(1), nil }
	assignProcessToJob = func(_ windows.Handle, child *os.Process) error {
		process = child
		return want
	}
	t.Cleanup(func() {
		createKillOnCloseJob = oldCreate
		assignProcessToJob = oldAssign
		if process != nil {
			_ = process.Kill()
		}
	})

	child, err := startProcess(os.Args[0], []string{"-test.run=^TestLauncherHelperProcess$"})
	if !errors.Is(err, want) || child != nil {
		t.Fatalf("child=%v error=%v, want nil child wrapping %v", child, err, want)
	}
	if process == nil {
		t.Fatal("assignment hook did not observe the child")
	}
	if err := process.Kill(); !errors.Is(err, os.ErrProcessDone) && !errors.Is(err, syscall.EINVAL) {
		t.Fatalf("child survived failed job assignment: Kill = %v", err)
	}
}

func TestLauncherHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_LAUNCHER_HELPER") != "1" {
		return
	}
	select {}
}

// dialFailure is a probe or readiness dialer that always reports the same connect error.
func dialFailure(err error) readinessDial {
	return func(context.Context, string, string) (net.Conn, error) { return nil, err }
}

// dialSuccess answers every connect with a connection the caller can only close, which is
// exactly what readiness does with it.
func dialSuccess() readinessDial {
	return func(context.Context, string, string) (net.Conn, error) {
		local, remote := net.Pipe()
		_ = remote.Close()
		return local, nil
	}
}

type fakeOwnedChild struct {
	exited   chan struct{}
	stopOnce sync.Once
	killed   atomic.Bool
	closed   atomic.Bool
	closeFn  func() error
}

func newFakeOwnedChild() *fakeOwnedChild {
	return &fakeOwnedChild{exited: make(chan struct{})}
}

func (c *fakeOwnedChild) Exited() <-chan struct{} { return c.exited }

func (c *fakeOwnedChild) Wait(ctx context.Context) error {
	select {
	case <-c.exited:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *fakeOwnedChild) Kill() error {
	c.killed.Store(true)
	c.stop()
	return nil
}

func (c *fakeOwnedChild) Close() error {
	c.closed.Store(true)
	if c.closeFn != nil {
		return c.closeFn()
	}
	return nil
}

func (c *fakeOwnedChild) stop() {
	c.stopOnce.Do(func() {
		close(c.exited)
	})
}

func swapStartProcessForTest(t *testing.T, start func(string, []string) (ownedChild, error)) {
	t.Helper()
	previous := startProcess
	startProcess = start
	t.Cleanup(func() { startProcess = previous })
}

func TestLaunchRetainsTheHostClaimAndOwnsTheSocketUntilTerminate(t *testing.T) {
	dir := t.TempDir()
	socketPath := shortLauncherSocketPath(t)
	child := newFakeOwnedChild()
	swapStartProcessForTest(t, func(string, []string) (ownedChild, error) {
		// The real child binds the socket; the fake stands in for that one side effect.
		listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
		if err != nil {
			return nil, err
		}
		listener.SetUnlinkOnClose(false)
		child.closeFn = listener.Close
		return child, nil
	})

	launcher := newLauncher(&Config{OpenVMMBinaryPath: filepath.Join(dir, "openvmm.exe"), VMServiceSocket: socketPath})
	launcher.probeDial = dialFailure(os.ErrNotExist)
	launcher.readyDial = systemDial

	got, err := launcher.Launch(context.Background(), "sandbox")
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if got != socketPath {
		t.Fatalf("Launch returned %q, want %q", got, socketPath)
	}

	competing, claimErr := acquireHostSocketClaim(socketPath)
	if competing != nil {
		_ = competing.Release()
	}
	if !errors.Is(claimErr, errVMServiceSocketClaimed) {
		t.Fatalf("a competing claim during the live lifecycle = %v, want %v", claimErr, errVMServiceSocketClaimed)
	}

	if err := os.Remove(socketPath); err == nil {
		t.Fatal("the owned VM service socket was removable by pathname while the launcher owned it")
	}
	replacement := filepath.Join(dir, "replacement")
	if err := os.WriteFile(replacement, []byte("replacement"), 0o600); err != nil {
		t.Fatalf("writing the replacement: %v", err)
	}
	if err := os.Rename(replacement, socketPath); err == nil {
		t.Fatal("the owned VM service socket was replaceable while the launcher owned it")
	}

	child.stop()
	if err := launcher.Terminate(context.Background()); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if !child.closed.Load() {
		t.Fatal("Terminate did not close the owned child handles")
	}
	if _, err := os.Stat(socketPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the owned VM service socket survived Terminate: %v", err)
	}
	if _, err := os.Stat(socketClaimPath(socketPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the host claim survived Terminate: %v", err)
	}

	after, err := acquireHostSocketClaim(socketPath)
	if err != nil {
		t.Fatalf("the host claim was not released after Terminate: %v", err)
	}
	if err := after.Release(); err != nil {
		t.Fatalf("releasing the post-Terminate claim: %v", err)
	}
}

func TestLaunchReleasesTheHostClaimWhenTheSpawnFails(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "vmservice.sock")
	spawnErr := errors.New("spawn failed")
	swapStartProcessForTest(t, func(string, []string) (ownedChild, error) { return nil, spawnErr })

	launcher := newLauncher(&Config{OpenVMMBinaryPath: filepath.Join(dir, "openvmm.exe"), VMServiceSocket: socketPath})
	launcher.probeDial = dialFailure(os.ErrNotExist)
	launcher.readyDial = dialSuccess()

	if _, err := launcher.Launch(context.Background(), "sandbox"); !errors.Is(err, spawnErr) {
		t.Fatalf("Launch error = %v, want %v", err, spawnErr)
	}
	if _, err := os.Stat(socketClaimPath(socketPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the host claim survived a failed spawn: %v", err)
	}
	claim, err := acquireHostSocketClaim(socketPath)
	if err != nil {
		t.Fatalf("the host claim was not released after a failed spawn: %v", err)
	}
	if err := claim.Release(); err != nil {
		t.Fatalf("releasing the claim: %v", err)
	}
}

func TestLaunchRetainsFailedPreChildClaimReleaseForTerminateRetry(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "vmservice.sock")
	spawnErr := errors.New("spawn failed")
	releaseErr := errors.New("claim close failed")
	swapStartProcessForTest(t, func(string, []string) (ownedChild, error) { return nil, spawnErr })

	previousAcquire := acquireSocketClaim
	acquireSocketClaim = func(path string) (*hostSocketClaim, error) {
		claim, err := acquireHostSocketClaim(path)
		if err != nil {
			return nil, err
		}
		closeHandle := claim.close
		calls := 0
		claim.close = func(handle windows.Handle) error {
			calls++
			if calls == 1 {
				return releaseErr
			}
			return closeHandle(handle)
		}
		return claim, nil
	}
	t.Cleanup(func() { acquireSocketClaim = previousAcquire })

	launcher := newLauncher(&Config{OpenVMMBinaryPath: filepath.Join(dir, "openvmm.exe"), VMServiceSocket: socketPath})
	if _, err := launcher.Launch(context.Background(), "sandbox"); !errors.Is(err, spawnErr) || !errors.Is(err, releaseErr) {
		t.Fatalf("Launch error = %v, want spawn and claim-release failures", err)
	}
	if _, err := acquireHostSocketClaim(socketPath); !errors.Is(err, errVMServiceSocketClaimed) {
		t.Fatalf("competing claim after failed release = %v, want %v", err, errVMServiceSocketClaimed)
	}

	if err := launcher.Terminate(context.Background()); err != nil {
		t.Fatalf("Terminate retry: %v", err)
	}
	claim, err := acquireHostSocketClaim(socketPath)
	if err != nil {
		t.Fatalf("claim after Terminate retry: %v", err)
	}
	if err := claim.Release(); err != nil {
		t.Fatalf("release final claim: %v", err)
	}
}

func TestTerminateRetainsClaimWhenSocketExistsWithoutCapturedOwner(t *testing.T) {
	socketPath := shortLauncherSocketPath(t)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatalf("create socket: %v", err)
	}
	listener.SetUnlinkOnClose(false)
	t.Cleanup(func() { _ = listener.Close() })

	claim, err := acquireHostSocketClaim(socketPath)
	if err != nil {
		t.Fatalf("acquire host claim: %v", err)
	}
	child := newFakeOwnedChild()
	child.stop()
	launcher := newLauncher(&Config{VMServiceSocket: socketPath})
	launcher.child = child
	launcher.socketPath = socketPath
	launcher.claim = claim

	if err := launcher.Terminate(context.Background()); err == nil {
		t.Fatal("Terminate without a captured socket owner = nil, want a cleanup failure")
	}
	if _, err := os.Lstat(socketPath); err != nil {
		t.Fatalf("uncertain socket pathname was removed: %v", err)
	}
	if _, err := acquireHostSocketClaim(socketPath); !errors.Is(err, errVMServiceSocketClaimed) {
		t.Fatalf("competing claim after uncertain cleanup = %v, want %v", err, errVMServiceSocketClaimed)
	}
}

func TestLaunchRefusesWhileAnotherProcessHoldsTheHostClaim(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "vmservice.sock")

	held, err := acquireHostSocketClaim(socketPath)
	if err != nil {
		t.Fatalf("acquiring the competing claim: %v", err)
	}
	t.Cleanup(func() { _ = held.Release() })

	spawned := false
	swapStartProcessForTest(t, func(string, []string) (ownedChild, error) {
		spawned = true
		return newFakeOwnedChild(), nil
	})

	launcher := newLauncher(&Config{OpenVMMBinaryPath: filepath.Join(dir, "openvmm.exe"), VMServiceSocket: socketPath})
	launcher.probeDial = dialFailure(os.ErrNotExist)
	launcher.readyDial = dialSuccess()

	if _, err := launcher.Launch(context.Background(), "sandbox"); !errors.Is(err, errVMServiceSocketClaimed) {
		t.Fatalf("Launch error = %v, want %v", err, errVMServiceSocketClaimed)
	}
	if spawned {
		t.Fatal("a child was spawned while another process held the host claim")
	}
}

func TestLaunchReadinessFailureTerminatesChild(t *testing.T) {
	oldGrace := gracefulExitBudget
	gracefulExitBudget = time.Millisecond
	t.Cleanup(func() { gracefulExitBudget = oldGrace })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	child := newFakeOwnedChild()
	swapStartProcessForTest(t, func(string, []string) (ownedChild, error) {
		cancel()
		return child, nil
	})
	socketPath := shortLauncherSocketPath(t)
	launcher := newLauncher(&Config{
		OpenVMMBinaryPath: os.Args[0],
		VMServiceSocket:   socketPath,
		HybridVsockBase:   shortHybridBasePath(t),
	})
	launcher.probeDial = dialFailure(os.ErrNotExist)

	if path, err := launcher.Launch(ctx, "sandbox"); path != "" || !errors.Is(err, context.Canceled) {
		t.Fatalf("Launch = %q, %v, want readiness cancellation", path, err)
	}
	if !child.killed.Load() || !child.closed.Load() {
		t.Fatalf("child killed=%v closed=%v, want both after readiness failure", child.killed.Load(), child.closed.Load())
	}
	claim, err := acquireHostSocketClaim(socketPath)
	if err != nil {
		t.Fatalf("claim survived readiness failure: %v", err)
	}
	if err := claim.Release(); err != nil {
		t.Fatalf("release final claim: %v", err)
	}
}

func shortHybridBasePath(t *testing.T) string {
	t.Helper()
	path := filepath.Join(os.TempDir(), fmt.Sprintf("ovmm-hv-%d", time.Now().UnixNano()))
	t.Cleanup(func() { _ = os.Remove(path) })
	return path
}

func bindForTest(path string) (*net.UnixListener, error) {
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	l.SetUnlinkOnClose(false)
	return l, nil
}

// fakeChildBindingBoth stands in for OpenVMM, which binds both the VM service socket and
// the hybrid-vsock base and unlinks neither.
func fakeChildBindingBoth(t *testing.T, child *fakeOwnedChild, socketPath, basePath string, started *bool) {
	t.Helper()
	swapStartProcessForTest(t, func(string, []string) (ownedChild, error) {
		*started = true
		vm, err := bindForTest(socketPath)
		if err != nil {
			return nil, err
		}
		hv, err := bindForTest(basePath)
		if err != nil {
			_ = vm.Close()
			return nil, err
		}
		child.closeFn = func() error { return errors.Join(vm.Close(), hv.Close()) }
		return child, nil
	})
}

func hybridTestLauncher(t *testing.T, socketPath, basePath string) *openvmmLauncher {
	t.Helper()
	launcher := newLauncher(&Config{
		OpenVMMBinaryPath: filepath.Join(t.TempDir(), "openvmm.exe"),
		VMServiceSocket:   socketPath,
		HybridVsockBase:   basePath,
	})
	launcher.readyDial = systemDial
	return launcher
}

func TestTerminateRemovesTheHybridBaseTheChildBound(t *testing.T) {
	socketPath, basePath := shortLauncherSocketPath(t), shortHybridBasePath(t)
	child := newFakeOwnedChild()
	var started bool
	fakeChildBindingBoth(t, child, socketPath, basePath, &started)
	launcher := hybridTestLauncher(t, socketPath, basePath)
	launcher.probeDial = dialFailure(os.ErrNotExist)

	if _, err := launcher.Launch(context.Background(), "sandbox"); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	child.stop()
	if err := launcher.Terminate(context.Background()); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if _, err := os.Lstat(basePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the hybrid-vsock base survived termination: %v", err)
	}
	if _, err := os.Lstat(socketPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the VM service socket survived termination: %v", err)
	}
}

func TestLaunchClearsADeadHybridBase(t *testing.T) {
	socketPath, basePath := shortLauncherSocketPath(t), shortHybridBasePath(t)
	stale, err := bindForTest(basePath)
	if err != nil {
		t.Fatal(err)
	}
	_ = stale.Close()

	child := newFakeOwnedChild()
	var started bool
	fakeChildBindingBoth(t, child, socketPath, basePath, &started)
	launcher := hybridTestLauncher(t, socketPath, basePath)
	launcher.probeDial = dialFailure(windows.WSAECONNREFUSED)

	if _, err := launcher.Launch(context.Background(), "sandbox"); err != nil {
		t.Fatalf("Launch over a dead hybrid-vsock base: %v", err)
	}
	child.stop()
	if err := launcher.Terminate(context.Background()); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
}

func TestLaunchRefusesALiveHybridBase(t *testing.T) {
	socketPath, basePath := shortLauncherSocketPath(t), shortHybridBasePath(t)
	live, err := bindForTest(basePath)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()

	child := newFakeOwnedChild()
	var started bool
	fakeChildBindingBoth(t, child, socketPath, basePath, &started)
	launcher := hybridTestLauncher(t, socketPath, basePath)
	launcher.probeDial = systemDial

	_, err = launcher.Launch(context.Background(), "sandbox")
	if !errors.Is(err, errVMServiceSocketInUse) {
		t.Fatalf("Launch with a live hybrid-vsock base = %v, want %v", err, errVMServiceSocketInUse)
	}
	if started {
		t.Fatal("OpenVMM was started while another VM still listened on the hybrid-vsock base")
	}
	claim, err := acquireHostSocketClaim(socketPath)
	if err != nil {
		t.Fatalf("the refused launch kept the host claim: %v", err)
	}
	_ = claim.Release()
}

// TestRetryAfterAFailedClaimReleaseIsNotLockedOut verifies that a same-ID retry in the
// same shim can take the VM service socket claim back after a failed launch could not
// release it. The controller stays in StateNotCreated after a failed create, so
// containerd may retry, and a claim stranded by the first attempt must not turn that
// retry into a permanent errVMServiceSocketClaimed refusal.
func TestRetryAfterAFailedClaimReleaseIsNotLockedOut(t *testing.T) {
	socketPath, basePath := shortLauncherSocketPath(t), shortHybridBasePath(t)
	releaseFailure := errors.New("injected CloseHandle failure")

	var attempts int
	swapStartProcessForTest(t, func(string, []string) (ownedChild, error) {
		attempts++
		return nil, errors.New("injected spawn failure")
	})

	previous := acquireSocketClaim
	acquireSocketClaim = func(path string) (*hostSocketClaim, error) {
		claim, err := acquireHostSocketClaim(path)
		if err == nil && attempts == 0 {
			realClose := claim.close
			claim.close = func(h windows.Handle) error {
				// Fail the first release only. The handle stays open, as it does after
				// a real CloseHandle failure, and the retry's own release closes it.
				claim.close = realClose
				return releaseFailure
			}
		}
		return claim, err
	}
	t.Cleanup(func() { acquireSocketClaim = previous })

	first := newLauncher(&Config{
		OpenVMMBinaryPath: os.Args[0],
		VMServiceSocket:   socketPath,
		HybridVsockBase:   basePath,
	})
	if _, err := first.Launch(context.Background(), "sandbox"); err == nil {
		t.Fatal("first launch unexpectedly succeeded")
	}

	// The retry is a launch for the same VM through a launcher built the way the
	// direct create path builds one for every create.
	second := newLauncher(&Config{
		OpenVMMBinaryPath: os.Args[0],
		VMServiceSocket:   socketPath,
		HybridVsockBase:   basePath,
	})
	_, err := second.Launch(context.Background(), "sandbox")
	if errors.Is(err, errVMServiceSocketClaimed) {
		t.Fatalf("the retry was locked out by the first attempt's stranded claim: %v", err)
	}
}

// TestTerminateIsIdempotentWhenCallsOverlap verifies that concurrent Terminate calls
// on one launcher all succeed and leave nothing behind. System and a failed Launch can
// both reach Terminate for the same child, and a loser that reports a sharing violation
// or skips releasing the host claim would strand the next shim's launch.
func TestTerminateIsIdempotentWhenCallsOverlap(t *testing.T) {
	const callers = 8
	for iteration := 0; iteration < 25; iteration++ {
		socketPath, basePath := shortLauncherSocketPath(t), shortHybridBasePath(t)
		child := newFakeOwnedChild()
		var started bool
		fakeChildBindingBoth(t, child, socketPath, basePath, &started)
		// processChild.Close is idempotent, so the fake's must be too; otherwise the
		// second Close reports a listener error production cannot produce.
		var closeOnce sync.Once
		var closeErr error
		bindClose := child.closeFn
		launcher := hybridTestLauncher(t, socketPath, basePath)
		launcher.probeDial = dialFailure(os.ErrNotExist)

		if _, err := launcher.Launch(context.Background(), "sandbox"); err != nil {
			t.Fatalf("iteration %d: Launch: %v", iteration, err)
		}
		bindClose = child.closeFn
		child.closeFn = func() error {
			closeOnce.Do(func() { closeErr = bindClose() })
			return closeErr
		}
		child.stop()

		start := make(chan struct{})
		errs := make([]error, callers)
		var wg sync.WaitGroup
		for i := 0; i < callers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs[i] = launcher.Terminate(context.Background())
			}()
		}
		close(start)
		wg.Wait()

		if err := errors.Join(errs...); err != nil {
			t.Fatalf("iteration %d: overlapping Terminate calls reported: %v", iteration, err)
		}
		if _, err := os.Lstat(basePath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("iteration %d: the hybrid-vsock base survived: %v", iteration, err)
		}
		if _, err := os.Lstat(socketPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("iteration %d: the VM service socket survived: %v", iteration, err)
		}
		claim, err := acquireHostSocketClaim(socketPath)
		if err != nil {
			t.Fatalf("iteration %d: the host claim was not released: %v", iteration, err)
		}
		_ = claim.Release()
	}
}

func TestWaitVMServiceReadyReturnsImmediatelyForNonSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vmservice.sock")
	if err := os.WriteFile(path, []byte("not a socket"), 0o600); err != nil {
		t.Fatalf("writing ordinary file: %v", err)
	}

	const poll = time.Second
	ctx, cancel := context.WithTimeout(context.Background(), poll/2)
	defer cancel()
	started := time.Now()
	owner, err := waitVMServiceReady(ctx, dialFailure(errors.New("dial must not run")), path, make(chan struct{}), poll)
	elapsed := time.Since(started)
	if owner != nil || !errors.Is(err, errVMServiceSocketNotASocket) || elapsed >= poll/4 {
		t.Fatalf("owner=%v error=%v elapsed=%v, want nil owner and not-a-socket error in under %v", owner, err, elapsed, poll/4)
	}
}

func TestRemoveDeadSocketPreservesOrdinaryFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hv")
	if err := os.WriteFile(path, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := removeDeadSocket(path)
	if !errors.Is(err, errHybridBaseNotASocket) {
		t.Fatalf("err = %v, want %v", err, errHybridBaseNotASocket)
	}
	if !errors.Is(err, safefile.ErrSocketPathNotASocket) {
		t.Fatalf("err = %v, want shared cause %v", err, safefile.ErrSocketPathNotASocket)
	}
	if errors.Is(err, errVMServiceSocketNotASocket) {
		t.Fatalf("hybrid-base error = %v, must be distinct from %v", err, errVMServiceSocketNotASocket)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("ordinary file was removed: %v", err)
	}
}
