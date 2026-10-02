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
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/antonve/dev-cli/internal/config"
	"github.com/antonve/dev-cli/internal/deeplink"
	"github.com/antonve/dev-cli/internal/document"
	"github.com/antonve/dev-cli/internal/execx"
	"github.com/antonve/dev-cli/internal/naming"
)

const (
	ManagedLabel   = "dev-cli.io/managed-by"
	RouteLabel     = "dev-cli.io/route"
	OwnerLabel     = "dev-cli.io/owner"
	ServiceLabel   = "dev-cli.io/service"
	cliVersion     = "v0.5.1"
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
  /work/current "$@" &
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

func (c Client) In(namespace string) Client { c.Namespace = namespace; return c }

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

func stringMap(v map[string]any) map[string]any {
	out := make(map[string]any, len(v))
	for k, value := range v {
		out[k] = value
	}
	return out
}

func mergeNamed(values []any, additions ...map[string]any) []any {
	for _, addition := range additions {
		name, _ := addition["name"].(string)
		replaced := false
		for i, value := range values {
			item, _ := value.(map[string]any)
			if item["name"] == name {
				values[i], replaced = addition, true
				break
			}
		}
		if !replaced {
			values = append(values, addition)
		}
	}
	return values
}

func loadPodTemplate(path, route, namespace string) (map[string]any, error) {
	if path == "" {
		return map[string]any{}, nil
	}
	b, err := readRepoFile(path)
	if err != nil {
		return nil, fmt.Errorf("read workload template %s: %w", path, err)
	}
	b = []byte(strings.NewReplacer("${DEV_ROUTE}", route, "${DEV_NAMESPACE}", namespace).Replace(string(b)))
	var template map[string]any
	if err := document.Unmarshal(b, &template); err != nil {
		return nil, fmt.Errorf("parse workload template %s: %w", path, err)
	}
	if template["apiVersion"] != nil || template["kind"] != nil {
		return nil, fmt.Errorf("workload template %s must be a PodTemplateSpec YAML or JSON object", path)
	}
	return template, nil
}

func readRepoFile(path string) ([]byte, error) {
	if !filepath.IsLocal(path) {
		return nil, fmt.Errorf("path must be repository-relative: %q", path)
	}
	root, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(root, path))
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(resolvedRoot, resolved)
	if err != nil || !filepath.IsLocal(rel) {
		return nil, fmt.Errorf("path escapes repository: %q", path)
	}
	return os.ReadFile(resolved)
}

func (c Client) writeOwnedObjects(ctx context.Context, items []any, owner, route string) error {
	creates, updates := []any{}, []any{}
	for _, value := range items {
		object := value.(map[string]any)
		kind := strings.ToLower(object["kind"].(string))
		metadata := object["metadata"].(map[string]any)
		name := metadata["name"].(string)
		out, err := c.RunKubectl(ctx, []string{"get", kind, name, "--ignore-not-found=true", "-o", "json"}, nil)
		if err != nil {
			return err
		}
		if len(bytes.TrimSpace(out)) == 0 {
			creates = append(creates, object)
			continue
		}
		if err := c.ensureOwnedJSON(out, kind, name, owner, route); err != nil {
			return err
		}
		var current map[string]any
		if err := json.Unmarshal(out, &current); err != nil {
			return err
		}
		currentMetadata, _ := current["metadata"].(map[string]any)
		resourceVersion, _ := currentMetadata["resourceVersion"].(string)
		if resourceVersion == "" {
			return fmt.Errorf("existing %s/%s has no resourceVersion", kind, name)
		}
		metadata["resourceVersion"] = resourceVersion
		if kind == "service" {
			currentSpec, _ := current["spec"].(map[string]any)
			desiredSpec, _ := object["spec"].(map[string]any)
			for _, field := range []string{"clusterIP", "clusterIPs", "ipFamilies", "ipFamilyPolicy", "healthCheckNodePort"} {
				if value, ok := currentSpec[field]; ok {
					desiredSpec[field] = value
				}
			}
		}
		updates = append(updates, object)
	}
	for _, write := range []struct {
		verb  string
		items []any
	}{{"create", creates}, {"replace", updates}} {
		if len(write.items) == 0 {
			continue
		}
		payload, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "List", "items": write.items})
		if _, err := c.RunKubectl(ctx, []string{write.verb, "-f", "-"}, bytes.NewReader(payload)); err != nil {
			return err
		}
	}
	return nil
}

