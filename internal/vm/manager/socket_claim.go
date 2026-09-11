//go:build windows && (lcow || wcow) && openvmm_prototype

package manager

import (
	"errors"
	"fmt"
	"sync"

	"golang.org/x/sys/windows"
)

// errVMServiceSocketClaimed reports that another process on this host already holds the
// claim on the configured VM service socket. The pathname comes from one host-wide
// configuration file, so without a host-level claim two shim processes would each probe
// it, each conclude it is stale, and each unlink and rebind it underneath the other.
var errVMServiceSocketClaimed = errors.New("another process on this host already holds the VM service socket claim")

// socketClaimSuffix names the claim file. It sits beside the socket rather than in a
// temporary directory so the claim shares the socket's configured location, and therefore
// its access control, its volume, and its lifetime.
const socketClaimSuffix = ".shim-claim"

func socketClaimPath(socketPath string) string { return socketPath + socketClaimSuffix }

var acquireSocketClaim = acquireHostSocketClaim

// hostSocketClaim is the exclusive, host-level right to probe, unlink, and rebind the one
// configured VM service socket. It is a file rather than a process-local mutex because the
// contenders are separate shim processes, and it is deliberately not a named mutex because
// a file makes the holder visible on disk while it is held.
//
// CREATE_NEW makes acquisition atomic across processes, and FILE_FLAG_DELETE_ON_CLOSE
// makes crash cleanup automatic: the kernel drops the name when the last handle goes,
// including when the holder dies without unwinding, so a crashed shim strands nothing.
type hostSocketClaim struct {
	path string

	mu     sync.Mutex
	handle windows.Handle
	held   bool
	close  func(windows.Handle) error
}

func acquireHostSocketClaim(socketPath string) (*hostSocketClaim, error) {
	claimPath := socketClaimPath(socketPath)
	name, err := windows.UTF16PtrFromString(claimPath)
	if err != nil {
		return nil, fmt.Errorf("cannot claim the VM service socket %s: %w", socketPath, err)
	}
	// Share nothing: the claim is the one object no other process may open while it is
	// held, which is what turns "the file exists" into "someone is alive and holding it".
	handle, err := windows.CreateFile(
		name,
		windows.GENERIC_WRITE,
		0,
		nil,
		windows.CREATE_NEW,
		windows.FILE_ATTRIBUTE_TEMPORARY|windows.FILE_FLAG_DELETE_ON_CLOSE,
		0,
	)
	if err != nil {
		if claimHeldElsewhere(err) {
			return nil, fmt.Errorf("refusing to take the VM service socket %s: %w", socketPath, errVMServiceSocketClaimed)
		}
		return nil, fmt.Errorf("cannot claim the VM service socket %s at %s: %w", socketPath, claimPath, err)
	}
	return &hostSocketClaim{path: claimPath, handle: handle, held: true, close: windows.CloseHandle}, nil
}

// claimHeldElsewhere is the whole "someone else has it" set. CREATE_NEW reports that the
// name exists; a holder that shares nothing makes the same attempt report the violation
// instead. Anything else is a real failure and is reported as one.
func claimHeldElsewhere(err error) bool {
	return errors.Is(err, windows.ERROR_FILE_EXISTS) ||
		errors.Is(err, windows.ERROR_ALREADY_EXISTS) ||
		errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}

// Release drops the claim. It is idempotent, so every failure path may call it without
// first working out whether an earlier one already did.
func (c *hostSocketClaim) Release() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.held {
		return nil
	}
	if err := c.close(c.handle); err != nil {
		return fmt.Errorf("cannot release the VM service socket claim %s: %w", c.path, err)
	}
	c.held = false
	return nil
}
