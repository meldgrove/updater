package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Engine decides what to install and when. One goroutine (Run) does all
// checks and installations; the API hands its requests to it.
type Engine struct {
	cfg      Config
	registry *Registry
	github   *GitHub
	now      func() time.Time
	hostname string
	logf     func(format string, args ...any)
	hook     *http.Client

	requests chan func(context.Context)

	mu           sync.Mutex
	state        State
	installed    *Version // pinned: the version that runs
	available    *offer
	attention    *offer
	checking     bool
	checkedAt    time.Time
	checkErr     string
	versionsSeen []Version // the registry's versions at the last check, newest first
	wakeAt       time.Time // a ready update waits for the window to open
	infos        map[string]ImageInfo
	logged       map[string]time.Time
	lastMode     string
}

// offer is an update the app can show.
type offer struct {
	Update
	version  *Version   // nil for an image without a version
	digest   string     // following a tag: the image it points to
	released *time.Time // the GitHub release date, which the cooldown counts from
}

// Update is an update as the API shows it (docs/updates.md, "HTTP API").
type Update struct {
	Version     string     `json:"version"`
	Preview     bool       `json:"preview"`
	PublishedAt *time.Time `json:"publishedAt"`
	URL         *string    `json:"url"`
	Notes       []string   `json:"notes"`
	Changes     int        `json:"changes"`
	InstallsAt  *time.Time `json:"installsAt"`
	Failed      *string    `json:"failed"`
}

func NewEngine(cfg Config, logf func(string, ...any)) *Engine {
	hostname, _ := os.Hostname()
	return &Engine{
		cfg:      cfg,
		registry: NewRegistry(cfg),
		github:   NewGitHub(cfg),
		now:      time.Now,
		hostname: hostname,
		logf:     logf,
		// The deploy link is called as given: no redirects, which could carry
		// the token elsewhere.
		hook: &http.Client{Timeout: 60 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
		requests: make(chan func(context.Context)),
		infos:    map[string]ImageInfo{},
		logged:   map[string]time.Time{},
	}
}

func (e *Engine) path(name string) string { return filepath.Join(e.cfg.Trigger, name) }

// host tells whether a host setup watches the volume, its protocol, and the
// Docker Compose project it serves.
func (e *Engine) host() (watched bool, protocol int, project string) {
	fields, ok := readFields(e.path("watched"))
	if !ok {
		return false, 0, ""
	}
	if fields["protocol"] == "2" {
		return true, 2, fields["project"]
	}
	return true, 1, fields["project"]
}

// mode: host, platform or off (docs: "Modes").
func (e *Engine) mode() string {
	if e.cfg.HookURL != "" {
		return "platform"
	}
	if watched, _, _ := e.host(); watched {
		return "host"
	}
	return "off"
}

// pinned tells whether the updater chooses versions by channel and the host
// pins them, or follows the image tag (a platform, an older host setup, or no
// VERSION_VARIABLE).
func (e *Engine) pinned() bool {
	if e.cfg.HookURL != "" || e.cfg.VersionVariable == "" {
		return false
	}
	watched, protocol, _ := e.host()
	return !watched || protocol == 2
}

func (e *Engine) channelLocked() string {
	switch {
	case e.pinned():
		return ""
	case e.cfg.HookURL != "":
		return fmt.Sprintf("The image tag %s set where the app is deployed decides which updates install (the tag preview gives previews). Change it there.", e.cfg.Tag)
	case e.cfg.VersionVariable == "":
		return fmt.Sprintf("compose.yaml sets no VERSION_VARIABLE for the updater, so the image tag %s decides which updates install.", e.cfg.Tag)
	default:
		return "This server's update setup is from an older updater. Run the host setup again to choose the channel here (deployment guide)."
	}
}

func (e *Engine) save() error {
	err := e.state.save(e.path("state.json"))
	if err != nil {
		e.once("save", 24*time.Hour, "Cannot keep the state in %s: %v", e.cfg.Trigger, err)
	}
	return err
}

// once logs a message at most once per period for its key.
func (e *Engine) once(key string, period time.Duration, format string, args ...any) {
	if at, ok := e.logged[key]; ok && e.now().Sub(at) < period {
		return
	}
	e.logged[key] = e.now()
	e.logf(format, args...)
}

func (e *Engine) projectHint() string {
	_, _, project := e.host()
	if project == "" {
		project = "<project>"
	}
	return "meldgrove-updater-" + project
}

// start reads the state and what is installed. Called once by Run.
func (e *Engine) start() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.state = loadState(e.path("state.json"))
	if e.state.Image != "" && imageName(e.state.Image) != e.cfg.ImageName() {
		// Another image (a renamed app): what the updater knew is about that one.
		e.state = State{Format: stateFormat, Channel: e.state.Channel, Seen: map[string]int64{}, Failed: map[string]string{}}
	}
	fresh := e.state.Container != e.hostname || e.state.Image != e.cfg.Image
	e.state.Container, e.state.Image = e.hostname, e.cfg.Image
	e.lastMode = e.mode()
	if e.state.Channel == "" {
		e.state.Channel = "stable"
		if v, ok := ParseVersion(e.cfg.Tag); e.cfg.Tag == "preview" || (ok && v.IsPreview()) {
			e.state.Channel = "preview"
		}
	}
	if v, ok := ParseVersion(e.cfg.Tag); ok {
		e.installed = &v
		e.state.Installed = v.Text
	} else if fresh {
		// A deployment has just pulled what the tag points to now.
		e.state.Installed, e.state.InstalledDigest = "", ""
	} else if v, ok := ParseVersion(e.state.Installed); ok {
		e.installed = &v
	}
	if !fresh && e.state.InstalledDigest != "" {
		e.logf("Current image: %s, as before the restart.", e.state.InstalledDigest)
	}
	if e.state.Install != nil && e.state.Install.Legacy && fresh {
		// An older host setup recreated this container: it installed.
		e.finish(Last{Version: e.state.Install.Version, Result: "accepted", FinishedAt: e.now()})
	}
	e.save()
}

