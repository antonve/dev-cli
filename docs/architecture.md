# Architecture and repository contract

The CLI deliberately has no workload catalog. A Bazel repository defines one
`dev_deployable` metadata target per deployable and connects it to the build
graph it represents. The emitted JSON names logical application image and push
targets, runtime kind, readiness path, development command, routes, build
target, and sync roots. Registry locations remain environment configuration.
Deployables sharing a `selectionGroup` are selected together when any one is
affected or explicitly named. An empty group leaves selection independent.

`dev up` computes `merge-base(origin/main, HEAD)` by default, asks Bazel `rdeps`
which metadata targets are affected, builds only their image targets, and runs
their registry-neutral push targets with a collision-resistant route/revision
tag. It resolves each published manifest and injects the immutable digest into
the overlay Deployment. An unmapped changed file fails closed by selecting all
deployables.

Names contain a readable owner/branch prefix plus a SHA-256-derived suffix.
Original owner and branch values are retained in annotations. Every temporary
resource carries owner, route, service, source/base revision, timestamps,
expiry, and CLI version. The CLI never mutates an Argo-owned base Deployment.

Configuration `deployables` overrides replace a deployable's namespace,
workload template, base Service, public proxy, internal host and public host
before validation, so a configuration outside the repository can target
another environment. With `manifestsRelativeTo: config`, templates, tasks and
dependencies resolve inside the configuration file's directory instead of the
checkout.

Deployables may name a repository-relative YAML or JSON `workloadTemplate` containing a
Kubernetes `PodTemplateSpec` and a `devContainer`. The CLI preserves its pod
spec—including service accounts, projected volumes, Secret references,
resources, working directory, arguments and probes—then changes the designated
container's image/runtime fields. It replaces all pod labels with isolated
selector labels, so a base Service cannot select an overlay. Template
`initContainers` are rejected: migrations and seeds use declared tasks instead.
`${DEV_ROUTE}`, `${DEV_NAMESPACE}` and declared `${DEV_VAR_<NAME>}` values
are substituted; see Profiles and variables.

## Production mode

`mode: production` replaces the development runtime with release images. The
configuration must publish to a registry path ending in `/branches`, build with
`--config=release`, list `refuseChangedPaths`, and override every deployable's
`workloadTemplate`, so no development template can reach production. Metadata
supplies `release.imageName` and `release.pushTarget`; the development image,
push target, command, sync and dependency fields are ignored.

A preflight runs before any build or write for `up`, `task` and `provision`:
clean working tree, HEAD contained in a fetched `origin` branch, no change under
a refused prefix, and a routing namespace labelled for production. One push
guard covers overlays and task images. It allows only `<route>-<commit12>`
tags, one image name directly under the configured registry, and push targets
whose `bazel query --output=build` definition sets neither `repository` nor
`remote_tags`; a release target such as rules_oci `oci_push` with
`remote_tags` would otherwise still apply `latest` and `prod`.

An existing branch tag is reused because it names a clean, pushed commit. The
anonymous digest resolution after a push proves the cluster can pull the image
without a pull Secret. Overlays apply only the digest, labels and annotations to
the template and wait for rollout status for every kind. `up` never starts the
heartbeat or watch loop; expiry is creation plus `ttl`, renewed by rerunning
`up`, and teardown uses the same lifecycle hooks as development.

## Runtime ownership

Frontend metadata declares a normal repository-owned pnpm dev command. The CLI
starts the application image once with that command and copies changed source
files directly into its writable workspace. Vite, Next.js, TanStack Start, or
another repository-selected dev server owns filesystem watching and HMR; the
CLI does not ship a frontend server.

Only declared `syncPaths` are copied. Git's tracked plus unignored-untracked set
is the upper bound, so ignored private/generated files are not transferred.
The CLI also excludes `.env*`, dependency trees, Bazel outputs, `.next`, `dist`,
and coverage by default; `syncExcludes` adds repository-relative
`filepath.Match` patterns. Symlink escapes are rejected. `syncRoot` and
`syncStripPrefix` map repository paths to the image layout. Deleted files are
removed using the CLI's prior file manifest; unrelated files are retained.
`dependencyPaths` identify manifests and lockfiles, and `dependencyCommand`
declares the repository's install command. Include those paths in both
`sourceRoots` and `syncPaths`. The command runs after initial sync, changes to
those dependency inputs, or container replacement—not on ordinary source edits.

