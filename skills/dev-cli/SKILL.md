---
name: dev-cli
description: Run and verify branch-isolated Kubernetes development environments with dev-cli in repositories containing .dev/config.yaml or .dev/config.json and Bazel dev_deployable metadata. Use for live edits, branch tasks, multi-host links, status, logs and scoped cleanup; not for production deployment or generic local dev servers.
---

# Develop with dev-cli

Use the repository's `dev` workflow; do not recreate it with manual image pushes,
kubectl copies/restarts, or application-side branch selectors. Gateway deep links
require dev-cli v0.3.0 or newer and routes created by that version.

## Establish the environment

- Read repository instructions and `.dev/config.yaml` (or legacy `.dev/config.json`).
  If both exist, use `--config <path>` consistently to select the intended environment.
  Confirm the configured
  context, all allowed namespaces/hosts, registry, dependency and task target
  are within the user's task authority.
  The skill grants no additional deployment, cleanup or publication permission.
- Check `dev version`, Git branch/status and `git fetch origin main`, then
  `dev doctor`. YAML configuration, multi-host routing, dependencies and tasks
  require dev-cli v0.4.0 or newer; `worker` deployables and `selectionGroup`
  pairing require v0.5.0 or newer; lifecycle hooks require v0.6.0 or newer, and variables/profiles require v0.7.0
  or newer. Doctor is a prerequisite check, not an
  end-to-end build or routing proof. Do not print kubeconfigs, Git credentials
  or registry tokens.
- If missing, use the supported Go installation, with existing Git credentials
  and `GOPRIVATE=github.com/antonve/dev-cli` for the private module:
  `go install github.com/antonve/dev-cli/cmd/dev@latest`.
  Bazel belongs to the guest repository, not the CLI installation.
- Preserve existing worktrees and loops. Choose a stable owner for the session,
  unique across independent checkouts/machines using the same branch. Pass it
  consistently with `--owner` or set `DEV_OWNER` in the task environment. Owner is
  a collision/cleanup identity, not authentication.

## Start and edit

Make the intended service edits **before** `dev up --owner <owner>`. Uncommitted
edits count. The CLI uses the merge base with `origin/main` (or `--base <ref>`)
and Bazel reverse dependencies to select services once at startup. Unknown files
conservatively select all deployables. If adding another service later, stop and
rerun the loop with repeatable `--service <name>`; do not claim automatic
expansion or infer related services.

Read declared lifecycle hooks before startup: `beforeUp` runs before explicit
tasks and publication; `beforeStart` runs before its overlay Deployment.
Hook failures stop startup and preserve a marker for cleanup. Hooks must be
idempotent. Check the printed `profile=` and status profile when changed paths
select a configured profile. A live route cannot switch profiles or resolved
variables; run down first. Variables render workload/task/dependency manifests and task targets;
the lifecycle marker keeps resolved values for task/provision and teardown,
even from a checkout with different defaults. Variables are plaintext data,
not a place for credentials.

Provision only explicitly requested dependencies with `dev provision <name>` or
`dev up --dependency <name>`. `retain` dependencies deliberately survive down
and TTL. Run repository-defined migrations/seeds with `dev task <name>` or `dev
up --task <name>` in the required order. A failed/timed-out task blocks that up
invocation and never triggers reset. Never delete a task Lease to bypass
serialization. Normal retry recovers only after the exact prior Job is terminal;
an absent Job still requires proof that the original client stopped, followed
by the documented compare-and-set holder recovery.

Keep `dev up` running in a durable terminal/session supported by the environment.
Record the exact checkout, branch, owner and terminal/log handle. Do not launch
a second loop for the same checkout/route. `--no-watch` only performs initial
sync and is not live development.

- Frontend: edit declared sync paths; the CLI transfers files and the pod's
  normal pnpm-managed dev server provides HMR. Ordinary source edits do not
  rebuild images. Dependency changes run the declared dependency command.
- Backend: the loop builds the affected Bazel binary, uploads it, restarts the
  supervised process in the same pod and waits for readiness. Compile failures
  retain the working process. Readiness failure restores the previous binary.
