//go:build windows && (lcow || wcow)

/* Package transport preserves HCS socket identities and OpenVMM's hybrid-vsock path protocol. */
package transport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Microsoft/hcsshim/internal/safefile"

	"github.com/Microsoft/go-winio"
	"github.com/Microsoft/go-winio/pkg/guid"
)

// Factory belongs to one VM and retains ownership until cleanup succeeds.
type Factory interface {
	// Service GUIDs identify guest-dialed services, including GCS.
	ListenService(serviceID guid.GUID) (net.Listener, error)
	// Guest ABI ports: entropy 1, Linux logs 109; GCS allocates process-IO ports.
	ListenPort(port uint32) (net.Listener, error)
	// Paths returns currently tracked filesystem socket paths in creation order.
	Paths() []string
	// Close retries failed cleanup and does not repeat successful cleanup.
	Close() error
}

var (
	// ErrDuplicateBind reports a second listen for a key whose earlier bind is not yet closed and unlinked.
	ErrDuplicateBind = errors.New("a listener is already bound for this guest port or service id")
	// ErrClosed reports a listen attempted after the factory was closed.
	ErrClosed = errors.New("the transport factory is closed")
)

// HCS hvsock entries have no filesystem path.
type entry struct {
	key      string
	path     string
	listener net.Listener
	owner    *safefile.DeleteHandle

	mu             sync.Mutex
	listenerClosed bool
	pathRemoved    bool
}

// Live migration source rollback rebinds log and GCS listeners after blackout.
type trackedListener struct {
	net.Listener
	entry *entry
	book  *bookkeeping
}

func (t *trackedListener) Close() error {
	err := t.entry.close()
	if t.entry.done() {
		t.book.complete(t.entry)
	}
	return err
}

func (e *entry) close() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	var errs []error
	if !e.listenerClosed {
		if err := e.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			errs = append(errs, err)
		} else {
			e.listenerClosed = true
		}
	}
	if !e.pathRemoved {
		if e.owner == nil {
			e.pathRemoved = true
		} else if err := e.owner.Remove(); err != nil {
			errs = append(errs, err)
		} else {
			e.pathRemoved = true
		}
	}
	return errors.Join(errs...)
}

func (e *entry) done() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.listenerClosed && e.pathRemoved
}

type bookkeeping struct {
	mu       sync.Mutex
	closed   bool
	keys     map[string]struct{}
	entries  []*entry
	inflight int
	settled  chan struct{}
}

// Reserve before backend work so concurrent duplicate binds cannot race.
func (b *bookkeeping) reserve(key string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return fmt.Errorf("cannot bind %s: %w", key, ErrClosed)
	}
	if _, ok := b.keys[key]; ok {
		return fmt.Errorf("cannot bind %s a second time: %w", key, ErrDuplicateBind)
	}
	if b.inflight == 0 {
		b.settled = make(chan struct{})
	}
	b.inflight++
	b.keys[key] = struct{}{}
	return nil
}

func (b *bookkeeping) release(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.keys, key)
	b.settleLocked()
}

func (b *bookkeeping) settleLocked() {
	b.inflight--
	if b.inflight == 0 {
		close(b.settled)
	}
}

func (b *bookkeeping) commit(key, path string, l net.Listener, owner *safefile.DeleteHandle) (net.Listener, error) {
	e := &entry{key: key, path: path, listener: l, owner: owner}
	b.mu.Lock()
	if b.closed {
		b.entries = append(b.entries, e)
		b.mu.Unlock()
		cleanupErr := e.close()
		if e.done() {
			b.complete(e)
		}
		b.mu.Lock()
		b.settleLocked()
		b.mu.Unlock()
		return nil, errors.Join(ErrClosed, cleanupErr)
	}
	b.entries = append(b.entries, e)
	b.settleLocked()
	b.mu.Unlock()

	return &trackedListener{Listener: l, entry: e, book: b}, nil
}

func (b *bookkeeping) complete(completed *entry) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := range b.entries {
		if b.entries[i] == completed {
			delete(b.keys, completed.key)
			b.entries = append(b.entries[:i], b.entries[i+1:]...)
			return
		}
	}
}

func (b *bookkeeping) paths() []string {
	b.mu.Lock()
	defer b.mu.Unlock()

	out := make([]string, 0, len(b.entries))
	for _, e := range b.entries {
		if e.path == "" {
			continue
		}
		out = append(out, e.path)
	}
	return out
}

func (b *bookkeeping) close() error {
	b.mu.Lock()
	b.closed = true
	settled := b.settled
	inflight := b.inflight
	b.mu.Unlock()
	if inflight != 0 {
		<-settled
	}

	b.mu.Lock()
	entries := append([]*entry(nil), b.entries...)
	b.mu.Unlock()

	var errs []error
	for _, e := range entries {
		if err := e.close(); err != nil {
			errs = append(errs, fmt.Errorf("failed to close the listener for %s: %w", e.key, err))
		}
		if e.done() {
			b.complete(e)
		}
	}
	return errors.Join(errs...)
}

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

const (
	// afUnixPathLimit is the AF_UNIX named-path limit, in UTF-8 bytes.
	afUnixPathLimit = 107
	// OpenVMM represents guest ports as decimal uint32 values.
	widestNumericSuffix = "_4294967295"
	// Keep this budget in sync with manager's config validation.
	hybridBaseLimit = afUnixPathLimit - len(widestNumericSuffix)
)

