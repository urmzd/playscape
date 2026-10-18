# Agent Execution Design

Status: draft (pre-implementation). This document defines the target abstractions
for agent execution: delegation, handoffs, execution environments, event
streaming, and the reduced API grammar. The current beta implements a subset
(see `docs/architecture/overview.md`); nothing here is built until it is.

## System Abstraction

playscape is **two primitives around one policy machine on one workspace**:

- a **task queue** — competitive: exactly one consumer wins each item
- a **frame stream** — broadcast: every observer sees every item

Each primitive has a door in and a door out, giving four flow verbs total:

```text
                task flow (competitive)      frame flow (broadcast)
  door in       dispatch      API             emit          worker side
  door out      claim         worker side     watch         API
```

The doors are **operations on the primitives, not endpoints**. The trust
boundary cuts the square diagonally: anyone may produce work and anyone may
observe it; only workers — the machinery in the middle — take work and write
frames. Only the public diagonal is API: `POST /v1/dispatch` and
`GET /v1/tasks/{id}/watch`. The worker diagonal (`claim`, `heartbeat`,
`release`, `emit`) is spoken against the `queue.Queue` and `event.Bus`
*interfaces*; whether any wire exists for it is a property of the backend,
not of the system (see "The Worker Wire" under API Grammar).

There is exactly one work object: the **task**. A claimed task is not a
second resource — it is the same task in the `assigned` state, on loan to
one worker, with `(ID, Attempt)` as the claim ticket. Work enters through
`dispatch`, a worker `claim`s it, executes it inside a sandboxed node, and
streams back through `emit`; observers read through `watch`. The vocabulary
is the dispatcher's own: calls come in (`dispatch`), units claim them and
check in (`heartbeat`), and everyone else listens to the radio (`watch`).
Everything else in the system is one of:

- a **composition** of the four doors (`Spawn`, `Await`, `Handoff`, sync submit)
- a **projection** of the frame stream (`GET /v1/tasks/{id}`, the task ledger)
- **configuration** (`/v1/deployments`: specs, policies, environments, scale)

### Resource Hierarchy

```text
control plane          manages the fleet: every deployment it hosts
└── deployment         unit of creation: one spec = policy graph + environments
    └── environment    what runs the agents: queue + substrate + tools + capacity
        └── task       the work itself; while assigned, on loan to one worker
```

- The **fleet** is the set of deployments one control plane manages;
  `/v1/deployments` is the fleet view. (Distinct from a worker pool — workers
  belong to an environment, below.)
- A **deployment** is created as a single unit: its access policy and its
  environments. It owns no execution capacity of its own — it is the policy
  and routing boundary.
- An **environment** is what runs the agents. It owns a queue, an execution
  substrate (`node.Factory`), a tool binding, and capacity (local replicas
  and/or remote workers attached to it). Capacity, autoscaling signals, and
  `scale` live **here**, not on the deployment.
- A **task** is the leaf, with one lifecycle:

  ```text
  queued → assigned(worker, deadline, attempt) → ok | error | continued
     ↑            release / expiry (Attempt++)      (terminal)
     └──────────────────┘
  ```

  While `assigned`, the task is on loan to exactly one worker — exclusive
  and time-bounded (see Primitive 1). There is no separate claim, lease, or
  assignment resource; `assigned` is a state of the task.

### Invariants

1. **Default deny.** Unchanged from the sandbox/NGAC design: no grant, no access.
2. **One door in.** All work enters via `dispatch` — user submissions, spawns,
   and handoff continuations alike. One place for admission control,
   idempotency, and budgets.
3. **Observation never gates execution.** `Emit` never blocks or fails a task;
   a slow or absent watcher drops frames (bounded buffer, drop-oldest,
   `playscape_events_dropped_total`).
4. **Terminal retention.** Exactly one frame per task is durable: the terminal
   one. Intermediate frames are best-effort. This single rule makes `watch`
   total and `Await` correct; durable brokers extend retention backward
   without changing the contract.
5. **Watch is total.** Watching any task in any state is safe, follows
   `ContinuedBy` chains transparently, and always ends with a terminal frame.
