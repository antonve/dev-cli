package config

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

type Config struct {
	KubeContext      string       `json:"kubeContext"`
	Namespace        string       `json:"namespace"`
	Registry         string       `json:"registry"`
	IngressHost      string       `json:"ingressHost"`
	IngressClass     string       `json:"ingressClass"`
	GatewayName      string       `json:"gatewayName"`
	GatewayNamespace string       `json:"gatewayNamespace"`
	CookieName       string       `json:"cookieName"`
	TTL              string       `json:"ttl"`
	MetadataQuery    string       `json:"metadataQuery"`
	Namespaces       []string     `json:"namespaces"`
	PublicHosts      []string     `json:"publicHosts"`
	BazelArgs        []string     `json:"bazelArgs"`
	Dependencies     []Dependency `json:"dependencies"`
	Tasks            []Task       `json:"tasks"`
}

type ObjectRef struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Port      int    `json:"port"`
}

type Dependency struct {
	Name      string      `json:"name"`
	Namespace string      `json:"namespace"`
	Manifest  string      `json:"manifest"`
	Readiness []Readiness `json:"readiness"`
	Retention string      `json:"retention"`
}

type Readiness struct {
	Resource  string `json:"resource"`
	Name      string `json:"name"`
	Condition string `json:"condition"`
	Timeout   string `json:"timeout"`
}

type Task struct {
	Name         string   `json:"name"`
	Namespace    string   `json:"namespace"`
	Manifest     string   `json:"manifest"`
	Target       string   `json:"target"`
	Dependencies []string `json:"dependencies"`
	Timeout      string   `json:"timeout"`
	ImageName    string   `json:"imageName"`
	PushTarget   string   `json:"pushTarget"`
	Container    string   `json:"container"`
}

type Deployable struct {
	Name              string    `json:"name"`
	Kind              string    `json:"kind"`
	BuildTarget       string    `json:"buildTarget"`
	ImageName         string    `json:"imageName"`
	ImageTarget       string    `json:"imageTarget"`
	PushTarget        string    `json:"pushTarget"`
	Port              int       `json:"port"`
	ReadinessPath     string    `json:"readinessPath"`
	SourceRoots       []string  `json:"sourceRoots"`
	SyncPaths         []string  `json:"syncPaths"`
	DependencyPaths   []string  `json:"dependencyPaths"`
	DependencyCommand []string  `json:"dependencyCommand"`
	BinaryPath        string    `json:"binaryPath"`
	ContainerPath     string    `json:"containerPath"`
	DevCommand        []string  `json:"devCommand"`
	PublicPath        string    `json:"publicPath"`
	InternalHost      string    `json:"internalHost"`
	MetadataTarget    string    `json:"metadataTarget,omitempty"`
	Namespace         string    `json:"namespace"`
	PublicHost        string    `json:"publicHost"`
	ServicePort       int       `json:"servicePort"`
	BaseService       ObjectRef `json:"baseService"`
	WorkloadTemplate  string    `json:"workloadTemplate"`
	DevContainer      string    `json:"devContainer"`
	SyncRoot          string    `json:"syncRoot"`
	SyncStripPrefix   string    `json:"syncStripPrefix"`
	SyncExcludes      []string  `json:"syncExcludes"`
	StartupTasks      []string  `json:"startupTasks"`
}

