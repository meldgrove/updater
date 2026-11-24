package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sigstore/sigstore-go/pkg/root"
)

// The recorded images (testdata/README.md).
const (
	handbookSource = "https://github.com/meldgrove/handbook"
	backupsSource  = "https://github.com/meldgrove/backups"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// recordedRoot is Sigstore's trusted root as recorded: the tests need no
// network.
func recordedRoot(t *testing.T) func() (root.TrustedMaterial, error) {
	data := fixture(t, "trusted_root.json")
	return func() (root.TrustedMaterial, error) { return root.NewTrustedRootFromJSON(data) }
}

func recordedSignatures(t *testing.T) *Signatures {
	s := NewSignatures(t.TempDir(), "test")
	s.load = recordedRoot(t)
	return s
}

// edit changes a recorded bundle's JSON.
func edit(t *testing.T, bundle []byte, change func(map[string]any)) []byte {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(bundle, &value); err != nil {
		t.Fatal(err)
	}
	change(value)
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func envelope(value map[string]any) map[string]any { return value["dsseEnvelope"].(map[string]any) }

func TestVerifyRecordedBundles(t *testing.T) {
	s := recordedSignatures(t)
	handbook := fixture(t, "handbook-0.2.2.bundle.json")
	handbookDigest := sha256Digest(fixture(t, "handbook-0.2.2.index.json"))
	other := sha256Digest(fixture(t, "handbook-0.2.1.index.json"))
	brokenSignature := edit(t, handbook, func(v map[string]any) {
		signature := envelope(v)["signatures"].([]any)[0].(map[string]any)
		raw, _ := base64.StdEncoding.DecodeString(signature["sig"].(string))
		raw[len(raw)-1] ^= 1
		signature["sig"] = base64.StdEncoding.EncodeToString(raw)
	})
	// The statement names another image, under the same signature.
	otherSubject := edit(t, handbook, func(v map[string]any) {
		payload, _ := base64.StdEncoding.DecodeString(envelope(v)["payload"].(string))
		changed := strings.Replace(string(payload), strings.TrimPrefix(handbookDigest, "sha256:"), strings.TrimPrefix(other, "sha256:"), 1)
		envelope(v)["payload"] = base64.StdEncoding.EncodeToString([]byte(changed))
	})
	unknownFormat := edit(t, handbook, func(v map[string]any) { v["mediaType"] = "application/vnd.dev.sigstore.bundle.v0.9+json" })

	for _, c := range []struct {
		name    string
		bundles [][]byte
		digest  string
		source  string
		problem signatureProblem // -1: verified
		reason  string
	}{
		{"handbook", [][]byte{handbook}, handbookDigest, handbookSource, -1, ""},
		{"any repository", [][]byte{handbook}, handbookDigest, "", -1, ""},
		{"a second app", [][]byte{fixture(t, "backups-0.1.4.bundle.json")}, sha256Digest(fixture(t, "backups-0.1.4.index.json")), backupsSource, -1, ""},
		{"one of two bundles", [][]byte{fixture(t, "handbook-0.2.1.bundle.json"), handbook}, handbookDigest, handbookSource, -1, ""},
		{"another image", [][]byte{handbook}, other, handbookSource, signatureInvalid, "digest"},
		{"another repository", [][]byte{handbook}, handbookDigest, backupsSource, signatureInvalid, "for meldgrove/backups"},
		{"another signer workflow", [][]byte{fixture(t, "updater-0.1.0.bundle.json")}, sha256Digest(fixture(t, "updater-0.1.0.index.json")), "https://github.com/meldgrove/updater", signatureInvalid, "expected SAN value"},
		{"a broken signature", [][]byte{brokenSignature}, handbookDigest, handbookSource, signatureInvalid, "signature"},
		{"a changed statement", [][]byte{otherSubject}, other, handbookSource, signatureInvalid, "signature"},
		{"no bundle", nil, handbookDigest, handbookSource, signatureMissing, "carries no signature"},
		{"an unknown format", [][]byte{unknownFormat}, handbookDigest, handbookSource, signatureUnchecked, "cannot read"},
		{"no JSON", [][]byte{[]byte("<html>")}, handbookDigest, handbookSource, signatureUnchecked, "cannot read"},
		{"an unknown format next to an invalid bundle", [][]byte{brokenSignature, unknownFormat}, handbookDigest, handbookSource, signatureUnchecked, "cannot read"},
	} {
		t.Run(c.name, func(t *testing.T) {
			found, err := s.Verify(c.bundles, c.digest, c.source)
			if c.problem == -1 {
				if err != nil {
					t.Fatal(err)
				}
				if found.Source == "" || !strings.HasPrefix(found.Signer, signerWorkflow+"@") {
					t.Fatalf("attestation = %+v", found)
				}
				if c.source != "" && found.Source != c.source {
					t.Fatalf("source = %s, want %s", found.Source, c.source)
				}
				return
			}
			if err == nil {
				t.Fatalf("verified: %+v", found)
			}
			if problemOf(err) != c.problem || !strings.Contains(err.Error(), c.reason) {
				t.Fatalf("problem %d, %q; want %d with %q", problemOf(err), err, c.problem, c.reason)
			}
		})
	}
}

func TestTrustedRootFailureMeansUnchecked(t *testing.T) {
	s := NewSignatures(t.TempDir(), "test")
	s.load = func() (root.TrustedMaterial, error) { return nil, errors.New("no network") }
	digest := sha256Digest(fixture(t, "handbook-0.2.2.index.json"))
	_, err := s.Verify([][]byte{fixture(t, "handbook-0.2.2.bundle.json")}, digest, handbookSource)
	if problemOf(err) != signatureUnchecked || !strings.Contains(err.Error(), "no network") {
		t.Fatalf("err = %v", err)
	}
	// Read again at the next check.
	s.load = recordedRoot(t)
	if _, err := s.Verify([][]byte{fixture(t, "handbook-0.2.2.bundle.json")}, digest, handbookSource); err != nil {
		t.Fatal(err)
	}
}

// signedHarness: a server that runs Handbook 0.2.0, signed, as recorded.
// Its registry serves the recorded indexes of the versions given, each
// with its bundle.
func signedHarness(t *testing.T, env map[string]string, versions ...string) *harness {
	h := newHarness(t, env, time.Time{})
	h.engine.signatures.load = recordedRoot(t)
	h.sign(versions...)
	return h
}

// sign publishes recorded Handbook versions with their bundles.
func (h *harness) sign(versions ...string) {
	for _, v := range versions {
		digest := h.registry.record(v, fixture(h.t, "handbook-"+v+".index.json"))
		h.registry.attach(digest, fixture(h.t, "handbook-"+v+".bundle.json"))
	}
}

func handbookDigest(t *testing.T, version string) string {
	return sha256Digest(fixture(t, "handbook-"+version+".index.json"))
}

func TestASignedInstallationChecksAndPinsTheUpdate(t *testing.T) {
	h := signedHarness(t, nil, "0.2.0", "0.2.1")
	h.watch(2)
	h.engine.start()
	h.cycle()
	request := h.request()
	if request["version"] != "0.2.1" || request["digest"] != handbookDigest(t, "0.2.1") {
		t.Fatalf("request = %v", request)
	}
	s := h.status()
	if s.Signature == nil || *s.Signature != "required" || s.Error != nil {
		t.Fatalf("status: signature %v, error %v", s.Signature, s.Error)
	}
	if h.engine.state.Signed != handbookSource {
		t.Errorf("state.signed = %q", h.engine.state.Signed)
	}
	if !h.logged("signed by Meldgrove's release workflow for meldgrove/handbook") {
		t.Error("the requirement was not logged")
	}
}

func TestAnUnsignedInstallationChecksNothing(t *testing.T) {
	h := newHarness(t, nil, time.Time{})
	h.registry.set("0.2.0", "0.2.1")
	h.watch(2)
	h.engine.start()
	h.cycle()
	request := h.request()
	if request["version"] != "0.2.1" || request["digest"] != "" {
		t.Fatalf("request = %v", request)
	}
	if s := h.status(); s.Signature == nil || *s.Signature != "not-signed" {
		t.Fatalf("signature = %v", s.Signature)
	}
	var status map[string]any
	json.Unmarshal(call(t, h.engine.Handler(context.Background()), "GET", "/v1/status", "").Body.Bytes(), &status)
	if status["signature"] != "not-signed" {
		t.Errorf("the API says signature %v", status["signature"])
	}
}

// The release workflow tags an image before it attests it: a version
// without attestation is not failed, and installs once it has one.
func TestAMissingSignatureWaitsAndIsTriedAgain(t *testing.T) {
	h := signedHarness(t, nil, "0.2.0")
	h.registry.record("0.2.1", fixture(t, "handbook-0.2.1.index.json"))
	h.watch(2)
	h.engine.start()
	h.cycle()
	s := h.status()
	if h.request() != nil || s.Error == nil || !strings.Contains(*s.Error, "0.2.1 is not installed yet: its image carries no signature from Meldgrove's release workflow") {
		t.Fatalf("request %v, error %v", h.request(), s.Error)
	}
	if s.Available == nil || s.Available.Failed != nil || len(h.engine.state.Failed) != 0 || s.Last != nil {
		t.Fatalf("the version failed: %+v, last %+v", s.Available, s.Last)
	}
	if wait := h.engine.nextCheck(); wait > time.Minute {
		t.Errorf("next check in %s", wait)
	}
	// From the app: refused with the reason, nothing changes.
	go h.engine.serveRequests(t)
	response := call(t, h.engine.Handler(context.Background()), "POST", "/v1/install", `{"version":"0.2.1"}`)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "carries no signature") {
		t.Fatalf("manual install: %d %s", response.Code, response.Body)
	}
	h.registry.attach(handbookDigest(t, "0.2.1"), fixture(t, "handbook-0.2.1.bundle.json"))
	h.cycle()
	if request := h.request(); request["version"] != "0.2.1" || request["digest"] != handbookDigest(t, "0.2.1") {
		t.Fatalf("request after the attestation = %v", request)
	}
	if s := h.status(); s.Error != nil {
		t.Errorf("error = %s", *s.Error)
	}
}

func TestASignatureForAnotherRepositoryFailsTheVersion(t *testing.T) {
	h := signedHarness(t, nil, "0.2.0")
	// 0.2.1 is the signed image of another app.
	digest := h.registry.record("0.2.1", fixture(t, "backups-0.1.4.index.json"))
	h.registry.attach(digest, fixture(t, "backups-0.1.4.bundle.json"))
	h.watch(2)
	h.engine.start()
	h.cycle()
	s := h.status()
	if h.request() != nil || s.Last == nil || s.Last.Result != "failed" || s.Available == nil || s.Available.Failed == nil {
		t.Fatalf("request %v, last %+v", h.request(), s.Last)
	}
	if !strings.HasPrefix(*s.Last.Message, "0.2.1 was not installed: its signature is not from Meldgrove's release workflow for meldgrove/handbook") ||
		!strings.HasSuffix(*s.Last.Message, "Ask the maintainers.") {
		t.Fatalf("message = %s", *s.Last.Message)
	}
	h.cycle()
	if h.request() != nil {
		t.Fatal("tried the refused version again")
	}
	// A newer, signed version installs.
	h.sign("0.2.2")
	h.cycle()
	if request := h.request(); request["version"] != "0.2.2" || request["digest"] != handbookDigest(t, "0.2.2") {
		t.Fatalf("request = %v", request)
	}
}

// The signature covers the image, not its tag: the tag of a new version that
// points to a signed older image is refused.
func TestTheSignedImageMustNameTheVersion(t *testing.T) {
	h := signedHarness(t, nil, "0.2.0", "0.2.1")
	h.registry.record("0.2.2", fixture(t, "handbook-0.2.1.index.json"))
	h.watch(2)
	h.engine.start()
	h.cycle()
	s := h.status()
	if h.request() != nil || s.Last == nil || s.Last.Result != "failed" || s.Last.Version != "0.2.2" ||
		*s.Last.Message != "0.2.2 was not installed: its tag points to the signed image of 0.2.1. Ask the maintainers." {
		t.Fatalf("request %v, last %+v", h.request(), s.Last)
	}
}

func TestALookupFailureInstallsNothingAndIsTriedAgain(t *testing.T) {
	h := signedHarness(t, nil, "0.2.0")
	h.watch(2)
	h.engine.start()
	h.cycle() // the installed image is signed
	h.sign("0.2.1")
	h.registry.fail("/manifests/sha256-", http.StatusInternalServerError)
	h.cycle()
	s := h.status()
	if h.request() != nil || s.Error == nil || !strings.Contains(*s.Error, "0.2.1 is not installed yet: its signature cannot be checked") ||
		len(h.engine.state.Failed) != 0 {
		t.Fatalf("request %v, error %v, failed %v", h.request(), s.Error, h.engine.state.Failed)
	}
	h.registry.fail("/manifests/sha256-", 0)
	h.cycle()
	if request := h.request(); request["version"] != "0.2.1" {
		t.Fatalf("request = %v", request)
	}
}

func TestATrustedRootThatCannotBeReadInstallsNothing(t *testing.T) {
	h := signedHarness(t, nil, "0.2.0", "0.2.1")
	h.engine.signatures.load = func() (root.TrustedMaterial, error) {
		return nil, errors.New("tuf-repo-cdn.sigstore.dev does not answer")
	}
	h.watch(2)
	h.engine.start()
	h.cycle()
	s := h.status()
	if h.request() != nil || s.Signature != nil || s.Error == nil || !strings.Contains(*s.Error, "cannot tell whether it must be signed") {
		t.Fatalf("request %v, signature %v, error %v", h.request(), s.Signature, s.Error)
	}
	h.engine.signatures.load = recordedRoot(t)
	h.cycle()
	if request := h.request(); request["digest"] != handbookDigest(t, "0.2.1") {
		t.Fatalf("request = %v", request)
	}
}

// Once an installed image was signed, an attestation removed later does not
// switch the check off, also not in a new container.
func TestTheRequirementStays(t *testing.T) {
	h := signedHarness(t, nil, "0.2.0")
	h.watch(2)
	h.engine.start()
	h.cycle()
	h.registry.detach(handbookDigest(t, "0.2.0"))
	h.registry.set("0.2.1") // unsigned
	again := NewEngine(h.engine.cfg, h.engine.logf)
	again.now, again.hostname = h.engine.now, "container-2"
	again.signatures.load = recordedRoot(t)
	again.start()
	again.check(context.Background())
	again.autoInstall(context.Background())
	s := again.Status()
	if h.request() != nil || s.Signature == nil || *s.Signature != "required" || s.Error == nil || !strings.Contains(*s.Error, "carries no signature") {
		t.Fatalf("request %v, signature %v, error %v", h.request(), s.Signature, s.Error)
	}
}

// The server's operator may install a version without the check, once.
func TestTheOperatorMayInstallWithoutTheCheck(t *testing.T) {
	h := signedHarness(t, nil, "0.2.0")
	h.registry.set("0.2.1") // unsigned
	h.watch(2)
	h.engine.start()
	h.cycle()
	if h.request() != nil {
		t.Fatal("installed an unsigned version")
	}
	if code := allowUnsigned(h.trigger, "0.2.1"); code != 0 {
		t.Fatalf("allowUnsigned = %d", code)
	}
	h.cycle()
	if request := h.request(); request["version"] != "0.2.1" || request["digest"] != "" {
		t.Fatalf("request = %v", request)
	}
	if _, err := os.Stat(filepath.Join(h.trigger, "allow-unsigned")); err == nil {
		t.Error("the permission stays after its use")
	}
	if !h.logged("without checking its signature: the server's operator allowed it") {
		t.Error("not logged")
	}
	if code := allowUnsigned(h.trigger, "0.2.1\nversion=0.2.2"); code == 0 {
		t.Error("allowUnsigned took a version with a line break")
	}
}

func TestAPlatformChecksTheImageOfTheTag(t *testing.T) {
	calls := 0
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	defer hook.Close()
	h := signedHarness(t, map[string]string{"IMAGE": "REGISTRY/meldgrove/app:latest", "DEPLOY_HOOK_URL": hook.URL}, "0.2.0", "0.2.1")
	point := func(version string) { h.registry.record("latest", fixture(t, "handbook-"+version+".index.json")) }
	point("0.2.0")
	h.engine.start()
	h.cycle()
	if s := h.status(); s.Signature == nil || *s.Signature != "required" {
		t.Fatalf("signature = %v", s.Signature)
	}
	point("0.2.1")
	h.cycle()
	if calls != 1 {
		t.Fatalf("deployments = %d, status %+v", calls, h.status())
	}
	// An own build under the tag: no deployment.
	h.registry.recorded["latest"] = indexOf("0.2.5")
	h.cycle()
	if s := h.status(); calls != 1 || s.Error == nil || !strings.Contains(*s.Error, "carries no signature") {
		t.Fatalf("deployments = %d, error %v", calls, s.Error)
	}
	// A signed older image under the tag: refused.
	point("0.2.0")
	h.cycle()
	if s := h.status(); calls != 1 || s.Last == nil || s.Last.Result != "failed" || !strings.Contains(*s.Last.Message, "not newer than the installed 0.2.1") {
		t.Fatalf("deployments = %d, last %+v", calls, s.Last)
	}
}

func TestTheRegistryChecksDigests(t *testing.T) {
	h := newHarness(t, nil, time.Time{})
	digest := h.registry.record("0.2.1", fixture(t, "handbook-0.2.1.index.json"))
	if info, err := h.engine.registry.Info(context.Background(), digest); err != nil || info.Version != "0.2.1" {
		t.Fatalf("info = %+v, %v", info, err)
	}
	h.registry.content[digest] = fixture(t, "handbook-0.2.2.index.json")
	if _, err := h.engine.registry.Info(context.Background(), digest); err == nil || !strings.Contains(err.Error(), "other content") {
		t.Fatalf("err = %v", err)
	}
}

// A registry with the referrers API (OCI 1.1) answers there.
func TestTheReferrersAPI(t *testing.T) {
	bundle := fixture(t, "handbook-0.2.2.bundle.json")
	image := handbookDigest(t, "0.2.2")
	manifest := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","artifactType":"application/vnd.dev.sigstore.bundle.v0.3+json","layers":[{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json","digest":"%s"}],"subject":{"digest":"%s"}}`, sha256Digest(bundle), image))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/meldgrove/app/referrers/" + image:
			fmt.Fprintf(w, `{"schemaVersion":2,"manifests":[{"artifactType":"application/vnd.dev.sigstore.bundle.v0.3+json","digest":"%s"},{"artifactType":"application/spdx+json","digest":"sha256:%064d"}]}`, sha256Digest(manifest), 0)
		case "/v2/meldgrove/app/manifests/" + sha256Digest(manifest):
			w.Write(manifest)
		case "/v2/meldgrove/app/blobs/" + sha256Digest(bundle):
			w.Write(bundle)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	cfg, err := LoadConfig(func(name string) string {
		return map[string]string{"IMAGE": strings.TrimPrefix(server.URL, "http://") + "/meldgrove/app:0.2.2"}[name]
	})
	if err != nil {
		t.Fatal(err)
	}
	bundles, err := NewRegistry(cfg).Attestations(context.Background(), image)
	if err != nil || len(bundles) != 1 || string(bundles[0]) != string(bundle) {
		t.Fatalf("bundles = %d, %v", len(bundles), err)
	}
}
