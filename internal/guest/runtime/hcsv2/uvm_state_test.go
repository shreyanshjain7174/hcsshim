//go:build linux
// +build linux

package hcsv2

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/Microsoft/hcsshim/internal/guest/cgroup"
	"github.com/Microsoft/hcsshim/internal/guest/prot"
	"github.com/Microsoft/hcsshim/internal/protocol/guestrequest"
	"github.com/Microsoft/hcsshim/internal/protocol/guestresource"
	"github.com/Microsoft/hcsshim/pkg/securitypolicy"
	oci "github.com/opencontainers/runtime-spec/specs-go"
)

func TestCalculateCgroupMemoryLimit(t *testing.T) {
	tests := []struct {
		name        string
		total       uint64
		reserve     uint64
		want        int64
		wantErrText string
	}{
		{name: "subtracts reserve", total: 4096, reserve: 1024, want: 3072},
		{name: "rejects equal reserve", total: 1024, reserve: 1024, wantErrText: "must be greater"},
		{name: "rejects below reserve", total: 512, reserve: 1024, wantErrText: "must be greater"},
		{name: "rejects int64 overflow", total: uint64(1 << 63), wantErrText: "exceeds maximum"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := calculateCgroupMemoryLimit(test.total, test.reserve)
			if test.wantErrText != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErrText) {
					t.Fatalf("expected error containing %q, got %v", test.wantErrText, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("calculateCgroupMemoryLimit returned error: %v", err)
			}
			if got != test.want {
				t.Fatalf("calculateCgroupMemoryLimit returned %d, want %d", got, test.want)
			}
		})
	}
}

func TestSetCgroupMemoryLimit(t *testing.T) {
	podsControl := &testCgroupUpdater{}
	limit, updated, err := setCgroupMemoryLimit(podsControl, 4096, 1024, 2048)
	if err != nil {
		t.Fatalf("setCgroupMemoryLimit returned error: %v", err)
	}
	if !updated || limit != 3072 || podsControl.limit != 3072 {
		t.Fatalf("setCgroupMemoryLimit returned limit=%d, updated=%t, applied=%d; want 3072, true, 3072", limit, updated, podsControl.limit)
	}

	limit, updated, err = setCgroupMemoryLimit(podsControl, 4096, 1024, 3072)
	if err != nil {
		t.Fatalf("setCgroupMemoryLimit returned error: %v", err)
	}
	if updated || limit != 3072 || podsControl.updates != 1 {
		t.Fatalf("unchanged set returned limit=%d, updated=%t, updates=%d; want 3072, false, 1", limit, updated, podsControl.updates)
	}
}

func TestModifyHostSettingsRejectsInvalidPodCgroupMemoryLimitRequestType(t *testing.T) {
	for _, requestType := range []guestrequest.RequestType{
		guestrequest.RequestTypeAdd,
		guestrequest.RequestTypeRemove,
		guestrequest.RequestTypePreAdd,
	} {
		t.Run(string(requestType), func(t *testing.T) {
			host := NewHost(nil, nil, &securitypolicy.OpenDoorSecurityPolicyEnforcer{}, io.Discard)
			err := host.modifyHostSettings(context.Background(), UVMContainerID, &guestrequest.ModificationRequest{
				ResourceType: guestresource.ResourceTypePodCgroupMemoryLimit,
				RequestType:  requestType,
			})
			if err == nil || !strings.Contains(err.Error(), "RequestType") {
				t.Fatalf("modifyHostSettings returned %v, want invalid RequestType error", err)
			}
		})
	}
}

// testPodCgroup records Update calls; other cgroup.Manager methods are unused here.
type testPodCgroup struct {
	cgroup.Manager
	limit     int64
	updates   int
	calls     []*oci.LinuxResources
	updateErr error
}

func (cg *testPodCgroup) Update(resources *oci.LinuxResources) error {
	cg.calls = append(cg.calls, resources)
	if cg.updateErr != nil {
		return cg.updateErr
	}
	cg.updates++
	cg.limit = *resources.Memory.Limit
	return nil
}

func newHostWithPods(t *testing.T, ids ...string) (*Host, map[string]*testPodCgroup) {
	t.Helper()
	host := NewHost(nil, nil, &securitypolicy.OpenDoorSecurityPolicyEnforcer{}, io.Discard)
	cgroups := make(map[string]*testPodCgroup, len(ids))
	for _, id := range ids {
		cg := &testPodCgroup{limit: -1}
		cgroups[id] = cg
		host.pods[id] = &pod{sandboxID: id, cgroupControl: cg, containers: map[string]bool{id: true}}
	}
	return host, cgroups
}

func podMemoryLimitRequest(requestType guestrequest.RequestType, settings interface{}) *guestrequest.ModificationRequest {
	return &guestrequest.ModificationRequest{
		ResourceType: guestresource.ResourceTypePodMemoryLimit,
		RequestType:  requestType,
		Settings:     settings,
	}
}

func podMemoryLimit(podID string, limit int64) *guestresource.LCOWPodMemoryLimit {
	return &guestresource.LCOWPodMemoryLimit{PodID: podID, LimitInBytes: &limit}
}

func onlyMemoryLimit(limit int64) *oci.LinuxResources {
	return &oci.LinuxResources{Memory: &oci.LinuxMemory{Limit: &limit}}
}

func TestModifyHostSettingsPodMemoryLimitUpdatesOnlyTargetPod(t *testing.T) {
	const limit int64 = 134217728
	host, cgroups := newHostWithPods(t, "pod1", "pod2")

	err := host.modifyHostSettings(context.Background(), UVMContainerID,
		podMemoryLimitRequest(guestrequest.RequestTypeUpdate, podMemoryLimit("pod1", limit)))
	if err != nil {
		t.Fatalf("modifyHostSettings(%d) returned %v", limit, err)
	}
	if got := cgroups["pod1"].calls; len(got) != 1 || !reflect.DeepEqual(got[0], onlyMemoryLimit(limit)) {
		t.Fatalf("pod1 Update calls = %+v; want exactly one Memory.Limit=%d write", got, limit)
	}
	if len(cgroups["pod2"].calls) != 0 || cgroups["pod2"].limit != -1 {
		t.Fatalf("sibling pod2 changed: calls=%d limit=%d", len(cgroups["pod2"].calls), cgroups["pod2"].limit)
	}
}

func TestModifyHostSettingsPodMemoryLimitRejectsInvalidRequests(t *testing.T) {
	for _, tc := range []struct {
		name    string
		req     *guestrequest.ModificationRequest
		wantErr string
	}{
		{"unknown pod", podMemoryLimitRequest(guestrequest.RequestTypeUpdate, podMemoryLimit("nope", 1)), "does not exist"},
		{"add request type", podMemoryLimitRequest(guestrequest.RequestTypeAdd, podMemoryLimit("pod1", 1)), "RequestType"},
		{"remove request type", podMemoryLimitRequest(guestrequest.RequestTypeRemove, podMemoryLimit("pod1", 1)), "RequestType"},
		{"nil settings", podMemoryLimitRequest(guestrequest.RequestTypeUpdate, nil), "LCOWPodMemoryLimit"},
		{"typed nil settings", podMemoryLimitRequest(guestrequest.RequestTypeUpdate, (*guestresource.LCOWPodMemoryLimit)(nil)), "missing"},
		{"container settings", podMemoryLimitRequest(guestrequest.RequestTypeUpdate, &guestresource.LCOWContainerConstraints{}), "LCOWPodMemoryLimit"},
		{"empty pod id", podMemoryLimitRequest(guestrequest.RequestTypeUpdate, podMemoryLimit("", 1)), "empty PodID"},
		{"nil limit", podMemoryLimitRequest(guestrequest.RequestTypeUpdate, &guestresource.LCOWPodMemoryLimit{PodID: "pod1"}), "LimitInBytes"},
		{"zero limit", podMemoryLimitRequest(guestrequest.RequestTypeUpdate, podMemoryLimit("pod1", 0)), "invalid pod memory limit"},
		{"unlimited -1", podMemoryLimitRequest(guestrequest.RequestTypeUpdate, podMemoryLimit("pod1", -1)), "invalid pod memory limit"},
		{"negative limit", podMemoryLimitRequest(guestrequest.RequestTypeUpdate, podMemoryLimit("pod1", -2)), "invalid pod memory limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host, cgroups := newHostWithPods(t, "pod1")
			err := host.modifyHostSettings(context.Background(), UVMContainerID, tc.req)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("modifyHostSettings returned %v, want error containing %q", err, tc.wantErr)
			}
			if n := len(cgroups["pod1"].calls); n != 0 {
				t.Fatalf("pod1 cgroup Update called %d times on rejected request", n)
			}
		})
	}
}