func imageName(image string) string {
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		return image[:i]
	}
	return image
}

// Run checks once at start and then every interval, and serves requests.
func (e *Engine) Run(ctx context.Context) {
	e.start()
	poll := time.NewTicker(2 * time.Second)
	defer poll.Stop()
	timer := time.NewTimer(0)
	defer timer.Stop()
	next := e.now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			e.check(ctx)
			e.autoInstall(ctx)
			wait := e.nextCheck()
			next = e.now().Add(wait)
			timer.Reset(wait)
		case <-poll.C:
			e.pollHost()
		case request := <-e.requests:
			request(ctx)
			// A check may bring an update due sooner; never later than planned.
			if wait := e.nextCheck(); e.now().Add(wait).Before(next) {
				next = e.now().Add(wait)
				timer.Reset(wait)
			}
		}
	}
}

// nextCheck is the interval, or less when an update becomes due sooner or the
// last check failed.
func (e *Engine) nextCheck() time.Duration {
	e.mu.Lock()
	defer e.mu.Unlock()
	wait := e.cfg.Interval
	if e.checkErr != "" && wait > time.Minute {
		wait = time.Minute
	}
	if e.available != nil && e.available.InstallsAt != nil && e.mode() != "off" {
		if until := e.available.InstallsAt.Sub(e.now()); until > 0 && until < wait {
			wait = until + time.Second
		}
	}
	// While off, nothing clears a wake-up that has passed.
	if !e.wakeAt.IsZero() && e.mode() != "off" {
		if until := e.wakeAt.Sub(e.now()); until < wait {
			wait = until + time.Second
		}
	}
	if wait < time.Second {
		wait = time.Second
	}
	return wait
}

