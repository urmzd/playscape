<p align="center">
  <h1 align="center">playscape</h1>
  <p align="center">
    Sandboxed tools, one shared workspace, and NGAC access control for running agent tasks, as a Go library.
    <br /><br />
    <a href="#installation">Install</a>
    &middot;
    <a href="https://github.com/urmzd/playscape/issues">Report Bug</a>
    &middot;
    <a href="https://pkg.go.dev/github.com/urmzd/playscape">Go Docs</a>
  </p>
</p>

<p align="center">
  <a href="https://github.com/urmzd/playscape/actions/workflows/ci.yml"><img src="https://github.com/urmzd/playscape/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  &nbsp;
  <a href="https://pkg.go.dev/github.com/urmzd/playscape"><img src="https://pkg.go.dev/badge/github.com/urmzd/playscape.svg" alt="Go Reference"></a>
  &nbsp;
  <a href="LICENSE"><img src="https://img.shields.io/github/license/urmzd/playscape" alt="License"></a>
</p>

playscape is a library first. Compose it in code: a workspace, a tool
registry, a node factory and a control plane, with an access policy per
deployment. It starts no server and reads no configuration. The `playscape`
CLI does one job: `playscape run` validates a command in the sandbox, so you
can see what a policy lets it read and change.

## Features

- **Sandboxed tools**: every tool sees the workspace only through a view its policy grants; anything not granted is denied, so a misbehaving tool cannot read or write outside its areas
- **NGAC access control**: access is defined in a NIST-style policy machine (users, attributes, associations, prohibitions) and enforced by the sandbox; flat per-tool policies compile into the same graph
- **Bounded in-process pools**: each deployment runs its tasks on a fixed number of nodes, one task per node at a time, with a bounded backlog; resizing never cancels work
- **Shared workspace**: every node reads and writes one backend; the local backend is confined to its directory with `os.Root`, so a symbolic link cannot lead a key outside it
- **Self-referential agents**: a tool can spawn sub-tasks into its own deployment (an agent calling another agent), gated by its policy
- **Isolated instances**: `pkg/isolate` runs a run's commands in a sandbox instance instead of on the host. The Docker driver runs each under gVisor (runc only with an explicit development opt-in) with no network, a read-only root, no capabilities, a seccomp profile stricter than Docker's default, a non-root user, and CPU, memory, process and scratch limits; a second compartment can hold credential-bearing tool servers that share only a socket directory with the workload
- **Egress gate**: `isolate/egress` is an HTTP forward proxy that is an instance's only way out: a default-deny host allowlist per lease, and credential injection, so the sandbox holds a placeholder and the gate swaps in the real value for an allowed host
- **Warm pools, one run per instance**: `isolate.Pool` keeps instances warm per spec and leases each to exactly one run; releasing destroys it and a fresh one starts in the background
- **Command runners**: `runner.Shell` turns an executor into a tool; `isolate.Executor` runs it inside a leased instance; `pkg/runner/k8s`, a module of its own, runs each call as one locked-down Kubernetes Job
- **CLI tools**: the `cli` provider wraps a binary behind a fixed argv template, an environment allowlist, a timeout and output caps
- **Metrics seam**: node counts, backlog, throughput and latency recorded behind a transport-agnostic `metrics.Recorder`