- Read errors from the loop, `dev status --owner <owner>`, aggregate `dev logs
  --owner <owner>`, or filtered `dev logs --owner <owner> <service>` (flags
  precede positional arguments).
  Fix source/config errors within scope; stop for missing authority or unsafe
  infrastructure changes rather than bypassing the CLI.

## Give the user a clickable link

With `clusterIssuer`, v0.8.0 prints branch-host links that require no selection
cookie. Open these directly; each branch is its own origin. `dev url --cookie`
and `--clear` retain the base-host cookie fallback described below. Verify TLS
readiness and owned Ingress/Certificate/Secret removal on down; cert-manager
must enable certificate owner references for Secret cleanup.

Return the actual **Open environment** URL printed after startup, or run:

```sh
dev url --owner <owner> '/desired/path?existing=value#section'
dev url --owner <owner> --host <configured-host> '/desired/path'
dev url --owner <owner> --host <configured-host> --clear '/desired/path'
dev url --owner <owner> --clear '/desired/path?existing=value#section'
```

Quote destinations containing `&`, `?` or `#`. `dev url` only formats a link; it
does not start or verify an environment. `dev status` includes `url`, `baseURL`,
workload and sync health. Never ask the user to copy a route key into a menu.

Envoy handles `dev-branch=<route-key>` on GET/HEAD, overrides an old branch cookie
for the first request and sets the host-only Secure, SameSite=Lax cookie. `base`
clears it and uses base services immediately. The path, other query parameters
and fragment remain in the link; the selector is not removed or hidden from the
application. No frontend integration is needed. Query selection is not supported
on POST or other mutation methods. Unknown/deleted selections do not create an
overlay; normal existing-cookie/base routing applies. Base-clear rules exist
while at least one v0.3+ environment has routes on that host.

The cookie is shared across tabs for one host in the same browser profile;
configured public hosts are selected independently. Use separate profiles or
isolated contexts for simultaneous selections on one host. Lab DNS/network/CA
access is still required. Links are not authentication or access control.

## Prove the result and hand off

Confirm the affected service list before calling startup successful. Check
readiness and route admission, then actual application responses: a healthy
HTTPRoute alone does not prove which workload served the request. For partial
overlays verify each service independently falls back to base; internal calls
still need the application's existing normalized-header propagation through the
gateway. Do not promise transparent backend context propagation.
Where routing headers are available, `X-Dev-Selected` reports the requested
selection and `X-Dev-Backend` identifies the upstream that served the response.
Check the latter before attributing a response to an overlay.

For live-edit tests, make a second visible edit while the **same loop** runs.
Verify frontend HMR in a browser without navigation when possible, backend
identity/readiness on a fresh request, and unchanged pod UIDs/image digests.
Distinguish browser HMR proof from source-transfer or HTTP-only evidence. A UI
that fetched diagnostics once may need refreshing to display a backend change.
Allow bounded routing/health convergence; failover is not guaranteed zero-error.

If asked to leave a demo running, retain its loop and overlays and return the
clickable URL, owner, checkout/branch, visible change, expiry, log handle and exact
cleanup command. Be honest if the terminal cannot survive the handoff. Otherwise
use `dev down --owner <owner>` from the same branch/checkout to stop the local
loop and remove only that environment. Ctrl-C alone leaves Kubernetes resources.

`dev cleanup` is broader: it removes expired CLI-owned environments across owners
in configured namespaces, but never retained dependency data. Up/status also
perform expiry maintenance; status skips marked routes, reports
`teardownPending`, and never runs hooks. Down/cleanup wait for stopped pods,
run recorded `afterStop` and `afterDown` tasks with the recorded owner, then
remove dependencies and task Jobs and delete the lifecycle marker last. A
failed hook retains the marker for retry. Missing recorded tasks skip the
route with a warning. Inspect the hook Job logs, fix the cause, and rerun
down/cleanup; never delete a lifecycle marker by hand. Do not
use cleanup to delete another active environment or imply that expiry has an
always-running reaper. Never delete base/Argo resources or unrelated resources.