// do hands a request to the Run goroutine and waits for its result, at
// most timeout. accepted is false when Run did not take the request in time
// (nothing happened); done is false when it still runs.
func do[T any](e *Engine, ctx context.Context, timeout time.Duration, request func(context.Context) T) (result T, accepted, done bool) {
	results := make(chan T, 1) // buffered: a late result never blocks Run
	wait, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	select {
	case e.requests <- func(ctx context.Context) { results <- request(ctx) }:
	case <-wait.Done():
		return result, false, false
	}
	select {
	case result = <-results:
		return result, true, true
	case <-wait.Done():
		return result, true, false
	}
}

// check reads the registry and decides what is available.
func (e *Engine) check(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	e.mu.Lock()
	e.checking = true
	if mode := e.mode(); mode != e.lastMode {
		switch mode {
		case "off":
			e.logf("Automatic updates are off: no host watches %s any more.", e.cfg.Trigger)
		case "host":
			e.logf("The host deploys: it watches %s.", e.cfg.Trigger)
		}
		e.lastMode = mode
	}
	e.mu.Unlock()
	var err error
	if e.pinned() {
		err = e.checkPinned(ctx)
	} else {
		err = e.checkTag(ctx)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.checking = false
	if err != nil {
		if e.checkErr != err.Error() {
			e.logf("%s", err.Error())
		}
		e.checkErr = err.Error()
		return
	}
	e.checkErr = ""
	e.checkedAt = e.now()
	e.save()
}

// info reads an image's annotations once per version or digest; they never
// change for those.
func (e *Engine) info(ctx context.Context, reference string) (ImageInfo, error) {
	e.mu.Lock()
	info, ok := e.infos[reference]
	e.mu.Unlock()
	if ok {
		return info, nil
	}
	info, err := e.registry.Info(ctx, reference)
	if err != nil {
		return info, err
	}
	e.mu.Lock()
	e.infos[reference] = info
	e.mu.Unlock()
	return info, nil
}

func (e *Engine) versions(ctx context.Context) ([]Version, error) {
	tags, err := e.registry.Tags(ctx)
	if err != nil {
		return nil, err
	}
	var versions []Version
	for _, tag := range tags {
		if v, ok := ParseVersion(tag); ok {
			versions = append(versions, v)
		}
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i].Compare(versions[j]) > 0 })
	return versions, nil
}

// checkPinned: the channel decides (docs: "Rules").
func (e *Engine) checkPinned(ctx context.Context) error {
	e.mu.Lock()
	installed := e.installed
	e.mu.Unlock()
	if installed == nil {
		info, err := e.info(ctx, e.cfg.Tag)
		if err != nil {
			return fmt.Errorf("Cannot tell which version runs: %v", err)
		}
		e.mu.Lock()
		delete(e.infos, e.cfg.Tag) // the tag moves
		e.mu.Unlock()
		v, ok := ParseVersion(info.Version)
		if !ok {
			return fmt.Errorf("Cannot tell which version runs: the image %s names no version. Set %s to a version, such as 0.2.0.", e.cfg.Image, e.cfg.VersionVariable)
		}
		installed = &v
		e.mu.Lock()
		e.installed, e.state.Installed = installed, v.Text
		e.mu.Unlock()
		e.logf("Installed version: %s.", v.Text)
	}
	all, err := e.versions(ctx)
	if err != nil {
		return err
	}
	e.mu.Lock()
	e.versionsSeen = all
	channel := e.state.Channel
	now := e.now()
	// Forget versions at or below the installed one, but not while an
	// installation runs: the container of the version being tried may
	// still go back to the one before, and older failures must stay.
	if e.state.Install == nil {
		for key := range e.state.Seen {
			if v, ok := ParseVersion(key); !ok || v.Compare(*installed) <= 0 {
				delete(e.state.Seen, key)
			}
		}
		for key := range e.state.Failed {
			if v, ok := ParseVersion(key); !ok || v.Compare(*installed) <= 0 {
				delete(e.state.Failed, key)
			}
		}
	}
	var inSeries, newer []Version
	for _, v := range all {
		if v.Compare(*installed) <= 0 || (channel == "stable" && v.IsPreview()) {
			continue
		}
		if _, ok := e.state.Seen[v.Text]; !ok {
			e.state.Seen[v.Text] = now.Unix()
		}
		if channel == "preview" || v.Series() == installed.Series() {
			inSeries = append(inSeries, v)
		} else {
			newer = append(newer, v)
		}
	}
	e.mu.Unlock()

	var available, attention *offer
	if v, ok := Newest(inSeries); ok {
		available = e.describe(ctx, v, *installed, all)
		available.InstallsAt = e.installsAt(ctx, available)
	}
	if v, ok := Newest(newer); ok {
		attention = e.describe(ctx, v, *installed, all)
	}
	e.mu.Lock()
	if attention != nil {
		if message, failed := e.state.Failed[attention.Version]; failed {
			attention.Failed = &message
		}
	}
	e.available, e.attention = available, attention
	e.mu.Unlock()
	return nil
}

