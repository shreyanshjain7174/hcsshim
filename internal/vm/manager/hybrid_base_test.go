//go:build windows && (lcow || wcow) && openvmm_prototype

package manager

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Microsoft/hcsshim/internal/safefile"
)

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