func (c Client) ensureOwnedJSON(out []byte, resource, name, owner, route string) error {
	var object struct {
		Metadata struct {
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(out, &object); err != nil {
		return fmt.Errorf("inspect existing %s/%s: %w", resource, name, err)
	}
	if object.Metadata.Labels[ManagedLabel] != "dev-cli" || object.Metadata.Labels[OwnerLabel] != naming.Slug(owner, 40) || object.Metadata.Labels[RouteLabel] != route {
		return fmt.Errorf("refusing to adopt %s/%s: ownership labels do not match", resource, name)
	}
	return nil
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
	c = c.In(d.WorkloadNamespace(cfg))
	if cfg.InternalGateway == "" {
		cfg.InternalGateway = "http://dev-cli-gateway." + cfg.Namespace + ".svc.cluster.local"
	}
	name := naming.Resource(d.Name, route)
	created := time.Now().UTC().Format(time.RFC3339)
	if existing, err := c.RunKubectl(ctx, []string{"get", "deployment", name, "-o", "jsonpath={.metadata.annotations.dev-cli\\.io/created-at}"}, nil); err == nil && strings.TrimSpace(string(existing)) != "" {
		created = strings.TrimSpace(string(existing))
	}
	ann, lbl := annotations(owner, branch, revision, baseRef, baseRevision, image, expiry, created), labels(owner, route, d.Name)
	ann["dev-cli.io/dev-container"] = d.Container()
	ann["dev-cli.io/kind"] = d.Kind
	env := []any{
		map[string]any{"name": "DEV_BRANCH", "value": branch}, map[string]any{"name": "DEV_REVISION", "value": revision},
		map[string]any{"name": "DEV_NAMESPACE", "value": c.Namespace}, map[string]any{"name": "DEV_INTERNAL_GATEWAY", "value": cfg.InternalGateway},
	}
	template, err := loadPodTemplate(d.WorkloadTemplate, route, c.Namespace)
	if err != nil {
		return err
	}
	metadata, _ := template["metadata"].(map[string]any)
	podSpec, _ := template["spec"].(map[string]any)
	if podSpec == nil {
		podSpec = map[string]any{}
	} else {
		podSpec = stringMap(podSpec)
	}
	if _, exists := podSpec["initContainers"]; exists {
		return fmt.Errorf("workload template %s contains initContainers; declare migrations/seeds as tasks", d.WorkloadTemplate)
	}
	containers, _ := podSpec["containers"].([]any)
	containerName := d.Container()
	containerIndex := -1
	for i, value := range containers {
		item, _ := value.(map[string]any)
		if item["name"] == containerName {
			containerIndex = i
			break
		}
	}
	if d.WorkloadTemplate != "" && containerIndex < 0 {
		return fmt.Errorf("workload template %s has no designated dev container %q", d.WorkloadTemplate, containerName)
	}
	app := map[string]any{"name": containerName}
	if containerIndex >= 0 {
		app = stringMap(containers[containerIndex].(map[string]any))
	}
	command := []any{}
	if existing, ok := app["command"].([]any); ok {
		command = existing
	}
	if len(d.DevCommand) > 0 {
		command = make([]any, len(d.DevCommand))
		for i := range d.DevCommand {
			command[i] = d.DevCommand[i]
		}
	}
	volumes, mounts, items := []any{}, []any{}, []any{}
	security, _ := app["securityContext"].(map[string]any)
	if security == nil {
		security = map[string]any{"allowPrivilegeEscalation": false, "runAsNonRoot": true, "runAsUser": 1000, "runAsGroup": 1000}
	}
	if d.Kind != "frontend" {
		command = []any{"/bin/sh", "/dev-cli/supervise.sh"}
		env = append(env, map[string]any{"name": "DEV_BASE_BINARY", "value": d.ContainerPath})
		if d.Kind == "backend" || d.Kind == "worker" && d.Port > 0 {
			env = append(env, map[string]any{"name": "DEV_HEALTH_URL", "value": fmt.Sprintf("http://127.0.0.1:%d%s", d.Port, d.ReadinessPath)})
		}
		volumes = append(volumes, map[string]any{"name": "work", "emptyDir": map[string]any{}}, map[string]any{"name": "dev-cli-runtime", "configMap": map[string]any{"name": name + "-runtime", "defaultMode": 0555}})
		mounts = append(mounts, map[string]any{"name": "work", "mountPath": "/work"}, map[string]any{"name": "dev-cli-runtime", "mountPath": "/dev-cli", "readOnly": true})
		if _, exists := app["securityContext"]; !exists {
			security = map[string]any{"allowPrivilegeEscalation": false, "runAsNonRoot": true, "runAsUser": 65532, "runAsGroup": 65532}
		}
		items = append(items, map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": name + "-runtime", "labels": lbl, "annotations": ann}, "data": map[string]any{"supervise.sh": supervisorScript}})
	}
	existingEnv, _ := app["env"].([]any)
	for _, value := range env {
		existingEnv = mergeNamed(existingEnv, value.(map[string]any))
	}
	env = existingEnv
	existingMounts, _ := app["volumeMounts"].([]any)
	for _, value := range mounts {
		existingMounts = mergeNamed(existingMounts, value.(map[string]any))
	}
	mounts = existingMounts
	app["image"], app["imagePullPolicy"], app["command"], app["env"], app["volumeMounts"], app["securityContext"] = image, "IfNotPresent", command, env, mounts, security
	if _, ok := app["ports"]; !ok && (d.Kind != "worker" || d.Port > 0) {
		app["ports"] = []any{map[string]any{"name": "http", "containerPort": d.Port}}
	}
	if _, ok := app["readinessProbe"]; !ok {
		if d.Kind == "worker" && d.Port == 0 {
			app["readinessProbe"] = map[string]any{"exec": map[string]any{"command": []any{"/bin/sh", "-c", "generation=$(cat /work/generation) && pid=$(cat /work/pid) && kill -0 \"$pid\" && sleep 1 && test \"$(cat /work/generation)\" = \"$generation\" && test \"$(cat /work/pid)\" = \"$pid\" && kill -0 \"$pid\""}}, "periodSeconds": 1, "timeoutSeconds": 3, "failureThreshold": 15}
		} else {
			app["readinessProbe"] = map[string]any{"httpGet": map[string]any{"path": d.ReadinessPath, "port": d.Port}, "periodSeconds": 1, "failureThreshold": 15}
		}
	}
	if _, ok := app["resources"]; !ok {
		app["resources"] = map[string]any{"requests": map[string]any{"cpu": "10m", "memory": "32Mi"}, "limits": map[string]any{"cpu": "1", "memory": "512Mi"}}
	}
	if containerIndex < 0 {
		containers = append(containers, app)
	} else {
		containers[containerIndex] = app
	}
	existingVolumes := anySlice(podSpec["volumes"])
	for _, value := range volumes {
		existingVolumes = mergeNamed(existingVolumes, value.(map[string]any))
	}
	podSpec["containers"], podSpec["volumes"] = containers, existingVolumes
	if _, ok := podSpec["securityContext"]; !ok {
		podSpec["securityContext"] = map[string]any{"fsGroup": 65532}
	}
	podAnnotations := map[string]any{}
	if metadata != nil {
		if original, ok := metadata["annotations"].(map[string]any); ok {
			podAnnotations = stringMap(original)
		}
	}
	podAnnotations["dev-cli.io/branch-original"], podAnnotations["dev-cli.io/source-revision"], podAnnotations["dev-cli.io/cli-version"] = branch, revision, cliVersion
	deploy := map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]any{"name": name, "labels": lbl, "annotations": ann},
		"spec": map[string]any{
			"replicas": 1,
			"selector": map[string]any{"matchLabels": map[string]any{RouteLabel: route, ServiceLabel: d.Name}},
			"template": map[string]any{"metadata": map[string]any{"labels": lbl, "annotations": podAnnotations}, "spec": podSpec},
		},
	}
	items = append(items, deploy)
	if d.Kind != "worker" {
		svc := map[string]any{"apiVersion": "v1", "kind": "Service", "metadata": map[string]any{"name": name, "labels": lbl, "annotations": ann}, "spec": map[string]any{"selector": map[string]any{RouteLabel: route, ServiceLabel: d.Name}, "ports": []any{map[string]any{"name": "http", "port": d.OverlayPort(), "targetPort": d.Port}}}}
		items = append(items, svc)
	}
	if err := c.writeOwnedObjects(ctx, items, owner, route); err != nil {
		return err
	}
	if d.Kind == "frontend" {
		ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
		selector := RouteLabel + "=" + route + "," + ServiceLabel + "=" + d.Name
		// Deployment creation can return before its ReplicaSet creates a Pod.
		if _, err := c.RunKubectl(ctx, []string{"wait", "pod", "-l", selector, "--for=create", "--timeout=90s"}, nil); err != nil {
			return err
		}
		_, err = c.RunKubectl(ctx, []string{"wait", "pod", "-l", selector, "--for=jsonpath={.status.phase}=Running", "--timeout=90s"}, nil)
		return err
	}
	return c.WaitOverlay(ctx, d, route)
}

func anySlice(v any) []any { values, _ := v.([]any); return values }

// ApplyRoutes creates one independent route per deployable. Affected services
// use an overlay while unaffected services use their always-running base.
func (c Client) ApplyRoutes(ctx context.Context, cfg config.Config, deployables []config.Deployable, affected map[string]bool, owner, branch, revision, baseRef, baseRevision, route string, expiry time.Time) error {
	if cfg.Namespace == "" {
		cfg.Namespace = c.Namespace
	}
	created, items := time.Now().UTC().Format(time.RFC3339), []any{}
	ttl := 8 * time.Hour
	if cfg.TTL != "" {
		var err error
		ttl, err = time.ParseDuration(cfg.TTL)
		if err != nil || ttl < time.Second {
			return fmt.Errorf("ttl must be a duration of at least 1s")
		}
	}
	for _, d := range deployables {
		if d.Kind == "worker" || d.PublicPath == "" && d.InternalHost == "" {
			continue
		}
		baseRefs := c.serviceBackendRefs(cfg, d, d.Base(cfg), "base-", owner, branch, revision, baseRef, baseRevision, route, expiry, created, &items)
		backendRefs := baseRefs
		dataRouteName := naming.Resource("route-"+d.Name, route)
		publicRouteName := dataRouteName
		if d.InternalHost != "" && d.PublicPath != "" {
			publicRouteName = naming.Resource("route-public-"+d.Name, route)
		}
		directRoutes := []string{}
		if d.InternalHost != "" {
			directRoutes = append(directRoutes, dataRouteName)
		}
		if d.PublicPath != "" && d.Proxy(cfg).Name == "" {
			directRoutes = append(directRoutes, publicRouteName)
		}
		if affected[d.Name] {
			failover, refs := c.failoverResources(cfg, d, directRoutes, owner, branch, revision, baseRef, baseRevision, route, expiry, created)
			items = append(items, failover...)
			backendRefs = refs
		}
		if d.InternalHost != "" {
			match := map[string]any{"headers": []any{map[string]any{"name": "x-dev-branch", "type": "Exact", "value": route}}}
			rules := []any{map[string]any{"matches": []any{match}, "backendRefs": backendRefs, "filters": routeHeaderFilters(route, false, false)}}
			items = append(items, routeObject(cfg, d, dataRouteName, []any{d.InternalHost}, rules, owner, branch, revision, baseRef, baseRevision, route, expiry, created))
		}
		if d.PublicPath != "" {
			publicRefs, publicBaseRefs := backendRefs, baseRefs
			if proxy := d.Proxy(cfg); proxy.Name != "" {
				publicRefs = c.serviceBackendRefs(cfg, d, proxy, "proxy-", owner, branch, revision, baseRef, baseRevision, route, expiry, created, &items)
				publicBaseRefs = publicRefs
			}
			matches := []any{map[string]any{"path": map[string]any{"type": "PathPrefix", "value": d.PublicPath}, "headers": []any{map[string]any{"name": "Cookie", "type": "RegularExpression", "value": "(^|.*;[ ]*)" + regexp.QuoteMeta(cfg.CookieName) + "=" + regexp.QuoteMeta(route) + "(;.*|$)"}}}}
			filters := routeHeaderFilters(route, true, d.Proxy(cfg).Name != "")
			rules := []any{map[string]any{"matches": matches, "filters": filters, "backendRefs": publicRefs}}
			rules = append(rules, selectionRule(cfg, d, route, int(ttl.Seconds()), publicRefs))
			// Each owner carries an identical base-selection rule. There is no
			// shared mutable CLI resource and down/TTL remain owner-scoped.
			rules = append(rules, selectionRule(cfg, d, deeplink.Base, 0, publicBaseRefs))
			items = append(items, routeObject(cfg, d, publicRouteName, []any{d.Host(cfg)}, rules, owner, branch, revision, baseRef, baseRevision, route, expiry, created))
		}
	}
	if len(items) == 0 {
		return nil
	}
	return c.writeOwnedObjects(ctx, items, owner, route)
}

func (c Client) serviceBackendRefs(cfg config.Config, d config.Deployable, ref config.ObjectRef, prefix, owner, branch, revision, baseRef, baseRevision, route string, expiry time.Time, created string, items *[]any) []any {
	if ref.Namespace == c.Namespace {
		return []any{map[string]any{"name": ref.Name, "port": ref.Port}}
	}
	name := naming.Resource(prefix+d.Name, route)
	*items = append(*items, map[string]any{"apiVersion": "gateway.envoyproxy.io/v1alpha1", "kind": "Backend", "metadata": map[string]any{"name": name, "labels": labels(owner, route, d.Name), "annotations": annotations(owner, branch, revision, baseRef, baseRevision, "", expiry, created)}, "spec": map[string]any{"endpoints": []any{map[string]any{"fqdn": map[string]any{"hostname": ref.Name + "." + ref.Namespace + ".svc.cluster.local", "port": ref.Port}}}}})
	return []any{map[string]any{"group": "gateway.envoyproxy.io", "kind": "Backend", "name": name, "port": ref.Port}}
}

func routeObject(cfg config.Config, d config.Deployable, name string, hostnames, rules []any, owner, branch, revision, baseRef, baseRevision, route string, expiry time.Time, created string) map[string]any {
	return map[string]any{"apiVersion": "gateway.networking.k8s.io/v1", "kind": "HTTPRoute", "metadata": map[string]any{"name": name, "labels": labels(owner, route, d.Name), "annotations": annotations(owner, branch, revision, baseRef, baseRevision, "", expiry, created)}, "spec": map[string]any{"parentRefs": []any{map[string]any{"name": cfg.GatewayName, "namespace": cfg.GatewayNamespace}}, "hostnames": hostnames, "rules": rules}}
}

func (c Client) Pod(ctx context.Context, d config.Deployable, route string) (string, error) {
	name, _, err := c.PodRuntime(ctx, d, route)
	return name, err
}

func (c Client) RunningPod(ctx context.Context, d config.Deployable, route string) (string, error) {
	name, _, err := c.podRuntime(ctx, d, route, false)
	return name, err
}

func (c Client) WaitOverlay(ctx context.Context, d config.Deployable, route string) error {
	name := naming.Resource(d.Name, route)
	_, err := c.RunKubectl(ctx, []string{"rollout", "status", "deployment/" + name, "--timeout=90s"}, nil)
	return err
}

func (c Client) PodRuntime(ctx context.Context, d config.Deployable, route string) (string, string, error) {
	return c.podRuntime(ctx, d, route, true)
}
func (c Client) RunningPodRuntime(ctx context.Context, d config.Deployable, route string) (string, string, error) {
	return c.podRuntime(ctx, d, route, false)
}
func (c Client) podRuntime(ctx context.Context, d config.Deployable, route string, requireReady bool) (string, string, error) {
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
		ready := false
		for _, condition := range pod.Status.Conditions {
			if condition.Type == "Ready" && condition.Status == "True" {
				ready = true
			}
		}
		if requireReady && !ready {
			continue
		}
		{
			identity := pod.Metadata.UID
			for _, container := range pod.Status.ContainerStatuses {
				if container.Name == d.Container() {
					identity += ":" + strconv.Itoa(container.RestartCount)
				}
			}
			return pod.Metadata.Name, identity, nil
		}
	}
	if len(pods.Items) == 0 {
		return "", "", fmt.Errorf("no pod for %s", d.Name)
	}
	if requireReady {
		return "", "", fmt.Errorf("no ready running pod for %s", d.Name)
	}
	return "", "", fmt.Errorf("no running pod for %s", d.Name)
}

