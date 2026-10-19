//go:build linux
// +build linux

package storage

import (
	"bufio"
	"context"
	gerrors "errors"
	"fmt"
	"os"
	"path"
	"strings"
	"sync"
	"syscall"

	"github.com/Microsoft/hcsshim/internal/ot"
	"github.com/pkg/errors"
	"go.opentelemetry.io/otel/attribute"
	"golang.org/x/sys/unix"

	"github.com/Microsoft/hcsshim/internal/log"
)

const procMountFile = "/proc/mounts"
const numProcMountFields = 6

// Test dependencies
var (
	osStat      = os.Stat
	unixUnmount = unix.Unmount
	unixMount   = unix.Mount
	osRemoveAll = os.RemoveAll
	osMkdirAll  = os.MkdirAll
	listMounts  = listMountPointsUnderPath

	flags = map[string]struct {
		clear bool
		flag  uintptr
	}{
		"acl":           {false, unix.MS_POSIXACL},
		"async":         {true, unix.MS_SYNCHRONOUS},
		"atime":         {true, unix.MS_NOATIME},
		"bind":          {false, unix.MS_BIND},
		"defaults":      {false, 0},
		"dev":           {true, unix.MS_NODEV},
		"diratime":      {true, unix.MS_NODIRATIME},
		"dirsync":       {false, unix.MS_DIRSYNC},
		"exec":          {true, unix.MS_NOEXEC},
		"iversion":      {false, unix.MS_I_VERSION},
		"lazytime":      {false, unix.MS_LAZYTIME},
		"loud":          {true, unix.MS_SILENT},
		"mand":          {false, unix.MS_MANDLOCK},
		"noacl":         {true, unix.MS_POSIXACL},
		"noatime":       {false, unix.MS_NOATIME},
		"nodev":         {false, unix.MS_NODEV},
		"nodiratime":    {false, unix.MS_NODIRATIME},
		"noexec":        {false, unix.MS_NOEXEC},
		"noiversion":    {true, unix.MS_I_VERSION},
		"nolazytime":    {true, unix.MS_LAZYTIME},
		"nomand":        {true, unix.MS_MANDLOCK},
		"norelatime":    {true, unix.MS_RELATIME},
		"nostrictatime": {true, unix.MS_STRICTATIME},
		"nosuid":        {false, unix.MS_NOSUID},
		"rbind":         {false, unix.MS_BIND | unix.MS_REC},
		"relatime":      {false, unix.MS_RELATIME},
		"remount":       {false, unix.MS_REMOUNT},
		"ro":            {false, unix.MS_RDONLY},
		"rw":            {true, unix.MS_RDONLY},
		"silent":        {false, unix.MS_SILENT},
		"strictatime":   {false, unix.MS_STRICTATIME},
		"suid":          {true, unix.MS_NOSUID},
		"sync":          {false, unix.MS_SYNCHRONOUS},
	}

	propagationFlags = map[string]uintptr{
		"private":     unix.MS_PRIVATE,
		"shared":      unix.MS_SHARED,
		"slave":       unix.MS_SLAVE,
		"unbindable":  unix.MS_UNBINDABLE,
		"rprivate":    unix.MS_PRIVATE | unix.MS_REC,
		"rshared":     unix.MS_SHARED | unix.MS_REC,
		"rslave":      unix.MS_SLAVE | unix.MS_REC,
		"runbindable": unix.MS_UNBINDABLE | unix.MS_REC,
	}
)

func ParseMountOptions(options []string) (flagOpts uintptr, pgFlags []uintptr, data []string) {
	for _, o := range options {
		if f, exists := flags[o]; exists && f.flag != 0 {
			if f.clear {
				flagOpts &= ^f.flag
			} else {
				flagOpts |= f.flag
			}
		} else if f, exists := propagationFlags[o]; exists && f != 0 {
			pgFlags = append(pgFlags, f)
		} else {
			data = append(data, o)
		}
	}
	return
}

// MountRShared creates a bind mountpoint and marks it as rshared
// Expected that the filepath exists before calling this function
func MountRShared(path string) error {
	if path == "" {
		return errors.New("path must not be empty to mount as rshared")
	}
	if err := unixMount(path, path, "", syscall.MS_BIND, ""); err != nil {
		return fmt.Errorf("failed to create bind mount for %v: %w", path, err)
	}
	if err := unixMount(path, path, "", syscall.MS_SHARED|syscall.MS_REC, ""); err != nil {
		return fmt.Errorf("failed to make %v rshared: %w", path, err)
	}
	return nil
}

