package kube

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/antonve/dev-cli/internal/config"
	"github.com/antonve/dev-cli/internal/execx"
	"github.com/antonve/dev-cli/internal/naming"
)

const (
	ManagedLabel   = "dev-cli.io/managed-by"
	RouteLabel     = "dev-cli.io/route"
	OwnerLabel     = "dev-cli.io/owner"
	ServiceLabel   = "dev-cli.io/service"
	cliVersion     = "v0.2.0"
	ownedResources = "deployment,service,configmap,httproute,backends.gateway.envoyproxy.io,backendtrafficpolicies.gateway.envoyproxy.io"
)

// Runtime tooling belongs to the CLI. Backend repositories provide only an
// application binary and an application image.
const supervisorScript = `#!/bin/sh
set -u
stopping=0
child=""
terminate() {
  stopping=1
  touch /work/stop
  if [ -n "$child" ]; then kill -TERM "$child" 2>/dev/null || true; fi
}
trap terminate TERM INT
if [ ! -x /work/current ]; then
  cp "$DEV_BASE_BINARY" /work/current
  chmod 0555 /work/current
fi
generation=0
while [ "$stopping" -eq 0 ]; do
  if [ -f /work/rollback ] && [ -f /work/previous ]; then
    cp /work/previous /work/current
    chmod 0555 /work/current
    rm -f /work/rollback /work/next
  elif [ -f /work/next ]; then
    cp /work/current /work/previous
    mv /work/next /work/current
    chmod 0555 /work/current
  fi
  generation=$((generation + 1))
  printf '%s\n' "$generation" > /work/generation
  /work/current &
  child=$!
  printf '%s\n' "$child" > /work/pid
  wait "$child"
  status=$?
  child=""
  if [ "$stopping" -ne 0 ]; then exit "$status"; fi
  sleep 0.05
done
`

type Client struct {
	Run                execx.Runner
	Context, Namespace string
}

func (c Client) args(args ...string) []string {
	return append([]string{"--context", c.Context, "--namespace", c.Namespace}, args...)
}

func (c Client) RunKubectl(ctx context.Context, args []string, in io.Reader) ([]byte, error) {
	return c.Run.Run(ctx, "kubectl", c.args(args...), in)
}

func labels(owner, route, service string) map[string]any {
	return map[string]any{
		ManagedLabel: "dev-cli", OwnerLabel: naming.Slug(owner, 40), RouteLabel: route, ServiceLabel: service,
		"app.kubernetes.io/name": naming.Resource(service, route), "app.kubernetes.io/managed-by": "dev-cli",
	}
}

func annotations(owner, branch, revision, baseRef, baseRevision, image string, expiry time.Time, created string) map[string]any {
	return map[string]any{
		"dev-cli.io/owner-original": owner, "dev-cli.io/branch-original": branch,
		"dev-cli.io/source-revision": revision, "dev-cli.io/base-ref": baseRef,
		"dev-cli.io/base-revision": baseRevision, "dev-cli.io/image": image,
		"dev-cli.io/created-at": created, "dev-cli.io/last-sync-at": time.Now().UTC().Format(time.RFC3339),
		"dev-cli.io/expires-at": expiry.UTC().Format(time.RFC3339), "dev-cli.io/cli-version": cliVersion,
	}
}

