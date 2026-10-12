package kube

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/antonve/dev-cli/internal/config"
	"github.com/antonve/dev-cli/internal/naming"
)

const (
	capacityRoute    = "alice-feature-12345678"
	capacityRevision = "0123456789abcdef0123456789abcdef01234567"
)

type capacityRunner struct{ nodes, pods []map[string]any }

func (r capacityRunner) Run(_ context.Context, _ string, args []string, _ io.Reader) ([]byte, error) {
	if len(args) > 5 && args[4] == "get" {
		switch args[5] {
		case "nodes":
			return json.Marshal(map[string]any{"items": r.nodes})
		case "pods":
			if args[6] != "--all-namespaces" {
				break
			}
			return json.Marshal(map[string]any{"items": r.pods})
		}
	}
	return nil, fmt.Errorf("capacity check ran an unexpected command: kubectl %s", strings.Join(args, " "))
}

func (capacityRunner) Stream(context.Context, string, []string, io.Reader, io.Writer, io.Writer) error {
	return nil
}

func capacityNode(name, cpu, memory string, edit ...func(spec, metadata map[string]any)) map[string]any {
	spec, metadata := map[string]any{}, map[string]any{"name": name, "labels": map[string]any{}}
	for _, e := range edit {
		e(spec, metadata)
	}
	return map[string]any{
		"metadata": metadata,
		"spec":     spec,
		"status":   map[string]any{"allocatable": map[string]any{"cpu": cpu, "memory": memory, "pods": "110"}},
	}
}

func cordoned(spec, _ map[string]any) { spec["unschedulable"] = true }

func tainted(effect string) func(spec, metadata map[string]any) {
	return func(spec, _ map[string]any) {
		spec["taints"] = []any{map[string]any{"key": "dedicated", "value": "db", "effect": effect}}
	}
}

func labelled(key, value string) func(spec, metadata map[string]any) {
	return func(_, metadata map[string]any) { metadata["labels"].(map[string]any)[key] = value }
}

func container(cpu, memory string) map[string]any {
	return map[string]any{"name": "c", "resources": map[string]any{"requests": map[string]any{"cpu": cpu, "memory": memory}}}
}

// capacityPod is a pod already assigned to a node.
func capacityPod(node, phase string, containers []any, edit ...func(pod map[string]any)) map[string]any {
	pod := map[string]any{
		"metadata": map[string]any{"name": "p", "namespace": "other"},
		"spec":     map[string]any{"nodeName": node, "containers": containers},
		"status":   map[string]any{"phase": phase},
	}
	for _, e := range edit {
		e(pod)
	}
	return pod
}

// overlayPod is a running pod of this route's overlay for service.
func overlayPod(node, service, revision, cpu, memory string) map[string]any {
	return capacityPod(node, "Running", []any{container(cpu, memory)}, func(pod map[string]any) {
		metadata := pod["metadata"].(map[string]any)
		metadata["namespace"] = "ns"
		metadata["labels"] = map[string]any{ManagedLabel: "dev-cli", RouteLabel: capacityRoute, ServiceLabel: service}
		metadata["annotations"] = map[string]any{"dev-cli.io/source-revision": revision, "dev-cli.io/cli-version": CLIVersion}
	})
}

// capacityDeployables writes one production workload template per entry
// ("name": "cpu/memory") and returns the deployables in the given order.
func capacityDeployables(t *testing.T, root string, requests ...string) []config.Deployable {
	t.Helper()
	var ds []config.Deployable
	for _, request := range requests {
		name, quantities, _ := strings.Cut(request, "=")
		cpu, memory, _ := strings.Cut(quantities, "/")
		template := fmt.Sprintf(
			"spec:\n  containers:\n    - name: app\n      resources: {requests: {cpu: %s, memory: %s}}\n",
			cpu,
			memory,
		)
		if err := os.WriteFile(filepath.Join(root, name+".yaml"), []byte(template), 0600); err != nil {
			t.Fatal(err)
		}
		ds = append(ds, config.Deployable{Name: name, Kind: "worker", ContainerPath: "/app", WorkloadTemplate: name + ".yaml"})
	}
	return ds
}

func checkCapacity(t *testing.T, r capacityRunner, requests ...string) (string, error) {
	t.Helper()
	root := t.TempDir()
	ds := capacityDeployables(t, root, requests...)
	cfg := config.Config{Namespace: "ns", Mode: config.ModeProduction}
	c := Client{Run: r, Context: "prod", Namespace: "ns", ManifestRoot: root}
	var out bytes.Buffer
	err := c.CheckCapacity(context.Background(), cfg, ds, capacityRevision, capacityRoute, &out)
	return out.String(), err
}

