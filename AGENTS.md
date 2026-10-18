# playscape

Beta control plane for agent execution nodes: deploy one service, scale sandboxed agents with metrics on a shared workspace. Go module `github.com/urmzd/playscape`, binary `playscape`.

## Architecture

Producer/consumer over per-deployment queues. Producers (HTTP API, tools spawning sub-tasks) enqueue; competing consumers (local goroutines via `Scale`, remote `playscape work` processes leasing over HTTP) execute. Packages form a strict one-way DAG:

- `pkg/task`, `pkg/workspace`, `pkg/metrics`, `pkg/ngac` are leaves (stdlib only)
- `pkg/ngac` is the NGAC policy machine where access is defined (users/attributes/associations/prohibitions); `pkg/sandbox` is the enforcement point decorating `workspace`, with flat `Policy` compiling into the same graph via `FromPolicies`
- `pkg/tool` defines the capability unit and the `Runtime` it executes against
- `pkg/queue` is the producer/consumer seam (`Memory` in-process, `httpqueue` over the API)
- `pkg/node` + `pkg/node/inproc` define and implement the execution substrate
- `pkg/worker` is the consumer loop shared by local and remote scaling
- `pkg/controlplane` is the composition root; `internal/server` is a thin HTTP layer; `internal/cli` wires `serve`/`work`/`version`/`update`

Full guide: `docs/architecture/overview.md`. Discover layout with `tree` or ripgrep; do not trust stale listings.

## Commands

| Action | Command |
|--------|---------|
| init | `make init` |
| build | `make build` |
| test | `go test ./...` |
| lint | `golangci-lint run` |
| fmt | `gofmt -w .` |
| quality gate | `make check` |
| run server | `make run` (or `go run ./cmd/playscape serve`) |
| examples | `go run ./examples/basic/`; `cd examples/saige && go run .` |
| k8s validation | `deploy/k8s/playscape.yaml` on minikube (see README) |

## Code Style

- Idiomatic Go, stdlib-first; every dependency must earn its place (current: cobra)
- Interfaces stay small and live with their concern; implementations may depend on interfaces, never the reverse
- Errors: wrap with `fmt.Errorf("pkg: context: %w", err)`; sentinel errors (`ErrNotFound`, `ErrDenied`) for callers to `errors.Is` on
- Package doc comments explain the concern and its orthogonality boundary
- Conventional commits (feat/fix/chore/...); sr cuts releases from them

## Rules

- `pkg/sandbox` and `pkg/ngac` are a security boundary. Any change near them needs tests proving confinement still holds (traversal, sibling prefixes, list-leak, spawn gating, prohibition override, multi-policy-class conjunction). Default is always deny.
- Preserve the dependency DAG: nothing under `pkg/` may import `pkg/controlplane`; leaves stay stdlib-only.
- New backends implement existing interfaces (`workspace.Workspace`, `queue.Queue`, `node.Factory`, `metrics.Recorder`); do not widen interfaces for one backend's needs.
- `examples/saige` is its own Go module (keeps saige's deps out of the root module); it builds in CI but is not part of `go test ./...` at root.
- This is a beta: keep the surface small, document limitations honestly (README beta note, architecture caveats) rather than papering over them.

## Extension Guide

- **New tool**: implement `tool.Tool` (or `tool.Func`), register it, and grant it access in the `ServiceSpec`: a flat `sandbox.Policy`, or an `ngac.Spec` under `Access` for relational grants (user attributes, prohibitions). No grant means no workspace access and no spawning.
- **New workspace backend** (GCS/S3): implement `workspace.Workspace` in `pkg/workspace/<backend>.go`; validate keys with `workspace.ValidKey`.
- **New queue backend**: implement `queue.Queue` + `queue.Results`; see `httpqueue` for a remote example.
- **New execution substrate** (containers): implement `node.Factory`/`node.Node`; `pkg/worker` and the control plane need no changes.
