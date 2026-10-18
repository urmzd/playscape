# Architecture Overview

playscape is a control plane for agent execution nodes. It exists so that a user can deploy a single service, scale its agents out with proper metrics, and trust that every tool is confined to the workspace areas it was granted. This document describes the beta architecture; interfaces are stabilizing but pre-1.0.

## Design Goals

1. **One service, scaled execution.** The unit users deploy is a service spec. Execution capacity scales by adding consumers, never by redeploying the service.
2. **Default-deny sandbox.** Tools are locked into declared areas of the workspace and declared spawn targets. Anything not granted is denied, so a misbehaving or compromised tool cannot leak state outside its area.
3. **One shared workspace.** Every node mounts the same storage backend. State lives in exactly one place regardless of how many agents run or where.
4. **Orthogonality.** Each package owns one concern and composes through small interfaces. The dependency graph is a strict DAG; changing a backend, substrate, or transport touches one package.

## Package Graph

```text
task        (leaf: Task, Result)
workspace   (leaf: shared blob store; Local backend)
metrics     (leaf: Recorder; Nop, Memory)
ngac        (leaf: NGAC policy machine — Graph, Spec, decisions)
   |
sandbox     -> ngac, workspace    (enforcement point: ScopePDP, Policy sugar)
tool        -> task, workspace    (Tool, Runtime, Registry, Provider)
tool/cli    -> tool               ("cli" provider: argv-templated binaries)
queue       -> task               (Queue, Results; Memory, httpqueue)
node        -> task, sandbox      (Node, Spec, Factory, Spawner)
node/inproc -> node, tool, ...    (in-process execution substrate)
worker      -> queue, node, metrics (consumer loop)
   |
controlplane -> everything above  (composition root: Deploy/Scale/Submit/Status)
internal/server                   (HTTP translation layer over controlplane)
internal/cli                      (serve, work, version, update)
```

Arrows point at dependencies. Nothing depends downward on `controlplane`, and the leaves depend on nothing but the standard library.

## Producer/Consumer Execution

Every deployment owns a queue. Producers put tasks on it; consumers compete to take them off:

```text
producers                      queue                consumers
---------                      -----                ---------
HTTP API  --Submit/Async-->  [ task task task ]  <--Dequeue-- local goroutines (Scale)
tools     --Spawn---------->                     <--Lease---- playscape work pods (k8s/HPA)
```

- **Local scaling**: `Scale(n)` runs n worker goroutines inside the control plane process. Right default for a single binary.
- **Remote scaling**: `playscape work --server <url> --deployment <name>` runs the same consumer loop in its own process, leasing over HTTP (long poll) and reporting results back. Kubernetes Deployments, HPAs, or serverless containers replicate this process; the control plane never changes. Deploy with `"replicas": -1` for a fleet-only deployment.
- The two kinds of consumers share one queue, so they can coexist.

Results flow back through `queue.Results`: synchronous submitters block on `Await`, asynchronous submitters poll by task ID.

Beta caveats: the queue is in-memory and delivery is at-most-once (a consumer that dies mid-task drops it). Durable brokers (Redis, Pub/Sub, SQS) implement the same two interfaces when they land.

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

Caveat: the `cli` provider is not an isolation boundary. The binary runs on the worker host with the worker's privileges, and the workspace policy does not confine what it touches on disk. Grant `cli` tools only binaries you would run on the worker yourself.

## Self-Referential Agents

`tool.Runtime.Spawn` submits a sub-task back into the tool's own deployment and returns its task ID. The child task competes on the queue like any other work, so an agent calling another agent (or itself) is just another producer. Recursion is bounded by policy: a tool can only spawn tools on its allowlist, and the default is none.

## Metrics

Components record through `metrics.Recorder` (counters, gauges, distributions) and stay ignorant of transport. The beta ships an in-memory recorder exposed as text at `GET /metrics`:

| Series | Meaning |
|--------|---------|
| `playscape_nodes{deployment}` | current local node count |
| `playscape_tasks_submitted_total{deployment,tool}` | tasks produced |
| `playscape_tasks_total{deployment,tool,status}` | tasks executed locally |
| `playscape_tasks_total{deployment,source="remote",status}` | tasks reported by remote consumers |
| `playscape_task_seconds_{count,sum}{deployment,tool}` | execution latency |

A Prometheus or OpenTelemetry recorder replaces the in-memory one without any caller changing.

## Workspace Backends

`workspace.Workspace` is a minimal blob store (Read/Write/List/Delete over slash-separated keys). The beta ships `workspace.Local`, a directory on disk. Because the interface is the only thing the rest of the system sees, GCS/S3-style backends slot in without touching sandbox, nodes, or the control plane. Until a networked backend lands, remote workers on separate hosts each see their own local workspace; deploy them with a shared mount if tools must exchange artifacts.

## Validated Paths

- `go test ./...` covers sandbox confinement (including traversal and list-leak cases), NGAC semantics (attribute inheritance, multi-policy-class conjunction, prohibition override), queue-based scaling, policy-gated spawning, and a full remote-consumer round trip over HTTP.
- `examples/saige/` runs real [saige](https://github.com/urmzd/saige) agents as workloads, including an agent delegating to a sub-agent.
- `deploy/k8s/playscape.yaml` was validated on minikube: one server pod, a worker fleet scaled 2 to 5 with `kubectl scale`, all tasks executed remotely.

## Roadmap

- Durable queue backends (Redis, Pub/Sub, SQS) behind `queue.Queue`
- GCS/S3 workspace backends behind `workspace.Workspace`
- Prometheus exposition for `metrics.Recorder`
- Container-per-node execution substrate behind `node.Factory`
- Lease acknowledgment for at-least-once delivery
