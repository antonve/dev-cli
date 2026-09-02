# Architecture and repository contract

The CLI deliberately has no workload catalog. A Bazel repository defines a
custom `dev_deployable` rule per deployable. That rule depends on the build
graph it represents and emits JSON containing the runtime kind, image,
readiness path, build target, and sync roots. `dev up` asks Bazel for all source
files and uses `rdeps` from changed file targets to those metadata rules. An
unmapped changed file fails closed by selecting all deployables.

The default comparison is `merge-base(origin/main, HEAD)`. Names contain a
readable owner/branch prefix plus a SHA-256-derived suffix. Original owner and
branch values are retained in annotations. Every resource carries owner,
route, service, source/base revision, timestamps, expiry, and CLI version.

The CLI applies new Deployments and Services and never mutates base workloads.
Frontend overlays share a writable volume with a minimal sync sidecar; changed
source files are copied there and the repository's dev server performs HMR.
Backend overlays run the image's supervisor. Only after a successful Bazel
build does the CLI copy a staged executable and ask the supervisor to swap it,
so compile failures leave the last working process untouched.

The foreground `dev up` process owns local watching. Kubernetes resources are
explicitly removed with `dev down`; expiry cleanup runs during up/status or via
`dev cleanup`, so no overlay lifecycle controller is needed.

Browser routing headers are untrusted. The playground edge router removes
`x-dev-branch` and recreates it only from the scoped selection cookie. Services
may propagate that normalized header, but browser traffic cannot bypass the
router. The CLI uses argument arrays rather than a shell, never reads cluster
credential files, and only deletes resources bearing its management labels.
