//go:build windows && (lcow || wcow)

package vmmanager

import (
	"fmt"

	"github.com/Microsoft/hcsshim/internal/vm/transport"
)

// NewTransport creates the one host-side guest transport factory for this VM.
func (uvm *UtilityVM) NewTransport() (transport.Factory, error) {
	provider, ok := uvm.cs.(TransportProvider)
	if ok {
		base := provider.TransportBase()
		if base == "" {
			return nil, fmt.Errorf("OpenVMM transport provider returned an empty base: %w", transport.ErrEmptyBase)
		}
		return transport.NewHybrid(base)
	}
	return transport.NewHvsock(uvm.RuntimeID()), nil
}
