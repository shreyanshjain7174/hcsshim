//go:build windows && lcow && openvmm_prototype

package manager

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	runhcsoptions "github.com/Microsoft/hcsshim/cmd/containerd-shim-runhcs-v1/options"
	iannotations "github.com/Microsoft/hcsshim/internal/annotations"
	controllervm "github.com/Microsoft/hcsshim/internal/controller/vm"
	"github.com/Microsoft/hcsshim/internal/vmservice"
	vmsandbox "github.com/Microsoft/hcsshim/sandbox-spec/vm/v2"
)

func TestNewDirectCreateFailsClosedWithoutConfig(t *testing.T) {
	previous := executableDir
	executableDir = func() (string, error) { return t.TempDir(), nil }
	t.Cleanup(func() { executableDir = previous })

	create, err := NewDirectCreate()
	if create != nil || !errors.Is(err, errConfigSource) {
		t.Fatalf("NewDirectCreate without %s = creator %v, error %v; want nil and %v", configFileName, create, err, errConfigSource)
	}
}

func TestNewDirectCreateLaunchFailureTerminatesHost(t *testing.T) {
	config := nativeBuilderConfig()
	want := errors.New("launch failed")
	launcher := &fakeDirectLauncher{launchErr: want}
	create := newDirectCreate(config, Deps{
		Launcher: launcher,
		Dial: func(context.Context, string) (vmservice.VMClient, io.Closer, error) {
			t.Fatal("dial called after launch failure")
			return nil, nil, nil
		},
		TransportBase: config.HybridVsockBase,
	})

	result, err := create(context.Background(), nativeCreateOptions(t))
	if !errors.Is(err, want) || result != nil {
		t.Fatalf("result=%+v error=%v, want nil result wrapping %v", result, err, want)
	}
	if launcher.terminateCalls != 1 {
		t.Fatalf("terminate calls = %d, want 1", launcher.terminateCalls)
	}
}

func TestNewDirectCreateDialFailureClosesConnectionAndTerminatesHost(t *testing.T) {
	config := nativeBuilderConfig()
	want := errors.New("dial failed")
	launcher := &fakeDirectLauncher{socketPath: config.VMServiceSocket}
	connection := &recordingCloser{}
	create := newDirectCreate(config, Deps{
		Launcher: launcher,
		Dial: func(context.Context, string) (vmservice.VMClient, io.Closer, error) {
			return nil, connection, want
		},
		TransportBase: config.HybridVsockBase,
	})

	result, err := create(context.Background(), nativeCreateOptions(t))
	if !errors.Is(err, want) || result != nil {
		t.Fatalf("result=%+v error=%v, want nil result wrapping %v", result, err, want)
	}
	if connection.closeCalls != 1 || launcher.terminateCalls != 1 {
		t.Fatalf("close calls=%d terminate calls=%d, want 1 each", connection.closeCalls, launcher.terminateCalls)
	}
}