Watch retries unsuccessful transfers with a bounded backoff and checks pod UID
and app restart count every two seconds. A replacement runtime receives the
current local contents even if no further file is edited. Sync errors and last
successful sync time are recorded on the owned Deployment for `dev status`.
TTL heartbeats run independently of source polling and builds, so slow cluster
bookkeeping cannot pause live edits. Shutdown cancels and joins the heartbeat
before acknowledging `dev down`; reporting remains on the main watch loop.

Backend images contain only the application. The CLI injects its own versioned
POSIX supervisor in a ConfigMap, stages a successfully rebuilt Bazel binary in
an `emptyDir`, signals the process, and waits for the declared readiness URL.
Compile failures never touch the running binary. A readiness failure triggers a
swap back to the previous binary and a second readiness check.
The watch loop compares the rebuilt binary with the running Pod's current
binary after a staged upload. Identical content keeps its process running even
when a watched source file changes. Comparing inside the Pod also handles a
reused overlay whose running binary differs from its base image.

`worker` uses the same supervisor and binary update flow without a Service or
route. It may declare a private `port` and `readinessPath` pair for an HTTP
Pod probe and binary replacement check. Without that pair, its default
readiness probe and replacement check require the same child process to stay
alive. A repository can supply a stronger application-specific readiness probe
in its workload template.

## Routing ownership

The required `clusterIssuer` lets the CLI own one host-only HTTPRoute per public deployable
and one Ingress covering every `<route>.<publicHost>`. Host routes set the
trusted branch header and retain authentication proxy or active/base backend
references. Health policies include direct host routes. The Ingress
forwards to the routing namespace's `dev-cli-gateway` Service and requests TLS
through cert-manager. Down/expiry remove the owned Ingress. cert-manager must
run with `--enable-certificate-owner-ref=true`, so deleting the Ingress removes
its Certificate and Secret. Startup waits for certificate creation and readiness
within one 120-second deadline and fails with a describe hint if TLS is not
ready. Doctor checks the
issuer, gateway Service and Ingress/certificate permissions.

With `hostTLS: gateway`, the CLI creates neither the Ingress nor a
certificate. Host routes attach directly to the configured public Gateway,
whose wildcard listener terminates TLS and must admit routes from the routing
namespace; `clusterIssuer` is then optional. Routes for `internalHost` attach
to `internalGatewayName`/`internalGatewayNamespace`, which default to the
public Gateway.

Browser selection uses [gateway-owned deep links](deep-links.md): `dev up` and
`dev status` expose URLs, while `dev url` preserves a supplied deep destination.
Only the branch hostname selects an environment.
Guest frontends need no dev-specific code.

The current routing adapter requires Envoy Gateway with its Backend extension
enabled (`config.envoyGateway.extensionApis.enableBackend: true`) and a named
Gateway. It is not portable to every Gateway API implementation: health-based
failover depends on this provider. The
base application declares ordinary HTTPRoutes. Deployables independently name
their workload namespace, public host, overlay Service port, and base Service
name/namespace/port. A public route matches
the branch hostname and replaces `x-dev-branch` with the route key; the platform's
base routes remove an untrusted browser header. An internal route matches the
normalized header propagated by an application. When `publicProxy` is set, the
public route always targets that authentication proxy and the overlay is
reachable only through the separate `internalHost` route. This keeps
Oathkeeper or another existing boundary in front of both selected and base
traffic. The proxy/application remains responsible for propagating normalized
context internally; a browser header cannot override the public route's value.

Each route chooses its backend independently. Unaffected deployables reference
the base Service directly. Affected deployables use two Envoy Backend resources:
the branch Service as the active tier and the base Service as the fallback tier.
Both reference explicit namespace-local Service DNS names, so deleting the
branch Service does not invalidate the HTTPRoute's object references. A
BackendTrafficPolicy checks the declared readiness path every second. Envoy
refreshes these temporary Service DNS records every second without retaining
the cluster DNS TTL, so a recreated Service does not leave a stale address for
the default thirty-second refresh interval. Envoy
selects the base tier when the branch tier is unavailable, even if the local CLI
is stopped. Failover is eventual, not a zero-error guarantee: health checks,
DNS, and data-plane configuration take time to converge.

The CLI waits for current-generation HTTPRoute Accepted and ResolvedRefs
conditions before declaring startup complete. Route admission is reported
separately from workload readiness; it is not proof that an individual request
used the overlay. The response headers below provide per-request gateway evidence.

### Response provenance

CLI-owned routes use native Envoy `ResponseHeaderModifier` filters, without
application changes or gateway-wide patches:

