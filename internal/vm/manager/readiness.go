//go:build windows && (lcow || wcow) && openvmm_prototype

package manager

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/Microsoft/hcsshim/internal/safefile"
)

type readinessDial func(ctx context.Context, network, address string) (net.Conn, error)

// systemDial is the production dialer. Readiness is a raw AF_UNIX connect: constructing a
// gRPC client proves nothing, because that client is lazy and connects on its first RPC.
var systemDial readinessDial = (&net.Dialer{}).DialContext

// errChildExitedBeforeReady reports that the owned child died while readiness was still
// being polled. It is distinct from context.DeadlineExceeded so a caller can tell a crash
// from a timeout; both are launch failures and neither may skip the termination ladder.
var errChildExitedBeforeReady = errors.New("the openvmm child exited before the VM service became ready")

// waitVMServiceReady polls a real connect to the socket until it succeeds, ctx expires, or
// the owned child exits. Returning nil means a listener accepted a connection.
func waitVMServiceReady(ctx context.Context, dial readinessDial, socketPath string, exited <-chan struct{}, poll time.Duration) (*safefile.DeleteHandle, error) {
	if dial == nil {
		return nil, errors.New("cannot wait for VM service readiness: no dialer")
	}
	if socketPath == "" {
		return nil, errors.New("cannot wait for VM service readiness: no socket path")
	}
	if poll <= 0 {
		return nil, fmt.Errorf("cannot wait for VM service readiness at %s: the poll interval %v must be positive", socketPath, poll)
	}

	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	var lastDialErr error
	for {
		owner, err := safefile.OpenDeleteHandle(socketPath)
		if err == nil {
			mode, modeErr := owner.Mode()
			if modeErr != nil || mode&os.ModeSocket == 0 {
				_ = owner.Close()
				if modeErr != nil {
					lastDialErr = modeErr
				} else {
					lastDialErr = errVMServiceSocketNotASocket
				}
			} else {
				connection, dialErr := dial(ctx, "unix", socketPath)
				if dialErr == nil {
					_ = connection.Close()
					return owner, nil
				}
				_ = owner.Close()
				lastDialErr = dialErr
			}
		} else {
			lastDialErr = err
		}

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("the VM service at %s did not become ready: %w (last connect: %v)", socketPath, ctx.Err(), lastDialErr)
		case <-exited:
			return nil, fmt.Errorf("the VM service at %s did not become ready: %w (last connect: %v)", socketPath, errChildExitedBeforeReady, lastDialErr)
		case <-ticker.C:
		}
	}
}
