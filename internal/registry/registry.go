package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const manifestAccept = "application/vnd.oci.image.manifest.v1+json, application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.v2+json, application/vnd.docker.distribution.manifest.list.v2+json"

type Resolver struct {
	Client *http.Client
}

func Destination(registry, image, tag string) (repository, tagged string, err error) {
	base := strings.TrimSuffix(strings.TrimSpace(registry), "/")
	if base == "" || image == "" || tag == "" {
		return "", "", fmt.Errorf("registry, image, and tag are required")
	}
	if strings.Contains(base, "://") {
		return "", "", fmt.Errorf("registry must not include a URL scheme")
	}
	repository = base + "/" + strings.Trim(image, "/")
	return repository, repository + ":" + tag, nil
}

func (r Resolver) Resolve(ctx context.Context, tagged string) (string, error) {
	parsed, err := parse(tagged)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.manifestURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", manifestAccept)
	client := r.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", tagged, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("resolve %s: registry returned %s: %s", tagged, resp.Status, strings.TrimSpace(string(body)))
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	digest := resp.Header.Get("Docker-Content-Digest")
	if digest == "" {
		sum := sha256.Sum256(body)
		digest = "sha256:" + hex.EncodeToString(sum[:])
	}
	if !strings.HasPrefix(digest, "sha256:") {
		return "", fmt.Errorf("registry returned unsupported digest %q", digest)
	}
	return parsed.canonicalRepository + "@" + digest, nil
}

type reference struct {
	canonicalRepository string
	manifestURL         string
}

func parse(tagged string) (reference, error) {
	raw := strings.TrimSpace(tagged)
	scheme := "https"
	if !strings.Contains(raw, "://") {
		raw = scheme + "://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return reference{}, fmt.Errorf("invalid image reference %q", tagged)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return reference{}, fmt.Errorf("unsupported registry scheme %q", u.Scheme)
	}
	path := strings.TrimPrefix(u.Path, "/")
	colon := strings.LastIndex(path, ":")
	if colon < 1 || colon == len(path)-1 {
		return reference{}, fmt.Errorf("image reference %q requires a tag", tagged)
	}
	repository, tag := path[:colon], path[colon+1:]
	canonical := u.Host + "/" + repository
	return reference{
		canonicalRepository: canonical,
		manifestURL:         u.Scheme + "://" + u.Host + "/v2/" + repository + "/manifests/" + url.PathEscape(tag),
	}, nil
}
