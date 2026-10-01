//go:build windows && (lcow || wcow)

package transport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/Microsoft/hcsshim/internal/safefile"

	"github.com/Microsoft/go-winio/pkg/guid"
)

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
	// Inconclusive probes must never authorize unlinking.
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

// Preserve base bytes exactly so vmservice and this factory derive identical paths.
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