6. **Competitive and broadcast never merge.** A task must be claimed by
   exactly one worker; a frame must reach every watcher. Two primitives is
   the floor.
7. **The DAG holds.** `event` is a leaf; adapters live outside the root module;
   nothing under `pkg/` imports `controlplane`.
8. **The API is the public diagonal only.** `dispatch`/`watch` (plus the
   `tasks` projections and `deployments` configuration) are the entire
   contract. The worker verbs are interface operations, never API: when a
   backend forces a wire for them (the in-memory queue), that wire is
   unversioned, worker-credentialed, and carries no compatibility promise
   (server and worker ship in one binary). Workers are inside the trust
   boundary — they enforce the sandbox locally; tools are not. Custom
   execution substrates implement `node.Factory` in Go, not a wire protocol.
9. **Every claimed attempt settles exactly once.** claim → heartbeat\* →
   exactly one of {settle, release, expire}. The terminal frame *is* the ack,
   so result delivery and task acknowledgment cannot diverge.
10. **Frames are accepted only from the current attempt.** `(ID, Attempt)` is
    the fencing pair: a zombie worker holding a superseded attempt cannot
    write to the stream or double-settle. The first terminal frame wins;
    later ones are dropped with a metric.
11. **Workers are never called.** All worker communication is
    worker-initiated (claim, heartbeat, emit); refusal is expressed in
    responses (`410`), capacity in signals (metrics), lifecycle by the
    process owner (SIGTERM / context). A worker has no inbound surface.
12. **The control plane is stateless.** State lives in the queue (flow +
    clock), the bus (ephemeral fan-out), the ledger (durable truth), and the
    spec store. Any replica serves any request; scaling the control plane is
    adding replicas.
13. **Acknowledged means durable.** A `2xx` dispatch is in ledger + queue
    and will reach a terminal state; a `2xx` terminal emit is in the ledger.
    Durability attaches at the ack, nowhere else.
14. **Cross-store transitions are ordered idempotent steps, never
    transactions.** Every step is fenced by `(ID, Attempt)`; a crash at any
    point is retried or repaired — never lost, never double-truthed.

## Primitive 1: the Task Queue

### Door in: enqueue

Environment routing happens *above* the queue: the control plane owns one
`Queue` per (deployment, environment) and routes at enqueue time via the
tool→environment map. `queue.Results` dissolves — results are terminal frames
(see Primitive 2), and the task ledger is their projection.

### Door out: claiming a task

The door out cannot be a plain read. Handing a task to a worker transfers
responsibility, and a transfer of responsibility needs three properties a
pop does not have:

- **exclusive** — competitive consumption: exactly one worker wins the task
- **time-bounded** — workers die; a claimed task whose worker stops checking
  in must flow back to the queue
- **accountable** — every claimed attempt ends in exactly one of: **settled**
  (terminal frame accepted), **released** (voluntary give-back), or
  **expired** (deadline passed without a heartbeat)

There is no second object carrying these properties — no lease, no
assignment resource. **The claim hands out the task itself**, moved to the
`assigned` state, and the claim ticket is data the task already has:
`(ID, Attempt)`. That pair is the worker's fencing token for everything it
does to the task: the frame stream accepts emissions only from the current
attempt (invariant 10), so a zombie worker holding a superseded attempt
cannot write frames or double-settle.

```go
// pkg/queue — the competitive primitive's full contract
type Queue interface {
    Enqueue(ctx context.Context, t task.Task) error

    // Claim blocks until work is available and returns the task itself
    // (Attempt set) plus its deadline. (ID, Attempt) addresses the claim
    // in every call below.
    Claim(ctx context.Context, ttl time.Duration) (task.Task, time.Time, error)

    Heartbeat(ctx context.Context, id string, attempt int) (time.Time, error)
    Release(ctx context.Context, id string, attempt int) error // requeue, Attempt++
    Settle(ctx context.Context, id string, attempt int) error  // exactly once;
    // triggered by terminal-frame acceptance — workers never call it directly
}
```