// describe finds the notes, link and date of a version.
func (e *Engine) describe(ctx context.Context, v Version, installed Version, all []Version) *offer {
	o := &offer{version: &v, Update: Update{Version: v.Text, Preview: v.IsPreview(), Notes: []string{}}}
	info, _ := e.info(ctx, v.Text)
	repo, hasRepo := ownerRepo(orDefault(e.cfg.Source, info.Source))
	if created, err := time.Parse(time.RFC3339, info.Created); err == nil {
		o.PublishedAt = &created
	}
	if v.IsPreview() {
		if !hasRepo {
			return o
		}
		base := "v" + installed.Text
		if installed.IsPreview() {
			base = ""
			if before, err := e.info(ctx, installed.Text); err == nil {
				base = before.Revision
			}
		}
		if base != "" && info.Revision != "" {
			link := fmt.Sprintf("https://github.com/%s/compare/%s...%s", repo, base, info.Revision)
			o.URL = &link
			if notes, err := e.github.Changes(ctx, repo, base, info.Revision); err == nil {
				o.Notes, o.Changes = limitNotes(notes)
			}
		}
		return o
	}
	if !hasRepo {
		return o
	}
	link := fmt.Sprintf("https://github.com/%s/releases/tag/v%s", repo, v.Text)
	o.URL = &link
	if release, err := e.github.Release(ctx, repo, v.Text); err == nil {
		if release.URL != "" {
			o.URL = &release.URL
		}
		if !release.PublishedAt.IsZero() {
			published := release.PublishedAt
			o.PublishedAt, o.released = &published, &published
		}
	}
	// The notes of every release since the installed one, newest first.
	var notes []string
	count := 0
	for _, r := range all {
		if r.IsPreview() || r.Compare(v) > 0 || r.Compare(installed) <= 0 {
			continue
		}
		if count++; count > 5 {
			break
		}
		if release, err := e.github.Release(ctx, repo, r.Text); err == nil {
			notes = append(notes, release.Notes...)
		}
	}
	o.Notes, o.Changes = limitNotes(notes)
	return o
}

func limitNotes(notes []string) ([]string, int) {
	seen := map[string]bool{}
	unique := []string{}
	for _, note := range notes {
		if !seen[note] {
			seen[note] = true
			unique = append(unique, note)
		}
	}
	if len(unique) > 15 {
		return unique[:15], len(unique)
	}
	return unique, len(unique)
}

// readyAt is when a version may install automatically: previews and the
// Preview channel at once, releases on Stable once out for the cooldown
// (from the GitHub release date, or when the updater first saw it).
func (e *Engine) readyAt(ctx context.Context, o *offer) time.Time {
	e.mu.Lock()
	channel := e.state.Channel
	key := o.Version
	if o.digest != "" {
		key = o.digest
	}
	seen := time.Unix(e.state.Seen[key], 0)
	if e.state.Seen[key] == 0 {
		seen = e.now()
	}
	e.mu.Unlock()
	// Previews also on a platform that follows the tag preview.
	if o.version != nil && (o.version.IsPreview() || (e.pinned() && channel == "preview")) {
		return seen
	}
	out := seen
	if o.released != nil && e.pinned() {
		out = *o.released
	}
	return out.Add(e.cfg.Cooldown)
}

