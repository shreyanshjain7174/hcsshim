//go:build windows && lcow

package vm

import (
	"context"
	"fmt"

	"github.com/Microsoft/hcsshim/internal/builder/vm/lcow"
	"github.com/Microsoft/hcsshim/internal/controller/device/scsi"
	"github.com/Microsoft/hcsshim/internal/controller/device/scsi/disk"
	"github.com/Microsoft/hcsshim/internal/protocol/guestrequest"
	"github.com/Microsoft/hcsshim/internal/vm/guestmanager"
	"github.com/Microsoft/hcsshim/internal/vm/vmmanager"

	"github.com/Microsoft/go-winio/pkg/guid"
	"github.com/containerd/errdefs"
)

// DirectCreateResult is the native OpenVMM create outcome.
type DirectCreateResult struct {
	ComputeSystem      vmmanager.ComputeSystem
	RuntimeID          guid.GUID
	SandboxOptions     *lcow.SandboxOptions
	RootfsReservations []RootfsReservation
}

// RootfsReservation is a SCSI rootfs slot installed on the direct-create path.
type RootfsReservation struct {
	Controller uint
	Lun        uint
	Config     disk.Config
}

// DirectCreateFunc creates an OpenVMM compute system from existing CreateOptions.
type DirectCreateFunc func(ctx context.Context, opts *CreateOptions) (*DirectCreateResult, error)

func (c *Controller) rejectUnsupportedMigration(operation string) error {
	if c.directCreate == nil {
		return nil
	}
	return fmt.Errorf("%s is not supported by the OpenVMM runtime: %w", operation, errdefs.ErrNotImplemented)
}

func (c *Controller) createColdVM(ctx context.Context, opts *CreateOptions) (*coldCreateResult, error) {
	if c.directCreate == nil {
		doc, err := c.buildHCSConfig(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("failed to build VM config: %w", err)
		}
		uvm, err := vmmanager.Create(ctx, opts.ID, doc)
		if err != nil {
			return nil, fmt.Errorf("failed to create VM: %w", err)
		}
		return &coldCreateResult{uvm: uvm, hcsDocument: doc}, nil
	}

	result, err := c.directCreate(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("failed to create VM directly: %w", err)
	}
	if result == nil {
		return nil, fmt.Errorf("failed to create VM directly: callback returned no result")
	}
	uvm, err := vmmanager.WrapCreatedSystem(ctx, opts.ID, result.ComputeSystem, result.RuntimeID)
	if err != nil {
		return nil, fmt.Errorf("failed to wrap directly created VM: %w", err)
	}
	return &coldCreateResult{
		uvm: uvm,
		prepare: func(ctx context.Context, uvm *vmmanager.UtilityVM, guest *guestmanager.Guest) (func(), error) {
			scsiController := scsi.New(len(guestrequest.ScsiControllerGuids), uvm, guest)
			for _, reservation := range result.RootfsReservations {
				if err := scsiController.ReserveForRootfs(ctx, reservation.Controller, reservation.Lun, reservation.Config); err != nil {
					return nil, fmt.Errorf("reserve SCSI slot (controller=%d, lun=%d): %w", reservation.Controller, reservation.Lun, err)
				}
			}
			return func() {
				c.sandboxOptions = result.SandboxOptions
				c.scsiController = scsiController
			}, nil
		},
	}, nil
}
