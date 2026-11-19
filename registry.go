package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

const manifestTypes = "application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.v2+json"

// Registry reads tags, digests and annotations of one repository from an
// OCI distribution registry (GHCR, Docker Hub, a local registry).
type Registry struct {
	base       string // https://ghcr.io/v2/meldgrove/handbook
	scheme     string
	host       string
	repository string
	username   string
	password   string
	client     *http.Client

	mu    sync.Mutex
	token string
}

func NewRegistry(c Config) *Registry {
	return &Registry{
		base:       fmt.Sprintf("%s://%s/v2/%s", c.Scheme, c.Registry, c.Repository),
		scheme:     c.Scheme,
		host:       c.Registry,
		repository: c.Repository,
		username:   c.RegistryUsername,
		password:   c.RegistryPassword,
		client: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(request *http.Request, via []*http.Request) error {
			// Credentials go only to the registry itself, not to a CDN or
			// another host it redirects to (Go keeps them for subdomains).
			if first := via[0].URL; request.URL.Host != first.Host || request.URL.Scheme != first.Scheme {
				request.Header.Del("Authorization")
			}
			if len(via) >= 10 {
				return errors.New("too many redirects")
			}
			return nil
		}},
	}
}

// ImageInfo is what an image's index or config says about it.
type ImageInfo struct {
	Digest   string
	Version  string // org.opencontainers.image.version
	Revision string // org.opencontainers.image.revision
	Created  string // org.opencontainers.image.created
	Source   string // org.opencontainers.image.source
}

// do sends a request, signed in with a token or basic credentials when the
// registry asks for it.
func (r *Registry) do(ctx context.Context, method, target string, accept string) (*http.Response, error) {
	send := func() (*http.Response, error) {
		request, err := http.NewRequestWithContext(ctx, method, target, nil)
		if err != nil {
			return nil, err
		}
		if accept != "" {
			request.Header.Set("Accept", accept)
		}
		r.mu.Lock()
		token := r.token
		r.mu.Unlock()
		if strings.HasPrefix(token, "Basic ") {
			request.Header.Set("Authorization", token)
		} else if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		return r.client.Do(request)
	}
	response, err := send()
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusUnauthorized {
		return response, nil
	}
	challenges := response.Header.Values("WWW-Authenticate")
	drain(response)
	token := ""
	for _, scheme := range []string{"bearer", "basic"} {
		for _, challenge := range challenges {
			if strings.EqualFold(strings.SplitN(challenge, " ", 2)[0], scheme) {
				if found, err := r.signIn(ctx, challenge); err == nil && found != "" {
					token = found
				}
			}
		}
		if token != "" {
			break
		}
	}
	if token == "" {
		return nil, errRefused
	}
	r.mu.Lock()
	r.token = token
	r.mu.Unlock()
	return send()
}

var errRefused = errors.New("refused")

var challengeField = regexp.MustCompile(`(\w+)="([^"]*)"`)

// signIn answers a WWW-Authenticate challenge: a pull token for Bearer, the
// credentials themselves for Basic.
func (r *Registry) signIn(ctx context.Context, challenge string) (string, error) {
	scheme, params, _ := strings.Cut(challenge, " ")
	switch strings.ToLower(scheme) {
	case "basic":
		if r.username == "" {
			return "", errRefused
		}
		request, _ := http.NewRequest(http.MethodGet, "http://x", nil)
		request.SetBasicAuth(r.username, r.password)
		return request.Header.Get("Authorization"), nil
	case "bearer":
	default:
		return "", errRefused
	}
	fields := map[string]string{}
	for _, match := range challengeField.FindAllStringSubmatch(params, -1) {
		fields[strings.ToLower(match[1])] = match[2]
	}
	if fields["realm"] == "" {
		return "", errRefused
	}
	realm, err := url.Parse(fields["realm"])
	if err != nil {
		return "", err
	}
	query := realm.Query()
	query.Set("scope", "repository:"+r.repository+":pull")
	if fields["service"] != "" {
		query.Set("service", fields["service"])
	}
	realm.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return "", err
	}
	if r.username != "" {
		request.SetBasicAuth(r.username, r.password)
	}
	response, err := r.client.Do(request)
	if err != nil {
		return "", err
	}
	defer drain(response)
	if response.StatusCode != http.StatusOK {
		return "", errRefused
	}
	var answer struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&answer); err != nil {
		return "", err
	}
	return orDefault(answer.Token, answer.AccessToken), nil
}

func drain(response *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	_ = response.Body.Close()
}

func (r *Registry) describe(image string, response *http.Response, err error) error {
	switch {
	case errors.Is(err, errRefused) || (response != nil && response.StatusCode == http.StatusUnauthorized):
		return fmt.Errorf("The registry refused access to %s: it does not exist, or it is private and needs REGISTRY_USERNAME and REGISTRY_PASSWORD.", image)
	case err != nil:
		return fmt.Errorf("The registry did not answer for %s: %v", image, err)
	default:
		return fmt.Errorf("The registry answered HTTP %d for %s.", response.StatusCode, image)
	}
}

