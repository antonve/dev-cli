# dev-cli

`dev` is a repository-driven Kubernetes development CLI. It asks Bazel for
repository-owned deployable metadata, computes affected deployables relative to
the merge base with `origin/main`, creates owner/branch-isolated overlays, and
keeps frontend file sync or backend binary restart loops running.

## Install

```sh
go install github.com/antonve/dev-cli/cmd/dev@latest
```

The target repository supplies `.dev/config.yaml` (or legacy `.dev/config.json`) and Bazel
`dev_deployable` targets. The CLI itself is an ordinary Go module and does not
use Bazel.

## Commands

```sh
dev doctor
dev up                         # compare with merge-base(origin/main, HEAD)
dev up --base origin/other     # explicit comparison base
dev up --service unchanged-api # explicitly add an unchanged deployable
dev up --dependency branch-db --task migrate
dev provision branch-db       # explicit, on-demand dependency
dev task migrate              # serialized repository-owned Job
dev url '/settings?tab=profile' # clickable branch link; no cluster mutation
dev url --host account.dev.lab '/settings'
dev url --clear '/settings'     # select base and clear the branch cookie
dev status
dev logs [service]
dev down
dev cleanup                    # remove expired dev-cli overlays
```

`--service`, `--dependency`, and `--task` are explicit and repeatable.
Dependencies run when requested directly or by a declared task or hook. Deployables with the same
`selectionGroup` are selected together, including when one is named with
`--service`. `dev up`
stays in the foreground and supervises live updates. Stop it with
Ctrl-C; the Kubernetes resources remain until `dev down` or TTL cleanup. It is
safe to rerun. Set `DEV_OWNER` or pass `--owner` to choose the ownership
identity. `dev up` prints clickable environment and base links; `dev status`
includes both URLs. Envoy selects the branch and sets the browser cookie from
the `dev-branch` query parameter, with no frontend integration or manual menu.
Each configured public host has an independent host-only selection cookie.
Existing paths, query parameters and fragments are preserved. See
[deep links](docs/deep-links.md) for selection precedence and limits.

CLI-routed responses report `X-Dev-Selected` and Envoy's actual upstream in
`X-Dev-Backend`, so an overlay selection can be distinguished from base fallback.
Authentication proxy hops use a separate `X-Dev-Proxy-Backend` header. See
[response provenance](docs/architecture.md#response-provenance) for error,
internal-hop and uninstrumented-base limitations.

Affected application images are built on demand, pushed through repository
Bazel targets to the configured registry, resolved to an
immutable digest, and injected into the overlay. The CLI injects its own backend
supervisor and Envoy Gateway routing resources; application repositories do not contain
CLI runtime tools or routing code. Frontends use their normal pnpm-managed dev
server (for example Vite, Next.js, or TanStack Start) and native HMR.
Route-free `worker` deployables run as long-lived Deployments without a Service
or HTTPRoute. Their default readiness probe checks the supervised process.

See [docs/architecture.md](docs/architecture.md) and
[docs/troubleshooting.md](docs/troubleshooting.md) for the repository contract,
security model, lifecycle, and recovery behavior.

[Installation and onboarding](docs/installation.md) covers private-module
credentials and the target repository contract. [Contributing](docs/contributing.md)
describes the local build and release gates.

## Agent skill

The portable [dev-cli skill](skills/dev-cli/SKILL.md) teaches agents the actual
startup, live-edit, deep-link, verification and cleanup workflow. Install that
folder into your agent's skills directory (for Codex, `$CODEX_HOME/skills` or
`~/.codex/skills`). It does not grant cluster or registry permissions.

## Lifecycle hooks

Repositories may bind declared tasks to startup and teardown:

```yaml
hooks:
  beforeUp: [tenant]
  afterDown: [tenant-teardown]
  deployables:
    worker:
      beforeStart: [worker-override-set]
      afterStop: [worker-override-clear]
```

Hooks require v0.6.0 or newer. `beforeUp` completes before explicit `--task`
tasks, image publication and overlays. A deployable's `beforeStart` completes
before its Deployment is created or replaced. Teardown waits for overlays and
pods to stop, runs every recorded `afterStop`, then `afterDown`, removes `down`
dependencies and task Jobs, and deletes the route's lifecycle marker last.
Startup renews the marker while hooks and publication run. Expired cleanup
stops the recorded local owner before hooks; discovered services are saved
before deletion so interrupted waits do not lose their cleanup tasks.
Hooks must be idempotent: reruns and failed teardown retries may repeat them.
`dev cleanup` and startup run expired routes' recorded teardown hooks;
`dev status` skips those routes and reports `teardownPending` without running
any hook. A checkout missing a recorded task skips that route with a warning.
Configuration rejects unknown keys and undeclared hook task/deployable names.

## Profiles and variables

v0.7.0 adds variables and first-match path profiles:

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

Variables use uppercase names and `${DEV_VAR_DATABASE}` in workload templates,
task Jobs and dependency manifests. Their values may contain `${DEV_ROUTE}`
and `${DEV_NAMESPACE}`; the latter resolves to the routing namespace. Direct
manifest `${DEV_NAMESPACE}` retains the manifest's declared namespace. Unknown
variables fail before the corresponding resource is written. Task targets also
expand these tokens before acquiring their Lease, so tasks can serialize one
route by declaring a target such as `database/${DEV_ROUTE}`.

Changed repository-relative path prefixes choose the first matching profile;
otherwise the profile is `default`. A profile overrides only declared variables
and hook keys it supplies; an explicit empty hook array disables that hook.
Startup prints `profile=`, and status shows the recorded profile. The lifecycle
marker records resolved variables; task/provision reuse them, and teardown uses
them even when another checkout's values differ. A live route cannot change
profile or resolved variables until `dev down`. Variables are plaintext
ConfigMap data; reference Secrets in manifests for credentials.
