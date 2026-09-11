//go:build windows && (lcow || wcow)

package transport

import (
	"fmt"
	"net"

	"github.com/Microsoft/go-winio"
	"github.com/Microsoft/go-winio/pkg/guid"
)

// hvsockFactory is the HCS-backed factory. AF_HYPERV creates no filesystem object, so
// Paths is empty and Close has nothing to unlink.
type hvsockFactory struct {
	vmID guid.GUID
	book bookkeeping
}

var _ Factory = (*hvsockFactory)(nil)

// NewHvsock returns the HCS-backed factory over the VM's runtime GUID.
func NewHvsock(vmID guid.GUID) Factory {
	return &hvsockFactory{vmID: vmID, book: bookkeeping{keys: make(map[string]struct{})}}
}

func (f *hvsockFactory) ListenService(serviceID guid.GUID) (net.Listener, error) {
	key := serviceID.String()
	if err := f.book.reserve(key); err != nil {
		return nil, err
	}

	l, err := winio.ListenHvsock(&winio.HvsockAddr{VMID: f.vmID, ServiceID: serviceID})
	if err != nil {
		f.book.release(key)
		return nil, fmt.Errorf("failed to listen on hvsock service %s for VM %s: %w", serviceID, f.vmID, err)
	}

	return f.book.commit(key, "", l, nil)
}

// ListenPort maps the numeric guest port through the AF_VSOCK service template, which is
// what the guest dials.
func (f *hvsockFactory) ListenPort(port uint32) (net.Listener, error) {
	return f.ListenService(winio.VsockServiceID(port))
}

func (f *hvsockFactory) Paths() []string { return f.book.paths() }

func (f *hvsockFactory) Close() error { return f.book.close() }
