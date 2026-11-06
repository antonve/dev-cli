# Branch links

Every branch environment has its own hostname for each public host:
`https://<route>.<publicHost>/`. The route is built from the owner and branch,
so two people on the same branch name get different links. The hostname is
the only thing that selects a branch. There are no cookies or headers to set,
so one browser profile can have several branches open at once.

Links don't log you in or grant access. Application login stays with the
application, and you still need network, DNS and CA access to the lab.

## Getting a link

`dev up` prints branch links and plain base links once the environment is
ready. `dev status` shows them too: `url` and `baseURL` for the primary host,
plus `urls` and `baseURLs` for every host.

`dev url` builds a link to a specific page. It only prints a link; it doesn't
start anything or call Kubernetes:

```sh
dev url --owner alice '/settings?tab=profile#details'
dev url --owner alice --host account.dev.lab '/settings'
```

- Flags go before the path.
- The path must start with `/`. Full external URLs are rejected.
- Query parameters (including repeated ones) and fragments are kept.
- `--host` must be one of the configured `publicHosts`.
- Quote paths containing `?`, `&` or `#`.

Public host names must leave room for a 47-character route label.

## What the gateway does

- Public routes match the branch hostname and path, and set the branch header
  to the route key, overwriting anything the client sent.
- Branch hosts send `Cache-Control: no-store`.
- `publicProxy` routes still go through the real authentication proxy.
- A host for a deleted environment returns 404. An unknown host never creates
  an environment.

See [routing](architecture.md#routing) for TLS modes, failover and internal
calls.

## Upgrading from v0.6–v0.8

Those versions selected branches with a cookie.

1. Stop the old `dev up` loop.
2. Add `clusterIssuer` (or set `hostTLS: gateway`) and remove `cookieName`
   from the config.
3. Rerun `dev up`, or run `dev down`, with the new CLI. This replaces only your
   own route's old cookie routes.
4. Use the newly printed links. Old cookie links and the `--cookie`/`--clear`
   flags no longer exist.
