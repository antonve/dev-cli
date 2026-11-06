# Installation and onboarding

## Install the CLI

You need Go 1.24 or later on Linux or macOS. Bazel is not needed for the CLI
itself.

The module is private, so Go must skip the public module proxy for it, and
your Git credentials need read access:

```sh
export GOPRIVATE=github.com/antonve/dev-cli
go install github.com/antonve/dev-cli/cmd/dev@latest
dev version
```

If GitHub access only works over SSH, rewrite the URL for this one command:

```sh
GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=url.git@github.com:.insteadOf \
GIT_CONFIG_VALUE_0=https://github.com/ GOPRIVATE=github.com/antonve/dev-cli \
go install github.com/antonve/dev-cli/cmd/dev@latest
```

Make sure `$(go env GOPATH)/bin` (or `GOBIN`) is on your `PATH`, and check
`command -v dev`: an older binary earlier on `PATH` can hide the new one. To
install somewhere specific, add `GOBIN="$HOME/.local/bin"` to the install
command. Never put a token in a repository URL.

**Before upgrading**, stop your own running `dev up` loops (not anyone
else's).

## Prepare a repository

### What the repository needs

- Git, the repository's pinned Bazel, and `kubectl` with access to the target
  context and namespaces.
- A reachable OCI registry.
- From the platform: Envoy Gateway with the Backend extension enabled, and a
  named Gateway that routes can attach to.

### `.dev/config.yaml`

The CLI reads `.dev/config.yaml`, or `.dev/config.json` for older
repositories. If both exist, you must pick one with `--config <path>`. An
explicit path can have any filename.

```yaml
kubeContext: homelab-dev
clusterIssuer: lab-ca-acme
namespace: dev-cli-playground
registry: registry.dev.lab/dev-cli-playground
ingressHost: dev-cli-playground.dev.lab
gatewayNamespace: envoy-gateway-system
```

Required: `kubeContext`, `namespace` (the routing namespace) and `registry`,
plus `clusterIssuer` unless `hostTLS: gateway`.

| Field | Default |
| --- | --- |
| `ingressHost` | none; the primary public host |
| `publicHosts` | `[ingressHost]`; the allowed public hosts |
| `namespaces` | `[namespace]`; the allowed workload namespaces |
| `ingressClass` | `nginx` |
| `gatewayName` | `dev-cli-playground` |
| `gatewayNamespace` | `namespace` |
| `internalGateway` | `http://dev-cli-gateway.<namespace>.svc.cluster.local` |
| `taskLockNamespace` | `namespace` |
| `ttl` | `8h` |
| `metadataQuery` | `kind(dev_deployable, //...)` |
| `bazelArgs` | none; applied to builds, cqueries and runs (for example a pinned Linux platform), not to graph queries |

Unknown keys are rejected. Other options: [dependencies and
tasks](architecture.md#dependencies-and-tasks), [hooks](architecture.md#lifecycle-hooks),
[profiles and variables](architecture.md#profiles-and-variables),
[environment overrides](architecture.md#environment-overrides) and
[production mode](architecture.md#production-mode). The
[playground config](https://github.com/antonve/dev-cli-playground/blob/main/.dev/config.json)
is a working example.

The config, workload templates, dependencies and task Jobs all accept YAML or
JSON. Each file holds exactly one object; duplicate keys and multi-document
YAML are rejected (use a Kubernetes `List` for several dependency objects).
Quote values like `"on"`, `"off"`, `"yes"`, `"no"` and numbers that must stay
strings, since YAML would otherwise convert them.

### Deployable metadata

Each deployable has one Bazel `dev_deployable` target, attached to the build
inputs it represents. Its JSON output declares:

| Concern | Fields |
| --- | --- |
| Identity and runtime | `name`, `kind` (`frontend`, `backend` or `worker`), `containerPath`, optional `namespace` and `selectionGroup`. HTTP deployables also need `port` and `readinessPath`, and may set `servicePort`. |
| Build and publication | `buildTarget`, `imageTarget`, `imageName`, `pushTarget` |
| Workload | optional `workloadTemplate`, `devContainer`, `baseService` |
| Frontend sync | `devCommand`, `sourceRoots`, `syncPaths`, `dependencyPaths`, `dependencyCommand`, optional `syncRoot`, `syncStripPrefix`, `syncExcludes` |
| HTTP routing | `publicPath`/`publicHost`, `internalHost`, optional `publicProxy` |

**Push targets** don't hard-code a registry. They must accept `--repository`
and `--tag`.

**Image requirements:**

- Backends: a POSIX shell, `tar`, `cmp`, `wget`, and the binary at
  `containerPath`. Without a workload template they run as UID 65532.
- Workers: the same, minus `wget` unless they declare a `port` and
  `readinessPath`.
- Frontends: the dev user must be able to write to `syncRoot` (default
  `/workspace`) and run the dev and install commands.

**Sync paths.** Declare only what should be copied, and list dependency
manifests and lockfiles in both `sourceRoots` and `syncPaths`. See
[live updates](architecture.md#frontends) for what is never copied.

**Base Services** default to the deployable's name. Set `baseService` if the
name, namespace or port differs.

**Workload templates** are a `PodTemplateSpec`, not a Deployment; see
[overlays](architecture.md#overlays). The overlay Service exposes
`servicePort` (default `port`) and targets the container's numeric `port`, so
the port doesn't need to be called `http`.

**Selection groups.** Give deployables that must run together the same
`selectionGroup`, for example an API and its worker. Changing or naming
either starts both. A worker shows up in `dev status`, `dev logs` and
`dev down` under its own name, and has no URL.

**Protected APIs.** Set both `publicProxy` (the authentication proxy's
Service, such as Oathkeeper) and `internalHost`. Public traffic always goes
through the proxy, and only the internal route reaches the branch API. Don't
use `baseService` for the proxy: it's the API's fallback.

**Internal calls.** For a service-to-service call to stay on the branch, the
application must call the gateway alias (`dev-cli-gateway` in the
playground) with its logical internal Host and pass on the `x-dev-branch`
header. Calling a base Service directly skips the branch.

### Dependencies and tasks

A dependency has a `name`, an allowed `namespace`, a `manifest` file, optional
`readiness` and `retention` (`retain`, the default, or `down`). The manifest is
one namespaced object or a `List`. Each readiness entry has `resource`, `name`,
optional `timeout`, and one of:

```json
{"condition":"Ready"}
{"jsonPath":".status.PostgresClusterStatus","value":"Running"}
```

A task has a `name`, `namespace`, a Job `manifest`, a `target`, optional
`dependencies` and an optional `timeout`. The `target` must name the real
thing the task changes (for example the shared database), the same way in
every task; it's the lock key. To build the task's image from the checkout,
set `imageName`, `pushTarget` and `container`. Otherwise the Job must pin its
image.

Manifests and task targets support `${DEV_ROUTE}`, `${DEV_NAMESPACE}` and
`${DEV_VAR_<NAME>}`. This is plain string substitution, not shell templating.
Dependencies can't be changed after creation; use a new name or route instead.

## Daily use

```sh
git fetch origin main
dev doctor
dev up --owner alice
dev url --owner alice '/settings?tab=profile'
dev status --owner alice
dev logs --owner alice [service]
dev down --owner alice
```

Open the branch URL printed by `dev up`, or make a deep link with `dev url`.
The route includes the owner, so two people can use the same branch name.
Ctrl-C stops watching but keeps the environment for inspection; rerun from the
same checkout to resume, or `dev down` to remove it.

Services are chosen when `dev up` starts. If your work spreads to another
service, rerun `dev up`. Startup checks that direct public routes answer from
the branch. For proxied or internal routes, check `X-Dev-Backend` yourself
before trusting which side served a request.