func (c Client) ApplyOverlay(ctx context.Context, cfg config.Config, d config.Deployable, image, owner, branch, revision, baseRef, baseRevision, route string, expiry time.Time) error {
	name := naming.Resource(d.Name, route)
	created := time.Now().UTC().Format(time.RFC3339)
	if existing, err := c.RunKubectl(ctx, []string{"get", "deployment", name, "-o", "jsonpath={.metadata.annotations.dev-cli\\.io/created-at}"}, nil); err == nil && strings.TrimSpace(string(existing)) != "" {
		created = strings.TrimSpace(string(existing))
	}
	ann, lbl := annotations(owner, branch, revision, baseRef, baseRevision, image, expiry, created), labels(owner, route, d.Name)
	env := []any{
		map[string]any{"name": "DEV_BRANCH", "value": branch}, map[string]any{"name": "DEV_REVISION", "value": revision},
		map[string]any{"name": "DEV_NAMESPACE", "value": cfg.Namespace}, map[string]any{"name": "DEV_INTERNAL_GATEWAY", "value": "http://dev-cli-gateway"},
	}
	command := make([]any, len(d.DevCommand))
	for i := range d.DevCommand {
		command[i] = d.DevCommand[i]
	}
	volumes, mounts, items := []any{}, []any{}, []any{}
	security := map[string]any{"allowPrivilegeEscalation": false, "runAsNonRoot": true, "runAsUser": 1000, "runAsGroup": 1000}
	if d.Kind != "frontend" {
		command = []any{"/bin/sh", "/dev-cli/supervise.sh"}
		env = append(env, map[string]any{"name": "DEV_BASE_BINARY", "value": d.ContainerPath}, map[string]any{"name": "DEV_HEALTH_URL", "value": fmt.Sprintf("http://127.0.0.1:%d%s", d.Port, d.ReadinessPath)})
		volumes = append(volumes, map[string]any{"name": "work", "emptyDir": map[string]any{}}, map[string]any{"name": "dev-cli-runtime", "configMap": map[string]any{"name": name + "-runtime", "defaultMode": 0555}})
		mounts = append(mounts, map[string]any{"name": "work", "mountPath": "/work"}, map[string]any{"name": "dev-cli-runtime", "mountPath": "/dev-cli", "readOnly": true})
		security = map[string]any{"allowPrivilegeEscalation": false, "runAsNonRoot": true, "runAsUser": 65532, "runAsGroup": 65532}
		items = append(items, map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": name + "-runtime", "labels": lbl, "annotations": ann}, "data": map[string]any{"supervise.sh": supervisorScript}})
	}
	app := map[string]any{
		"name": "app", "image": image, "imagePullPolicy": "IfNotPresent", "command": command, "env": env,
		"ports":          []any{map[string]any{"name": "http", "containerPort": d.Port}},
		"readinessProbe": map[string]any{"httpGet": map[string]any{"path": d.ReadinessPath, "port": "http"}, "periodSeconds": 1, "failureThreshold": 15},
		"volumeMounts":   mounts, "resources": map[string]any{"requests": map[string]any{"cpu": "10m", "memory": "32Mi"}, "limits": map[string]any{"cpu": "1", "memory": "512Mi"}}, "securityContext": security,
	}
	deploy := map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]any{"name": name, "labels": lbl, "annotations": ann},
		"spec": map[string]any{
			"replicas": 1,
			"selector": map[string]any{"matchLabels": map[string]any{RouteLabel: route, ServiceLabel: d.Name}},
			"template": map[string]any{
				"metadata": map[string]any{
					"labels":      lbl,
					"annotations": map[string]any{"dev-cli.io/branch-original": branch, "dev-cli.io/source-revision": revision, "dev-cli.io/cli-version": cliVersion},
				},
				"spec": map[string]any{
					"containers": []any{app}, "volumes": volumes,
					"securityContext": map[string]any{"fsGroup": 65532},
				},
			},
		},
	}
	svc := map[string]any{"apiVersion": "v1", "kind": "Service", "metadata": map[string]any{"name": name, "labels": lbl, "annotations": ann}, "spec": map[string]any{"selector": map[string]any{RouteLabel: route, ServiceLabel: d.Name}, "ports": []any{map[string]any{"name": "http", "port": d.Port, "targetPort": "http"}}}}
	items = append(items, deploy, svc)
	payload, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "List", "items": items})
	if _, err := c.RunKubectl(ctx, []string{"apply", "-f", "-"}, bytes.NewReader(payload)); err != nil {
		return err
	}
	_, err := c.RunKubectl(ctx, []string{"rollout", "status", "deployment/" + name, "--timeout=90s"}, nil)
	return err
}

