//go:build windows && (lcow || wcow) && openvmm_prototype

package manager

import (
	"errors"
	"fmt"

	hcsschema "github.com/Microsoft/hcsshim/internal/hcs/schema2"
	"github.com/Microsoft/hcsshim/internal/protocol/guestrequest"
	"github.com/Microsoft/hcsshim/internal/vmservice"
	"github.com/containerd/errdefs"
)

// ErrModifyNotSupported names every hot-plug ModifySettingRequest this backend has no
// vmservice translation for. It wraps errdefs.ErrNotImplemented so a caller can match
// either sentinel with errors.Is.
var ErrModifyNotSupported = fmt.Errorf("this ModifySettingRequest has no vmservice translation on the %s backend: %w", BackendName, errdefs.ErrNotImplemented)

var errMalformedModifyRequest = errors.New("no usable ModifySettingRequest was supplied to the openvmm modify translation")

// BuildModifyResourceRequest translates one hot-plug ModifySettingRequest into the
// vmservice request that performs it. Only a SCSI add or remove translates; a SCSI update
// and every non-SCSI resource path (network, vPCI, vPMem, plan9, VSMB, memory, processor,
// ...) is a named not-implemented error with a nil request - this function never returns a
// partial request beside an error.
func BuildModifyResourceRequest(req *hcsschema.ModifySettingRequest) (*vmservice.ModifyResourceRequest, error) {
	if req == nil {
		return nil, fmt.Errorf("cannot build a ModifyResourceRequest: %w", errMalformedModifyRequest)
	}
	if req.ResourcePath == "" {
		return nil, fmt.Errorf("ModifySettingRequest.ResourcePath is empty: %w", errMalformedModifyRequest)
	}
	if !isSCSIResourcePath(req.ResourcePath) {
		return nil, fmt.Errorf("ModifySettingRequest.ResourcePath %q has no vmservice modify translation: %w", req.ResourcePath, ErrModifyNotSupported)
	}

	controller, lun, err := ParseSCSIResourcePath(req.ResourcePath)
	if err != nil {
		return nil, err
	}

	switch req.RequestType {
	case guestrequest.RequestTypeAdd:
		return buildSCSIAddRequest(req, controller, lun)
	case guestrequest.RequestTypeRemove:
		return &vmservice.ModifyResourceRequest{
			Type: vmservice.ModifyType_REMOVE,
			Resource: &vmservice.ModifyResourceRequest_ScsiDisk{
				ScsiDisk: &vmservice.SCSIDisk{
					Controller: controller,
					Lun:        lun,
				},
			},
		}, nil
	default:
		return nil, fmt.Errorf("RequestType %q on SCSI resource path %q is not supported: %w", req.RequestType, req.ResourcePath, ErrModifyNotSupported)
	}
}

func buildSCSIAddRequest(req *hcsschema.ModifySettingRequest, controller, lun uint32) (*vmservice.ModifyResourceRequest, error) {
	attachment, ok := req.Settings.(hcsschema.Attachment)
	if !ok {
		return nil, fmt.Errorf("SCSI add at controller %d LUN %d carries Settings of type %T, want hcsschema.Attachment: %w", controller, lun, req.Settings, errInvalidSCSIAttachment)
	}
	diskType, err := scsiDiskType(attachment, controller, lun)
	if err != nil {
		return nil, err
	}
	return &vmservice.ModifyResourceRequest{
		Type: vmservice.ModifyType_ADD,
		Resource: &vmservice.ModifyResourceRequest_ScsiDisk{
			ScsiDisk: &vmservice.SCSIDisk{
				Controller: controller,
				Lun:        lun,
				HostPath:   attachment.Path,
				Type:       diskType,
				ReadOnly:   attachment.ReadOnly,
			},
		},
	}, nil
}
