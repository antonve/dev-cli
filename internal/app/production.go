package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/antonve/dev-cli/internal/bazel"
	"github.com/antonve/dev-cli/internal/config"
	"github.com/antonve/dev-cli/internal/gitx"
	"github.com/antonve/dev-cli/internal/kube"
	"github.com/antonve/dev-cli/internal/registry"
)

var (
	branchTag          = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*-[0-9a-f]{12}$`)
	imageComponent     = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`)
	fixedPushAttribute = regexp.MustCompile(`(?m)^\s*(?:repository|remote_tags)\s*=`)
)

// publish pushes one image through its repository-owned target and returns
// its digest reference. Production first guards the destination and reuses an
// existing tag, which names a clean, pushed commit.
func publish(ctx context.Context, bz bazel.Bazel, cfg config.Config, imageName, pushTarget, tag string, w io.Writer) (string, error) {
	repository, tagged, err := registry.Destination(cfg.Registry, imageName, tag)
	if err != nil {
		return "", err
	}
	if cfg.Mode == config.ModeProduction {
		if err := guardPush(ctx, bz, cfg.Registry, pushTarget, repository, tag); err != nil {
			return "", err
		}
		existing, err := (registry.Resolver{}).Resolve(ctx, tagged)
		if err == nil {
			fmt.Fprintf(w, "reusing %s\n", tagged)
			return existing, nil
		}
		if !errors.Is(err, registry.ErrNotFound) && !errors.Is(err, registry.ErrNotPullable) {
			return "", err
		}
	}
	fmt.Fprintf(w, "publishing %s\n", tagged)
	if err := bz.PushImage(ctx, pushTarget, repository, tag); err != nil {
		return "", err
	}
	return (registry.Resolver{}).Resolve(ctx, tagged)
}

// guardPush refuses any publication that could replace a release image: a
// tag other than <route>-<commit12>, a destination outside the branch
// registry, or a push target that applies its own repository or tags.
func guardPush(ctx context.Context, bz bazel.Bazel, registryBase, target, repository, tag string) error {
	if !branchTag.MatchString(tag) {
		return fmt.Errorf("refusing tag %q: production publishes only <route>-<commit12> tags", tag)
	}
	image, ok := strings.CutPrefix(repository, strings.TrimSuffix(registryBase, "/")+"/")
	if !ok || !imageComponent.MatchString(image) {
		return fmt.Errorf("refusing destination %s outside %s/", repository, registryBase)
	}
	definition, err := bz.Definition(ctx, target)
	if err != nil {
		return fmt.Errorf("inspect push target %s: %w", target, err)
	}
	if fixedPushAttribute.MatchString(definition) {
		return fmt.Errorf("refusing push target %s: it sets a fixed repository or remote_tags", target)
	}
	return nil
}

// preflight runs before any command that builds or writes route state.
func preflight(ctx context.Context, g gitx.Git, k kube.Client, cfg config.Config, base string) error {
	labels, err := k.NamespaceLabels(ctx, cfg.Namespace)
	if err != nil {
		return err
	}
	if err := checkRoutingNamespace(cfg, labels); err != nil {
		return err
	}
	if cfg.Mode == config.ModeProduction {
		clean, err := g.Clean(ctx)
		if err != nil {
			return err
		}
		if !clean {
			return errors.New("production refuses a working tree that is not clean; commit and push first")
		}
		pushed, err := g.Pushed(ctx)
		if err != nil {
			return err
		}
		if !pushed {
			return errors.New("production refuses a HEAD that is not pushed to origin")
		}
	}
	if len(cfg.RefuseChangedPaths) == 0 {
		return nil
	}
	merge, err := g.MergeBase(ctx, base)
	if err != nil {
		return err
	}
	changed, err := g.Changed(ctx, merge)
	if err != nil {
		return err
	}
	for _, path := range changed {
		for _, prefix := range cfg.RefuseChangedPaths {
			if strings.HasPrefix(filepath.ToSlash(path), prefix) {
				return fmt.Errorf("refusing branch: %s changes %s, which this environment does not deploy", path, prefix)
			}
		}
	}
	return nil
}

func checkRoutingNamespace(cfg config.Config, labels map[string]string) error {
	production := labels["dev-cli.io/environment"] == "production"
	if cfg.Mode != config.ModeProduction {
		if production {
			return fmt.Errorf("development mode refuses routing namespace %s labelled dev-cli.io/environment=production", cfg.Namespace)
		}
		return nil
	}
	if !production || labels["dev-cli.io/routing-enabled"] != "true" {
		return fmt.Errorf(
			"production mode requires routing namespace %s labelled dev-cli.io/routing-enabled=true and dev-cli.io/environment=production",
			cfg.Namespace,
		)
	}
	return nil
}