// ApplyRoutes creates one independent route per deployable. Affected services
// use an overlay while unaffected services use their always-running base.
func (c Client) ApplyRoutes(ctx context.Context, cfg config.Config, deployables []config.Deployable, affected map[string]bool, owner, branch, revision, baseRef, baseRevision, route string, expiry time.Time) error {
	created, items := time.Now().UTC().Format(time.RFC3339), []any{}
	for _, d := range deployables {
		if d.PublicPath == "" && d.InternalHost == "" {
			continue
		}
		backend := d.Name
		backendRefs := []any{map[string]any{"name": backend, "port": d.Port}}
		if affected[d.Name] {
			failover, refs := c.failoverResources(d, owner, branch, revision, baseRef, baseRevision, route, expiry, created)
			items = append(items, failover...)
			backendRefs = refs
		}
		matches, hostnames, filters := []any{}, []any{}, []any{}
		if d.PublicPath != "" {
			hostnames = append(hostnames, cfg.IngressHost)
			matches = append(matches, map[string]any{"path": map[string]any{"type": "PathPrefix", "value": d.PublicPath}, "headers": []any{map[string]any{"name": "Cookie", "type": "RegularExpression", "value": "(^|.*;[ ]*)" + cfg.CookieName + "=" + route + "(;.*|$)"}}})
			filters = append(filters, map[string]any{"type": "RequestHeaderModifier", "requestHeaderModifier": map[string]any{"set": []any{map[string]any{"name": "x-dev-branch", "value": route}}}})
			// URLs are shared by all branches: never reuse another cookie's response.
			filters = append(filters, map[string]any{"type": "ResponseHeaderModifier", "responseHeaderModifier": map[string]any{"set": []any{map[string]any{"name": "Cache-Control", "value": "no-store"}, map[string]any{"name": "Vary", "value": "Cookie"}}}})
		} else {
			hostnames = append(hostnames, d.InternalHost)
			matches = append(matches, map[string]any{"headers": []any{map[string]any{"name": "x-dev-branch", "type": "Exact", "value": route}}})
		}
		items = append(items, map[string]any{
			"apiVersion": "gateway.networking.k8s.io/v1",
			"kind":       "HTTPRoute",
			"metadata": map[string]any{
				"name": naming.Resource("route-"+d.Name, route), "labels": labels(owner, route, d.Name),
				"annotations": annotations(owner, branch, revision, baseRef, baseRevision, "", expiry, created),
			},
			"spec": map[string]any{
				"parentRefs": []any{map[string]any{"name": cfg.GatewayName, "namespace": cfg.GatewayNamespace}},
				"hostnames":  hostnames,
				"rules": []any{map[string]any{
					"matches": matches, "filters": filters,
					"backendRefs": backendRefs,
				}},
			},
		})
	}
	if len(items) == 0 {
		return nil
	}
	payload, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "List", "items": items})
	_, err := c.RunKubectl(ctx, []string{"apply", "-f", "-"}, bytes.NewReader(payload))
	return err
}

func (c Client) Pod(ctx context.Context, d config.Deployable, route string) (string, error) {
	name, _, err := c.PodRuntime(ctx, d, route)
	return name, err
}

