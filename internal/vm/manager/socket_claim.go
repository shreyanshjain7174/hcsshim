//go:build windows && (lcow || wcow) && openvmm_prototype

package manager

import (
	"errors"
	"fmt"
	"sync"

	"golang.org/x/sys/windows"
)

// Without a cross-process claim, shims can race between probing and rebinding the socket.
var errVMServiceSocketClaimed = errors.New("another process on this host already holds the VM service socket claim")

// Keep the claim beside the socket to share its access control, volume, and lifetime.
const socketClaimSuffix = ".shim-claim"

func socketClaimPath(socketPath string) string { return socketPath + socketClaimSuffix }

var acquireSocketClaim = acquireHostSocketClaim

// Claim files expose cross-process ownership on disk, unlike a process-local mutex.
// CREATE_NEW is atomic; DELETE_ON_CLOSE removes the name when the last handle closes, even on crash.
type hostSocketClaim struct {
	path string

	mu     sync.Mutex
	handle windows.Handle
	held   bool
	close  func(windows.Handle) error
}

// Failed releases outlive discarded launchers; retain the handle for same-ID retries.
var strandedClaims sync.Map

// Retry only this process's stranded claims, never another shim's claim.
func reclaimStrandedClaim(socketPath string) {
	if stranded, ok := strandedClaims.Load(socketClaimPath(socketPath)); ok {
		_ = stranded.(*hostSocketClaim).Release()
	}
}

func acquireHostSocketClaim(socketPath string) (*hostSocketClaim, error) {
	claimPath := socketClaimPath(socketPath)
	name, err := windows.UTF16PtrFromString(claimPath)
	if err != nil {
		return nil, fmt.Errorf("cannot claim the VM service socket %s: %w", socketPath, err)
	}
	// No sharing makes an existing claim evidence of a live holder.
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

// A no-sharing holder may report SHARING_VIOLATION instead of FILE_EXISTS.
func claimHeldElsewhere(err error) bool {
	return errors.Is(err, windows.ERROR_FILE_EXISTS) ||
		errors.Is(err, windows.ERROR_ALREADY_EXISTS) ||
		errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}

func (c *hostSocketClaim) Release() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.held {
		return nil
	}
	if err := c.close(c.handle); err != nil {
		strandedClaims.Store(c.path, c)
		return fmt.Errorf("cannot release the VM service socket claim %s: %w", c.path, err)
	}
	c.held = false
	strandedClaims.CompareAndDelete(c.path, c)
	return nil
}
