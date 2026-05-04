//go:build linux

package stdio

import (
	"bytes"
	"errors"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/Microsoft/hcsshim/internal/guest/transport"
)

// fakeConn is a controllable transport.Connection backed by an in-memory
// buffer pair, used to exercise ConnSlot without touching real sockets.
type fakeConn struct {
	mu          sync.Mutex
	rd          *bytes.Buffer
	wr          *bytes.Buffer
	closed      bool
	failNextRW  error
	closeReadCh chan struct{}
}

func newFakeConn() *fakeConn {
	return &fakeConn{
		rd:          new(bytes.Buffer),
		wr:          new(bytes.Buffer),
		closeReadCh: make(chan struct{}),
	}
}

func (c *fakeConn) feedRead(b []byte) {
	c.mu.Lock()
	c.rd.Write(b)
	c.mu.Unlock()
}

func (c *fakeConn) failNext(err error) {
	c.mu.Lock()
	c.failNextRW = err
	c.mu.Unlock()
}

func (c *fakeConn) writtenBytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.wr.Bytes()...)
}

func (c *fakeConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return 0, io.EOF
	}
	if c.failNextRW != nil {
		err := c.failNextRW
		c.failNextRW = nil
		c.mu.Unlock()
		return 0, err
	}
	if c.rd.Len() == 0 {
		c.mu.Unlock()
		// Block until close or another write feeds data; simulate a live socket.
		<-c.closeReadCh
		return 0, io.EOF
	}
	n, err := c.rd.Read(p)
	c.mu.Unlock()
	return n, err
}

func (c *fakeConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, io.ErrClosedPipe
	}
	if c.failNextRW != nil {
		err := c.failNextRW
		c.failNextRW = nil
		return 0, err
	}
	return c.wr.Write(p)
}

func (c *fakeConn) Close() error {
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		close(c.closeReadCh)
	}
	c.mu.Unlock()
	return nil
}

func (c *fakeConn) CloseRead() error              { return c.Close() }
func (c *fakeConn) CloseWrite() error             { return nil }
func (c *fakeConn) File() (*os.File, error)       { return nil, errors.New("no file") }

var _ transport.Connection = (*fakeConn)(nil)

func TestConnSlot_Write_PassThroughWhenConnected(t *testing.T) {
	c := newFakeConn()
	s := NewConnSlot(c)

	n, err := s.Write([]byte("hello"))
	if err != nil || n != 5 {
		t.Fatalf("Write got n=%d err=%v, want n=5 err=nil", n, err)
	}
	if got := string(c.writtenBytes()); got != "hello" {
		t.Fatalf("underlying conn got %q, want %q", got, "hello")
	}
}

func TestConnSlot_Write_BlocksWhileDisconnected_ResumesAfterSet(t *testing.T) {
	c1 := newFakeConn()
	s := NewConnSlot(c1)
	s.Disconnect()

	done := make(chan error, 1)
	go func() {
		_, err := s.Write([]byte("queued"))
		done <- err
	}()

	// Confirm the goroutine is parked, not racing through.
	select {
	case <-done:
		t.Fatal("Write returned before Set was called")
	case <-time.After(50 * time.Millisecond):
	}

	c2 := newFakeConn()
	s.Set(c2)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Write after Set returned err=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Write did not return after Set")
	}

	if got := string(c2.writtenBytes()); got != "queued" {
		t.Fatalf("c2 got %q, want %q", got, "queued")
	}
}

func TestConnSlot_Write_DropsConnOnError_RetriesRemainingOnNewConn(t *testing.T) {
	c1 := newFakeConn()
	c1.failNext(io.ErrShortWrite)
	s := NewConnSlot(c1)

	done := make(chan error, 1)
	go func() {
		_, err := s.Write([]byte("payload"))
		done <- err
	}()

	// First write fails -> slot drops c1 -> goroutine is now waiting on Set.
	select {
	case <-done:
		t.Fatal("Write returned before reconnect")
	case <-time.After(50 * time.Millisecond):
	}

	c2 := newFakeConn()
	s.Set(c2)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Write after recovery err=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Write did not complete after Set")
	}

	if got := string(c2.writtenBytes()); got != "payload" {
		t.Fatalf("c2 got %q, want full payload", got)
	}
}

