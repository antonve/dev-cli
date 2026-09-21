package kube

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/antonve/dev-cli/internal/config"
	"github.com/antonve/dev-cli/internal/naming"
)

const (
	dependencyManaged = "dev-cli-dependency"
	taskManaged       = "dev-cli-task"
	lockManaged       = "dev-cli-task-lock"
)

func readObjects(path, route, namespace string) ([]map[string]any, error) {
	b, err := readRepoFile(path)
	if err != nil {
		return nil, fmt.Errorf("read manifest %s: %w", path, err)
	}
	b = []byte(strings.NewReplacer("${DEV_ROUTE}", route, "${DEV_NAMESPACE}", namespace).Replace(string(b)))
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("parse manifest %s: %w", path, err)
	}
	if raw["kind"] == "List" {
		items, ok := raw["items"].([]any)
		if !ok {
			return nil, fmt.Errorf("manifest %s List has invalid items", path)
		}
		objects := make([]map[string]any, 0, len(items))
		for _, item := range items {
			object, ok := item.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("manifest %s contains a non-object item", path)
			}
			objects = append(objects, object)
		}
		return objects, nil
	}
	return []map[string]any{raw}, nil
}

func prepareObjects(objects []map[string]any, namespace, managed, owner, route, service string) error {
	for _, object := range objects {
		kind, _ := object["kind"].(string)
		if kind == "" || object["apiVersion"] == nil {
			return fmt.Errorf("manifest object requires apiVersion and kind")
		}
		if kind == "Secret" {
			return fmt.Errorf("Secret bodies are not accepted; reference a pre-existing Secret")
		}
		metadata, _ := object["metadata"].(map[string]any)
		if metadata == nil {
			return fmt.Errorf("%s requires metadata", kind)
		}
		name, _ := metadata["name"].(string)
		if name == "" {
			return fmt.Errorf("%s requires metadata.name", kind)
		}
		declared, _ := metadata["namespace"].(string)
		if declared != "" && declared != namespace {
			return fmt.Errorf("%s/%s namespace %q is outside declared scope %q", kind, name, declared, namespace)
		}
		metadata["namespace"] = namespace
		original, _ := metadata["labels"].(map[string]any)
		if original == nil {
			original = map[string]any{}
		}
		for key, value := range labels(owner, route, service) {
			original[key] = value
		}
		original[ManagedLabel] = managed
		metadata["labels"] = original
	}
	return nil
}

func objectIdentity(object map[string]any) (string, string) {
	kind, _ := object["kind"].(string)
	metadata, _ := object["metadata"].(map[string]any)
	name, _ := metadata["name"].(string)
	return strings.ToLower(kind), name
}

func (c Client) ensureManaged(ctx context.Context, kind, name, managed, owner, route string) error {
	out, err := c.RunKubectl(ctx, []string{"get", kind, name, "--ignore-not-found=true", "-o", "json"}, nil)
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return nil
	}
	var object struct {
		Metadata struct {
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(out, &object); err != nil {
		return err
	}
	labels := object.Metadata.Labels
	if labels[ManagedLabel] != managed || labels[OwnerLabel] != naming.Slug(owner, 40) || labels[RouteLabel] != route {
		return fmt.Errorf("refusing to mutate %s/%s: ownership labels do not match", kind, name)
	}
	return nil
}

func (c Client) Provision(ctx context.Context, d config.Dependency, owner, route string) error {
	c = c.In(d.Namespace)
	objects, err := readObjects(d.Manifest, route, d.Namespace)
	if err != nil {
		return err
	}
	if err := prepareObjects(objects, d.Namespace, dependencyManaged, owner, route, d.Name); err != nil {
		return err
	}
	for _, object := range objects {
		kind, name := objectIdentity(object)
		if err := c.ensureManaged(ctx, kind, name, dependencyManaged, owner, route); err != nil {
			return err
		}
	}
	payload, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "List", "items": objects})
	if _, err := c.RunKubectl(ctx, []string{"apply", "-f", "-"}, bytes.NewReader(payload)); err != nil {
		return err
	}
	for _, ready := range d.Readiness {
		timeout := ready.Timeout
		if timeout == "" {
			timeout = "2m"
		}
		args := []string{"wait", ready.Resource + "/" + strings.ReplaceAll(ready.Name, "${DEV_ROUTE}", route), "--for=condition=" + ready.Condition, "--timeout=" + timeout}
		if _, err := c.RunKubectl(ctx, args, nil); err != nil {
			return fmt.Errorf("dependency %s readiness: %w", d.Name, err)
		}
	}
	return nil
}

func (c Client) RemoveDependency(ctx context.Context, d config.Dependency, owner, route string) error {
	if d.Retention == "retain" {
		return nil
	}
	c = c.In(d.Namespace)
	objects, err := readObjects(d.Manifest, route, d.Namespace)
	if err != nil {
		return err
	}
	if err := prepareObjects(objects, d.Namespace, dependencyManaged, owner, route, d.Name); err != nil {
		return err
	}
	for _, object := range objects {
		kind, name := objectIdentity(object)
		if err := c.ensureManaged(ctx, kind, name, dependencyManaged, owner, route); err != nil {
			return err
		}
		if _, err := c.RunKubectl(ctx, []string{"delete", kind, name, "--ignore-not-found=true", "--wait=true", "--timeout=30s"}, nil); err != nil {
			return err
		}
	}
	return nil
}

type lease struct {
	Metadata struct {
		Name, ResourceVersion string
		Labels                map[string]string
	} `json:"metadata"`
	Spec struct {
		Holder   string    `json:"holderIdentity"`
		Duration int       `json:"leaseDurationSeconds"`
		Renew    time.Time `json:"renewTime"`
	} `json:"spec"`
}