// UnmountPath unmounts the target path if it exists and is a mount path. If
// removeTarget this will remove the previously mounted folder.
func UnmountPath(ctx context.Context, target string, removeTarget bool) (err error) {
	_, span := ot.StartSpan(ctx, "storage::UnmountPath")
	defer span.End()
	defer func() { ot.SetSpanStatus(span, err) }()

	span.SetAttributes(
		attribute.String("target", target),
		attribute.Bool("remove", removeTarget))

	if _, err := osStat(target); err != nil {
		if os.IsNotExist(err) {
			log.G(ctx).WithField("target", target).Warnf("UnmountPath called for non-existent path")
			return nil
		}
		return errors.Wrapf(err, "failed to determine if path '%s' exists", target)
	}

	// Loop to handle stacked mounts (e.g. tmpfs + bind mount from MountRShared).
	// Each Unmount call only peels one layer off the stack, so we must keep
	// unmounting until EINVAL tells us nothing is mounted anymore.
	const maxUnmountAttempts = 5
	unmounted := false
	for i := 0; i < maxUnmountAttempts; i++ {
		if err := unixUnmount(target, 0); err != nil {
			// EINVAL means it's not mounted — we're done unmounting.
			if gerrors.Is(err, unix.EINVAL) {
				unmounted = true
				break
			}
			return errors.Wrapf(err, "failed to unmount path '%s'", target)
		}
	}
	if !unmounted {
		log.G(ctx).WithField("target", target).Warnf("path still mounted after %d unmount attempts", maxUnmountAttempts)
	}
	if removeTarget {
		return osRemoveAll(target)
	}
	return nil
}

func UnmountAllInPath(ctx context.Context, path string, removeTarget bool) (err error) {
	childMounts, err := listMounts(path)
	if err != nil {
		return err
	}

	for i := len(childMounts) - 1; i >= 0; i-- {
		childPath := childMounts[i]
		if err := UnmountPath(ctx, childPath, removeTarget); err != nil {
			return err
		}
	}
	return nil
}

func listMountPointsUnderPath(path string) ([]string, error) {
	var mountPoints []string
	f, err := os.Open(procMountFile)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Split(line, " ")
		if len(fields) < numProcMountFields {
			continue
		}
		destPath := fields[1]
		if strings.HasPrefix(destPath, path) {
			mountPoints = append(mountPoints, destPath)
		}
	}

	return mountPoints, nil
}

const virtioFSAggregateRoot = "/run/mounts/virtiofs"

var (
	virtioFSMu      sync.Mutex
	virtioFSMounted = map[string]bool{}
)

// isVirtioFSPathElem reports whether s is a single, non-relative path element.
func isVirtioFSPathElem(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, "/\x00")
}

// MountVirtioFSShare bind-mounts share from the aggregate virtio-fs device tagged tag onto target.
func MountVirtioFSShare(_ context.Context, tag, share, target string, readonly bool) error {
	if !isVirtioFSPathElem(tag) || len(tag) > 36 {
		return fmt.Errorf("invalid virtio-fs tag %q", tag)
	}
	if !isVirtioFSPathElem(share) {
		return fmt.Errorf("invalid virtio-fs share name %q", share)
	}
	agg := path.Join(virtioFSAggregateRoot, tag)

	virtioFSMu.Lock()
	defer virtioFSMu.Unlock()
	if !virtioFSMounted[tag] {
		if err := osMkdirAll(agg, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", agg, err)
		}
		if err := unixMount(tag, agg, "virtiofs", 0, ""); err != nil {
			return fmt.Errorf("mount virtio-fs %s at %s: %w", tag, agg, err)
		}
		virtioFSMounted[tag] = true
	}

	if err := osMkdirAll(target, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", target, err)
	}
	if err := unixMount(path.Join(agg, share), target, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind virtio-fs share %s to %s: %w", share, target, err)
	}
	if readonly {
		if err := unixMount("", target, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, ""); err != nil {
			_ = unixUnmount(target, 0)
			return fmt.Errorf("remount %s read-only: %w", target, err)
		}
	}
	return nil
}
