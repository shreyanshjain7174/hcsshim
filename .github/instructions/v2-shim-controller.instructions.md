---
applyTo:
  - "internal/controller/vm/**/*.go"
  - "internal/vm/**/*.go"
  - "cmd/containerd-shim-lcow-v2/**/*.go"
---

# VM Controller & LCOW Shim V2 — Review Rules

These rules apply to the VM controller state machine, VM manager/guest manager
interfaces, and the new LCOW containerd shim v2 binary.

## State Machine Correctness

- Every state transition MUST be validated against the allowed transitions.
- Flag any direct state assignment that bypasses the transition validation function.
- Flag missing error handling on invalid state transitions.
- Verify that terminal states (e.g., `stopped`, `failed`) do not allow further transitions.

## Interface Contracts

- `vmmanager.LifetimeManager` implementations MUST handle idempotent `Stop()` / `Close()`.
- `guestmanager.Manager` implementations MUST tolerate calls after `Close()`.
- Flag implementations that panic or return untyped errors on double-close.
- Mock implementations (`internal/vm/.../mock/`) must match the real interface exactly.

## LCOW Shim V2 Service

- Every containerd RPC handler (Create, Start, Delete, Exec) MUST have proper
  context propagation and cancellation.
- Flag any handler that blocks indefinitely without respecting `ctx.Done()`.
- Flag missing cleanup in `Delete` — all resources from `Create` must be released.
- The shim MUST NOT leak UVM references across task boundaries.

## Resource Ownership

- Document clearly whether the caller or callee owns a returned resource.
- Flag ambiguous ownership: if a function creates a UVM/container and returns it,
  the error path MUST clean it up internally before returning.
- The VM controller MUST track all resources it creates and release them on shutdown.

## Concurrency in Controller

- The VM controller may serve multiple container tasks concurrently.
- Flag shared state access without mutex protection.
- Flag goroutines spawned without `context.Context` propagation.
