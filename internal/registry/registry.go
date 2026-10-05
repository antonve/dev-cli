package registry

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

var (
	ErrNotFound    = errors.New("image tag not found")
	ErrNotPullable = errors.New("image is not anonymously pullable")
)

var challengeParam = regexp.MustCompile(`(\w+)="([^"]*)"`)

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

// Resolve returns the digest reference of a tag as an anonymous client sees
// it, answering a registry's Bearer challenge with an anonymous token.
func (r Resolver) Resolve(ctx context.Context, tagged string) (string, error) {
	parsed, err := parse(tagged)
	if err != nil {
		return "", err
	}
	client := r.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := getManifest(ctx, client, parsed.manifestURL, "")
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", tagged, err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		challenge := resp.Header.Get("WWW-Authenticate")
		resp.Body.Close()
		token, err := anonymousToken(ctx, client, challenge, parsed.canonicalRepository)
		if err != nil {
			return "", fmt.Errorf("resolve %s: %w", tagged, err)
		}
		if resp, err = getManifest(ctx, client, parsed.manifestURL, token); err != nil {
			return "", fmt.Errorf("resolve %s: %w", tagged, err)
		}
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			resp.Body.Close()
			return "", fmt.Errorf("resolve %s: %w: %s", tagged, ErrNotPullable, parsed.canonicalRepository)
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("resolve %s: %w", tagged, ErrNotFound)
	}
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

func getManifest(ctx context.Context, client *http.Client, manifestURL, token string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, manifestURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", manifestAccept)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return client.Do(req)
}

func anonymousToken(ctx context.Context, client *http.Client, challenge, repository string) (string, error) {
	scheme, rawParams, _ := strings.Cut(challenge, " ")
	if !strings.EqualFold(scheme, "Bearer") {
		return "", fmt.Errorf("registry requires unsupported authentication %q", challenge)
	}
	params := map[string]string{}
	for _, match := range challengeParam.FindAllStringSubmatch(rawParams, -1) {
		params[strings.ToLower(match[1])] = match[2]
	}
	realm, err := url.Parse(params["realm"])
	if err != nil || realm.Host == "" || realm.Scheme != "https" && realm.Scheme != "http" {
		return "", fmt.Errorf("registry returned invalid token realm %q", params["realm"])
	}
	query := realm.Query()
	for _, key := range []string{"service", "scope"} {
		if params[key] != "" {
			query.Set(key, params[key])
		}
	}
	realm.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return "", fmt.Errorf("%w: %s", ErrNotPullable, repository)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("anonymous token request returned %s", resp.Status)
	}
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return "", fmt.Errorf("decode anonymous token: %w", err)
	}
	if token := cmp.Or(body.Token, body.AccessToken); token != "" {
		return token, nil
	}
	return "", errors.New("anonymous token response has no token")
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
