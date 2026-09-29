//go:build windows && (lcow || wcow) && openvmm_prototype

package manager

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func staleSocket(t *testing.T) string {
	t.Helper()
	path := shortHybridBasePath(t)
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = l.Close()
	return path
}

func TestRemoveDeadSocketRemovesStaleBase(t *testing.T) {
	path := staleSocket(t)
	if err := removeDeadSocket(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale base still present: %v", err)
	}
}

func TestRemoveDeadSocketMissingIsFine(t *testing.T) {
	if err := removeDeadSocket(filepath.Join(t.TempDir(), "hv")); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveDeadSocketPreservesOrdinaryFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hv")
	if err := os.WriteFile(path, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeDeadSocket(path); !errors.Is(err, errHybridBaseNotASocket) {
		t.Fatalf("err = %v, want %v", err, errHybridBaseNotASocket)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("ordinary file was removed: %v", err)
	}
}
