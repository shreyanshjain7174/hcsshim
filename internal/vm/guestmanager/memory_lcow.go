//go:build windows && lcow

package guestmanager

import (
	"context"
	"fmt"

	"github.com/Microsoft/hcsshim/internal/protocol/guestrequest"
	"github.com/Microsoft/hcsshim/internal/protocol/guestresource"
)

// UpdateCgroupMemoryLimits refreshes the top-level cgroup memory limits in the guest.
func (gm *Guest) UpdateCgroupMemoryLimits(ctx context.Context) error {
	request := guestrequest.ModificationRequest{
		ResourceType: guestresource.ResourceTypePodCgroupMemoryLimit,
		RequestType:  guestrequest.RequestTypeUpdate,
	}
	if err := gm.modify(ctx, request); err != nil {
		return fmt.Errorf("failed to update cgroup memory limits: %w", err)
	}
	return nil
}

// UpdatePodMemoryLimit sets the memory limit of a single pod's cgroup in the guest.
func (gm *Guest) UpdatePodMemoryLimit(ctx context.Context, podID string, limitInBytes int64) error {
	settings := &guestresource.LCOWPodMemoryLimit{PodID: podID, LimitInBytes: &limitInBytes}
	if err := settings.Validate(); err != nil {
		return err
	}
	request := guestrequest.ModificationRequest{
		ResourceType: guestresource.ResourceTypePodMemoryLimit,
		RequestType:  guestrequest.RequestTypeUpdate,
		Settings:     settings,
	}
	if err := gm.modify(ctx, request); err != nil {
		return fmt.Errorf("failed to update memory limit for pod %s: %w", podID, err)
	}
	return nil
}