Lifecycle: **claim → heartbeat\* → exactly one of {settle, release, expire}.**
Settlement is not an endpoint and not a worker verb: **the terminal frame is
the ack.** When the bus accepts a `playscape.result`/`playscape.error` frame
from the current attempt, the control plane settles the task and retains the
frame in one step — there is no dual write between "report the result" and
"ack the task" for a worker crash to fall between.

Redelivery: release and expiry requeue the task (`Attempt++`, same task ID),
giving **at-least-once** delivery. A task may therefore execute more than
once; the first terminal frame wins, and terminal frames from superseded
attempts are rejected (`410 Gone` over HTTP) and counted. `Seq` is assigned
by the bus on acceptance, not by workers, so frame ordering survives
redelivery.

The claim lifecycle is worker-plane machinery only: none of it appears in
`tool.Runtime`. A tool knows nothing about the loan it runs under.

`task.Task` gains two budgets and a global ID:

```go
// pkg/task
type Task struct {
    ID      string // globally unique — /v1/tasks/{id} has no deployment segment
    Tool    string
    Input   []byte
    Depth   int // spawn depth consumed (call stack)
    Hops    int // handoff hops consumed (tail-call chain; bounds ping-pong)
    Attempt int // redelivery count; incremented on release/expiry; half of
                // the (ID, Attempt) fencing pair
}

type Result struct { // decoded form of a terminal frame — a projection type
    TaskID      string
    Status      Status // ok | error | continued
    Output      []byte
    Error       string
    ContinuedBy string // set when Status == continued (handoff)
    NodeID      string
}
```

## Primitive 2: the Frame Stream

A new leaf package, stdlib-only:

```go
// pkg/event
type Event struct {
    Task    string // global task ID
    Seq     uint64 // per-task ordering; assigned by the bus on acceptance
    Type    string // adapter-namespaced ("saige.delta"); "playscape.*" reserved
    Payload []byte // opaque to playscape, always
}

// Sink is the execution-side door in.
type Sink interface {
    Emit(ctx context.Context, e Event) error
}

// Stream is the observation-side door out. Recv returns io.EOF after a
// terminal frame.
type Stream interface {
    Recv(ctx context.Context) (Event, error)
}

// Bus is the broadcast primitive: fan-out plus terminal-frame retention.
// Implementations: Memory (beta), durable brokers later.
type Bus interface {
    Sink
    Watch(ctx context.Context, taskID string) (Stream, error)
}
```

### Frame grammar

The `playscape.*` type namespace is reserved for the envelope's own frames;
adapters own everything else.

| Type | Terminal | Meaning |
|------|----------|---------|
| `playscape.result` | yes | task succeeded; payload is the `Result` |
| `playscape.error` | yes | task failed; payload is the `Result` |
| `playscape.handoff` | no (chain hop) | control transferred; `ContinuedBy` names the continuation |
| anything else | no | adapter-defined; payload opaque |

Exactly one terminal frame ends each chain link; a `playscape.handoff` frame
links to the continuation, and chain-following lives in the control plane's
`Watch` (the bus does not know about lineage).

## Runtime: Delegation as Compositions

Every delegation verb is a composition of the four doors, confined by the
executing tool's policy before the tool sees it:

```go
// pkg/tool
type Runtime interface {
    Workspace() workspace.Workspace
    Events() event.Sink // this task's frame stream, door in

    // Spawn is a call: dispatch a child, keep running. Gated by NGAC op
    // "spawn" over tool:<target>. Composition: dispatch.
    Spawn(ctx context.Context, t task.Task) (string, error)

    // Await blocks for a task's terminal result. Composition: watch to EOF.
    Await(ctx context.Context, id string) (task.Result, error)

    // Handoff is a tail call: close this task as "continued" and transfer
    // control (and the caller's awaiters/watchers) to the continuation.
    // Gated by NGAC op "handoff" — a distinct, larger grant than "spawn".
    // Composition: dispatch(continuation) + emit(terminal frame), atomic.
    Handoff(ctx context.Context, t task.Task) error
}
```

Semantics:

