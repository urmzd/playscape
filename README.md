<p align="center">
  <h1 align="center">playscape</h1>
  <p align="center">
    Control plane for agent execution nodes: deploy one service, scale sandboxed agents with metrics on a shared workspace.
    <br /><br />
    <a href="https://github.com/urmzd/playscape/releases">Download</a>
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

> Formerly `dispatch`.

> **Beta.** playscape is pre-1.0 and under active development. Interfaces are stabilizing but may change between minor versions; queues are in-memory (at-most-once delivery) and the shared workspace backend is a local directory. Durable brokers and GCS/S3 workspace backends are on the roadmap.

## Features

- **Deploy a single service** and scale its agent execution nodes without changing it
- **Producer/consumer execution**: tasks flow through a queue; scaling out is adding consumers, locally as goroutines or remotely as Kubernetes pods and serverless containers running `playscape work`
- **Sandboxed tools**: every tool is locked to declared workspace areas and spawn targets; anything not granted is denied, so a compromised tool cannot leak outside its area
- **NGAC access control**: access is defined in a NIST-style policy machine (users, attributes, associations, prohibitions) and enforced by the sandbox; flat per-tool policies compile into the same graph, full `access` specs express group grants and overriding denials
- **Shared workspace**: all nodes read and write one storage backend, so state lives in exactly one place
- **Self-referential agents**: a tool can spawn sub-tasks back into its own deployment (an agent calling another agent), gated by its policy
- **Metrics built in**: node counts, task throughput, and latency recorded behind a transport-agnostic interface
- **Orthogonal interfaces**: workspace, sandbox, tool, queue, node, and control plane compose without knowing about each other

## Installation

### Script (macOS / Linux)

```sh
curl -fsSL https://raw.githubusercontent.com/urmzd/playscape/main/install.sh | sh
```

### Manual

Download a pre-built binary from the [releases page](https://github.com/urmzd/playscape/releases/latest).

### Go SDK

```sh
go get github.com/urmzd/playscape
```

## Quick Start

Run the control plane and exercise it with the built-in sandboxed `echo` tool:

```sh
playscape serve &

curl -X POST localhost:8484/v1/deployments \
  -d '{"name":"echo-service","replicas":3,"policies":[{"tool":"echo","areas":[{"prefix":"echo"}]}]}'

curl -X POST localhost:8484/v1/deployments/echo-service/tasks \
  -d '{"tool":"echo","input":"hello"}'

curl -X POST localhost:8484/v1/deployments/echo-service/scale -d '{"replicas":10}'

curl localhost:8484/metrics
```

Or compose the same thing in Go:

<!-- fsrc src="examples/basic/main.go" fence="auto" -->
```go
// Basic example: compose a control plane from the orthogonal pieces
// (a local workspace, a sandboxed tool, an in-process node factory, and an
// in-memory metrics recorder), then deploy a service, scale it out, submit
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

	// Scale out; submitted tasks are consumed by competing nodes.
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

See [`examples/`](examples/) for more.

## Scaling on Kubernetes

Execution scales by replicating consumers, not by touching the control plane. The reference manifests deploy one `playscape serve` service and a fleet of `playscape work` pods that lease tasks from it over HTTP:

```sh
minikube start
eval $(minikube docker-env) && docker build -t playscape:dev .
kubectl apply -f deploy/k8s/playscape.yaml

kubectl scale deployment/playscape-worker --replicas=10
```

Deploy a service with `"replicas": -1` (no local nodes) and every task is executed by the worker fleet. An HPA on queue or task metrics gives the same effect for serverless-style autoscaling.

## Examples

| Example | Description |
|---------|-------------|
| [`basic`](examples/basic/) | Deploy, scale, submit, and observe with a sandboxed tool |
| [`ngac`](examples/ngac/) | Define access as an NGAC policy graph: attribute grants, prohibitions, spawn objects |
| [`saige`](examples/saige/) | Run [saige](https://github.com/urmzd/saige) AI agents as workloads, including an agent spawning a sub-agent |

## Architecture

Tasks are produced onto per-deployment queues and executed by competing consumers. Each consumer is a node that resolves tools from a registry and confines every call through the deployment's NGAC policy graph: the executing tool is the policy-machine user, workspace keys and spawn targets are the objects, and the default is deny. The packages form a strict one-way dependency graph so each concern can change independently.

See [docs/architecture/overview.md](docs/architecture/overview.md) for the full architecture guide.

## Agent Skill

This repo's conventions are available as portable agent skills in [`skills/`](skills/).

## License

[Apache-2.0](LICENSE)