func TestCapacityRefusesWhenOnlyTheClusterSumFits(t *testing.T) {
	r := capacityRunner{nodes: []map[string]any{
		capacityNode("node-a", "1", "300Mi"),
		capacityNode("node-b", "1", "300Mi"),
	}}
	out, err := checkCapacity(t, r, "api=10m/400Mi")
	if err == nil {
		t.Fatalf("400Mi fitted on two nodes with 300Mi free each:\n%s", out)
	}
	for _, want := range []string{"api", "memory=400Mi", "node-a", "node-b", "memory=300Mi"} {
		if !strings.Contains(err.Error()+out, want) {
			t.Fatalf("refusal does not show %q:\n%v\n%s", want, err, out)
		}
	}
}

func TestCapacityPlacesLargestPodsFirst(t *testing.T) {
	r := capacityRunner{nodes: []map[string]any{
		capacityNode("node-a", "1", "300Mi"),
		capacityNode("node-b", "1", "200Mi"),
	}}
	// In request order, small pods would take node-a and strand the 200Mi pod.
	if out, err := checkCapacity(t, r, "small=10m/100Mi", "medium=10m/150Mi", "large=10m/200Mi"); err != nil {
		t.Fatalf("pods that pack as 200+100 and 150 were refused: %v\n%s", err, out)
	}
	if out, err := checkCapacity(t, r, "a=10m/200Mi", "b=10m/150Mi", "c=10m/140Mi"); err == nil {
		t.Fatalf("490Mi of pods fitted 500Mi split as 300Mi and 200Mi:\n%s", out)
	}
}

func TestCapacitySubtractsRequestsOfLivePodsOnly(t *testing.T) {
	r := capacityRunner{
		nodes: []map[string]any{capacityNode("node-a", "1", "1Gi")},
		pods: []map[string]any{
			capacityPod("node-a", "Running", []any{container("100m", "512Mi"), container("100m", "256Mi")}),
			capacityPod("node-a", "Pending", []any{container("100m", "128Mi")}),
			capacityPod("node-a", "Succeeded", []any{container("100m", "512Mi")}),
			capacityPod("node-a", "Failed", []any{container("100m", "512Mi")}),
			capacityPod("", "Pending", []any{container("100m", "512Mi")}),
		},
	}
	// 1024Mi - 512Mi - 256Mi - 128Mi leaves 128Mi.
	if out, err := checkCapacity(t, r, "api=10m/128Mi"); err != nil {
		t.Fatalf("terminated or unassigned pods consumed room: %v\n%s", err, out)
	}
	if out, err := checkCapacity(t, r, "api=10m/129Mi"); err == nil {
		t.Fatalf("running and assigned pending pods did not consume room:\n%s", out)
	}
}

func TestCapacityCountsTheLargerOfInitAndRegularContainers(t *testing.T) {
	withInit := func(memory string) map[string]any {
		return capacityPod("node-a", "Running", []any{container("10m", "100Mi")}, func(pod map[string]any) {
			pod["spec"].(map[string]any)["initContainers"] = []any{container("10m", memory)}
		})
	}
	nodes := []map[string]any{capacityNode("node-a", "1", "1Gi")}
	if out, err := checkCapacity(t, capacityRunner{nodes: nodes, pods: []map[string]any{withInit("50Mi")}}, "api=10m/924Mi"); err != nil {
		t.Fatalf("a smaller init container was added to the pod's requests: %v\n%s", err, out)
	}
	if out, err := checkCapacity(t, capacityRunner{nodes: nodes, pods: []map[string]any{withInit("512Mi")}}, "api=10m/513Mi"); err == nil {
		t.Fatalf("a larger init container did not set the pod's requests:\n%s", out)
	}
}

func TestCapacityRefusesCPUShortage(t *testing.T) {
	r := capacityRunner{
		nodes: []map[string]any{capacityNode("node-a", "1", "4Gi")},
		pods:  []map[string]any{capacityPod("node-a", "Running", []any{container("950m", "64Mi")})},
	}
	out, err := checkCapacity(t, r, "api=100m/32Mi")
	if err == nil || !strings.Contains(err.Error()+out, "cpu=50m") {
		t.Fatalf("100m fitted beside 950m of 1 CPU: %v\n%s", err, out)
	}
}

func TestCapacitySkipsUnschedulableNodes(t *testing.T) {
	for name, node := range map[string]map[string]any{
		"cordoned":   capacityNode("node-b", "1", "4Gi", cordoned),
		"NoSchedule": capacityNode("node-b", "1", "4Gi", tainted("NoSchedule")),
		"NoExecute":  capacityNode("node-b", "1", "4Gi", tainted("NoExecute")),
	} {
		t.Run(name, func(t *testing.T) {
			r := capacityRunner{nodes: []map[string]any{capacityNode("node-a", "1", "100Mi"), node}}
			if out, err := checkCapacity(t, r, "api=10m/200Mi"); err == nil {
				t.Fatalf("pod was placed on a %s node:\n%s", name, out)
			}
		})
	}
	r := capacityRunner{nodes: []map[string]any{capacityNode("node-b", "1", "4Gi", tainted("PreferNoSchedule"))}}
	if out, err := checkCapacity(t, r, "api=10m/200Mi"); err != nil {
		t.Fatalf("a PreferNoSchedule taint blocked placement: %v\n%s", err, out)
	}
}

