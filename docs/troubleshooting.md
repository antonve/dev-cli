# Troubleshooting

Start with `dev doctor`. It's read-only and checks Git, Bazel, the kube
context, namespace and route permissions, the ingress class, the Gateway and
the registry.

## Startup

**Branch host returns 404.** The environment has no running route. Use the URL
from the latest `dev up` and check `dev status`.

**Branch TLS isn't ready.** Run the `kubectl describe` command from the error.
Check the issuer, DNS and HTTP-01 routing, then rerun `dev up`.

**`gateway did not serve <url> from branch backend <host>`.** The overlay is
running, but for three minutes Envoy kept answering from somewhere else. The
error's `X-Dev-Backend` says where:

- **The base Service:** the branch is failing its health check. The usual
  causes are a readiness path slower than three seconds, a non-200 response,
  or a crash. Check `dev logs <service>` and the
  `route-<service>-dev-<route>` BackendTrafficPolicy.
- **No header:** the request never reached the branch route. Your machine must
  be able to resolve and trust the branch host.

Nothing is torn down; fix the cause and rerun `dev up`, or run `dev down`.

**`image is not anonymously pullable`.** The registry refused an anonymous
token for the named repository. The cluster pulls without a Secret, so the
image must be publicly pullable.

**`open bazel-out/...: no such file or directory`.** You're on a CLI older
than v0.10.1 and your Bazel doesn't create `bazel-*` links
(`--symlink_prefix=/`). Upgrade; don't create the links by hand.

## While editing

**Backend build error.** Nothing changes; the previous process keeps running.
Fix the code and save again.

**Backend fails readiness.** The CLI switches back to the previous binary.
Check `dev logs <service>` before the next edit.

**Push or digest failure.** Images are published before any Deployment
changes, so the running overlay is untouched.

**Sync error.** It's printed with the service name and shown in `dev status`.
The CLI retries on its own. Check `dev logs <service>` for app errors. A
replaced pod gets resynced automatically.

**`dev up` was interrupted.** Rerun `dev up` to resume, or `dev down` to
remove the environment. Otherwise it expires after the TTL.

## Dependencies and tasks

**Manifest hash error.** Dependencies can't be changed in place; the CLI won't
apply over a live database. Use a new dependency name or route. `retain`
dependencies survive config changes, `dev down` and expiry.

**Readiness for operator resources** without standard conditions: use
`jsonPath` and `value`, for example `.status.PostgresClusterStatus` =
`Running`.

**A task Lease is stuck.** A timed-out or interrupted task keeps its Lease.
The next run takes over automatically once that exact Job is Complete or
Failed. If the Job no longer exists, the CLI won't take over on its own,
because the old client might only be paused. To recover by hand:

1. Make sure the original `dev task` or `dev up` process has stopped.
2. Confirm `kubectl -n <namespace> get job <job>` returns NotFound.
3. Clear the holder with a compare-and-set patch. It fails safely if the
   holder has changed. Never delete the Lease.

```sh
kubectl --context <context> -n <lock-namespace> patch lease <lease> \
  --type=json \
  -p '[{"op":"test","path":"/spec/holderIdentity","value":"<namespace>/<job>"},{"op":"replace","path":"/spec/holderIdentity","value":""}]'
```

## Teardown

**Teardown is stuck.** Look at the logs of the hook Job named in the error,
fix the cause, then rerun `dev down` or `dev cleanup` from a checkout that
declares the recorded tasks. Retries don't recreate overlays. Never delete the
lifecycle marker by hand; it's what makes the retry work.

**`teardownPending` in `dev status`.** The route expired or was partly torn
down, and `dev status` never runs hooks. Run `dev down` or `dev cleanup`. If
a task is missing, the warning names it.

**Certificates or Secrets left behind.** cert-manager must run with
`--enable-certificate-owner-ref=true`.
