//go:build windows && (lcow || wcow) && openvmm_prototype

package manager

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Microsoft/hcsshim/hcn"
	"github.com/Microsoft/hcsshim/internal/hns"

	"github.com/Microsoft/go-winio/pkg/guid"
)

// BoundEndpoint is what a bind attempt against an HNS endpoint reported. PortID is always
// the port the bind attempted, even when it accompanies an error, so a caller can unbind
// exactly that tuple after a failure in the bind RPC or in the requery that follows it.
// EndpointID, SwitchID, and MacAddress are only meaningful once the requery has succeeded.
type BoundEndpoint struct {
	PortID     guid.GUID
	EndpointID string
	SwitchID   string
	MacAddress string
}

// EndpointPortBinder binds and releases a Direct I/O network port.
type EndpointPortBinder interface {
	// Bind creates a switch port for nicID on the switch backing endpointID, then requeries
	// the endpoint and reports what it found.
	Bind(ctx context.Context, endpointID string, nicID guid.GUID) (BoundEndpoint, error)
	// Unbind releases exactly the port tuple a prior Bind - attempted or completed -
	// produced.
	Unbind(ctx context.Context, endpointID string, portID guid.GUID, nicID guid.GUID) error
}

type hcnEndpointPortBinder struct{}

func (hcnEndpointPortBinder) Bind(_ context.Context, endpointID string, nicID guid.GUID) (BoundEndpoint, error) {
	portID, err := guid.NewV4()
	if err != nil {
		return BoundEndpoint{}, fmt.Errorf("failed to generate a port id to bind endpoint %s to NIC %s: %w", endpointID, nicID, err)
	}

	if err := modifyEndpointPort(endpointID, portID, nicID, hcn.RequestTypeAdd); err != nil {
		return BoundEndpoint{PortID: portID}, fmt.Errorf("failed to bind HNS port %s to endpoint %s: %w", portID, endpointID, err)
	}

	endpoint, err := hcn.GetEndpointByID(endpointID)
	if err != nil {
		return BoundEndpoint{PortID: portID}, fmt.Errorf("failed to requery endpoint %s after binding port %s: %w", endpointID, portID, err)
	}
	network, err := hns.GetHNSNetworkByID(endpoint.HostComputeNetwork)
	if err != nil {
		return BoundEndpoint{PortID: portID}, fmt.Errorf("failed to resolve switch for endpoint %s on network %s: %w", endpointID, endpoint.HostComputeNetwork, err)
	}

	return BoundEndpoint{
		PortID:     portID,
		EndpointID: endpoint.Id,
		SwitchID:   endpointSwitchID(endpoint, network),
		MacAddress: endpoint.MacAddress,
	}, nil
}

func endpointSwitchID(endpoint *hcn.HostComputeEndpoint, network *hns.HNSNetwork) string {
	if network.SwitchGuid != "" {
		return network.SwitchGuid
	}
	return endpoint.HostComputeNetwork
}

func (hcnEndpointPortBinder) Unbind(_ context.Context, endpointID string, portID guid.GUID, nicID guid.GUID) error {
	if err := modifyEndpointPort(endpointID, portID, nicID, hcn.RequestTypeRemove); err != nil {
		return fmt.Errorf("failed to unbind HNS port %s from endpoint %s: %w", portID, endpointID, err)
	}
	return nil
}

func modifyEndpointPort(endpointID string, portID guid.GUID, nicID guid.GUID, requestType hcn.RequestType) error {
	settings, err := json.Marshal(hcn.VmEndpointRequest{PortId: portID, VirtualNicName: nicID.String()})
	if err != nil {
		return fmt.Errorf("failed to encode the HNS port request for endpoint %s port %s: %w", endpointID, portID, err)
	}
	return hcn.ModifyEndpointSettings(endpointID, &hcn.ModifyEndpointSettingRequest{
		ResourceType: hcn.EndpointResourceTypePort,
		RequestType:  requestType,
		Settings:     settings,
	})
}
