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
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func shortLauncherSocketPath(t *testing.T) string {
	t.Helper()
	path := filepath.Join(os.TempDir(), fmt.Sprintf("ovmm-launcher-%d.sock", time.Now().UnixNano()))
	t.Cleanup(func() { _ = os.Remove(path) })
	return path
}

func TestStartProcessJobCreationFailureStartsNoChild(t *testing.T) {
	want := errors.New("job creation failed")
	oldCreate := createKillOnCloseJob
	createKillOnCloseJob = func() (windows.Handle, error) { return 0, want }
	t.Cleanup(func() { createKillOnCloseJob = oldCreate })

	child, err := startProcess(`C:\definitely-missing-openvmm.exe`, nil)
	if !errors.Is(err, want) || child != nil {
		t.Fatalf("child=%v error=%v, want nil child wrapping %v", child, err, want)
	}
}

func TestStartProcessAssignmentFailureReapsChild(t *testing.T) {
	want := errors.New("assignment failed")
	t.Setenv("GO_WANT_LAUNCHER_HELPER", "1")
	oldCreate := createKillOnCloseJob
	oldAssign := assignProcessToJob
	createKillOnCloseJob = func() (windows.Handle, error) { return windows.Handle(1), nil }
	assignProcessToJob = func(windows.Handle, *os.Process) error { return want }
	t.Cleanup(func() {
		createKillOnCloseJob = oldCreate
		assignProcessToJob = oldAssign
	})

	child, err := startProcess(os.Args[0], []string{"-test.run=^TestLauncherHelperProcess$"})
	if !errors.Is(err, want) || child != nil {
		t.Fatalf("child=%v error=%v, want nil child wrapping %v", child, err, want)
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

func TestClaimSocketPathPinsSocketBeforeProbe(t *testing.T) {
	socketPath := shortLauncherSocketPath(t)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatalf("creating the stale socket: %v", err)
	}
	listener.SetUnlinkOnClose(false)
	if err := listener.Close(); err != nil {
		t.Fatalf("closing the stale socket listener: %v", err)
	}

	launcher := &openvmmLauncher{probeDial: func(context.Context, string, string) (net.Conn, error) {
		if err := os.Remove(socketPath); err == nil {
			t.Fatal("probe replaced a stale socket before cleanup pinned it")
		}
		return nil, windows.WSAECONNREFUSED
	}}
	if err := launcher.claimSocketPath(context.Background(), socketPath); err != nil {
		t.Fatalf("claimSocketPath: %v", err)
	}
	if _, err := os.Lstat(socketPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale socket survived cleanup: %v", err)
	}
}

func TestClaimSocketPathPreservesOrdinaryFileWhenProbeReportsRefused(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "vmservice.sock")
	const contents = "ordinary file"
	if err := os.WriteFile(socketPath, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing the ordinary file: %v", err)
	}

	launcher := &openvmmLauncher{probeDial: dialFailure(windows.WSAECONNREFUSED)}
	err := launcher.claimSocketPath(context.Background(), socketPath)
	if !errors.Is(err, errVMServiceSocketNotASocket) {
		t.Fatalf("claimSocketPath error = %v, want %v", err, errVMServiceSocketNotASocket)
	}
	got, readErr := os.ReadFile(socketPath)
	if readErr != nil {
		t.Fatalf("the ordinary file was not preserved: %v", readErr)
	}
	if string(got) != contents {
		t.Fatalf("ordinary file contents = %q, want %q", got, contents)
	}
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
