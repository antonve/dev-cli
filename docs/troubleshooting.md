# Operations and troubleshooting

- Run `dev doctor` first. It is read-only and checks Git/Bazel, the exact kube
  context, namespace RBAC, ingress class, and registry endpoint.
- A backend build error is non-destructive: fix the source and save again; the
  previous process remains running.
- A sync error is printed with the service name. Check `dev status` and
  `dev logs <service>`, then rerun `dev up`; apply and copy are idempotent.
- If the local process is interrupted, rerun `dev up` to resume or `dev down`
  to remove the route. Abandoned resources expire according to repository TTL.
- The branch menu takes the `route` shown by `dev up` or `dev status`, not a raw
  branch name, preventing collisions between owners using identical branches.