func TestCapacityHonoursTolerationsAndNodeSelectors(t *testing.T) {
	root := t.TempDir()
	template := `spec:
  nodeSelector: {pool: apps}
  tolerations:
    - {key: dedicated, operator: Equal, value: db, effect: NoSchedule}
  containers:
    - name: app
      resources: {requests: {cpu: 10m, memory: 200Mi}}
`
	if err := os.WriteFile(filepath.Join(root, "api.yaml"), []byte(template), 0600); err != nil {
		t.Fatal(err)
	}
	ds := []config.Deployable{{Name: "api", Kind: "worker", ContainerPath: "/app", WorkloadTemplate: "api.yaml"}}
	cfg := config.Config{Namespace: "ns", Mode: config.ModeProduction}
	for name, tc := range map[string]struct {
		node map[string]any
		fits bool
	}{
		"tolerated taint and matching label": {capacityNode("n", "1", "1Gi", tainted("NoSchedule"), labelled("pool", "apps")), true},
		"untolerated taint":                  {capacityNode("n", "1", "1Gi", tainted("NoExecute"), labelled("pool", "apps")), false},
		"label mismatch":                     {capacityNode("n", "1", "1Gi", labelled("pool", "db")), false},
	} {
		t.Run(name, func(t *testing.T) {
			c := Client{Run: capacityRunner{nodes: []map[string]any{tc.node}}, Context: "prod", Namespace: "ns", ManifestRoot: root}
			var out bytes.Buffer
			err := c.CheckCapacity(context.Background(), cfg, ds, capacityRevision, capacityRoute, &out)
			if (err == nil) != tc.fits {
				t.Fatalf("fits=%t err=%v\n%s", tc.fits, err, out.String())
			}
		})
	}
}

func TestCapacityRedeploys(t *testing.T) {
	nodes := []map[string]any{capacityNode("node-a", "1", "300Mi")}
	// The overlay fills the node; a rerun at the same commit rolls nothing.
	same := capacityRunner{nodes: nodes, pods: []map[string]any{overlayPod("node-a", "api", capacityRevision, "10m", "300Mi")}}
	if out, err := checkCapacity(t, same, "api=10m/300Mi"); err != nil {
		t.Fatalf("an unchanged overlay needed room for itself: %v\n%s", err, out)
	}
	// A new commit surges a second pod before the old one stops.
	newer := capacityRunner{nodes: nodes, pods: []map[string]any{overlayPod("node-a", "api", "old", "10m", "200Mi")}}
	if out, err := checkCapacity(t, newer, "api=10m/200Mi"); err == nil {
		t.Fatalf("a rolling replacement was assumed to free its old pod first:\n%s", out)
	}
	// Once the replacement is placed, its old pod's room serves later pods.
	if out, err := checkCapacity(t, newer, "api=10m/100Mi", "web=10m/150Mi"); err != nil {
		t.Fatalf("the replaced pod's room was never released: %v\n%s", err, out)
	}
}

func TestCapacityReportsTheFit(t *testing.T) {
	r := capacityRunner{nodes: []map[string]any{capacityNode("node-a", "1", "1Gi")}}
	out, err := checkCapacity(t, r, "api=25m/32Mi")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"api", "cpu=25m", "memory=32Mi", "node-a", "memory=1024Mi"} {
		if !strings.Contains(out, want) {
			t.Fatalf("report does not show %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, naming.Resource("api", capacityRoute)) {
		t.Fatalf("report names Kubernetes resources instead of deployables:\n%s", out)
	}
}

func TestParseQuantity(t *testing.T) {
	for in, want := range map[string]int64{
		"1":         1000,
		"250m":      250,
		"0.5":       500,
		"1.5":       1500,
		"128Mi":     128 << 20 * 1000,
		"1919424Ki": 1919424 << 10 * 1000,
		"1.5Gi":     3 << 29 * 1000,
		"1G":        1e9 * 1000,
		"512M":      512e6 * 1000,
		"1e3":       1e3 * 1000,
		"100k":      100e3 * 1000,
	} {
		got, err := parseQuantity(in)
		if err != nil || got != want {
			t.Fatalf("parseQuantity(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "Mi", "1Xi", "-1", "1.2.3"} {
		if _, err := parseQuantity(in); err == nil {
			t.Fatalf("parseQuantity(%q) accepted an invalid quantity", in)
		}
	}
}
