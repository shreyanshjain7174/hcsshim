//go:build windows && (lcow || wcow)

package transport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Microsoft/hcsshim/internal/safefile"

	"github.com/Microsoft/go-winio/pkg/guid"

	"golang.org/x/sys/windows"
)

const (
	// afUnixPathLimit is the AF_UNIX named-path limit, in UTF-8 bytes.
	afUnixPathLimit = 107
	// widestNumericSuffix is the widest suffix a numeric guest port can produce.
	widestNumericSuffix = "_4294967295"
	// hybridBaseLimit keeps every numeric derivation inside afUnixPathLimit. It is the
	// same budget internal/vm/manager validates the configured base against.
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
	// ErrProbeInconclusive reports a probe that did not prove the pathname stale. The
	// pathname is preserved: an inconclusive answer must never become "looks dead".
	ErrProbeInconclusive = errors.New("the hybrid vsock socket path probe did not prove the path stale")
)

// vsockTemplate is OpenVMM's embedding of an AF_VSOCK port into an AF_HYPERV service ID:
// 00000000-facb-11e6-bd58-64006a7986d3, the VSOCK_TEMPLATE of
// support/hybrid_vsock/src/lib.rs. It is the same template winio.VsockServiceID uses.
var vsockTemplate = guid.GUID{
	Data2: 0xfacb,
	Data3: 0x11e6,
	Data4: [8]byte{0xbd, 0x58, 0x64, 0x00, 0x6a, 0x79, 0x86, 0xd3},
}

// hybridFactory is the OpenVMM-backed factory. Every listener is an AF_UNIX socket under
// the one configured base, and the base is carried through byte for byte.
type hybridFactory struct {
	base string
	book bookkeeping
	// probeDial is the connect used to decide whether a pathname is stale.
	probeDial func(ctx context.Context, network, address string) (net.Conn, error)
}

var _ Factory = (*hybridFactory)(nil)

// NewHybrid returns the OpenVMM-backed factory. base is Config.HybridVsockBase, which is
// byte-identical to the HVSocketConfig.Path handed to vmservice. It is used unmodified: no
// Clean, no case folding, no separator fixups, because vmservice and this factory must
// derive the same pathnames from the same bytes.
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

// portPath derives the numeric form: base + "_" + the decimal port, which is what
// VsockPortOrId::host_uds_path builds for a vsock port.
func (f *hybridFactory) portPath(port uint32) string {
	return f.base + "_" + strconv.FormatUint(uint64(port), 10)
}

// servicePath derives the service form. A service ID matching the vsock template collapses
// through Data1 to the numeric form; any other GUID appends its lowercase GUID string.
func (f *hybridFactory) servicePath(serviceID guid.GUID) string {
	if port, ok := templatePort(serviceID); ok {
		return f.portPath(port)
	}
	return f.base + "_" + strings.ToLower(serviceID.String())
}

// templatePort reports the embedded vsock port of a template service ID.
func templatePort(serviceID guid.GUID) (uint32, bool) {
	stripped := serviceID
	stripped.Data1 = 0
	if stripped != vsockTemplate {
		return 0, false
	}
	return serviceID.Data1, true
}

// ListenService binds the path derived from serviceID.
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

// claimPath refuses to unlink a pathname a live peer still answers on, and refuses to
// unlink one whose probe was inconclusive. Only a definitive absent or refused answer
// permits the unlink. The probe is detached from the caller's
// cancellation, because a cancelled caller must not turn "in use" into "looks dead".
func (f *hybridFactory) claimPath(path string) error {
	owner, err := safefile.OpenDeleteHandle(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("cannot inspect the hybrid vsock path %s: %w", path, err)
	}
	mode, err := owner.Mode()
	if err != nil {
		_ = owner.Close()
		return fmt.Errorf("cannot inspect the hybrid vsock path %s: %w", path, err)
	}
	if mode&os.ModeSocket == 0 {
		_ = owner.Close()
		return fmt.Errorf("refusing to unlink %s: %w", path, ErrProbeInconclusive)
	}

	probeCtx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), socketProbeBudget)
	defer cancel()

	connection, probeErr := f.probeDial(probeCtx, "unix", path)
	if probeErr == nil {
		_ = connection.Close()
		_ = owner.Close()
		return fmt.Errorf("refusing to unlink %s because a live peer answered on it: %w", path, ErrPathInUse)
	}
	if !pathDefinitelyUnused(probeErr) {
		_ = owner.Close()
		return fmt.Errorf("refusing to unlink %s: %w: %v", path, ErrProbeInconclusive, probeErr)
	}
	if err := owner.Remove(); err != nil {
		return fmt.Errorf("cannot remove the dead socket path %s: %w", path, errors.Join(err, owner.Close()))
	}
	return nil
}

// pathDefinitelyUnused is the whole permissive set. Timed-out, access-denied, and unknown
// answers are deliberately absent: they are preserved, not unlinked.
func pathDefinitelyUnused(err error) bool {
	return errors.Is(err, os.ErrNotExist) ||
		errors.Is(err, windows.ERROR_FILE_NOT_FOUND) ||
		errors.Is(err, windows.ERROR_PATH_NOT_FOUND) ||
		errors.Is(err, windows.WSAECONNREFUSED)
}

func (f *hybridFactory) Paths() []string { return f.book.paths() }

func (f *hybridFactory) Close() error { return f.book.close() }
