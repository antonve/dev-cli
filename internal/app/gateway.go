package app

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/antonve/dev-cli/internal/config"
	"github.com/antonve/dev-cli/internal/deeplink"
	"github.com/antonve/dev-cli/internal/kube"
)

// A ready Pod and an admitted route do not mean Envoy's active health check has
// selected the branch yet; only the response's X-Dev-Backend proves it.
const gatewayConsecutive = 3

type gatewayProbe struct{ url, backend string }

type gatewayWaiter struct {
	client            *http.Client
	interval, timeout time.Duration
}

var gateway = gatewayWaiter{
	client: &http.Client{
		Timeout: 10 * time.Second,
		// A redirect is still answered by the probed upstream; following it
		// could leave the branch host.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	},
	interval: time.Second,
	timeout:  3 * time.Minute,
}

// gatewayProbes covers selected deployables whose public host route targets
// their own active/fallback Backends. Workers, internal-only routes and
// publicProxy routes are not reachable without the proxy's own rules.
func gatewayProbes(cfg config.Config, ds []config.Deployable, route string) []gatewayProbe {
	var probes []gatewayProbe
	for _, d := range ds {
		if d.Kind == "worker" || d.PublicPath == "" || d.Proxy(cfg).Name != "" {
			continue
		}
		path := d.PublicPath
		if d.ReadinessPath != "" && strings.HasPrefix(d.ReadinessPath, d.PublicPath) {
			path = d.ReadinessPath
		}
		url, err := deeplink.HostURL(d.Host(cfg), route, path)
		if err != nil {
			continue
		}
		probes = append(probes, gatewayProbe{url: url, backend: kube.OverlayHost(cfg, d, route)})
	}
	return probes
}

// wait returns once every probe's URL was served by its branch backend for
// gatewayConsecutive requests in a row, within one shared timeout.
func (g gatewayWaiter) wait(ctx context.Context, probes []gatewayProbe, progress func(string)) error {
	ctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	for _, p := range probes {
		progress("waiting for the gateway to serve " + p.url + " from the branch")
		streak, last := 0, "no response"
		timedOut := func() error {
			return fmt.Errorf("gateway did not serve %s from branch backend %s within %s; last response: %s", p.url, p.backend, g.timeout, last)
		}
		for {
			observed, result := g.observe(ctx, p.url)
			if ctx.Err() != nil {
				return timedOut()
			}
			last = result
			if observed != p.backend {
				streak = 0
			} else if streak++; streak == gatewayConsecutive {
				break
			}
			select {
			case <-ctx.Done():
				return timedOut()
			case <-time.After(g.interval):
			}
		}
	}
	return nil
}

// observe returns the backend that served a successful response, or "" with a
// description of why the response does not count.
func (g gatewayWaiter) observe(ctx context.Context, url string) (string, string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err.Error()
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return "", err.Error()
	}
	resp.Body.Close()
	backend := resp.Header.Get("X-Dev-Backend")
	switch {
	case backend == "":
		return "", fmt.Sprintf("status %d with no X-Dev-Backend header", resp.StatusCode)
	case resp.StatusCode >= 500:
		return "", fmt.Sprintf("status %d from X-Dev-Backend %s", resp.StatusCode, backend)
	}
	return backend, fmt.Sprintf("status %d from X-Dev-Backend %s", resp.StatusCode, backend)
}