| Header | Meaning |
| --- | --- |
| `X-Dev-Selected` | Normalized environment route key, or `base` for a clear link. Selection intent, **not** proof of overlay use. |
| `X-Dev-Backend` | Envoy's actual selected upstream hostname, or IP:port when no DNS name is available. |
| `X-Dev-Proxy-Backend` | On a `publicProxy` route, the outer authentication proxy's upstream instead; any internal `X-Dev-Backend` is preserved. |

Branch-host responses also set `Cache-Control: no-store` and
`X-Robots-Tag: noindex, nofollow`.

The backend value uses Envoy's native
[`%UPSTREAM_HOST_NAME%` formatter](https://www.envoyproxy.io/docs/envoy/v1.39.0/configuration/advanced/substitution_formatter).
For active/fallback DNS Backends this identifies the overlay or base Service
actually selected by health-based routing, not merely the matched HTTPRoute.
Ordinary Service references may yield a pod IP:port instead: inspect endpoints
to map that address, and do not treat it as a categorical `base`/`overlay` flag.
On errors this can identify an **attempted** upstream, not a completed response;
if no upstream was selected it can be empty/absent. Always inspect HTTP status.

Diagnostic request headers are removed before forwarding, and direct data routes
overwrite upstream diagnostic response values. Public selection is still derived
from the hostname, never a browser routing header. The trusted `publicProxy`
must preserve internal response headers for API identity to reach the browser.
An authentication rejection or a call through an uninstrumented internal
base route can have only the proxy header: absence means **unknown**, not base.
These are development diagnostics, not a security attestation or full hop trace.

Only CLI-owned routes are instrumented. Existing static base routes are untouched;
unselected, expired or deleted environments may return no diagnostic headers.
Rerun `dev up` with the new CLI to instrument existing overlays. View headers in
the browser Network panel or with `curl -D - -o /dev/null '<environment URL>'`.
This does not add cross-origin JavaScript exposure or change application CORS.

This preserves partial-overlay fallback and internal-hop affinity without a
service mesh, application router, Kubernetes discovery in application code, or
overlay lifecycle controller.

## Dependencies and tasks

`.dev/config.yaml` (or `.dev/config.json`) may declare environment-owned dependency manifests and named
task Jobs as repository-relative YAML or JSON. `dev provision` and `dev task` are
explicit operations; `dev up --dependency ... --task ...` runs provision →
readiness → tasks in flag order before applying an overlay. A failed task aborts
that up invocation before overlay publication or creation. Startup runs
repository-declared hooks; migrations/seeds run only when requested explicitly
or by those hooks.

Dependency objects must be namespaced in an allowed namespace. Server dry-run
proves their scope; Secret and cluster-scoped objects are rejected. Atomic
create plus an immutable manifest hash prevents cross-owner adoption races and
makes identical reprovisioning idempotent. Readiness supports either a standard
`condition` or `jsonPath` plus `value`. Retention is written on each object:
`retain` is the default and survives down/TTL, while `down` is removed only
after live ownership and persisted retention are verified. A later config edit
cannot turn retained data into disposable data.

Tasks accept exactly one Job and preserve its Secret references. A `target`
identifies the mutated database/environment; one Lease in the shared
`taskLockNamespace` serializes it across task names, owners and Job namespaces.
A lock is released only after a terminal Complete or Failed Job condition.
After interruption, only the exact prior Job reaching a terminal condition
permits automatic takeover. Lease expiry or Job absence is not sufficient: the
prior client may be paused between acquiring the Lease and creating the Job.
An abandoned absent-Job holder therefore requires the inspected compare-and-set
recovery in the troubleshooting guide. Optional `imageName`, `pushTarget` and
`container` fields publish the current source revision and inject its digest
into the named regular or init container. A migration init container can finish
before a pinned post-migration container runs.
Without them, the manifest's explicitly pinned image owns provenance.

The foreground `dev up` process owns local watching. `dev down` removes only
objects matching both current owner and route labels, including the Backend and
BackendTrafficPolicy resources. Expiry cleanup scans all owned resource kinds,
so orphan routing objects remain cleanable after their Deployment disappears.
Expiry maintenance runs on startup/status or through `dev cleanup`; status
never runs lifecycle hooks and leaves marked routes pending teardown.
While the local watcher runs it renews expiry every 30 seconds (or one third
of a shorter configured TTL). After it stops, the last renewed expiry remains
the cleanup deadline.

On supported Linux/macOS development hosts, an exclusive file lock admits one
local `dev up` per checkout and route before publication or cluster mutation.
A private Unix socket handles status and graceful stop. `dev down` waits for
that owner to stop before deleting resources; it never signals a stored PID.
After an abrupt exit the kernel releases the lock and the next invocation
reclaims the stale socket. Stop older CLI versions before upgrading: legacy
PID files are intentionally not used to control unknown processes. Different
checkouts on different machines must use different owner identities if their
loops are to operate independently.

## Lifecycle hooks

`hooks.beforeUp` and `hooks.afterDown` name ordered lists of declared tasks.
`hooks.deployables.<name>.beforeStart` and `afterStop` bind tasks to a Bazel
metadata deployable. Unknown tasks, deployables and configuration fields fail
before startup. Hook Jobs use the same publication, task target Lease, timeout,
namespace and ownership boundaries as `dev task`.

Startup first cleans up expired routes and selects deployables. With no
selection it exits without hooks. Otherwise it writes a labelled lifecycle
ConfigMap in the routing namespace, provisions explicit dependencies, runs
`beforeUp`, then explicit tasks, publishes images, and runs each deployable's
`beforeStart` immediately before creating or replacing its overlay. Routing,
initial synchronization and the watch loop follow. A failed hook prevents its
subsequent workload write; a failed `beforeUp` prevents image publication too.
The marker exists before the first hook, so a failed start remains cleanable.

The marker records teardown task names, the original owner and selected
services. Heartbeat renewal begins after the marker is written and continues during
startup hooks, image publication and live sync; it includes this ConfigMap.
The marker also records its checkout for exact local-owner shutdown during
cleanup. Startup expiry maintenance protects its newly acquired local socket
while tearing down a prior expired marker for the same checkout/route.
`dev down` stops its local loop, lists owned Deployments and persists the
discovered service names before deleting overlays with foreground propagation.
It waits for each service's pods to disappear and records that stopping has
completed. It runs the recorded service `afterStop` tasks, then
`afterDown`, removes dependencies with `retention: down`, deletes task Jobs,
and deletes the marker last. A failed wait or hook leaves the marker. A retry
after stopping has completed runs the hooks without another overlay deletion.
All hooks must be idempotent because any hook can run more than once.

Cleanup groups resource expiries across all configured namespaces and uses the
latest expiry for a route. Expired marked routes stop the recorded local owner,
then use the same teardown and original owner. Missing recorded tasks produce
a warning and leave every
resource untouched. Routes from older CLIs without a marker retain delete-only
cleanup. Status performs no hook work, leaves expired marked routes in place,
and reports `teardownPending` for the current route. Cleanup is command-driven;
there is no always-running reaper.

## Profiles and variables

`variables` maps names matching `^[A-Z][A-Z0-9_]*$` to strings. A variable value
may contain `${DEV_ROUTE}` and `${DEV_NAMESPACE}`; values resolve once using
the route and routing namespace. Values cannot reference other DEV_VAR values.
Workload templates, task Job manifests and dependency manifests substitute
`${DEV_VAR_<NAME>}` alongside the existing direct route/namespace tokens. Direct
`${DEV_NAMESPACE}` is the workload/task/dependency namespace. An unresolved
DEV_VAR token is an error before that resource is mutated.

`profiles` is an ordered list with a unique nonempty `name` (excluding reserved
`default`), nonempty repository-relative `whenChanged` prefixes, optional
variable overrides and optional hook overrides. Only top-level variables may
be overridden. The first profile whose literal prefix matches a changed path
wins. The changed set includes committed changes from the comparison merge
base, uncommitted changes and unignored untracked files. Without a match, the
profile is `default`. Profiles replace only hook keys they specify; omitted
keys inherit, and an explicit empty array clears that event. Deployable hook
keys in every profile must name known Bazel metadata deployables.

Startup reports `profile=<name>`. The lifecycle marker stores that name and
resolved variables; a live marker with a different profile or unequal
resolved values makes startup fail with `run dev down first`. This preserves
the original cleanup scope even if a later startup hook would fail. The
refusal reports the route without exposing variable values. `dev task` and `dev provision` use a marker's profile
and recorded values when present; otherwise they select from the changed files
in the same way as startup. Down and expiry cleanup render recorded hook Jobs
and removable dependency manifests with the marker's values, rather than the
caller's configuration. Status shows the recorded profile. These values are
plaintext ConfigMap data and must not carry credentials.

A task's `target` also substitutes route, its task namespace and resolved
variables before Lease acquisition. Every lifecycle task for a route must use
the same stable target; a target containing `${DEV_ROUTE}` creates a Lease per
route. This does not change terminal-Job proof, compare-and-set acquisition or
interruption recovery. Dependency readiness names keep their existing direct
route substitution; profiles do not add readiness-expression templating.
