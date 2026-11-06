---
name: dev-cli
description: Run and verify branch-isolated Kubernetes development environments with dev-cli in repositories containing .dev/config.yaml or .dev/config.json and Bazel dev_deployable metadata. Use for live edits, branch tasks, multi-host links, status, logs and scoped cleanup; not for production deployment or generic local dev servers.
---

# Develop with dev-cli

`dev up` deploys the services your branch changed next to the shared base
environment, gives the branch its own URL, and live-updates it while you
edit. Use the `dev` commands for all of this. Don't recreate the workflow with
manual image pushes, `kubectl cp`, restarts, or app-side branch switches.

## Ground rules

- **Production configs are off limits.** If the config has
  `mode: production`, don't run `dev up`, `task`, `provision`, `down` or
  `cleanup` unless the maintainer tells you to for that branch and command.
  Reading `dev status` or `dev doctor` output they share is fine.
- **Stay within your authority.** Check that the configured context,
  namespaces, hosts, registry, dependencies and task targets are things your
  task allows you to touch. This skill grants no extra permissions.
- **Don't print secrets** such as kubeconfigs, Git credentials or registry
  tokens.
- **Leave other people's work alone.** Don't stop other loops or worktrees,
  and never delete base/Argo or unrelated resources.

## Set up

1. Read the repository's instructions and `.dev/config.yaml` (or
   `.dev/config.json`). If both exist, pass the same `--config <path>` to
   every command.
2. Run `dev version`. This skill describes v0.11. If `dev` is older or
   missing, install it:
   `GOPRIVATE=github.com/antonve/dev-cli go install github.com/antonve/dev-cli/cmd/dev@latest`
   (existing Git credentials are enough; Bazel belongs to the repository).
3. Check `git status` and run `git fetch origin main`, then `dev doctor`.
   Doctor checks prerequisites only. It doesn't prove builds or routing work.
4. Pick one owner name for the session, unique to this checkout and machine.
   Pass it on every command with `--owner` or set `DEV_OWNER`. The owner is
   for naming and cleanup, not authentication.

## Start

Make your edits **before** `dev up --owner <owner>`. Uncommitted changes
count. Services are chosen once, at startup, from the diff against
`origin/main` (or `--base <ref>`). To add a service later, stop and rerun with
`--service <name>`. Don't claim the CLI picks up new services on its own.

Before starting, read the config's hooks and profiles:

- `beforeUp` runs before tasks and image publishing, and `beforeStart` runs
  before each service's overlay. A failed hook stops startup but leaves the
  route cleanable.
- If changed paths match a profile, check the `profile=` line `dev up`
  prints. A running route can't switch profiles or variable values; run
  `dev down` first.
- Variables are plain text. Never put credentials in them.

Dependencies and tasks run only when asked for:

- `dev provision <name>` or `dev up --dependency <name>`. `retain`
  dependencies survive `dev down` and expiry on purpose.
- `dev task <name>` or `dev up --task <name>`, in the order the repository
  needs. A failed task stops `up` and never resets anything.
- Never delete a task Lease. If a Lease is stuck and its Job no longer
  exists, prove the original client has stopped, then follow the
  compare-and-set recovery in dev-cli's `docs/troubleshooting.md`.

Run `dev up` in a terminal that will survive the session. Record the checkout,
branch, owner and terminal or log handle. Only one loop per checkout and
route. `--no-watch` only does the first sync; it isn't live development.

## Edit

- **Frontend:** edit files under the declared sync paths. They're copied into
  the pod and the dev server hot-reloads them. No image rebuild happens.
  Dependency file changes rerun the install command.
- **Backend:** the loop rebuilds the binary, restarts it in the same pod and
  waits for readiness. A compile error keeps the old process running. A
  readiness failure restores the previous binary.
- **Errors:** read the loop output, `dev status --owner <owner>` and
  `dev logs --owner <owner> [service]` (flags go before the service name).
  Fix source and config problems within your task. Stop and ask about missing
  permissions or risky infrastructure changes; don't work around the CLI.

## Share the link

Give the user the **Open environment** URL that `dev up` printed, or build a
deep link:

```sh
dev url --owner <owner> '/desired/path?existing=value#section'
dev url --owner <owner> --host <configured-host> '/desired/path'
```

Quote paths containing `&`, `?` or `#`. `dev url` only prints a link; it
doesn't check that the environment works. `dev status` lists every branch and
base URL plus workload and sync health. Never ask the user to copy a route
key anywhere.

Each branch is its own browser origin, so several can be open in one profile.
Links don't log anyone in or grant access, and the user still needs lab
network, DNS and CA access.

## Verify

- Confirm the list of selected services.
- `dev up` already checks that each direct public host answers from the
  branch. It doesn't check `publicProxy` or internal routes, so verify those
  with a real request.
- Check `X-Dev-Backend` before saying the branch served a response.
  `X-Dev-Selected` only shows which branch was requested.
- With some services on the branch and some on the base, check that each
  falls back to base on its own. Internal calls stay on the branch only if
  the app passes on the `x-dev-branch` header. Don't promise that happens
  automatically.
- For live edits, make a second visible change while the **same loop** keeps
  running. Check frontend HMR in a browser without reloading where you can,
  check backend behaviour on a fresh request, and confirm pod UIDs and image
  digests didn't change. Say which kind of evidence you have: a browser HMR
  check is stronger than a file-sync log or an HTTP response.
- A UI that loaded diagnostics once may need a refresh to show a backend
  change. Failover takes a few seconds and isn't error-free.

## Hand off or clean up

**Leaving a demo running:** keep the loop and overlays, and tell the user the
URL, owner, checkout and branch, the visible change, the expiry, the log
handle and the exact cleanup command. Say so if the terminal won't survive
the handoff.

**Otherwise:** run `dev down --owner <owner>` from the same checkout. Ctrl-C
alone leaves the Kubernetes resources running.

`dev cleanup` removes expired environments for every owner, but never
retained dependency data. Don't use it to remove someone's active
environment, and don't suggest that expired environments get removed in the
background. Cleanup only runs on `dev up`, `dev status` and `dev cleanup`.

If teardown fails, read the hook Job logs, fix the cause and rerun
`dev down` or `dev cleanup`. Never delete the lifecycle marker by hand.
`teardownPending` in `dev status` means teardown still has to run.
