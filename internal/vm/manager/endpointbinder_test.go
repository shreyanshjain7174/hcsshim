//go:build windows && (lcow || wcow) && openvmm_prototype

package manager

import (
	"encoding/json"
	"testing"

	"github.com/Microsoft/hcsshim/hcn"
	"github.com/Microsoft/hcsshim/internal/hns"
)

func TestEndpointSwitchIDUsesNetworkSwitchGuid(t *testing.T) {
	const (
		networkID = "b6f70cfb-7570-4256-91d4-dc8e4d8c8666"
		switchID  = "c08cb7b8-9b3c-408e-8e30-5e16a3aeb444"
	)

	endpoint := &hcn.HostComputeEndpoint{HostComputeNetwork: networkID}
	var network hns.HNSNetwork
	if err := json.Unmarshal([]byte(`{"ID":"`+networkID+`","SwitchGuid":"`+switchID+`"}`), &network); err != nil {
		t.Fatal(err)
	}

	if got := endpointSwitchID(endpoint, &network); got != switchID {
		t.Fatalf("endpointSwitchID() = %q, want HCN SwitchGuid %q instead of network ID %q", got, switchID, networkID)
	}
}
