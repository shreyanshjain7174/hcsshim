//go:build windows && (lcow || wcow) && openvmm_prototype

package manager

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/Microsoft/hcsshim/internal/log"
	"github.com/Microsoft/hcsshim/internal/safefile"

	"golang.org/x/sys/windows"
)

var (
	// Earlier caller deadlines still win.
	readinessBudget = 90 * time.Second
	// ladderBudget bounds the whole termination ladder.
	ladderBudget = 30 * time.Second
	// System has already issued Quit before this grace period.
	gracefulExitBudget = 5 * time.Second
	// socketProbeBudget bounds the connect that must fail before a pathname is unlinked.
	socketProbeBudget = 2 * time.Second
	// readinessPoll is the interval between readiness connect attempts.
	readinessPoll = 50 * time.Millisecond
)

var (
	// Unlinking a live listener would break another shim's VM.
	errVMServiceSocketInUse = errors.New("the configured VM service socket already has a live listener")
	// errOneVMPerShimProcess reports a second launch while a child is live.
	errOneVMPerShimProcess = errors.New("this shim process already owns a live OpenVMM child")
	// Terminate has no id, so reuse could let a late close kill another sandbox's child.
	errLauncherAlreadyUsed = errors.New("this launcher has already run its one VM and cannot launch again")
	// An inconclusive probe is not permission to unlink.
	errSocketProbeInconclusive = errors.New("the configured VM service socket probe was inconclusive")
	// An ordinary file belongs to another owner, not to stale-socket cleanup.
	errVMServiceSocketNotASocket = errors.New("the configured VM service socket path names an ordinary file, not a socket")
)

const childDiagnosticLimit = 8 << 10

type boundedBuffer struct {
	mu    sync.Mutex
	limit int
	buf   bytes.Buffer
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	remaining := b.limit - b.buf.Len()
	if remaining > 0 {
		if remaining > len(p) {
			remaining = len(p)
		}
		_, _ = b.buf.Write(p[:remaining])
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// PID or image lookup could target a replacement process instead of the owned child.
type ownedChild interface {
	Exited() <-chan struct{}
	Wait(ctx context.Context) error
	Kill() error
	Close() error
}

var startProcess = func(exe string, args []string) (ownedChild, error) {
	// CommandContext would let caller cancellation kill the child outside the ladder.
	command := exec.Command(exe, args...)
	stderr := &boundedBuffer{limit: childDiagnosticLimit}
	command.Stderr = stderr
	command.WaitDelay = 5 * time.Second
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}

	job, jobErr := createKillOnCloseJob()
	if jobErr != nil {
		return nil, jobErr
	}
	if err := command.Start(); err != nil {
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("cannot start %q: %w", exe, err)
	}

	if assignErr := assignProcessToJob(job, command.Process); assignErr != nil {
		killErr := command.Process.Kill()
		waitErr := command.Wait()
		closeErr := windows.CloseHandle(job)
		return nil, fmt.Errorf("cannot assign the OpenVMM child to its kill-on-close job: %w", errors.Join(assignErr, killErr, waitErr, closeErr))
	}
	child := &processChild{command: command, exited: make(chan struct{}), job: job, hasJob: true, stderr: stderr}

	go func() {
		_ = command.Wait()
		close(child.exited)
	}()
	return child, nil
}

var (
	createKillOnCloseJob = newKillOnCloseJob
	assignProcessToJob   = assignToJob
)

type processChild struct {
	command *exec.Cmd
	// job and hasJob are fixed before the child is published.
	job     windows.Handle
	hasJob  bool
	stderr  *boundedBuffer
	exited  chan struct{}
	closeMu sync.Mutex
}

func (c *processChild) Exited() <-chan struct{} { return c.exited }

func (c *processChild) Wait(ctx context.Context) error {
	select {
	case <-c.exited:
		return nil
	default:
	}
	select {
	case <-c.exited:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("the owned OpenVMM child did not exit: %w", ctx.Err())
	}
}

func (c *processChild) Kill() error {
	select {
	case <-c.exited:
		return nil
	default:
	}
	if c.command.Process == nil {
		return errors.New("cannot terminate the owned OpenVMM child: it was never started")
	}
	if err := c.command.Process.Kill(); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		return fmt.Errorf("cannot terminate the owned OpenVMM child: %w", err)
	}
	return nil
}

