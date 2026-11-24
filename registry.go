package main

import (
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

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// get reads a manifest or blob (kind "manifests" or "blobs") of at most
// limit bytes. Content named by a digest must have that digest.
func (r *Registry) get(ctx context.Context, kind, reference, accept string, limit int64) ([]byte, *http.Response, error) {
	response, err := r.do(ctx, http.MethodGet, r.base+"/"+kind+"/"+url.PathEscape(reference), accept)
	if err != nil || response.StatusCode != http.StatusOK {
		if response != nil {
			drain(response)
		}
		return nil, response, r.describe(r.host+"/"+r.repository+"@"+reference, response, err)
	}
	defer drain(response)
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, response, fmt.Errorf("The registry broke off sending %s@%s: %v", r.repository, reference, err)
	}
	if int64(len(data)) > limit {
		return nil, response, fmt.Errorf("The registry sent more than %d bytes for %s@%s.", limit, r.repository, reference)
	}
	if strings.HasPrefix(reference, "sha256:") {
		sum := sha256.Sum256(data)
		if "sha256:"+hex.EncodeToString(sum[:]) != reference {
			return nil, response, fmt.Errorf("The registry sent other content for %s@%s than its digest names.", r.repository, reference)
		}
	}
	return data, response, nil
}

func (r *Registry) manifest(ctx context.Context, reference string) (manifest, string, error) {
	var m manifest
	data, response, err := r.get(ctx, "manifests", reference, manifestTypes, 4<<20)
	if err != nil {
		return m, "", err
	}
	err = json.Unmarshal(data, &m)
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
	data, _, err := r.get(ctx, "blobs", digest, "", 4<<20)
	if err != nil {
		return nil, err
	}
	var config struct {
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"config"`
	}
	err = json.Unmarshal(data, &config)
	return config.Config.Labels, err
}

// descriptor points to a manifest or blob.
type descriptor struct {
	MediaType    string `json:"mediaType"`
	ArtifactType string `json:"artifactType"`
	Digest       string `json:"digest"`
}

// referrers lists what refers to an image (OCI 1.1): through the referrers
// API, or, where the registry answers 404 to that (GHCR), the index under
// the tag sha256-<hex> (the referrers tag schema). An image without
// referrers: an empty list.
func (r *Registry) referrers(ctx context.Context, digest string) ([]descriptor, error) {
	var index struct {
		Manifests []descriptor `json:"manifests"`
	}
	const indexType = "application/vnd.oci.image.index.v1+json"
	response, err := r.do(ctx, http.MethodGet, r.base+"/referrers/"+digest, indexType)
	if err != nil {
		return nil, r.describe(r.host+"/"+r.repository+"@"+digest, nil, err)
	}
	if response.StatusCode == http.StatusOK {
		defer drain(response)
		if err := json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&index); err != nil {
			return nil, fmt.Errorf("The registry sent a list of referrers that is no JSON: %v", err)
		}
		return index.Manifests, nil
	}
	drain(response)
	if response.StatusCode != http.StatusNotFound {
		return nil, r.describe(r.host+"/"+r.repository+"@"+digest, response, nil)
	}
	tag := strings.Replace(digest, ":", "-", 1)
	data, response, err := r.get(ctx, "manifests", tag, indexType, 4<<20)
	if response != nil && response.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &index); err != nil {
		return nil, fmt.Errorf("The registry sent a list of referrers that is no JSON: %v", err)
	}
	return index.Manifests, nil
}

// Attestations reads the Sigstore bundles that refer to an image, as the
// release workflow pushes them (actions/attest, push-to-registry): an OCI
// manifest whose layer is the bundle. None: an empty list.
func (r *Registry) Attestations(ctx context.Context, digest string) ([][]byte, error) {
	if !digestPattern.MatchString(digest) {
		return nil, fmt.Errorf("%s is no image digest", digest)
	}
	referrers, err := r.referrers(ctx, digest)
	if err != nil {
		return nil, err
	}
	var bundles [][]byte
	for _, referrer := range referrers {
		if !strings.HasPrefix(referrer.ArtifactType, bundleType) || !digestPattern.MatchString(referrer.Digest) {
			continue
		}
		if len(bundles) == 10 {
			break
		}
		data, _, err := r.get(ctx, "manifests", referrer.Digest, "application/vnd.oci.image.manifest.v1+json", 1<<20)
		if err != nil {
			return nil, err
		}
		var m struct {
			Subject *descriptor  `json:"subject"`
			Layers  []descriptor `json:"layers"`
		}
		if err := json.Unmarshal(data, &m); err != nil {
			return nil, fmt.Errorf("The registry sent an attestation of %s that is no JSON: %v", digest, err)
		}
		if m.Subject != nil && m.Subject.Digest != digest {
			continue // it refers to another image
		}
		for _, layer := range m.Layers {
			if !strings.HasPrefix(layer.MediaType, bundleType) || !digestPattern.MatchString(layer.Digest) {
				continue
			}
			bundle, _, err := r.get(ctx, "blobs", layer.Digest, "", 4<<20)
			if err != nil {
				return nil, err
			}
			bundles = append(bundles, bundle)
			break
		}
	}
	return bundles, nil
}
