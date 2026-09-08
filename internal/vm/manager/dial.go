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

// vmServiceDialTarget is a placeholder authority. The real address is an AF_UNIX pathname,
// which is not a legal gRPC target, so the pathname travels in the context dialer instead
// and the target only has to be stable and resolvable by the passthrough resolver.
const vmServiceDialTarget = "passthrough:///openvmm-vmservice"

// newVMServiceDialer returns the context dialer gRPC calls for every connection attempt.
// It captures the socket path and nothing else - in particular no context, so the dial is
// bounded by the per-dial context gRPC supplies and by nothing older.
func newVMServiceDialer(socketPath string) func(context.Context, string) (net.Conn, error) {
	return func(ctx context.Context, _ string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, "unix", socketPath)
	}
}

// dialVMService builds the production client. The client is lazy: it connects on its first
// RPC, which is why it is never treated as a readiness proof. The returned io.Closer is the
// same connection the client speaks over, so closing it releases exactly one object.
func dialVMService(ctx context.Context, socketPath string) (vmservice.VMClient, io.Closer, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, fmt.Errorf("cannot build a VM service client for %s: %w", socketPath, err)
	}
	connection, err := grpc.NewClient(vmServiceDialTarget,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(newVMServiceDialer(socketPath)))
	if err != nil {
		return nil, nil, fmt.Errorf("cannot build a VM service client for %s: %w", socketPath, err)
	}
	return vmservice.NewVMClient(connection), connection, nil
}