// installsAt is when an update installs automatically; nil when it does not.
func (e *Engine) installsAt(ctx context.Context, o *offer) *time.Time {
	e.mu.Lock()
	failed, hasFailed := e.state.Failed[o.Version]
	e.mu.Unlock()
	if hasFailed {
		o.Failed = &failed
		return nil
	}
	if e.mode() == "off" {
		return nil
	}
	at := e.readyAt(ctx, o)
	if now := e.now(); at.Before(now) {
		at = now
	}
	if e.cfg.Window != nil {
		at = e.cfg.Window.NextOpen(at.In(e.cfg.Location))
	}
	at = at.UTC()
	return &at
}

// checkTag follows the image tag, as on a platform.
func (e *Engine) checkTag(ctx context.Context) error {
	e.mu.Lock()
	installedDigest := e.state.InstalledDigest
	e.mu.Unlock()
	latest, err := e.registry.Digest(ctx, e.cfg.Tag)
	if err != nil {
		return err
	}
	if installedDigest == "" {
		installedDigest = latest
		e.mu.Lock()
		e.state.InstalledDigest = latest
		e.mu.Unlock()
		e.logf("Current image: %s", latest)
	}
	var installed *Version
	if info, err := e.info(ctx, installedDigest); err == nil {
		if v, ok := ParseVersion(info.Version); ok {
			installed = &v
		}
	}
	e.mu.Lock()
	if installed != nil {
		e.installed, e.state.Installed = installed, installed.Text
	}
	var available *offer
	if latest != installedDigest {
		if _, ok := e.state.Seen[latest]; !ok {
			// A newer image starts the cooldown again.
			e.state.Seen = map[string]int64{latest: e.now().Unix()}
			if e.cfg.Cooldown == 0 || e.cfg.Tag == "preview" { // previews: no cooldown
				e.logf("%s points to a new image: %s.", e.cfg.Image, latest)
			} else {
				e.logf("%s points to a new image: %s. It installs once it has been out for %s.", e.cfg.Image, latest, e.cfg.CooldownText)
			}
		}
	} else {
		e.state.Seen = map[string]int64{}
	}
	e.mu.Unlock()

	var all []Version
	if versions, err := e.versions(ctx); err == nil {
		all = versions
	}
	if latest != installedDigest {
		info, _ := e.info(ctx, latest)
		if v, ok := ParseVersion(info.Version); ok && installed != nil {
			available = e.describe(ctx, v, *installed, all)
		} else {
			available = &offer{Update: Update{Version: "sha256:" + shortDigest(latest), Notes: []string{}}}
			if v, ok := ParseVersion(info.Version); ok {
				available.Version, available.version, available.Preview = v.Text, &v, v.IsPreview()
			}
		}
		available.digest = latest
		available.InstallsAt = e.installsAt(ctx, available)
	}

	// A release that the tag never reaches needs someone to change the tag.
	var attention *offer
	for _, r := range all {
		if r.IsPreview() {
			continue
		}
		if !reachedBy(e.cfg.Tag, r) && (installed == nil || r.Compare(*installed) > 0) && beyondTag(e.cfg.Tag, r) {
			if installed != nil {
				attention = e.describe(ctx, r, *installed, all)
			} else {
				attention = &offer{version: &r, Update: Update{Version: r.Text, Notes: []string{}}}
			}
			e.mu.Lock()
			e.once("newer "+r.Text, 24*time.Hour, "Release %s is out, and the tag %s does not reach it. To install it, read the release notes of every release since yours, then change the tag.", r.Text, e.cfg.Tag)
			e.mu.Unlock()
		}
		break // the newest release only
	}
	e.mu.Lock()
	e.available, e.attention = available, attention
	e.mu.Unlock()
	return nil
}

