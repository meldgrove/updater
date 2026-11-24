package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/fulcio/certificate"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	"github.com/sigstore/sigstore-go/pkg/verify"
	"github.com/theupdateframework/go-tuf/v2/metadata/fetcher"
)

// The signer of Meldgrove's images (README.md, "Signatures"): the release
// workflow that every Meldgrove repository shares (meldgrove/.github). For
// each image of a public repository it signs a SLSA provenance attestation
// with actions/attest, in GitHub Actions, through Sigstore's public-good
// instance (a Fulcio certificate and a Rekor entry). The certificate names
// the workflow (any ref) and, as an extension, the app's repository. The
// release workflow checks its own images the same way:
// gh attestation verify --repo meldgrove/<app> --signer-repo meldgrove/.github.
const (
	signerIssuer   = "https://token.actions.githubusercontent.com"
	signerWorkflow = "https://github.com/meldgrove/.github/.github/workflows/release.yml"
	provenanceType = "https://slsa.dev/provenance/v1"
	// bundleType starts the media types of Sigstore bundles, such as
	// application/vnd.dev.sigstore.bundle.v0.3+json.
	bundleType = "application/vnd.dev.sigstore.bundle"
)

var signerPattern = "^" + regexp.QuoteMeta(signerWorkflow) + "@"

// attestation is what a verified bundle says about an image.
type attestation struct {
	Source string // the repository it was built from, e.g. https://github.com/meldgrove/handbook
	Signer string // the workflow that signed it, with its ref
}

// repository is Source without https://github.com/, e.g. meldgrove/handbook.
func repository(source string) string { return strings.TrimPrefix(source, "https://github.com/") }

// signatureProblem says why an image's signature did not pass.
type signatureProblem int

const (
	// signatureUnchecked: the check could not run (registry, TUF, a bundle
	// format this updater does not know). Nothing installs; the next check
	// tries again.
	signatureUnchecked signatureProblem = iota
	// signatureMissing: the image carries no bundle. The release workflow
	// tags an image before it attests it, so a new version has none for up
	// to a minute: the next check looks again.
	signatureMissing
	// signatureInvalid: bundles, but none from the release workflow for the
	// app's repository, or one that does not verify. The version fails.
	signatureInvalid
)

type signatureError struct {
	problem signatureProblem
	reason  string
}

func (e *signatureError) Error() string { return e.reason }

// problemOf: the problem of a signature error; any other error means the
// check could not run.
func problemOf(err error) signatureProblem {
	var s *signatureError
	if errors.As(err, &s) {
		return s.problem
	}
	return signatureUnchecked
}

// Signatures verifies Sigstore bundles with sigstore-go against the trusted
// root of Sigstore's public-good instance, which it reads through TUF from
// the root of trust that sigstore-go embeds. The TUF metadata is cached in
// a directory and refreshed once a day.
type Signatures struct {
	dirs  []string // where the TUF cache may go, first writable wins
	agent string

	mu       sync.Mutex
	material root.TrustedMaterial
	loadedAt time.Time
	// load reads the trusted root; tests replace it.
	load func() (root.TrustedMaterial, error)
	now  func() time.Time
}

func NewSignatures(trigger, version string) *Signatures {
	s := &Signatures{
		dirs:  []string{filepath.Join(trigger, "sigstore"), filepath.Join(os.TempDir(), "meldgrove-updater-sigstore")},
		agent: "meldgrove-updater/" + orDefault(version, "development"),
		now:   time.Now,
	}
	s.load = s.fetch
	return s
}

// cacheDir is the first directory of dirs that the updater can write to.
func (s *Signatures) cacheDir() (string, error) {
	var last error
	for _, dir := range s.dirs {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			last = err
			continue
		}
		probe, err := os.CreateTemp(dir, ".probe-*")
		if err != nil {
			last = err
			continue
		}
		probe.Close()
		os.Remove(probe.Name())
		return dir, nil
	}
	return "", fmt.Errorf("no writable directory for Sigstore's TUF cache: %v", last)
}