// Digest is the digest that a tag points to now.
func (r *Registry) Digest(ctx context.Context, tag string) (string, error) {
	image := r.host + "/" + r.repository + ":" + tag
	response, err := r.do(ctx, http.MethodHead, r.base+"/manifests/"+url.PathEscape(tag), manifestTypes)
	if err != nil || response.StatusCode != http.StatusOK {
		if response != nil {
			drain(response)
		}
		return "", r.describe(image, response, err)
	}
	drain(response)
	digest := response.Header.Get("Docker-Content-Digest")
	if digest == "" {
		return "", fmt.Errorf("The registry sent no digest for %s.", image)
	}
	return digest, nil
}

// Tags lists the repository's tags, following the Link header across pages.
func (r *Registry) Tags(ctx context.Context) ([]string, error) {
	var tags []string
	next := r.base + "/tags/list?n=1000"
	for page := 0; next != "" && page < 50; page++ {
		response, err := r.do(ctx, http.MethodGet, next, "")
		if err != nil || response.StatusCode != http.StatusOK {
			if response != nil {
				drain(response)
			}
			return nil, r.describe(r.host+"/"+r.repository, response, err)
		}
		var list struct {
			Tags []string `json:"tags"`
		}
		err = json.NewDecoder(io.LimitReader(response.Body, 16<<20)).Decode(&list)
		drain(response)
		if err != nil {
			return nil, fmt.Errorf("The registry sent a tag list that is no JSON: %v", err)
		}
		tags = append(tags, list.Tags...)
		next = nextPage(response.Header.Get("Link"), response.Request.URL)
		// The registry's credentials go only to the registry.
		if parsed, err := url.Parse(next); next != "" && (err != nil || parsed.Host != r.host || parsed.Scheme != r.scheme) {
			next = ""
		}
	}
	return tags, nil
}

var linkNext = regexp.MustCompile(`<([^>]+)>\s*;\s*rel="?next"?`)

func nextPage(link string, current *url.URL) string {
	match := linkNext.FindStringSubmatch(link)
	if match == nil {
		return ""
	}
	target, err := current.Parse(match[1])
	if err != nil {
		return ""
	}
	return target.String()
}

type manifest struct {
	MediaType   string            `json:"mediaType"`
	Annotations map[string]string `json:"annotations"`
	Manifests   []struct {
		Digest   string `json:"digest"`
		Platform struct {
			Architecture string `json:"architecture"`
			OS           string `json:"os"`
		} `json:"platform"`
	} `json:"manifests"`
	Config struct {
		Digest string `json:"digest"`
	} `json:"config"`
}

func (r *Registry) manifest(ctx context.Context, reference string) (manifest, string, error) {
	var m manifest
	response, err := r.do(ctx, http.MethodGet, r.base+"/manifests/"+url.PathEscape(reference), manifestTypes)
	if err != nil || response.StatusCode != http.StatusOK {
		if response != nil {
			drain(response)
		}
		return m, "", r.describe(r.host+"/"+r.repository+"@"+reference, response, err)
	}
	defer drain(response)
	err = json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&m)
	return m, response.Header.Get("Docker-Content-Digest"), err
}

// Info reads the annotations of the image a tag or digest names: from the
// index, as the Meldgrove release workflow writes them, or else from the
// labels of the linux/amd64 (or first) image's config.
func (r *Registry) Info(ctx context.Context, reference string) (ImageInfo, error) {
	index, digest, err := r.manifest(ctx, reference)
	if err != nil {
		return ImageInfo{}, err
	}
	values := index.Annotations
	if values["org.opencontainers.image.version"] == "" {
		image := index
		if len(index.Manifests) > 0 {
			chosen := index.Manifests[0].Digest
			for _, entry := range index.Manifests {
				if entry.Platform.OS == "linux" && entry.Platform.Architecture == "amd64" {
					chosen = entry.Digest
					break
				}
			}
			if image, _, err = r.manifest(ctx, chosen); err != nil {
				return ImageInfo{}, err
			}
		}
		if image.Config.Digest != "" {
			if labels, err := r.labels(ctx, image.Config.Digest); err == nil {
				merged := map[string]string{}
				for k, v := range labels {
					merged[k] = v
				}
				for k, v := range values {
					merged[k] = v
				}
				values = merged
			}
		}
	}
	if strings.HasPrefix(reference, "sha256:") {
		digest = reference
	}
	return ImageInfo{
		Digest:   digest,
		Version:  values["org.opencontainers.image.version"],
		Revision: values["org.opencontainers.image.revision"],
		Created:  values["org.opencontainers.image.created"],
		Source:   values["org.opencontainers.image.source"],
	}, nil
}

func (r *Registry) labels(ctx context.Context, digest string) (map[string]string, error) {
	response, err := r.do(ctx, http.MethodGet, r.base+"/blobs/"+digest, "")
	if err != nil || response.StatusCode != http.StatusOK {
		if response != nil {
			drain(response)
		}
		return nil, r.describe(r.host+"/"+r.repository+"@"+digest, response, err)
	}
	defer drain(response)
	var config struct {
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"config"`
	}
	err = json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&config)
	return config.Config.Labels, err
}