- **Spawn is a call, handoff is a tail call.** Spawn keeps the parent's node
  slot and creates an independent result lineage. Handoff frees the slot
  immediately and *extends* the current lineage: the original submitter's
  `Await`/`watch` transparently follows to the terminal task and never learns
  a handoff happened.
- **Scaling falls out of handoffs.** With per-environment queues and
  depth-driven autoscaling, a handoff drains the source environment's queue
  (scales down) and grows the target's (scales up). No lifecycle choreography.
- **Budgets.** `Depth` bounds spawn recursion (a stack); `Hops` bounds handoff
  chains (tail calls consume no stack, so depth alone cannot stop ping-pong).
- **Capacity deadlock (spawn+await).** All consumers blocked awaiting queued
  children starves the queue. Beta mitigations, in order: prefer handoffs for
  chains (no parent blocks); depth-tagged tasks with documented
  `Scale(n) > max depth`; parent-executes-child work stealing later.

## Execution Environments

The environment is what runs the agents: a named binding from tools to the
substrate and capacity that execute them. Routing is at **enqueue time, not
claim time**: one queue per environment, workers attach to exactly one.

```go
// pkg/controlplane
type ServiceSpec struct {
    Name         string
    Environments []Environment // default: one "default" env, all tools
    Policies     []sandbox.Policy
    Access       *ngac.Spec
}

type Environment struct {
    Name     string
    Tools    []string // tool→env binding; a tool belongs to exactly one env
    Replicas int      // local nodes; 0 = remote workers only
}
```

- Environments are declared in the deployment spec but are **addressable
  sub-resources**, because capacity lives on them: they are listed, inspected,
  and scaled individually. In particular, `scale` moves from the deployment
  to the environment — a deployment has no replica count of its own.
- `node.Factory` does not change shape; an environment is which factory
  builds its nodes plus which queue its tasks route to.
- `playscape work --deployment X --environment gpu` attaches a remote worker;
  on Kubernetes, one worker Deployment (and HPA on
  `playscape_queue_depth{deployment,environment}`) per environment.
- Environments are **orthogonal to NGAC**: policy answers *who may invoke a
  tool*; environment answers *where it runs*. Environments never appear in
  the policy graph.

## Workers: No Inbound Surface

A worker has no API. Its interface is inverted — defined by what it
consumes, not what it offers:

```go
// pkg/worker
type Deps struct {
    Queue queue.Queue  // claim / heartbeat / release — pull work, prove liveness
    Bus   event.Bus    // emit — push frames
    Nodes node.Factory // execute claimed tasks in sandboxed nodes
}

// Run is the whole worker: a loop, not a service. No listen socket.
func Run(ctx context.Context, d Deps) error
```

Every arrow points outward from the worker: it claims, it heartbeats, it
emits. Nothing in the system can call a worker because there is nothing to
call — unidirectionality is structural, not conventional. Even "you lost the
task" is never pushed: it is the `410 Gone` a superseded attempt receives on
its next heartbeat or emit. The control plane influences workers through
exactly three one-way channels:

1. **Work** — tasks appearing on the queue (data)
2. **Signal** — capacity metrics that a scaler acts on (control, indirect)
3. **Lifecycle** — context cancellation or SIGTERM from whatever owns the
   process (never a playscape RPC)

Shutdown protocol: stop claiming, finish or release the current task, exit.

## The Scaling Plane Is a Signal, Not a Component

There is a scaling plane, but playscape does not own it as code — its
interface is a metric, not a Go type. `playscape_queue_depth
{deployment,environment}` (with task latency alongside) is the contract;
anything that reconciles capacity against it is a scaler:

- locally, the `Scale(n)` goroutine pool inside the control plane
- remotely, a Kubernetes HPA replicating `playscape work` pods per environment
- anywhere else, any loop that starts and stops worker processes

A `Scaler` interface in playscape would re-implement HPA and pull worker
lifecycle into the control plane — over-abstraction, refused. playscape's
obligations end at two things: publish the signal, and tolerate workers
appearing or vanishing at any moment (which claim, heartbeat, and expiry
already guarantee). Control flows down as desired state and data; status
flows up only as worker-initiated reports.