// fetch reads the trusted root through TUF. The metadata in the cache counts
// for a day; then TUF asks tuf-repo-cdn.sigstore.dev again. When that does
// not answer, the cache serves until its metadata expires (Sigstore's
// timestamp lasts about a week), as TUF allows.
func (s *Signatures) fetch() (root.TrustedMaterial, error) {
	dir, err := s.cacheDir()
	if err != nil {
		return nil, err
	}
	options := func() *tuf.Options {
		get := fetcher.NewDefaultFetcher()
		get.SetHTTPClient(&http.Client{Timeout: 30 * time.Second})
		get.SetHTTPUserAgent(s.agent)
		return tuf.DefaultOptions().WithCachePath(dir).WithCacheValidity(1).WithFetcher(get)
	}
	client, err := tuf.New(options())
	if err == nil {
		trusted, rootErr := root.GetTrustedRoot(client)
		if rootErr == nil {
			return trusted, nil
		}
		err = rootErr
	}
	if cached, cacheErr := tuf.New(options().WithForceCache()); cacheErr == nil {
		if trusted, rootErr := root.GetTrustedRoot(cached); rootErr == nil {
			return trusted, nil
		}
	}
	return nil, err
}

// trusted is the trusted root, read again every hour: from the cache within
// a day, then through TUF.
func (s *Signatures) trusted() (root.TrustedMaterial, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.material != nil && s.now().Sub(s.loadedAt) < time.Hour {
		return s.material, nil
	}
	material, err := s.load()
	if err != nil {
		return nil, &signatureError{signatureUnchecked, fmt.Sprintf("Sigstore's trusted root cannot be read (TUF, tuf-repo-cdn.sigstore.dev): %v", err)}
	}
	s.material, s.loadedAt = material, s.now()
	return material, nil
}

// Verify checks an image's bundles: one of them must be SLSA provenance for
// the digest, signed by the release workflow, for the repository source (a
// URL such as https://github.com/meldgrove/handbook; empty: any
// repository).
func (s *Signatures) Verify(bundles [][]byte, digest, source string) (attestation, error) {
	if len(bundles) == 0 {
		return attestation{}, &signatureError{signatureMissing, "its image carries no signature from Meldgrove's release workflow"}
	}
	hexDigest, ok := strings.CutPrefix(digest, "sha256:")
	sum, err := hex.DecodeString(hexDigest)
	if !ok || err != nil || len(sum) != 32 {
		return attestation{}, fmt.Errorf("%s is no image digest", digest)
	}
	trusted, err := s.trusted()
	if err != nil {
		return attestation{}, err
	}
	// As gh attestation verify for Sigstore's public-good instance.
	verifier, err := verify.NewVerifier(trusted, verify.WithSignedCertificateTimestamps(1), verify.WithTransparencyLog(1),
		verify.WithObserverTimestamps(1), verify.WithoutStatementPredicate())
	if err != nil {
		return attestation{}, fmt.Errorf("the signature check cannot start: %v", err)
	}
	signer, err := verify.NewSANMatcher("", signerPattern)
	if err != nil {
		return attestation{}, err
	}
	issuer, err := verify.NewIssuerMatcher(signerIssuer, "")
	if err != nil {
		return attestation{}, err
	}
	identity, err := verify.NewCertificateIdentity(signer, issuer, certificate.Extensions{SourceRepositoryURI: source})
	if err != nil {
		return attestation{}, err
	}
	policy := verify.NewPolicy(verify.WithArtifactDigest("sha256", sum), verify.WithCertificateIdentity(identity))
	var invalid, unknown error
	for _, data := range bundles {
		var b bundle.Bundle
		if err := b.UnmarshalJSON(data); err != nil {
			unknown = err
			continue
		}
		result, err := verifier.Verify(&b, policy)
		switch {
		case err != nil:
			invalid = err
		case result.Statement == nil || result.Statement.PredicateType != provenanceType:
			invalid = errors.New("it attests no SLSA provenance")
		case result.Signature == nil || result.Signature.Certificate == nil:
			invalid = errors.New("it has no certificate")
		default:
			certificate := result.Signature.Certificate
			return attestation{Source: certificate.SourceRepositoryURI, Signer: certificate.SubjectAlternativeName}, nil
		}
	}
	// A bundle of an unknown format may be valid for a newer updater: never
	// install, never fail the version for good.
	if unknown != nil {
		return attestation{}, &signatureError{signatureUnchecked, fmt.Sprintf("its signature has a format that this updater cannot read (%v); a newer updater may", unknown)}
	}
	reason := "its signature is not from Meldgrove's release workflow"
	if source != "" {
		reason += " for " + repository(source)
	}
	return attestation{}, &signatureError{signatureInvalid, fmt.Sprintf("%s (%v)", reason, invalid)}
}

