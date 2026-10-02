package kube

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/antonve/dev-cli/internal/config"
	"github.com/antonve/dev-cli/internal/document"
	"github.com/antonve/dev-cli/internal/naming"
)

const (
	dependencyManaged = "dev-cli-dependency"
	taskManaged       = "dev-cli-task"
	lockManaged       = "dev-cli-task-lock"
	microTimeFormat   = "2006-01-02T15:04:05.000000Z07:00"
)

func readObjects(path, route, namespace string) ([]map[string]any, error) {
	b, err := readRepoFile(path)
	if err != nil {
		return nil, fmt.Errorf("read manifest %s: %w", path, err)
	}
	b = []byte(strings.NewReplacer("${DEV_ROUTE}", route, "${DEV_NAMESPACE}", namespace).Replace(string(b)))
	var raw map[string]any
	if err := document.Unmarshal(b, &raw); err != nil {
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
		switch kind {
		case "Namespace", "Node", "PersistentVolume", "ClusterRole", "ClusterRoleBinding", "CustomResourceDefinition", "StorageClass", "GatewayClass":
			return fmt.Errorf("cluster-scoped kind %s is not accepted", kind)
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
		metadata := object["metadata"].(map[string]any)
		annotations, _ := metadata["annotations"].(map[string]any)
		if annotations == nil {
			annotations = map[string]any{}
		}
		annotations["dev-cli.io/retention"] = d.Retention
		metadata["annotations"] = annotations
		digest, _ := json.Marshal(object)
		sum := sha256.Sum256(digest)
		annotations["dev-cli.io/manifest-sha256"] = hex.EncodeToString(sum[:])
	}
	for _, object := range objects {
		kind, name := objectIdentity(object)
		payload, _ := json.Marshal(object)
		existing, err := c.RunKubectl(ctx, []string{"get", kind, name, "--ignore-not-found=true", "-o", "json"}, nil)
		if err != nil {
			return err
		}
		if len(bytes.TrimSpace(existing)) > 0 {
			if err := c.ensureManagedJSON(existing, kind, name, dependencyManaged, owner, route); err != nil {
				return err
			}
			var current struct {
				Metadata struct {
					Annotations map[string]string `json:"annotations"`
					Namespace   string            `json:"namespace"`
				} `json:"metadata"`
			}
			if json.Unmarshal(existing, &current) != nil || current.Metadata.Namespace != d.Namespace || current.Metadata.Annotations["dev-cli.io/manifest-sha256"] != object["metadata"].(map[string]any)["annotations"].(map[string]any)["dev-cli.io/manifest-sha256"] {
				return fmt.Errorf("dependency %s object %s/%s differs from its immutable provisioned manifest", d.Name, kind, name)
			}
			continue
		}
		dry, err := c.RunKubectl(ctx, []string{"create", "--dry-run=server", "-f", "-", "-o", "json"}, bytes.NewReader(payload))
		if err != nil {
			return fmt.Errorf("validate dependency %s object %s/%s: %w", d.Name, kind, name, err)
		}
		var validated struct {
			Metadata struct {
				Namespace string `json:"namespace"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal(dry, &validated); err != nil || validated.Metadata.Namespace != d.Namespace {
			return fmt.Errorf("dependency %s object %s/%s is not namespaced in %s", d.Name, kind, name, d.Namespace)
		}
		if _, err := c.RunKubectl(ctx, []string{"create", "-f", "-"}, bytes.NewReader(payload)); err != nil {
			return fmt.Errorf("create dependency %s object %s/%s: %w", d.Name, kind, name, err)
		}
	}
	for _, ready := range d.Readiness {
		timeout := ready.Timeout
		if timeout == "" {
			timeout = "2m"
		}
		waitFor := "condition=" + ready.Condition
		if ready.JSONPath != "" {
			waitFor = "jsonpath={" + ready.JSONPath + "}=" + ready.Value
		}
		args := []string{"wait", ready.Resource + "/" + strings.ReplaceAll(ready.Name, "${DEV_ROUTE}", route), "--for=" + waitFor, "--timeout=" + timeout}
		if _, err := c.RunKubectl(ctx, args, nil); err != nil {
			return fmt.Errorf("dependency %s readiness: %w", d.Name, err)
		}
	}
	return nil
}

func (c Client) ensureManagedJSON(out []byte, kind, name, managed, owner, route string) error {
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
		existing, err := c.RunKubectl(ctx, []string{"get", kind, name, "--ignore-not-found=true", "-o", "json"}, nil)
		if err != nil {
			return err
		}
		if len(bytes.TrimSpace(existing)) == 0 {
			continue
		}
		if err := c.ensureManagedJSON(existing, kind, name, dependencyManaged, owner, route); err != nil {
			return err
		}
		var current struct {
			Metadata struct {
				Annotations map[string]string `json:"annotations"`
				UID         string            `json:"uid"`
			} `json:"metadata"`
		}
		if json.Unmarshal(existing, &current) != nil || current.Metadata.Annotations["dev-cli.io/retention"] != "down" || current.Metadata.UID == "" {
			return fmt.Errorf("refusing to delete retained dependency object %s/%s", kind, name)
		}
		path, err := c.namespacedResourcePath(ctx, object, name)
		if err != nil {
			return err
		}
		options, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "DeleteOptions", "propagationPolicy": "Foreground", "preconditions": map[string]any{"uid": current.Metadata.UID}})
		if _, err := c.RunKubectl(ctx, []string{"delete", "--raw", path, "-f", "-"}, bytes.NewReader(options)); err != nil {
			return err
		}
		deadline := time.Now().Add(30 * time.Second)
		deleted := false
		for time.Now().Before(deadline) {
			out, err := c.RunKubectl(ctx, []string{"get", kind, name, "--ignore-not-found=true", "-o", "json"}, nil)
			if err != nil {
				return err
			}
			if len(bytes.TrimSpace(out)) == 0 {
				deleted = true
				break
			}
			var live struct {
				Metadata struct {
					UID string `json:"uid"`
				} `json:"metadata"`
			}
			if json.Unmarshal(out, &live) == nil && live.Metadata.UID != current.Metadata.UID {
				deleted = true
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(250 * time.Millisecond):
			}
		}
		if !deleted {
			return fmt.Errorf("timed out waiting for dependency object %s/%s uid %s to be deleted", kind, name, current.Metadata.UID)
		}
	}
	return nil
}

func (c Client) namespacedResourcePath(ctx context.Context, object map[string]any, name string) (string, error) {
	apiVersion, _ := object["apiVersion"].(string)
	kind, _ := object["kind"].(string)
	group := ""
	if parts := strings.SplitN(apiVersion, "/", 2); len(parts) == 2 {
		group = parts[0]
	}
	out, err := c.RunKubectl(ctx, []string{"api-resources", "--namespaced=true", "--api-group=" + group, "-o", "wide", "--no-headers"}, nil)
	if err != nil {
		return "", err
	}
	resource := ""
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		for i := 1; i+2 < len(fields); i++ {
			if fields[i] == apiVersion && fields[i+1] == "true" && strings.EqualFold(fields[i+2], kind) {
				resource = fields[0]
				break
			}
		}
		if resource != "" {
			break
		}
	}
	if resource == "" {
		return "", fmt.Errorf("cannot resolve namespaced API resource for %s %s", apiVersion, kind)
	}
	prefix := "/api/" + url.PathEscape(apiVersion)
	if group != "" {
		parts := strings.SplitN(apiVersion, "/", 2)
		prefix = "/apis/" + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1])
	}
	return prefix + "/namespaces/" + url.PathEscape(c.Namespace) + "/" + url.PathEscape(resource) + "/" + url.PathEscape(name), nil
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

func (c Client) acquireTaskLock(ctx context.Context, task config.Task, lockNamespace, holder string, timeout time.Duration) (func(), error) {
	c = c.In(lockNamespace)
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
		if current.Spec.Holder != "" {
			exists, terminal, err := c.holderJobState(ctx, current.Spec.Holder)
			if err != nil {
				return nil, err
			}
			// Absence is not proof that the prior invoker stopped: it may have
			// paused after taking the Lease and before creating its Job. Only an
			// exact terminal Job permits automatic takeover.
			if !exists || !terminal {
				return nil, fmt.Errorf("task target %q is locked by %s in lease/%s", task.Target, current.Spec.Holder, name)
			}
		}
	}
	object := map[string]any{"apiVersion": "coordination.k8s.io/v1", "kind": "Lease", "metadata": map[string]any{"name": name, "namespace": lockNamespace, "labels": map[string]any{ManagedLabel: lockManaged}, "annotations": map[string]any{"dev-cli.io/task-target": task.Target}}, "spec": map[string]any{"holderIdentity": holder, "leaseDurationSeconds": duration, "renewTime": now.Format(microTimeFormat)}}
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
		object["spec"].(map[string]any)["renewTime"] = time.Now().UTC().Format(microTimeFormat)
		payload, _ := json.Marshal(object)
		_, _ = c.RunKubectl(releaseCtx, []string{"replace", "-f", "-"}, bytes.NewReader(payload))
	}, nil
}

type jobStatus struct {
	Status struct {
		Conditions []struct{ Type, Status string } `json:"conditions"`
	} `json:"status"`
}

func terminalJob(data []byte) (bool, error) {
	var job jobStatus
	if err := json.Unmarshal(data, &job); err != nil {
		return false, err
	}
	for _, condition := range job.Status.Conditions {
		if condition.Status == "True" && (condition.Type == "Complete" || condition.Type == "Failed") {
			return true, nil
		}
	}
	return false, nil
}

func (c Client) holderJobState(ctx context.Context, holder string) (bool, bool, error) {
	parts := strings.Split(holder, "/")
	if len(parts) != 2 {
		return false, false, fmt.Errorf("cannot verify task lock holder %q", holder)
	}
	out, err := c.In(parts[0]).RunKubectl(ctx, []string{"get", "job", parts[1], "--ignore-not-found=true", "-o", "json"}, nil)
	if err != nil {
		return false, false, err
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return false, false, nil
	}
	terminal, err := terminalJob(out)
	return true, terminal, err
}

func (c Client) RunTask(ctx context.Context, task config.Task, lockNamespace, image, revision, owner, route string) error {
	timeout, _ := time.ParseDuration(task.Timeout)
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
	name := naming.Resource(task.Name+"-"+strings.ToLower(rand.Text()[:8]), route)
	metadata["name"] = name
	annotations, _ := metadata["annotations"].(map[string]any)
	if annotations == nil {
		annotations = map[string]any{}
	}
	annotations["dev-cli.io/source-revision"] = revision
	metadata["annotations"] = annotations
	if image != "" {
		spec, _ := objects[0]["spec"].(map[string]any)
		if spec == nil {
			return fmt.Errorf("task %s Job has no spec", task.Name)
		}
		template, _ := spec["template"].(map[string]any)
		if template == nil {
			return fmt.Errorf("task %s Job has no pod template", task.Name)
		}
		podSpec, _ := template["spec"].(map[string]any)
		if podSpec == nil {
			return fmt.Errorf("task %s Job has no pod spec", task.Name)
		}
		found := false
		for _, field := range []string{"containers", "initContainers"} {
			containers, _ := podSpec[field].([]any)
			for _, value := range containers {
				container, _ := value.(map[string]any)
				if container["name"] == task.Container {
					container["image"] = image
					found = true
				}
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
	holder := task.Namespace + "/" + name
	release, err := c.acquireTaskLock(ctx, task, lockNamespace, holder, timeout)
	if err != nil {
		return err
	}
	started := false
	defer func() {
		if !started {
			release()
		}
	}()
	payload, _ := json.Marshal(objects[0])
	// The API may create the Job even when kubectl loses the response. From this
	// point onward only terminal Job proof may release the target lock.
	started = true
	if _, err := c.RunKubectl(ctx, []string{"create", "-f", "-"}, bytes.NewReader(payload)); err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := c.RunKubectl(ctx, []string{"get", "job", name, "-o", "json"}, nil)
		if err != nil {
			return err
		}
		var status jobStatus
		if err := json.Unmarshal(out, &status); err != nil {
			return err
		}
		for _, condition := range status.Status.Conditions {
			if condition.Status != "True" {
				continue
			}
			if condition.Type == "Complete" {
				release()
				return nil
			}
			if condition.Type == "Failed" {
				release()
				return fmt.Errorf("task %s failed; inspect with kubectl --context %s -n %s logs job/%s", task.Name, c.Context, task.Namespace, name)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("task %s did not complete within %s", task.Name, task.Timeout)
}
