package kube

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/antonve/dev-cli/internal/config"
	"github.com/antonve/dev-cli/internal/execx"
	"github.com/antonve/dev-cli/internal/naming"
)

const (
	ManagedLabel = "dev-cli.io/managed-by"
	RouteLabel   = "dev-cli.io/route"
	OwnerLabel   = "dev-cli.io/owner"
	ServiceLabel = "dev-cli.io/service"
)

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
	return map[string]any{ManagedLabel: "dev-cli", OwnerLabel: naming.Slug(owner, 40), RouteLabel: route, ServiceLabel: service, "app.kubernetes.io/name": naming.Resource(service, route), "app.kubernetes.io/managed-by": "dev-cli"}
}

func (c Client) ApplyOverlay(ctx context.Context, cfg config.Config, d config.Deployable, owner, branch, revision, base, route string, expiry time.Time) error {
	name := naming.Resource(d.Name, route)
	now := time.Now().UTC().Format(time.RFC3339)
	if existing, err := c.RunKubectl(ctx, []string{"get", "deployment", name, "-o", "jsonpath={.metadata.annotations.dev-cli\\.io/created-at}"}, nil); err == nil && strings.TrimSpace(string(existing)) != "" {
		now = strings.TrimSpace(string(existing))
	}
	ann := map[string]any{"dev-cli.io/owner-original": owner, "dev-cli.io/branch-original": branch, "dev-cli.io/source-revision": revision, "dev-cli.io/base-revision": base, "dev-cli.io/created-at": now, "dev-cli.io/last-sync-at": now, "dev-cli.io/expires-at": expiry.UTC().Format(time.RFC3339), "dev-cli.io/cli-version": "v0.1.0"}
	podAnn := map[string]any{"dev-cli.io/owner-original": owner, "dev-cli.io/branch-original": branch, "dev-cli.io/source-revision": revision, "dev-cli.io/base-revision": base, "dev-cli.io/cli-version": "v0.1.0"}
	lbl := labels(owner, route, d.Name)
	volumeMounts := []any{map[string]any{"name": "work", "mountPath": "/work"}}
	command := []any{}
	if d.Kind == "frontend" {
		volumeMounts = append(volumeMounts, map[string]any{"name": "workspace", "mountPath": "/workspace"})
		command = []any{"/app/hello-web", "--root", "/workspace/apps/hello-web/web", "--fallback-root", "/app/web"}
	} else {
		command = []any{"/app/dev-supervisor", "--binary", d.BinaryPath, "--next", "/work/" + d.Name + ".next", "--health", fmt.Sprintf("http://127.0.0.1:%d%s", d.Port, d.ReadinessPath)}
	}
	app := map[string]any{"name": "app", "image": d.Image, "imagePullPolicy": "IfNotPresent", "command": command, "env": []any{map[string]any{"name": "DEV_BRANCH", "value": branch}, map[string]any{"name": "DEV_REVISION", "value": revision}, map[string]any{"name": "DEV_NAMESPACE", "value": cfg.Namespace}}, "ports": []any{map[string]any{"name": "http", "containerPort": d.Port}, map[string]any{"name": "supervisor", "containerPort": 9000}}, "readinessProbe": map[string]any{"httpGet": map[string]any{"path": d.ReadinessPath, "port": "http"}, "periodSeconds": 1, "failureThreshold": 10}, "volumeMounts": volumeMounts, "resources": map[string]any{"requests": map[string]any{"cpu": "10m", "memory": "24Mi"}, "limits": map[string]any{"cpu": "500m", "memory": "192Mi"}}, "securityContext": map[string]any{"allowPrivilegeEscalation": false, "runAsNonRoot": true, "runAsUser": 65532, "runAsGroup": 65532}}
	syncMounts := []any{map[string]any{"name": "work", "mountPath": "/work"}}
	volumes := []any{map[string]any{"name": "work", "emptyDir": map[string]any{}}}
	if d.Kind == "frontend" {
		syncMounts = append(syncMounts, map[string]any{"name": "workspace", "mountPath": "/workspace"})
		volumes = append(volumes, map[string]any{"name": "workspace", "emptyDir": map[string]any{}})
	}
	sync := map[string]any{"name": "sync", "image": cfg.SyncImage, "command": []any{"sh", "-c", "trap : TERM INT; sleep infinity & wait"}, "volumeMounts": syncMounts, "resources": map[string]any{"requests": map[string]any{"cpu": "2m", "memory": "4Mi"}, "limits": map[string]any{"cpu": "100m", "memory": "32Mi"}}, "securityContext": map[string]any{"allowPrivilegeEscalation": false, "runAsNonRoot": true, "runAsUser": 65532, "runAsGroup": 65532}}
	deploy := map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]any{"name": name, "labels": lbl, "annotations": ann}, "spec": map[string]any{"replicas": 1, "selector": map[string]any{"matchLabels": map[string]any{RouteLabel: route, ServiceLabel: d.Name}}, "template": map[string]any{"metadata": map[string]any{"labels": lbl, "annotations": podAnn}, "spec": map[string]any{"containers": []any{app, sync}, "volumes": volumes, "securityContext": map[string]any{"fsGroup": 65532}}}}}
	svc := map[string]any{"apiVersion": "v1", "kind": "Service", "metadata": map[string]any{"name": name, "labels": lbl, "annotations": ann}, "spec": map[string]any{"selector": map[string]any{RouteLabel: route, ServiceLabel: d.Name}, "ports": []any{map[string]any{"name": "http", "port": d.Port, "targetPort": "http"}}}}
	list := map[string]any{"apiVersion": "v1", "kind": "List", "items": []any{deploy, svc}}
	payload, _ := json.Marshal(list)
	_, err := c.RunKubectl(ctx, []string{"apply", "-f", "-"}, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	_, err = c.RunKubectl(ctx, []string{"rollout", "status", "deployment/" + name, "--timeout=90s"}, nil)
	return err
}