// TestModifyHostSettingsPodMemoryLimitWire drives raw bridge JSON through the
// real decoder into the handler.
func TestModifyHostSettingsPodMemoryLimitWire(t *testing.T) {
	message := func(settings string) []byte {
		m := `{"Request":{"ResourceType":"PodMemoryLimit","RequestType":"Update"`
		if settings != "" {
			m += `,"Settings":` + settings
		}
		return []byte(m + `}}`)
	}
	handle := func(host *Host, b []byte) error {
		request, err := prot.UnmarshalContainerModifySettings(b)
		if err != nil {
			return err
		}
		return host.modifyHostSettings(context.Background(), UVMContainerID, request.Request.(*guestrequest.ModificationRequest))
	}

	host, cgroups := newHostWithPods(t, "pod1", "pod2")
	if err := handle(host, message(`{"PodID":"pod1","LimitInBytes":134217728}`)); err != nil {
		t.Fatalf("valid wire request returned %v", err)
	}
	if got := cgroups["pod1"].calls; len(got) != 1 || !reflect.DeepEqual(got[0], onlyMemoryLimit(134217728)) || len(cgroups["pod2"].calls) != 0 {
		t.Fatalf("valid wire request: pod1 calls=%+v pod2 calls=%d", got, len(cgroups["pod2"].calls))
	}

	for name, settings := range map[string]string{
		"omitted settings": "",
		"null settings":    `null`,
		"empty settings":   `{}`,
		"empty pod id":     `{"PodID":"","LimitInBytes":134217728}`,
		"omitted limit":    `{"PodID":"pod1"}`,
		"null limit":       `{"PodID":"pod1","LimitInBytes":null}`,
		"zero limit":       `{"PodID":"pod1","LimitInBytes":0}`,
		"unlimited limit":  `{"PodID":"pod1","LimitInBytes":-1}`,
		"string limit":     `{"PodID":"pod1","LimitInBytes":"1"}`,
		"unknown field":    `{"PodID":"pod1","LimitInBytes":134217728,"Linux":{"cpu":{"quota":1}}}`,
		"wrong shape":      `[]`,
	} {
		t.Run(name, func(t *testing.T) {
			host, cgroups := newHostWithPods(t, "pod1")
			if err := handle(host, message(settings)); err == nil {
				t.Fatalf("settings %q accepted", settings)
			}
			if n := len(cgroups["pod1"].calls); n != 0 {
				t.Fatalf("pod1 cgroup Update called %d times on rejected request", n)
			}
		})
	}
}