func TestNewDirectCreateSerialListenerFailureCleansUp(t *testing.T) {
	config := nativeBuilderConfig()
	config.SerialSocket = shortSerialSocketPath(t)
	listener, err := net.Listen("unix", config.SerialSocket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	launcher := &fakeDirectLauncher{socketPath: config.VMServiceSocket}
	connection := &recordingCloser{}
	create := newDirectCreate(config, Deps{
		Launcher: launcher,
		Dial: func(context.Context, string) (vmservice.VMClient, io.Closer, error) {
			return &fakeModifyVMClient{}, connection, nil
		},
		TransportBase: config.HybridVsockBase,
	})
	opts := nativeCreateOptions(t)
	opts.SandboxSpec.Annotations[iannotations.UVMConsolePipe] = `\\.\pipe\console`

	result, err := create(context.Background(), opts)
	if err == nil || result != nil {
		t.Fatalf("result=%+v error=%v, want listener rejection", result, err)
	}
	if connection.closeCalls != 1 || launcher.terminateCalls != 1 {
		t.Fatalf("close calls=%d terminate calls=%d, want 1 each", connection.closeCalls, launcher.terminateCalls)
	}
}

func TestNewDirectCreateRPCFailureClosesSerialListener(t *testing.T) {
	config := nativeBuilderConfig()
	config.SerialSocket = shortSerialSocketPath(t)
	launcher := &fakeDirectLauncher{socketPath: config.VMServiceSocket}
	connection := &recordingCloser{}
	client := &fakeModifyVMClient{createErr: errors.New("create failed")}
	create := newDirectCreate(config, Deps{
		Launcher: launcher,
		Dial: func(context.Context, string) (vmservice.VMClient, io.Closer, error) {
			return client, connection, nil
		},
		TransportBase: config.HybridVsockBase,
	})
	opts := nativeCreateOptions(t)
	opts.SandboxSpec.Annotations[iannotations.UVMConsolePipe] = `\\.\pipe\console`

	result, err := create(context.Background(), opts)
	if err == nil || result != nil {
		t.Fatalf("result=%+v error=%v, want RPC failure", result, err)
	}
	if connection.closeCalls != 1 || launcher.terminateCalls != 1 {
		t.Fatalf("close calls=%d terminate calls=%d, want 1 each", connection.closeCalls, launcher.terminateCalls)
	}
	if _, statErr := os.Stat(config.SerialSocket); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("serial socket survived RPC failure: %v", statErr)
	}
}

func TestNewDirectCreateRejectsUnsupportedInputBeforeLaunch(t *testing.T) {
	config := nativeBuilderConfig()
	launcher := &fakeDirectLauncher{socketPath: config.VMServiceSocket}
	create := newDirectCreate(config, Deps{
		Launcher: launcher,
		Dial: func(context.Context, string) (vmservice.VMClient, io.Closer, error) {
			t.Fatal("dial called for rejected input")
			return nil, nil, nil
		},
		TransportBase: config.HybridVsockBase,
	})
	opts := nativeCreateOptions(t)
	opts.ShimOpts.SandboxPlatform = "linux/arm64"

	result, err := create(context.Background(), opts)
	if err == nil || result != nil {
		t.Fatalf("result=%+v error=%v, want pre-launch rejection", result, err)
	}
	if launcher.launchCalls != 0 || launcher.terminateCalls != 0 {
		t.Fatalf("rejected input touched launcher: launch=%d terminate=%d", launcher.launchCalls, launcher.terminateCalls)
	}
}

func shortSerialSocketPath(t *testing.T) string {
	t.Helper()
	path := filepath.Join(os.TempDir(), fmt.Sprintf("ovmm-%d.sock", time.Now().UnixNano()))
	t.Cleanup(func() { _ = os.Remove(path) })
	return path
}

func nativeCreateOptions(t *testing.T) *controllervm.CreateOptions {
	t.Helper()
	return &controllervm.CreateOptions{
		ID:          "native-create",
		Owner:       "test-owner",
		BundlePath:  t.TempDir(),
		ShimOpts:    &runhcsoptions.Options{SandboxPlatform: "linux/amd64", BootFilesRootPath: newBootFiles(t)},
		SandboxSpec: &vmsandbox.Spec{Annotations: map[string]string{}},
	}
}

type fakeDirectLauncher struct {
	socketPath     string
	launchErr      error
	launchCalls    int
	terminateCalls int
	// terminateErrs is a queue: each Terminate pops the front entry and an empty queue
	// succeeds, so a caller can fail only the first attempt and let a retry pass.
	terminateErrs []error
}

func (f *fakeDirectLauncher) Launch(context.Context, string) (string, error) {
	f.launchCalls++
	return f.socketPath, f.launchErr
}

func (f *fakeDirectLauncher) Terminate(ctx context.Context) error {
	f.terminateCalls++
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(f.terminateErrs) == 0 {
		return nil
	}
	err := f.terminateErrs[0]
	f.terminateErrs = f.terminateErrs[1:]
	return err
}

type nilReader struct{}

func (nilReader) Read([]byte) (int, error) { return 0, io.EOF }

type recordingCloser struct{ closeCalls int }

func (c *recordingCloser) Close() error {
	c.closeCalls++
	return nil
}
