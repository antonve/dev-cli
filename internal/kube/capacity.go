package kube

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/antonve/dev-cli/internal/config"
)

// usage holds CPU and memory requests in milli-units: millicores and
// thousandths of a byte.
type usage struct{ cpu, memory int64 }

func (u usage) add(v usage) usage  { return usage{u.cpu + v.cpu, u.memory + v.memory} }
func (u usage) sub(v usage) usage  { return usage{u.cpu - v.cpu, u.memory - v.memory} }
func (u usage) holds(v usage) bool { return v.cpu <= u.cpu && v.memory <= u.memory }
func (u usage) need() string       { return u.format(1<<20*1000 - 1) }
func (u usage) free() string       { return u.format(0) }
func (u usage) format(up int64) string {
	return fmt.Sprintf("cpu=%dm memory=%dMi", u.cpu, (u.memory+up)/(1<<20*1000))
}

type quantity int64

func (q *quantity) UnmarshalJSON(b []byte) error {
	v, err := parseQuantity(strings.Trim(string(b), `"`))
	*q = quantity(v)
	return err
}

var quantityPattern = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)(?:[eE]([0-9]+)|(m|k|M|G|T|P|E|Ki|Mi|Gi|Ti|Pi|Ei))?$`)

// parseQuantity converts a Kubernetes resource quantity to milli-units,
// rounding up.
func parseQuantity(s string) (int64, error) {
	match := quantityPattern.FindStringSubmatch(s)
	if match == nil {
		return 0, fmt.Errorf("invalid resource quantity %q", s)
	}
	value, _ := new(big.Rat).SetString(match[1])
	value.Mul(value, big.NewRat(1000, 1))
	multiplier := new(big.Int)
	switch suffix := match[3]; {
	case match[2] != "":
		exponent, _ := new(big.Int).SetString(match[2], 10)
		multiplier.Exp(big.NewInt(10), exponent, nil)
	case suffix == "m":
		value.Quo(value, big.NewRat(1000, 1))
		multiplier.SetInt64(1)
	case strings.HasSuffix(suffix, "i"):
		multiplier.Lsh(big.NewInt(1), uint(10*(strings.Index("KMGTPE", suffix[:1])+1)))
	case suffix != "":
		multiplier.Exp(big.NewInt(10), big.NewInt(int64(3*(strings.Index("kMGTPE", suffix)+1))), nil)
	default:
		multiplier.SetInt64(1)
	}
	value.Mul(value, new(big.Rat).SetInt(multiplier))
	result, remainder := new(big.Int).QuoRem(value.Num(), value.Denom(), new(big.Int))
	if remainder.Sign() > 0 {
		result.Add(result, big.NewInt(1))
	}
	if !result.IsInt64() {
		return 0, fmt.Errorf("resource quantity %q is too large", s)
	}
	return result.Int64(), nil
}

type requests struct {
	Requests struct{ CPU, Memory quantity }
}

type podSpec struct {
	NodeName                   string
	NodeSelector               map[string]string
	Tolerations                []struct{ Key, Operator, Value, Effect string }
	Containers, InitContainers []struct{ Resources requests }
}

// requests is what the scheduler reserves: the larger of the regular
// containers' sum and the largest init container, per resource.
func (s podSpec) requests() usage {
	var regular, init usage
	for _, c := range s.Containers {
		regular = regular.add(usage{int64(c.Resources.Requests.CPU), int64(c.Resources.Requests.Memory)})
	}
	for _, c := range s.InitContainers {
		init.cpu = max(init.cpu, int64(c.Resources.Requests.CPU))
		init.memory = max(init.memory, int64(c.Resources.Requests.Memory))
	}
	return usage{max(regular.cpu, init.cpu), max(regular.memory, init.memory)}
}

type node struct {
	Metadata struct {
		Name   string
		Labels map[string]string
	}
	Spec struct {
		Unschedulable bool
		Taints        []struct{ Key, Value, Effect string }
	}
	Status struct {
		Allocatable struct{ CPU, Memory quantity }
	}
}

// admits applies the scheduler's hard node filters that a pod spec can
// declare. ponytail: node and pod affinity and topology spread are ignored;
// add them when a workload template uses them to restrict placement.
func (n node) admits(s podSpec) bool {
	if n.Spec.Unschedulable {
		return false
	}
	for key, value := range s.NodeSelector {
		if n.Metadata.Labels[key] != value {
			return false
		}
	}
	for _, taint := range n.Spec.Taints {
		if taint.Effect != "NoSchedule" && taint.Effect != "NoExecute" {
			continue
		}
		tolerated := false
		for _, t := range s.Tolerations {
			key := t.Key == taint.Key || t.Key == "" && t.Operator == "Exists"
			value := t.Operator == "Exists" || t.Value == taint.Value
			tolerated = tolerated || key && value && (t.Effect == "" || t.Effect == taint.Effect)
		}
		if !tolerated {
			return false
		}
	}
	return true
}

type pod struct {
	Metadata struct {
		Namespace           string
		Labels, Annotations map[string]string
	}
	Spec   podSpec
	Status struct{ Phase string }
}

func (p pod) live() bool { return p.Status.Phase != "Succeeded" && p.Status.Phase != "Failed" }

// CheckCapacity refuses a deploy whose overlay pods cannot all be placed on
// schedulable nodes beside the requests of every live pod. Each pod must fit
// on one node; placement is first-fit, largest first. An overlay already
// running revision is not rolled and needs no room. Any other existing
// overlay rolls with a surge pod, so its old pod is released only after the
// replacement is placed. It reads nodes and pods and changes nothing.
// ponytail: task Job pods are not counted and nothing is reserved, so
// concurrent deploys can race; reserve capacity if that happens in practice.
func (c Client) CheckCapacity(ctx context.Context, cfg config.Config, ds []config.Deployable, revision, route string, w io.Writer) error {
	var nodes struct{ Items []node }
	if err := c.getJSON(ctx, &nodes, "nodes"); err != nil {
		return err
	}
	var pods struct{ Items []pod }
	if err := c.getJSON(ctx, &pods, "pods", "--all-namespaces"); err != nil {
		return err
	}

	free := map[string]usage{}
	for _, n := range nodes.Items {
		free[n.Metadata.Name] = usage{int64(n.Status.Allocatable.CPU), int64(n.Status.Allocatable.Memory)}
	}
	for _, p := range pods.Items {
		if p.live() && p.Spec.NodeName != "" {
			free[p.Spec.NodeName] = free[p.Spec.NodeName].sub(p.Spec.requests())
		}
	}

	type need struct {
		name     string
		spec     podSpec
		replaces []pod
	}
	var needs []need
	for _, d := range ds {
		k := c.In(d.WorkloadNamespace(cfg))
		items, err := k.overlayObjects(cfg, d, "capacity-check", "", "", revision, "", "", route, time.Time{}, "")
		if err != nil {
			return err
		}
		var deployment struct {
			Spec struct {
				Replicas int
				Template struct{ Spec podSpec }
			}
		}
		for _, item := range items {
			if object := item.(map[string]any); object["kind"] == "Deployment" {
				b, _ := json.Marshal(object)
				if err := json.Unmarshal(b, &deployment); err != nil {
					return fmt.Errorf("overlay %s: %w", d.Name, err)
				}
			}
		}

		var running []pod
		unchanged := true
		for _, p := range pods.Items {
			l, a := p.Metadata.Labels, p.Metadata.Annotations
			if !p.live() || p.Metadata.Namespace != k.Namespace || l[ManagedLabel] != "dev-cli" || l[RouteLabel] != route || l[ServiceLabel] != d.Name {
				continue
			}
			running = append(running, p)
			unchanged = unchanged && a["dev-cli.io/source-revision"] == revision && a["dev-cli.io/cli-version"] == CLIVersion
		}
		if len(running) > 0 && unchanged {
			fmt.Fprintf(w, "capacity: %s already runs this commit and needs no room\n", d.Name)
			continue
		}

		fmt.Fprintf(w, "capacity: %s needs %s x%d\n", d.Name, deployment.Spec.Template.Spec.requests().need(), deployment.Spec.Replicas)
		for i := range deployment.Spec.Replicas {
			n := need{name: d.Name, spec: deployment.Spec.Template.Spec}
			if i == 0 {
				n.replaces = running
			}
			needs = append(needs, n)
		}
	}

	slices.SortFunc(nodes.Items, func(a, b node) int { return cmp.Compare(a.Metadata.Name, b.Metadata.Name) })
	for _, n := range nodes.Items {
		if n.Spec.Unschedulable {
			fmt.Fprintf(w, "capacity: node %s is cordoned\n", n.Metadata.Name)
			continue
		}
		fmt.Fprintf(w, "capacity: node %s free %s\n", n.Metadata.Name, free[n.Metadata.Name].free())
	}

	// Replacements go first because placing them releases their old pods.
	slices.SortStableFunc(needs, func(a, b need) int {
		ra, rb := a.spec.requests(), b.spec.requests()
		return cmp.Or(
			cmp.Compare(min(len(b.replaces), 1), min(len(a.replaces), 1)),
			cmp.Compare(rb.memory, ra.memory),
			cmp.Compare(rb.cpu, ra.cpu),
		)
	})
	var placed, refused []string
	for _, n := range needs {
		requested := n.spec.requests()
		target := ""
		for _, candidate := range nodes.Items {
			if candidate.admits(n.spec) && free[candidate.Metadata.Name].holds(requested) {
				target = candidate.Metadata.Name
				break
			}
		}
		if target == "" {
			refused = append(refused, fmt.Sprintf("%s needs %s", n.name, requested.need()))
			continue
		}
		free[target] = free[target].sub(requested)
		for _, old := range n.replaces {
			free[old.Spec.NodeName] = free[old.Spec.NodeName].add(old.Spec.requests())
		}
		placed = append(placed, n.name+" on "+target)
	}
	if len(refused) == 0 {
		fmt.Fprintf(w, "capacity: fits: %s\n", strings.Join(placed, ", "))
		return nil
	}
	var room []string
	for _, n := range nodes.Items {
		if !n.Spec.Unschedulable {
			room = append(room, n.Metadata.Name+" "+free[n.Metadata.Name].free())
		}
	}
	return fmt.Errorf(
		"insufficient cluster capacity: %s fits on no schedulable node; free after placing the other pods: %s",
		strings.Join(refused, ", "),
		strings.Join(room, ", "),
	)
}

func (c Client) getJSON(ctx context.Context, v any, args ...string) error {
	out, err := c.RunKubectl(ctx, append(append([]string{"get"}, args...), "-o", "json"), nil)
	if err != nil {
		return err
	}
	return json.Unmarshal(out, v)
}