func TestModifyHostSettingsPodMemoryLimitFailureThenRetry(t *testing.T) {
	host, cgroups := newHostWithPods(t, "pod1", "pod2")
	updateErr := errors.New("cgroup write failed")
	cgroups["pod1"].updateErr = updateErr
	req := podMemoryLimitRequest(guestrequest.RequestTypeUpdate, podMemoryLimit("pod1", 134217728))

	if err := host.modifyHostSettings(context.Background(), UVMContainerID, req); !errors.Is(err, updateErr) {
		t.Fatalf("modifyHostSettings returned %v, want wrapped %v", err, updateErr)
	}
	if cgroups["pod1"].limit != -1 || len(cgroups["pod2"].calls) != 0 || cgroups["pod2"].limit != -1 {
		t.Fatalf("state changed after failure: pod1 limit=%d, pod2 calls=%d limit=%d",
			cgroups["pod1"].limit, len(cgroups["pod2"].calls), cgroups["pod2"].limit)
	}
	if _, ok := host.pods["pod1"]; !ok {
		t.Fatal("pod1 unregistered after failed update")
	}

	cgroups["pod1"].updateErr = nil
	if err := host.modifyHostSettings(context.Background(), UVMContainerID, req); err != nil {
		t.Fatalf("retry returned %v", err)
	}
	if cgroups["pod1"].limit != 134217728 || cgroups["pod1"].updates != 1 || len(cgroups["pod2"].calls) != 0 {
		t.Fatalf("after retry: pod1 limit=%d updates=%d, pod2 calls=%d",
			cgroups["pod1"].limit, cgroups["pod1"].updates, len(cgroups["pod2"].calls))
	}
}

type testCgroupUpdater struct {
	limit   int64
	updates int
}

func (cgroup *testCgroupUpdater) Update(resources *oci.LinuxResources) error {
	cgroup.updates++
	cgroup.limit = *resources.Memory.Limit
	return nil
}

func Test_Add_Remove_RWDevice(t *testing.T) {
	hm := newHostMounts()
	mountPath := "/run/gcs/c/abcd"
	sourcePath := "/dev/sda"

	hm.Lock()
	defer hm.Unlock()

	if err := hm.AddRWDevice(mountPath, sourcePath, false); err != nil {
		t.Fatalf("unexpected error adding RW device: %s", err)
	}
	if err := hm.RemoveRWDevice(mountPath, sourcePath, false); err != nil {
		t.Fatalf("unexpected error removing RW device: %s", err)
	}
}

func Test_Cannot_AddRWDevice_Twice(t *testing.T) {
	hm := newHostMounts()
	mountPath := "/run/gcs/c/abc"
	sourcePath := "/dev/sda"

	hm.Lock()
	if err := hm.AddRWDevice(mountPath, sourcePath, false); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	hm.Unlock()

	hm.Lock()
	if err := hm.AddRWDevice(mountPath, sourcePath, false); err == nil {
		t.Fatalf("expected error adding %q for the second time", mountPath)
	}
	hm.Unlock()
}

func Test_Cannot_RemoveRWDevice_Wrong_Source(t *testing.T) {
	hm := newHostMounts()
	hm.Lock()
	defer hm.Unlock()

	mountPath := "/run/gcs/c/abcd"
	sourcePath := "/dev/sda"
	wrongSource := "/dev/sdb"
	if err := hm.AddRWDevice(mountPath, sourcePath, false); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if err := hm.RemoveRWDevice(mountPath, wrongSource, false); err == nil {
		t.Fatalf("expected error removing wrong source %s", wrongSource)
	}
}

func Test_Cannot_RemoveRWDevice_Wrong_Encrypted(t *testing.T) {
	hm := newHostMounts()
	hm.Lock()
	defer hm.Unlock()

	mountPath := "/run/gcs/c/abcd"
	sourcePath := "/dev/sda"
	if err := hm.AddRWDevice(mountPath, sourcePath, false); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if err := hm.RemoveRWDevice(mountPath, sourcePath, true); err == nil {
		t.Fatalf("expected error removing RW device with wrong encrypted flag")
	}
}

