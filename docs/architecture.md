# Architecture

The CLI has no list of workloads of its own. The repository describes each
deployable with a Bazel `dev_deployable` metadata target, and `.dev/config.yaml`
describes the cluster. Everything below follows from those two inputs.

## Choosing what to deploy

`dev up` compares `HEAD` (including uncommitted and untracked files) with
`merge-base(origin/main, HEAD)`, or with `--base <ref>`. It asks Bazel `rdeps`
which metadata targets the changed files affect. A changed file Bazel doesn't
know about selects every deployable, to be safe.

- `--service <name>` adds a deployable that didn't change.
- Deployables that share a `selectionGroup` are always selected together. An
  empty group means the deployable is selected on its own.
- The selection is fixed at startup. To add a service later, stop and rerun
  `dev up`.

## Images

For each selected deployable, the CLI builds its image target and runs its push
target with a unique route/revision tag. Push targets don't hard-code a
registry; the config's `registry` supplies it. The CLI then resolves the pushed
image to its digest and deploys by digest.

Digest resolution answers the registry's anonymous Bearer challenge, as ghcr.io
requires. If the token is denied, the command fails with "image is not
anonymously pullable" and names the repository: the cluster pulls without a
Secret, so the image must be public to it.

## Overlays

An overlay is the branch's copy of a deployable, running next to the base.

- **Names** are a readable owner/branch prefix plus a SHA-256 suffix. The
  original owner and branch are kept in annotations.
- **Labels** on every resource record owner, route, service, source and base
  revision, timestamps, expiry and CLI version. They are also what cleanup
  matches on.
- **The base is never modified.** The CLI never changes an Argo-owned base
  Deployment.

