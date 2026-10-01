//go:build windows && lcow

package service

import (
	"context"
	"sync"
	"testing"
	"time"

	sandboxsvc "github.com/containerd/containerd/api/runtime/sandbox/v1"
	"go.uber.org/mock/gomock"

	"github.com/Microsoft/hcsshim/internal/controller/vm"
)

// TestShutdownSandbox_ConcurrentWithCreate verifies that ShutdownSandbox can
// read the sandbox ID while CreateSandbox is recording it. containerd sends
// ShutdownSandbox to reap a shim whose create is still running or has failed,
// so the read must not depend on the create's lock. Run with -race.
func TestShutdownSandbox_ConcurrentWithCreate(t *testing.T) {
	t.Parallel()

	// The mock controller's own mutex orders some interleavings, so repeat
	// with fresh state until the detector sees the write before the read.
	for i := 0; i < 200; i++ {
		svc, mockCtrl := newTestService(t)
		bundleDir := t.TempDir()
		writeMiniConfigJSON(t, bundleDir)

		mockCtrl.EXPECT().CreateVM(gomock.Any(), gomock.Any()).Return(nil)
		mockCtrl.EXPECT().State().Return(vm.StateTerminated).AnyTimes()

		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, _ = svc.createSandboxInternal(context.Background(), &sandboxsvc.CreateSandboxRequest{
				SandboxID:  "test-sandbox",
				BundlePath: bundleDir,
			})
		}()
		go func() {
			defer wg.Done()
			<-start
			// Sleeping does not synchronize, so the read lands after the
			// write in real time without an ordering edge the detector could
			// use to excuse it.
			time.Sleep(2 * time.Millisecond)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_, _ = svc.shutdownSandboxInternal(ctx, &sandboxsvc.ShutdownSandboxRequest{SandboxID: "other-sandbox"})
		}()
		close(start)
		wg.Wait()
	}
}
