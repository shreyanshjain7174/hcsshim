//go:build windows && (lcow || wcow)

package vmmanager

import (
	"context"
	"fmt"
	"time"

	"github.com/Microsoft/hcsshim/internal/hcs/schema1"
	hcsschema "github.com/Microsoft/hcsshim/internal/hcs/schema2"
	hcs "github.com/Microsoft/hcsshim/internal/hcs/v2"

	"github.com/Microsoft/go-winio/pkg/guid"
)

// ComputeSystem is the exact set of methods vmmanager calls on the compute system backing
// a [UtilityVM]. It is transcribed verbatim from *hcs.System in internal/hcs/v2 - not
// internal/hcs, which defines a different System with different Properties, Save and
// Migration shapes.
//
// ComputeSystem is exported because the OpenVMM implementation lives in its own package,
// internal/vm/manager, and returns a directly created system to the VM controller.
type ComputeSystem interface {
	Start(ctx context.Context) error
	Terminate(ctx context.Context) error
	CloseCtx(ctx context.Context) error
	Pause(ctx context.Context) error
	Resume(ctx context.Context) error
	Save(ctx context.Context, options interface{}) error
	WaitCtx(ctx context.Context) error
	PropertiesV2(ctx context.Context, types ...hcsschema.PropertyType) (*hcsschema.Properties, error)
	PropertiesV3(ctx context.Context, query *hcsschema.PropertyQuery) (*hcsschema.Properties, error)
	StartedTime() time.Time
	StoppedTime() time.Time
	ExitError() error
	Modify(ctx context.Context, config interface{}) error
	Properties(ctx context.Context, types ...schema1.PropertyType) (*schema1.ContainerProperties, error)
	StartWithMigrationOptions(ctx context.Context, config *hcs.MigrationConfig) error
	InitializeLiveMigrationOnSource(ctx context.Context, options *hcsschema.MigrationInitializeOptions) error
	StartLiveMigrationOnSource(ctx context.Context, config *hcs.MigrationConfig) error
	StartLiveMigrationTransfer(ctx context.Context, options *hcsschema.MigrationTransferOptions) error
	CancelLiveMigration(ctx context.Context, options *hcsschema.MigrationCancelOptions) error
	FinalizeLiveMigration(ctx context.Context, options *hcsschema.MigrationFinalizedOptions) error
	MigrationNotifications() <-chan hcsschema.OperationSystemMigrationNotificationInfo
}

// TransportProvider supplies the OpenVMM hybrid-vsock base.
type TransportProvider interface {
	// TransportBase returns, byte for byte, the hybrid-vsock base path this compute
	// system handed to vmservice as VMConfig.HvsocketConfig.Path.
	TransportBase() string
}

func createHCSComputeSystem(ctx context.Context, id string, config *hcsschema.ComputeSystem) (result ComputeSystem, runtimeID guid.GUID, err error) {
	cs, err := hcs.CreateComputeSystem(ctx, id, config)
	if err != nil {
		// Explicitly nil rather than cs: a nil *hcs.System returned as a ComputeSystem
		// would be a non-nil interface holding a nil pointer.
		return nil, guid.GUID{}, fmt.Errorf("failed to create compute system: %w", err)
	}
	defer func() {
		if result == nil {
			_ = cs.Terminate(ctx)
			_ = cs.WaitCtx(ctx)
		}
	}()

	properties, err := cs.Properties(ctx)
	if err != nil {
		return nil, guid.GUID{}, fmt.Errorf("failed to get compute system properties: %w", err)
	}
	return cs, properties.RuntimeID, nil
}