func (c Client) Pod(ctx context.Context, d config.Deployable, route string) (string, error) {
	out, err := c.RunKubectl(ctx, []string{"get", "pods", "-l", RouteLabel + "=" + route + "," + ServiceLabel + "=" + d.Name, "-o", "json"}, nil)
	if err != nil {
		return "", err
	}
	var pods struct {
		Items []struct {
			Metadata struct {
				Name              string     `json:"name"`
				DeletionTimestamp *time.Time `json:"deletionTimestamp"`
			} `json:"metadata"`
			Status struct {
				Phase      string `json:"phase"`
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &pods); err != nil {
		return "", err
	}
	for _, pod := range pods.Items {
		if pod.Metadata.DeletionTimestamp != nil || pod.Status.Phase != "Running" {
			continue
		}
		for _, condition := range pod.Status.Conditions {
			if condition.Type == "Ready" && condition.Status == "True" {
				return pod.Metadata.Name, nil
			}
		}
	}
	if len(pods.Items) == 0 {
		return "", fmt.Errorf("no pod for %s", d.Name)
	}
	return "", fmt.Errorf("no ready running pod for %s", d.Name)
}
func (c Client) SyncFile(ctx context.Context, pod, local, remote string) error {
	dir := filepath.Dir(remote)
	if _, err := c.RunKubectl(ctx, []string{"exec", pod, "-c", "sync", "--", "mkdir", "-p", dir}, nil); err != nil {
		return err
	}
	_, err := c.RunKubectl(ctx, []string{"cp", local, pod + ":" + remote, "-c", "sync"}, nil)
	return err
}

// SyncFiles transfers a frontend update in one Kubernetes exec round trip.
// Paths are archived relative to root and extracted into /workspace by the
// pinned BusyBox sync sidecar.
func (c Client) SyncFiles(ctx context.Context, pod, root string, paths []string) error {
	var payload bytes.Buffer
	tw := tar.NewWriter(&payload)
	for _, rel := range paths {
		local := filepath.Join(root, rel)
		info, err := os.Stat(local)
		if err != nil {
			return err
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
	_, err := c.RunKubectl(ctx, []string{"exec", "-i", pod, "-c", "sync", "--", "tar", "-x", "-C", "/workspace"}, bytes.NewReader(payload.Bytes()))
	return err
}
func (c Client) SyncBinary(ctx context.Context, pod, local, name string) error {
	remote := "/work/" + name + ".next"
	if err := c.SyncFile(ctx, pod, local, remote); err != nil {
		return err
	}
	if _, err := c.RunKubectl(ctx, []string{"exec", pod, "-c", "sync", "--", "chmod", "0555", remote}, nil); err != nil {
		return err
	}
	_, err := c.RunKubectl(ctx, []string{"exec", pod, "-c", "sync", "--", "wget", "-qO-", "--timeout=15", "http://127.0.0.1:9000/reload"}, nil)
	return err
}
func (c Client) Down(ctx context.Context, owner, route string) error {
	sel := ManagedLabel + "=dev-cli," + OwnerLabel + "=" + naming.Slug(owner, 40) + "," + RouteLabel + "=" + route
	_, err := c.RunKubectl(ctx, []string{"delete", "deployment,service", "-l", sel, "--ignore-not-found=true", "--wait=true", "--timeout=30s"}, nil)
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
			Name        string            `json:"name"`
			Creation    string            `json:"creationTimestamp"`
			Labels      map[string]string `json:"labels"`
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
		Status struct {
			ReadyReplicas int `json:"readyReplicas"`
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
func (c Client) Cleanup(ctx context.Context, now time.Time) (int, error) {
	v, err := c.List(ctx, ManagedLabel+"=dev-cli")
	if err != nil {
		return 0, err
	}
	routes := map[string]bool{}
	for _, i := range v.Items {
		exp, err := time.Parse(time.RFC3339, i.Metadata.Annotations["dev-cli.io/expires-at"])
		if err == nil && now.After(exp) {
			routes[i.Metadata.Labels[RouteLabel]] = true
		}
	}
	for r := range routes {
		if _, err := c.RunKubectl(ctx, []string{"delete", "deployment,service", "-l", ManagedLabel + "=dev-cli," + RouteLabel + "=" + r, "--ignore-not-found=true"}, nil); err != nil {
			return len(routes), err
		}
	}
	return len(routes), nil
}
func ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }
