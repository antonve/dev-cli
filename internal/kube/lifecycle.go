package kube

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/antonve/dev-cli/internal/config"
	"github.com/antonve/dev-cli/internal/naming"
)

const LifecycleLabel = "dev-cli.io/lifecycle-marker"

type Lifecycle struct {
	Owner                                   string       `json:"-"`
	Hooks                                   config.Hooks `json:"hooks"`
	Deployables                             []string     `json:"deployables,omitempty"`
	OverlaysStopped                         bool         `json:"overlaysStopped,omitempty"`
	Branch, Revision, BaseRef, BaseRevision string       `json:"-"`
	Expiry                                  time.Time    `json:"-"`
}

func (c Client) ReadLifecycle(ctx context.Context, route string) (*Lifecycle, error) {
	name := naming.Resource("lifecycle", route)
	out, err := c.RunKubectl(ctx, []string{"get", "configmap", name, "--ignore-not-found=true", "-o", "json"}, nil)
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return nil, nil
	}
	var object struct {
		Metadata struct{ Labels, Annotations map[string]string }
		Data     map[string]string
	}
	if err := json.Unmarshal(out, &object); err != nil {
		return nil, err
	}
	owner := object.Metadata.Annotations["dev-cli.io/owner-original"]
	if owner == "" || object.Metadata.Labels[LifecycleLabel] != "true" {
		return nil, fmt.Errorf("invalid lifecycle marker for route %s", route)
	}
	if err := c.ensureOwnedJSON(out, "configmap", name, owner, route); err != nil {
		return nil, err
	}
	var m Lifecycle
	if err := json.Unmarshal([]byte(object.Data["hooks.json"]), &m); err != nil {
		return nil, fmt.Errorf("invalid lifecycle hooks for route %s: %w", route, err)
	}
	m.Owner = owner
	a := object.Metadata.Annotations
	m.Branch, m.Revision, m.BaseRef, m.BaseRevision = a["dev-cli.io/branch-original"], a["dev-cli.io/source-revision"], a["dev-cli.io/base-ref"], a["dev-cli.io/base-revision"]
	m.Expiry, err = time.Parse(time.RFC3339, a["dev-cli.io/expires-at"])
	if err != nil {
		return nil, fmt.Errorf("invalid lifecycle expiry for route %s: %w", route, err)
	}
	return &m, nil
}

func (c Client) WriteLifecycle(ctx context.Context, m Lifecycle, branch, revision, baseRef, baseRevision, route string, expiry time.Time) error {
	recorded := config.Hooks{AfterDown: append([]string{}, m.Hooks.AfterDown...), Deployables: map[string]config.DeployableHooks{}}
	for name, hooks := range m.Hooks.Deployables {
		recorded.Deployables[name] = config.DeployableHooks{AfterStop: append([]string{}, hooks.AfterStop...)}
	}
	m.Hooks = recorded
	m.Branch, m.Revision, m.BaseRef, m.BaseRevision, m.Expiry = "", "", "", "", time.Time{}
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	ls := labels(m.Owner, route, "lifecycle")
	ls[LifecycleLabel] = "true"
	marker := map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": naming.Resource("lifecycle", route), "namespace": c.Namespace, "labels": ls, "annotations": annotations(m.Owner, branch, revision, baseRef, baseRevision, "", expiry, time.Now().UTC().Format(time.RFC3339))}, "data": map[string]any{"hooks.json": string(data)}}
	return c.writeOwnedObjects(ctx, []any{marker}, m.Owner, route)
}

func (c Client) SaveLifecycle(ctx context.Context, route string, m Lifecycle) error {
	return c.WriteLifecycle(ctx, m, m.Branch, m.Revision, m.BaseRef, m.BaseRevision, route, m.Expiry)
}

func (c Client) DeleteLifecycle(ctx context.Context, owner, route string) error {
	_, err := c.RunKubectl(ctx, []string{"delete", "configmap", "-l", ManagedLabel + "=dev-cli," + OwnerLabel + "=" + naming.Slug(owner, 40) + "," + RouteLabel + "=" + route + "," + LifecycleLabel + "=true", "--ignore-not-found=true", "--wait=true"}, nil)
	return err
}
