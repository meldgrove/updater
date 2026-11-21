package main

import (
	"testing"
	"time"
)

func mustVersion(t *testing.T, text string) Version {
	t.Helper()
	v, ok := ParseVersion(text)
	if !ok {
		t.Fatalf("%q is no version", text)
	}
	return v
}

func TestParseVersion(t *testing.T) {
	for _, text := range []string{"0.2.0", "1.10.3", "0.2.1-preview.3", "10.0.0-preview.12"} {
		if _, ok := ParseVersion(text); !ok {
			t.Errorf("%q should parse", text)
		}
	}
	for _, text := range []string{"latest", "0.2", "1", "v1.2.3", "1.2.3-rc.1", "1.2.3-preview.0", "01.2.3", "1.2.3-preview", "1.2.3+build", ""} {
		if _, ok := ParseVersion(text); ok {
			t.Errorf("%q should not parse", text)
		}
	}
}

func TestCompareOrdersPreviewsBeforeTheirRelease(t *testing.T) {
	ordered := []string{"0.2.0", "0.2.1-preview.1", "0.2.1-preview.2", "0.2.1-preview.10", "0.2.1", "0.3.0-preview.1", "0.3.0", "0.10.0", "1.0.0"}
	for i := 0; i < len(ordered); i++ {
		for j := 0; j < len(ordered); j++ {
			got := mustVersion(t, ordered[i]).Compare(mustVersion(t, ordered[j]))
			want := 0
			if i < j {
				want = -1
			} else if i > j {
				want = 1
			}
			if got != want {
				t.Errorf("Compare(%s, %s) = %d, want %d", ordered[i], ordered[j], got, want)
			}
		}
	}
}

func TestSeries(t *testing.T) {
	cases := map[string]string{"0.2.0": "0.2", "0.2.9": "0.2", "0.3.0-preview.1": "0.3", "1.4.2": "1", "2.0.0": "2"}
	for text, want := range cases {
		if got := mustVersion(t, text).Series(); got != want {
			t.Errorf("Series(%s) = %s, want %s", text, got, want)
		}
	}
}

func TestTagReach(t *testing.T) {
	cases := []struct {
		tag, version   string
		reach, outside bool
	}{
		{"0.3", "0.3.7", true, false},
		{"0.3", "0.4.0", false, true},
		{"0.3", "0.2.9", false, false},
		{"1", "1.9.0", true, false},
		{"1", "2.0.0", false, true},
		{"1.2.3", "1.2.4", false, true},
		{"latest", "9.9.9", true, false},
		{"preview", "9.9.9", true, false},
	}
	for _, c := range cases {
		v := mustVersion(t, c.version)
		if got := reachedBy(c.tag, v); got != c.reach {
			t.Errorf("reachedBy(%s, %s) = %v", c.tag, c.version, got)
		}
		if got := beyondTag(c.tag, v); got != c.outside {
			t.Errorf("beyondTag(%s, %s) = %v", c.tag, c.version, got)
		}
	}
	if beyondTag("1.2.3-preview.1", mustVersion(t, "2.0.0")) {
		t.Error("a preview tag has no series")
	}
}

func TestCooldown(t *testing.T) {
	cases := map[string]time.Duration{"": 0, "0": 0, "90": 90 * time.Second, "90s": 90 * time.Second, "30m": 30 * time.Minute, "12h": 12 * time.Hour, "3d": 72 * time.Hour}
	for text, want := range cases {
		got, err := ParseCooldown(text)
		if err != nil || got != want {
			t.Errorf("ParseCooldown(%q) = %v, %v", text, got, err)
		}
	}
	for _, text := range []string{"soon", "3w", "-1", "1.5h"} {
		if _, err := ParseCooldown(text); err == nil {
			t.Errorf("ParseCooldown(%q) should fail", text)
		}
	}
}