## State, Scale, and Durability

"The control plane controls the queue and bus" must not mean it *stores*
them — that would make it a single point of failure with every frame flowing
through one process's memory. Exactly: **the control plane composes and
mediates, but never stores.** It is stateless; every piece of state lives
behind one of three interfaces, each with its own durability contract:

| State | Owner | Durability | Scale property |
|-------|-------|------------|----------------|
| deployment specs | spec store | durable (config) | read-mostly, cacheable |
| queued tasks, claims, deadlines, attempts | `queue.Queue` — *flow + clock* | durable until terminal | partitioned per (deployment, env) |
| intermediate frames | `event.Bus` — *fan-out* | **none, by design** | pure pub/sub, zero persistence |
| task index + terminal results | `ledger.Ledger` — *truth* | durable, one row per task | **two writes per task lifetime** |
| metrics | `metrics.Recorder` | none, recomputable | — |

`ledger` is a new leaf package (depends only on `task`): one row per task
holding `(ID, deployment, environment, state, attempt, result)`, with two
authoritative transitions — `Accept` at dispatch and a fenced
`Complete(id, attempt, result)` CAS at terminal. Everything between
(assigned, deadlines) is flow state owned by the queue: observable,
never authoritative. The terminal-retention invariant (4) is satisfied by
the ledger row, which is why the bus is *allowed* to be fully ephemeral.

Two rules keep the durable path off the hot path:

- **Heartbeats never touch the ledger.** The clock lives in the queue backend
  (Redis TTLs, SQS visibility timeouts). Durable writes per task: exactly two,
  regardless of frames, heartbeats, or retries.
- **The bus persists nothing.** Watchers needing the past get it from the
  ledger; watchers needing the present subscribe.

### Durability contract

**Acknowledged means durable.** A `2xx` from `dispatch` means the task is in
the ledger and the queue and will reach a terminal state (at-least-once). A
`2xx` on a terminal emit means the result is in the ledger. Durability
attaches at the ack and nowhere else; intermediate frames are explicitly
excluded.

### Recovery orderings

Cross-store transitions are **ordered idempotent steps fenced by
`(ID, Attempt)`, never distributed transactions**. A crash at any point is
retried or repaired, never lost and never double-truthed:

- **Dispatch**: (1) `ledger.Accept` (this row *is* the task→deployment
  index) → (2) `queue.Enqueue` → (3) `2xx`. Crash after (1): client never
  got the ack; a janitor re-enqueues `accepted` rows never claimed past a
  TTL — idempotent because the task ID is already fixed.
- **Terminal accept** (emit carrying `playscape.result|error`): (1)
  `ledger.Complete` CAS — wrong attempt or already terminal → `410` →
  (2) publish terminal frame on the bus → (3) `queue.Settle`. If (2) is
  lost, watchers reconcile against the ledger on reconnect; if (3) is lost,
  expiry discovers the truth (below).
- **Expiry** (any replica may run the reaper): before requeueing a
  dead claim, **read the ledger**. Terminal → drop the claim silently
  (the settle message was lost, not the task). Not terminal → requeue with
  `Attempt++`. This one rule prevents zombie re-execution.
- **Delegation** (`Spawn`/`Handoff`): child task IDs are **deterministic** —
  derived from (parent ID, attempt, delegation index) — so re-executing a
  parent after a crash re-derives the same child ID and `ledger.Accept`
  no-ops. Enqueue-then-close-parent, in that order, is safe to replay.
- **Watch** (any replica): read the ledger — terminal → serve the retained
  result, close. Otherwise **subscribe to the bus first, then re-check the
  ledger**: checking only before subscribing would miss a terminal that
  lands in the gap; subscribe-then-check closes the race.

### Scaling the control plane itself