// Closing the job also kills descendants that would otherwise outlive the shim.
func (c *processChild) Close() error {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	if !c.hasJob {
		return nil
	}
	if err := windows.CloseHandle(c.job); err != nil {
		return fmt.Errorf("cannot close the owned job object: %w", err)
	}
	c.hasJob = false
	return nil
}

func (c *processChild) Diagnostic() string {
	if c.stderr == nil {
		return ""
	}
	return c.stderr.String()
}

func newKillOnCloseJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, fmt.Errorf("cannot create the job object: %w", err)
	}
	var limits windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		_ = windows.CloseHandle(job)
		return 0, fmt.Errorf("cannot set the job kill-on-close limit: %w", err)
	}
	return job, nil
}

func assignToJob(job windows.Handle, process *os.Process) error {
	var assignErr error
	if err := process.WithHandle(func(handle uintptr) {
		assignErr = windows.AssignProcessToJobObject(job, windows.Handle(handle))
	}); err != nil {
		return fmt.Errorf("cannot obtain the owned child handle: %w", err)
	}
	if assignErr != nil {
		return fmt.Errorf("cannot assign the owned child to the job: %w", assignErr)
	}
	return nil
}

// System owns teardown and Quit; the launcher owns the later process and socket rungs.
type openvmmLauncher struct {
	config    *Config
	probeDial readinessDial
	readyDial readinessDial
	poll      time.Duration

	mu sync.Mutex
	// Serializes whole Terminate ladders; acquire before mu, never while holding mu.
	terminateMu sync.Mutex
	child       ownedChild
	childID     string
	socketPath  string
	launching   bool
	terminal    bool
	// Held from before the probe until cleanup is verified.
	claim *hostSocketClaim
	// Delete-denying handle prevents cleanup from deleting a replacement socket.
	socketOwner *safefile.DeleteHandle
}

var _ VMLauncher = (*openvmmLauncher)(nil)

func newLauncher(config *Config) *openvmmLauncher {
	return &openvmmLauncher{
		config:    config,
		probeDial: systemDial,
		readyDial: systemDial,
		poll:      readinessPoll,
	}
}

// IDs are log identity, not path material: they exceed the AF_UNIX path budget.
func (l *openvmmLauncher) Launch(ctx context.Context, id string) (string, error) {
	if l.config == nil {
		return "", fmt.Errorf("cannot launch %s: %w", id, errLauncherNotConfigured)
	}
	if err := l.reserve(id); err != nil {
		return "", err
	}

	socketPath := l.config.VMServiceSocket

	// Claim before probing so another shim cannot take the pathname before this one binds.
	reclaimStrandedClaim(socketPath)
	claim, err := acquireSocketClaim(socketPath)
	if err != nil {
		l.release()
		return "", fmt.Errorf("cannot launch %s: %w", id, err)
	}

	if err := l.claimSocketPath(ctx, socketPath, errVMServiceSocketNotASocket); err != nil {
		claimErr := l.abandonClaim(ctx, claim)
		l.release()
		return "", errors.Join(err, claimErr)
	}
	// OpenVMM binds the hybrid-vsock base itself and never unlinks it.
	if base := l.config.HybridVsockBase; base != "" {
		if err := l.claimSocketPath(ctx, base, errHybridBaseNotASocket); err != nil {
			claimErr := l.abandonClaim(ctx, claim)
			l.release()
			return "", errors.Join(err, claimErr)
		}
	}

	child, err := startProcess(l.config.OpenVMMBinaryPath, []string{"--rpc", "path=" + socketPath + ",transport=grpc"})
	if err != nil {
		claimErr := l.abandonClaim(ctx, claim)
		l.release()
		return "", fmt.Errorf("cannot launch %s: %w", id, errors.Join(err, claimErr))
	}
	l.adopt(child, socketPath, claim)

	readyCtx, cancelReady := context.WithTimeout(ctx, readinessBudget)
	defer cancelReady()
	socketOwner, readyErr := waitVMServiceReady(readyCtx, l.readyDial, socketPath, child.Exited(), l.poll)
	if readyErr == nil {
		l.mu.Lock()
		if l.child == child {
			l.socketOwner = socketOwner
			l.mu.Unlock()
			return socketPath, nil
		}
		l.mu.Unlock()
		_ = socketOwner.Close()
		readyErr = errors.New("the OpenVMM child was cleaned up while VM service readiness was being committed")
	}

	// Readiness may exhaust the caller's context; cleanup must still run.
	cleanupCtx, cancelCleanup := context.WithTimeout(context.WithoutCancel(ctx), ladderBudget)
	defer cancelCleanup()
	if err := l.Terminate(cleanupCtx); err != nil {
		log.G(cleanupCtx).WithField("id", id).WithError(err).Warn("the OpenVMM termination ladder reported an error after a readiness failure")
	}
	if err := child.Wait(cleanupCtx); err != nil {
		return "", withChildDiagnostic(fmt.Errorf("cannot launch %s: readiness failed and the owned child is still running: %w", id, errors.Join(readyErr, err)), child)
	}
	return "", withChildDiagnostic(fmt.Errorf("cannot launch %s: %w", id, readyErr), child)
}