func (c Client) PodRuntime(ctx context.Context, d config.Deployable, route string) (string, string, error) {
	out, err := c.RunKubectl(ctx, []string{"get", "pods", "-l", RouteLabel + "=" + route + "," + ServiceLabel + "=" + d.Name, "-o", "json"}, nil)
	if err != nil {
		return "", "", err
	}
	var pods struct {
		Items []struct {
			Metadata struct {
				Name              string     `json:"name"`
				UID               string     `json:"uid"`
				DeletionTimestamp *time.Time `json:"deletionTimestamp"`
			} `json:"metadata"`
			Status struct {
				ContainerStatuses []struct {
					Name         string `json:"name"`
					RestartCount int    `json:"restartCount"`
				} `json:"containerStatuses"`
				Phase      string `json:"phase"`
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &pods); err != nil {
		return "", "", err
	}
	for _, pod := range pods.Items {
		if pod.Metadata.DeletionTimestamp != nil || pod.Status.Phase != "Running" {
			continue
		}
		for _, condition := range pod.Status.Conditions {
			if condition.Type == "Ready" && condition.Status == "True" {
				identity := pod.Metadata.UID
				for _, container := range pod.Status.ContainerStatuses {
					if container.Name == "app" {
						identity += ":" + strconv.Itoa(container.RestartCount)
					}
				}
				return pod.Metadata.Name, identity, nil
			}
		}
	}
	if len(pods.Items) == 0 {
		return "", "", fmt.Errorf("no pod for %s", d.Name)
	}
	return "", "", fmt.Errorf("no ready running pod for %s", d.Name)
}

func (c Client) SyncFile(ctx context.Context, pod, local, remote string) error {
	if _, err := c.RunKubectl(ctx, []string{"exec", pod, "-c", "app", "--", "mkdir", "-p", filepath.Dir(remote)}, nil); err != nil {
		return err
	}
	_, err := c.RunKubectl(ctx, []string{"cp", local, pod + ":" + remote, "-c", "app"}, nil)
	return err
}

func (c Client) SyncFiles(ctx context.Context, pod, root string, paths []string, knownPaths ...string) error {
	// This manifest records only paths previously copied by this CLI. Never
	// prune unrelated application files or the dev server's dependency tree.
	previous, err := c.execShell(ctx, pod, "if [ -f /tmp/dev-cli-synced-files ]; then cat /tmp/dev-cli-synced-files; fi")
	if err != nil {
		return err
	}
	removed, err := removedSyncPaths(string(previous)+"\n"+strings.Join(knownPaths, "\n"), paths)
	if err != nil {
		return err
	}
	var payload bytes.Buffer
	tw := tar.NewWriter(&payload)
	for _, rel := range paths {
		local := filepath.Join(root, rel)
		info, err := os.Lstat(local)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("sync requires a regular file: %s", rel)
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		file, err := os.Open(local)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(tw, file)
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if _, err := c.RunKubectl(ctx, []string{"exec", "-i", pod, "-c", "app", "--", "tar", "-x", "-C", "/workspace"}, bytes.NewReader(payload.Bytes())); err != nil {
		return err
	}
	args := []string{"exec", "-i", pod, "-c", "app", "--", "/bin/sh", "-c", `set -eu
for path do rm -f -- "/workspace/$path"; done
cat > /tmp/dev-cli-synced-files.next
mv /tmp/dev-cli-synced-files.next /tmp/dev-cli-synced-files`, "dev-cli-sync"}
	_, err = c.RunKubectl(ctx, append(args, removed...), strings.NewReader(strings.Join(paths, "\n")+"\n"))
	return err
}

func removedSyncPaths(previous string, paths []string) ([]string, error) {
	current := make(map[string]bool, len(paths))
	valid := func(p string) bool {
		return filepath.IsLocal(p) && filepath.Clean(p) == p && p != "." && !strings.ContainsAny(p, "\n\r\\")
	}
	for _, p := range paths {
		if !valid(p) {
			return nil, fmt.Errorf("unsafe sync path %q", p)
		}
		current[p] = true
	}
	var removed []string
	for _, p := range strings.Split(strings.TrimSuffix(previous, "\n"), "\n") {
		if p == "" {
			continue
		}
		if !valid(p) {
			return nil, fmt.Errorf("unsafe recorded sync path %q", p)
		}
		if !current[p] {
			removed = append(removed, p)
		}
	}
	return removed, nil
}

func (c Client) execShell(ctx context.Context, pod, script string) ([]byte, error) {
	return c.RunKubectl(ctx, []string{"exec", pod, "-c", "app", "--", "/bin/sh", "-c", script}, nil)
}
func (c Client) generation(ctx context.Context, pod string) int {
	out, err := c.execShell(ctx, pod, "cat /work/generation 2>/dev/null || echo 0")
	if err != nil {
		return 0
	}
	v, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return v
}
func (c Client) waitReady(ctx context.Context, pod string, after int, healthURL string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		script := fmt.Sprintf("test $(cat /work/generation 2>/dev/null || echo 0) -gt %d && wget -q -T 1 -O /dev/null %s", after, healthURL)
		if _, err := c.execShell(ctx, pod, script); err == nil {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("process did not become ready within %s", timeout)
}
func (c Client) SyncBinary(ctx context.Context, pod, local, name, healthURL string) error {
	before := c.generation(ctx, pod)
	// The supervisor must never observe a binary while it is being copied.
	// Interrupted uploads are inert; only a completed upload becomes next.
	upload := "/work/upload-" + rand.Text()
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, _ = c.execShell(cleanup, pod, "rm -f "+upload)
	}()
	if err := c.SyncFile(ctx, pod, local, upload); err != nil {
		return err
	}
	if _, err := c.execShell(ctx, pod, "chmod 0555 "+upload+" && mv "+upload+" /work/next && kill -TERM \"$(cat /work/pid)\""); err != nil {
		return err
	}
	if err := c.waitReady(ctx, pod, before, healthURL, 10*time.Second); err == nil {
		return nil
	}
	rollbackGeneration := c.generation(ctx, pod)
	_, _ = c.execShell(ctx, pod, "touch /work/rollback; kill -TERM \"$(cat /work/pid)\" 2>/dev/null || true")
	if rollbackErr := c.waitReady(ctx, pod, rollbackGeneration, healthURL, 10*time.Second); rollbackErr != nil {
		return fmt.Errorf("%s update failed and rollback was not ready: %w", name, rollbackErr)
	}
	return fmt.Errorf("%s update failed readiness; restored last working binary", name)
}

func (c Client) Down(ctx context.Context, owner, route string) error {
	sel := ManagedLabel + "=dev-cli," + OwnerLabel + "=" + naming.Slug(owner, 40) + "," + RouteLabel + "=" + route
	_, err := c.RunKubectl(ctx, []string{"delete", ownedResources, "-l", sel, "--ignore-not-found=true", "--wait=true", "--timeout=30s"}, nil)
	return err
}

func (c Client) RecordSync(ctx context.Context, route, service string, syncErr error) error {
	args := []string{"annotate", "deployment", "-l", ManagedLabel + "=dev-cli," + RouteLabel + "=" + route + "," + ServiceLabel + "=" + service, "--overwrite"}
	if syncErr != nil {
		message := syncErr.Error()
		if len(message) > 2048 {
			message = message[:2048]
		}
		args = append(args, "dev-cli.io/sync-health=error", "dev-cli.io/sync-error="+message)
	} else {
		args = append(args, "dev-cli.io/sync-health=healthy", "dev-cli.io/sync-error-", "dev-cli.io/last-sync-at="+time.Now().UTC().Format(time.RFC3339))
	}
	_, err := c.RunKubectl(ctx, args, nil)
	return err
}

func (c Client) Heartbeat(ctx context.Context, route string, ttl time.Duration) error {
	_, err := c.RunKubectl(ctx, []string{"annotate", ownedResources, "-l", ManagedLabel + "=dev-cli," + RouteLabel + "=" + route, "--overwrite", "dev-cli.io/last-seen-at=" + time.Now().UTC().Format(time.RFC3339), "dev-cli.io/expires-at=" + time.Now().Add(ttl).UTC().Format(time.RFC3339)}, nil)
	return err
}
func (c Client) Logs(ctx context.Context, route, service string, stdout, stderr io.Writer) error {
	sel := ManagedLabel + "=dev-cli," + RouteLabel + "=" + route
	if service != "" {
		sel += "," + ServiceLabel + "=" + service
	}
	return c.Run.Stream(ctx, "kubectl", c.args("logs", "-l", sel, "-c", "app", "--prefix=true", "--tail=200", "-f"), nil, stdout, stderr)
}

type ObjectList struct {
	Items []struct {
		Kind     string `json:"kind"`
		Metadata struct {
			Generation  int64             `json:"generation"`
			Name        string            `json:"name"`
			Creation    string            `json:"creationTimestamp"`
			Labels      map[string]string `json:"labels"`
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
		Status struct {
			ReadyReplicas int `json:"readyReplicas"`
			Parents       []struct {
				Conditions []struct {
					Type               string `json:"type"`
					Status             string `json:"status"`
					ObservedGeneration int64  `json:"observedGeneration"`
				} `json:"conditions"`
			} `json:"parents"`
		} `json:"status"`
	} `json:"items"`
}

func (c Client) List(ctx context.Context, selector string) (ObjectList, error) {
	out, err := c.RunKubectl(ctx, []string{"get", "deployments", "-l", selector, "-o", "json"}, nil)
	if err != nil {
		return ObjectList{}, err
	}
	var v ObjectList
	err = json.Unmarshal(out, &v)
	return v, err
}
func (c Client) ListRoutes(ctx context.Context, selector string) (ObjectList, error) {
	out, err := c.RunKubectl(ctx, []string{"get", "httproutes", "-l", selector, "-o", "json"}, nil)
	if err != nil {
		return ObjectList{}, err
	}
	var v ObjectList
	err = json.Unmarshal(out, &v)
	return v, err
}
func (c Client) Cleanup(ctx context.Context, now time.Time) (int, error) {
	out, err := c.RunKubectl(ctx, []string{"get", ownedResources, "-l", ManagedLabel + "=dev-cli", "-o", "json"}, nil)
	if err != nil {
		return 0, err
	}
	var v ObjectList
	if err := json.Unmarshal(out, &v); err != nil {
		return 0, err
	}
	expiries := map[string]time.Time{}
	for _, i := range v.Items {
		exp, err := time.Parse(time.RFC3339, i.Metadata.Annotations["dev-cli.io/expires-at"])
		route := i.Metadata.Labels[RouteLabel]
		if err == nil && route != "" && exp.After(expiries[route]) {
			expiries[route] = exp
		}
	}
	removed := 0
	for route, expiry := range expiries {
		if !now.After(expiry) {
			continue
		}
		if _, err := c.RunKubectl(ctx, []string{"delete", ownedResources, "-l", ManagedLabel + "=dev-cli," + RouteLabel + "=" + route, "--ignore-not-found=true"}, nil); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

func (v ObjectList) RoutesReady() bool {
	if len(v.Items) == 0 {
		return false
	}
	for _, route := range v.Items {
		ready := false
		for _, parent := range route.Status.Parents {
			accepted, resolved := false, false
			for _, condition := range parent.Conditions {
				if condition.Status != "True" || condition.ObservedGeneration != route.Metadata.Generation {
					continue
				}
				if condition.Type == "Accepted" {
					accepted = true
				}
				if condition.Type == "ResolvedRefs" {
					resolved = true
				}
			}
			ready = ready || accepted && resolved
		}
		if !ready {
			return false
		}
	}
	return true
}

func (c Client) WaitRoutes(ctx context.Context, route string) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	for {
		v, err := c.ListRoutes(ctx, ManagedLabel+"=dev-cli,"+RouteLabel+"="+route)
		if err != nil {
			return err
		}
		if v.RoutesReady() {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("branch routes were not accepted with resolved backends: %w", ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }
