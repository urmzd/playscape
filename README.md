# playscape

> **Moved.** playscape is now `sandbox` in [urmzd/orchagent](https://github.com/urmzd/orchagent). This repository is archived. Its last release only marks the Go module deprecated.

Sandboxed tools, one shared workspace, and NGAC access control for running agent tasks.

## Migrating

```sh
go get github.com/urmzd/orchagent@latest
```

Then rewrite imports:

| Before | After |
|--------|-------|
| `github.com/urmzd/playscape/pkg/<pkg>` | `github.com/urmzd/orchagent/sandbox/<pkg>` |
| `github.com/urmzd/playscape/pkg/runner/k8s` | `github.com/urmzd/orchagent/sandbox/runner/k8s` (its own module) |
| `playscape run` | `go run github.com/urmzd/orchagent/sandbox/cmd/sandbox run` |


The history of this repository is preserved in orchagent under `sandbox/`.
