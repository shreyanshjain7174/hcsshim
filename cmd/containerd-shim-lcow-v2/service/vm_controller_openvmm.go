//go:build windows && lcow && openvmm_prototype

package service

import (
	"github.com/Microsoft/hcsshim/internal/controller/vm"
	"github.com/Microsoft/hcsshim/internal/vm/manager"
)

func newVMController() (vmController, error) {
	create, err := manager.NewDirectCreate()
	if err != nil {
		return nil, err
	}
	return vm.NewWithDirectCreate(create)
}