func Test_HostMounts_IsEncrypted(t *testing.T) {
	hm := newHostMounts()
	hm.Lock()
	defer hm.Unlock()

	encryptedPath := "/run/gcs/c/encrypted"
	encryptedSource := "/dev/sda"
	if err := hm.AddRWDevice(encryptedPath, encryptedSource, true); err != nil {
		t.Fatalf("unexpected error adding RW device: %s", err)
	}
	nestedUnencrypted := "/run/gcs/c/encrypted/unencrypted"
	unencryptedSource := "/dev/sdb"
	if err := hm.AddRWDevice(nestedUnencrypted, unencryptedSource, false); err != nil {
		t.Fatalf("unexpected error adding RW device: %s", err)
	}

	for _, tc := range []struct {
		name     string
		testPath string
		expected bool
	}{
		{
			name:     "ValidSubPath1",
			testPath: "/run/gcs/c/encrypted/nested",
			expected: true,
		},
		{
			name:     "ValidSubPath2",
			testPath: "/run/gcs/c/encrypted/../encrypted/nested",
			expected: true,
		},
		{
			name:     "NotSubPath1",
			testPath: "/run/gcs/c/abcdef",
			expected: false,
		},
		{
			name:     "NotSubPath2",
			testPath: "/run/gcs/c",
			expected: false,
		},
		{
			name:     "NotSubPath3",
			testPath: "/run/gcs/c/../abcd",
			expected: false,
		},
		{
			name:     "NestedUnencrypted",
			testPath: "/run/gcs/c/encrypted/unencrypted/foo",
			expected: false,
		},
		{
			name:     "SamePathEncrypted",
			testPath: "/run/gcs/c/encrypted",
			expected: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			encrypted := hm.IsEncrypted(tc.testPath)
			if encrypted != tc.expected {
				t.Fatalf("expected encrypted %t, got %t", tc.expected, encrypted)
			}
		})
	}
}

func Test_HostMounts_AddRemoveRODevice(t *testing.T) {
	hm := newHostMounts()
	hm.Lock()
	defer hm.Unlock()

	mountPath := "/run/gcs/c/abcd"
	sourcePath := "/dev/sda"

	if err := hm.AddRODevice(mountPath, sourcePath); err != nil {
		t.Fatalf("unexpected error adding RO device: %s", err)
	}

	if err := hm.RemoveRODevice(mountPath, sourcePath); err != nil {
		t.Fatalf("unexpected error removing RO device: %s", err)
	}
}

func Test_HostMounts_Cannot_AddRODevice_Twice(t *testing.T) {
	hm := newHostMounts()
	hm.Lock()
	defer hm.Unlock()

	mountPath := "/run/gcs/c/abc"
	sourcePath := "/dev/sda"

	if err := hm.AddRODevice(mountPath, sourcePath); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if err := hm.AddRODevice(mountPath, sourcePath); err == nil {
		t.Fatalf("expected error adding %q for the second time", mountPath)
	}
}

func Test_HostMounts_AddRemoveOverlay(t *testing.T) {
	hm := newHostMounts()
	hm.Lock()
	defer hm.Unlock()

	mountPath := "/run/gcs/c/aaaa/rootfs"
	layers := []string{
		"/run/mounts/scsi/m1",
		"/run/mounts/scsi/m2",
		"/run/mounts/scsi/m3",
	}
	for _, layer := range layers {
		if err := hm.AddRODevice(layer, layer); err != nil {
			t.Fatalf("unexpected error adding RO device: %s", err)
		}
	}
	scratchDir := "/run/gcs/c/aaaa/scratch"
	if err := hm.AddRWDevice(scratchDir, scratchDir, true); err != nil {
		t.Fatalf("unexpected error adding RW device: %s", err)
	}
	if err := hm.AddOverlay(mountPath, layers, scratchDir); err != nil {
		t.Fatalf("unexpected error adding overlay: %s", err)
	}
	undo, err := hm.RemoveOverlay(mountPath)
	if err != nil {
		t.Fatalf("unexpected error removing overlay: %s", err)
	}
	if undo == nil {
		t.Fatalf("expected undo function to be non-nil")
	}
	undo()
	if _, err = hm.RemoveOverlay(mountPath); err != nil {
		t.Fatalf("unexpected error removing overlay again: %s", err)
	}
}

