//go:build windows && lcow && openvmm_prototype

package manager

import (
	"context"
	"errors"
	"testing"
)

// containerd skips its own cleanup on any stop error, so a Quit that failed after every other
// rung succeeded must not fail the close and leave the pod and its network namespace behind.
func TestSystemCloseSucceedsWhenOnlyQuitFailed(t *testing.T) {
	client := &fakeModifyVMClient{quitErr: errors.New("quit rpc: deadline exceeded")}
	launcher := &fakeDirectLauncher{}
	system := newCloseTestSystem(t, "late-quit", client, &recordingCloser{}, launcher)

	if err := system.CloseCtx(context.Background()); err != nil {
		t.Fatalf("CloseCtx = %v, want nil", err)
	}
	if !system.closed || launcher.terminateCalls != 1 {
		t.Fatalf("closed=%v terminate calls=%d, want closed with one terminate", system.closed, launcher.terminateCalls)
	}
}

func TestSystemCloseFailsWhenQuitFailedAndTheProcessSurvives(t *testing.T) {
	quitErr := errors.New("quit rpc: deadline exceeded")
	client := &fakeModifyVMClient{quitErr: quitErr}
	launcher := &fakeDirectLauncher{terminateErrs: []error{errors.New("owned child still running")}}
	system := newCloseTestSystem(t, "late-quit-survivor", client, &recordingCloser{}, launcher)

	err := system.CloseCtx(context.Background())
	if !errors.Is(err, quitErr) {
		t.Fatalf("CloseCtx = %v, want an error wrapping the Quit failure", err)
	}
	if system.closed {
		t.Fatal("system.closed = true with a surviving process")
	}
}
