//go:build windows && (lcow || wcow) && openvmm_prototype

package manager

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Microsoft/hcsshim/internal/hcs/resourcepaths"
	hcsschema "github.com/Microsoft/hcsshim/internal/hcs/schema2"
	hcs "github.com/Microsoft/hcsshim/internal/hcs/v2"
	"github.com/Microsoft/hcsshim/internal/log"
	"github.com/Microsoft/hcsshim/internal/protocol/guestrequest"
	"github.com/Microsoft/hcsshim/internal/vmservice"

	"github.com/Microsoft/go-winio/pkg/guid"
)

var errInvalidNetworkAdapter = errors.New("the ModifySettingRequest carries a network adapter the openvmm backend cannot translate")

var errDuplicateNetworkAdapter = errors.New("the openvmm backend already has a network adapter bound at this NIC id")

var errUnknownNetworkAdapter = errors.New("the openvmm backend has no network adapter bound at this NIC id")

var errNetworkBindMismatch = errors.New("the openvmm backend's network state does not match the bind it was asked to perform")

// Partial tuples track ports for rollback before vmservice has accepted the NIC.
type networkBinding struct {
	endpointID       string
	portID           guid.GUID
	switchID         guid.GUID
	macAddress       string
	vmserviceRemoved bool
}

// Malformed NIC IDs must also receive the network-specific error.
func isNetworkResourcePath(path string) bool {
	segments := strings.SplitN(path, "/", 4)
	return len(segments) >= 3 && segments[0] == "VirtualMachine" && segments[1] == "Devices" && segments[2] == "NetworkAdapters"
}

func parseNetworkResourcePath(path string) (guid.GUID, error) {
	segments := strings.Split(path, "/")
	if len(segments) != 4 || segments[0] != "VirtualMachine" || segments[1] != "Devices" || segments[2] != "NetworkAdapters" {
		return guid.GUID{}, fmt.Errorf("network resource path %q does not match the %q shape: %w", path, resourcepaths.NetworkResourceFormat, errInvalidNetworkAdapter)
	}
	nicID, err := guid.FromString(segments[3])
	if err != nil {
		return guid.GUID{}, fmt.Errorf("network resource path %q has an invalid NIC id %q: %w", path, segments[3], errInvalidNetworkAdapter)
	}
	return nicID, nil
}

func normalizeMAC(mac string) string {
	return strings.ToLower(strings.NewReplacer(":", "", "-", "").Replace(mac))
}

func parseEndpointGUID(value string) (guid.GUID, error) {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "{") || strings.HasSuffix(value, "}") {
		if len(value) < 2 || !strings.HasPrefix(value, "{") || !strings.HasSuffix(value, "}") {
			return guid.GUID{}, fmt.Errorf("endpoint GUID %q has unbalanced braces", value)
		}
		value = value[1 : len(value)-1]
	}
	return guid.FromString(value)
}

func (s *System) modifyNetwork(ctx context.Context, req *hcsschema.ModifySettingRequest) error {
	nicID, err := parseNetworkResourcePath(req.ResourcePath)
	if err != nil {
		return err
	}
	s.networkTxnMu.Lock()
	defer s.networkTxnMu.Unlock()

	switch req.RequestType {
	case guestrequest.RequestTypeAdd:
		return s.modifyNetworkAdd(ctx, req, nicID)
	case guestrequest.RequestTypeRemove:
		return s.modifyNetworkRemove(ctx, req, nicID)
	default:
		return fmt.Errorf("RequestType %q on network resource path %q is not supported: %w", req.RequestType, req.ResourcePath, ErrModifyNotSupported)
	}
}

