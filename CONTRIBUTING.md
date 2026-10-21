# Contributing

## Prerequisites

| Requirement | Version |
|-------------|---------|
| Go | per `go.mod` |
| golangci-lint | latest |
| make | any |

## Getting Started

```sh
git clone https://github.com/urmzd/playscape
cd playscape
make init
```

## Development

```sh
make check   # fmt + vet + lint + test (quality gate)
make test    # go test -race in the root module and pkg/runner/k8s
make fmt     # gofmt -w .
make build   # build the validation CLI into bin/playscape
```

The repository holds two modules: the root library and `pkg/runner/k8s` (kept apart so importers do not inherit client-go). `go test ./...` at the root does not reach the second; `make test` runs both.

The sandbox (`pkg/sandbox`), the policy machine (`pkg/ngac`) and the workspace (`pkg/workspace`) are a security boundary: changes touching them must include tests that prove confinement still holds.

## Commit Convention

Conventional commits (Angular style): `feat:`, `fix:`, `docs:`, `chore:`, etc. Releases are cut automatically from commit types by [sr](https://github.com/urmzd/sr).

## Pull Requests

Fork, branch from `main`, make your change, ensure `make check` passes, and open a PR. CI must be green before review.

## Code Style

Idiomatic Go. Small interfaces, one concern per package, errors wrapped with context and sentinel errors for callers to match on. See AGENTS.md for the full rules.
