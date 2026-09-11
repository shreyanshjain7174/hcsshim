//go:build windows && (lcow || wcow)

/*
Package transport gives the shim one guest-transport factory that serves both the static
guest ports the LCOW cold path binds (entropy, the Linux log channel, and the GCS service)
and the dynamic process-IO ports the GCS bridge allocates, on either backend.

Two shapes exist. On HCS the host side is an AF_HYPERV socket keyed by the VM GUID and a
service GUID, and no filesystem object is created. On OpenVMM the host side is an AF_UNIX
listener whose pathname is derived from a single configured base, exactly as OpenVMM's
support/hybrid_vsock/src/lib.rs derives it.
*/
package transport

import (
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/Microsoft/hcsshim/internal/safefile"

	"github.com/Microsoft/go-winio/pkg/guid"
)

// Factory produces host-side listeners for guest ports on one backend. One factory belongs
// to one VM, and [Factory.Close] is the single cleanup point for everything it created.
type Factory interface {
	// ListenService binds the host side of a guest service GUID, such as the GCS service
	// ID the guest dials.
	ListenService(serviceID guid.GUID) (net.Listener, error)
	// ListenPort binds the host side of a numeric guest vsock port: entropy is 1, the
	// Linux log channel is 109, and the GCS bridge allocates the dynamic IO ports.
	ListenPort(port uint32) (net.Listener, error)
	// Paths returns currently tracked filesystem socket paths in creation order.
	Paths() []string
	// Close retries failed cleanup and does not repeat successful cleanup.
	Close() error
}

var (
	// ErrDuplicateBind reports a second listen for a guest port or service ID this
	// factory has bound and whose listener is still open. Two live listeners for one
	// guest port is never the intent; a re-arm after the first was closed is legal.
	ErrDuplicateBind = errors.New("a listener is already bound for this guest port or service id")
	// ErrClosed reports a listen attempted after the factory was closed.
	ErrClosed = errors.New("the transport factory is closed")
)

// entry is one bind this factory owns. path is empty for hvsock.
type entry struct {
	key      string
	path     string
	listener net.Listener
	owner    *safefile.DeleteHandle

	mu             sync.Mutex
	listenerClosed bool
	pathRemoved    bool
}

// trackedListener releases the bind reservation when the consumer closes the listener.
// Two simultaneous listeners on one guest port stay impossible, while a legitimate re-arm
// stays possible: the live-migration source rollback rebinds the log and GCS listeners
// after the blackout consumed the first pair.
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

// bookkeeping is the shared reservation, ordering, and exactly-once removal state. Both
// factories embed it so duplicate detection, ordering, and idempotent close behave
// identically on either backend.
type bookkeeping struct {
	mu       sync.Mutex
	closed   bool
	keys     map[string]struct{}
	entries  []*entry
	inflight int
	settled  chan struct{}
}

// reserve claims a bind key before any backend work happens, so a duplicate never reaches
// the backend and a failed bind can be rolled back with release.
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

// release undoes a reservation whose bind failed.
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

// commit records a successful bind in creation order and returns the listener the caller
// sees, which releases the reservation when it is closed.
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

// paths reports the filesystem paths in creation order.
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

// close retries failed cleanup and removes entries after successful cleanup.
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
