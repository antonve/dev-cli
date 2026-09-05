# Architecture and repository contract

The CLI deliberately has no workload catalog. A Bazel repository defines one
`dev_deployable` metadata target per deployable and connects it to the build
graph it represents. The emitted JSON names logical application image and push
targets, runtime kind, readiness path, development command, routes, build
target, and sync roots. Registry locations remain environment configuration.

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

## Runtime ownership

Frontend metadata declares a normal repository-owned pnpm dev command. The CLI
starts the application image once with that command and copies changed source
files directly into its writable workspace. Vite, Next.js, TanStack Start, or
another repository-selected dev server owns filesystem watching and HMR; the
CLI does not ship a frontend server.

Only declared `syncPaths` are copied. Deleted files are removed using the
CLI's prior file manifest; dependency trees and unrelated files are retained.
`dependencyPaths` identify manifests and lockfiles, and `dependencyCommand`
declares the repository's install command. Include those paths in both
`sourceRoots` and `syncPaths`. The command runs after initial sync, changes to
those dependency inputs, or container replacement—not on ordinary source edits.

Watch retries unsuccessful transfers with a bounded backoff and checks pod UID
and app restart count every two seconds. A replacement runtime receives the
current local contents even if no further file is edited. Sync errors and last
successful sync time are recorded on the owned Deployment for `dev status`.

Backend images contain only the application. The CLI injects its own versioned
POSIX supervisor in a ConfigMap, stages a successfully rebuilt Bazel binary in
an `emptyDir`, signals the process, and waits for the declared readiness URL.
Compile failures never touch the running binary. A readiness failure triggers a
swap back to the previous binary and a second readiness check.

## Routing ownership

The current routing adapter requires Envoy Gateway with its Backend extension
enabled (`config.envoyGateway.extensionApis.enableBackend: true`) and a named
Gateway. It is not portable to every Gateway API implementation: cookie regex
matching and health-based failover depend on this provider. The
base application declares ordinary HTTPRoutes. For an active branch the CLI
creates one temporary HTTPRoute per routable deployable. A public route matches
the scoped cookie and replaces `x-dev-branch` with the route key; the platform's
base routes remove an untrusted browser header. An internal route matches the
normalized header propagated by an application.

Each route chooses its backend independently. Unaffected deployables reference
the base Service directly. Affected deployables use two Envoy Backend resources:
the branch Service as the active tier and the base Service as the fallback tier.
Both reference explicit namespace-local Service DNS names, so deleting the
branch Service does not invalidate the HTTPRoute's object references. A
BackendTrafficPolicy checks the declared readiness path every second. Envoy
selects the base tier when the branch tier is unavailable, even if the local CLI
is stopped. Failover is eventual, not a zero-error guarantee: health checks,
DNS, and data-plane configuration take time to converge.

The CLI waits for current-generation HTTPRoute Accepted and ResolvedRefs
conditions before declaring startup complete. Route admission is reported
separately from workload readiness; it is not proof that an individual request
used the overlay. Application diagnostics provide that evidence.

This preserves partial-overlay fallback and internal-hop affinity without a
service mesh, application router, Kubernetes discovery in application code, or
overlay lifecycle controller.

The foreground `dev up` process owns local watching. `dev down` removes only
objects matching both current owner and route labels, including the Backend and
BackendTrafficPolicy resources. Expiry cleanup scans all owned resource kinds,
so orphan routing objects remain cleanable after their Deployment disappears.
Expiry cleanup runs on
up/status or through `dev cleanup`.
While the local watcher runs it renews expiry every 30 seconds (or one third
of a shorter configured TTL). After it stops, the last renewed expiry remains
the cleanup deadline.
