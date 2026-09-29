//go:build windows && (lcow || wcow) && openvmm_prototype

package manager

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
)

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
