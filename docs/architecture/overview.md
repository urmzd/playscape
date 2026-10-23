# Architecture Overview

playscape is the library for where an agent's turn runs. A program composes it in code to run tools on bounded pools of execution nodes, over one shared workspace, and to trust that every tool is confined to the workspace areas it was granted. It is not a service: nothing in it listens on a port or reads configuration. This document describes the beta architecture; interfaces are stabilizing but pre-1.0.

## Design Goals

1. **A library, chosen in code.** The unit a program deploys is a service spec, and the program decides every backend by constructing it. A process embedding playscape owns the lifecycle; playscape owns no listener and no configuration.
2. **Default-deny sandbox.** Tools are locked into declared areas of the workspace and declared spawn targets. Anything not granted is denied, so a misbehaving or compromised tool cannot leak state outside its area.
3. **One shared workspace.** Every node mounts the same storage backend. State lives in exactly one place regardless of how many agents run or where.
4. **Orthogonality.** Each package owns one concern and composes through small interfaces. The dependency graph is a strict DAG; changing a backend or substrate touches one package.
5. **No second durability layer.** Tasks run in process. Durable, retried runs belong to anchor, which the layers above wrap around playscape tasks.

## Package Graph

```text
task        (leaf: Task, Result)
workspace   (leaf: shared blob store; Local backend on os.Root)
metrics     (leaf: Recorder; Nop, Memory)
ngac        (leaf: NGAC policy machine: Graph, Spec, decisions)
   |
sandbox     -> ngac, workspace    (enforcement point: ScopePDP, Policy sugar)
tool        -> task, workspace    (Tool, Runtime, Registry, Provider)
tool/cli    -> tool               ("cli" provider: argv-templated binaries)
runner      -> tool, task         (Executor, Local, Shell tool adapter)
runner/k8s  -> runner, client-go  (one batch/v1 Job per run; its own module)
node        -> task, sandbox      (Node, Spec, Factory, Spawner)
node/inproc -> node, tool, ...    (in-process execution substrate)
   |
controlplane -> everything above  (composition root: pools, Deploy/Scale/Submit/Status)
cmd/playscape -> controlplane, runner, ... (the validation CLI: `playscape run`)
```

Arrows point at dependencies. Nothing under `pkg/` depends on `controlplane`, and the leaves depend on nothing but the standard library. The root module has no dependencies at all; `pkg/runner/k8s` is a separate module (`github.com/urmzd/playscape/pkg/runner/k8s`) so that only its importers inherit client-go.

## Execution: Bounded In-Process Pools

Every deployment owns a pool. `Replicas` nodes from the `node.Factory` each run one task at a time; tasks submitted while every node is busy wait in a FIFO backlog:

```text
submitters                     backlog              pool
----------                     -------              ----
program   --Submit/Async-->  [ task task task ]  --> node 1 (busy)
tools     --Spawn---------->   (oldest first)    --> node 2 (busy)
                                                 --> node 3 (free longest: takes the next task)
```

- **Sizing**: `Replicas` (zero means one) is how many of the deployment's tasks run at once. A negative value starts with no nodes, so tasks wait until `Scale` adds some.
- **Backlog**: bounded per deployment (`WithBacklog`, default 1024). A full backlog fails `SubmitAsync` with `ErrFull` rather than blocking the submitter.
- **Scaling**: growing starts nodes at once and hands them waiting tasks. Shrinking closes free nodes at once; a busy node finishes its task and then retires, so `Scale` never cancels work.
- **Goroutines**: one exists only while a node has work; an idle deployment costs nothing but its nodes.
- **Contexts**: tasks run under the control plane's own context, never the submitter's. Ending the context given to `Submit` stops the wait, not the task. `Close` refuses new work, fails waiting tasks with `ErrClosed`, cancels running ones, waits for them, and closes every node.
- **Results**: `Submit` blocks on a task's result; `SubmitAsync` returns its ID for `Result` to poll. Each deployment keeps finished results up to a bound (`WithResultRetention`, default 1024), and `Result` fails with `ErrUnknownTask` for an ID it never had or has forgotten, so a poller can tell a lost task from a running one.

