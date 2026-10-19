//go:build linux
// +build linux

package storage

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/pkg/errors"
	"golang.org/x/sys/unix"
)

func clearTestDependencies() {
	osStat = nil
	unixUnmount = nil
	osRemoveAll = nil
}

func Test_Unmount_Stat_Valid_Path(t *testing.T) {
	clearTestDependencies()

	expectedName := "/dev/fake"
	osStat = func(name string) (os.FileInfo, error) {
		if expectedName != name {
			t.Errorf("expected name: %s, got: %s", expectedName, name)
			return nil, errors.New("unexpected name")
		}
		return nil, nil
	}
	unixUnmount = func(target string, flags int) error {
		return nil
	}
	err := UnmountPath(context.Background(), expectedName, false)
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
}

func Test_Unmount_Stat_NotExist(t *testing.T) {
	clearTestDependencies()

	// Should return early
	osStat = func(name string) (os.FileInfo, error) {
		return nil, os.ErrNotExist
	}
	err := UnmountPath(context.Background(), "/dev/fake", false)
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
}

func Test_Unmount_Stat_OtherError_Error(t *testing.T) {
	clearTestDependencies()

	expectedErr := errors.New("expected stat err")
	osStat = func(name string) (os.FileInfo, error) {
		return nil, expectedErr
	}
	err := UnmountPath(context.Background(), "/dev/fake", false)
	if errors.Cause(err) != expectedErr { //nolint:errorlint
		t.Fatalf("expected err: %v, got: %v", expectedErr, err)
	}
}

func Test_Unmount_Valid_Target(t *testing.T) {
	clearTestDependencies()

	osStat = func(name string) (os.FileInfo, error) {
		return nil, nil
	}
	expectedTarget := "/dev/fake"
	unixUnmount = func(target string, flags int) error {
		if expectedTarget != target {
			t.Errorf("expected target: %s, got: %s", expectedTarget, target)
			return errors.New("unexpected target")
		}
		return nil
	}
	err := UnmountPath(context.Background(), expectedTarget, false)
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
}

func Test_Unmount_Valid_Flags(t *testing.T) {
	clearTestDependencies()

	osStat = func(name string) (os.FileInfo, error) {
		return nil, nil
	}
	unixUnmount = func(target string, flags int) error {
		if flags != 0 {
			t.Errorf("expected flags 0, got: %d", flags)
			return errors.New("unexpected flags")
		}
		return nil
	}
	err := UnmountPath(context.Background(), "/fake/path", false)
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
}

func Test_Unmount_NotMounted(t *testing.T) {
	clearTestDependencies()

	osStat = func(name string) (os.FileInfo, error) {
		return nil, nil
	}
	unixUnmount = func(target string, flags int) error {
		return unix.EINVAL
	}
	err := UnmountPath(context.Background(), "/dev/fake", false)
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
}

func Test_Unmount_OtherError(t *testing.T) {
	clearTestDependencies()

	osStat = func(name string) (os.FileInfo, error) {
		return nil, nil
	}
	expectedErr := errors.New("expected unmount error")
	unixUnmount = func(target string, flags int) error {
		return expectedErr
	}
	err := UnmountPath(context.Background(), "/dev/fake", false)
	if errors.Cause(err) != expectedErr { //nolint:errorlint
		t.Fatalf("expected err: %v, got: %v", expectedErr, err)
	}
}

func Test_Unmount_RemoveAll_Valid_Path(t *testing.T) {
	clearTestDependencies()

	osStat = func(name string) (os.FileInfo, error) {
		return nil, nil
	}
	unixUnmount = func(target string, flags int) error {
		return nil
	}
	expectedPath := "/fake/path"
	osRemoveAll = func(path string) error {
		if expectedPath != path {
			t.Errorf("expected path %s, got: %s", expectedPath, path)
			return errors.New("unexpected path")
		}
		return nil
	}
	err := UnmountPath(context.Background(), expectedPath, true)
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
}