With all state behind the three interfaces, `playscape serve` replicas are
interchangeable: any replica serves any `dispatch`, `watch`, or operator
call; the janitor and reaper run on every replica (safe — every action is a
fenced CAS). Requirements this places on backends: the queue needs
linearizable claim per environment (one winner per attempt); the bus needs
cross-replica subscription (broker pub/sub) for `watch` to be servable
anywhere; the ledger needs CAS. The beta's `Memory` implementations meet the
*correctness* contracts in one process and simply do not hold the durability
promise — single replica, documented, and the composition is identical when
real backends land.

## API Grammar

**Verbs move work, nouns hold state.** Flow verbs address by envelope
(`{"deployment": ..., "tool": ...}`); state nouns address by path. `/tasks` is
never client-writable — most tasks are created internally (spawns, handoffs),
so tasks are consequences and `dispatch` is the cause.

| Plane | Endpoint | Semantics |
|-------|----------|-----------|
| Work (untrusted) | `POST /v1/dispatch` | enqueue; 202 `{id, watch}`; always async |
| Work (untrusted) | `GET /v1/tasks/{id}/watch` | SSE to terminal frame; total; chain-following |
| Work (untrusted) | `GET /v1/tasks/{id}` | status + result snapshot (projection) |
| Work (untrusted) | `GET /v1/tasks?deployment=&environment=&state=` | ledger listing; `state=assigned` is the in-flight-work view |
| Operate | `GET/POST /v1/deployments` | fleet view: list / create |
| Operate | `GET /v1/deployments/{name}` (+`/spec`) | status, spec |
| Operate | `GET /v1/deployments/{name}/environments` | list environments: queue depth, workers, capacity |
| Operate | `GET /v1/deployments/{name}/environments/{env}` | one environment's status |
| Operate | `POST /v1/deployments/{name}/environments/{env}/scale` | local replica count (the one imperative sub-resource, k8s precedent) |
| Infra | `GET /healthz`, `GET /metrics` | unversioned by convention |

Sync submit, `Await`, and streaming are **client compositions**, not server
modes: `dispatch` then read `watch` to EOF, take the last frame. SSE `id` is
`taskID:seq`; `Last-Event-ID` is the reconnect hook (beta: no replay except
the terminal frame — documented, not papered over).

`/tasks/{id}/watch` vs a future `/tasks/{id}/events`: `watch` is the live,
held-open stream; `events` is reserved for the replayable, paginatable
collection once durable brokers land. Two names so neither mutates.

The verb placement rule: **queue doors are top-level verbs because task
identity is unknown at call time** — `dispatch` creates the ID, `claim`
doesn't know which task it will win. **Stream doors and the claim lifecycle
are task sub-resources because identity is known** — `emit`, `watch`,
`heartbeat`, and `release` all address one specific task (workers by
`{attempt}` in the body, the fencing pair). No claim, lease, or assignment
noun exists: `assigned` is a task state, and the in-flight-work view is just
`GET /v1/tasks?state=assigned`.

### The Worker Wire

The worker verbs — `claim`, `heartbeat`, `release`, `emit` — have **no
endpoints in the API**. Workers speak them against the `queue.Queue` and
`event.Bus` interfaces, and whether a wire exists underneath is decided by
the backend:

- **In-memory backend (beta):** the queue and bus live inside the server
  process, so the server must double as its own message broker. The `Memory`
  backends' remote transport is a small HTTP surface under `/internal/*`
  (`POST /internal/claim`, `POST /internal/tasks/{id}/heartbeat|release|emit`)
  spoken only by `playscape work`. It is unversioned, worker-credentialed when
  auth lands, firewallable or bound to a separate listener, and free to
  change every release — server and worker ship in one binary.
- **Broker backend (Redis, SQS, Pub/Sub):** workers' `queue.Queue`
  implementation talks to the broker directly; the control plane is not in
  the execution data path at all, and `/internal/*` does not exist.
- **In-process workers (`Scale`):** direct method calls; no wire, ever.

The test this encodes: anything that disappears when a backend is swapped
was never API. If polyglot workers are ever wanted, designing a public
worker protocol at `/v1` is a deliberate act, not a default.

