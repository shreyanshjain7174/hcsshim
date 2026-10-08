//go:build windows && (lcow || wcow) && openvmm_prototype

package manager

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

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
