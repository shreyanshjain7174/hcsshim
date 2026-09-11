//go:build windows && (lcow || wcow) && openvmm_prototype

package manager

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Microsoft/hcsshim/internal/hcs/resourcepaths"
	hcsschema "github.com/Microsoft/hcsshim/internal/hcs/schema2"
	"github.com/Microsoft/hcsshim/internal/protocol/guestrequest"
	"github.com/Microsoft/hcsshim/internal/vmservice"
)

var errInvalidSCSIAttachment = errors.New("the SCSI attachment cannot be represented by the OpenVMM runtime")

func scsiControllerIndex(controllerID string) (uint32, error) {
	for index, knownID := range guestrequest.ScsiControllerGuids {
		if strings.EqualFold(controllerID, knownID) {
			return uint32(index), nil
		}
	}
	return 0, fmt.Errorf("unknown SCSI controller %q: %w", controllerID, errInvalidSCSIAttachment)
}

func scsiDiskType(attachment hcsschema.Attachment, controller, lun uint32) (vmservice.DiskType, error) {
	if attachment.Path == "" {
		return 0, fmt.Errorf("SCSI attachment at controller %d LUN %d has an empty path: %w", controller, lun, errInvalidSCSIAttachment)
	}
	switch attachment.Type_ {
	case "VirtualDisk":
		switch strings.ToLower(filepath.Ext(attachment.Path)) {
		case ".vhd":
			return vmservice.DiskType_SCSI_DISK_TYPE_VHD1, nil
		case ".vhdx":
			return vmservice.DiskType_SCSI_DISK_TYPE_VHDX, nil
		}
	case "PassThru":
		return vmservice.DiskType_SCSI_DISK_TYPE_PHYSICAL, nil
	}
	return 0, fmt.Errorf("SCSI attachment at controller %d LUN %d has unsupported type %q or path %q: %w", controller, lun, attachment.Type_, attachment.Path, errInvalidSCSIAttachment)
}

// isSCSIResourcePath reports whether path is shaped like resourcepaths.SCSIResourceFormat
// (VirtualMachine/Devices/Scsi/...), without requiring the controller and LUN segments to
// be well-formed. It routes a SCSI-shaped path - malformed or not - to ParseSCSIResourcePath
// for the named-error verdict, and every other path to the not-implemented arm.
func isSCSIResourcePath(path string) bool {
	segments := strings.SplitN(path, "/", 4)
	return len(segments) >= 3 && segments[0] == "VirtualMachine" && segments[1] == "Devices" && segments[2] == "Scsi"
}

// ParseSCSIResourcePath resolves a ModifySettingRequest.ResourcePath built from
// resourcepaths.SCSIResourceFormat back to the controller index the guest sees (via the
// shared scsiControllerIndex lookup) and the LUN. An unrecognised GUID, a non-numeric LUN,
// a wrong segment name, and a path with trailing segments are each a distinct named error;
// none of them fall back to controller 0, which would attach a disk to the wrong controller.
func ParseSCSIResourcePath(path string) (controller uint32, lun uint32, err error) {
	segments := strings.Split(path, "/")
	if len(segments) != 6 || segments[0] != "VirtualMachine" || segments[1] != "Devices" || segments[2] != "Scsi" || segments[4] != "Attachments" {
		return 0, 0, fmt.Errorf("SCSI resource path %q does not match the %q shape: %w", path, resourcepaths.SCSIResourceFormat, errInvalidSCSIAttachment)
	}

	controller, err = scsiControllerIndex(segments[3])
	if err != nil {
		return 0, 0, err
	}

	lunValue, err := strconv.ParseUint(segments[5], 10, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("SCSI resource path %q has a non-numeric LUN %q: %w", path, segments[5], errInvalidSCSIAttachment)
	}
	return controller, uint32(lunValue), nil
}
