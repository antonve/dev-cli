# Branch-host links

v0.9.0 requires `clusterIssuer`, for example `lab-ca-acme`. Each public host
gets `https://<route>.<publicHost>/`; the route includes the owner and branch.
Branches have independent browser origins, so one profile can open multiple
owners without selecting or clearing a routing cookie. Application login cookies
remain application-owned; links do not authenticate or grant access.

```sh
dev up --owner alice
dev url --owner alice '/settings?tab=profile#details'
dev url --owner alice --host account.dev.lab '/settings'
```

Flags precede the root-relative destination. The configured `publicHosts` are
an allowlist for `--host`; DNS names must leave room for a 47-character route
label. External destinations, malformed queries and non-root-relative paths
are rejected. Ordinary query parameters, duplicate values and fragments are
preserved. `dev url` only formats a link; it does not start an environment or
make Kubernetes calls.

Up prints branch and plain base links after initial sync, route admission and
certificate readiness. Status exposes primary `url`/`baseURL` and per-host
`urls`/`baseURLs`; base links are ordinary `https://<publicHost>/` URLs.
An unavailable certificate fails startup with an exact describe command. Owned
resources remain available for diagnosis and `dev down`. Trust the issuing CA.

Public HTTPRoutes match the branch hostname and path, replace client branch
context with the route key, and preserve `Cache-Control: no-store`. Proxy routes
retain the real authentication proxy. Direct routes keep independent active/base
failover; internal requests still need the application's normalized header
propagation through the gateway. Platform base routes must strip forged branch
headers. Deleted hosts return 404; an unknown host never creates an environment.

Down and expiry remove the owned Ingress. cert-manager must enable certificate
owner references so the Certificate and Secret are removed with it.

## Upgrade existing environments

Stop the older CLI loop first. Add `clusterIssuer` and remove any legacy
`cookieName` config key; configuration rejects unknown fields. Environments
created by v0.6–v0.8 retain their cookie routes until rerun with v0.9.0 or torn
down. Rerunning up replaces branch-host routes and deletes only the same
owner/route's legacy public selection HTTPRoutes. Use the newly printed URLs;
old base-host selection links and the `--cookie`/`--clear` flags are retired.
