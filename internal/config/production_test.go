package config

import (
	"strings"
	"testing"
)

const productionBase = "mode: production\nkubeContext: prod\nnamespace: branches\nhostTLS: gateway\n"

func TestProductionConfigRequirements(t *testing.T) {
	valid := productionBase +
		"registry: ghcr.io/team/repo/branches\n" +
		"bazelArgs: [--config=release]\n" +
		"refuseChangedPaths: [services/api/migrations/]\n"
	cfg, err := Load(writeConfig(t, valid))
	if err != nil || cfg.Mode != ModeProduction {
		t.Fatalf("valid production config: %#v %v", cfg.Mode, err)
	}

	for name, body := range map[string]string{
		"registry outside branches": strings.Replace(valid, "repo/branches", "repo", 1),
		"no release flags":          strings.Replace(valid, "--config=release", "--config=dev", 1),
		"no refused paths":          strings.Replace(valid, "refuseChangedPaths: [services/api/migrations/]\n", "", 1),
		"absolute refused path":     strings.Replace(valid, "services/api/migrations/", "/etc", 1),
		"unknown mode":              strings.Replace(valid, "mode: production", "mode: staging", 1),
	} {
		if _, err := Load(writeConfig(t, body)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}

	cfg, err = Load(writeConfig(t, environmentBase+"clusterIssuer: ca\n"))
	if err != nil || cfg.Mode != ModeDevelopment {
		t.Fatalf("development default: %#v %v", cfg.Mode, err)
	}
}

func TestDeployableImageFollowsMode(t *testing.T) {
	d := Deployable{ImageName: "api-dev", PushTarget: "//:dev_push", Release: Release{ImageName: "api", PushTarget: "//:branch_push"}}
	if name, target := d.Image(ModeDevelopment); name != "api-dev" || target != "//:dev_push" {
		t.Fatalf("development image %s %s", name, target)
	}
	if name, target := d.Image(ModeProduction); name != "api" || target != "//:branch_push" {
		t.Fatalf("production image %s %s", name, target)
	}
}
