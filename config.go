package main

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Config is the updater's environment (README.md, "Settings").
type Config struct {
	Image      string // IMAGE as given, e.g. ghcr.io/meldgrove/handbook:0.1.3
	Registry   string // ghcr.io, registry-1.docker.io, localhost:5000
	Repository string // meldgrove/handbook
	Tag        string // 0.1.3, 0.1, latest, preview
	Scheme     string // https, or http for a registry on localhost

	VersionVariable string
	HookURL         string
	HookToken       string
	Cooldown        time.Duration
	CooldownText    string
	Window          *Window
	Location        *time.Location
	TimeZone        string
	Interval        time.Duration

	RegistryUsername string
	RegistryPassword string
	GitHubToken      string
	GitHubAPI        string
	Source           string // repository URL for release notes; empty: from the image

	Listen  string // API address; "off" switches it off
	Trigger string // the volume shared with the host and kept over deployments
	Version string // the updater's own version
}

var variablePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// LoadConfig reads the environment. getenv is os.Getenv outside tests.
func LoadConfig(getenv func(string) string) (Config, error) {
	c := Config{
		Image:            getenv("IMAGE"),
		VersionVariable:  getenv("VERSION_VARIABLE"),
		HookURL:          getenv("DEPLOY_HOOK_URL"),
		HookToken:        getenv("DEPLOY_HOOK_TOKEN"),
		CooldownText:     getenv("COOLDOWN"),
		RegistryUsername: getenv("REGISTRY_USERNAME"),
		RegistryPassword: getenv("REGISTRY_PASSWORD"),
		GitHubToken:      getenv("GITHUB_TOKEN"),
		GitHubAPI:        orDefault(getenv("GITHUB_API_URL"), "https://api.github.com"),
		Source:           getenv("SOURCE"),
		Listen:           orDefault(getenv("LISTEN"), ":8080"),
		Trigger:          orDefault(getenv("TRIGGER_DIR"), "/trigger"),
		Version:          getenv("APP_VERSION"),
		TimeZone:         orDefault(getenv("TZ"), "UTC"),
	}
	if c.Image == "" {
		return c, errors.New("Set IMAGE to the image and tag the app runs.")
	}
	if err := c.parseImage(); err != nil {
		return c, err
	}
	if c.VersionVariable != "" && !variablePattern.MatchString(c.VersionVariable) {
		return c, fmt.Errorf("VERSION_VARIABLE must be the name of a variable, such as HANDBOOK_VERSION")
	}
	interval := orDefault(getenv("CHECK_INTERVAL"), "3600")
	seconds, err := strconv.Atoi(interval)
	if err != nil || seconds <= 0 {
		return c, errors.New("CHECK_INTERVAL must be a number of seconds.")
	}
	c.Interval = time.Duration(seconds) * time.Second
	if c.Cooldown, err = ParseCooldown(c.CooldownText); err != nil {
		return c, err
	}
	if c.Cooldown == 0 {
		c.CooldownText = ""
	}
	if c.Window, err = ParseWindow(getenv("WINDOW")); err != nil {
		return c, err
	}
	if c.Location, err = time.LoadLocation(c.TimeZone); err != nil {
		return c, fmt.Errorf("TZ names no time zone: %s", c.TimeZone)
	}
	if c.HookURL != "" && !strings.HasPrefix(c.HookURL, "http://") && !strings.HasPrefix(c.HookURL, "https://") {
		return c, errors.New("DEPLOY_HOOK_URL must start with http:// or https://.")
	}
	return c, nil
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// parseImage splits IMAGE the way Docker reads it.
func (c *Config) parseImage() error {
	if strings.Contains(c.Image, "@") {
		return errors.New("IMAGE names a digest, which never changes. Name a tag instead.")
	}
	name, tag := c.Image, "latest"
	if i := strings.LastIndex(c.Image, ":"); i > strings.LastIndex(c.Image, "/") {
		name, tag = c.Image[:i], c.Image[i+1:]
	}
	if tag == "" || name == "" {
		return fmt.Errorf("IMAGE is not an image reference: %s", c.Image)
	}
	first, rest, hasSlash := strings.Cut(name, "/")
	if hasSlash && (strings.ContainsAny(first, ".:") || first == "localhost") {
		c.Registry, c.Repository = first, rest
	} else {
		c.Registry, c.Repository = "registry-1.docker.io", name
	}
	if c.Registry == "docker.io" || c.Registry == "index.docker.io" {
		c.Registry = "registry-1.docker.io"
	}
	if c.Registry == "registry-1.docker.io" && !strings.Contains(c.Repository, "/") {
		c.Repository = "library/" + c.Repository
	}
	c.Tag = tag
	c.Scheme = "https"
	if c.Registry == "localhost" || strings.HasPrefix(c.Registry, "localhost:") || strings.HasPrefix(c.Registry, "127.0.0.1:") {
		c.Scheme = "http"
	}
	return nil
}

// ImageName is IMAGE without its tag, e.g. ghcr.io/meldgrove/handbook.
func (c Config) ImageName() string {
	i := strings.LastIndex(c.Image, ":")
	if i > strings.LastIndex(c.Image, "/") {
		return c.Image[:i]
	}
	return c.Image
}

// LoadConfigFromEnvironment is LoadConfig for the running process.
func LoadConfigFromEnvironment() (Config, error) { return LoadConfig(os.Getenv) }
