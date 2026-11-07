# dev-cli

`dev` lets you try your branch in a real, shared Kubernetes environment
without deploying the whole stack. Run `dev up` and it:

1. works out which services your branch changed,
2. deploys only those, next to the normal shared deployment (the "base"),
3. gives you a branch URL such as `https://alice-my-feature-1a2b3c4d.app.dev.example/`,
4. keeps those services updated as you edit.

Requests to services you didn't change go to the base, so your branch behaves
like the full app.

- **Frontends** get your file changes synced into a running dev server (Vite,
  Next.js, TanStack Start, …) with normal HMR.
- **Backends and workers** are rebuilt with Bazel and restarted in place. A
  broken build or failed readiness check keeps the previous binary running.
- **Each branch gets its own hostname**, `https://<route>.<publicHost>/`, so
  several branches and developers can share one browser profile.

The repository being developed owns all the details: Bazel `dev_deployable`
targets describe each service, and `.dev/config.yaml` describes the cluster.
The CLI itself is a plain Go module and does not need Bazel.

## Install

```sh
GOPRIVATE=github.com/antonve/dev-cli go install github.com/antonve/dev-cli/cmd/dev@latest
```

See [installation](docs/installation.md) for credentials and for setting up a
repository.

## Use

```sh
dev doctor                      # check prerequisites (read-only)
dev up                          # start the changed services and watch for edits
dev url '/settings?tab=profile' # print a branch link to a page
dev status
dev logs [service]
dev down                        # remove this branch's environment
```

`dev up` stays in the foreground. Ctrl-C stops watching but leaves the
environment running until `dev down` or its TTL expires. Rerunning `dev up`
is safe.

Other useful flags and commands:

```sh
dev up --base origin/other      # compare against another base (default: merge base with origin/main)
dev up --service unchanged-api  # also start a service you didn't change
dev up --dependency db --task migrate
dev provision db                # create a declared dependency
dev task migrate                # run a declared task Job
dev cleanup                     # remove expired environments
dev down --headless --namespace ns --route r  # tear down from the cluster marker alone, no checkout
dev cleanup --headless --namespace ns         # remove expired environments without a checkout
```

`--service`, `--dependency` and `--task` can be repeated. Pass `--owner` or
set `DEV_OWNER` to choose whose environment you're working with.

## Docs

- [Installation and onboarding](docs/installation.md): installing the CLI and
  preparing a repository.
- [Architecture](docs/architecture.md): how selection, live updates, routing,
  tasks, hooks, profiles, environment overrides and production mode work.
- [Branch links](docs/deep-links.md): how branch hostnames and `dev url` work.
- [Troubleshooting](docs/troubleshooting.md)
- [Contributing](docs/contributing.md)

## Agent skill

[skills/dev-cli](skills/dev-cli/SKILL.md) teaches coding agents the dev-cli
workflow. Copy that folder into your agent's skills directory (for Codex,
`$CODEX_HOME/skills` or `~/.codex/skills`). It grants no cluster or registry
access by itself.