// beyondTag: a release greater than the tag in the parts the tag names (0.4.0
// for the tag 0.3, not 0.2.9).
func beyondTag(tag string, v Version) bool {
	parts := strings.Count(tag, ".") + 1
	if parts > 3 {
		return false
	}
	t, ok := ParseVersion(tag + strings.Repeat(".0", 3-parts))
	if !ok || t.IsPreview() {
		return false
	}
	a := []int{v.Major, v.Minor, v.Patch}[:parts]
	b := []int{t.Major, t.Minor, t.Patch}[:parts]
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

func shortDigest(digest string) string {
	hexPart := strings.TrimPrefix(digest, "sha256:")
	if len(hexPart) > 12 {
		return hexPart[:12]
	}
	return hexPart
}

// autoInstall installs what is due (docs: "Automatic install").
func (e *Engine) autoInstall(ctx context.Context) {
	if e.mode() == "off" {
		return
	}
	e.mu.Lock()
	busy := e.state.Install != nil
	available := e.available
	stale := e.checkErr != ""
	e.wakeAt = time.Time{}
	e.mu.Unlock()
	// Only from a check that just succeeded: an older offer may be gone.
	if busy || available == nil || stale {
		return
	}
	var due *offer
	if e.pinned() {
		due = e.dueVersion(ctx)
	} else if available.InstallsAt != nil && !available.InstallsAt.After(e.now()) {
		due = available
	} else if available.InstallsAt != nil && e.cfg.Window != nil && !e.cfg.Window.Contains(e.now().In(e.cfg.Location)) && !e.readyAt(ctx, available).After(e.now()) {
		e.mu.Lock()
		e.once("window "+available.Version, 24*time.Hour, "Waiting for the window that opens at %s.", e.cfg.Window.Text[:5])
		e.wakeAt = e.cfg.Window.NextOpen(e.now().In(e.cfg.Location))
		e.mu.Unlock()
	}
	if due == nil {
		return
	}
	if err := e.install(ctx, due, false); err != nil {
		e.logf("%s", err.Error())
	}
}

// dueVersion is the newest candidate that may install now: on Stable the
// newest release of the series that is out for the cooldown.
func (e *Engine) dueVersion(ctx context.Context) *offer {
	e.mu.Lock()
	installed := e.installed
	channel := e.state.Channel
	all := e.versionsSeen
	e.mu.Unlock()
	if installed == nil {
		return nil
	}
	now := e.now()
	for _, v := range all { // newest first
		if v.Compare(*installed) <= 0 {
			break
		}
		if channel == "stable" && (v.IsPreview() || v.Series() != installed.Series()) {
			continue
		}
		e.mu.Lock()
		_, failed := e.state.Failed[v.Text]
		e.mu.Unlock()
		if failed {
			continue
		}
		o := &offer{version: &v, Update: Update{Version: v.Text, Preview: v.IsPreview(), Notes: []string{}}}
		e.mu.Lock()
		if e.available != nil && e.available.Version == v.Text {
			o = e.available
		}
		e.mu.Unlock()
		if o.released == nil && channel == "stable" {
			o = e.describe(ctx, v, *installed, all)
		}
		cooled := e.readyAt(ctx, o)
		ready := cooled
		if e.cfg.Window != nil {
			if ready.Before(now) {
				ready = now
			}
			ready = e.cfg.Window.NextOpen(ready.In(e.cfg.Location))
		}
		if ready.After(now) {
			// Wake for the earliest candidate that becomes due.
			e.mu.Lock()
			if e.wakeAt.IsZero() || ready.Before(e.wakeAt) {
				e.wakeAt = ready
			}
			if !cooled.After(now) {
				e.once("window "+v.Text, 24*time.Hour, "%s waits for the window that opens at %s.", v.Text, e.cfg.Window.Text[:5])
			}
			e.mu.Unlock()
			continue
		}
		return o
	}
	return nil
}

var errNotOffered = errors.New("not offered")

// install starts an installation: manual from the app, or automatic.
func (e *Engine) install(ctx context.Context, o *offer, manual bool) error {
	mode := e.mode()
	if mode == "off" {
		return errors.New("Automatic updates are off on this server, so nothing can install. Set them up as the deployment guide describes.")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.state.Install != nil {
		return fmt.Errorf("%s is being installed.", e.state.Install.Version)
	}
	id := randomID()
	previous := e.state.Installed
	switch {
	case mode == "platform":
		e.logf("Calling the deploy link.")
		// Saved as installed before the call: the platform may stop this
		// container while it answers, and the next start must not deploy the
		// same image again.
		before := e.state.InstalledDigest
		e.state.InstalledDigest = o.digest
		e.save()
		e.mu.Unlock()
		err := e.callHook(ctx)
		e.mu.Lock()
		if err != nil {
			e.state.InstalledDigest = before
			e.state.Last = &Last{Version: o.Version, Result: "failed", Message: err.Error() + " The updater tries again at the next check.", FinishedAt: e.now()}
			e.save()
			e.logf("%s The deployment did not start; trying again in %s.", err.Error(), e.cfg.Interval)
			return err
		}
		e.logf("The platform accepted the deployment.")
		e.state.Last = &Last{Version: o.Version, Result: "accepted", FinishedAt: e.now()}
		if o.version != nil {
			e.installed = o.version
			e.state.Installed = o.version.Text
		}
		e.available = nil
		e.save()
		return nil
	case e.pinned():
		// Saved before the request: if this container stops right after, the
		// next one still knows which result to wait for.
		e.state.Install = &Install{ID: id, Version: o.Version, Previous: previous, StartedAt: e.now(), Manual: manual, Stage: "requested"}
		if err := e.save(); err != nil {
			e.state.Install = nil
			return fmt.Errorf("Cannot keep the state in %s, so nothing is installed: %v", e.cfg.Trigger, err)
		}
		request := fmt.Sprintf("protocol=2\nid=%s\nversion=%s\n", id, o.Version)
		if err := writeAtomic(e.path("deploy"), []byte(request), 0o644); err != nil {
			e.state.Install = nil
			e.save()
			return fmt.Errorf("Cannot write to %s: %v", e.cfg.Trigger, err)
		}
		e.state.Install.Published = true
		_ = e.save()
		e.logf("Asking the host to install %s.", o.Version)
	default:
		// A host setup of updater 0.1 (or no VERSION_VARIABLE): the host
		// deploys what the tag points to. Previous keeps the digest before.
		e.state.Install = &Install{ID: id, Version: o.Version, Previous: e.state.InstalledDigest, StartedAt: e.now(), Manual: manual, Stage: "deploying", Legacy: true}
		e.state.InstalledDigest = o.digest
		if err := e.save(); err != nil {
			e.state.InstalledDigest = e.state.Install.Previous
			e.state.Install = nil
			return fmt.Errorf("Cannot keep the state in %s, so nothing is installed: %v", e.cfg.Trigger, err)
		}
		if err := writeAtomic(e.path("deploy"), []byte(o.digest+"\n"), 0o644); err != nil {
			e.state.InstalledDigest = e.state.Install.Previous
			e.state.Install = nil
			e.save()
			return fmt.Errorf("Cannot write to %s: %v", e.cfg.Trigger, err)
		}
		e.state.Install.Published = true
		_ = e.save()
		e.logf("Asking the host to deploy.")
	}
	return nil
}

func randomID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (e *Engine) callHook(ctx context.Context) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, e.cfg.HookURL, nil)
	if err != nil {
		// The error names the link, which may hold a secret.
		return errors.New("DEPLOY_HOOK_URL is not a valid link.")
	}
	if e.cfg.HookToken != "" {
		request.Header.Set("Authorization", "Bearer "+e.cfg.HookToken)
	}
	response, err := e.hook.Do(request)
	if err != nil {
		// The error names the link, which may hold a secret: say less.
		return errors.New("The deploy link did not answer.")
	}
	drain(response)
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return fmt.Errorf("The deploy link answered HTTP %d.", response.StatusCode)
	}
	return nil
}

