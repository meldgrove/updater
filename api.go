package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Status is what GET /v1/status answers (docs/updates.md, "HTTP API").
type Status struct {
	Updater       string      `json:"updater"`
	Mode          string      `json:"mode"`
	Channel       string      `json:"channel"`
	ChannelLocked *string     `json:"channelLocked"`
	Installed     *string     `json:"installed"`
	Available     *Update     `json:"available"`
	Attention     *Update     `json:"attention"`
	Installing    *Installing `json:"installing"`
	Last          *LastStatus `json:"last"`
	Checking      bool        `json:"checking"`
	CheckedAt     *time.Time  `json:"checkedAt"`
	Error         *string     `json:"error"`
	Automatic     bool        `json:"automatic"`
	Cooldown      *string     `json:"cooldown"`
	Window        *string     `json:"window"`
	Timezone      string      `json:"timezone"`
}

type Installing struct {
	Version   string    `json:"version"`
	StartedAt time.Time `json:"startedAt"`
	Manual    bool      `json:"manual"`
	Stage     string    `json:"stage"`
}

type LastStatus struct {
	Version    string    `json:"version"`
	Result     string    `json:"result"`
	Message    *string   `json:"message"`
	FinishedAt time.Time `json:"finishedAt"`
}

func optional(text string) *string {
	if text == "" {
		return nil
	}
	return &text
}

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC().Truncate(time.Second)
	return &u
}

func publicUpdate(o *offer) *Update {
	if o == nil {
		return nil
	}
	u := o.Update
	u.PublishedAt, u.InstallsAt = utc(u.PublishedAt), utc(u.InstallsAt)
	if u.Notes == nil {
		u.Notes = []string{}
	}
	if u.Changes < len(u.Notes) {
		u.Changes = len(u.Notes)
	}
	return &u
}

// Status describes the updater for the app.
func (e *Engine) Status() Status {
	mode := e.mode()
	locked := e.channelLocked()
	e.mu.Lock()
	defer e.mu.Unlock()
	s := Status{
		Updater:       orDefault(e.cfg.Version, "development"),
		Mode:          mode,
		Channel:       e.state.Channel,
		ChannelLocked: optional(locked),
		Available:     publicUpdate(e.available),
		Attention:     publicUpdate(e.attention),
		Checking:      e.checking,
		Error:         optional(e.checkErr),
		Automatic:     mode != "off",
		Window:        nil,
		Timezone:      e.cfg.TimeZone,
	}
	if locked != "" {
		s.Channel = "stable"
		if v, ok := ParseVersion(e.cfg.Tag); e.cfg.Tag == "preview" || (ok && v.IsPreview()) {
			s.Channel = "preview"
		}
	}
	// The cooldown of stable releases; previews install without one.
	s.Cooldown = optional(e.cfg.CooldownText)
	if e.cfg.Window != nil {
		s.Window = &e.cfg.Window.Text
	}
	if e.installed != nil {
		s.Installed = &e.installed.Text
	}
	if !e.checkedAt.IsZero() {
		s.CheckedAt = utc(&e.checkedAt)
	}
	if i := e.state.Install; i != nil {
		s.Installing = &Installing{Version: i.Version, StartedAt: i.StartedAt.UTC().Truncate(time.Second), Manual: i.Manual, Stage: i.Stage}
	}
	if l := e.state.Last; l != nil {
		s.Last = &LastStatus{Version: l.Version, Result: l.Result, Message: optional(l.Message), FinishedAt: l.FinishedAt.UTC().Truncate(time.Second)}
	}
	return s
}

type apiError struct {
	status  int
	message string
}

func (e apiError) Error() string { return e.message }

// Handler serves the API on the Compose network. It has no authentication:
// the port is never published, and only the app calls it.
func (e *Engine) Handler(ctx context.Context) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, e.Status())
	})
	mux.HandleFunc("POST /v1/check", func(w http.ResponseWriter, r *http.Request) {
		e.do(ctx, 30*time.Second, func(ctx context.Context) {
			e.check(ctx)
			e.autoInstall(ctx)
		})
		writeJSON(w, http.StatusOK, e.Status())
	})
	mux.HandleFunc("POST /v1/install", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Version string `json:"version"`
		}
		if !readJSON(w, r, &body) {
			return
		}
		if body.Version == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Name the version to install."})
			return
		}
		var failure error
		if !e.do(ctx, 15*time.Second, func(ctx context.Context) { failure = e.manualInstall(ctx, body.Version) }) {
			failure = apiError{http.StatusServiceUnavailable, "The updater is busy. Try again in a minute."}
		}
		if failure != nil {
			status := http.StatusConflict
			var known apiError
			if errors.As(failure, &known) {
				status = known.status
			}
			writeJSON(w, status, map[string]any{"error": failure.Error()})
			return
		}
		writeJSON(w, http.StatusAccepted, e.Status())
	})
	mux.HandleFunc("POST /v1/channel", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Channel string `json:"channel"`
		}
		if !readJSON(w, r, &body) {
			return
		}
		if body.Channel != "stable" && body.Channel != "preview" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "The channel is stable or preview."})
			return
		}
		if locked := e.channelLocked(); locked != "" {
			writeJSON(w, http.StatusConflict, map[string]string{"error": locked})
			return
		}
		e.do(ctx, 30*time.Second, func(ctx context.Context) {
			e.mu.Lock()
			changed := e.state.Channel != body.Channel
			e.state.Channel = body.Channel
			e.save()
			e.mu.Unlock()
			if changed {
				e.logf("Channel: %s.", body.Channel)
			}
			e.check(ctx)
		})
		writeJSON(w, http.StatusOK, e.Status())
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Not found."})
	})
	return mux
}

// manualInstall installs an offered version at once, regardless of the
// cooldown, the window, or an earlier failure.
func (e *Engine) manualInstall(ctx context.Context, version string) error {
	e.mu.Lock()
	var chosen *offer
	for _, o := range []*offer{e.available, e.attention} {
		if o != nil && o.Version == version {
			chosen = o
		}
	}
	installing := e.state.Install
	e.mu.Unlock()
	if installing != nil {
		return apiError{http.StatusConflict, fmt.Sprintf("%s is being installed.", installing.Version)}
	}
	if chosen == nil {
		return apiError{http.StatusConflict, fmt.Sprintf("%s is not offered. Check for updates again.", version)}
	}
	if !e.pinned() && chosen == e.attention {
		return apiError{http.StatusConflict, fmt.Sprintf("The image tag %s does not reach %s. Take its upgrade steps and change the tag where the app is deployed.", e.cfg.Tag, version)}
	}
	if err := e.install(ctx, chosen, true); err != nil {
		return apiError{http.StatusConflict, err.Error()}
	}
	return nil
}

func readJSON(w http.ResponseWriter, r *http.Request, into any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	if err := decoder.Decode(into); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "The request is not the expected JSON."})
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