// verdict is the answer for a digest that never changes: verified, or
// invalid.
type verdict struct {
	attestation attestation
	err         error
}

// reported is an error that the updater has logged and shows in its
// status already.
type reported struct{ error }

// verified checks the signature of the image at digest for the repository
// source; verified and invalid answers are kept.
func (e *Engine) verified(ctx context.Context, digest, source string) (attestation, error) {
	key := digest + " " + source
	e.mu.Lock()
	known, ok := e.verdicts[key]
	e.mu.Unlock()
	if ok {
		return known.attestation, known.err
	}
	bundles, err := e.registry.Attestations(ctx, digest)
	if err != nil {
		return attestation{}, err
	}
	found, err := e.signatures.Verify(bundles, digest, source)
	if err == nil && found.Source == "" {
		err = &signatureError{signatureInvalid, "its signature names no repository"}
	}
	if err == nil || problemOf(err) == signatureInvalid {
		e.mu.Lock()
		e.verdicts[key] = verdict{found, err}
		e.mu.Unlock()
	}
	return found, err
}

// signedBy is the repository whose release workflow must sign every update
// (README.md, "Signatures"): the one that signed the installed image. Empty
// when the installed image carries no attestation of Meldgrove's release
// workflow (an image of a private repository, an own build, an older
// release): then nothing is checked. Once an installed image was signed, the
// state keeps the repository, so an attestation removed later does not
// switch the check off. A "no" is asked again each time: the attestation of
// an image that was just published may still come.
func (e *Engine) signedBy(ctx context.Context) (string, error) {
	pinned := e.pinned()
	e.mu.Lock()
	signed := e.state.Signed
	// The image that IMAGE names on a server (usually the installed version's
	// tag), or the image the followed tag pointed to when it was installed.
	reference := e.state.InstalledDigest
	if pinned {
		reference = e.cfg.Tag
	}
	e.mu.Unlock()
	switch {
	case signed != "":
		return signed, nil
	case reference == "":
		return "", errors.New("the installed image is not known yet")
	}
	digest := reference
	if !digestPattern.MatchString(reference) {
		found, err := e.registry.Digest(ctx, reference)
		if err != nil {
			return "", err
		}
		digest = found
	}
	found, err := e.verified(ctx, digest, "")
	if err != nil && problemOf(err) == signatureUnchecked {
		return "", err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err != nil {
		e.once("not signed "+digest, 24*time.Hour, "The installed image carries no signature of Meldgrove's release workflow, so updates install without a check of theirs.")
		return "", nil
	}
	e.state.Signed = found.Source
	e.save()
	e.logf("The installed image is signed by Meldgrove's release workflow for %s: every update must be signed for it as well.", repository(found.Source))
	return found.Source, nil
}

// updateSignature tells the status whether updates must be signed. When
// that cannot be told, the check fails: nothing installs, and the next
// check comes within a minute.
func (e *Engine) updateSignature(ctx context.Context) error {
	source, err := e.signedBy(ctx)
	e.mu.Lock()
	defer e.mu.Unlock()
	switch {
	case err != nil:
		e.signature = ""
		return fmt.Errorf("Cannot tell whether updates must be signed: %s.", sentence(err))
	case source != "":
		e.signature = "required"
	default:
		e.signature = "not-signed"
	}
	return nil
}

// checkSignature runs before an installation. When the installed image is
// signed, the update's image must carry a valid attestation of the release
// workflow for the same repository, and name the version that is installed
// (the signature covers the image, not its tag). It answers the digest that
// was checked, for the server to pin; empty when nothing was checked.
//
// A refusal fails the version, as a failed installation does. When the
// check cannot run, or the image carries no attestation yet, nothing
// installs and the next check tries again.
func (e *Engine) checkSignature(ctx context.Context, o *offer) (string, error) {
	// The operator's way out, also when the check cannot run.
	if e.unsignedAllowed(o.Version) {
		return "", nil
	}
	source, err := e.signedBy(ctx)
	if err != nil {
		return "", e.retry(fmt.Sprintf("%s is not installed yet: the updater cannot tell whether it must be signed: %s. It tries again at the next check.", o.Version, sentence(err)))
	}
	if source == "" {
		return "", nil
	}
	pinned := e.pinned()
	e.mu.Lock()
	installed := e.installed
	e.mu.Unlock()
	if !pinned && installed == nil {
		// Without it, an older signed image under the tag would pass.
		return "", e.retry(fmt.Sprintf("%s is not installed yet: the installed version is not known yet. The updater tries again at the next check.", o.Version))
	}
	digest := o.digest
	if pinned {
		// The version's tag, resolved once: the server pins this digest.
		if digest, err = e.registry.Digest(ctx, o.Version); err != nil {
			return "", e.retry(fmt.Sprintf("%s is not installed yet: %s The updater tries again at the next check.", o.Version, err))
		}
	}
	if _, err := e.verified(ctx, digest, source); err != nil {
		switch problemOf(err) {
		case signatureInvalid:
			return "", e.refuse(o, fmt.Sprintf("%s was not installed: %s. Ask the maintainers.", o.Version, sentence(err)))
		case signatureMissing:
			return "", e.retry(fmt.Sprintf("%s is not installed yet: %s. The release workflow signs an image right after it publishes it, so the updater looks again at the next check. If this stays, ask the maintainers.", o.Version, sentence(err)))
		default:
			return "", e.retry(fmt.Sprintf("%s is not installed yet: its signature cannot be checked: %s. The updater tries again at the next check.", o.Version, sentence(err)))
		}
	}
	info, err := e.registry.Info(ctx, digest)
	if err != nil {
		return "", e.retry(fmt.Sprintf("%s is not installed yet: %s The updater tries again at the next check.", o.Version, err))
	}
	// A signed image of another version under this tag, such as an older
	// release with a known bug, is refused.
	if pinned && info.Version != o.Version {
		return "", e.refuse(o, fmt.Sprintf("%s was not installed: its tag points to the signed image of %s. Ask the maintainers.", o.Version, orDefault(info.Version, "no version")))
	}
	if !pinned {
		v, ok := ParseVersion(info.Version)
		if !ok {
			return "", e.refuse(o, fmt.Sprintf("%s was not installed: the signed image that %s points to names no version. Ask the maintainers.", o.Version, e.cfg.Tag))
		}
		if v.Compare(*installed) <= 0 {
			return "", e.refuse(o, fmt.Sprintf("%s was not installed: %s points to the signed image of %s, which is not newer than the installed %s. Ask the maintainers.", o.Version, e.cfg.Tag, v.Text, installed.Text))
		}
		// The platform or server pulls the tag itself: it must still point to
		// the image just checked.
		if now, err := e.registry.Digest(ctx, e.cfg.Tag); err != nil {
			return "", e.retry(fmt.Sprintf("%s is not installed yet: %s The updater tries again at the next check.", o.Version, err))
		} else if now != digest {
			return "", e.retry(fmt.Sprintf("%s is not installed yet: the tag %s points to another image since the last check. The updater checks that one at the next check.", o.Version, e.cfg.Tag))
		}
	}
	return digest, nil
}

// sentence is an error's message without its final period, to go inside a
// sentence.
func sentence(err error) string { return strings.TrimRight(err.Error(), ". ") }

// refuse fails a version whose signature does not pass, as a failed
// installation: it does not install again automatically; a newer version
// does.
func (e *Engine) refuse(o *offer, message string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.logf("%s", message)
	e.finish(Last{Version: o.Version, Result: "failed", Message: message, FinishedAt: e.now()})
	return reported{errors.New(message)}
}

// retry shows why nothing installs now in the status; the next check (within
// a minute, as after a failed check) tries again.
func (e *Engine) retry(message string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.checkErr = message
	e.once("retry "+message, time.Hour, "%s", message)
	return reported{errors.New(message)}
}

// unsignedAllowed is the way out for the server's operator: updater install
// --unsigned VERSION, in the container, writes the version to the file
// allow-unsigned in the volume, which the API cannot write. The next
// installation of that version then goes without the check, once.
func (e *Engine) unsignedAllowed(version string) bool {
	path := e.path("allow-unsigned")
	fields, ok := readFields(path)
	if !ok || fields["version"] != version || os.Remove(path) != nil {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.logf("Installing %s without checking its signature: the server's operator allowed it (updater install --unsigned).", version)
	return true
}

// verifyImage serves updater verify-image IMAGE[:TAG|@DIGEST] [--repo
// OWNER/NAME]: the check of an update, for one image, with one line as the
// answer. With --repo, the image must be built from that repository;
// without, the answer names the repository. A tag that is a version must
// point to the signed image of that version. Exit 0: signed; 1: not
// signed, not valid, or the check could not run; 2: usage.
func verifyImage(args []string, getenv func(string) string, out io.Writer) int {
	var image, repo string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--repo" && i+1 < len(args):
			repo = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--repo="):
			repo = strings.TrimPrefix(args[i], "--repo=")
		case image == "" && !strings.HasPrefix(args[i], "-"):
			image = args[i]
		default:
			fmt.Fprintln(os.Stderr, usage)
			return 2
		}
	}
	name, digest, pinned := strings.Cut(image, "@")
	repo = strings.Trim(strings.TrimPrefix(repo, "https://github.com/"), "/")
	if image == "" || (pinned && !digestPattern.MatchString(digest)) || (repo != "" && strings.Count(repo, "/") != 1) {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	cfg := Config{Image: name, RegistryUsername: getenv("REGISTRY_USERNAME"), RegistryPassword: getenv("REGISTRY_PASSWORD")}
	if err := cfg.parseImage(); err != nil {
		fmt.Fprintf(out, "cannot check: %s\n", sentence(err))
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	registry := NewRegistry(cfg)
	answer := func(format string, values ...any) int {
		fmt.Fprintf(out, format+"\n", values...)
		return 1
	}
	if !pinned {
		var err error
		if digest, err = registry.Digest(ctx, cfg.Tag); err != nil {
			return answer("cannot check: %s", sentence(err))
		}
	}
	source := ""
	if repo != "" {
		source = "https://github.com/" + repo
	}
	bundles, err := registry.Attestations(ctx, digest)
	if err != nil {
		return answer("cannot check: %s (%s): %s", image, digest, sentence(err))
	}
	found, err := NewSignatures(orDefault(getenv("TRIGGER_DIR"), "/trigger"), getenv("APP_VERSION")).Verify(bundles, digest, source)
	if err != nil {
		switch problemOf(err) {
		case signatureMissing:
			return answer("not signed: %s (%s) carries no signature from Meldgrove's release workflow.", image, digest)
		case signatureInvalid:
			return answer("not valid: %s (%s): %s.", image, digest, sentence(err))
		default:
			return answer("cannot check: %s (%s): %s.", image, digest, sentence(err))
		}
	}
	info, err := registry.Info(ctx, digest)
	if err != nil {
		return answer("cannot check: %s (%s): %s", image, digest, sentence(err))
	}
	if _, isVersion := ParseVersion(cfg.Tag); !pinned && isVersion && info.Version != cfg.Tag {
		return answer("not valid: %s (%s) is the signed image of %s, not of %s.", image, digest, orDefault(info.Version, "no version"), cfg.Tag)
	}
	fmt.Fprintf(out, "signed: %s (%s) is %s, signed by Meldgrove's release workflow for %s (%s).\n",
		image, digest, orDefault(info.Version, "an image without version"), repository(found.Source), found.Signer)
	return 0
}
