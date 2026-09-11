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
	// readinessBudget bounds the active readiness wait when the caller supplies no
	// deadline of its own. A caller deadline that is earlier still wins.
	readinessBudget = 90 * time.Second
	// ladderBudget bounds the whole termination ladder.
	ladderBudget = 30 * time.Second
	// gracefulExitBudget is rung 3's share: how long the owned child is given to leave on
	// its own after the vmservice Quit that rung 2 already issued.
	gracefulExitBudget = 5 * time.Second
	// socketProbeBudget bounds the connect that must fail before a pathname is unlinked.
	socketProbeBudget = 2 * time.Second
	// readinessPoll is the interval between readiness connect attempts.
	readinessPoll = 50 * time.Millisecond
)

var (
	// errVMServiceSocketInUse reports a live peer answering on the configured pathname.
	// Because that pathname is host-shared, unlinking it would break the other shim's VM,
	// so the launcher refuses and supports one concurrent OpenVMM sandbox per host.
	errVMServiceSocketInUse = errors.New("the configured VM service socket already has a live listener")
	// errOneVMPerShimProcess reports a second launch while a child is live.
	errOneVMPerShimProcess = errors.New("this shim process already owns a live OpenVMM child")
	// errLauncherAlreadyUsed reports a launch after this launcher's one child was cleaned
	// up. The Terminate contract carries no id, so a relaunched launcher would let a
	// late close for one sandbox kill another one's child.
	errLauncherAlreadyUsed = errors.New("this launcher has already run its one VM and cannot launch again")
	// errSocketProbeInconclusive reports a probe failure that does not prove the
	// configured pathname is stale. The launcher preserves the pathname.
	errSocketProbeInconclusive = errors.New("the configured VM service socket probe was inconclusive")
	// errVMServiceSocketNotASocket reports that the configured pathname names an ordinary
	// file. That answer proves the pathname is occupied by something this launcher never
	// created, so it is preserved: not-a-socket is evidence of a misconfiguration or of
	// another owner's data, never of a stale socket this launcher may unlink.
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

// ownedChild is the process handle this package started. Nothing here looks a process up
// by identifier or image.
type ownedChild interface {
	Exited() <-chan struct{}
	Wait(ctx context.Context) error
	Kill() error
	Close() error
}

// launchArgs is the whole command line contract, in one place. Two tokens: the flag and its
// value. The value grammar and explicit gRPC transport follow OpenVMM's --rpc option.
func launchArgs(socketPath string) []string {
	return []string{"--rpc", "path=" + socketPath + ",transport=grpc"}
}

// startProcess is the owned-handle spawn seam. Tests replace it to assert the executable
// and the exact argv without introducing a second configuration source.
var startProcess = func(exe string, args []string) (ownedChild, error) {
	// exec.Command, not exec.CommandContext: the caller's context bounds the launch, never
	// the child's lifetime. A second owner able to kill the child outside the ladder would
	// put the one thing cleanup describes beyond cleanup's control.
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

// Wait blocks until the owned child is gone or ctx expires.
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

// Kill signals the child through the handle this package started. There is no lookup by
// identifier or by image anywhere on this path.
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

// Close releases the owned handles. Closing the kill-on-close job is what guarantees that
// nothing the child started outlives this process.
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

// openvmmLauncher owns one child for the life of this shim process. It holds no client and
// issues no VM service RPC: rungs 1 and 2 of the composed termination ladder belong to
// System, and this type implements the process and socket cleanup rungs.
type openvmmLauncher struct {
	config    *Config
	probeDial readinessDial
	readyDial readinessDial
	poll      time.Duration

	mu         sync.Mutex
	child      ownedChild
	childID    string
	socketPath string
	launching  bool
	terminal   bool
	// claim is this shim process's exclusive host-level right to the configured pathname.
	// It is taken before the pathname is probed and released only once cleanup is verified.
	claim *hostSocketClaim
	// socketOwner is a delete-denying handle to the exact socket object the child bound.
	// Cleanup deletes through it, so no pathname lookup can redirect the delete.
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

// Launch starts the configured binary, waits actively for readiness, and returns the socket
// path it created, byte for byte as configured. The id is log identity and the one-child
// record; it is never path material, because ids are far wider than the AF_UNIX budget.
func (l *openvmmLauncher) Launch(ctx context.Context, id string) (string, error) {
	if l.config == nil {
		return "", fmt.Errorf("cannot launch %s: %w", id, errLauncherNotConfigured)
	}
	if err := l.reserve(id); err != nil {
		return "", err
	}

	socketPath := l.config.VMServiceSocket

	// The host claim comes before the probe, the unlink, and the spawn, and it is held
	// through cleanup. Probing first would let a second shim decide the pathname is stale
	// in the window between this shim's probe and its bind.
	claim, err := acquireSocketClaim(socketPath)
	if err != nil {
		l.release()
		return "", fmt.Errorf("cannot launch %s: %w", id, err)
	}

	if err := l.claimSocketPath(ctx, socketPath); err != nil {
		claimErr := l.abandonClaim(ctx, claim)
		l.release()
		return "", errors.Join(err, claimErr)
	}

	child, err := startProcess(l.config.OpenVMMBinaryPath, launchArgs(socketPath))
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

	// The caller's context is already spent by construction on a timeout, so cleanup runs
	// on a bounded context detached from it. Leaking the child is the one outcome that is
	// worse than a slow failure.
	cleanupCtx, cancelCleanup := context.WithTimeout(context.WithoutCancel(ctx), ladderBudget)
	defer cancelCleanup()
	if err := l.Terminate(cleanupCtx); err != nil {
		log.G(cleanupCtx).WithField("id", id).WithError(err).Warn("the OpenVMM termination ladder reported an error after a readiness failure")
	}
	// Entering cleanup is not evidence; the owned handle is.
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

// reserve admits exactly one launch. It refuses a relaunch after cleanup and a second
// concurrent launch, naming both ids so the refusal is diagnosable.
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

// release undoes a reservation that never produced a child, so a failed spawn leaves the
// launcher reusable and a retry reports the original cause rather than a terminal refusal.
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

// abandonClaim releases a claim taken for a launch that never produced an owned child, so
// the next attempt - in this process or another - is not locked out by a dead reservation.
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

// claimSocketPath decides whether the configured pathname may be taken. A live peer means
// no. An answer that does not prove the pathname stale means no. An ordinary file means no,
// and it is preserved. Only a definitively dead socket is removed, and the removal happens
// through a handle to that exact object rather than by pathname.
func (l *openvmmLauncher) claimSocketPath(ctx context.Context, socketPath string) error {
	owner, err := safefile.OpenDeleteHandle(socketPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("cannot inspect the VM service socket %s: %w", socketPath, err)
	}
	mode, err := owner.Mode()
	if err != nil {
		_ = owner.Close()
		return fmt.Errorf("cannot inspect the VM service socket %s: %w", socketPath, err)
	}
	if mode&os.ModeSocket == 0 {
		_ = owner.Close()
		return fmt.Errorf("refusing to unlink %s: %w", socketPath, errVMServiceSocketNotASocket)
	}

	probeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), socketProbeBudget)
	defer cancel()

	connection, probeErr := l.probeDial(probeCtx, "unix", socketPath)
	if probeErr == nil {
		_ = connection.Close()
		_ = owner.Close()
		return fmt.Errorf("refusing to unlink %s because a live peer answered on it: %w", socketPath, errVMServiceSocketInUse)
	}
	if errors.Is(probeErr, windows.WSAENOTSOCK) {
		_ = owner.Close()
		return fmt.Errorf("refusing to unlink %s: %w", socketPath, errVMServiceSocketNotASocket)
	}
	if !socketDefinitelyUnused(probeErr) {
		_ = owner.Close()
		return fmt.Errorf("refusing to unlink %s because its probe did not prove the pathname stale: %w: %v", socketPath, errSocketProbeInconclusive, probeErr)
	}
	if err := owner.Remove(); err != nil {
		return fmt.Errorf("cannot remove the dead socket pathname %s: %w", socketPath, errors.Join(err, owner.Close()))
	}
	return nil
}

// socketDefinitelyUnused is the whole permissive set. Timed-out, access-denied, unknown,
// and not-a-socket answers are deliberately absent: those pathnames are preserved.
func socketDefinitelyUnused(err error) bool {
	return errors.Is(err, os.ErrNotExist) ||
		errors.Is(err, windows.ERROR_FILE_NOT_FOUND) ||
		errors.Is(err, windows.WSAECONNREFUSED)
}

// Terminate runs the child-only rungs of the composed ladder:
//
//	3 bounded wait on the owned handle
//	4 kill through the owned handle, then prove the child is gone
//	5 close the owned handles
//	6 remove the socket this launcher owns, through its ownership handle
//	7 release the host claim on the configured pathname
//
// The first error is returned and later errors are logged. VM service teardown and quit belong to
// System, which is why nothing here holds a client. It is idempotent, and it detaches from
// the caller's cancellation immediately so cleanup still works for a cancelled caller.
func (l *openvmmLauncher) Terminate(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ladderBudget)
	defer cancel()

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

	// Rung 3. A budget expiry here is the expected transition to rung 4, not a failure:
	// rung 2 asked the VM host to quit and this is how long it is given to comply.
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

	// Rung 4. Nothing is signalled when the child has already gone.
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
	// Rung 5.
	closeErr := child.Close()
	if closeErr != nil {
		record(fmt.Errorf("termination rung 5, closing the owned handles: %w", closeErr))
	}

	// Rung 6. Only the socket object this launcher's child bound, deleted through the
	// handle captured at readiness so no replacement can be deleted in its place. When
	// readiness never got that far, the host claim this launcher still holds is what makes
	// a fresh capture of the pathname safe. The serial socket is owned by serialRelay.
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

	// Rung 7. The host claim is the last thing released, and only once the child is gone,
	// its handles are closed, and the socket is confirmed removed. Releasing it earlier
	// would let another shim take the pathname while this one still has work to do on it.
	if exited && closeErr == nil && socketRemoved {
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