// finish records the outcome of the running installation. Locked.
func (e *Engine) finish(last Last) {
	e.state.Last = &last
	e.state.Install = nil
	if last.Result == "failed" || last.Result == "rolled-back" {
		message := last.Message
		if message == "" {
			message = "The installation failed."
		}
		e.state.Failed[last.Version] = message
		if e.available != nil && e.available.Version == last.Version {
			e.available.Failed = &message
			e.available.InstallsAt = nil
		}
	}
	if last.Result == "installed" {
		if v, ok := ParseVersion(last.Version); ok {
			e.installed = &v
			e.state.Installed = v.Text
		}
		e.available, e.attention = nil, nil
	}
	e.save()
}

// pollHost follows an installation on the host: its stage and its result.
func (e *Engine) pollHost() {
	e.mu.Lock()
	defer e.mu.Unlock()
	install := e.state.Install
	if install == nil {
		return
	}
	pending := exists(e.path("deploy"))
	age := e.now().Sub(install.StartedAt)
	if install.Legacy {
		if !pending && !install.Published {
			// The updater stopped before it wrote the request.
			e.abandon(install)
		} else if !pending {
			e.logf("The host deploys it now.")
			e.finish(Last{Version: install.Version, Result: "accepted", FinishedAt: e.now()})
		} else if age > 10*time.Minute {
			e.abandon(install)
		}
		return
	}
	if progress, ok := readFields(e.path("progress")); ok && progress["id"] == install.ID {
		switch stage := progress["stage"]; stage {
		case "pulling", "starting", "rolling-back":
			if install.Stage != stage {
				install.Stage = stage
				e.save()
			}
		}
	}
	if result, ok := readFields(e.path("result")); ok && result["id"] == install.ID {
		last := Last{Version: install.Version, Result: result["result"], Message: result["message"], FinishedAt: e.now()}
		if seconds, err := parseUnix(result["finished"]); err == nil {
			last.FinishedAt = seconds
		}
		switch last.Result {
		case "installed":
			e.logf("%s is installed.", install.Version)
		case "rolled-back":
			e.logf("%s did not start, so the version before runs again: %s", install.Version, last.Message)
		default:
			last.Result = "failed"
			e.logf("%s did not install: %s", install.Version, last.Message)
		}
		e.finish(last)
		return
	}
	switch {
	case pending && age > 10*time.Minute:
		e.abandon(install)
	case !pending && age > time.Hour:
		e.logf("The host did not report the result of installing %s.", install.Version)
		e.state.Last = &Last{Version: install.Version, Result: "failed",
			Message: fmt.Sprintf("The server did not report a result within an hour. See journalctl -u %s.service.", e.projectHint()), FinishedAt: e.now()}
		e.state.Install = nil
		e.save()
	}
}

// abandon takes back a request that no host picked up. A later check asks
// again.
func (e *Engine) abandon(install *Install) {
	_ = os.Remove(e.path("deploy"))
	if install.Legacy {
		// Nothing was deployed: the next check offers the image again.
		e.state.InstalledDigest = install.Previous
	}
	e.logf("No host picked up the installation of %s.", install.Version)
	e.state.Last = &Last{Version: install.Version, Result: "failed",
		Message:    fmt.Sprintf("The server did not pick up the installation within 10 minutes. Check that %s.path is active (systemctl status %s.path).", e.projectHint(), e.projectHint()),
		FinishedAt: e.now()}
	e.state.Install = nil
	e.save()
}

func parseUnix(text string) (time.Time, error) {
	var seconds int64
	if _, err := fmt.Sscanf(text, "%d", &seconds); err != nil || seconds <= 0 {
		return time.Time{}, errors.New("no time")
	}
	return time.Unix(seconds, 0), nil
}