func lockName(target string) string {
	sum := sha256.Sum256([]byte(target))
	return "dev-task-" + hex.EncodeToString(sum[:])[:16]
}

func (c Client) acquireTaskLock(ctx context.Context, task config.Task, holder string, timeout time.Duration) (func(), error) {
	c = c.In(task.Namespace)
	name, now, duration := lockName(task.Target), time.Now().UTC(), int(timeout.Seconds())+60
	out, err := c.RunKubectl(ctx, []string{"get", "lease", name, "--ignore-not-found=true", "-o", "json"}, nil)
	if err != nil {
		return nil, err
	}
	var current lease
	if len(bytes.TrimSpace(out)) > 0 {
		if err := json.Unmarshal(out, &current); err != nil {
			return nil, err
		}
		if current.Metadata.Labels[ManagedLabel] != lockManaged {
			return nil, fmt.Errorf("refusing to adopt lease/%s", name)
		}
		if current.Spec.Holder != "" && now.Before(current.Spec.Renew.Add(time.Duration(current.Spec.Duration)*time.Second)) {
			return nil, fmt.Errorf("task target %q is locked by %s", task.Target, current.Spec.Holder)
		}
	}
	object := map[string]any{"apiVersion": "coordination.k8s.io/v1", "kind": "Lease", "metadata": map[string]any{"name": name, "namespace": task.Namespace, "labels": map[string]any{ManagedLabel: lockManaged}}, "spec": map[string]any{"holderIdentity": holder, "leaseDurationSeconds": duration, "renewTime": now.Format(time.RFC3339)}}
	verb := "create"
	if current.Metadata.ResourceVersion != "" {
		object["metadata"].(map[string]any)["resourceVersion"] = current.Metadata.ResourceVersion
		verb = "replace"
	}
	payload, _ := json.Marshal(object)
	if _, err := c.RunKubectl(ctx, []string{verb, "-f", "-"}, bytes.NewReader(payload)); err != nil {
		return nil, fmt.Errorf("acquire task target %q: %w", task.Target, err)
	}
	return func() {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		out, err := c.RunKubectl(releaseCtx, []string{"get", "lease", name, "-o", "json"}, nil)
		if err != nil {
			return
		}
		var latest lease
		if json.Unmarshal(out, &latest) != nil || latest.Spec.Holder != holder {
			return
		}
		object["metadata"].(map[string]any)["resourceVersion"] = latest.Metadata.ResourceVersion
		object["spec"].(map[string]any)["holderIdentity"] = ""
		object["spec"].(map[string]any)["renewTime"] = time.Now().UTC().Format(time.RFC3339)
		payload, _ := json.Marshal(object)
		_, _ = c.RunKubectl(releaseCtx, []string{"replace", "-f", "-"}, bytes.NewReader(payload))
	}, nil
}

func (c Client) RunTask(ctx context.Context, task config.Task, image, revision, owner, route string) error {
	timeout, _ := time.ParseDuration(task.Timeout)
	holder := route + "/" + task.Name
	release, err := c.acquireTaskLock(ctx, task, holder, timeout)
	if err != nil {
		return err
	}
	defer release()
	c = c.In(task.Namespace)
	objects, err := readObjects(task.Manifest, route, task.Namespace)
	if err != nil {
		return err
	}
	if len(objects) != 1 || objects[0]["kind"] != "Job" {
		return fmt.Errorf("task %s manifest must contain exactly one batch/v1 Job", task.Name)
	}
	metadata, _ := objects[0]["metadata"].(map[string]any)
	if metadata == nil {
		metadata = map[string]any{}
		objects[0]["metadata"] = metadata
	}
	name := naming.Resource(task.Name, route)
	metadata["name"] = name
	annotations, _ := metadata["annotations"].(map[string]any)
	if annotations == nil {
		annotations = map[string]any{}
	}
	annotations["dev-cli.io/source-revision"] = revision
	metadata["annotations"] = annotations
	if image != "" {
		spec, _ := objects[0]["spec"].(map[string]any)
		template, _ := spec["template"].(map[string]any)
		podSpec, _ := template["spec"].(map[string]any)
		containers, _ := podSpec["containers"].([]any)
		found := false
		for _, value := range containers {
			container, _ := value.(map[string]any)
			if container["name"] == task.Container {
				container["image"] = image
				found = true
			}
		}
		if !found {
			return fmt.Errorf("task %s has no image container %q", task.Name, task.Container)
		}
	}
	if err := prepareObjects(objects, task.Namespace, taskManaged, owner, route, task.Name); err != nil {
		return err
	}
	if err := c.ensureManaged(ctx, "job", name, taskManaged, owner, route); err != nil {
		return err
	}
	if _, err := c.RunKubectl(ctx, []string{"delete", "job", name, "--ignore-not-found=true", "--wait=true", "--timeout=30s"}, nil); err != nil {
		return err
	}
	payload, _ := json.Marshal(objects[0])
	if _, err := c.RunKubectl(ctx, []string{"create", "-f", "-"}, bytes.NewReader(payload)); err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := c.RunKubectl(ctx, []string{"get", "job", name, "-o", "json"}, nil)
		if err != nil {
			return err
		}
		var status struct {
			Status struct{ Succeeded, Failed, Active int } `json:"status"`
		}
		if err := json.Unmarshal(out, &status); err != nil {
			return err
		}
		if status.Status.Succeeded > 0 {
			return nil
		}
		if status.Status.Failed > 0 {
			return fmt.Errorf("task %s failed; inspect with kubectl --context %s -n %s logs job/%s", task.Name, c.Context, task.Namespace, name)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("task %s did not complete within %s", task.Name, task.Timeout)
}
