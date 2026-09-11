//go:build windows && (lcow || wcow) && openvmm_prototype

package manager

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/Microsoft/hcsshim/internal/log"
	"github.com/Microsoft/hcsshim/internal/safefile"

	"github.com/sirupsen/logrus"
	"golang.org/x/sys/windows"
)

var (
	// errSerialPathInUse reports a live peer answering on the configured COM1 pathname.
	// The shim never takes that pathname: it belongs to whoever is already answering.
	errSerialPathInUse = errors.New("a live peer answered on the COM1 serial socket path")
	// errSerialProbeInconclusive reports a probe that did not prove the pathname stale.
	// The pathname is preserved: "I could not tell" must never become "looks dead".
	errSerialProbeInconclusive = errors.New("the COM1 serial socket path probe did not prove the path stale")
)

// serialProbeBudget bounds the connect that must fail before the COM1 pathname is
// unlinked. It matches the transport factory's rule so both owners behave identically.
var serialProbeBudget = 2 * time.Second

// serialProbeDial is the connect used to decide whether the pathname is stale.
var serialProbeDial = (&net.Dialer{}).DialContext

// serialOpenOwnedPath captures the exact COM1 socket behind a delete-denying handle. It is
// a variable so a test can fail ownership capture without racing the filesystem.
var serialOpenOwnedPath = safefile.OpenDeleteHandle

// serialRelay owns the host side of OpenVMM's COM1 stream: one AF_UNIX listener at the
// single configured pathname, one accepted connection, and one goroutine copying guest
// serial output into the shim log. It is created only for a document that asked for COM1,
// and Close is the single, exactly-once cleanup point for all three.
//
// This is deliberately not a guest port on the transport factory: COM1 is a host AF_UNIX
// stream the VM dials, not a hybrid-vsock port the guest addresses by number.
type serialRelay struct {
	listener *net.UnixListener
	path     string
	log      *logrus.Entry

	// owner holds the exact file this relay's listener created and denies delete sharing
	// until cleanup marks that handle for deletion.
	owner       *safefile.DeleteHandle
	removeOwner func(*safefile.DeleteHandle) error

	// done is closed when the accept/copy goroutine has returned, so Close can prove no
	// goroutine outlives the compute system rather than assume it.
	done chan struct{}

	mu     sync.Mutex
	conn   net.Conn
	closed bool

	// streamOnce shuts the listener and connection down and joins the relay goroutine
	// exactly once; the pathname removal it used to guard is tracked separately because
	// a transient unlink failure must stay retryable.
	streamOnce sync.Once
	streamErr  error

	pathMu       sync.Mutex
	pathReleased bool
}

// newSerialRelay claims the configured pathname and starts relaying. Because the create
// request sets SerialConfig.Connect, OpenVMM dials this listener during CreateVM, so
// the caller must bind before that call and roll back on every failure after it.
func newSerialRelay(ctx context.Context, path, id string) (*serialRelay, error) {
	if err := claimSerialPath(path); err != nil {
		return nil, err
	}

	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("failed to listen on the COM1 serial socket %s: %w", path, err)
	}
	// The pathname is this relay's to unlink, and only while it can still prove the file
	// is the one it bound. Closing the listener must never make that decision for it.
	listener.SetUnlinkOnClose(false)

	owner, err := serialOpenOwnedPath(path)
	if err != nil {
		// The pathname is left in place: a relay without an ownership handle has no
		// standing to unlink whatever may occupy the name later.
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

// claimSerialPath refuses to unlink a pathname a live peer still answers on, and refuses
// to unlink one whose probe was inconclusive. Only a definitive absent or refused answer
// permits the unlink. The probe is detached from the caller's
// cancellation, because a cancelled caller must not turn "in use" into "looks dead".
func claimSerialPath(path string) error {
	owner, err := safefile.OpenDeleteHandle(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("cannot inspect the COM1 serial socket %s: %w", path, err)
	}
	mode, err := owner.Mode()
	if err != nil {
		_ = owner.Close()
		return fmt.Errorf("cannot inspect the COM1 serial socket %s: %w", path, err)
	}
	if mode&os.ModeSocket == 0 {
		_ = owner.Close()
		return fmt.Errorf("refusing to unlink the COM1 serial socket %s: %w", path, errSerialProbeInconclusive)
	}

	probeCtx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), serialProbeBudget)
	defer cancel()

	connection, probeErr := serialProbeDial(probeCtx, "unix", path)
	if probeErr == nil {
		_ = connection.Close()
		_ = owner.Close()
		return fmt.Errorf("refusing to unlink the COM1 serial socket %s because a live peer answered on it: %w", path, errSerialPathInUse)
	}
	if !serialPathDefinitelyUnused(probeErr) {
		_ = owner.Close()
		return fmt.Errorf("refusing to unlink the COM1 serial socket %s: %w: %v", path, errSerialProbeInconclusive, probeErr)
	}
	if err := owner.Remove(); err != nil {
		return fmt.Errorf("cannot remove the dead COM1 serial socket %s: %w", path, errors.Join(err, owner.Close()))
	}
	return nil
}

// serialPathDefinitelyUnused is the whole permissive set. Timed-out, access-denied, and
// unknown answers are deliberately absent: they are preserved, not unlinked.
func serialPathDefinitelyUnused(err error) bool {
	return errors.Is(err, os.ErrNotExist) ||
		errors.Is(err, windows.ERROR_FILE_NOT_FOUND) ||
		errors.Is(err, windows.ERROR_PATH_NOT_FOUND) ||
		errors.Is(err, windows.WSAECONNREFUSED)
}

// relay accepts the single serial connection OpenVMM makes and copies it line by line into
// the shim log. It returns once the listener or the connection is closed, and always
// closes done so Close can join it.
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

// Close shuts the listener and any accepted connection down, joins the relay goroutine,
// and removes the pathname this relay created. The shutdown half runs exactly once; the
// removal half is retried by a later Close until it is settled, because a transient
// unlink failure would otherwise strand the pathname forever. A retry never unlinks a
// file this relay does not still own, so a pathname recreated after teardown is safe.
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

		// The goroutine is unblocked by the two closes above; joining it is what makes
		// "no accept/copy goroutine remains" a fact rather than an expectation.
		<-r.done
	})
}

// releasePath marks the exact owned socket for deletion through its retained handle. A
// failed disposition remains retryable, and no pathname lookup can redirect deletion to
// a replacement object.
func (r *serialRelay) releasePath() error {
	r.pathMu.Lock()
	defer r.pathMu.Unlock()
	if r.pathReleased {
		return nil
	}

	if r.owner == nil {
		return fmt.Errorf("cannot remove the COM1 serial socket %s: this relay never captured ownership", r.path)
	}
	remove := r.owner.Remove
	if r.removeOwner != nil {
		remove = func() error { return r.removeOwner(r.owner) }
	}
	if err := remove(); err != nil {
		return err
	}
	r.pathReleased = true
	return nil
}

// ignoreAlreadyClosed treats a listener or connection the relay goroutine already closed
// as successful cleanup: the guest hanging up first is the normal end of a serial stream.
func ignoreAlreadyClosed(err error) error {
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}
