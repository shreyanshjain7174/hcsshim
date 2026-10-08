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

// A lazy gRPC client is not readiness proof; use a raw AF_UNIX connect.
var systemDial readinessDial = (&net.Dialer{}).DialContext

// A crash must not be mistaken for a readiness timeout or skip cleanup.
var errChildExitedBeforeReady = errors.New("the openvmm child exited before the VM service became ready")

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
			if modeErr != nil {
				_ = owner.Close()
				lastDialErr = modeErr
			} else if mode&os.ModeSocket == 0 {
				_ = owner.Close()
				return nil, fmt.Errorf("the VM service at %s cannot become ready: %w", socketPath, errVMServiceSocketNotASocket)
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
