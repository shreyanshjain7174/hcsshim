//go:build windows && lcow && openvmm_prototype

package manager

import (
	"context"
	"fmt"

	controllervm "github.com/Microsoft/hcsshim/internal/controller/vm"
	"github.com/Microsoft/hcsshim/internal/log"
)

// NewDirectCreate loads the OpenVMM configuration and constructs the direct VM creator.
func NewDirectCreate() (controllervm.DirectCreateFunc, error) {
	config, err := LoadConfig()
	if err != nil {
		return nil, fmt.Errorf("the openvmm backend configuration is unusable: %w", err)
	}
	warnings, err := config.Validate()
	for _, warning := range warnings {
		log.G(context.Background()).WithField("config", configFileName).Warn(warning)
	}
	if err != nil {
		return nil, fmt.Errorf("the openvmm backend configuration is unusable: %w", err)
	}
	return newDirectCreate(config, Deps{
		Launcher:      newLauncher(config),
		Dial:          dialVMService,
		TransportBase: config.HybridVsockBase,
	}), nil
}

func newDirectCreate(config *Config, deps Deps) controllervm.DirectCreateFunc {
	requestBackend := &backend{deps: deps}
	return func(ctx context.Context, opts *controllervm.CreateOptions) (*controllervm.DirectCreateResult, error) {
		if opts == nil {
			return nil, fmt.Errorf("no create options provided")
		}
		request, sandboxOptions, reservations, err := BuildCreateVMRequestFromOptions(
			ctx,
			opts.ShimOpts,
			opts.SandboxSpec,
			config,
			opts.ID,
		)
		if err != nil {
			return nil, err
		}
		computeSystem, runtimeID, err := requestBackend.CreateFromRequest(ctx, opts.ID, request)
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