// Failed rollback must retain the tuple for close-time retry, not leak an untracked port.
func (s *System) modifyNetworkAdd(ctx context.Context, req *hcsschema.ModifySettingRequest, nicID guid.GUID) error {
	settings, ok := req.Settings.(*hcsschema.NetworkAdapter)
	if !ok || settings == nil {
		return fmt.Errorf("network add at NIC %s carries Settings of type %T, want *hcsschema.NetworkAdapter: %w", nicID, req.Settings, errInvalidNetworkAdapter)
	}
	if settings.EndpointId == "" {
		return fmt.Errorf("network add at NIC %s has an empty EndpointId: %w", nicID, errInvalidNetworkAdapter)
	}
	if _, err := parseEndpointGUID(settings.EndpointId); err != nil {
		return fmt.Errorf("network add at NIC %s has an invalid EndpointId %q: %w", nicID, settings.EndpointId, errInvalidNetworkAdapter)
	}
	if settings.MacAddress == "" {
		return fmt.Errorf("network add at NIC %s has an empty MacAddress: %w", nicID, errInvalidNetworkAdapter)
	}

	// Close may have finished releasing ports while this add waited for the transaction lock.
	if !s.operationStillAllowed() {
		return fmt.Errorf("compute system %s closed before the network add at NIC %s could bind: %w", s.id, nicID, hcs.ErrAlreadyClosed)
	}

	key := nicID.String()
	if !s.reserveNetworkBinding(key) {
		return fmt.Errorf("NIC %s is already bound: %w", key, errDuplicateNetworkAdapter)
	}

	bound, bindErr := s.binder.Bind(ctx, settings.EndpointId, nicID)
	if bound.PortID != (guid.GUID{}) {
		// Track the port before validation or RPC can fail, so rollback remains possible.
		s.storeNetworkBinding(key, networkBinding{
			endpointID: settings.EndpointId,
			portID:     bound.PortID,
		})
	}
	var switchID guid.GUID
	if bindErr == nil {
		switchID, bindErr = validateBoundEndpoint(settings.EndpointId, settings.MacAddress, bound)
	}
	if bindErr != nil {
		if unbindErr := s.rollbackNetworkBind(ctx, settings.EndpointId, bound.PortID, nicID); unbindErr != nil {
			return fmt.Errorf("network add at NIC %s failed and its HNS rollback also failed, retaining the binding: %w", key, errors.Join(bindErr, unbindErr))
		}
		s.forgetNetworkBinding(key)
		return bindErr
	}

	modifyReq := &vmservice.ModifyResourceRequest{
		Type: vmservice.ModifyType_ADD,
		Resource: &vmservice.ModifyResourceRequest_NicConfig{
			NicConfig: &vmservice.NICConfig{
				NicId:      key,
				MacAddress: bound.MacAddress,
				Backend: &vmservice.NICConfig_Dio{
					Dio: &vmservice.DioBackend{
						SwitchId: switchID.String(),
						PortId:   bound.PortID.String(),
					},
				},
			},
		},
	}
	if _, err := s.client.ModifyResource(ctx, modifyReq); err != nil {
		wrapped := fmt.Errorf("failed to add NIC %s to compute system %s: %w", key, s.id, err)
		if unbindErr := s.rollbackNetworkBind(ctx, settings.EndpointId, bound.PortID, nicID); unbindErr != nil {
			return fmt.Errorf("network add at NIC %s: vmservice add failed and its HNS rollback also failed, retaining the binding: %w", key, errors.Join(wrapped, unbindErr))
		}
		s.forgetNetworkBinding(key)
		return wrapped
	}

	s.storeNetworkBinding(key, networkBinding{
		endpointID: settings.EndpointId,
		portID:     bound.PortID,
		switchID:   switchID,
		macAddress: bound.MacAddress,
	})
	log.G(ctx).
		WithField("endpoint_id", settings.EndpointId).
		WithField("nic_id", key).
		WithField("mac_address", bound.MacAddress).
		WithField("switch_id", switchID.String()).
		WithField("port_id", bound.PortID.String()).
		Info("OpenVMM DIO network adapter added")
	return nil
}

// Remove the VM NIC before unbinding its host port; failed cleanup must remain retryable.
func (s *System) modifyNetworkRemove(ctx context.Context, req *hcsschema.ModifySettingRequest, nicID guid.GUID) error {
	settings, ok := req.Settings.(*hcsschema.NetworkAdapter)
	if !ok || settings == nil {
		return fmt.Errorf("network remove at NIC %s carries Settings of type %T, want *hcsschema.NetworkAdapter: %w", nicID, req.Settings, errInvalidNetworkAdapter)
	}

	key := nicID.String()
	binding, exists := s.lookupNetworkBinding(key)
	if !exists {
		return fmt.Errorf("NIC %s is not bound: %w", key, errUnknownNetworkAdapter)
	}
	if settings.EndpointId == "" {
		return fmt.Errorf("network remove at NIC %s has an empty EndpointId: %w", key, errInvalidNetworkAdapter)
	}
	requestedEndpointID, err := parseEndpointGUID(settings.EndpointId)
	if err != nil {
		return fmt.Errorf("network remove at NIC %s has an invalid EndpointId %q: %w", key, settings.EndpointId, errInvalidNetworkAdapter)
	}
	boundEndpointID, err := parseEndpointGUID(binding.endpointID)
	if err != nil || requestedEndpointID != boundEndpointID {
		return fmt.Errorf("network remove at NIC %s targets endpoint %q, want the bound endpoint %q: %w", key, settings.EndpointId, binding.endpointID, errNetworkBindMismatch)
	}
	if settings.MacAddress == "" {
		return fmt.Errorf("network remove at NIC %s has an empty MacAddress: %w", key, errInvalidNetworkAdapter)
	}
	if normalizeMAC(settings.MacAddress) != normalizeMAC(binding.macAddress) {
		return fmt.Errorf("network remove at NIC %s targets MAC %q, want the bound MAC %q: %w", key, settings.MacAddress, binding.macAddress, errNetworkBindMismatch)
	}

	if !binding.vmserviceRemoved {
		modifyReq := &vmservice.ModifyResourceRequest{
			Type: vmservice.ModifyType_REMOVE,
			Resource: &vmservice.ModifyResourceRequest_NicConfig{
				NicConfig: &vmservice.NICConfig{
					NicId:      key,
					MacAddress: binding.macAddress,
					Backend: &vmservice.NICConfig_Dio{
						Dio: &vmservice.DioBackend{
							SwitchId: binding.switchID.String(),
							PortId:   binding.portID.String(),
						},
					},
				},
			},
		}
		if _, err := s.client.ModifyResource(ctx, modifyReq); err != nil {
			return fmt.Errorf("failed to remove NIC %s from compute system %s, retaining the binding: %w", key, s.id, err)
		}
		binding.vmserviceRemoved = true
		s.storeNetworkBinding(key, binding)
	}

	if err := s.binder.Unbind(ctx, binding.endpointID, binding.portID, nicID); err != nil {
		return fmt.Errorf("removed NIC %s from compute system %s but the HNS unbind of port %s failed, retaining the binding: %w", key, s.id, binding.portID, err)
	}

	log.G(ctx).
		WithField("endpoint_id", binding.endpointID).
		WithField("nic_id", key).
		WithField("mac_address", binding.macAddress).
		WithField("switch_id", binding.switchID.String()).
		WithField("port_id", binding.portID.String()).
		Info("OpenVMM DIO network adapter removed")
	s.forgetNetworkBinding(key)
	return nil
}

