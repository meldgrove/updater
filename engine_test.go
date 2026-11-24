package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRegistry serves tags with index annotations, as GHCR does for images
// of the Meldgrove release workflow. Anonymous pulls need a token. Content
// is addressed by its digest. It can also serve recorded images (a real
// index) with their Sigstore bundles under the referrers tag sha256-<hex>,
// as GHCR does.
type fakeRegistry struct {
	mu       sync.Mutex
	tags     map[string]string   // tag: version annotation
	head     map[string]string   // moving tags (latest, 0.2, preview): version they point to
	recorded map[string][]byte   // tag: a recorded index, which wins over tags and head
	content  map[string][]byte   // digest: a manifest or blob of attestations and recorded images
	attached map[string][]string // image digest: digests of its attestation manifests
	failing  map[string]int      // part of a path: the HTTP status it answers
}

func newFakeRegistry() *fakeRegistry {
	return &fakeRegistry{tags: map[string]string{}, head: map[string]string{}, recorded: map[string][]byte{},
		content: map[string][]byte{}, attached: map[string][]string{}, failing: map[string]int{}}
}

func sha256Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// indexOf is the index the fake registry serves for a version.
func indexOf(version string) []byte {
	data, _ := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.index.v1+json",
		"manifests":     []any{},
		"annotations": map[string]string{
			"org.opencontainers.image.version":  version,
			"org.opencontainers.image.revision": fmt.Sprintf("%040x", len(version)*7919),
			"org.opencontainers.image.created":  "2026-11-10T10:00:00Z",
			"org.opencontainers.image.source":   "https://github.com/meldgrove/app",
		},
	})
	return data
}

func digestOf(version string) string { return sha256Digest(indexOf(version)) }

func (f *fakeRegistry) set(tags ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, tag := range tags {
		f.tags[tag] = tag
	}
}

func (f *fakeRegistry) point(tag, version string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.head[tag] = version
}

// record serves a recorded index under a tag; it answers its digest.
func (f *fakeRegistry) record(tag string, index []byte) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recorded[tag] = index
	f.content[sha256Digest(index)] = index
	return sha256Digest(index)
}

// attach adds a Sigstore bundle to the image at digest, as actions/attest
// pushes it: a manifest whose layer is the bundle.
func (f *fakeRegistry) attach(digest string, bundle []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	const bundleType = "application/vnd.dev.sigstore.bundle.v0.3+json"
	manifest, _ := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"artifactType":  bundleType,
		"config":        map[string]any{"mediaType": "application/vnd.oci.empty.v1+json", "digest": sha256Digest([]byte("{}")), "size": 2},
		"layers":        []any{map[string]any{"mediaType": bundleType, "digest": sha256Digest(bundle), "size": len(bundle)}},
		"subject":       map[string]any{"mediaType": "application/vnd.oci.image.index.v1+json", "digest": digest},
	})
	f.content[sha256Digest(bundle)] = bundle
	f.content[sha256Digest(manifest)] = manifest
	f.attached[digest] = append(f.attached[digest], sha256Digest(manifest))
}

// detach removes the attestations of the image at digest.
func (f *fakeRegistry) detach(digest string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.attached, digest)
}

// fail makes requests whose path contains part answer status; 0 heals.
func (f *fakeRegistry) fail(part string, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if status == 0 {
		delete(f.failing, part)
	} else {
		f.failing[part] = status
	}
}

// find answers the content of a tag or digest.
func (f *fakeRegistry) find(reference string) ([]byte, bool) {
	if index, ok := f.recorded[reference]; ok {
		return index, true
	}
	if version, ok := f.head[reference]; ok {
		return indexOf(version), true
	}
	if version, ok := f.tags[reference]; ok {
		return indexOf(version), true
	}
	if strings.HasPrefix(reference, "sha256:") {
		if data, ok := f.content[reference]; ok {
			return data, true
		}
		for _, v := range f.tags {
			if digestOf(v) == reference {
				return indexOf(v), true
			}
		}
		for _, v := range f.head {
			if digestOf(v) == reference {
				return indexOf(v), true
			}
		}
	}
	if subject, ok := strings.CutPrefix(reference, "sha256-"); ok {
		attached := f.attached["sha256:"+subject]
		if len(attached) == 0 {
			return nil, false
		}
		var manifests []any
		for _, digest := range attached {
			manifests = append(manifests, map[string]any{"mediaType": "application/vnd.oci.image.manifest.v1+json", "digest": digest,
				"size": len(f.content[digest]), "artifactType": "application/vnd.dev.sigstore.bundle.v0.3+json"})
		}
		data, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.index.v1+json", "manifests": manifests})
		return data, true
	}
	return nil, false
}

