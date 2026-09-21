# Gateway-owned deep links (v0.3.0)

```sh
dev up --owner alice
dev url --owner alice '/settings?tab=profile#details'
dev url --owner alice --host account.dev.lab '/settings'
dev url --owner alice --clear '/settings?tab=profile#details'
```

Up prints both links after initial sync and route admission. Status JSON adds
`url` and `baseURL`. `dev url` formats a link without Kubernetes calls, builds or
deployment; its positional destination must be root-relative, never an external
URL. Flags precede the destination. The CLI safely encodes queries and replaces
any existing `dev-branch` parameters instead of appending duplicates.

For example, `https://app.dev.lab/settings?dev-branch=alice-feature-hash&tab=profile#details`
selects the environment on the first request, sets its cookie, and keeps subsequent
requests on that environment. This uses native Envoy Gateway HTTPRoute filters:
no frontend handler, JavaScript injection, extra router service or new dependency.

`publicHosts` is an explicit allowlist for `--host`. Selection cookies remain
host-only, so selecting the main application does not silently select account,
admin, or flags hosts. Generate/open a link for each desired host. `dev status`
returns `urls` and `baseURLs` maps in addition to the backward-compatible
primary `url` and `baseURL`.

## First-request precedence

For every public deployable path, the CLI adds GET/HEAD matches for its exact
route key and for the reserved value `base`. At equal path specificity, Gateway
API method matches outrank header matches. This intentionally makes a deep link
win over an old cookie, even when another owner or an older CLI created that
cookie route. A query-only match would incorrectly lose to a Cookie header match.

The selected rule overwrites `x-dev-branch` before forwarding and adds a separate
`Set-Cookie` response header without replacing application cookies. The cookie
retains the existing host-only, Path=/, Secure, SameSite=Lax attributes and has a
Max-Age equal to the configured TTL. It is intentionally JavaScript-readable for
compatibility with existing optional menus; new applications need no such menu.
Responses carry `Cache-Control: no-store`. Each affected service keeps the same
Envoy active/base fallback backends. Unaffected services use base directly while
preserving normalized branch context for downstream calls.

The `base` rule clears the cookie, removes even a forged browser routing header,
and immediately forwards to the base Service. Identical base-selection rules
reside in each owner's existing HTTPRoutes, so no shared mutable lifecycle resource
is needed. Deleting one owner leaves the other owners' selection routes intact.
Existing down/TTL cleanup handles all new rules because they are inside the
already-owned HTTPRoutes.

## Boundaries

- Re-run up with v0.3+ to install selection rules for an older environment.
- Selection is for GET/HEAD navigation; other methods retain ordinary cookie
  routing. More-specific unrelated platform routes can take precedence; guest
  base HTTPRoutes must retain the compatible path-prefix contract.
- Only active CLI route keys and `base` are matched. Unknown, expired/deleted or
  malformed selections are ignored by these rules: existing-cookie/base routing
  applies. A link neither creates an environment nor authenticates its visitor.
- Base clearing works while at least one v0.3+ environment has public routes;
  after all overlays are removed, normal base routing already applies. A stale
  cookie can remain until its expiry; use the clear link before final down when
  cookie removal matters.
- Use one selector parameter. CLI-generated URLs guarantee that; duplicate query
  keys supplied manually are subject to the gateway's parsing behavior.
- The query parameter remains in the URL and is forwarded to the application.
  Applications with strict query validation or signed URLs may require a future
  gateway-side query-removal mechanism; this feature does not alter their code.
- All tabs in one browser profile share the cookie. Separate profiles/contexts
  are required for independent simultaneous selections on one hostname.
- Internal backend calls still propagate normalized branch context through the
  gateway; frontend-free selection does not make backend propagation automatic.
- Protected APIs declare a `publicProxy` and separate `internalHost`. Envoy
  normalizes the browser header before the authentication proxy; the proxy and
  application must forward that normalized context to the internal host.
- Route admission and health/DNS convergence are distinct. Transient failover
  errors remain possible. Validate response identities, not only resource health.

See [Gateway API match precedence](https://github.com/kubernetes-sigs/gateway-api/blob/v1.5.0/apis/v1/httproute_types.go#L179)
and [Envoy response headers](https://gateway.envoyproxy.io/docs/tasks/traffic/http-response-headers/).
