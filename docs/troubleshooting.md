# Operations and troubleshooting

- A branch-host 404 means that environment has no running host route. Use
  the current printed URL and inspect `dev status`.
- If branch TLS is not ready, run the exact certificate describe command
  printed by startup. Check issuer readiness, DNS and HTTP-01 routing. Cookie
  links remain available with `dev url --cookie`. Certificate and Secret cleanup
  requires cert-manager's `--enable-certificate-owner-ref=true` setting.

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
- Dependency reprovisioning is intentionally immutable. A manifest-hash error
  means the route needs a new dependency name/route; the CLI will not apply over
  a live database. `retain` objects survive config edits, down, and TTL.
- A timed-out or interrupted task leaves its Lease held. A later invocation
  recovers automatically only when the exact named Job has a terminal
  Complete/Failed condition. Lease expiry and Job absence are deliberately not
  proof: the old client may still create the Job after a pause.
- Recover an abandoned absent-Job holder manually only after proving the
  original `dev task`/`dev up` process has stopped and
  `kubectl -n <namespace> get job <job>` reports the exact holder Job as
  NotFound. Then clear that exact unchanged `<namespace>/<job>` holder with a
  compare-and-set patch; never delete the Lease or clear a changed holder:

  ```sh
  kubectl --context <context> -n <lock-namespace> patch lease <lease> \
    --type=json \
    -p '[{"op":"test","path":"/spec/holderIdentity","value":"<namespace>/<job>"},{"op":"replace","path":"/spec/holderIdentity","value":""}]'
  ```
- For operator resources without standard conditions, use `jsonPath` and
  `value` readiness (for example PostgresClusterStatus = Running).
- If teardown is stuck, inspect the failing hook Job logs printed by the error.
  Fix its cause and rerun `dev down` or `dev cleanup` from a checkout declaring
  the recorded tasks. The lifecycle marker preserves the retry contract; never
  delete it by hand. After overlays stop, hook retries do not recreate them.
- `teardownPending` means status left an expired or partially stopped lifecycle
  in place. Status never runs hook Jobs. Run `dev down` or `dev cleanup` to
  finish it; missing task warnings identify the checkout contract needed.