func (f *fakeRegistry) serve(t *testing.T) *httptest.Server {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			fmt.Fprint(w, `{"token":"pull"}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer pull" {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="test"`, server.URL))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		for part, status := range f.failing {
			if strings.Contains(r.URL.Path, part) {
				w.WriteHeader(status)
				return
			}
		}
		const prefix = "/v2/meldgrove/app/"
		path := strings.TrimPrefix(r.URL.Path, prefix)
		switch {
		case path == "tags/list":
			var list []string
			for tag := range f.tags {
				list = append(list, tag)
			}
			for tag := range f.head {
				list = append(list, tag)
			}
			for tag := range f.recorded {
				list = append(list, tag)
			}
			json.NewEncoder(w).Encode(map[string]any{"name": "meldgrove/app", "tags": list})
		case strings.HasPrefix(path, "manifests/"), strings.HasPrefix(path, "blobs/"):
			_, reference, _ := strings.Cut(path, "/")
			data, ok := f.find(reference)
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `{"errors":[{"code":"MANIFEST_UNKNOWN","message":"manifest unknown"}]}`)
				return
			}
			var m struct {
				MediaType string `json:"mediaType"`
			}
			json.Unmarshal(data, &m)
			w.Header().Set("Docker-Content-Digest", sha256Digest(data))
			w.Header().Set("Content-Type", orDefault(m.MediaType, "application/octet-stream"))
			if r.Method == http.MethodHead {
				return
			}
			w.Write(data)
		default:
			// Referrers too, as on GHCR.
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"errors":[{"code":"MANIFEST_UNKNOWN","message":"manifest unknown"}]}`)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// fakeGitHub publishes every release on 1 November 2026 with one note.
func fakeGitHub(t *testing.T, published time.Time) *httptest.Server {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tag, ok := strings.CutPrefix(r.URL.Path, "/repos/meldgrove/app/releases/tags/v"); ok {
			json.NewEncoder(w).Encode(map[string]any{
				"html_url":     "https://github.com/meldgrove/app/releases/tag/v" + tag,
				"published_at": published.Format(time.RFC3339),
				"body":         "## What's Changed\n* feat: Change of " + tag + " by @a in https://github.com/meldgrove/app/pull/1\n",
			})
			return
		}
		if strings.HasPrefix(r.URL.Path, "/repos/meldgrove/app/compare/") {
			json.NewEncoder(w).Encode(map[string]any{"commits": []any{
				map[string]any{"commit": map[string]string{"message": "Merge pull request #5 from meldgrove/x\n\nfeat: Previewed change"}, "parents": []any{map[string]any{}, map[string]any{}}},
			}})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(server.Close)
	return server
}

type harness struct {
	t        *testing.T
	engine   *Engine
	registry *fakeRegistry
	clock    time.Time
	trigger  string
	logs     []string
	mu       sync.Mutex
}

func newHarness(t *testing.T, env map[string]string, published time.Time) *harness {
	h := &harness{t: t, registry: newFakeRegistry(), trigger: t.TempDir()}
	h.clock = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	registry := h.registry.serve(t)
	github := fakeGitHub(t, published)
	host := strings.TrimPrefix(registry.URL, "http://")
	values := map[string]string{
		"IMAGE":            host + "/meldgrove/app:0.2.0",
		"VERSION_VARIABLE": "APP_VERSION_TAG",
		"TRIGGER_DIR":      h.trigger,
		"GITHUB_API_URL":   github.URL,
		"APP_VERSION":      "0.2.0-test",
	}
	for k, v := range env {
		values[k] = strings.ReplaceAll(v, "REGISTRY", host)
	}
	cfg, err := LoadConfig(func(name string) string { return values[name] })
	if err != nil {
		t.Fatal(err)
	}
	h.engine = NewEngine(cfg, func(format string, args ...any) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.logs = append(h.logs, fmt.Sprintf(format, args...))
	})
	h.engine.now = func() time.Time { return h.clock }
	h.engine.hostname = "container-1"
	return h
}

func (h *harness) watch(protocol int) {
	content := ""
	if protocol == 2 {
		content = "protocol=2\nproject=app\n"
	}
	if err := os.WriteFile(filepath.Join(h.trigger, "watched"), []byte(content), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) cycle() {
	h.engine.check(context.Background())
	h.engine.autoInstall(context.Background())
}

func (h *harness) status() Status { return h.engine.Status() }

func (h *harness) request() map[string]string {
	fields, ok := readFields(filepath.Join(h.trigger, "deploy"))
	if !ok {
		return nil
	}
	return fields
}

func (h *harness) report(file string, lines ...string) {
	if err := os.WriteFile(filepath.Join(h.trigger, file), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) logged(part string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, line := range h.logs {
		if strings.Contains(line, part) {
			return true
		}
	}
	return false
}

func TestStableOffersTheSeriesAndFlagsANewSeries(t *testing.T) {
	h := newHarness(t, map[string]string{"COOLDOWN": "3d"}, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
	h.registry.set("0.1.9", "0.2.0", "0.2.1", "0.2.2", "0.2.3-preview.1", "0.3.0", "0.3.1-preview.2")
	h.watch(2)
	h.engine.start()
	h.engine.check(context.Background())
	s := h.status()
	if s.Mode != "host" || s.Channel != "stable" || s.ChannelLocked != nil || *s.Installed != "0.2.0" {
		t.Fatalf("status = %+v", s)
	}
	if s.Available == nil || s.Available.Version != "0.2.2" || s.Available.Preview {
		t.Fatalf("available = %+v", s.Available)
	}
	if s.Attention == nil || s.Attention.Version != "0.3.0" || s.Attention.InstallsAt != nil {
		t.Fatalf("attention = %+v", s.Attention)
	}
	if strings.Join(s.Available.Notes, "|") != "feat: Change of 0.2.2|feat: Change of 0.2.1" {
		t.Errorf("notes = %q", s.Available.Notes)
	}
	if s.Available.URL == nil || *s.Available.URL != "https://github.com/meldgrove/app/releases/tag/v0.2.2" {
		t.Errorf("url = %v", s.Available.URL)
	}
	// Published 18 days ago, so out for the cooldown: it installs at once.
	if s.Available.InstallsAt == nil || !s.Available.InstallsAt.Equal(h.clock) {
		t.Errorf("installsAt = %v", s.Available.InstallsAt)
	}
}

func TestStableWaitsForTheCooldownAndInstallsTheNewestReadyRelease(t *testing.T) {
	h := newHarness(t, map[string]string{"COOLDOWN": "3d"}, time.Time{}) // GitHub has no dates
	h.registry.set("0.2.0", "0.2.1")
	h.watch(2)
	h.engine.start()
	h.cycle()
	if h.request() != nil {
		t.Fatal("installed a release that was just seen")
	}
	if at := h.status().Available.InstallsAt; at == nil || !at.Equal(h.clock.Add(72*time.Hour)) {
		t.Fatalf("installsAt = %v", at)
	}
	h.clock = h.clock.Add(48 * time.Hour)
	h.registry.set("0.2.2")
	h.cycle()
	if h.request() != nil {
		t.Fatal("installed before the cooldown")
	}
	h.clock = h.clock.Add(25 * time.Hour)
	h.cycle()
	request := h.request()
	if request["protocol"] != "2" || request["version"] != "0.2.1" || request["id"] == "" {
		t.Fatalf("request = %v (0.2.2 is out for one day only, so 0.2.1 installs)", request)
	}
	s := h.status()
	if s.Installing == nil || s.Installing.Version != "0.2.1" || s.Installing.Manual || s.Installing.Stage != "requested" {
		t.Fatalf("installing = %+v", s.Installing)
	}
}

func TestHostResultsAndRollback(t *testing.T) {
	h := newHarness(t, nil, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
	h.registry.set("0.2.0", "0.2.1")
	h.watch(2)
	h.engine.start()
	h.cycle()
	id := h.request()["id"]
	if id == "" {
		t.Fatal("no request")
	}
	// The server takes the request and reports its stages.
	os.Remove(filepath.Join(h.trigger, "deploy"))
	for _, stage := range []string{"pulling", "backing-up", "starting"} {
		h.report("progress", "id="+id, "stage="+stage)
		h.engine.pollHost()
		if s := h.status(); s.Installing == nil || s.Installing.Stage != stage {
			t.Fatalf("installing = %+v, want the stage %s", s.Installing, stage)
		}
	}
	h.report("progress", "id="+id, "stage=dancing")
	h.engine.pollHost()
	if s := h.status(); s.Installing == nil || s.Installing.Stage != "starting" {
		t.Fatalf("an unknown stage was taken: %+v", s.Installing)
	}
	h.report("result", "id="+id, "version=0.2.1", "result=rolled-back", `message=container app-web-1 is unhealthy`, "finished=1795000000")
	h.engine.pollHost()
	s := h.status()
	if s.Installing != nil || s.Last == nil || s.Last.Result != "rolled-back" || *s.Last.Message != "container app-web-1 is unhealthy" {
		t.Fatalf("last = %+v", s.Last)
	}
	if s.Available == nil || s.Available.Failed == nil || s.Available.InstallsAt != nil {
		t.Fatalf("available after the failure = %+v", s.Available)
	}
	// A failed version does not install again on its own; a newer one does.
	h.cycle()
	if h.request() != nil {
		t.Fatal("tried the failed version again")
	}
	if s := h.status(); s.Available.Failed == nil {
		t.Fatal("the failure was forgotten at the next check")
	}
	h.registry.set("0.2.2")
	h.cycle()
	if v := h.request()["version"]; v != "0.2.2" {
		t.Fatalf("request = %v", h.request())
	}
}

func TestManualInstallSkipsCooldownAndWindowAndTakesAttention(t *testing.T) {
	window := time.Date(2026, 11, 19, 15, 0, 0, 0, time.UTC).Format("15:04") + "-16:00"
	h := newHarness(t, map[string]string{"COOLDOWN": "3d", "WINDOW": window}, time.Time{})
	h.registry.set("0.2.0", "0.2.1", "0.3.0")
	h.watch(2)
	h.engine.start()
	h.cycle()
	if h.request() != nil {
		t.Fatal("installed automatically")
	}
	if err := h.engine.manualInstall(context.Background(), "0.4.0"); err == nil || !strings.Contains(err.Error(), "not offered") {
		t.Fatalf("installing an unknown version: %v", err)
	}
	if err := h.engine.manualInstall(context.Background(), "0.3.0"); err != nil {
		t.Fatal(err)
	}
	if v := h.request()["version"]; v != "0.3.0" {
		t.Fatalf("request = %v", h.request())
	}
	if err := h.engine.manualInstall(context.Background(), "0.2.1"); err == nil || !strings.Contains(err.Error(), "is being installed") {
		t.Fatalf("a second installation: %v", err)
	}
}

func TestWindowDefersAutomaticInstalls(t *testing.T) {
	h := newHarness(t, map[string]string{"WINDOW": "02:00-05:00", "TZ": "Europe/Berlin"}, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
	h.registry.set("0.2.0", "0.2.1")
	h.watch(2)
	h.engine.start()
	h.cycle() // 13:00 in Berlin
	if h.request() != nil {
		t.Fatal("installed outside the window")
	}
	at := h.status().Available.InstallsAt
	if at == nil || !at.Equal(time.Date(2026, 11, 20, 1, 0, 0, 0, time.UTC)) {
		t.Fatalf("installsAt = %v, want 02:00 in Berlin", at)
	}
	if wait := h.engine.nextCheck(); wait != time.Hour {
		t.Errorf("next check in %v", wait)
	}
	h.clock = time.Date(2026, 11, 20, 1, 30, 0, 0, time.UTC)
	h.cycle()
	if v := h.request()["version"]; v != "0.2.1" {
		t.Fatalf("request inside the window = %v", h.request())
	}
}

func TestPreviewChannel(t *testing.T) {
	h := newHarness(t, map[string]string{"COOLDOWN": "3d"}, time.Time{})
	h.registry.set("0.2.0", "0.2.1-preview.1", "0.2.1-preview.4")
	h.watch(2)
	h.engine.start()
	h.engine.check(context.Background())
	if s := h.status(); s.Available != nil {
		t.Fatalf("stable offers a preview: %+v", s.Available)
	}
	handler := h.engine.Handler(context.Background())
	go h.engine.serveRequests(t)
	response := call(t, handler, "POST", "/v1/channel", `{"channel":"preview"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("channel: %d %s", response.Code, response.Body)
	}
	var s Status
	json.Unmarshal(response.Body.Bytes(), &s)
	if s.Channel != "preview" || s.Available == nil || s.Available.Version != "0.2.1-preview.4" || !s.Available.Preview || s.Cooldown == nil {
		t.Fatalf("status = %+v / %+v", s, s.Available)
	}
	if strings.Join(s.Available.Notes, "") != "feat: Previewed change" || s.Available.URL == nil || !strings.Contains(*s.Available.URL, "/compare/v0.2.0...") {
		t.Errorf("preview notes %q url %v", s.Available.Notes, s.Available.URL)
	}
	// Previews install without the cooldown.
	h.cycle()
	if v := h.request()["version"]; v != "0.2.1-preview.4" {
		t.Fatalf("request = %v", h.request())
	}
	// Back on stable, a newer release installs; the preview never goes back.
	os.Remove(filepath.Join(h.trigger, "deploy"))
	h.engine.mu.Lock()
	h.engine.state.Install = nil
	h.engine.mu.Unlock()
	h.engine.cfg.Tag = "0.2.1-preview.4"
	h.engine.hostname = "container-2"
	h.engine.start()
	if s := h.status(); s.Channel != "preview" || *s.Installed != "0.2.1-preview.4" {
		t.Fatalf("after the deployment: %+v", s)
	}
	h.engine.mu.Lock()
	h.engine.state.Channel = "stable"
	h.engine.mu.Unlock()
	h.engine.check(context.Background())
	if s := h.status(); s.Available != nil {
		t.Fatalf("stable offers an older release: %+v", s.Available)
	}
	h.registry.set("0.2.1")
	h.engine.check(context.Background())
	if s := h.status(); s.Available == nil || s.Available.Version != "0.2.1" {
		t.Fatalf("stable after the release: %+v", s.Available)
	}
}

