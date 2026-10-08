//go:build windows && (lcow || wcow) && openvmm_prototype

package manager

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

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
