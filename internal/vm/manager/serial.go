//go:build windows && (lcow || wcow) && openvmm_prototype

package manager

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/Microsoft/hcsshim/internal/log"
	"github.com/Microsoft/hcsshim/internal/safefile"

	"github.com/sirupsen/logrus"
)

var (
	// A live listener may belong to another shim.
	errSerialPathInUse = errors.New("a live peer answered on the COM1 serial socket path")
	// An inconclusive probe is not permission to unlink.
	errSerialProbeInconclusive = errors.New("the COM1 serial socket path probe did not prove the path stale")
)

// Match the transport factory's stale-socket probe budget.
var serialProbeBudget = 2 * time.Second

var serialProbeDial = (&net.Dialer{}).DialContext

var serialOpenOwnedPath = safefile.OpenDeleteHandle

// COM1 is host AF_UNIX, not a numbered guest hybrid-vsock port.
type serialRelay struct {
	listener *net.UnixListener
	path     string
	log      *logrus.Entry

	// Denies delete sharing until cleanup marks the owned object for deletion.
	owner *safefile.DeleteHandle

	done chan struct{}

	mu     sync.Mutex
	conn   net.Conn
	closed bool

	// Stream shutdown is once-only; unlink failure must remain retryable.
	streamOnce sync.Once
	streamErr  error

	pathMu       sync.Mutex
	pathReleased bool
}

// SerialConfig.Connect makes OpenVMM dial during CreateVM, so bind before that RPC.
func newSerialRelay(ctx context.Context, path, id string) (*serialRelay, error) {
	if err := claimSerialPath(path); err != nil {
		return nil, err
	}

	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("failed to listen on the COM1 serial socket %s: %w", path, err)
	}
	// Listener close must not unlink a replacement socket at this pathname.
	listener.SetUnlinkOnClose(false)

	owner, err := serialOpenOwnedPath(path)
	if err != nil {
		// Without an ownership handle, unlink could delete another owner's replacement.
		_ = listener.Close()
		return nil, fmt.Errorf("failed to capture ownership of the COM1 serial socket %s: %w", path, err)
	}

	relay := &serialRelay{
		listener: listener,
		path:     path,
		owner:    owner,
		log:      log.G(ctx).WithField("compute-system", id),
		done:     make(chan struct{}),
	}
	go relay.relay()
	return relay, nil
}

// Caller cancellation must not turn an occupied socket into a stale-socket verdict.
func claimSerialPath(path string) error {
	return safefile.ClaimSocketPath(context.Background(), path, safefile.ClaimSocketPathOptions{
		ProbeDial:            serialProbeDial,
		ProbeBudget:          serialProbeBudget,
		InUseError:           errSerialPathInUse,
		InconclusiveError:    errSerialProbeInconclusive,
		PathDescription:      "COM1 serial socket",
		PathNotFoundIsUnused: true,
	})
}

func (r *serialRelay) relay() {
	defer close(r.done)

	connection, err := r.listener.Accept()
	if err != nil {
		return
	}

	// Close raced us to the accept: the connection is ours to close, not to read.
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		_ = connection.Close()
		return
	}
	r.conn = connection
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		if r.conn == connection {
			r.conn = nil
		}
		r.mu.Unlock()
		_ = connection.Close()
	}()

	scanner := bufio.NewScanner(connection)
	for scanner.Scan() {
		r.log.WithField("serial", scanner.Text()).Info("OpenVMM COM1")
	}
}

// Retry pathname removal without repeating shutdown or deleting a replacement file.
func (r *serialRelay) Close() error {
	r.shutdownStream()
	err := r.releasePath()
	if r.streamErr != nil {
		return r.streamErr
	}
	return err
}

func (r *serialRelay) shutdownStream() {
	r.streamOnce.Do(func() {
		r.mu.Lock()
		r.closed = true
		connection := r.conn
		r.conn = nil
		r.mu.Unlock()

		r.streamErr = ignoreAlreadyClosed(r.listener.Close())
		if connection != nil {
			if err := ignoreAlreadyClosed(connection.Close()); err != nil && r.streamErr == nil {
				r.streamErr = err
			}
		}

		// Join before deletion so the relay cannot outlive the compute system.
		<-r.done
	})
}

// Keep the ownership handle across failed deletion attempts; never resolve the path again.
func (r *serialRelay) releasePath() error {
	r.pathMu.Lock()
	defer r.pathMu.Unlock()
	if r.pathReleased {
		return nil
	}

	if r.owner == nil {
		return fmt.Errorf("cannot remove the COM1 serial socket %s: this relay never captured ownership", r.path)
	}
	if err := r.owner.Remove(); err != nil {
		return err
	}
	r.pathReleased = true
	return nil
}

// Guest hangup may close the connection before host cleanup.
func ignoreAlreadyClosed(err error) error {
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}
