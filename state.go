package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// State is what the updater keeps in its volume, so that it survives the new
// containers of every deployment (docs: "State").
type State struct {
	Format          int               `json:"format"`
	Image           string            `json:"image"`     // IMAGE of the container that wrote it
	Container       string            `json:"container"` // its hostname: a new one means a new container
	Channel         string            `json:"channel,omitempty"`
	Installed       string            `json:"installed,omitempty"`       // version, when known
	InstalledDigest string            `json:"installedDigest,omitempty"` // following a tag
	Seen            map[string]int64  `json:"seen,omitempty"`            // version or digest: first seen (unix)
	Failed          map[string]string `json:"failed,omitempty"`          // version: why it did not install
	Install         *Install          `json:"install,omitempty"`
	Last            *Last             `json:"last,omitempty"`
}

// Install is an installation that runs.
type Install struct {
	ID        string    `json:"id"`
	Version   string    `json:"version"`
	Previous  string    `json:"previous,omitempty"`
	StartedAt time.Time `json:"startedAt"`
	Manual    bool      `json:"manual"`
	Stage     string    `json:"stage"`
	Legacy    bool      `json:"legacy,omitempty"`    // a host setup of updater 0.1 reports no result
	Published bool      `json:"published,omitempty"` // the request is written
}

// Last is the outcome of the last installation.
type Last struct {
	Version    string    `json:"version"`
	Result     string    `json:"result"` // installed, failed, rolled-back, accepted
	Message    string    `json:"message,omitempty"`
	FinishedAt time.Time `json:"finishedAt"`
}

const stateFormat = 2

func loadState(path string) State {
	var s State
	data, err := os.ReadFile(path)
	if err == nil {
		if json.Unmarshal(data, &s) != nil || s.Format != stateFormat {
			s = State{}
		}
	}
	s.Format = stateFormat
	if s.Seen == nil {
		s.Seen = map[string]int64{}
	}
	if s.Failed == nil {
		s.Failed = map[string]string{}
	}
	return s
}

func (s State) save(path string) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, data, 0o644)
}

// writeAtomic replaces a file in one step, so that a reader never sees half
// of it.
func writeAtomic(path string, data []byte, mode os.FileMode) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporary.Name(), path)
}

// readFields reads a file of key=value lines, as the host writes them.
func readFields(path string) (map[string]string, bool) {
	file, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer file.Close()
	fields := map[string]string{}
	scanner := bufio.NewScanner(io.LimitReader(file, 16<<10))
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), "=")
		if ok {
			fields[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	return fields, true
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
