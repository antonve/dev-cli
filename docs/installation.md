# Installation and repository onboarding

The CLI is a normal Go module, supported on Linux and macOS. Building or
installing it does not require Bazel. Go 1.24 or later is required.

```sh
go install github.com/antonve/dev-cli/cmd/dev@latest
dev version
```

This repository is private. Your Git credentials must already grant read
access, and Go must bypass public module proxies for this module. For example:

```sh
export GOPRIVATE=github.com/antonve/dev-cli
go install github.com/antonve/dev-cli/cmd/dev@latest
```

Include your Go binary directory (`go env GOPATH`, followed by `/bin`, unless
GOBIN is configured) in PATH. Never put a token in a repository URL or commit
credentials. Existing SSH or an authenticated Git credential helper is enough.

## Target repository contract

The target repository needs Git, its pinned Bazel version, kubectl, access to
the configured Kubernetes context/namespace, and a reachable OCI registry.
Frontend pods run the repository's pnpm-managed command; the CLI does not
install or provide a development server. The platform must supply Envoy
Gateway with the Backend extension enabled and an attachable named Gateway.

Create `.dev/config.json` with `kubeContext`, routing `namespace`, `registry`,
`ingressHost`, `ingressClass`, `gatewayName`, `gatewayNamespace`, `cookieName`,
`ttl`, and `metadataQuery`. Optional `namespaces` and `publicHosts` are explicit
allowlists (both default to the legacy singular values). `internalGateway`
defaults to the fully qualified `dev-cli-gateway` Service in the routing
namespace. `bazelArgs` applies to configured builds, cqueries and runs—for
example a repository's pinned Linux platform—but not graph-only queries.
`taskLockNamespace` defaults to the routing namespace. The playground is the
executable reference:
https://github.com/antonve/dev-cli-playground/blob/main/.dev/config.json

Every deployable has one Bazel-owned metadata target attached to the build
inputs it represents. The emitted JSON declares:

| Concern | Fields |
| --- | --- |
| Identity and runtime | `name`, `kind`, container `port`, `readinessPath`, `containerPath`, optional `namespace`, `servicePort` |
| Build and publication | `buildTarget`, `imageTarget`, `imageName`, `pushTarget` |
| Workload | optional `workloadTemplate`, `devContainer`, `baseService` |
| Frontend development | `devCommand`, `sourceRoots`, `syncPaths`, `dependencyPaths`, `dependencyCommand`, optional `syncRoot`, `syncStripPrefix`, `syncExcludes` |
| HTTP routing | `publicPath`/`publicHost`, `internalHost`, optional authenticated `publicProxy` |

Image/push targets are registry-neutral. The push target accepts `--repository`
and `--tag`; the CLI resolves the resulting digest before creating a workload.
Backend runtime images need a POSIX shell, tar, and wget, plus the application
binary at `containerPath`. The CLI supplies its supervisor via ConfigMap.
Frontend images must allow the selected development user to write `syncRoot`
(`/workspace` by default) and run their normal dev/install commands. Backend
overlays without a workload template run as UID 65532.

Declare only intentional sync roots, and include dependency manifests and
lockfiles in both watching and synchronization inputs. Only Git-tracked or
unignored-untracked regular files are eligible. Ignored files, `.env*`, common
generated trees, dependency trees and Bazel outputs are excluded; symlink
escapes fail closed.
Base Services must use the declared deployable names. Applications that need
internal branch routing call the gateway alias with their logical internal
Host and propagate the normalized `x-dev-branch`; direct base-Service calls
bypass branch selection. The playground uses the `dev-cli-gateway` alias.

`workloadTemplate` is a repository-relative JSON `PodTemplateSpec`, not a
Deployment. `devContainer` defaults to `app`. Its pod labels are discarded and
replaced with isolated CLI labels; annotations and pod/container configuration
are preserved. `initContainers` are rejected. Use `${DEV_ROUTE}` and
`${DEV_NAMESPACE}` in environment-specific names or Secret references. The
overlay Service exposes `servicePort` (default `port`) and targets numeric
container `port`, so templates need not call the port `http`.

For a protected API, set both `publicProxy` (the Oathkeeper/authentication
Service reference) and `internalHost`. The public route always targets the
proxy; only the internal normalized-header route selects the API overlay. Do
not use `baseService` as an authentication proxy: it is the API's independent
fallback Service.

## Dependency and task declarations

Dependencies contain `name`, allowed `namespace`, repository-relative JSON
`manifest`, optional `readiness`, and `retention` (`retain`, the default, or
`down`). A manifest is one namespaced object or a Kubernetes `List`; Secret and
cluster-scoped objects are rejected. Each readiness entry supplies `resource`,
token-aware `name`, optional `timeout`, and exactly one of:

```json
{"condition":"Ready"}
{"jsonPath":".status.PostgresClusterStatus","value":"Running"}
```

Tasks contain `name`, `namespace`, JSON Job `manifest`, stable mutation
`target`, optional dependency names, and optional `timeout`. The target must
identify the real shared database/environment consistently across every task;
it is the cross-owner serialization key. A task may also supply `imageName`,
registry-neutral `pushTarget`, and `container` to publish the current checkout
and inject its digest. Otherwise the Job manifest must deliberately pin the
task image; its provenance is independent of the checkout annotation.
Tasks run only when explicitly named. Repeat `--task` in the required order on
`dev up`; any failure blocks that invocation before overlays are published or
created. Ordinary startup never auto-seeds.

Both manifest types support only `${DEV_ROUTE}` and `${DEV_NAMESPACE}` string
substitution. They are not shell templates. Dependency resources are immutable
after creation; change the route/name for a new definition rather than asking
the CLI to reconcile mutable infrastructure.

## Daily commands

```sh
git fetch origin main
dev doctor
dev up --owner alice                    # merge-base with origin/main
dev up --owner alice --service unchanged-api
dev up --owner alice --dependency postgres --task migrate
dev provision --owner alice postgres
dev task --owner alice migrate
dev up --owner alice --base origin/next # alternative comparison base
dev status --owner alice
dev url --owner alice '/settings?tab=profile'
dev url --owner alice --clear '/settings'
dev logs --owner alice                  # all overlay services/namespaces
dev logs --owner alice hello-api        # optional service filter
dev down --owner alice
dev cleanup
```

Open the emitted environment URL, or use `dev url` for a deep link. Envoy sets
the cookie without an application menu or handler. The key includes the owner
so two developers can use the same branch name. See [deep links](deep-links.md).
Ctrl-C stops the foreground loop but retains overlays for inspection. Restart
with the same checkout/owner/branch, or use `dev down` for explicit cleanup.
Stop an older CLI before upgrading; v0.2.0 does not signal legacy PID files.

The affected service set is selected at startup. If development expands to
another service, stop and rerun `dev up` to select the new set. On startup,
workload readiness and HTTPRoute admission are checked; allow DNS/health-check
convergence before asserting which tier handled a request.
