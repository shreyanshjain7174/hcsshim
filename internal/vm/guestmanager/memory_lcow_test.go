//go:build windows && lcow

package guestmanager

import (
	"context"
	"errors"
	"testing"
)

// TestUpdatePodMemoryLimitValidatesBeforeSend uses a Guest without a connection:
// reaching the transport returns ErrGuestConnectionUnavailable, so any other
// error proves the request was rejected before it was sent.
func TestUpdatePodMemoryLimitValidatesBeforeSend(t *testing.T) {
	for _, tc := range []struct {
		name  string
		podID string
		limit int64
		valid bool
	}{
		{"limit", "pod1", 134217728, true},
		{"empty pod id", "", 134217728, false},
		{"zero limit", "pod1", 0, false},
		{"unlimited -1", "pod1", -1, false},
		{"negative limit", "pod1", -2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := (&Guest{}).UpdatePodMemoryLimit(context.Background(), tc.podID, tc.limit)
			if sent := errors.Is(err, ErrGuestConnectionUnavailable); sent != tc.valid || err == nil {
				t.Fatalf("UpdatePodMemoryLimit(%q, %d) = %v; want sent=%v", tc.podID, tc.limit, err, tc.valid)
			}
		})
	}
}
