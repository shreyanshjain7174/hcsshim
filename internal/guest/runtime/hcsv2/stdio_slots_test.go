//go:build linux

package hcsv2

import (
	"errors"
	"io"
	"os"
	"sync"
	"testing"

	"github.com/Microsoft/hcsshim/internal/guest/stdio"
	"github.com/Microsoft/hcsshim/internal/guest/transport"
)

// stubConn is a minimal transport.Connection used to exercise
// Host.RegisterStdioSlots / DisconnectAllStdio without real sockets.
type stubConn struct {
	mu     sync.Mutex
	closed bool
}

func (c *stubConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, io.EOF
	}
	return 0, nil
}
func (c *stubConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, io.ErrClosedPipe
	}
	return len(p), nil
}
func (c *stubConn) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return nil
}
func (c *stubConn) CloseRead() error        { return c.Close() }
func (c *stubConn) CloseWrite() error       { return nil }
func (c *stubConn) File() (*os.File, error) { return nil, errors.New("no file") }
func (c *stubConn) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

var _ transport.Connection = (*stubConn)(nil)

func TestHost_RegisterStdioSlots_TracksSlots(t *testing.T) {
	h := &Host{}
	c1, c2, c3 := &stubConn{}, &stubConn{}, &stubConn{}
	set := &stdio.ConnectionSet{
		In:  stdio.NewConnSlot(c1),
		Out: stdio.NewConnSlot(c2),
		Err: stdio.NewConnSlot(c3),
	}
	h.RegisterStdioSlots(set)

	if got, want := len(h.stdioSlots), 3; got != want {
		t.Fatalf("stdioSlots len = %d, want %d", got, want)
	}
}

func TestHost_RegisterStdioSlots_IgnoresNilAndNonSlot(t *testing.T) {
	h := &Host{}
	// Mixed set: only Out is a ConnSlot; In is nil; Err is a non-slot.
	set := &stdio.ConnectionSet{
		Out: stdio.NewConnSlot(&stubConn{}),
		Err: &stubConn{},
	}
	h.RegisterStdioSlots(set)

	if got, want := len(h.stdioSlots), 1; got != want {
		t.Fatalf("stdioSlots len = %d, want %d", got, want)
	}
}

func TestHost_DisconnectAllStdio_ClosesEveryUnderlyingConn(t *testing.T) {
	h := &Host{}
	conns := []*stubConn{{}, {}, {}}
	for _, c := range conns {
		h.RegisterStdioSlots(&stdio.ConnectionSet{Out: stdio.NewConnSlot(c)})
	}

	h.DisconnectAllStdio()

	for i, c := range conns {
		if !c.isClosed() {
			t.Fatalf("conns[%d] not closed by DisconnectAllStdio", i)
		}
	}
}
