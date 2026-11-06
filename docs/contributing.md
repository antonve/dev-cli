# Contributing

This repository holds the reusable CLI only. Application code, its Bazel
graph, images, deployable metadata and conformance tests live in the
application repository. Installing the gateway is the platform's job. Don't
add Bazel or app-specific servers here.

```sh
go test -count=1 -race ./...
go vet ./...
go install ./cmd/dev
```

## Code layout

| Package | Responsibility |
| --- | --- |
| `app` | Commands and the `up` lifecycle |
| `deeplink` | Branch URLs |
| `gitx` | Git discovery |
| `bazel` | Graph queries and builds |
| `registry` | Publishing and digest resolution |
| `kube` | Kubernetes resources and runtime operations |
| `syncer` | File sync and build loops |
| `config`, `naming`, `localstate` | Configuration, resource names, local process ownership |

External commands go through `execx.Runner`, so tests can fake their output.
Tests use real temporary files and processes for filesystem and socket
behaviour. Add a regression test when you fix a lifecycle bug.

## Rules that must hold

- Never modify an Argo base Deployment to create an overlay.
- Route, owner, service and management labels define what cleanup may
  delete. Keep them accurate.
- An upload must not be visible until it has fully transferred.
- A failed build or readiness check must leave the last working backend
  running.
- Never report local shutdown as done before the work has actually stopped.

## Releasing

These gates run locally; no Actions runner is needed. (The playground has its
own GitHub-hosted CI.)

1. Run the playground's full Bazel, pnpm and image gates, plus live branch
   conformance.
2. Check by hand: browser HMR without a page reload, backend restart and
   readiness recovery, two branch hosts in one browser profile, internal hops,
   rerunning `dev up`, fallback after deletion, TTL cleanup, installing from a
   clean checkout, and a healthy Argo base.
3. Tag the release.
4. Test the published `@latest` install separately.
