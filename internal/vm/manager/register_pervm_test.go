//go:build windows && lcow && openvmm_prototype

package manager

import (
	"context"
	"io"
	"testing"

	"github.com/Microsoft/hcsshim/internal/vmservice"
)

func TestNewDirectCreateUsesPerVMPaths(t *testing.T) {
	config := nativeBuilderConfig()
	opts := nativeCreateOptions(t)
	want := config.ForVM(opts.ID)

	var got *Config
	client := &fakeModifyVMClient{}
	launcher := &fakeDirectLauncher{socketPath: want.VMServiceSocket}
	create := newDirectCreate(config, Deps{
		NewLauncher: func(c *Config) VMLauncher {
			got = c
			return launcher
		},
		Dial: func(context.Context, string) (vmservice.VMClient, io.Closer, error) {
			return client, &recordingCloser{}, nil
		},
	})

	result, err := create(context.Background(), opts)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	system, ok := result.ComputeSystem.(*System)
	if !ok {
		t.Fatalf("compute system is %T, want *System", result.ComputeSystem)
	}
	t.Cleanup(func() { _ = system.CloseCtx(context.Background()) })

	if got == nil || *got != *want {
		t.Fatalf("launcher config = %+v, want %+v", got, want)
	}
	if len(client.createCalls) != 1 {
		t.Fatalf("CreateVM calls = %d, want 1", len(client.createCalls))
	}
	if path := client.createCalls[0].GetConfig().GetHvsocketConfig().GetPath(); path != want.HybridVsockBase {
		t.Fatalf("request hvsocket path = %q, want %q", path, want.HybridVsockBase)
	}
	if base := system.TransportBase(); base != want.HybridVsockBase {
		t.Fatalf("advertised transport base = %q, want %q", base, want.HybridVsockBase)
	}
	if want.HybridVsockBase == config.HybridVsockBase {
		t.Fatal("per-VM base equals the shared configured base")
	}
}