A deployable can name a `workloadTemplate`: a YAML or JSON `PodTemplateSpec`
with a `devContainer` (default `app`). The CLI keeps the pod spec as written,
including service accounts, volumes, Secret references, resources, working
directory, arguments and probes. It changes only the dev container's image and
runtime fields. Pod labels are replaced so a base Service can never select an
overlay pod. `initContainers` are rejected; use [tasks](#dependencies-and-tasks)
for migrations and seeds. Templates can use `${DEV_ROUTE}`, `${DEV_NAMESPACE}`
and `${DEV_VAR_<NAME>}` ([variables](#profiles-and-variables)).

## Live updates

### Frontends

The pod runs the image once with the repository's own pnpm dev command (Vite,
Next.js, TanStack Start or anything else). The CLI copies changed files into
the pod, and the dev server handles watching and HMR. The CLI ships no
frontend server.

What gets copied:

- only files under the declared `syncPaths`, and only files Git tracks or
  could track (ignored files never leave your machine);
- never `.env*`, dependency trees, Bazel outputs, `.next`, `dist` or coverage;
  `syncExcludes` adds more `filepath.Match` patterns;
- no symlinks that point outside the repository.

`syncRoot` and `syncStripPrefix` map repository paths to the image layout.
Deleted files are removed from the pod; files the CLI didn't put there are left
alone.

`dependencyPaths` (manifests and lockfiles) and `dependencyCommand` (for
example `pnpm install`) handle dependency changes. Include those paths in both
`sourceRoots` and `syncPaths`. The command runs after the first sync, when a
dependency file changes, and when the container is replaced. It does not run
on ordinary source edits.

Failed transfers retry with backoff. Every two seconds the CLI checks the pod
UID and restart count; a replaced container gets the current files without
waiting for another edit. Sync errors and the last successful sync time show
up in `dev status`.

### Backends

Backend images contain only the application. The CLI adds its own small POSIX
supervisor through a ConfigMap. On each edit it:

1. rebuilds the Bazel binary (a compile error leaves the running binary alone);
2. uploads it to an `emptyDir` and compares it with the running binary,
   skipping the restart if it's identical;
3. restarts the process and waits for the readiness URL;
4. if readiness fails, swaps back to the previous binary and checks again.

### Workers

`worker` deployables use the same supervisor and binary updates, with no
Service or route. A worker can declare a private `port` and `readinessPath`
for an HTTP readiness probe. Without them, readiness just means the process is
still running. A workload template can supply a stronger probe.

### Expiry

While `dev up` runs, it renews the environment's expiry every 30 seconds (or a
third of the TTL, if shorter). Renewal runs separately from syncing and
building, so a slow cluster never pauses live edits. Once `dev up` stops, the
last renewed expiry stands.

## Routing

Routing requires Envoy Gateway with its Backend extension enabled
(`config.envoyGateway.extensionApis.enableBackend: true`) and a named Gateway.
Health-based failover depends on Envoy Gateway, so other Gateway API
implementations won't work.

### Branch hostnames

Each public deployable gets `https://<route>.<publicHost>/`. The hostname is
the only thing that selects a branch; see [branch links](deep-links.md).
`hostTLS` decides where TLS comes from:

| `hostTLS` | What the CLI creates | Needs |
| --- | --- | --- |
| `certificate` (default) | One host-only HTTPRoute per public deployable, plus one Ingress covering every branch host, with a cert-manager certificate. | `clusterIssuer`; cert-manager running with `--enable-certificate-owner-ref=true` so deleting the Ingress also deletes its Certificate and Secret. |
| `gateway` | Host routes attached straight to the public Gateway. No Ingress or certificate. | A wildcard listener on the Gateway that terminates TLS and admits routes from the routing namespace. |

With `certificate`, startup waits up to 120 seconds for the certificate and
fails with a `kubectl describe` hint if it isn't ready. `dev down` and expiry
delete the Ingress.

Routes for `internalHost` attach to `internalGatewayName` and
`internalGatewayNamespace`, which default to the public Gateway.

### Public, internal and proxied routes

- **Public routes** match the branch hostname and set the `x-dev-branch`
  header to the route key, overwriting anything the browser sent. The
  platform's base routes must strip that header from untrusted requests.
- **Internal routes** match the `x-dev-branch` header. To keep a call on the
  branch, an application calls the gateway alias (`dev-cli-gateway`) with the
  logical internal Host and passes the header on. Calling a base Service
  directly bypasses branch selection.
- **Proxied routes.** When a deployable sets `publicProxy` (for example
  Oathkeeper), its public route always goes to that proxy, and the overlay is
  reachable only through the `internalHost` route. Authentication stays in
  front of both branch and base traffic.

### Failover to the base

Each route picks its backend on its own. An unchanged deployable points
straight at its base Service. A changed one gets two Envoy Backends: the
branch Service first, the base Service as fallback.

A BackendTrafficPolicy checks the readiness path every second with a
three-second timeout. Three failures in a row switch to the base; one success
switches back. A slow first server-rendered response doesn't trigger
failover, and a hung branch fails over within about twelve seconds. This also
works when your local CLI isn't running.

Both Backends use namespace-local Service DNS names, so deleting the branch
Service doesn't break the HTTPRoute. Envoy re-resolves these names every
second instead of using the cluster DNS TTL, so a recreated Service never
leaves a stale address.

Failover takes time to settle, and some requests may fail while it does.

### Startup check

Before printing links, `dev up` waits until each HTTPRoute is Accepted with
ResolvedRefs. It then requests
`https://<route>.<publicHost><readinessPath>` from your machine (the
`publicPath` instead, if the readiness path lies outside it) without
following redirects. It needs three responses in a row below HTTP 500 whose
`X-Dev-Backend` is the branch Service.

After three minutes without that, `up` fails. The error shows the URL, the
expected backend, and the last response it got. Nothing is torn down.

Workers, internal-only routes and `publicProxy` deployables aren't checked:
your machine can't reach the internal gateway, and a proxy decides for itself
which paths reach the app. Failover still applies to them.

### Response headers

Routes the CLI creates add these headers using Envoy's own
`ResponseHeaderModifier`. The application needs no changes:

| Header | Meaning |
| --- | --- |
| `X-Dev-Selected` | The route key, or `base`. This shows which branch the request asked for, not where it went. |
| `X-Dev-Backend` | The upstream Envoy actually chose, from [`%UPSTREAM_HOST_NAME%`](https://www.envoyproxy.io/docs/envoy/v1.39.0/configuration/advanced/substitution_formatter). |
| `X-Dev-Proxy-Backend` | On a `publicProxy` route: the proxy's upstream. Any `X-Dev-Backend` from the inner hop is passed through. |

Branch hosts also send `Cache-Control: no-store` and
`X-Robots-Tag: noindex, nofollow`.

Reading `X-Dev-Backend`:

- For changed deployables it is the branch or base Service name, so it tells
  you which one failover picked.
- For a plain Service reference it may be a pod `IP:port`. Look at the
  endpoints to identify it.
- On an error it may name an upstream that was tried but didn't answer, or be
  missing. Always check the status code too.
- A missing header means **unknown**, not base. Auth rejections and calls
  through base routes the CLI doesn't own carry no header.

To see the headers, open the browser's Network panel or run
`curl -D - -o /dev/null '<environment URL>'`. Environments created by an older
CLI get the headers after you rerun `dev up`. These headers are for debugging.
They are not a security guarantee or a full trace.

## Dependencies and tasks

`.dev/config.yaml` can declare **dependencies** (for example a branch
database) and **tasks** (Jobs such as migrations or seeds). Both run only when
you ask for them, through `dev provision`, `dev task`,
`dev up --dependency … --task …` or [hooks](#lifecycle-hooks).
`dev up --dependency db --task migrate` provisions, waits for readiness, then
runs tasks in flag order. A failed task stops `up` before any overlay is
published.

Dependency rules:

- Objects must be namespaced, in an allowed namespace. Secrets and
  cluster-scoped objects are rejected; a server dry-run confirms the scope.
- Dependencies are immutable. Creation is atomic and records a manifest hash,
  so reprovisioning the same manifest is a no-op and two owners can't take
  over each other's objects. To change one, use a new name or route.
- Readiness is a `condition`, or a `jsonPath` with a `value`.
- `retention: retain` (the default) survives `dev down` and expiry.
  `retention: down` is deleted on teardown, after ownership and the stored
  retention are checked. Changing the config later can't make retained data
  deletable.

Task rules:

- A task is exactly one Job. Its Secret references are kept.
- `target` names what the task changes, such as a database. One Lease per
  target, in `taskLockNamespace`, makes sure only one task runs against it at
  a time, across task names, owners and namespaces.
- The Lease is released only when the Job reaches Complete or Failed. If a
  run is interrupted, the next run takes over only once that exact Job has
  finished. An expired Lease or a missing Job isn't enough, because the old
  client may only be paused and could still create the Job. See
  [troubleshooting](troubleshooting.md) for manual recovery.
- `imageName`, `pushTarget` and `container` publish the current checkout and
  inject its digest into that container (regular or init). Without them, the
  Job's manifest must pin its own image.

## Lifecycle hooks

Hooks run declared tasks at startup and teardown:

```yaml
hooks:
  beforeUp: [tenant]
  afterDown: [tenant-teardown]
  deployables:
    worker:
      beforeStart: [worker-override-set]
      afterStop: [worker-override-clear]
```

Hook Jobs follow the same rules as `dev task`. Unknown tasks, deployables or
config keys fail before startup. **Hooks must be idempotent**, because retries
can run any hook more than once.

Startup order:

1. Clean up expired routes and select deployables. With nothing selected,
   stop here.
2. Write the route's lifecycle marker, a ConfigMap in the routing namespace.
   From here on the route can always be cleaned up, even if startup fails.
   Then publish the images of the teardown tasks (`afterStop`, `afterDown`)
   and record in the marker everything teardown needs (see
   [headless teardown](#headless-teardown)).
3. Provision `--dependency` dependencies.
4. Run `beforeUp`, then `--task` tasks.
5. Publish images.
6. For each deployable, run `beforeStart`, then create its overlay.
7. Set up routing, do the first sync, and start watching.

A failed hook stops everything after it. Expiry renewal starts once the
marker exists, so a long hook or build can't let the route expire.

Teardown (`dev down`, cleanup of an expired route, or their `--headless`
forms):

1. Stop the local `dev up` for that route. Headless teardown has no checkout
   and skips this.
2. Save the list of deployed services in the marker, then delete overlays and
   wait for their pods to go away.
3. Run each service's `afterStop`, then `afterDown`.
4. Delete `retention: down` dependencies and task Jobs.
5. Delete the marker, last.

If a step fails, the marker stays and a retry continues from where it
stopped, without deleting overlays twice. A checkout that doesn't declare a
recorded task skips that route with a warning and touches nothing. Routes from
CLIs older than hooks have no marker and are simply deleted.

### Headless teardown

`dev down --headless` and `dev cleanup --headless` tear routes down from their
markers alone, for example from a Kubernetes Job that has cluster credentials
but no checkout, `.dev/config` or Bazel:

```sh
dev down --headless --namespace <routing-namespace> --route <route> [--context <kube-context>]
dev cleanup --headless --namespace <routing-namespace> [--context <kube-context>]
```

They run the same teardown steps and checks, under the owner recorded in the
marker; `--owner` and `--config` are refused. Without `--context`, kubectl
uses the in-cluster or current context. `cleanup --headless` finds expired
routes in the routing namespace, also honours a later expiry in any recorded
namespace, and tears down only routes with a complete marker.

The marker's `hooks.json` holds, besides the profile, variables, hooks and
deployed services, a `teardown` record written by `dev up`:

| Field | Contents |
| --- | --- |
| `namespaces` | The workload namespaces (`namespaces` in the config) |
| `taskLockNamespace` | Where task Leases live |
| `tasks.<name>` | Each `afterStop`/`afterDown` task: `namespace`, resolved `target`, `timeout`, `dependencies`, the published `image` digest, and the `job` manifest with variables substituted and that digest injected |
| `dependencies` | `retention: down` dependencies and those teardown tasks use: `name`, `namespace`, `retention`, `readiness`, and the rendered `objects` |

Headless teardown refuses, and leaves untouched, a route with no marker or a
marker from an older CLI without this record; tear those down from their
checkout. A marker over 768 KiB is refused at `dev up`, keeping it well under
the 1 MiB ConfigMap limit. A running local `dev up` is not stopped; its next
sync fails once the overlays are gone.

The headless Job's ServiceAccount needs, in every recorded namespace: `get`,
`list`, `watch` and `delete` on Deployments, Services, ConfigMaps, HTTPRoutes,
Envoy Gateway Backends and BackendTrafficPolicies, and Ingresses; `get`,
`list` and `watch` on Pods; and `get`, `list`, `watch`, `create` and `delete`
on Jobs. In the routing namespace it also needs `create` and `update` on
ConfigMaps (the marker), and in `taskLockNamespace` `get`, `create` and
`update` on Leases. `retention: down` dependency kinds need `get` and
`delete`, plus `create` and readiness `watch` if a teardown task depends on
them.

The repository's `Dockerfile` builds a minimal image: a static `dev`, a
kubectl pinned by digest, no shell, running as a non-root user.

Push a single-platform image manifest, not an index with attestations: some
registries keep an index but not the manifests it points to, which makes the
image impossible to pull.

```sh
tag=registry.dev.example/dev-cli/dev:$(git rev-parse HEAD)
docker buildx build --provenance=false --sbom=false --platform linux/amd64 \
  --output "type=registry,name=$tag" .
docker buildx imagetools inspect "$tag"   # prints the digest to deploy
```

## Profiles and variables

```yaml
variables:
  DATABASE: tadoku
profiles:
  - name: isolated-database
    whenChanged: [services/api/migrations/]
    variables:
      DATABASE: tadoku-${DEV_ROUTE}
    hooks:
      beforeUp: [migrate, tenant]
```

**Variables** have uppercase names (`^[A-Z][A-Z0-9_]*$`). Workload templates,
task Jobs, dependency manifests and task targets use them as
`${DEV_VAR_DATABASE}`. Values can contain `${DEV_ROUTE}` and
`${DEV_NAMESPACE}` (the routing namespace) but not other variables. A
`${DEV_NAMESPACE}` written directly in a manifest means that manifest's own
namespace. An unknown variable fails before the resource is written. Values
are stored as plain ConfigMap data, so use Secret references for credentials.

**Profiles** are checked in order. The first one with a `whenChanged` prefix
matching a changed path wins (committed, uncommitted or untracked). If none
match, the profile is `default`, which is a reserved name. A profile overrides
only the variables and hook keys it lists. An empty hook list turns that hook
off.

The marker stores the profile and resolved variables:

- Startup prints `profile=<name>`, and `dev status` shows it.
- A running route can't switch profile or variable values. Startup fails with
  `run dev down first`, without printing the values.
- `dev task`, `dev provision` and teardown use the stored values, even from a
  checkout whose config differs.

A task `target` such as `database/${DEV_ROUTE}` gives each route its own
Lease. Every lifecycle task for a route must resolve to the same target.
Dependency readiness names only substitute `${DEV_ROUTE}`.

## Environment overrides

A config file kept outside the repository can point the same deployable
metadata at a different environment:

```yaml
manifestsRelativeTo: config      # load templates, tasks and dependencies next to this file
hostTLS: gateway
gatewayName: edge
gatewayNamespace: gateway
internalGatewayName: internal
internalGatewayNamespace: routing
deployables:
  api:
    namespace: prod-api
    workloadTemplate: api.yaml
    baseService: {name: api, namespace: prod-api, port: 80}
    publicProxy: {name: oathkeeper-proxy, namespace: prod-auth, port: 4455}
    internalHost: api-internal.prod-auth.svc.cluster.local
    publicHost: preview.example.com
```

An override replaces only the fields it sets: `namespace`,
`workloadTemplate`, `baseService`, `publicProxy`, `internalHost` and
`publicHost`. Naming a deployable that the metadata doesn't have is an error.
With `manifestsRelativeTo: config`, files resolve relative to the config file,
with the same symlink-escape check. Git and Bazel still run in the checkout.

## Production mode

`mode: production` deploys a pushed commit as release images, by digest, next
to an existing environment's base workloads:

```yaml
mode: production
registry: ghcr.io/org/repo/branches     # must end in /branches
bazelArgs: [--config=release]           # required
refuseChangedPaths: [services/api/migrations/]
hostTLS: gateway
manifestsRelativeTo: config
deployables:                            # every deployable needs a template
  api: {workloadTemplate: api.yaml}
```

Each deployable's metadata must declare `release: {imageName, pushTarget}`,
the repository's release entrypoint without a fixed repository or tags. The
development `imageName`, `pushTarget`, `devCommand`, sync and dependency
fields are ignored. Because every deployable needs a template override, no
development template can reach production.

**Before `up`, `task` or `provision` does anything,** the CLI refuses:

- a dirty working tree;
- a `HEAD` that no fetched `origin` branch contains;
- changes under any `refuseChangedPaths` prefix;
- a routing namespace without the labels `dev-cli.io/routing-enabled=true`
  and `dev-cli.io/environment=production`.

Development configs, in turn, refuse a namespace labelled production.

**Every push**, including task images, must:

- use the tag `<route>-<commit12>` (never `latest` or `prod`);
- push one image directly under the configured registry;
- use a push target whose `bazel query --output=build` sets neither
  `repository` nor `remote_tags`. Otherwise a rules_oci `oci_push` with
  `remote_tags` would still push `latest` and `prod`.

`up` first looks for an existing `<registry>/<release.imageName>:<route>-<commit12>`
and reuses it, since it names a clean, pushed commit. Otherwise it pushes and
resolves the digest anonymously, which proves the cluster can pull it without
a Secret.

Overlays keep the template's command, arguments, probes, resources and
security context. Only the image, labels and annotations change. There is no
supervisor, sync, expiry renewal or HMR. `up` waits for every rollout and
route, prints the route, commit, digests, branch URLs and expiry, then exits.
Expiry is startup time plus `ttl`; rerun `up` to extend it. Teardown uses the
same hooks as development. `dev doctor` also checks the namespace labels and
Docker.

## Local process and cleanup

**One `dev up` per checkout and route.** On Linux and macOS an exclusive file
lock enforces this before anything is published. A private Unix socket
handles `status` and graceful stop. `dev down` asks the running `dev up` to
stop and waits for it; it never signals a stored PID. After a crash, the
kernel releases the lock and the next run reclaims the socket. Stop older
CLIs before upgrading, since their PID files are ignored. Checkouts on
different machines need different owners if they should run independently.

**`dev down`** deletes only objects that match both your owner and route
labels, including Backends and BackendTrafficPolicies.

**Expiry cleanup** runs when `dev up` starts, when `dev status` runs, and on
`dev cleanup`. Nothing runs it in the background. It looks at every owned
resource kind in every configured namespace, so routing objects left behind
by a deleted Deployment are still found, and it uses the latest expiry seen
for a route. Expired routes with a marker get the full
[teardown](#lifecycle-hooks) under their original owner.

`dev cleanup --headless` removes expired routes from their markers alone; see
[headless teardown](#headless-teardown).

`dev status` never runs hooks. It leaves expired routes that have a marker in
place and reports `teardownPending`.