**Early development:** pre-1.0, so the API may change between minor versions. Tasks are not durable, the only workspace backend is a local directory, and metrics have no exporter yet. See [Limits](#limits).

## Installation

The library has no dependencies outside the Go standard library:

```sh
go get github.com/urmzd/playscape
```

The Kubernetes Job runner is a separate module, so only programs that use it inherit client-go:

```sh
go get github.com/urmzd/playscape/pkg/runner/k8s
```

The CLI, for validating commands:

```sh
go install github.com/urmzd/playscape/cmd/playscape@latest
# or a release binary (macOS, Linux)
curl -fsSL https://raw.githubusercontent.com/urmzd/playscape/main/install.sh | sh
```

## Quick Start

Compose a control plane from its pieces, deploy one service with a policy, and submit tasks to it:

<!-- fsrc src="examples/basic/main.go" fence="auto" -->
```go
// Basic example: compose a control plane from the orthogonal pieces
// (a local workspace, a sandboxed tool, an in-process node factory, and an
// in-memory metrics recorder), then deploy a service, size its pool, submit
// tasks, and print the resulting metrics.
//
// Prerequisites: none. Run with:
//
//	go run ./examples/basic/
package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"sort"

	"github.com/urmzd/playscape/pkg/controlplane"
	"github.com/urmzd/playscape/pkg/metrics"
	"github.com/urmzd/playscape/pkg/node/inproc"
	"github.com/urmzd/playscape/pkg/sandbox"
	"github.com/urmzd/playscape/pkg/task"
	"github.com/urmzd/playscape/pkg/tool"
	"github.com/urmzd/playscape/pkg/workspace"
)

func main() {
	ctx := context.Background()

	// Shared workspace: every node reads and writes the same backend.
	dir, err := os.MkdirTemp("", "playscape-example-*")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)
	ws, err := workspace.NewLocal(dir)
	if err != nil {
		log.Fatal(err)
	}

	// One tool. It sees only the workspace view its policy grants below.
	registry := tool.NewRegistry()
	err = registry.Register(tool.Func("greet", func(ctx context.Context, rt tool.Runtime, in []byte) ([]byte, error) {
		out := []byte("hello, " + string(in))
		// Allowed: "greetings/..." is inside this tool's area.
		if err := rt.Workspace().Write(ctx, "greetings/last", bytes.NewReader(out)); err != nil {
			return nil, err
		}
		// Denied by the sandbox: "secrets/..." is outside the area.
		if err := rt.Workspace().Write(ctx, "secrets/steal", bytes.NewReader(out)); err != nil {
			fmt.Printf("sandbox blocked the leak: %v\n", err)
		}
		return out, nil
	}))
	if err != nil {
		log.Fatal(err)
	}

	// Compose the control plane and deploy one service.
	rec := metrics.NewMemory()
	plane := controlplane.NewMemory(inproc.NewFactory(registry, ws), rec)
	err = plane.Deploy(ctx, controlplane.ServiceSpec{
		Name:     "greeter",
		Replicas: 1,
		Policies: []sandbox.Policy{{Tool: "greet", Areas: []sandbox.Area{{Prefix: "greetings"}}}},
	})
	if err != nil {
		log.Fatal(err)
	}

	// Grow the pool to three nodes; each runs one task at a time.
	if err := plane.Scale(ctx, "greeter", 3); err != nil {
		log.Fatal(err)
	}
	for _, name := range []string{"ada", "grace", "alan"} {
		res, err := plane.Submit(ctx, "greeter", task.Task{Tool: "greet", Input: []byte(name)})
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("%s ran on %s: %s\n", name, res.NodeID, res.Output)
	}

	// Metrics accumulated along the way.
	fmt.Println("\nmetrics:")
	snap := rec.Snapshot()
	keys := make([]string, 0, len(snap))
	for k := range snap {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("  %s %g\n", k, snap[k])
	}
}
```
<!-- /fsrc -->

`Submit` blocks until the task's result arrives; `SubmitAsync` returns its ID for `Result` to poll. A deployment's `Replicas` is how many of its tasks run at once; further tasks wait in its backlog for a free node.

## Examples

| Example | Description |
|---------|-------------|
| [`basic`](examples/basic/) | Deploy, size the pool, submit, and observe with a sandboxed tool |
| [`ngac`](examples/ngac/) | Define access as an NGAC policy graph: attribute grants, prohibitions, spawn objects |
| [`saige`](examples/saige/) | Run [saige](https://github.com/urmzd/saige) agents as workloads, including an agent spawning a sub-agent |

## Usage

### Validating a command

`playscape run` runs one command the way a program embedding the library would: a local workspace, a tool resolved by an in-process node, and an NGAC policy deciding every workspace operation. Files the policy lets the command read are copied into a scratch directory, its output streams as it runs, and each file it creates, changes or removes is applied back to the workspace through the policy:

```console
$ playscape run -workspace ./ws -read src -area out 'wc -l src/*.go > out/lines; echo x > notes'
playscape: ran as "run" for 4ms
playscape: staged 3 file(s) the policy lets "run" read
playscape: wrote out/lines
playscape: DENIED write notes (sandbox: access denied by policy: user "run", op write, key "notes")
playscape: command exited 0, but the policy DENIED 1 change(s)
$ echo $?
77
```

| Flag | Meaning |
|------|---------|
| `-workspace DIR` | workspace directory (default: a temporary one, removed afterwards) |
| `-area PREFIX` / `-read PREFIX` | grant read and write, or read only, on a key prefix (repeatable) |
| `-policy FILE` | a full NGAC policy (`ngac.Spec` as JSON) instead of `-area`/`-read`; `-as` names the user |
| `-env NAME`, `-env NAME=VALUE` | pass a host variable through, or set one; nothing else is inherited except `PATH` |
| `-timeout DUR` | how long the command may run (default 5m) |
| `-json` | print one JSON report on stdout, with the output captured in it |

The exit status is the command's own, or 77 when it succeeded but a change was denied, 124 on timeout, 125 when it could not run, 130 when interrupted. The command is a host process with your privileges: the policy decides what it sees of the workspace and what it may change there, not what it can reach on the machine.

### Isolated instances

```go
drv, err := docker.New(docker.Config{}) // runsc (gVisor); the daemon must have it registered
if err != nil {
	return err
}
pool := isolate.NewPool(drv, isolate.PoolConfig{Warm: 4, Max: 500})
defer pool.Close(ctx)

lease, err := pool.Lease(ctx, isolate.Spec{
	Image:  "python:3.13-alpine",
	Mounts: isolate.Mounts{Project: snapshotDir},
})
if err != nil {
	return err
}
defer lease.Release(ctx) // destroys the instance; it is never reused
if err := registry.Register(runner.Shell("shell", isolate.Executor{Instance: lease}, runner.ShellConfig{})); err != nil {
	return err
}
```

To give an instance a way out, serve an `egress.Gate` on an `--internal` Docker network, name that network in `docker.Config.EgressNetwork`, and set `Spec.Network.Proxy` to the gate's address. Each lease gets its own grant:

```go
token, err := gate.Grant(lease.ID(), egress.Policy{Allow: []egress.Rule{{
	Host:     "github.route", // the sandbox calls http://github.route/...
	Upstream: "https://api.github.com",
	Inject:   []egress.Injection{{Header: "Authorization", Placeholder: "PLACEHOLDER", Credential: "github"}},
}}})
if err != nil {
	return err
}
defer gate.Revoke(lease.ID())
env := egress.ProxyEnv("gate:3128", token) // add to every command of this lease
```

Inside an instance, the run's files are at `/work` (where commands start, and what `lease.Files()` reads and writes from the host), the project at `/project` (read only), the person volume at `/me`, and the socket directory shared with the connectors compartment at `/run/playscape`. Every zero `Spec` field is the strictest choice: no network, no extra mounts, a non-root user, small default limits. `docker.New` refuses a runtime that shares the host kernel (`ErrInsecureRuntime`) unless `Config.InsecureDevelopmentRuntime` is set, for machines without gVisor; it then warns on every `New`. `isolate/local` implements the same interface with no isolation at all, for tests. Every driver runs the `isolatetest` conformance suite, and CI runs it against Docker with both runc and runsc.

### Sandboxed shell on Kubernetes

```go
exec, err := k8s.New(clientset, k8s.Config{Namespace: "runs", Image: "alpine:3.20", NetworkIsolated: true})
if err != nil {
	return err
}
if err := registry.Register(runner.Shell("shell", exec, runner.ShellConfig{Timeout: time.Minute})); err != nil {
	return err
}
```

Each call becomes one batch/v1 Job: non-root, no privilege escalation, all capabilities dropped, read-only root filesystem, size-limited scratch, CPU and memory limits, no service-account token, an active deadline and TTL cleanup. The program embedding the executor needs [`manifests/rbac.yaml`](pkg/runner/k8s/manifests/rbac.yaml); [`manifests/networkpolicy.yaml`](pkg/runner/k8s/manifests/networkpolicy.yaml) optionally denies all traffic to isolated runs.

### Limits

- **Nothing is durable.** A task lives as long as the process that accepted it. Retried, replayable runs are the caller's job.
- **No per-task cancel.** Ending the context given to `Submit` stops the wait, not the task; `Close` cancels every running task at once.
- **Waiting on your own deployment can deadlock.** A tool that blocks on a sub-task it spawned holds its node; if every node waits, nothing runs. Size `Replicas` for it, or run sub-tasks on the parent's node.
- **Results are kept up to a bound** (1024 per deployment by default); `Result` reports `ErrUnknownTask` for one that has been forgotten.
- **The workspace policy is not process isolation.** It confines what a tool reaches through the workspace. The `cli` provider, `runner.Local`, `isolate/local` and `playscape run` start host processes with your privileges; isolation needs `pkg/isolate/docker` or `pkg/runner/k8s`.
- **The egress gate cannot inject into HTTPS it only tunnels.** A `CONNECT` is allowed or denied by host and port; what is inside is encrypted, so a placeholder sent through it arrives unchanged. Injection works for plain HTTP and for gate-terminated routes (the sandbox speaks HTTP to the gate, the gate speaks TLS upstream). The lease token is in the sandbox's environment; anyone on the egress network who learns it gets that lease's allowlist until `Revoke`. Instances on one egress network can reach each other, not only the gate.
- **Docker instances**: `/work` is a host directory and is not size limited by the driver; only `/tmp` is. Put `Config.WorkRoot` on a filesystem of its own (a sized tmpfs, a loop device or an XFS project quota) so a run cannot fill the host's disk. A command that times out restarts its compartment, which ends every other command running there. The daemon must be able to bind-mount `Config.WorkRoot` (with Docker Desktop or Colima, a directory shared with the VM). With runc (development only), the boundary is the host kernel's namespaces, which is not enough for hostile code. gVisor enforces the seccomp profile only when runsc is registered with `--oci-seccomp`; without it, gVisor's own kernel is the boundary. Debuggers and tracers (`ptrace`) do not work inside an instance.
- **Compartments** share one 1 MiB tmpfs directory, meant for sockets. Anything either side writes there, the other can read.
- **Pools** fix mounts at start, so a spec with a person volume warms only that person's instances. Warm instances are not health checked before a lease, and every spec a pool has seen stays warm until evicted.
- **Kubernetes runs** see no shared workspace (scratch only), stdout and stderr arrive as one stream, and status is polled, not watched.
- **The CLI** runs commands with `runner.Local` only, shows stdout and stderr as one stream, and stages every readable file before a run.

## Development

```sh
make init     # install git hooks and download modules
make check    # fmt, vet, lint, and race tests for both modules
make build    # writes bin/playscape
```

`pkg/runner/k8s` and `examples/saige` are separate Go modules, so `go test ./...` at the root does not reach them. `make test` covers the root and `pkg/runner/k8s`; run `examples/saige` from its own directory. See [CONTRIBUTING.md](CONTRIBUTING.md).

## Documentation

- [Architecture overview](docs/architecture/overview.md)
- [Agent execution design](docs/rfcs/0001-agent-execution.md)
- [Contributing](CONTRIBUTING.md)
- [Changelog](CHANGELOG.md)

## Agent Skill

This repo's conventions are available as portable agent skills in [`skills/`](skills/).

## License

[Apache-2.0](LICENSE)