Caveat: nothing is persisted. A task lives as long as the process; a crash loses waiting and running tasks and their results. That is deliberate. In the stack, fiber wraps each turn in an anchor run, and anchor's ledger and queue are what retry a lost attempt. playscape does not keep a second, weaker queue underneath.

## Access Control: NGAC Definition, Sandbox Enforcement

Access is *defined* in an NGAC policy machine (`pkg/ngac`, after NIST SP 800-178) and *enforced* by the sandbox (`pkg/sandbox`). The policy graph relates:

- **Users** (agent and tool names) grouped by **user attributes** (e.g. `agents`, `trusted`)
- **Objects** (workspace keys, spawn targets like `tool:auditor`) contained in **object attributes**; an attribute with a key prefix contains every workspace key under it, path-segment aware
- **Policy classes** as top-level containers: access is granted only if *every* policy class containing the object is satisfied by some association
- **Associations** granting operations (`read`, `write`, `delete`, `spawn`) from a user attribute over an object attribute
- **Prohibitions** as overriding denials (e.g. a group may write, this one member may not)

Deployments define access one of two ways in their `ServiceSpec`:

1. **Flat policies** (`policies`): per-tool workspace areas plus a spawn allowlist. `sandbox.FromPolicies` compiles them into an NGAC graph, so both paths share one decision engine. This covers most deployments.
2. **Full NGAC** (`access`): a declarative `ngac.Spec` (JSON-able) with attributes, associations, and prohibitions, for relationships flat policies cannot express.

Enforcement: nodes never hand tools the raw workspace. `sandbox.ScopePDP` wraps it in a decision-checking decorator (the enforcement point); every read/write/delete is decided under the executing tool's name, `List` filters out keys the user cannot read, traversal (`..`, absolute keys) is rejected before any decision, and spawn attempts check the `spawn` operation against `tool:<target>` objects. Unknown users, ungoverned objects, empty graphs: all deny. The sandbox and the policy machine are a security boundary; a bypass is a vulnerability (see SECURITY.md).

## Tool Providers

A `tool.Provider` builds a family of tools by one mechanism, labelled by a `tool.Kind`; `Registry.RegisterProvider` registers all of a provider's tools or none. The beta ships the `cli` kind (`pkg/tool/cli`): each tool wraps one binary behind a fixed argv template whose `{name}` placeholders are filled from the task's JSON input, one argument per element, never shell-expanded. Values starting with `-` are rejected unless the spec opts in, the child environment is an explicit allowlist, and every call has a timeout and per-stream output caps. A completed run returns `{exit_code, stdout, stderr}` whatever the exit code, so the agent can react to failures.

Caveat: the `cli` provider is not an isolation boundary. The binary runs on the host with the embedding process's privileges, and the workspace policy does not confine what it touches on disk. Grant `cli` tools only binaries you would run on that host yourself.

## Sandboxed Runs

`pkg/runner` is process isolation, distinct from `pkg/sandbox` (the NGAC enforcement point over the workspace). A `runner.Executor` runs one command to completion, streams its combined output, and returns its exit code; `runner.Shell` adapts an executor into a tool taking `{"script": ...}`. `runner.Local` runs host processes and is for development and tests only.

`pkg/runner/k8s` creates one batch/v1 Job per run: `backoffLimit: 0`, `restartPolicy: Never`, `activeDeadlineSeconds` from the run timeout, `ttlSecondsAfterFinished` for cleanup, no service-account token or service links, non-root UID with RuntimeDefault seccomp, no privilege escalation, all capabilities dropped, read-only root filesystem with size-limited `emptyDir` scratch at `/work` and `/tmp`, and requests equal to limits for CPU, memory, and ephemeral storage. Logs are followed from the pod; the exit code comes from the terminated container. Cancelled or failed-to-start runs delete their Job immediately. The embedding process needs `pkg/runner/k8s/manifests/rbac.yaml`; `manifests/networkpolicy.yaml` optionally denies all traffic to runs labelled `playscape/network: isolated`.