func Load(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config %s: %w", path, err)
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	if c.KubeContext == "" || c.Namespace == "" || c.Registry == "" {
		return Config{}, fmt.Errorf("config requires kubeContext, namespace, and registry")
	}
	if c.IngressClass == "" {
		c.IngressClass = "nginx"
	}
	if c.GatewayName == "" {
		c.GatewayName = "dev-cli-playground"
	}
	if c.GatewayNamespace == "" {
		c.GatewayNamespace = c.Namespace
	}
	if c.CookieName == "" {
		c.CookieName = "dev_branch"
	}
	if err := (&http.Cookie{Name: c.CookieName, Value: "route"}).Valid(); err != nil {
		return Config{}, fmt.Errorf("invalid cookieName: %w", err)
	}
	if c.TTL == "" {
		c.TTL = "8h"
	}
	if ttl, err := time.ParseDuration(c.TTL); err != nil || ttl < time.Second {
		return Config{}, fmt.Errorf("ttl must be a duration of at least 1s")
	}
	if c.MetadataQuery == "" {
		c.MetadataQuery = "kind(dev_deployable, //...)"
	}
	if len(c.Namespaces) == 0 {
		c.Namespaces = []string{c.Namespace}
	}
	if !contains(c.Namespaces, c.Namespace) {
		return Config{}, fmt.Errorf("namespaces must include routing namespace %q", c.Namespace)
	}
	if c.IngressHost != "" && len(c.PublicHosts) == 0 {
		c.PublicHosts = []string{c.IngressHost}
	}
	for i := range c.Dependencies {
		d := &c.Dependencies[i]
		if d.Name == "" || d.Manifest == "" || !filepath.IsLocal(d.Manifest) || d.Namespace == "" || !contains(c.Namespaces, d.Namespace) {
			return Config{}, fmt.Errorf("dependency %q requires name, manifest, and an allowed namespace", d.Name)
		}
		if d.Retention == "" {
			d.Retention = "retain"
		}
		if d.Retention != "retain" && d.Retention != "down" {
			return Config{}, fmt.Errorf("dependency %q retention must be retain or down", d.Name)
		}
	}
	for i := range c.Tasks {
		t := &c.Tasks[i]
		if t.Name == "" || t.Manifest == "" || !filepath.IsLocal(t.Manifest) || t.Namespace == "" || t.Target == "" || !contains(c.Namespaces, t.Namespace) {
			return Config{}, fmt.Errorf("task %q requires name, manifest, target, and an allowed namespace", t.Name)
		}
		if t.Timeout == "" {
			t.Timeout = "5m"
		}
		if _, err := time.ParseDuration(t.Timeout); err != nil {
			return Config{}, fmt.Errorf("task %q timeout: %w", t.Name, err)
		}
		if (t.ImageName == "") != (t.PushTarget == "") {
			return Config{}, fmt.Errorf("task %q imageName and pushTarget must be set together", t.Name)
		}
		if t.ImageName != "" && t.Container == "" {
			return Config{}, fmt.Errorf("task %q container is required for a published image", t.Name)
		}
	}
	return c, nil
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func (c Config) Dependency(name string) (Dependency, bool) {
	for _, d := range c.Dependencies {
		if d.Name == name {
			return d, true
		}
	}
	return Dependency{}, false
}

func (c Config) Task(name string) (Task, bool) {
	for _, t := range c.Tasks {
		if t.Name == name {
			return t, true
		}
	}
	return Task{}, false
}

func (c Config) AllowsNamespace(namespace string) bool { return contains(c.Namespaces, namespace) }
func (c Config) AllowsHost(host string) bool           { return contains(c.PublicHosts, host) }

func (d Deployable) WorkloadNamespace(c Config) string {
	if d.Namespace != "" {
		return d.Namespace
	}
	return c.Namespace
}
func (d Deployable) Host(c Config) string {
	if d.PublicHost != "" {
		return d.PublicHost
	}
	return c.IngressHost
}
func (d Deployable) OverlayPort() int {
	if d.ServicePort != 0 {
		return d.ServicePort
	}
	return d.Port
}
func (d Deployable) Base(c Config) ObjectRef {
	r := d.BaseService
	if r.Name == "" {
		r.Name = d.Name
	}
	if r.Namespace == "" {
		r.Namespace = d.WorkloadNamespace(c)
	}
	if r.Port == 0 {
		r.Port = d.OverlayPort()
	}
	return r
}
func (d Deployable) Container() string {
	if d.DevContainer != "" {
		return d.DevContainer
	}
	return "app"
}
func (d Deployable) TargetRoot() string {
	if d.SyncRoot != "" {
		return d.SyncRoot
	}
	return "/workspace"
}
