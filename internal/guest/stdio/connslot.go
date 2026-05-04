//go:build linux

package stdio

import (
	"io"
	"os"
	"sync"

	"github.com/Microsoft/hcsshim/internal/guest/transport"
)

// ConnSlot wraps a transport.Connection so the underlying connection can be
// replaced at runtime. Read and Write block while no connection is set,
// providing back pressure on the producer instead of failing.
//
// This is the live-migration primitive for container stdio: when the bridge
// disconnects, callers invoke Disconnect on every slot. Any relay io.Copy
// driving a slot parks inside Write. The producing process keeps writing
// into its kernel pipe (~64KiB buffer) until the buffer fills, then blocks
// on its next write syscall — naturally pausing the process without losing
// any bytes. The host then re-attaches stdio with a fresh connection by
// calling Set, which wakes the blocked relay so the stream resumes.
//
// File returns the current connection's file descriptor and is only safe to
// call before the slot is disconnected. Hot-swap is not supported for callers
// that capture the file descriptor (e.g., external processes wired directly
// to a vsock fd); those callers see today's behavior unchanged.
type ConnSlot struct {
	mu     sync.Mutex
	cond   *sync.Cond
	conn   transport.Connection
	closed bool
}

var _ transport.Connection = (*ConnSlot)(nil)

// NewConnSlot wraps an initial connection. The slot starts connected.
func NewConnSlot(conn transport.Connection) *ConnSlot {
	s := &ConnSlot{conn: conn}
	s.cond = sync.NewCond(&s.mu)
	return s
}

// Set installs a new connection, closing any previous one, and wakes
// goroutines blocked in Read or Write.
func (s *ConnSlot) Set(c transport.Connection) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != nil {
		_ = s.conn.Close()
	}
	s.conn = c
	s.cond.Broadcast()
}

// Disconnect closes the current connection but keeps the slot open. Subsequent
// Read and Write calls block until Set is called or the slot is closed.
func (s *ConnSlot) Disconnect() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != nil {
		_ = s.conn.Close()
		s.conn = nil
	}
}

// Close permanently closes the slot. Any blocked Read or Write returns io.EOF.
func (s *ConnSlot) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.conn != nil {
		_ = s.conn.Close()
		s.conn = nil
	}
	s.cond.Broadcast()
	return nil
}

// Read implements transport.Connection. Blocks until a connection is available
// or the slot is closed. On read error, drops the current connection so the
// next Read waits for a replacement.
func (s *ConnSlot) Read(p []byte) (int, error) {
	c, err := s.acquire()
	if err != nil {
		return 0, err
	}
	n, rerr := c.Read(p)
	if rerr != nil && rerr != io.EOF {
		s.dropIfCurrent(c)
	}
	return n, rerr
}

// Write implements transport.Connection. Loops until all bytes are written or
// the slot is closed; on connection failure, drops the conn and waits for a
// replacement before retrying the remaining bytes. This is the back-pressure
// path: while disconnected, the loop parks in acquire and the caller's
// upstream pipe fills, eventually blocking the producing process.
func (s *ConnSlot) Write(p []byte) (int, error) {
	written := 0
	for written < len(p) {
		c, err := s.acquire()
		if err != nil {
			return written, err
		}
		n, werr := c.Write(p[written:])
		written += n
		if werr != nil {
			s.dropIfCurrent(c)
		}
	}
	return written, nil
}

// CloseRead delegates to the underlying connection if one is set.
func (s *ConnSlot) CloseRead() error {
	s.mu.Lock()
	c := s.conn
	s.mu.Unlock()
	if c == nil {
		return nil
	}
	return c.CloseRead()
}

// CloseWrite delegates to the underlying connection if one is set.
func (s *ConnSlot) CloseWrite() error {
	s.mu.Lock()
	c := s.conn
	s.mu.Unlock()
	if c == nil {
		return nil
	}
	return c.CloseWrite()
}

// File returns the current connection's file descriptor. Returns an error
// if the slot is disconnected or closed.
func (s *ConnSlot) File() (*os.File, error) {
	c, err := s.acquire()
	if err != nil {
		return nil, err
	}
	return c.File()
}

func (s *ConnSlot) acquire() (transport.Connection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for s.conn == nil && !s.closed {
		s.cond.Wait()
	}
	if s.closed {
		return nil, io.EOF
	}
	return s.conn, nil
}

func (s *ConnSlot) dropIfCurrent(c transport.Connection) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == c {
		_ = s.conn.Close()
		s.conn = nil
	}
}
