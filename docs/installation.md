# Installation and repository onboarding

The CLI is a normal Go module, supported on Linux and macOS. Building or
installing it does not require Bazel. Go 1.24 or later is required.

```sh
go install github.com/antonve/dev-cli/cmd/dev@latest
dev version
```

This repository is private. Your Git credentials must already grant read
access, and Go must bypass public module proxies for this module. For example:

```sh
export GOPRIVATE=github.com/antonve/dev-cli
go install github.com/antonve/dev-cli/cmd/dev@latest
```

Include your Go binary directory (`go env GOPATH`, followed by `/bin`, unless
GOBIN is configured) in PATH. Never put a token in a repository URL or commit
credentials. Existing SSH or an authenticated Git credential helper is enough.

## Target repository contract

The target repository needs Git, its pinned Bazel version, kubectl, access to
the configured Kubernetes context/namespace, and a reachable OCI registry.
Frontend pods run the repository's pnpm-managed command; the CLI does not
install or provide a development server. The platform must supply Envoy
Gateway with the Backend extension enabled and an attachable named Gateway.

Create `.dev/config.json` with `kubeContext`, `namespace`, `registry`,
`ingressHost`, `ingressClass`, `gatewayName`, `gatewayNamespace`, `cookieName`,
`ttl`, and `metadataQuery`. The playground is the executable reference:
https://github.com/antonve/dev-cli-playground/blob/main/.dev/config.json

Every deployable has one Bazel-owned metadata target attached to the build
inputs it represents. The emitted JSON declares:

| Concern | Fields |
| --- | --- |
| Identity and runtime | `name`, `kind`, `port`, `readinessPath`, `containerPath` |
| Build and publication | `buildTarget`, `imageTarget`, `imageName`, `pushTarget` |
| Frontend development | `devCommand`, `sourceRoots`, `syncPaths`, `dependencyPaths`, `dependencyCommand` |
| HTTP routing | `publicPath` or `internalHost` |

Image/push targets are registry-neutral. The push target accepts `--repository`
and `--tag`; the CLI resolves the resulting digest before creating a workload.
Backend runtime images need a POSIX shell, tar, and wget, plus the application
binary at `containerPath`. The CLI supplies its supervisor via ConfigMap.
Frontend images must allow UID 1000 to write their `/workspace` source tree and
run their normal dev/install commands. Backend overlays run as UID 65532.

Declare only intentional sync roots, and include dependency manifests and
lockfiles in both watching and synchronization inputs. Local source directories
must contain regular files; dependency trees and Bazel outputs are excluded.
Base Services must use the declared deployable names. Applications that need
internal branch routing call the gateway alias with their logical internal
Host and propagate the normalized `x-dev-branch`; direct base-Service calls
bypass branch selection. The playground uses the `dev-cli-gateway` alias.

## Daily commands

```sh
git fetch origin main
dev doctor
dev up --owner alice                    # merge-base with origin/main
dev up --owner alice --base origin/next # alternative comparison base
dev status --owner alice
dev logs --owner alice hello-api        # flags before positional service
dev down --owner alice
dev cleanup
```

Choose the emitted route key in the browser menu, not the raw Git branch name.
The key includes the owner so two developers can use the same branch name.
Ctrl-C stops the foreground loop but retains overlays for inspection. Restart
with the same checkout/owner/branch, or use `dev down` for explicit cleanup.
Stop an older CLI before upgrading; v0.2.0 does not signal legacy PID files.

The affected service set is selected at startup. If development expands to
another service, stop and rerun `dev up` to select the new set. On startup,
workload readiness and HTTPRoute admission are checked; allow DNS/health-check
convergence before asserting which tier handled a request.
