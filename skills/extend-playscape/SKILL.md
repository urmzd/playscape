---
name: extend-playscape
description: Extend the playscape control plane with new tools, workspace backends, queue backends, or execution substrates while preserving the sandbox boundary and the package dependency DAG. Use when adding capabilities to playscape or reviewing changes to it.
---

# Extend playscape

playscape composes small interfaces: `workspace.Workspace` (shared storage), `sandbox.Policy` (default-deny confinement), `tool.Tool` (capability), `queue.Queue`/`queue.Results` (producer/consumer seam), `node.Factory` (execution substrate), `metrics.Recorder` (observability), `controlplane.ControlPlane` (composition root). Extend by implementing an interface, never by widening one.

## Add a tool

1. Implement `tool.Tool` or wrap a function with `tool.Func(name, fn)`.
2. Inside the tool, reach the workspace only through `rt.Workspace()` and spawn sub-tasks only through `rt.Spawn(ctx, task.Task{...})`. Both are pre-confined.
3. Register it: `registry.Register(myTool)`.
4. Grant capabilities in the `ServiceSpec`. Simple path: `sandbox.Policy{Tool: "name", Areas: [...], Spawn: [...]}` (compiled into an NGAC graph). Relational path: an `ngac.Spec` under `Access` with user attributes, associations, and prohibitions (see `examples/ngac/`). The two are mutually exclusive; no grant means no workspace access and no spawning.

## Add a workspace backend (GCS, S3)

1. Create `pkg/workspace/<backend>.go` implementing `Workspace` (Read/Write/List/Delete over slash-separated keys).
2. Validate every key with `workspace.ValidKey`; return `workspace.ErrNotFound` and `workspace.ErrInvalidKey` sentinels.
3. Mirror `local_test.go` for the new backend: round trip, list ordering, invalid keys.

## Add a queue backend (Redis, Pub/Sub, SQS)

1. Implement `queue.Queue` (Enqueue, blocking Dequeue) and `queue.Results` (Report, Await, Get).
2. `Dequeue` must honor context cancellation; see `httpqueue` for a remote reference.

## Add an execution substrate (containers, VMs)

Implement `node.Factory` and `node.Node`. Honor `node.Spec`: scope every tool's workspace view with `sandbox.ScopePDP(ws, toolName, spec.PDP)` and gate `tool.Runtime.Spawn` with `spec.PDP.Can(user, sandbox.OpSpawn, sandbox.SpawnObject(target))` before delegating to `Spec.Spawn`. `pkg/worker` and the control plane need no changes.

## Rules

- The sandbox and policy machine are a security boundary: any change near `pkg/sandbox` or `pkg/ngac` needs tests proving confinement (traversal, sibling prefixes, list-leak, spawn gating, prohibition override).
- Preserve the DAG: nothing under `pkg/` imports `pkg/controlplane`; `task`, `workspace`, `metrics`, `ngac` stay stdlib-only.
- Verify with `make check`; run `go run ./examples/basic/` to see the composition end to end.