func (c Client) SyncFile(ctx context.Context, pod, container, local, remote string) error {
	if _, err := c.RunKubectl(ctx, []string{"exec", pod, "-c", container, "--", "mkdir", "-p", filepath.Dir(remote)}, nil); err != nil {
		return err
	}
	_, err := c.RunKubectl(ctx, []string{"cp", local, pod + ":" + remote, "-c", container}, nil)
	return err
}

func (c Client) SyncFiles(ctx context.Context, pod string, d config.Deployable, root string, paths []string, knownPaths ...string) error {
	// This manifest records only paths previously copied by this CLI. Never
	// prune unrelated application files or the dev server's dependency tree.
	previous, err := c.execShell(ctx, pod, d.Container(), "if [ -f /tmp/dev-cli-synced-files ]; then cat /tmp/dev-cli-synced-files; fi")
	if err != nil {
		return err
	}
	targets := make([]string, 0, len(paths))
	knownTargets := make([]string, 0, len(knownPaths))
	for _, path := range paths {
		target, err := syncTarget(path, d.SyncStripPrefix)
		if err != nil {
			return err
		}
		targets = append(targets, target)
	}
	for _, path := range knownPaths {
		target, err := syncTarget(path, d.SyncStripPrefix)
		if err != nil {
			return err
		}
		knownTargets = append(knownTargets, target)
	}
	removed, err := removedSyncPaths(string(previous)+"\n"+strings.Join(knownTargets, "\n"), targets)
	if err != nil {
		return err
	}
	var payload bytes.Buffer
	tw := tar.NewWriter(&payload)
	for i, rel := range paths {
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
		header.Name = targets[i]
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
	args := []string{"exec", "-i", pod, "-c", d.Container(), "--", "/bin/sh", "-c", syncExtractScript, "dev-cli-extract", d.TargetRoot()}
	if _, err := c.RunKubectl(ctx, append(args, targets...), bytes.NewReader(payload.Bytes())); err != nil {
		return err
	}
	args = []string{"exec", "-i", pod, "-c", d.Container(), "--", "/bin/sh", "-c", `set -eu
destination=$1
shift
for path do rm -f -- "$destination/$path"; done
cat > /tmp/dev-cli-synced-files.next
mv /tmp/dev-cli-synced-files.next /tmp/dev-cli-synced-files`, "dev-cli-sync", d.TargetRoot()}
	_, err = c.RunKubectl(ctx, append(args, removed...), strings.NewReader(strings.Join(targets, "\n")+"\n"))
	return err
}

func syncTarget(path, strip string) (string, error) {
	path, strip = filepath.ToSlash(path), strings.Trim(filepath.ToSlash(strip), "/")
	if strip != "" {
		prefix := strip + "/"
		if !strings.HasPrefix(path, prefix) {
			return "", fmt.Errorf("sync path %q is outside syncStripPrefix %q", path, strip)
		}
		path = strings.TrimPrefix(path, prefix)
	}
	if !filepath.IsLocal(path) || path == "." || strings.ContainsAny(path, "\n\r\\") {
		return "", fmt.Errorf("unsafe sync target %q", path)
	}
	return path, nil
}

// Tar timestamps lose subsecond precision. Copy only differing bytes from a
// temporary extraction so rapid same-size edits get fresh filesystem mtimes,
// without touching unchanged Vite configuration and forcing server restarts.
const syncExtractScript = `set -eu
destination=$1
shift
stage=$(mktemp -d /tmp/dev-cli-sync.XXXXXX)
trap 'rm -rf -- "$stage"' EXIT
tar -x -C "$stage"
for path do
  if cmp -s "$stage/$path" "$destination/$path"; then continue; fi
  mkdir -p -- "$destination/$(dirname -- "$path")"
  cp -- "$stage/$path" "$destination/$path"
done`

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

func (c Client) execShell(ctx context.Context, pod, container, script string) ([]byte, error) {
	return c.RunKubectl(ctx, []string{"exec", pod, "-c", container, "--", "/bin/sh", "-c", script}, nil)
}
func (c Client) generation(ctx context.Context, pod, container string) int {
	out, err := c.execShell(ctx, pod, container, "cat /work/generation 2>/dev/null || echo 0")
	if err != nil {
		return 0
	}
	v, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return v
}
func (c Client) waitReady(ctx context.Context, pod, container string, after int, healthURL string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		check := "pid=$(cat /work/pid) && kill -0 \"$pid\" && sleep 1 && test \"$(cat /work/generation)\" = \"$generation\" && test \"$(cat /work/pid)\" = \"$pid\" && kill -0 \"$pid\""
		if healthURL != "" {
			check = "wget -q -T 1 -O /dev/null " + healthURL
		}
		script := fmt.Sprintf("generation=$(cat /work/generation 2>/dev/null || echo 0); test \"$generation\" -gt %d && %s", after, check)
		if _, err := c.execShell(ctx, pod, container, script); err == nil {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("process did not become ready within %s", timeout)
}
func (c Client) SyncBinary(ctx context.Context, pod, container, local, name, healthURL string) error {
	// The supervisor must never observe a binary while it is being copied.
	// Interrupted uploads are inert; only a completed upload becomes next.
	upload := "/work/upload-" + rand.Text()
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, _ = c.execShell(cleanup, pod, container, "rm -f "+upload)
	}()
	if err := c.SyncFile(ctx, pod, container, local, upload); err != nil {
		return err
	}
	comparison, err := c.execShell(ctx, pod, container, "cmp -s "+upload+" /work/current; rc=$?; if [ \"$rc\" -eq 0 ]; then echo same; elif [ \"$rc\" -eq 1 ]; then echo different; else exit \"$rc\"; fi")
	if err != nil {
		return fmt.Errorf("compare uploaded binary with running binary: %w", err)
	}
	if strings.TrimSpace(string(comparison)) == "same" {
		return nil
	}
	if strings.TrimSpace(string(comparison)) != "different" {
		return fmt.Errorf("unexpected binary comparison result %q", strings.TrimSpace(string(comparison)))
	}
	before := c.generation(ctx, pod, container)
	if _, err := c.execShell(ctx, pod, container, "chmod 0555 "+upload+" && mv "+upload+" /work/next && kill -TERM \"$(cat /work/pid)\""); err != nil {
		return err
	}
	if err := c.waitReady(ctx, pod, container, before, healthURL, 10*time.Second); err == nil {
		return nil
	}
	rollbackGeneration := c.generation(ctx, pod, container)
	_, _ = c.execShell(ctx, pod, container, "touch /work/rollback; kill -TERM \"$(cat /work/pid)\" 2>/dev/null || true")
	if rollbackErr := c.waitReady(ctx, pod, container, rollbackGeneration, healthURL, 10*time.Second); rollbackErr != nil {
		return fmt.Errorf("%s update failed and rollback was not ready: %w", name, rollbackErr)
	}
	return fmt.Errorf("%s update failed readiness; restored last working binary", name)
}

func (c Client) Down(ctx context.Context, owner, route string) error {
	sel := ManagedLabel + "=dev-cli," + OwnerLabel + "=" + naming.Slug(owner, 40) + "," + RouteLabel + "=" + route
	if _, err := c.RunKubectl(ctx, []string{"delete", ownedResources, "-l", sel, "--ignore-not-found=true", "--wait=true", "--timeout=30s"}, nil); err != nil {
		return err
	}
	taskSelector := ManagedLabel + "=" + taskManaged + "," + OwnerLabel + "=" + naming.Slug(owner, 40) + "," + RouteLabel + "=" + route
	_, err := c.RunKubectl(ctx, []string{"delete", "jobs", "-l", taskSelector, "--ignore-not-found=true", "--wait=true", "--timeout=30s"}, nil)
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
func (c Client) Logs(ctx context.Context, route, service, container string, stdout, stderr io.Writer) error {
	sel := ManagedLabel + "=dev-cli," + RouteLabel + "=" + route
	if service != "" {
		sel += "," + ServiceLabel + "=" + service
	}
	return c.Run.Stream(ctx, "kubectl", c.args("logs", "-l", sel, "-c", container, "--prefix=true", "--tail=200", "-f"), nil, stdout, stderr)
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