func TestConnSlot_Read_BlocksWhileDisconnected_ResumesAfterSet(t *testing.T) {
	s := NewConnSlot(newFakeConn())
	s.Disconnect()

	done := make(chan struct {
		n   int
		err error
		buf []byte
	}, 1)
	go func() {
		buf := make([]byte, 16)
		n, err := s.Read(buf)
		done <- struct {
			n   int
			err error
			buf []byte
		}{n, err, buf[:n]}
	}()

	select {
	case <-done:
		t.Fatal("Read returned before Set")
	case <-time.After(50 * time.Millisecond):
	}

	c2 := newFakeConn()
	c2.feedRead([]byte("greetings"))
	s.Set(c2)

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("Read err=%v", r.err)
		}
		if string(r.buf) != "greetings" {
			t.Fatalf("Read got %q, want %q", string(r.buf), "greetings")
		}
	case <-time.After(time.Second):
		t.Fatal("Read did not return after Set")
	}
}

func TestConnSlot_Close_UnblocksWriteWithEOF(t *testing.T) {
	s := NewConnSlot(newFakeConn())
	s.Disconnect()

	done := make(chan error, 1)
	go func() {
		_, err := s.Write([]byte("never sent"))
		done <- err
	}()

	time.Sleep(20 * time.Millisecond)
	_ = s.Close()

	select {
	case err := <-done:
		if err != io.EOF {
			t.Fatalf("Write after Close got err=%v, want io.EOF", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Write did not return after Close")
	}
}

func TestConnSlot_Set_ClosesPreviousConnection(t *testing.T) {
	c1 := newFakeConn()
	c2 := newFakeConn()
	s := NewConnSlot(c1)

	s.Set(c2)

	if !c1.closed {
		t.Fatal("Set did not close previous connection")
	}
	if c2.closed {
		t.Fatal("Set must not close the new connection")
	}
}

// TestConnSlot_PipeRelayIntegration drives a real os.Pipe through a relay-like
// io.Copy into a slot, simulates a bridge disconnect, and verifies the producer
// blocks on pipe-full back pressure rather than losing data.
func TestConnSlot_PipeRelayIntegration(t *testing.T) {
	pipeR, pipeW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer pipeR.Close()
	defer pipeW.Close()

	c1 := newFakeConn()
	s := NewConnSlot(c1)

	// Relay goroutine: read from pipe, write to slot. This is what
	// PipeRelay.Start does for stdout/stderr.
	relayDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(s, pipeR)
		relayDone <- err
	}()

	// Producer writes a chunk; relay forwards to c1.
	if _, err := pipeW.Write([]byte("first")); err != nil {
		t.Fatalf("write first: %v", err)
	}
	// Wait briefly for relay to drain.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if string(c1.writtenBytes()) == "first" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := string(c1.writtenBytes()); got != "first" {
		t.Fatalf("c1 got %q, want %q", got, "first")
	}

	// Simulate bridge disconnect.
	s.Disconnect()

	// Producer keeps writing; pipe absorbs into kernel buffer, eventually
	// the relay parks in slot.acquire.
	if _, err := pipeW.Write([]byte("second")); err != nil {
		t.Fatalf("write second: %v", err)
	}

	// Give the relay a chance to read "second" from the pipe and park.
	time.Sleep(50 * time.Millisecond)
	if got := string(c1.writtenBytes()); got != "first" {
		t.Fatalf("c1 received bytes during disconnect: %q", got)
	}

	// Reconnect: replace with a fresh conn.
	c2 := newFakeConn()
	s.Set(c2)

	// Relay drains "second" to c2.
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if string(c2.writtenBytes()) == "second" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := string(c2.writtenBytes()); got != "second" {
		t.Fatalf("c2 got %q after reconnect, want %q", got, "second")
	}

	// Tear down.
	pipeW.Close()
	select {
	case <-relayDone:
	case <-time.After(time.Second):
		t.Fatal("relay did not exit after pipe close")
	}
	_ = s.Close()
}