// socketProbeBudget bounds the connect that must fail before a pathname is unlinked.
var socketProbeBudget = 2 * time.Second

var hybridListen = func(path string) (*net.UnixListener, error) {
	return net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
}

var (
	// ErrEmptyBase reports a hybrid factory asked for with no base at all.
	ErrEmptyBase = errors.New("the hybrid vsock base path is empty")
	// ErrBaseTooLong reports a base that cannot carry the widest numeric suffix.
	ErrBaseTooLong = errors.New("the hybrid vsock base path is over the byte limit")
	// ErrPathTooLong reports a derived path over the AF_UNIX limit.
	ErrPathTooLong = errors.New("the derived hybrid vsock socket path is over the AF_UNIX byte limit")
	// ErrPathInUse reports a live peer answering on the pathname this factory wanted.
	ErrPathInUse = errors.New("a live peer answered on the hybrid vsock socket path")
	// ErrProbeInconclusive reports a probe that cannot authorize unlinking.
	ErrProbeInconclusive = errors.New("the hybrid vsock socket path probe did not prove the path stale")
)

// Must match OpenVMM's VSOCK_TEMPLATE in support/hybrid_vsock/src/lib.rs.
var vsockTemplate = guid.GUID{
	Data2: 0xfacb,
	Data3: 0x11e6,
	Data4: [8]byte{0xbd, 0x58, 0x64, 0x00, 0x6a, 0x79, 0x86, 0xd3},
}

type hybridFactory struct {
	base string
	book bookkeeping
	// Only an absent or refused answer proves staleness; other errors are inconclusive.
	probeDial func(ctx context.Context, network, address string) (net.Conn, error)
}

var _ Factory = (*hybridFactory)(nil)

// NewHybrid preserves base bytes exactly so vmservice and this factory derive identical paths.
func NewHybrid(base string) (Factory, error) {
	if base == "" {
		return nil, fmt.Errorf("cannot build a hybrid transport: %w", ErrEmptyBase)
	}
	if n := len(base); n > hybridBaseLimit {
		return nil, fmt.Errorf(
			"cannot build a hybrid transport: the base is %d UTF-8 bytes, over the limit of %d, which is the AF_UNIX limit of %d less the %d bytes of the widest %q suffix: %w",
			n, hybridBaseLimit, afUnixPathLimit, len(widestNumericSuffix), widestNumericSuffix, ErrBaseTooLong)
	}
	return &hybridFactory{
		base:      base,
		book:      bookkeeping{keys: make(map[string]struct{})},
		probeDial: (&net.Dialer{}).DialContext,
	}, nil
}

// Match VsockPortOrId::host_uds_path's numeric path format.
func (f *hybridFactory) portPath(port uint32) string {
	return f.base + "_" + strconv.FormatUint(uint64(port), 10)
}

// Template GUIDs alias numeric ports in OpenVMM's hybrid-vsock protocol.
func (f *hybridFactory) servicePath(serviceID guid.GUID) string {
	if port, ok := templatePort(serviceID); ok {
		return f.portPath(port)
	}
	return f.base + "_" + strings.ToLower(serviceID.String())
}

func templatePort(serviceID guid.GUID) (uint32, bool) {
	stripped := serviceID
	stripped.Data1 = 0
	if stripped != vsockTemplate {
		return 0, false
	}
	return serviceID.Data1, true
}

func (f *hybridFactory) ListenService(serviceID guid.GUID) (net.Listener, error) {
	return f.listenAt(f.servicePath(serviceID))
}

func (f *hybridFactory) ListenPort(port uint32) (net.Listener, error) {
	return f.listenAt(f.portPath(port))
}

func (f *hybridFactory) listenAt(path string) (net.Listener, error) {
	if n := len(path); n > afUnixPathLimit {
		return nil, fmt.Errorf("cannot bind %s: it is %d UTF-8 bytes, over the AF_UNIX limit of %d: %w", path, n, afUnixPathLimit, ErrPathTooLong)
	}
	if err := f.book.reserve(path); err != nil {
		return nil, err
	}
	if err := f.claimPath(path); err != nil {
		f.book.release(path)
		return nil, err
	}

	l, err := hybridListen(path)
	if err != nil {
		f.book.release(path)
		return nil, fmt.Errorf("failed to listen on the hybrid vsock path %s: %w", path, err)
	}
	l.SetUnlinkOnClose(false)
	owner, err := safefile.OpenDeleteHandle(path)
	if err != nil {
		_ = l.Close()
		f.book.release(path)
		return nil, fmt.Errorf("failed to capture ownership of the hybrid vsock path %s: %w", path, err)
	}

	return f.book.commit(path, path, l, owner)
}

// Caller cancellation must not turn an occupied socket into a stale-socket verdict.
func (f *hybridFactory) claimPath(path string) error {
	return safefile.ClaimSocketPath(context.Background(), path, safefile.ClaimSocketPathOptions{
		ProbeDial:            f.probeDial,
		ProbeBudget:          socketProbeBudget,
		InUseError:           ErrPathInUse,
		InconclusiveError:    ErrProbeInconclusive,
		PathDescription:      "hybrid vsock path",
		PathNotFoundIsUnused: true,
	})
}

func (f *hybridFactory) Paths() []string { return f.book.paths() }

func (f *hybridFactory) Close() error { return f.book.close() }
