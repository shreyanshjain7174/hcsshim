//go:build windows

package safefile

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

var ErrSocketPathNotASocket = errors.New("the configured VM service socket path names an ordinary file, not a socket")

type ClaimSocketPathOptions struct {
	ProbeDial              func(context.Context, string, string) (net.Conn, error)
	ProbeBudget            time.Duration
	InUseError             error
	InconclusiveError      error
	PathDescription        string
	PathNotFoundIsUnused   bool
	ProbeNotASocketIsError bool
}

func ClaimSocketPath(ctx context.Context, path string, options ClaimSocketPathOptions) error {
	owner, err := OpenDeleteHandle(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("cannot inspect the %s %s: %w", options.PathDescription, path, err)
	}
	mode, err := owner.Mode()
	if err != nil {
		_ = owner.Close()
		return fmt.Errorf("cannot inspect the %s %s: %w", options.PathDescription, path, err)
	}
	if mode&os.ModeSocket == 0 {
		_ = owner.Close()
		return fmt.Errorf("refusing to unlink the %s %s: %w", options.PathDescription, path, ErrSocketPathNotASocket)
	}

	probeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), options.ProbeBudget)
	defer cancel()

	connection, probeErr := options.ProbeDial(probeCtx, "unix", path)
	if probeErr == nil {
		_ = connection.Close()
		_ = owner.Close()
		return fmt.Errorf("refusing to unlink the %s %s because a live peer answered on it: %w", options.PathDescription, path, options.InUseError)
	}
	if options.ProbeNotASocketIsError && errors.Is(probeErr, windows.WSAENOTSOCK) {
		_ = owner.Close()
		return fmt.Errorf("refusing to unlink the %s %s: %w", options.PathDescription, path, ErrSocketPathNotASocket)
	}
	if !socketDefinitelyUnused(probeErr, options.PathNotFoundIsUnused) {
		_ = owner.Close()
		return fmt.Errorf("refusing to unlink the %s %s: %w: %v", options.PathDescription, path, options.InconclusiveError, probeErr)
	}
	if err := owner.Remove(); err != nil {
		return fmt.Errorf("cannot remove the dead %s %s: %w", options.PathDescription, path, errors.Join(err, owner.Close()))
	}
	return nil
}

func socketDefinitelyUnused(err error, pathNotFoundIsUnused bool) bool {
	return errors.Is(err, os.ErrNotExist) ||
		errors.Is(err, windows.ERROR_FILE_NOT_FOUND) ||
		pathNotFoundIsUnused && errors.Is(err, windows.ERROR_PATH_NOT_FOUND) ||
		errors.Is(err, windows.WSAECONNREFUSED)
}
