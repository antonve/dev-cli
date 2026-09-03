package config

import (
	"encoding/json"
	"fmt"
	"os"
)

type Config struct {
	KubeContext      string `json:"kubeContext"`
	Namespace        string `json:"namespace"`
	Registry         string `json:"registry"`
	IngressHost      string `json:"ingressHost"`
	IngressClass     string `json:"ingressClass"`
	GatewayName      string `json:"gatewayName"`
	GatewayNamespace string `json:"gatewayNamespace"`
	CookieName       string `json:"cookieName"`
	TTL              string `json:"ttl"`
	MetadataQuery    string `json:"metadataQuery"`
}

type Deployable struct {
	Name           string   `json:"name"`
	Kind           string   `json:"kind"`
	BuildTarget    string   `json:"buildTarget"`
	ImageName      string   `json:"imageName"`
	ImageTarget    string   `json:"imageTarget"`
	PushTarget     string   `json:"pushTarget"`
	Port           int      `json:"port"`
	ReadinessPath  string   `json:"readinessPath"`
	SourceRoots    []string `json:"sourceRoots"`
	SyncPaths      []string `json:"syncPaths"`
	BinaryPath     string   `json:"binaryPath"`
	ContainerPath  string   `json:"containerPath"`
	DevCommand     []string `json:"devCommand"`
	PublicPath     string   `json:"publicPath"`
	InternalHost   string   `json:"internalHost"`
	MetadataTarget string   `json:"metadataTarget,omitempty"`
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
	if c.TTL == "" {
		c.TTL = "8h"
	}
	if c.MetadataQuery == "" {
		c.MetadataQuery = "kind(dev_deployable, //...)"
	}
	return c, nil
}