Migration from the current surface: `POST /v1/deployments/{name}/tasks` →
`POST /v1/dispatch`; `GET /v1/deployments/{name}/tasks/{id}` →
`GET /v1/tasks/{id}` (task IDs go global; the control plane needs a
task→deployment index); `POST /v1/deployments/{name}/lease` and
`POST /v1/deployments/{name}/results` leave the API — they become the
in-memory backend's worker wire (see above); `POST /v1/deployments/{name}/scale` →
`POST /v1/deployments/{name}/environments/{env}/scale` (capacity belongs to
the environment). `/deployments` itself is unchanged.

## Control Plane Surface

```go
// pkg/controlplane — composition root
// operator
Deploy(ctx context.Context, spec ServiceSpec) error
Scale(ctx context.Context, deployment, environment string, replicas int) error
Status(ctx context.Context, name string) (Status, error)

// doors (untrusted pair)
Dispatch(ctx context.Context, s Submission) (id string, err error)
Watch(ctx context.Context, taskID string) (event.Stream, error) // follows ContinuedBy

// projection
Task(ctx context.Context, taskID string) (task.Result, bool, error)

type Submission struct {
    Deployment string // optional when only one deployment exists
    Tool       string
    Input      []byte
}
```

The worker loop becomes: claim a task → heartbeat while the node executes →
forward tool frames via `emit` under `(ID, Attempt)` → the terminal frame
settles the task. On graceful shutdown mid-task, release; on crash, expiry
requeues. The loop is written once against `queue.Queue` and `event.Bus`;
whether those calls cross a wire is the backend's business (see "The Worker
Wire").

## Adapter Contract

There is deliberately **no `Agent` interface in playscape** and no framework
named in the root module. Any agent framework integrates by meeting three
obligations:

1. Implement `tool.Tool` — run the framework's agent inside `Call`, using
   `Runtime` for workspace, delegation, and emission.
2. Pick an event `Type` namespace and encode the framework's native stream
   into `Payload` bytes.
3. Ship a client-side decoder turning an `event.Stream` back into the
   framework's native stream type.

The saige adapter (separate module, e.g. `adapters/saige/`, keeping saige's
dependencies out of the root module) is the reference implementation, and it
is bidirectional:

```go
// playscape runs saige — agent as workload:
// pipes stream.Deltas() → rt.Events().Emit("saige.delta", json(delta)).
func Tool(name string, cfg saige.AgentConfig) tool.Tool

// saige calls playscape — delegation as LLM-facing saige tools:
func DelegateTool(rt tool.Runtime) /* saige tool */ // Spawn + Await
func HandoffTool(rt tool.Runtime)  /* saige tool */ // tail call

// client consumes playscape as saige — location-transparent streaming:
func Deltas(s event.Stream) (<-chan types.Delta, func() error)
```

A caller that dispatches a task and decodes `watch` with `Deltas()` streams a
remote agent three handoffs deep on a GPU worker identically to a local
`ag.Invoke` — chain-following plus the codec make location fully transparent.

## Beta Caveats

- The in-memory bus has no replay: a late or reconnecting watcher gets frames
  from now on, plus the retained terminal result from the ledger. Honest gap;
  durable brokers fill it behind `event.Bus`.
- The `Memory` queue/bus/ledger meet the correctness contracts (fenced CAS,
  one winner per claim, terminal-first-wins) but not the durability promise:
  process death loses queued tasks and results. Invariant 13 states the
  contract real backends must satisfy; the beta documents that it holds it
  only within a process lifetime.
- The claim protocol upgrades delivery from the current at-most-once to
  at-least-once: expiry and release requeue, so a task may execute twice.
  Tools should be idempotent or tolerate duplicate side effects; the fencing
  token and first-terminal-wins keep the *observable* result single-valued
  even when execution is not. The in-memory queue implements deadlines with
  timers; brokers map naturally (SQS visibility timeout, Redis consumer
  groups + `XAUTOCLAIM`).
- Per-environment queues multiply in-memory queue count (trivial now) and
  shape what Redis/SQS backends model: queue-per-env vs one stream with
  routing keys.
- Spawn/handoff gating is enforced at the node (worker side). Acceptable
  because workers are trusted infrastructure; revisit when worker auth lands.