func Test_HostMounts_Cannot_RemoveInUseDeviceByOverlay(t *testing.T) {
	hm := newHostMounts()
	hm.Lock()
	defer hm.Unlock()

	mountPath := "/run/gcs/c/aaaa/rootfs"
	layers := []string{
		"/run/mounts/scsi/m1",
		"/run/mounts/scsi/m2",
		"/run/mounts/scsi/m3",
	}
	for _, layer := range layers {
		if err := hm.AddRODevice(layer, layer); err != nil {
			t.Fatalf("unexpected error adding RO device: %s", err)
		}
	}
	scratchDir := "/run/gcs/c/aaaa/scratch"
	if err := hm.AddRWDevice(scratchDir, scratchDir, true); err != nil {
		t.Fatalf("unexpected error adding RW device: %s", err)
	}
	if err := hm.AddOverlay(mountPath, layers, scratchDir); err != nil {
		t.Fatalf("unexpected error adding overlay: %s", err)
	}

	for _, layer := range layers {
		if err := hm.RemoveRODevice(layer, layer); err == nil {
			t.Fatalf("expected error removing RO device %s while in use by overlay", layer)
		}
	}
	if err := hm.RemoveRWDevice(scratchDir, scratchDir, true); err == nil {
		t.Fatalf("expected error removing RW device %s while in use by overlay", scratchDir)
	}

	if _, err := hm.RemoveOverlay(mountPath); err != nil {
		t.Fatalf("unexpected error removing overlay: %s", err)
	}

	// now we can remove
	for _, layer := range layers {
		if err := hm.RemoveRODevice(layer, layer); err != nil {
			t.Fatalf("unexpected error removing RO device %s: %s", layer, err)
		}
	}
	if err := hm.RemoveRWDevice(scratchDir, scratchDir, true); err != nil {
		t.Fatalf("unexpected error removing RW device %s: %s", scratchDir, err)
	}
}

