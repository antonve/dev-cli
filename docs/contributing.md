# Contributing

Keep reusable development orchestration here. Application sources, their
Bazel graph and images, deployment metadata, and application conformance tests
belong in the target application repository. Gateway installation belongs to
the platform. Do not add Bazel or application-specific runtime servers here.

```sh
go test -count=1 -race ./...
go vet ./...
go install ./cmd/dev
```

The packages separate Git discovery (`gitx`), Bazel graph/build operations,
registry publication resolution, Kubernetes resources and runtime operations,
filesystem/build loops (`syncer`), configuration, naming, and local ownership.
External commands go through the injectable `execx.Runner`; tests use fake
outputs for failure cases and temporary files/processes for filesystem and
socket behavior. Include a regression test when fixing a lifecycle boundary.

No Actions runner is required for these CLI gates. The playground separately
uses GitHub-hosted Ubuntu CI under the owner's explicit authorization.

Never mutate an Argo base Deployment to make an overlay. Route, owner, service,
and management labels are cleanup boundaries. Keep upload staging inert until
transfer completes, preserve the last working backend on build/readiness
failure, and never acknowledge local shutdown before its work has stopped.

Before releasing, run the playground's complete Bazel/pnpm/image gates and live
branch conformance. Verify browser DOM HMR without navigation, binary restart
and readiness recovery, two branch hosts in one profile and internal hops, idempotent
startup, deletion fallback, TTL cleanup, installation from a clean checkout,
and a healthy Argo base. Tag only after those gates; test the published @latest
installation separately.
