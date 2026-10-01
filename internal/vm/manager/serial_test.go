//go:build windows && lcow && openvmm_prototype

package manager

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/Microsoft/hcsshim/internal/log"
	"github.com/Microsoft/hcsshim/internal/safefile"
)

func swapSerialOpenOwnedPathForTest(open func(string) (*safefile.DeleteHandle, error)) func() {
	previous := serialOpenOwnedPath
	serialOpenOwnedPath = open
	return func() { serialOpenOwnedPath = previous }
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

func TestSerialRelayCloseRetriesPathRemovalAfterTransientFailure(t *testing.T) {
	relay, path := newTestSerialRelay(t)
	owner := relay.owner
	t.Cleanup(func() {
		_ = os.Chmod(path, 0600)
		_ = owner.Close()
	})

	if err := os.Chmod(path, 0400); err != nil {
		t.Fatalf("make COM1 socket read-only: %v", err)
	}
	if err := relay.Close(); !errors.Is(err, syscall.ERROR_ACCESS_DENIED) {
		t.Fatalf("first Close = %v, want access denied removing the read-only socket", err)
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("the COM1 pathname vanished despite a failed removal: %v", err)
	}

	if err := os.Chmod(path, 0600); err != nil {
		t.Fatalf("make COM1 socket writable: %v", err)
	}
	if err := relay.Close(); err != nil {
		t.Fatalf("retry Close: %v", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the COM1 pathname survived a successful retry: %v", err)
	}

	relay.owner = nil
	if err := relay.Close(); err != nil {
		t.Fatalf("third Close retried a settled removal: %v", err)
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
