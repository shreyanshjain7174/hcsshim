//go:build windows && lcow

package vm

import (
	"context"
	"testing"
	"time"

	"github.com/Microsoft/hcsshim/internal/vm/vmmanager"

	"github.com/Microsoft/go-winio/pkg/guid"
)

// containerd waits on WaitSandbox before it finishes a stop, so a log reader that outlives the
// VM must not keep Wait blocked and leave the pod running.
func TestWaitReturnsAfterVMExitWhenLogOutputNeverCompletes(t *testing.T) {
	const grace = 200 * time.Millisecond
	previous := logOutputDrainGrace
	logOutputDrainGrace = grace
	t.Cleanup(func() { logOutputDrainGrace = previous })

	utilityVM, err := vmmanager.WrapCreatedSystem(context.Background(), "stuck-log-reader", newRecordingComputeSystem(""), guid.GUID{Data1: 1})
	if err != nil {
		t.Fatalf("WrapCreatedSystem: %v", err)
	}
	controller := New()
	controller.vmID = "stuck-log-reader"
	controller.uvm = utilityVM
	controller.vmState = StateRunning

	done := make(chan error, 1)
	go func() { done <- controller.Wait(context.Background()) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Wait = %v, want nil", err)
		}
	case <-time.After(10 * grace):
		t.Fatal("Wait did not return after the VM exited with the log reader still running")
	}
}
