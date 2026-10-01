//go:build windows && (lcow || wcow)

/* Package transport preserves HCS socket identities and OpenVMM's hybrid-vsock path protocol. */
package transport

import (
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/Microsoft/hcsshim/internal/safefile"

	"github.com/Microsoft/go-winio/pkg/guid"
)

// A factory belongs to one VM and retains ownership until cleanup succeeds.
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
	// ErrDuplicateBind reports a second listen for a key whose listener is still open.
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