Caveats: runs see no shared workspace (scratch only), stdout and stderr arrive interleaved as one stream, and status is polled rather than watched.

## Self-Referential Agents

`tool.Runtime.Spawn` submits a sub-task back into the tool's own deployment and returns its task ID. The child waits for a node like any other task, so an agent calling another agent (or itself) is just another submitter. Recursion is bounded by policy: a tool can only spawn tools on its allowlist, and the default is none.

Caveat: a tool that blocks until its own sub-task finishes holds its node while it waits. If every node of a deployment is waiting on a child, no child can run. Size `Replicas` for the depth of waiting, or run the sub-task on the parent's node (fiber does: `Spec` returns the deployment's policy so a caller can scope a sub-task's runtime itself).

## Metrics

Components record through `metrics.Recorder` (counters, gauges, distributions) and stay ignorant of transport. The beta ships an in-memory recorder (`metrics.NewMemory`, read with `Snapshot`) and a no-op one:

| Series | Meaning |
|--------|---------|
| `playscape_nodes{deployment}` | nodes the deployment holds |
| `playscape_tasks_pending{deployment}` | tasks waiting for a free node |
| `playscape_tasks_submitted_total{deployment,tool}` | tasks accepted |
| `playscape_tasks_total{deployment,tool,status}` | tasks finished, `ok` or `error` |
| `playscape_task_seconds_{count,sum}{deployment,tool}` | execution latency |

A Prometheus or OpenTelemetry recorder replaces the in-memory one without any caller changing; in the stack, Humagen is to build the providers.

## Workspace Backends

`workspace.Workspace` is a minimal blob store (Read/Write/List/Delete over slash-separated keys). The beta ships `workspace.Local`, a directory on disk. Because the interface is the only thing the rest of the system sees, GCS/S3-style backends slot in without touching sandbox, nodes, or the control plane.

`Local` reaches its directory only through an `os.Root`. Key validation already rejects `..` and absolute keys; the root also refuses any path that a symbolic link would lead out of the directory, including a link swapped in while a call is in flight, which a check-then-open could not catch. Keys name regular files: `List` does not report links, `Read` and `Delete` of a directory or link report `ErrNotFound`, and `Write` (a temporary file renamed into place) replaces a link at its key instead of writing through it.

## The Validation CLI

`cmd/playscape` exists so a person can see what a command does under a policy. `playscape run [flags] COMMAND` builds what an embedding program would (a `workspace.Local`, a tool registry, an `inproc` factory, `controlplane.NewMemory`), deploys one service whose policy comes from `-area`/`-read` or an `ngac.Spec` file (`-policy`), and submits the command as one task. The tool stages every key the policy lets it read into a scratch directory, runs the command there with `runner.Local` (streaming its output, environment limited to `PATH` and `-env`), and applies every file the command created, changed or removed back through the policy-scoped workspace. Writes and deletes the policy refuses are reported, not applied; links and other non-regular files are skipped. The exit status separates the command's own failure from a denial (77), a timeout (124), an interrupt (130) and playscape failing to run it (125).

It opens no listener and exits with the command. It is not isolation: `runner.Local` runs a host process with the caller's privileges.

## Validated Paths

- `make test` race-tests both modules: sandbox confinement (traversal, list-leak), workspace confinement (symlink escapes through linked directories and files), NGAC semantics (attribute inheritance, multi-policy-class conjunction, prohibition override), the pools (bounded concurrency, FIFO backlog, `ErrFull`, drain on scale-down, retention, `Close`), policy-gated spawning, the Job runner against a fake clientset, and the CLI end to end.
- `examples/saige/` runs real [saige](https://github.com/urmzd/saige) agents as workloads, including an agent delegating to a sub-agent.
- fiber's sandbox and Humagen's platform build on `controlplane.NewMemory` and are built against each change.

## Roadmap

- GCS/S3 workspace backends behind `workspace.Workspace`
- An OpenTelemetry recorder for `metrics.Recorder`, built by Humagen
- Container-per-node execution substrate behind `node.Factory`
- Commands from saige run through `runner.Executor` (ecosystem step 15)
