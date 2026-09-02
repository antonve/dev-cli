# dev-cli

`dev` is a repository-driven Kubernetes development CLI. It asks Bazel for
repository-owned deployable metadata, computes affected deployables relative to
the merge base with `origin/main`, creates owner/branch-isolated overlays, and
keeps frontend file sync or backend binary restart loops running.

## Install

```sh
go install github.com/antonve/dev-cli/cmd/dev@latest
```

The target repository supplies `.dev/config.json` and Bazel
`dev_deployable` targets. The CLI itself is an ordinary Go module and does not
use Bazel.

## Commands

```sh
dev doctor
dev up                         # compare with merge-base(origin/main, HEAD)
dev up --base origin/other     # explicit comparison base
dev status
dev logs [service]
dev down
dev cleanup                    # remove expired dev-cli overlays
```

`dev up` stays in the foreground and supervises live updates. Stop it with
Ctrl-C; the Kubernetes resources remain until `dev down` or TTL cleanup. It is
safe to rerun. Set `DEV_OWNER` or pass `--owner` to choose the ownership
identity. `dev status` prints the collision-resistant route key used by the
playground branch menu.

See [docs/architecture.md](docs/architecture.md) and
[docs/troubleshooting.md](docs/troubleshooting.md) for the repository contract,
security model, lifecycle, and recovery behavior.