func withChildDiagnostic(err error, child ownedChild) error {
	reporter, ok := child.(interface{ Diagnostic() string })
	if !ok {
		return err
	}
	diagnostic := strings.TrimSpace(reporter.Diagnostic())
	if diagnostic == "" {
		return err
	}
	return fmt.Errorf("%w (OpenVMM stderr: %s)", err, diagnostic)
}

func (l *openvmmLauncher) reserve(id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.terminal {
		return fmt.Errorf("cannot launch %s: %w", id, errLauncherAlreadyUsed)
	}
	if l.launching || l.child != nil {
		return fmt.Errorf("cannot launch %s while this shim process still owns the child launched for %s: %w", id, l.childID, errOneVMPerShimProcess)
	}
	l.launching = true
	l.childID = id
	return nil
}

func (l *openvmmLauncher) release() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.launching = false
	l.childID = ""
}

func (l *openvmmLauncher) adopt(child ownedChild, socketPath string, claim *hostSocketClaim) {
	l.mu.Lock()
	l.launching = false
	l.child = child
	l.socketPath = socketPath
	l.claim = claim
	l.mu.Unlock()
}

func (l *openvmmLauncher) abandonClaim(ctx context.Context, claim *hostSocketClaim) error {
	if claim == nil {
		return nil
	}
	if err := claim.Release(); err != nil {
		l.mu.Lock()
		l.claim = claim
		l.mu.Unlock()
		log.G(ctx).WithError(err).Warn("cannot release the VM service socket claim after a failed launch")
		return err
	}
	return nil
}

func (l *openvmmLauncher) claimSocketPath(ctx context.Context, socketPath string, notASocketError error) error {
	err := safefile.ClaimSocketPath(ctx, socketPath, safefile.ClaimSocketPathOptions{
		ProbeDial:              l.probeDial,
		ProbeBudget:            socketProbeBudget,
		InUseError:             errVMServiceSocketInUse,
		InconclusiveError:      errSocketProbeInconclusive,
		PathDescription:        "socket pathname",
		ProbeNotASocketIsError: true,
	})
	if errors.Is(err, safefile.ErrSocketPathNotASocket) {
		return fmt.Errorf("%w: %w", notASocketError, err)
	}
	return err
}

var errHybridBaseNotASocket = errors.New("the configured hybrid-vsock base names an ordinary file, not a socket")

func removeDeadSocket(path string) error {
	owner, err := safefile.OpenDeleteHandle(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("cannot inspect %s: %w", path, err)
	}
	mode, err := owner.Mode()
	if err != nil {
		return errors.Join(fmt.Errorf("cannot inspect %s: %w", path, err), owner.Close())
	}
	if mode&os.ModeSocket == 0 {
		return errors.Join(fmt.Errorf("refusing to unlink %s: %w: %w", path, errHybridBaseNotASocket, safefile.ErrSocketPathNotASocket), owner.Close())
	}
	if err := owner.Remove(); err != nil {
		return errors.Join(fmt.Errorf("cannot remove %s: %w", path, err), owner.Close())
	}
	return nil
}

