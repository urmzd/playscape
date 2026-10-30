# playscape

Beta library for where an agent's turn runs: sandboxed tools, one shared workspace, NGAC access control, and bounded in-process pools of execution nodes. Go module `github.com/urmzd/playscape`. Library first: nothing here listens on a port or reads configuration. The `playscape` binary (`cmd/playscape`) only validates a command in the sandbox (`playscape run`).

## Architecture

A program composes the pieces in code: `controlplane.NewMemory(inproc.NewFactory(registry, ws), recorder)`, then `Deploy` / `Submit` / `SubmitAsync` / `Result`. Each deployment is a bounded pool: `Replicas` nodes, one task per node at a time, and a FIFO backlog. Nothing is durable; retried runs belong to the caller (anchor, in the stack). Packages form a strict one-way DAG:

- `pkg/task`, `pkg/workspace`, `pkg/metrics`, `pkg/ngac` are leaves (stdlib only)
- `pkg/workspace` `Local` reaches its directory only through an `os.Root`; keys are regular files
- `pkg/ngac` is the NGAC policy machine where access is defined (users/attributes/associations/prohibitions); `pkg/sandbox` is the enforcement point decorating `workspace`, with flat `Policy` compiling into the same graph via `FromPolicies`
- `pkg/tool` defines the capability unit, the `Runtime` it executes against, and `Provider` (a family of tools of one `Kind`); `pkg/tool/cli` is the `cli` provider kind wrapping CLI binaries
- `pkg/runner` is the run executor seam (`Executor`: run one command, stream logs, report the exit code; `Local` for dev/tests, `Shell` adapts an executor into a tool). Process isolation lives here, not in `pkg/sandbox`
- `pkg/runner/k8s` is its own Go module (`github.com/urmzd/playscape/pkg/runner/k8s`, so only its importers get client-go): one restricted batch/v1 Job per run; the RBAC and NetworkPolicy it needs are in `pkg/runner/k8s/manifests`
- `pkg/isolate` is where a run's tools execute: `Driver` starts an `Instance` from a `Spec` (zero value is the strictest: no network, read-only root, non-root, default limits), `Pool` keeps them warm and leases each to one run, never reused, and `Executor` adapts an instance into a `runner.Executor`. `isolate/docker` drives the docker CLI (runc or runsc); `isolate/local` has no isolation and is for tests; `isolate/isolatetest` is the conformance suite every driver runs; `isolate/egress` is the egress gate (an `http.Handler` forward proxy: per-lease default-deny allowlist, credential injection for plain HTTP and gate-terminated routes, never inside a CONNECT tunnel)
- `pkg/node` + `pkg/node/inproc` define and implement the execution substrate
- `pkg/controlplane` is the composition root and owns the pools
- `cmd/playscape` is the validation CLI: stdlib `flag`, `run` and `version`, no listener

Full guide: `docs/architecture/overview.md`. Discover layout with `tree` or ripgrep; do not trust stale listings.

## Commands

| Action | Command |
|--------|---------|
| init | `make init` |
| test (both modules, race) | `make test` |
| vet (both modules) | `make vet` |
| lint | `make lint` (golangci-lint, per module) |
| fmt | `gofmt -w .` |
| quality gate | `make check` |
| build the CLI | `make build` (writes `bin/playscape`) |
| validate a command | `go run ./cmd/playscape run -area out 'echo hi > out/x'` |
| examples | `go run ./examples/basic/`; `cd examples/saige && go run .` |

`go test ./...` at the root does not reach `pkg/runner/k8s` or `examples/saige`: each is its own module. Run it from their directories, or use `make test`.

## Code Style

- Idiomatic Go, stdlib-first; every dependency must earn its place. The root module has none; client-go lives only in the `pkg/runner/k8s` module
- Interfaces stay small and live with their concern; implementations may depend on interfaces, never the reverse
- Errors: wrap with `fmt.Errorf("pkg: context: %w", err)`; sentinel errors (`ErrNotFound`, `ErrDenied`, `ErrFull`, `ErrUnknownTask`) for callers to `errors.Is` on
- Package doc comments explain the concern and its orthogonality boundary
- Conventional commits (feat/fix/chore/...); sr cuts releases from them, for both modules

## Rules

- Nothing in the library is a service: no listener, no reading files, environment variables or URLs to decide what to build. Backends are chosen in code. The CLI may read its own flags and a policy file; it must never start a server.
- `pkg/sandbox`, `pkg/ngac`, `pkg/workspace` and `pkg/isolate` (with `isolate/egress`) are a security boundary. A driver never starts an instance weaker than its spec (`ErrUnsupported` instead). Any change near them needs tests proving confinement still holds (traversal, symlink escapes, sibling prefixes, list-leak, spawn gating, prohibition override, multi-policy-class conjunction). Default is always deny.
- Preserve the dependency DAG: nothing under `pkg/` may import `pkg/controlplane`; leaves stay stdlib-only.
- New backends implement existing interfaces (`workspace.Workspace`, `node.Factory`, `runner.Executor`, `metrics.Recorder`); do not widen interfaces for one backend's needs.
- Do not add a durable queue here. Durability is anchor's; the control plane runs tasks in process and says so.
- `examples/saige` is its own Go module (keeps saige's deps out of the root module); it builds in CI.
- `pkg/runner/k8s/go.mod` requires a root release plus `replace ../../..` for development. Before tagging the k8s module, bump that require to the first root release without `pkg/runner/k8s` in it (the one after v0.6.1); a root version that still contains the package makes the import ambiguous.
- This is a beta: keep the surface small, document limitations honestly (README Limitations, architecture caveats) rather than papering over them.

## Extension Guide

- **New CLI-backed tools**: declare `cli.Spec`s (binary, argv template, env allowlist, timeout, output caps) and `registry.RegisterProvider(cli.New(specs...))`. The binary is not confined by the workspace policy; see the architecture caveat.
- **Sandboxed shell**: `runner.Shell(name, k8s.New(clientset, cfg), shellCfg)` registers a tool that runs each call as one Kubernetes Job; apply `pkg/runner/k8s/manifests/rbac.yaml` (and optionally `networkpolicy.yaml`) in the run namespace. New isolation backends implement `runner.Executor`.
- **Isolated runs**: `pool.Lease(ctx, isolate.Spec{...})` per run, `runner.Shell(name, isolate.Executor{Instance: lease}, cfg)`, and `lease.Release(ctx)` at the end. New isolation backends implement `isolate.Driver` and must pass `isolatetest.Run` with `Isolated: true`.
- **New tool**: implement `tool.Tool` (or `tool.Func`), register it, and grant it access in the `ServiceSpec`: a flat `sandbox.Policy`, or an `ngac.Spec` under `Access` for relational grants (user attributes, prohibitions). No grant means no workspace access and no spawning.
- **New workspace backend** (GCS/S3): implement `workspace.Workspace` in `pkg/workspace/<backend>.go`; validate keys with `workspace.ValidKey`.
- **New execution substrate** (containers): implement `node.Factory`/`node.Node`; the control plane needs no changes.
