//go:build windows && lcow && !openvmm_prototype

package service

import "github.com/Microsoft/hcsshim/internal/controller/vm"

func newVMController() (vmController, error) {
	return vm.New(), nil
}
