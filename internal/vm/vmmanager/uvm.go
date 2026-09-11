//go:build windows && (lcow || wcow)

package vmmanager

import (
	"context"
	"fmt"

	hcsschema "github.com/Microsoft/hcsshim/internal/hcs/schema2"
	"github.com/Microsoft/hcsshim/internal/log"
	"github.com/Microsoft/hcsshim/internal/logfields"

	"github.com/Microsoft/go-winio/pkg/guid"
	"github.com/sirupsen/logrus"
)

// UtilityVM is an abstraction around a lightweight virtual machine.
// It houses core lifecycle methods such as Create, Start, and Stop and
// also several optional methods that can be used to determine what the virtual machine
// supports and to configure these resources.
type UtilityVM struct {
	id   string
	vmID guid.GUID
	cs   ComputeSystem
}

// Create creates a new utility VM with the given ID and compute system configuration.
//
// This method returns the concrete UtilityVM. Callers
// can use the manager interfaces (for example, LifetimeManager, NetworkManager)
// as needed.
//
// OpenVMM-created systems enter through [WrapCreatedSystem].
func Create(ctx context.Context, id string, config *hcsschema.ComputeSystem) (*UtilityVM, error) {
	cs, vmID, err := createHCSComputeSystem(ctx, id, config)
	if err != nil {
		return nil, err
	}
	return WrapCreatedSystem(ctx, id, cs, vmID)
}

// WrapCreatedSystem wraps an already-created compute system and assigns its identity.
func WrapCreatedSystem(ctx context.Context, id string, computeSystem ComputeSystem, runtimeID guid.GUID) (*UtilityVM, error) {
	if computeSystem == nil {
		return nil, fmt.Errorf("cannot wrap utility VM %q: nil compute system", id)
	}

	uvm := &UtilityVM{
		id:   id,
		vmID: runtimeID,
		cs:   computeSystem,
	}

	log.G(ctx).WithFields(logrus.Fields{
		logfields.UVMID: uvm.id,
		"runtime-id":    uvm.vmID.String(),
	}).Debug("created utility VM")

	return uvm, nil
}