// Terminate follows System's teardown and Quit; release the host claim only after cleanup.
func (l *openvmmLauncher) Terminate(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ladderBudget)
	defer cancel()

	l.terminateMu.Lock()
	defer l.terminateMu.Unlock()

	l.mu.Lock()
	child, socketPath := l.child, l.socketPath
	socketOwner, claim := l.socketOwner, l.claim
	if child != nil {
		l.terminal = true
	}
	l.mu.Unlock()

	if child == nil {
		if claim == nil {
			return nil
		}
		if err := claim.Release(); err != nil {
			return fmt.Errorf("releasing the retained VM service socket claim: %w", err)
		}
		l.mu.Lock()
		if l.claim == claim {
			l.claim = nil
		}
		l.mu.Unlock()
		return nil
	}

	var firstErr error
	record := func(err error) {
		if err == nil {
			return
		}
		if firstErr == nil {
			firstErr = err
			return
		}
		log.G(ctx).WithError(err).Warn("a later OpenVMM termination rung also failed")
	}

	// Quit gets a grace period before kill; expiry is escalation, not a cleanup failure.
	exited := false
	waitCtx, cancelWait := context.WithTimeout(ctx, gracefulExitBudget)
	waitErr := child.Wait(waitCtx)
	cancelWait()
	switch {
	case waitErr == nil:
		exited = true
	case errors.Is(waitErr, context.DeadlineExceeded):
		log.G(ctx).Debug("the OpenVMM child did not exit within its graceful budget; escalating to the owned kill")
	default:
		record(fmt.Errorf("termination rung 3, the bounded wait on the owned OpenVMM child: %w", waitErr))
	}

	if !exited {
		if err := child.Kill(); err != nil {
			record(fmt.Errorf("termination rung 4, the owned kill: %w", err))
		}
		if err := child.Wait(ctx); err != nil {
			record(fmt.Errorf("termination rung 4, the owned OpenVMM child is still running after its kill: %w", err))
		} else {
			exited = true
		}
	}
	closeErr := child.Close()
	if closeErr != nil {
		record(fmt.Errorf("termination rung 5, closing the owned handles: %w", closeErr))
	}

	// The host claim alone cannot prove ownership of an uncaptured socket object.
	socketRemoved := true
	if socketOwner != nil {
		if err := socketOwner.Remove(); err != nil {
			socketRemoved = false
			record(fmt.Errorf("termination rung 6, removing the owned socket %s: %w", socketPath, err))
		}
	} else if socketPath != "" {
		if _, err := os.Lstat(socketPath); err == nil {
			socketRemoved = false
			record(fmt.Errorf("termination rung 6, preserving socket %s because this launcher never captured its ownership handle", socketPath))
		} else if !errors.Is(err, os.ErrNotExist) {
			socketRemoved = false
			record(fmt.Errorf("termination rung 6, inspecting socket %s without an ownership handle: %w", socketPath, err))
		}
	}

	// Remove the base only after child exit proves it cannot still be listening.
	hybridRemoved := true
	if exited && l.config != nil && l.config.HybridVsockBase != "" {
		if err := removeDeadSocket(l.config.HybridVsockBase); err != nil {
			hybridRemoved = false
			record(fmt.Errorf("termination rung 6b, removing the hybrid-vsock base: %w", err))
		}
	}

	// Release the claim last so another shim cannot take paths this ladder still owns.
	if exited && closeErr == nil && socketRemoved && hybridRemoved {
		claimReleased := true
		if claim != nil {
			if err := claim.Release(); err != nil {
				claimReleased = false
				record(fmt.Errorf("termination rung 7, releasing the VM service socket claim: %w", err))
			}
		}
		if claimReleased {
			l.mu.Lock()
			if l.child == child {
				l.child, l.socketPath, l.socketOwner, l.claim = nil, "", nil, nil
			}
			l.mu.Unlock()
		}
	}
	return firstErr
}
