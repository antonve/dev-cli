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

Backend images contain only the application. The CLI injects its own versioned
POSIX supervisor in a ConfigMap, stages a successfully rebuilt Bazel binary in
an `emptyDir`, signals the process, and waits for the declared readiness URL.
Compile failures never touch the running binary. A readiness failure triggers a
swap back to the previous binary and a second readiness check.

## Routing ownership

The platform supplies a Gateway API implementation and a named Gateway. The
base application declares ordinary HTTPRoutes. For an active branch the CLI
creates one temporary HTTPRoute per routable deployable. A public route matches
the scoped cookie and replaces `x-dev-branch` with the route key; the platform's
base routes remove an untrusted browser header. An internal route matches the
normalized header propagated by an application.

Each route chooses its backend independently: affected deployables reference
their branch Service, while unaffected deployables reference the base Service.
This preserves partial-overlay fallback and internal-hop affinity without a
service mesh, application router, Kubernetes discovery in application code, or
overlay lifecycle controller.

The foreground `dev up` process owns local watching. `dev down` removes only
objects matching both current owner and route labels. Expiry cleanup runs on
up/status or through `dev cleanup`.
