//go:build windows && (lcow || wcow) && openvmm_prototype

package manager

import (
	"context"
	"errors"
	"os"
	"testing"

	"golang.org/x/sys/windows"
)

// TestRetryAfterAFailedClaimReleaseIsNotLockedOut verifies that a same-ID retry in the
// same shim can take the VM service socket claim back after a failed launch could not
// release it. The controller stays in StateNotCreated after a failed create, so
// containerd may retry, and a claim stranded by the first attempt must not turn that
// retry into a permanent errVMServiceSocketClaimed refusal.
func TestRetryAfterAFailedClaimReleaseIsNotLockedOut(t *testing.T) {
	socketPath, basePath := shortLauncherSocketPath(t), shortHybridBasePath(t)
	releaseFailure := errors.New("injected CloseHandle failure")

	var attempts int
	swapStartProcessForTest(t, func(string, []string) (ownedChild, error) {
		attempts++
		return nil, errors.New("injected spawn failure")
	})

	previous := acquireSocketClaim
	acquireSocketClaim = func(path string) (*hostSocketClaim, error) {
		claim, err := acquireHostSocketClaim(path)
		if err == nil && attempts == 0 {
			realClose := claim.close
			claim.close = func(h windows.Handle) error {
				// Fail the first release only. The handle stays open, as it does after
				// a real CloseHandle failure, and the retry's own release closes it.
				claim.close = realClose
				return releaseFailure
			}
		}
		return claim, err
	}
	t.Cleanup(func() { acquireSocketClaim = previous })

	first := newLauncher(&Config{
		OpenVMMBinaryPath: os.Args[0],
		VMServiceSocket:   socketPath,
		HybridVsockBase:   basePath,
	})
	if _, err := first.Launch(context.Background(), "sandbox"); err == nil {
		t.Fatal("first launch unexpectedly succeeded")
	}

	// The retry is a launch for the same VM through a launcher built the way the
	// direct create path builds one for every create.
	second := newLauncher(&Config{
		OpenVMMBinaryPath: os.Args[0],
		VMServiceSocket:   socketPath,
		HybridVsockBase:   basePath,
	})
	_, err := second.Launch(context.Background(), "sandbox")
	if errors.Is(err, errVMServiceSocketClaimed) {
		t.Fatalf("the retry was locked out by the first attempt's stranded claim: %v", err)
	}
}
