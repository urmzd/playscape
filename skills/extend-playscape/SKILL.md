---
name: extend-playscape
description: Extend the playscape library with new tools, workspace backends, run executors, or execution substrates while preserving the sandbox boundary and the package dependency DAG. Use when adding capabilities to playscape or reviewing changes to it.
---

# Extend playscape

playscape is a library composed in code from small interfaces: `workspace.Workspace` (shared storage), `sandbox.Policy` / `ngac.Spec` (default-deny confinement), `tool.Tool` (capability), `runner.Executor` (where a command runs), `node.Factory` (execution substrate), `metrics.Recorder` (observability), and `controlplane.ControlPlane` (the composition root, which runs each deployment's tasks on a bounded in-process pool). Extend by implementing an interface, never by widening one. Nothing here is a service: do not add a listener, a config file, or a durable queue (durability is anchor's).

## Add a tool

1. Implement `tool.Tool` or wrap a function with `tool.Func(name, fn)`.
2. Inside the tool, reach the workspace only through `rt.Workspace()` and spawn sub-tasks only through `rt.Spawn(ctx, task.Task{...})`. Both are pre-confined.
3. Register it: `registry.Register(myTool)`.
4. Grant capabilities in the `ServiceSpec`. Simple path: `sandbox.Policy{Tool: "name", Areas: [...], Spawn: [...]}` (compiled into an NGAC graph). Relational path: an `ngac.Spec` under `Access` with user attributes, associations, and prohibitions (see `examples/ngac/`). The two are mutually exclusive; no grant means no workspace access and no spawning.
5. Check it by hand with the CLI when it shells out: `go run ./cmd/playscape run -area out 'your command'` shows what the policy lets a command read and change.

## Add a workspace backend (GCS, S3)

1. Create `pkg/workspace/<backend>.go` implementing `Workspace` (Read/Write/List/Delete over slash-separated keys).
2. Validate every key with `workspace.ValidKey`; return `workspace.ErrNotFound` and `workspace.ErrInvalidKey` sentinels.
3. Mirror `local_test.go` for the new backend: round trip, list ordering, invalid keys, and anything the backend can be tricked into following (for `Local`, symbolic links; it confines itself with `os.Root`).

## Add a run executor

Implement `runner.Executor` (run one command to completion, stream combined output to the writer, return the exit code; `ErrTimeout`, `ErrStart` for runs that did not finish). `runner.Shell` turns it into a tool. A backend with heavy dependencies goes in its own module, as `pkg/runner/k8s` does, so the root module stays stdlib-only.

## Add an execution substrate (containers, VMs)

Implement `node.Factory` and `node.Node`. Honor `node.Spec`: scope every tool's workspace view with `sandbox.ScopePDP(ws, toolName, spec.PDP)` and gate `tool.Runtime.Spawn` with `spec.PDP.Can(user, sandbox.OpSpawn, sandbox.SpawnObject(target))` before delegating to `Spec.Spawn`. `Node.Run` must be safe for concurrent calls. The control plane needs no changes.

## Rules

- The sandbox, the policy machine and the workspace are a security boundary: any change near `pkg/sandbox`, `pkg/ngac` or `pkg/workspace` needs tests proving confinement (traversal, symlink escapes, sibling prefixes, list-leak, spawn gating, prohibition override).
- Preserve the DAG: nothing under `pkg/` imports `pkg/controlplane`; `task`, `workspace`, `metrics`, `ngac` stay stdlib-only.
- Verify with `make check` (both modules); run `go run ./examples/basic/` to see the composition end to end.
