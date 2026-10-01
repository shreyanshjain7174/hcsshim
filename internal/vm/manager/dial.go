//go:build windows && (lcow || wcow) && openvmm_prototype

package manager

import (
	"context"
	"fmt"
	"io"
	"net"

	"github.com/Microsoft/hcsshim/internal/vmservice"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// AF_UNIX paths are not legal gRPC targets; pass the pathname through the dialer instead.
const vmServiceDialTarget = "passthrough:///openvmm-vmservice"

// The lazy client connects on its first RPC, so construction is not readiness proof.
func dialVMService(ctx context.Context, socketPath string) (vmservice.VMClient, io.Closer, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, fmt.Errorf("cannot build a VM service client for %s: %w", socketPath, err)
	}
	connection, err := grpc.NewClient(vmServiceDialTarget,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", socketPath)
		}))
	if err != nil {
		return nil, nil, fmt.Errorf("cannot build a VM service client for %s: %w", socketPath, err)
	}
	return vmservice.NewVMClient(connection), connection, nil
}