func Test_Unmount_RemoveAll_Called(t *testing.T) {
	clearTestDependencies()

	osStat = func(name string) (os.FileInfo, error) {
		return nil, nil
	}
	unixUnmount = func(target string, flags int) error {
		return nil
	}
	removeAllCalled := false
	osRemoveAll = func(path string) error {
		removeAllCalled = true
		return nil
	}
	err := UnmountPath(context.Background(), "/fake/path", true)
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
	if !removeAllCalled {
		t.Fatal("expected remove to be called")
	}
}

func Test_UnmountAllInPath_Unmount_Order(t *testing.T) {
	clearTestDependencies()
	parent := "/fake"
	child := "/fake/test"
	listMounts = func(path string) ([]string, error) {
		return []string{parent, child}, nil
	}

	osStat = func(name string) (os.FileInfo, error) {
		return nil, nil
	}

	timesCalled := 0
	unixUnmount = func(target string, flags int) error {
		if timesCalled == 0 && target != child {
			return errors.Errorf("expected to unmount %v first, got %v", child, target)
		}
		timesCalled += 1
		return nil
	}

	osRemoveAll = func(path string) error {
		return nil
	}

	err := UnmountAllInPath(context.Background(), parent, true)

	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
}

type vfsMountCall struct {
	source, target, fstype string
	flags                  uintptr
}

func fakeVirtioFSMounts(t *testing.T) *[]vfsMountCall {
	t.Helper()
	var calls []vfsMountCall
	oldMount, oldMkdir, oldUnmount := unixMount, osMkdirAll, unixUnmount
	unixMount = func(source, target, fstype string, flags uintptr, _ string) error {
		calls = append(calls, vfsMountCall{source, target, fstype, flags})
		return nil
	}
	osMkdirAll = func(string, os.FileMode) error { return nil }
	unixUnmount = func(string, int) error { return nil }
	virtioFSMounted = map[string]bool{}
	t.Cleanup(func() { unixMount, osMkdirAll, unixUnmount = oldMount, oldMkdir, oldUnmount })
	return &calls
}

func TestMountVirtioFSShareMountsAggregateOnceThenBinds(t *testing.T) {
	calls := fakeVirtioFSMounts(t)
	ctx := context.Background()
	if err := MountVirtioFSShare(ctx, "hcsshim-shares", "0", "/run/mounts/plan9/0", false); err != nil {
		t.Fatal(err)
	}
	if err := MountVirtioFSShare(ctx, "hcsshim-shares", "1", "/run/mounts/plan9/1", true); err != nil {
		t.Fatal(err)
	}
	want := []vfsMountCall{
		{"hcsshim-shares", "/run/mounts/virtiofs/hcsshim-shares", "virtiofs", 0},
		{"/run/mounts/virtiofs/hcsshim-shares/0", "/run/mounts/plan9/0", "", unix.MS_BIND},
		{"/run/mounts/virtiofs/hcsshim-shares/1", "/run/mounts/plan9/1", "", unix.MS_BIND},
		{"", "/run/mounts/plan9/1", "", unix.MS_BIND | unix.MS_REMOUNT | unix.MS_RDONLY},
	}
	if len(*calls) != len(want) {
		t.Fatalf("calls = %+v", *calls)
	}
	for i := range want {
		if (*calls)[i] != want[i] {
			t.Fatalf("call %d = %+v, want %+v", i, (*calls)[i], want[i])
		}
	}
}

func TestMountVirtioFSShareRejectsBadShareName(t *testing.T) {
	calls := fakeVirtioFSMounts(t)
	for _, name := range []string{"", "..", "a/b", "../x"} {
		if err := MountVirtioFSShare(context.Background(), "hcsshim-shares", name, "/t", false); err == nil {
			t.Fatalf("share %q should be rejected", name)
		}
	}
	for _, tag := range []string{"", ".", "..", "a/b", "../../etc", "a\x00b", strings.Repeat("t", 37)} {
		if err := MountVirtioFSShare(context.Background(), tag, "0", "/t", false); err == nil {
			t.Fatalf("tag %q should be rejected", tag)
		}
	}
	if len(*calls) != 0 {
		t.Fatalf("rejected requests mounted: %+v", *calls)
	}
}