func Test_HostMounts_Cannot_RemoveInUseDeviceByOverlay_MultipleUsers(t *testing.T) {
	hm := newHostMounts()
	hm.Lock()
	defer hm.Unlock()

	overlay1 := "/run/gcs/c/aaaa/rootfs"
	overlay2 := "/run/gcs/c/bbbb/rootfs"
	layers := []string{
		"/run/mounts/scsi/m1",
		"/run/mounts/scsi/m2",
		"/run/mounts/scsi/m3",
	}
	for _, layer := range layers {
		if err := hm.AddRODevice(layer, layer); err != nil {
			t.Fatalf("unexpected error adding RO device: %s", err)
		}
	}
	sharedScratchMount := "/run/gcs/c/sandbox"
	scratch1 := sharedScratchMount + "/scratch/aaaa"
	scratch2 := sharedScratchMount + "/scratch/bbbb"
	if err := hm.AddRWDevice(sharedScratchMount, sharedScratchMount, true); err != nil {
		t.Fatalf("unexpected error adding RW device: %s", err)
	}
	if err := hm.AddOverlay(overlay1, layers, scratch1); err != nil {
		t.Fatalf("unexpected error adding overlay1: %s", err)
	}

	if err := hm.AddOverlay(overlay2, layers[0:2], scratch2); err != nil {
		t.Fatalf("unexpected error adding overlay2: %s", err)
	}

	for _, layer := range layers {
		if err := hm.RemoveRODevice(layer, layer); err == nil {
			t.Fatalf("expected error removing RO device %s while in use by overlay", layer)
		}
	}
	if err := hm.RemoveRWDevice(sharedScratchMount, sharedScratchMount, true); err == nil {
		t.Fatalf("expected error removing RW device %s while in use by overlay", sharedScratchMount)
	}

	if _, err := hm.RemoveOverlay(overlay1); err != nil {
		t.Fatalf("unexpected error removing overlay 1: %s", err)
	}

	for _, layer := range layers[0:2] {
		if err := hm.RemoveRODevice(layer, layer); err == nil {
			t.Fatalf("expected error removing RO device %s (still in use by overlay 2)", layer)
		}
	}
	if err := hm.RemoveRODevice(layers[2], layers[2]); err != nil {
		t.Fatalf("unexpected error removing layers[2] which is not being used by overlay 2: %s", err)
	}
	if err := hm.RemoveRWDevice(sharedScratchMount, sharedScratchMount, true); err == nil {
		t.Fatalf("expected error removing RW device %s while in use by overlay 2", scratch2)
	}

	if _, err := hm.RemoveOverlay(overlay2); err != nil {
		t.Fatalf("unexpected error removing overlay 2: %s", err)
	}
	for _, layer := range layers[0:2] {
		if err := hm.RemoveRODevice(layer, layer); err != nil {
			t.Fatalf("unexpected error removing RO device %s: %s", layer, err)
		}
	}
	if err := hm.RemoveRWDevice(sharedScratchMount, sharedScratchMount, true); err != nil {
		t.Fatalf("unexpected error removing RW device %s: %s", sharedScratchMount, err)
	}
}

func Test_HostMounts_RemoveOverlay_PathNormalization_AddUncleanRemoveClean(t *testing.T) {
	hm := newHostMounts()
	hm.Lock()
	defer hm.Unlock()

	layer := "/run/mounts/scsi/m1"
	if err := hm.AddRODevice(layer, layer); err != nil {
		t.Fatalf("unexpected error adding RO device: %s", err)
	}

	scratch := "/run/gcs/c/aaaa/scratch"
	if err := hm.AddRWDevice(scratch, scratch, false); err != nil {
		t.Fatalf("unexpected error adding RW device: %s", err)
	}

	uncleanOverlay := "/run/gcs/c/aaaa/./rootfs"
	cleanOverlay := "/run/gcs/c/aaaa/rootfs"

	if err := hm.AddOverlay(uncleanOverlay, []string{layer}, scratch); err != nil {
		t.Fatalf("unexpected error adding overlay with unclean path: %s", err)
	}

	if _, err := hm.RemoveOverlay(cleanOverlay); err != nil {
		t.Fatalf("expected removing overlay with clean path to succeed after add with unclean path, got error: %s", err)
	}
}

func Test_HostMounts_RemoveOverlay_PathNormalization_AddCleanRemoveUnclean(t *testing.T) {
	hm := newHostMounts()
	hm.Lock()
	defer hm.Unlock()

	layer := "/run/mounts/scsi/m1"
	if err := hm.AddRODevice(layer, layer); err != nil {
		t.Fatalf("unexpected error adding RO device: %s", err)
	}

	scratch := "/run/gcs/c/aaaa/scratch"
	if err := hm.AddRWDevice(scratch, scratch, false); err != nil {
		t.Fatalf("unexpected error adding RW device: %s", err)
	}

	cleanOverlay := "/run/gcs/c/aaaa/rootfs"
	uncleanOverlay := "/run/gcs/c/aaaa/./rootfs"

	if err := hm.AddOverlay(cleanOverlay, []string{layer}, scratch); err != nil {
		t.Fatalf("unexpected error adding overlay with clean path: %s", err)
	}

	if _, err := hm.RemoveOverlay(uncleanOverlay); err != nil {
		t.Fatalf("expected removing overlay with unclean path to succeed after add with clean path, got error: %s", err)
	}
}
