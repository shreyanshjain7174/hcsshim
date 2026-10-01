//go:build windows

package safefile

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func shortSocketPath(t *testing.T) string {
	t.Helper()
	path := filepath.Join(os.TempDir(), fmt.Sprintf("safefile-%d.sock", time.Now().UnixNano()))
	t.Cleanup(func() { _ = os.Remove(path) })
	return path
}

func TestClaimSocketPathRefusesLivePeer(t *testing.T) {
	path := shortSocketPath(t)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	errInUse := errors.New("socket path in use")
	err = ClaimSocketPath(context.Background(), path, ClaimSocketPathOptions{
		ProbeDial:         (&net.Dialer{}).DialContext,
		ProbeBudget:       2 * time.Second,
		InUseError:        errInUse,
		InconclusiveError: errors.New("socket probe inconclusive"),
		PathDescription:   "test socket path",
	})
	if !errors.Is(err, errInUse) {
		t.Fatalf("ClaimSocketPath error = %v, want %v", err, errInUse)
	}
	if _, statErr := os.Lstat(path); statErr != nil {
		t.Fatalf("live socket path was not preserved: %v", statErr)
	}
}

func TestClaimSocketPathPreservesOrdinaryFileAndReturnsNotASocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ordinary.sock")
	const contents = "ordinary file"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write ordinary file: %v", err)
	}

	errInconclusive := errors.New("socket probe inconclusive")
	err := ClaimSocketPath(context.Background(), path, ClaimSocketPathOptions{
		ProbeDial: func(context.Context, string, string) (net.Conn, error) {
			t.Fatal("ordinary file was probed")
			return nil, nil
		},
		ProbeBudget:       2 * time.Second,
		InUseError:        errors.New("socket path in use"),
		InconclusiveError: errInconclusive,
		PathDescription:   "test socket path",
	})
	if !errors.Is(err, ErrSocketPathNotASocket) {
		t.Fatalf("ClaimSocketPath error = %v, want %v", err, ErrSocketPathNotASocket)
	}
	if errors.Is(err, errInconclusive) {
		t.Fatalf("ClaimSocketPath error = %v, must be distinct from %v", err, errInconclusive)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil || string(got) != contents {
		t.Fatalf("ordinary file after ClaimSocketPath: contents=%q error=%v", got, readErr)
	}
}