func TestWindow(t *testing.T) {
	for _, text := range []string{"2-5", "02:00-02:00", "25:00-03:00", "02:00", "02:60-03:00"} {
		if _, err := ParseWindow(text); err == nil {
			t.Errorf("ParseWindow(%q) should fail", text)
		}
	}
	berlin, _ := time.LoadLocation("Europe/Berlin")
	at := func(hour, minute int) time.Time { return time.Date(2026, 11, 19, hour, minute, 0, 0, berlin) }
	night, _ := ParseWindow("02:00-05:00")
	if !night.Contains(at(2, 0)) || !night.Contains(at(4, 59)) || night.Contains(at(5, 0)) || night.Contains(at(1, 59)) {
		t.Error("02:00-05:00 contains the wrong times")
	}
	if got := night.NextOpen(at(10, 0)); !got.Equal(time.Date(2026, 11, 20, 2, 0, 0, 0, berlin)) {
		t.Errorf("NextOpen after the window = %v", got)
	}
	if got := night.NextOpen(at(1, 30)); !got.Equal(at(2, 0)) {
		t.Errorf("NextOpen before the window = %v", got)
	}
	if got := night.NextOpen(at(3, 0)); !got.Equal(at(3, 0)) {
		t.Errorf("NextOpen inside the window = %v", got)
	}
	overnight, _ := ParseWindow("22:00-02:00")
	if !overnight.Contains(at(23, 0)) || !overnight.Contains(at(1, 0)) || overnight.Contains(at(12, 0)) {
		t.Error("22:00-02:00 contains the wrong times")
	}
	// Summer time skips 02:00-03:00 on 29 March; the next day opens on time.
	early, _ := ParseWindow("02:30-03:00")
	if got := early.NextOpen(time.Date(2026, 3, 29, 10, 0, 0, 0, berlin)); !got.Equal(time.Date(2026, 3, 30, 2, 30, 0, 0, berlin)) {
		t.Errorf("NextOpen after the change to summer time = %v", got)
	}
}

func TestReleaseNotes(t *testing.T) {
	body := "Image: `ghcr.io/meldgrove/app:0.2.1` for linux/amd64 and linux/arm64 (`sha256:abc`)\n\n" +
		"Upgrade: [docs/deployment.md](https://github.com/meldgrove/app/blob/v0.2.1/docs/deployment.md).\n\n" +
		"## What's Changed\n### Changes\n" +
		"* feat: Show who changed a page by @simonmcschubert in https://github.com/meldgrove/app/pull/12\r\n" +
		"* fix: Keep the session by @dependabot[bot] in https://github.com/meldgrove/app/pull/13\n" +
		"- docs: Explain updates\n\n" +
		"## New Contributors\n* @someone made their first contribution in https://github.com/meldgrove/app/pull/9\n\n" +
		"**Full Changelog**: https://github.com/meldgrove/app/compare/v0.2.0...v0.2.1\n"
	got := releaseNotes(body)
	want := []string{"feat: Show who changed a page", "fix: Keep the session", "docs: Explain updates"}
	if len(got) != len(want) {
		t.Fatalf("releaseNotes = %q", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("note %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestImageParsing(t *testing.T) {
	cases := []struct{ image, registry, repository, tag, scheme string }{
		{"ghcr.io/meldgrove/handbook:0.1", "ghcr.io", "meldgrove/handbook", "0.1", "https"},
		{"alpine:3", "registry-1.docker.io", "library/alpine", "3", "https"},
		{"docker.io/library/alpine", "registry-1.docker.io", "library/alpine", "latest", "https"},
		{"localhost:5000/app:1.0", "localhost:5000", "app", "1.0", "http"},
		{"127.0.0.1:5555/team/app:1.0.0-preview.2", "127.0.0.1:5555", "team/app", "1.0.0-preview.2", "http"},
	}
	for _, c := range cases {
		cfg, err := LoadConfig(func(name string) string {
			if name == "IMAGE" {
				return c.image
			}
			return ""
		})
		if err != nil {
			t.Fatalf("%s: %v", c.image, err)
		}
		if cfg.Registry != c.registry || cfg.Repository != c.repository || cfg.Tag != c.tag || cfg.Scheme != c.scheme {
			t.Errorf("%s: got %s %s %s %s", c.image, cfg.Registry, cfg.Repository, cfg.Tag, cfg.Scheme)
		}
	}
	for _, bad := range []map[string]string{
		{"IMAGE": "app@sha256:00"},
		{"IMAGE": "app:1", "CHECK_INTERVAL": "hourly"},
		{"IMAGE": "app:1", "COOLDOWN": "soon"},
		{"IMAGE": "app:1", "WINDOW": "2-5"},
		{"IMAGE": "app:1", "DEPLOY_HOOK_URL": "ftp://example.org/"},
		{"IMAGE": "app:1", "VERSION_VARIABLE": "APP VERSION"},
		{"IMAGE": "app:1", "TZ": "Mars/Base"},
		{"IMAGE": "app:1", "API_TOKEN": "two words"},
		{"IMAGE": "app:1", "API_TOKEN": "line\nbreak"},
		{},
	} {
		if _, err := LoadConfig(func(name string) string { return bad[name] }); err == nil {
			t.Errorf("LoadConfig(%v) should fail", bad)
		}
	}
}
