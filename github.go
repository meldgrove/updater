package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// GitHub reads release notes and the changes of previews. Everything here is
// optional: without an answer, the app shows the version and no notes.
type GitHub struct {
	api    string
	token  string
	client *http.Client

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	at    time.Time
	value any
	err   error
}

func NewGitHub(c Config) *GitHub {
	return &GitHub{api: strings.TrimRight(c.GitHubAPI, "/"), token: c.GitHubToken,
		client: &http.Client{Timeout: 15 * time.Second}, cache: map[string]cached{}}
}

// Release is one GitHub Release.
type Release struct {
	URL         string
	PublishedAt time.Time
	Notes       []string
}

// ownerRepo reads owner and repository from a GitHub URL, such as the
// image's org.opencontainers.image.source.
func ownerRepo(source string) (string, bool) {
	u, err := url.Parse(source)
	if err != nil || u.Host != "github.com" {
		return "", false
	}
	parts := strings.Split(strings.Trim(strings.TrimSuffix(u.Path, ".git"), "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", false
	}
	return parts[0] + "/" + parts[1], true
}

func (g *GitHub) get(ctx context.Context, path string, into any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, g.api+path, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if g.token != "" {
		request.Header.Set("Authorization", "Bearer "+g.token)
	}
	response, err := g.client.Do(request)
	if err != nil {
		return err
	}
	defer drain(response)
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub answered HTTP %d for %s", response.StatusCode, path)
	}
	return json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(into)
}

// remember caches answers (and failures) for six hours: release notes rarely
// change, and GitHub allows 60 requests an hour without a token.
func (g *GitHub) remember(key string, fetch func() (any, error)) (any, error) {
	g.mu.Lock()
	entry, ok := g.cache[key]
	g.mu.Unlock()
	if ok && time.Since(entry.at) < 6*time.Hour {
		return entry.value, entry.err
	}
	value, err := fetch()
	g.mu.Lock()
	g.cache[key] = cached{at: time.Now(), value: value, err: err}
	g.mu.Unlock()
	return value, err
}

// Release reads the GitHub Release of tag vX.Y.Z.
func (g *GitHub) Release(ctx context.Context, repo, version string) (*Release, error) {
	value, err := g.remember("release "+repo+" "+version, func() (any, error) {
		var answer struct {
			HTMLURL     string    `json:"html_url"`
			PublishedAt time.Time `json:"published_at"`
			Body        string    `json:"body"`
		}
		if err := g.get(ctx, "/repos/"+repo+"/releases/tags/v"+url.PathEscape(version), &answer); err != nil {
			return nil, err
		}
		return &Release{URL: answer.HTMLURL, PublishedAt: answer.PublishedAt, Notes: releaseNotes(answer.Body)}, nil
	})
	if err != nil {
		return nil, err
	}
	return value.(*Release), nil
}

// Changes reads the pull request titles merged between two commits or tags,
// newest first, for a preview.
func (g *GitHub) Changes(ctx context.Context, repo, base, head string) ([]string, error) {
	value, err := g.remember("compare "+repo+" "+base+" "+head, func() (any, error) {
		var answer struct {
			Commits []struct {
				Commit struct {
					Message string `json:"message"`
				} `json:"commit"`
				Parents []struct{} `json:"parents"`
			} `json:"commits"`
		}
		if err := g.get(ctx, "/repos/"+repo+"/compare/"+url.PathEscape(base)+"..."+url.PathEscape(head), &answer); err != nil {
			return nil, err
		}
		var merged, plain []string
		for i := len(answer.Commits) - 1; i >= 0; i-- {
			commit := answer.Commits[i]
			subject, body, _ := strings.Cut(commit.Commit.Message, "\n")
			if strings.HasPrefix(subject, "Merge pull request #") {
				if title := firstLine(body); title != "" {
					merged = append(merged, title)
				}
			} else if len(commit.Parents) < 2 {
				plain = append(plain, strings.TrimSpace(subject))
			}
		}
		if len(merged) > 0 {
			return merged, nil
		}
		return plain, nil
	})
	if err != nil {
		return nil, err
	}
	return value.([]string), nil
}

func firstLine(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

var (
	noteItem   = regexp.MustCompile(`^\s*[*-]\s+(.+)$`)
	noteSuffix = regexp.MustCompile(`\s+by @[\w.\-\[\]]+ in https?://\S+$`)
	noteLink   = regexp.MustCompile(`\s+in https?://\S+$`)
)

// releaseNotes takes the change titles from the notes that GitHub generates:
// "* feat: Add X by @someone in https://…/pull/12" becomes "feat: Add X".
// The image and upgrade lines at the top, headings and the changelog link
// are left out.
func releaseNotes(body string) []string {
	var notes []string
	for _, line := range strings.Split(body, "\n") {
		match := noteItem.FindStringSubmatch(strings.TrimRight(line, "\r"))
		if match == nil {
			continue
		}
		item := noteSuffix.ReplaceAllString(match[1], "")
		item = noteLink.ReplaceAllString(item, "")
		item = strings.TrimSpace(item)
		if item == "" || strings.HasPrefix(item, "@") || strings.HasPrefix(strings.ToLower(item), "full changelog") {
			continue
		}
		notes = append(notes, item)
	}
	return notes
}
