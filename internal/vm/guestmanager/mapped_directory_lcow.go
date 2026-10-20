//go:build windows && lcow

package guestmanager

import (
	"context"
	"fmt"

	hcsschema "github.com/Microsoft/hcsshim/internal/hcs/schema2"
	"github.com/Microsoft/hcsshim/internal/protocol/guestrequest"
	"github.com/Microsoft/hcsshim/internal/protocol/guestresource"
)

// AddMappedDirectory maps a directory into LCOW guest.
func (gm *Guest) AddMappedDirectory(ctx context.Context, settings guestresource.LCOWMappedDirectory) error {
	request := &hcsschema.ModifySettingRequest{
		GuestRequest: guestrequest.ModificationRequest{
			ResourceType: guestresource.ResourceTypeMappedDirectory,
			RequestType:  guestrequest.RequestTypeAdd,
			Settings:     settings,
		},
	}

	err := gm.modify(ctx, request.GuestRequest)
	if err != nil {
		return fmt.Errorf("failed to add mapped directory for lcow: %w", err)
	}
	return nil
}

// RemoveMappedDirectory unmaps a directory from LCOW guest.
func (gm *Guest) RemoveMappedDirectory(ctx context.Context, settings guestresource.LCOWMappedDirectory) error {
	request := &hcsschema.ModifySettingRequest{
		GuestRequest: guestrequest.ModificationRequest{
			ResourceType: guestresource.ResourceTypeMappedDirectory,
			RequestType:  guestrequest.RequestTypeRemove,
			Settings:     settings,
		},
	}

	err := gm.modify(ctx, request.GuestRequest)
	if err != nil {
		return fmt.Errorf("failed to remove mapped directory for lcow: %w", err)
	}
	return nil
}

// AddMappedVirtioFS mounts a virtio-fs share in the guest.
func (gm *Guest) AddMappedVirtioFS(ctx context.Context, settings guestresource.LCOWMappedVirtioFS) error {
	request := &hcsschema.ModifySettingRequest{GuestRequest: guestrequest.ModificationRequest{
		ResourceType: guestresource.ResourceTypeMappedVirtioFS,
		RequestType:  guestrequest.RequestTypeAdd,
		Settings:     settings,
	}}
	if err := gm.modify(ctx, request.GuestRequest); err != nil {
		return fmt.Errorf("failed to add mapped virtio-fs share for lcow: %w", err)
	}
	return nil
}

// RemoveMappedVirtioFS unmounts a virtio-fs share from the guest.
func (gm *Guest) RemoveMappedVirtioFS(ctx context.Context, settings guestresource.LCOWMappedVirtioFS) error {
	request := &hcsschema.ModifySettingRequest{GuestRequest: guestrequest.ModificationRequest{
		ResourceType: guestresource.ResourceTypeMappedVirtioFS,
		RequestType:  guestrequest.RequestTypeRemove,
		Settings:     settings,
	}}
	if err := gm.modify(ctx, request.GuestRequest); err != nil {
		return fmt.Errorf("failed to remove mapped virtio-fs share for lcow: %w", err)
	}
	return nil
}
