//go:build windows && lcow && openvmm_prototype

package manager

import (
	"context"
	"fmt"

	controllervm "github.com/Microsoft/hcsshim/internal/controller/vm"
)

// NewDirectCreate loads the OpenVMM configuration and constructs the direct VM creator.
func NewDirectCreate() (controllervm.DirectCreateFunc, error) {
	config, err := LoadConfig()
	if err != nil {
		return nil, fmt.Errorf("the openvmm backend configuration is unusable: %w", err)
	}
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("the openvmm backend configuration is unusable: %w", err)
	}
	return newDirectCreate(config, Deps{
		NewLauncher: func(c *Config) VMLauncher { return newLauncher(c) },
		Dial:        dialVMService,
	}), nil
}

func newDirectCreate(config *Config, deps Deps) controllervm.DirectCreateFunc {
	return func(ctx context.Context, opts *controllervm.CreateOptions) (*controllervm.DirectCreateResult, error) {
		if opts == nil {
			return nil, fmt.Errorf("no create options provided")
		}
		vmConfig := config.ForVM(opts.ID)
		vmDeps := deps
		vmDeps.TransportBase = vmConfig.HybridVsockBase
		if vmDeps.Launcher == nil && vmDeps.NewLauncher != nil {
			vmDeps.Launcher = vmDeps.NewLauncher(vmConfig)
		}
		request, sandboxOptions, reservations, err := BuildCreateVMRequestFromOptions(
			ctx,
			opts.ShimOpts,
			opts.SandboxSpec,
			vmConfig,
			opts.ID,
		)
		if err != nil {
			return nil, err
		}
		computeSystem, runtimeID, err := (&backend{deps: vmDeps}).CreateFromRequest(ctx, opts.ID, request)
		if err != nil {
			return nil, err
		}
		return &controllervm.DirectCreateResult{
			ComputeSystem:      computeSystem,
			RuntimeID:          runtimeID,
			SandboxOptions:     sandboxOptions,
			RootfsReservations: reservations,
		}, nil
	}
}
