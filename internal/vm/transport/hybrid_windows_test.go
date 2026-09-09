//go:build windows && lcow

package transport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

type retryCloseListener struct {
	mu    sync.Mutex
	calls int
	fails int
}

func (l *retryCloseListener) Accept() (net.Conn, error) { return nil, net.ErrClosed }
func (l *retryCloseListener) Addr() net.Addr            { return &net.UnixAddr{Net: "unix"} }
func (l *retryCloseListener) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	if l.calls <= l.fails || l.fails == 0 && l.calls == 1 {
		return errors.New("close failed")
	}
	return nil
}

func shortHybridBase(t *testing.T) string {
	t.Helper()
	base := filepath.Join(os.TempDir(), fmt.Sprintf("ovmm-hybrid-%d", time.Now().UnixNano()))
	t.Cleanup(func() {
		matches, _ := filepath.Glob(base + "_*")
		for _, path := range matches {
			_ = os.Remove(path)
		}
	})
	return base
}

func TestHybridListenerPreventsPathReplacementUntilClose(t *testing.T) {
	factory, err := NewHybrid(shortHybridBase(t))
	if err != nil {
		t.Fatalf("NewHybrid: %v", err)
	}
	defer factory.Close()

	listener, err := factory.ListenPort(1024)
	if err != nil {
		t.Fatalf("ListenPort: %v", err)
	}
	path := factory.Paths()[0]
	if err := os.Remove(path); err == nil {
		t.Fatal("removed a hybrid socket while its listener still owned the pathname")
	}

	if err := listener.Close(); err != nil {
		t.Fatalf("listener Close: %v", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("hybrid socket survived listener Close: %v", err)
	}
}

func TestHybridListenerCloseRemovesEntryAndAllowsRearm(t *testing.T) {
	factory, err := NewHybrid(shortHybridBase(t))
	if err != nil {
		t.Fatalf("NewHybrid: %v", err)
	}
	defer factory.Close()

	first, err := factory.ListenPort(1025)
	if err != nil {
		t.Fatalf("first ListenPort: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if paths := factory.Paths(); len(paths) != 0 {
		t.Fatalf("Paths after listener Close = %v, want no completed entries", paths)
	}

	second, err := factory.ListenPort(1025)
	if err != nil {
		t.Fatalf("rearm ListenPort: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestHybridClaimPreservesOrdinaryFile(t *testing.T) {
	base := shortHybridBase(t)
	factoryValue, err := NewHybrid(base)
	if err != nil {
		t.Fatalf("NewHybrid: %v", err)
	}
	factory := factoryValue.(*hybridFactory)
	path := factory.portPath(1026)
	const contents = "ordinary file"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write ordinary file: %v", err)
	}
	factory.probeDial = func(context.Context, string, string) (net.Conn, error) {
		return nil, windows.WSAENOTSOCK
	}

	if _, err := factory.ListenPort(1026); !errors.Is(err, ErrProbeInconclusive) {
		t.Fatalf("ListenPort error = %v, want %v", err, ErrProbeInconclusive)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil || string(got) != contents {
		t.Fatalf("ordinary file after ListenPort: contents=%q error=%v", got, readErr)
	}
}

func TestHybridCloseWinningBeforeCommitCleansBoundListener(t *testing.T) {
	base := shortHybridBase(t)
	factoryValue, err := NewHybrid(base)
	if err != nil {
		t.Fatalf("NewHybrid: %v", err)
	}
	factory := factoryValue.(*hybridFactory)
	path := factory.portPath(1027)

	bound := make(chan struct{})
	resume := make(chan struct{})
	previousListen := hybridListen
	hybridListen = func(path string) (*net.UnixListener, error) {
		listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
		if err != nil {
			return nil, err
		}
		close(bound)
		<-resume
		return listener, nil
	}
	t.Cleanup(func() { hybridListen = previousListen })

	type result struct {
		listener net.Listener
		err      error
	}
	resultCh := make(chan result, 1)
	go func() {
		listener, err := factory.ListenPort(1027)
		resultCh <- result{listener: listener, err: err}
	}()
	<-bound
	closeResult := make(chan error, 1)
	go func() { closeResult <- factory.Close() }()
	select {
	case err := <-closeResult:
		t.Fatalf("Close returned before the in-flight bind settled: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(resume)

	resultValue := <-resultCh
	if resultValue.listener != nil || !errors.Is(resultValue.err, ErrClosed) {
		t.Fatalf("ListenPort after Close won = listener %v, error %v; want nil, %v", resultValue.listener, resultValue.err, ErrClosed)
	}
	if paths := factory.Paths(); len(paths) != 0 {
		t.Fatalf("Paths after rejected commit = %v, want empty", paths)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket path survived rejected commit: %v", err)
	}
	if err := <-closeResult; err != nil {
		t.Fatalf("Close after rejected commit cleanup: %v", err)
	}
}

func TestBookkeepingStaleCompletionDoesNotReleaseRearmedKey(t *testing.T) {
	book := bookkeeping{keys: make(map[string]struct{})}
	old := &entry{key: "port"}
	book.keys[old.key] = struct{}{}
	book.entries = append(book.entries, old)
	book.complete(old)

	if err := book.reserve(old.key); err != nil {
		t.Fatalf("rearm reserve: %v", err)
	}
	book.complete(old)
	if err := book.reserve(old.key); !errors.Is(err, ErrDuplicateBind) {
		t.Fatalf("second reserve after stale completion = %v, want %v", err, ErrDuplicateBind)
	}
}

func TestBookkeepingRejectedCommitRetainsFailedCleanupForCloseRetry(t *testing.T) {
	book := bookkeeping{keys: make(map[string]struct{})}
	if err := book.reserve("late"); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	book.mu.Lock()
	book.closed = true
	book.mu.Unlock()
	listener := &retryCloseListener{}
	if _, err := book.commit("late", "", listener, nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("late commit error = %v, want %v", err, ErrClosed)
	}
	if len(book.entries) != 1 {
		t.Fatalf("entries after rejected cleanup failure = %d, want 1", len(book.entries))
	}
	if err := book.close(); err != nil {
		t.Fatalf("Close retry: %v", err)
	}
	if listener.calls != 2 || len(book.entries) != 0 {
		t.Fatalf("after retry: close calls=%d entries=%d, want 2 and 0", listener.calls, len(book.entries))
	}
}

func TestBookkeepingCloseWaitsForLateRejectedCommitFailure(t *testing.T) {
	book := bookkeeping{keys: make(map[string]struct{})}
	if err := book.reserve("late"); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	closeResult := make(chan error, 1)
	go func() { closeResult <- book.close() }()
	select {
	case err := <-closeResult:
		t.Fatalf("Close returned before commit: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	listener := &retryCloseListener{fails: 2}
	if _, err := book.commit("late", "", listener, nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("late commit = %v, want %v", err, ErrClosed)
	}
	if err := <-closeResult; err == nil {
		t.Fatal("Close missed the retained late-commit cleanup failure")
	}
	if len(book.entries) != 1 {
		t.Fatalf("entries after failed Close = %d, want 1", len(book.entries))
	}
	if err := book.close(); err != nil {
		t.Fatalf("Close retry: %v", err)
	}
	if listener.calls != 3 || len(book.entries) != 0 {
		t.Fatalf("after retry: close calls=%d entries=%d, want 3 and 0", listener.calls, len(book.entries))
	}
}