func TestMovingTagResolvesTheInstalledVersion(t *testing.T) {
	h := newHarness(t, map[string]string{"IMAGE": "REGISTRY/meldgrove/app:0.2"}, time.Time{})
	h.registry.set("0.2.0", "0.2.1", "0.2.2")
	h.registry.point("0.2", "0.2.1")
	h.watch(2)
	h.engine.start()
	h.engine.check(context.Background())
	s := h.status()
	if s.Installed == nil || *s.Installed != "0.2.1" || s.Available == nil || s.Available.Version != "0.2.2" {
		t.Fatalf("status = %+v", s)
	}
}

func TestPlatformFollowsTheTag(t *testing.T) {
	var calls []string
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.Header.Get("Authorization"))
	}))
	defer hook.Close()
	h := newHarness(t, map[string]string{"IMAGE": "REGISTRY/meldgrove/app:latest", "DEPLOY_HOOK_URL": hook.URL + "/deploy", "DEPLOY_HOOK_TOKEN": "secret"}, time.Time{})
	h.registry.set("0.2.0", "0.2.1")
	h.registry.point("latest", "0.2.0")
	h.engine.start()
	h.cycle()
	s := h.status()
	if s.Mode != "platform" || s.ChannelLocked == nil || s.Available != nil || *s.Installed != "0.2.0" {
		t.Fatalf("status = %+v", s)
	}
	if len(calls) != 0 {
		t.Fatal("deployed an unchanged tag")
	}
	h.registry.point("latest", "0.2.1")
	h.cycle()
	if len(calls) != 1 || calls[0] != "POST Bearer secret" {
		t.Fatalf("calls = %v", calls)
	}
	s = h.status()
	if s.Last == nil || s.Last.Result != "accepted" || s.Last.Version != "0.2.1" || s.Available != nil {
		t.Fatalf("after the deploy: %+v", s.Last)
	}
	h.cycle()
	if len(calls) != 1 {
		t.Fatal("deployed the same image again")
	}
	if response := call(t, h.engine.Handler(context.Background()), "POST", "/v1/channel", `{"channel":"preview"}`); response.Code != http.StatusConflict {
		t.Fatalf("channel on a platform: %d", response.Code)
	}
}

