# Operations and troubleshooting

- Run `dev doctor` first. It is read-only and checks Git/Bazel, the exact kube
  context, namespace RBAC, ingress class, Gateway API object and route RBAC,
  and registry endpoint.
- A backend build error is non-destructive: fix the source and save again; the
  previous process remains running.
- A backend that builds but fails readiness is automatically replaced with the
  previous ready binary. Inspect `dev logs <service>` before the next edit.
- Registry publishing happens before a Deployment changes. A push or digest
  resolution failure therefore leaves the existing overlay untouched.
- A sync error is printed with the service name and recorded in `dev status`.
  The watcher retries with a bounded backoff, including without another edit.
  Check `dev logs <service>` for process errors. Pod/container replacement
  triggers source or binary synchronization automatically.
- If the local process is interrupted, rerun `dev up` to resume or `dev down`
  to remove the route. Abandoned resources expire according to repository TTL.
- The branch menu takes the `route` shown by `dev up` or `dev status`, not a raw
  branch name, preventing collisions between owners using identical branches.
