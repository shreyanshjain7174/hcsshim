---
applyTo:
  - "cmd/containerd-shim-runhcs-v1/**/*.go"
  - "cmd/containerd-shim-lcow-v2/**/*.go"
  - "internal/hcsoci/**/*.go"
  - "internal/uvm/**/*.go"
  - "internal/layers/**/*.go"
  - "internal/hcs/**/*.go"
  - "internal/gcs/**/*.go"
  - "internal/cow/**/*.go"
  - "internal/resources/**/*.go"
  - "internal/jobcontainers/**/*.go"
  - "internal/guest/**/*.go"
  - "internal/shim/**/*.go"
  - "internal/controller/**/*.go"
  - "internal/vm/**/*.go"
---

# V2 Shim — Systems Code Review Rules

This file applies ONLY to code in the containerd shim v2 path:
the shim binaries, HCS/OCI bridge, UVM lifecycle, resource management,
guest compute service, VM controller, and container/process abstractions.

## Resource Lifecycle (CRITICAL)

### ResourceCloser Registration
- Every allocated resource MUST be registered with `r.Add(...)` or `r.SetLayers(...)`.
- Flag any `ResourceCloser` returned from a helper that is NOT added to the resource tracker.
- Flag resources allocated inside a loop where early `return` could skip `r.Add(...)`.

### Deferred Cleanup on Error
- `CreateContainer` and similar orchestrators MUST have a `defer` that calls
  `resources.ReleaseResources(ctx, r, vm, true)` on error.
- Flag any orchestration function that allocates resources without an error-path defer cleanup.

### Close / Release Ordering
- `ResourceCloserList.Release()` releases in REVERSE order. Do not manually reorder.
- Flag any code that manually iterates a closer list forward instead of using `Release()`.

### HCS System Close
- Every `hcs.System` or `cow.Container` MUST be `Close()`d.
- Every `cow.Process` MUST be `Close()`d after `Wait()` completes.
- Flag missing `Close()` or `defer Close()` after creation of HCS objects.
- Flag `Process.Close()` called BEFORE `Wait()` returns.

### UVM Lifecycle
- `uvm.CreateLCOW` / `uvm.CreateWCOW` returns a UVM that MUST be `Close()`d on error.
- Flag any code path where a created UVM is not closed on failure.
- `uvm.Start()` must be called after creation; flag orphaned UVMs that are created but never started.

## Memory & Handle Leaks

- Flag Go routines launched without cancellation (missing `ctx` or `context.WithCancel`).
- Flag `syscall.Handle` or `windows.Handle` values not closed in error paths.
- Flag `os.File` or `io.Closer` values created but not deferred-closed.
- Flag SCSI, vSMB, vPMEM, or Plan9 mounts added to a UVM but not tracked for cleanup.

## Error Handling

- Errors from `Close()` or `Release()` in cleanup paths SHOULD be logged, not returned.
- Use `%w` for error wrapping with `fmt.Errorf`; flag bare `fmt.Errorf` with `%v` for errors.
- Flag swallowed errors (e.g., `_ = thing.Close()` without a log).
- Cleanup functions MUST continue releasing remaining resources even if one fails.

## Concurrency

- `hcs.System` operations are NOT goroutine-safe. Flag concurrent access without synchronization.
- UVM device maps (`scsiLocations`, `vsmb`, `plan9`, `vpmem`) are mutex-protected;
  flag direct map access without holding the lock.
- Flag goroutines that capture a `*cow.Process` or `*hcs.System` without ensuring the object outlives the goroutine.

## Package Layering

Flag these violations:
- `internal/cow` importing anything above it (must be pure abstraction).
- `internal/hcs` importing from `internal/hcsoci` or `internal/uvm` (backwards dependency).
- `internal/resources` importing from shim-level packages.
- `cmd/` packages directly importing `internal/hcs` instead of going through `internal/hcsoci`.
- `internal/controller/vm` importing from `internal/hcsoci` (controller is below orchestration layer).

## Naming & API

- Exported identifiers must have doc comments.
- Resource-owning types should document their Close/Release contract.
- Flag unexported helpers that return `ResourceCloser` without documentation on who owns cleanup.

## Tests

- If behavior changes, require tests — ask explicitly if none are present.
- Test helpers must call `t.Helper()`.
- Use `functional` build tag for tests needing a live VM or container.
- Use `maps.Clone()` for copying annotation maps in tests.

## Review Output

- Max 2 comments per concern; group related items.
- Use **[BLOCKER]** only for resource leaks, correctness, safety, or API-breaking issues.
- Use **[ISSUE]** for likely bugs or pattern deviations.
- Use **[SUGGESTION]** for non-blocking improvements.