func validateBoundEndpoint(requestedEndpointID, requestedMAC string, bound BoundEndpoint) (guid.GUID, error) {
	requestedID, err := parseEndpointGUID(requestedEndpointID)
	if err != nil {
		return guid.GUID{}, fmt.Errorf("requested endpoint id %q is not a valid GUID: %w", requestedEndpointID, errNetworkBindMismatch)
	}
	boundID, err := parseEndpointGUID(bound.EndpointID)
	if err != nil || requestedID != boundID {
		return guid.GUID{}, fmt.Errorf("endpoint %s reported identity %q after bind: %w", requestedEndpointID, bound.EndpointID, errNetworkBindMismatch)
	}
	switchID, err := guid.FromString(bound.SwitchID)
	if err != nil {
		return guid.GUID{}, fmt.Errorf("endpoint %s reported switch id %q after bind, want a valid GUID: %w", requestedEndpointID, bound.SwitchID, errNetworkBindMismatch)
	}
	if bound.MacAddress == "" {
		return guid.GUID{}, fmt.Errorf("endpoint %s reported an empty MAC address after bind: %w", requestedEndpointID, errNetworkBindMismatch)
	}
	if normalizeMAC(bound.MacAddress) != normalizeMAC(requestedMAC) {
		return guid.GUID{}, fmt.Errorf("endpoint %s reported MAC %q after bind, want %q: %w", requestedEndpointID, bound.MacAddress, requestedMAC, errNetworkBindMismatch)
	}
	return switchID, nil
}

func (s *System) rollbackNetworkBind(ctx context.Context, endpointID string, portID guid.GUID, nicID guid.GUID) error {
	if portID == (guid.GUID{}) {
		return nil
	}
	return s.binder.Unbind(ctx, endpointID, portID, nicID)
}

func (s *System) reserveNetworkBinding(key string) bool {
	s.networkMu.Lock()
	defer s.networkMu.Unlock()
	if _, exists := s.networkBindings[key]; exists {
		return false
	}
	s.networkBindings[key] = networkBinding{}
	return true
}

func (s *System) storeNetworkBinding(key string, binding networkBinding) {
	s.networkMu.Lock()
	defer s.networkMu.Unlock()
	s.networkBindings[key] = binding
}

func (s *System) lookupNetworkBinding(key string) (networkBinding, bool) {
	s.networkMu.Lock()
	defer s.networkMu.Unlock()
	binding, exists := s.networkBindings[key]
	return binding, exists
}

func (s *System) forgetNetworkBinding(key string) {
	s.networkMu.Lock()
	defer s.networkMu.Unlock()
	delete(s.networkBindings, key)
}

func (s *System) releaseNetworkBindings(ctx context.Context) error {
	s.networkTxnMu.Lock()
	defer s.networkTxnMu.Unlock()

	s.networkMu.Lock()
	bindings := make(map[string]networkBinding, len(s.networkBindings))
	for key, binding := range s.networkBindings {
		bindings[key] = binding
	}
	s.networkMu.Unlock()

	var errs []error
	for key, binding := range bindings {
		if binding.endpointID == "" || binding.portID == (guid.GUID{}) {
			continue
		}
		nicID, err := guid.FromString(key)
		if err == nil {
			err = s.binder.Unbind(ctx, binding.endpointID, binding.portID, nicID)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("unbind NIC %s from endpoint %s: %w", key, binding.endpointID, err))
			continue
		}
		s.forgetNetworkBinding(key)
	}
	return errors.Join(errs...)
}