func TestPlatformInstallsPreviewsWithoutTheCooldown(t *testing.T) {
	calls := 0
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	defer hook.Close()
	h := newHarness(t, map[string]string{"IMAGE": "REGISTRY/meldgrove/app:preview", "DEPLOY_HOOK_URL": hook.URL + "/deploy", "COOLDOWN": "3d"}, time.Time{})
	h.registry.set("0.2.0", "0.2.1-preview.1", "0.2.1-preview.2")
	h.registry.point("preview", "0.2.1-preview.1")
	h.engine.start()
	h.cycle()
	h.registry.point("preview", "0.2.1-preview.2")
	h.cycle()
	if calls != 1 {
		t.Fatalf("deployments = %d, available = %+v", calls, h.status().Available)
	}
	if h.logged("It installs once") {
		t.Error("the log says the preview waits for the cooldown")
	}
}

func TestAnInvalidDeployLinkStaysOutOfTheStatus(t *testing.T) {
	h := newHarness(t, map[string]string{"IMAGE": "REGISTRY/meldgrove/app:latest", "DEPLOY_HOOK_URL": "https://deploy.example/hook/secret-key%zz"}, time.Time{})
	h.registry.set("0.2.0", "0.2.1")
	h.registry.point("latest", "0.2.0")
	h.engine.start()
	h.cycle()
	h.registry.point("latest", "0.2.1")
	h.cycle()
	s := h.status()
	if s.Last == nil || s.Last.Result != "failed" || s.Last.Message == nil {
		t.Fatalf("last = %+v", s.Last)
	}
	state, err := os.ReadFile(filepath.Join(h.trigger, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(*s.Last.Message, "secret-key") || strings.Contains(string(state), "secret-key") || h.logged("secret-key") {
		t.Errorf("the deploy link is shown: %q", *s.Last.Message)
	}
}

func TestLegacyHostFollowsTheTag(t *testing.T) {
	h := newHarness(t, map[string]string{"IMAGE": "REGISTRY/meldgrove/app:0.2"}, time.Time{})
	h.registry.set("0.2.0", "0.2.1", "0.3.0")
	h.registry.point("0.2", "0.2.0")
	h.watch(1)
	h.engine.start()
	h.cycle()
	h.registry.point("0.2", "0.2.1")
	h.cycle()
	request, _ := os.ReadFile(filepath.Join(h.trigger, "deploy"))
	if strings.TrimSpace(string(request)) != digestOf("0.2.1") {
		t.Fatalf("request = %q", request)
	}
	if s := h.status(); s.ChannelLocked == nil || s.Attention == nil || s.Attention.Version != "0.3.0" {
		t.Fatalf("status = %+v", s)
	}
	if !h.logged("Release 0.3.0 is out, and the tag 0.2 does not reach it") {
		t.Error("the newer series was not logged")
	}
	os.Remove(filepath.Join(h.trigger, "deploy"))
	h.engine.pollHost()
	if s := h.status(); s.Installing != nil || s.Last.Result != "accepted" {
		t.Fatalf("after the host took it: %+v", s)
	}
}

func TestUnansweredRequestIsTakenBack(t *testing.T) {
	h := newHarness(t, nil, time.Time{})
	h.registry.set("0.2.0", "0.2.1")
	h.watch(2)
	h.engine.start()
	h.cycle()
	h.clock = h.clock.Add(11 * time.Minute)
	h.engine.pollHost()
	s := h.status()
	if h.request() != nil || s.Installing != nil || s.Last == nil || s.Last.Result != "failed" || !strings.Contains(*s.Last.Message, "meldgrove-updater-app.path") {
		t.Fatalf("status = %+v", s.Last)
	}
	// Not counted as a failure of the version: the next check asks again.
	h.cycle()
	if h.request() == nil {
		t.Fatal("did not ask again")
	}
}

func TestOffModeShowsUpdatesButInstallsNothing(t *testing.T) {
	h := newHarness(t, nil, time.Time{})
	h.registry.set("0.2.0", "0.2.1")
	h.engine.start()
	go h.engine.serveRequests(t)
	h.cycle()
	s := h.status()
	if s.Mode != "off" || s.Automatic || s.Available == nil || s.Available.InstallsAt != nil || s.ChannelLocked != nil {
		t.Fatalf("status = %+v", s)
	}
	response := call(t, h.engine.Handler(context.Background()), "POST", "/v1/install", `{"version":"0.2.1"}`)
	if response.Code != http.StatusServiceUnavailable && response.Code != http.StatusConflict {
		t.Fatalf("install while off: %d", response.Code)
	}
}

func TestOffModeDoesNotWakeForTheWindow(t *testing.T) {
	h := newHarness(t, map[string]string{"WINDOW": "02:00-05:00"}, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
	h.registry.set("0.2.0", "0.2.1")
	h.watch(2)
	h.engine.start()
	h.cycle() // 12:00: the update waits for the window
	if err := os.Remove(filepath.Join(h.trigger, "watched")); err != nil {
		t.Fatal(err)
	}
	h.clock = time.Date(2026, 11, 20, 3, 0, 0, 0, time.UTC)
	h.cycle()
	if wait := h.engine.nextCheck(); wait != time.Hour {
		t.Errorf("next check while off in %v", wait)
	}
}

func TestStateSurvivesANewContainer(t *testing.T) {
	h := newHarness(t, nil, time.Time{})
	h.registry.set("0.2.0", "0.2.1")
	h.watch(2)
	h.engine.start()
	h.cycle()
	id := h.request()["id"]
	// The deployment makes a new container with the new version.
	h.engine.cfg.Tag = "0.2.1"
	h.engine.cfg.Image = strings.Replace(h.engine.cfg.Image, ":0.2.0", ":0.2.1", 1)
	h.engine.hostname = "container-2"
	h.engine.start()
	if s := h.status(); s.Installing == nil || *s.Installed != "0.2.1" {
		t.Fatalf("after the restart: %+v", s)
	}
	h.report("result", "id="+id, "version=0.2.1", "result=installed", "message=", "finished=1795000000")
	h.engine.pollHost()
	s := h.status()
	if s.Installing != nil || s.Last.Result != "installed" || s.Last.Message != nil {
		t.Fatalf("status = %+v", s)
	}
}

func TestAPIValidates(t *testing.T) {
	h := newHarness(t, nil, time.Time{})
	h.registry.set("0.2.0")
	h.engine.start()
	handler := h.engine.Handler(context.Background())
	for _, c := range []struct {
		method, path, body string
		status             int
	}{
		{"GET", "/healthz", "", 200},
		{"GET", "/v1/status", "", 200},
		{"POST", "/v1/install", `{"version":""}`, 400},
		{"POST", "/v1/install", `not json`, 400},
		{"POST", "/v1/channel", `{"channel":"nightly"}`, 400},
		{"GET", "/v1/nothing", "", 404},
	} {
		if response := call(t, handler, c.method, c.path, c.body); response.Code != c.status {
			t.Errorf("%s %s: %d, want %d (%s)", c.method, c.path, response.Code, c.status, response.Body)
		}
	}
	var s map[string]any
	json.Unmarshal(call(t, handler, "GET", "/v1/status", "").Body.Bytes(), &s)
	for _, key := range []string{"updater", "mode", "channel", "channelLocked", "installed", "available", "attention", "installing", "last", "checking", "checkedAt", "error", "automatic", "cooldown", "window", "timezone", "apiToken"} {
		if _, ok := s[key]; !ok {
			t.Errorf("status has no %s", key)
		}
	}
	if s["apiToken"] != false {
		t.Errorf("apiToken = %v without API_TOKEN, want false", s["apiToken"])
	}
}

func TestAPITokenGuardsEveryRequestButTheHealthCheck(t *testing.T) {
	h := newHarness(t, map[string]string{"API_TOKEN": "s3cret-token"}, time.Time{})
	h.registry.set("0.2.0")
	h.engine.start()
	handler := h.engine.Handler(context.Background())
	for _, c := range []struct {
		method, path, authorization, body string
		status                            int
	}{
		{"GET", "/healthz", "", "", 200},
		{"GET", "/v1/status", "", "", 401},
		{"GET", "/v1/status", "Bearer wrong", "", 401},
		{"GET", "/v1/status", "Bearer s3cret-toke", "", 401},
		{"GET", "/v1/status", "Bearer s3cret-token2", "", 401},
		{"GET", "/v1/status", "Basic s3cret-token", "", 401},
		{"GET", "/v1/status", "s3cret-token", "", 401},
		{"POST", "/v1/check", "", "", 401},
		{"POST", "/v1/install", "", `{"version":"0.2.1"}`, 401},
		{"POST", "/v1/channel", "Bearer wrong", `{"channel":"preview"}`, 401},
		{"GET", "/v1/nothing", "", "", 401},
		{"POST", "/healthz", "", "", 401},
		{"GET", "/v1/status", "Bearer s3cret-token", "", 200},
		{"GET", "/v1/status", "bearer s3cret-token", "", 200},
		{"GET", "/v1/nothing", "Bearer s3cret-token", "", 404},
	} {
		request := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		if c.authorization != "" {
			request.Header.Set("Authorization", c.authorization)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != c.status {
			t.Errorf("%s %s with %q: %d, want %d (%s)", c.method, c.path, c.authorization, response.Code, c.status, response.Body)
		}
		if c.status == 401 {
			var answer map[string]string
			if json.Unmarshal(response.Body.Bytes(), &answer) != nil || answer["error"] == "" {
				t.Errorf("%s %s: the refusal names no error: %s", c.method, c.path, response.Body)
			}
			if response.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Errorf("%s %s: no WWW-Authenticate header", c.method, c.path)
			}
		}
	}
	// A refused request changes nothing.
	if s := h.status(); s.Channel != "stable" || s.Installing != nil {
		t.Errorf("a refused request changed the status: %+v", s)
	}
	if s := h.status(); !s.APIToken {
		t.Error("apiToken is false although API_TOKEN is set")
	}
}

// The apps trim UPDATER_TOKEN; the updater trims API_TOKEN (and the deploy
// link's token) the same way, so both sides agree on a blank or padded value.
func TestTokensAreTrimmed(t *testing.T) {
	for _, c := range []struct {
		token, send string
		apiToken    bool
	}{
		{"   ", "", false},
		{"\t\n", "", false},
		{" s3cret-token\n", "Bearer s3cret-token", true},
	} {
		h := newHarness(t, map[string]string{"API_TOKEN": c.token, "DEPLOY_HOOK_TOKEN": c.token}, time.Time{})
		h.registry.set("0.2.0")
		h.engine.start()
		if s := h.status(); s.APIToken != c.apiToken {
			t.Errorf("API_TOKEN %q: apiToken = %v, want %v", c.token, s.APIToken, c.apiToken)
		}
		if want := strings.TrimPrefix(c.send, "Bearer "); h.engine.cfg.HookToken != want {
			t.Errorf("DEPLOY_HOOK_TOKEN %q: read as %q, want %q", c.token, h.engine.cfg.HookToken, want)
		}
		request := httptest.NewRequest("GET", "/v1/status", nil)
		if c.send != "" {
			request.Header.Set("Authorization", c.send)
		}
		response := httptest.NewRecorder()
		h.engine.Handler(context.Background()).ServeHTTP(response, request)
		if response.Code != 200 {
			t.Errorf("API_TOKEN %q: the API answered %d to %q", c.token, response.Code, c.send)
		}
	}
}

func call(t *testing.T, handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// serveRequests runs the API's requests, as Run does, for tests that call
// the API without Run.
func (e *Engine) serveRequests(t *testing.T) {
	for request := range e.requests {
		request(context.Background())
	}
}

func TestHostCommands(t *testing.T) {
	trigger := t.TempDir()
	if code := hostCommand(trigger, []string{"host-watch", "app"}); code != 0 {
		t.Fatal("host-watch failed")
	}
	if fields, _ := readFields(filepath.Join(trigger, "watched")); fields["protocol"] != "2" || fields["project"] != "app" {
		t.Fatalf("watched = %v", fields)
	}
	if code := hostCommand(trigger, []string{"host-watch", "../etc"}); code == 0 {
		t.Fatal("accepted an odd project name")
	}
	// A request that is a link is dropped unread.
	secret := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(secret, []byte("version=9.9.9\n"), 0o600)
	os.Symlink(secret, filepath.Join(trigger, "deploy"))
	if code := hostCommand(trigger, []string{"host-request"}); code != 0 || exists(filepath.Join(trigger, "deploy")) {
		t.Fatal("the linked request was not dropped")
	}
	// A result replaces a link instead of writing through it.
	os.Symlink(secret, filepath.Join(trigger, "result"))
	if code := hostCommand(trigger, []string{"host-report", "result", "abc", "1.0.1", "rolled-back", "container\nweb is unhealthy"}); code != 0 {
		t.Fatal("host-report failed")
	}
	fields, _ := readFields(filepath.Join(trigger, "result"))
	if fields["result"] != "rolled-back" || fields["message"] != "container web is unhealthy" || fields["id"] != "abc" {
		t.Fatalf("result = %v", fields)
	}
	if data, _ := os.ReadFile(secret); string(data) != "version=9.9.9\n" {
		t.Fatal("wrote through the link")
	}
	if code := hostCommand(trigger, []string{"host-report", "result", "abc", "1.0.1", "installed"}); code != 0 {
		t.Fatal("host-report without a message failed")
	}
	if code := hostCommand(trigger, []string{"host-unwatch"}); code != 0 || exists(filepath.Join(trigger, "watched")) || exists(filepath.Join(trigger, "result")) {
		t.Fatal("host-unwatch left files")
	}
}

// A failed version stays failed while the container of a newer attempt runs
// and that attempt rolls back.
func TestFailuresSurviveARolledBackAttempt(t *testing.T) {
	h := newHarness(t, nil, time.Time{})
	h.registry.set("0.2.0", "0.2.1")
	h.watch(2)
	h.engine.start()
	h.cycle()
	first := h.request()["id"]
	os.Remove(filepath.Join(h.trigger, "deploy"))
	h.report("result", "id="+first, "version=0.2.1", "result=rolled-back", "message=unhealthy", "finished=1795000000")
	h.engine.pollHost()
	h.registry.set("0.2.2")
	h.cycle()
	second := h.request()["id"]
	if h.request()["version"] != "0.2.2" {
		t.Fatalf("request = %v", h.request())
	}
	os.Remove(filepath.Join(h.trigger, "deploy"))
	// The deployment recreates the updater with the version it tries.
	restart := func(tag, host string) {
		h.engine.cfg.Tag = tag
		h.engine.cfg.Image = h.engine.cfg.ImageName() + ":" + tag
		h.engine.hostname = host
		h.engine.installed = nil
		h.engine.start()
		h.cycle()
	}
	restart("0.2.2", "container-2")
	// It does not start: back to 0.2.0, reported.
	restart("0.2.0", "container-3")
	h.report("result", "id="+second, "version=0.2.2", "result=rolled-back", "message=migrate failed", "finished=1795000100")
	h.engine.pollHost()
	h.cycle()
	if request := h.request(); request != nil {
		t.Fatalf("retried a failed version: %v", request)
	}
	s := h.status()
	if s.Available == nil || s.Available.Version != "0.2.2" || s.Available.Failed == nil {
		t.Fatalf("available = %+v", s.Available)
	}
}
