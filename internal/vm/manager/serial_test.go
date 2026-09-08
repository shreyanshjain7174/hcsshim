//go:build windows && lcow && openvmm_prototype

package manager

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Microsoft/hcsshim/internal/log"
	"github.com/Microsoft/hcsshim/internal/safefile"
	"golang.org/x/sys/windows"
)

type countingOwnedPathRemove struct {
	calls int
	errs  []error
}

func swapSerialProbeDialForTest(dial func(context.Context, string, string) (net.Conn, error)) func() {
	previous := serialProbeDial
	serialProbeDial = dial
	return func() { serialProbeDial = previous }
}

func swapSerialOpenOwnedPathForTest(open func(string) (*safefile.DeleteHandle, error)) func() {
	previous := serialOpenOwnedPath
	serialOpenOwnedPath = open
	return func() { serialOpenOwnedPath = previous }
}

func (c *countingOwnedPathRemove) remove(owner *safefile.DeleteHandle) error {
	c.calls++
	if len(c.errs) > 0 {
		err := c.errs[0]
		c.errs = c.errs[1:]
		if err != nil {
			return err
		}
	}
	return owner.Remove()
}

func newTestSerialRelay(t *testing.T) (*serialRelay, string) {
	t.Helper()
	path := shortSerialSocketPath(t)
	relay, err := newSerialRelay(context.Background(), path, "serial-close")
	if err != nil {
		t.Fatalf("newSerialRelay: %v", err)
	}
	t.Cleanup(func() { _ = relay.Close() })
	return relay, path
}

func TestSerialRelayPreventsPathReplacementUntilClose(t *testing.T) {
	relay, path := newTestSerialRelay(t)

	if err := os.Remove(path); err == nil {
		t.Fatal("removed the COM1 socket while its relay still owned the pathname")
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("the owned COM1 socket disappeared after the rejected removal: %v", err)
	}

	if err := relay.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the COM1 socket survived owner cleanup: %v", err)
	}
}

func TestClaimSerialPathPreservesOrdinaryFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "serial.sock")
	const contents = "ordinary file"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write ordinary file: %v", err)
	}
	t.Cleanup(swapSerialProbeDialForTest(dialFailure(windows.WSAENOTSOCK)))

	err := claimSerialPath(path)
	if !errors.Is(err, errSerialProbeInconclusive) {
		t.Fatalf("claimSerialPath error = %v, want %v", err, errSerialProbeInconclusive)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil || string(got) != contents {
		t.Fatalf("ordinary file after claim: contents=%q error=%v", got, readErr)
	}
}

func TestClaimSerialPathPinsSocketBeforeProbe(t *testing.T) {
	path := shortSerialSocketPath(t)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatalf("create stale socket: %v", err)
	}
	listener.SetUnlinkOnClose(false)
	if err := listener.Close(); err != nil {
		t.Fatalf("close stale listener: %v", err)
	}

	t.Cleanup(swapSerialProbeDialForTest(func(context.Context, string, string) (net.Conn, error) {
		if err := os.Remove(path); err == nil {
			t.Fatal("probe replaced a stale socket before cleanup pinned it")
		}
		return nil, windows.WSAECONNREFUSED
	}))

	if err := claimSerialPath(path); err != nil {
		t.Fatalf("claimSerialPath: %v", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale socket survived cleanup: %v", err)
	}
}

func TestSerialRelayCloseRetriesPathRemovalAfterTransientFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "serial.sock")
	if err := os.WriteFile(path, []byte("owned"), 0600); err != nil {
		t.Fatal(err)
	}
	owner, err := safefile.OpenDeleteHandle(path)
	if err != nil {
		t.Fatal(err)
	}
	relay := &serialRelay{path: path, owner: owner, log: log.G(context.Background())}

	removal := &countingOwnedPathRemove{errs: []error{errors.New("sharing violation")}}
	relay.removeOwner = removal.remove

	err = relay.releasePath()
	if err == nil || !strings.Contains(err.Error(), "sharing violation") {
		t.Fatalf("first Close = %v, want the removal failure", err)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("the COM1 pathname vanished despite a failed removal: %v", statErr)
	}

	if err := relay.releasePath(); err != nil {
		t.Fatalf("retry Close: %v", err)
	}
	if removal.calls != 2 {
		t.Fatalf("removal attempts = %d, want 2 (failed then retried)", removal.calls)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("the COM1 pathname survived a successful retry: %v", statErr)
	}

	if err := relay.releasePath(); err != nil {
		t.Fatalf("third Close: %v", err)
	}
	if removal.calls != 2 {
		t.Fatalf("removal attempts = %d, want no attempt once the pathname is gone", removal.calls)
	}
}

// TestSerialRelayReleasePathFailsClosedWithoutOwnership proves an unknown identity is
// never reported as a settled removal: it is an error, and the pathname is preserved.
func TestSerialRelayReleasePathFailsClosedWithoutOwnership(t *testing.T) {
	path := filepath.Join(t.TempDir(), "serial.sock")
	if err := os.WriteFile(path, []byte("someone else's file"), 0600); err != nil {
		t.Fatal(err)
	}
	relay := &serialRelay{path: path, log: log.G(context.Background())}

	err := relay.releasePath()
	if err == nil {
		t.Fatal("releasePath without a captured identity = nil, want a failure")
	}
	if !strings.Contains(err.Error(), "ownership") {
		t.Fatalf("releasePath error = %v, want it to name the missing ownership", err)
	}
	if relay.pathReleased {
		t.Fatal("releasePath recorded an unknown ownership as settled")
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("the pathname was removed despite unknown ownership: %v", statErr)
	}
}

// TestNewSerialRelayIdentityCaptureFailureClosesListenerAndKeepsPath proves creation never
// publishes a relay that cannot prove what it owns, and that the rollback closes the
// listener without unlinking a pathname it can no longer identify.
func TestNewSerialRelayIdentityCaptureFailureClosesListenerAndKeepsPath(t *testing.T) {
	path := shortSerialSocketPath(t)

	t.Cleanup(swapSerialOpenOwnedPathForTest(func(string) (*safefile.DeleteHandle, error) {
		return nil, errors.New("identity capture failed")
	}))

	relay, err := newSerialRelay(context.Background(), path, "identity-failure")
	if relay != nil {
		t.Fatal("newSerialRelay published a relay that could not identify its own pathname")
	}
	if err == nil || !strings.Contains(err.Error(), "identity capture failed") {
		t.Fatalf("newSerialRelay = %v, want the identity capture failure", err)
	}
	// The listener is closed: a dial to the pathname it held is refused rather than
	// accepted.
	connection, dialErr := net.DialTimeout("unix", path, serialProbeBudget)
	if dialErr == nil {
		_ = connection.Close()
		t.Fatal("the listener is still accepting after a failed newSerialRelay")
	}
}
