//go:build windows && (lcow || wcow) && openvmm_prototype

package manager

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/Microsoft/hcsshim/internal/vm/vmmanager"
	"github.com/Microsoft/hcsshim/internal/vmservice"

	"github.com/Microsoft/go-winio/pkg/guid"
)

// BackendName is the diagnostic name used in unsupported-operation errors.
const BackendName = "openvmm"

// runtimeIDNamespace is the fixed namespace the shim-assigned runtime GUID is derived
// under. Derivation is deterministic so the same compute-system id always yields the same
// runtime id in logs and in tests.
var runtimeIDNamespace = guid.GUID{
	Data1: 0x6f0c9d3b,
	Data2: 0x8f1a,
	Data3: 0x4f2e,
	Data4: [8]byte{0x9c, 0x2d, 0x1b, 0x77, 0x5a, 0x30, 0xd4, 0xe1},
}

// errLauncherNotConfigured is returned when a backend is constructed without a process
// launcher in its [Deps].
var errLauncherNotConfigured = errors.New("no VM launcher is configured on the openvmm backend")

// errDialerNotConfigured is its dialer counterpart.
var errDialerNotConfigured = errors.New("no vmservice dialer is configured on the openvmm backend")

// The hybrid-vsock base this backend advertises
// through TransportProvider must be byte-identical to the path handed to vmservice.
var errTransportBaseMismatch = errors.New("the request hvsocket path differs from the transport base the openvmm backend advertises")

const createCleanupTimeout = 30 * time.Second

// VMLauncher starts and terminates the OpenVMM process.
type VMLauncher interface {
	// Launch spawns the OpenVMM child, waits for vmservice readiness, and returns the
	// AF_UNIX path to dial. It is the only legal source of a socket path here.
	Launch(ctx context.Context, id string) (socketPath string, err error)
	// Terminate runs the termination ladder. It is idempotent.
	Terminate(ctx context.Context) error
}

// DialFunc produces a vmservice client over the path Launch returned, together with the
// closer that releases the connection.
type DialFunc func(ctx context.Context, socketPath string) (vmservice.VMClient, io.Closer, error)

// Deps are the request-native backend's constructor inputs.
type Deps struct {
	// Launcher spawns the VM host process and reports the socket to dial.
	Launcher VMLauncher
	// Dial builds the vmservice client from that socket path.
	Dial DialFunc
	// TransportBase is the hybrid-vsock base, handed through verbatim.
	TransportBase string
}

type backend struct {
	deps Deps
}

func (b *backend) CreateFromRequest(ctx context.Context, id string, request *vmservice.CreateVMRequest) (vmmanager.ComputeSystem, guid.GUID, error) {
	if request == nil || request.Config == nil {
		return nil, guid.GUID{}, fmt.Errorf("cannot create compute system %s: no vmservice create request", id)
	}
	if request.Config.HvsocketConfig == nil || request.Config.HvsocketConfig.Path != b.deps.TransportBase {
		return nil, guid.GUID{}, fmt.Errorf("cannot create compute system %s: request hvsocket path does not match advertised transport base: %w", id, errTransportBaseMismatch)
	}
	if b.deps.Launcher == nil {
		return nil, guid.GUID{}, fmt.Errorf("cannot create compute system %s: %w", id, errLauncherNotConfigured)
	}
	if b.deps.Dial == nil {
		return nil, guid.GUID{}, fmt.Errorf("cannot create compute system %s: %w", id, errDialerNotConfigured)
	}

	runtimeID, err := guid.NewV5(runtimeIDNamespace, []byte(id))
	if err != nil {
		return nil, guid.GUID{}, fmt.Errorf("failed to derive a runtime id for compute system %s: %w", id, err)
	}

	socketPath, err := b.deps.Launcher.Launch(ctx, id)
	if err != nil {
		return nil, guid.GUID{}, fmt.Errorf("failed to launch the VM host for compute system %s: %w", id, errors.Join(err, b.terminateLauncher(ctx)))
	}

	client, conn, err := b.deps.Dial(ctx, socketPath)
	if err != nil {
		if conn != nil {
			_ = conn.Close()
		}
		return nil, guid.GUID{}, fmt.Errorf("failed to dial vmservice for compute system %s: %w", id, errors.Join(err, b.terminateLauncher(ctx)))
	}
	var serial *serialRelay
	if config := request.Config.GetSerialConfig(); config != nil && len(config.GetPorts()) != 0 && config.GetPorts()[0].GetConnect() {
		serial, err = newSerialRelay(ctx, config.GetPorts()[0].GetSocketPath(), id)
		if err != nil {
			connErr := conn.Close()
			return nil, guid.GUID{}, fmt.Errorf("failed to prepare COM1 serial listener for compute system %s: %w", id, errors.Join(err, connErr, b.terminateLauncher(ctx)))
		}
	}

	if _, err := client.CreateVM(ctx, request); err != nil {
		if serial != nil {
			_ = serial.Close()
		}
		_ = conn.Close()
		return nil, guid.GUID{}, fmt.Errorf("failed to create compute system %s: %w", id, errors.Join(err, b.terminateLauncher(ctx)))
	}

	return newSystem(id, runtimeID, b.deps.TransportBase, client, conn, b.deps.Launcher, serial, nil), runtimeID, nil
}

func (b *backend) terminateLauncher(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), createCleanupTimeout)
	defer cancel()
	return b.deps.Launcher.Terminate(ctx)
}
