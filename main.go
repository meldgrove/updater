// Meldgrove Updater: installs new releases (or previews) of a self-hosted app
// that runs with Docker Compose. It reads the image registry, and installs
// through the hosting platform's deploy link or the server's host setup. The
// app shows its state and offers updates to its admins through a small HTTP
// API. See README.md and docs/updates.md in meldgrove/meldgrove-master.
package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // WINDOW is read in the time zone TZ
)

//go:embed host-setup.sh
var hostSetup string

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "%s %s\n", time.Now().UTC().Format("2006-01-02T15:04:05Z"), fmt.Sprintf(format, args...))
}

const usage = `Usage: updater [command]
  (none)            watch the image and install updates; serve the API
  check             print the digest the image tag points to now
  status            print the running updater's status
  install VERSION   install an offered version now
  channel [stable|preview]
                    print the channel, or switch to it
  host-setup        print the setup script for a server without a platform
  healthcheck       exit 0 while the API answers
  version           print the updater's version
The host setup's deploy script runs these in the container, so that it never
writes into the volume itself:
  host-watch PROJECT, host-unwatch, host-request,
  host-report progress ID STAGE, host-report result ID VERSION RESULT [MESSAGE]`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	command := ""
	if len(args) > 0 {
		command = args[0]
	}
	switch command {
	case "host-setup":
		fmt.Print(hostSetup)
		return 0
	case "version":
		fmt.Println(orDefault(os.Getenv("APP_VERSION"), "development"))
		return 0
	case "-h", "--help", "help":
		fmt.Println(usage)
		return 0
	case "healthcheck":
		return healthcheck()
	case "host-watch", "host-unwatch", "host-request", "host-report":
		return hostCommand(orDefault(os.Getenv("TRIGGER_DIR"), "/trigger"), args)
	case "status":
		return client(http.MethodGet, "/v1/status", nil)
	case "install":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, usage)
			return 2
		}
		return client(http.MethodPost, "/v1/install", map[string]string{"version": args[1]})
	case "channel":
		if len(args) == 1 {
			return client(http.MethodGet, "/v1/status", nil, "channel")
		}
		return client(http.MethodPost, "/v1/channel", map[string]string{"channel": args[1]}, "channel")
	case "", "check":
	default:
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}

	cfg, err := LoadConfigFromEnvironment()
	if err != nil {
		logf("%s", err.Error())
		return 1
	}
	engine := NewEngine(cfg, logf)
	if command == "check" {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		digest, err := engine.registry.Digest(ctx, cfg.Tag)
		if err != nil {
			logf("%s", err.Error())
			return 1
		}
		fmt.Println(digest)
		return 0
	}
	return serve(cfg, engine)
}

func serve(cfg Config, engine *Engine) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	logf("Updater %s watches %s every %ds.", orDefault(cfg.Version, "(no version)"), cfg.Image, int(cfg.Interval.Seconds()))
	if cfg.Cooldown > 0 {
		logf("A new release installs once it has been out for %s.", cfg.CooldownText)
	}
	if cfg.Window != nil {
		logf("New releases install only between %s and %s (time zone %s).", cfg.Window.Text[:5], cfg.Window.Text[6:], cfg.TimeZone)
	}
	switch engine.mode() {
	case "platform":
		logf("The platform deploys: the updater calls its deploy link.")
	case "host":
		logf("The host deploys: it watches %s.", cfg.Trigger)
	default:
		logf("Automatic updates are off: DEPLOY_HOOK_URL is empty, and no host watches %s. The app still shows new releases.", cfg.Trigger)
	}
	if locked := engine.channelLocked(); locked == "" {
		logf("Versions are chosen by the channel set in the app, and the host writes them to %s.", cfg.VersionVariable)
	}

	var server *http.Server
	if cfg.Listen != "off" {
		if cfg.APIToken == "" {
			logf("The API takes requests without a token: anything that reaches %s on the network can install updates. Set API_TOKEN, and give the app the same token.", cfg.Listen)
		}
		listener, err := net.Listen("tcp", cfg.Listen)
		if err != nil {
			logf("The API cannot listen on %s: %v", cfg.Listen, err)
			return 1
		}
		server = &http.Server{Handler: engine.Handler(ctx), ReadHeaderTimeout: 10 * time.Second}
		go func() {
			if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logf("The API stopped: %v", err)
			}
		}()
	}
	engine.Run(ctx)
	if server != nil {
		shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}
	return 0
}

// local is the address of the running updater's API in this container.
func local() string {
	listen := orDefault(os.Getenv("LISTEN"), ":8080")
	host, port, err := net.SplitHostPort(listen)
	if err != nil || port == "" {
		return "http://127.0.0.1:8080"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

func healthcheck() int {
	if os.Getenv("LISTEN") == "off" {
		return 0
	}
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Get(local() + "/healthz")
	if err != nil {
		return 1
	}
	drain(response)
	if response.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

// client calls the running updater's API and prints the answer: the whole
// status, or one field of it.
func client(method, path string, body any, field ...string) int {
	var reader io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		reader = bytes.NewReader(data)
	}
	request, _ := http.NewRequest(method, local()+path, reader)
	request.Header.Set("Content-Type", "application/json")
	if token := apiToken(os.Getenv); token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := (&http.Client{Timeout: 90 * time.Second}).Do(request)
	if err != nil {
		fmt.Fprintf(os.Stderr, "updater: the updater does not answer at %s: %v\n", local(), err)
		return 1
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	var answer map[string]any
	if json.Unmarshal(data, &answer) != nil {
		fmt.Fprintf(os.Stderr, "updater: unexpected answer: %s\n", data)
		return 1
	}
	if message, ok := answer["error"].(string); ok {
		fmt.Fprintf(os.Stderr, "updater: %s\n", message)
		return 1
	}
	if len(field) == 1 {
		fmt.Println(answer[field[0]])
		return 0
	}
	pretty, _ := json.MarshalIndent(answer, "", "  ")
	fmt.Println(string(pretty))
	return 0
}

// hostCommand serves the deploy script of the host setup, which runs it with
// docker exec as the container's unprivileged user. The script never reads
// or writes the volume as root, so links placed there cannot redirect it.
func hostCommand(trigger string, args []string) int {
	path := func(name string) string { return filepath.Join(trigger, name) }
	write := func(name string, lines ...string) int {
		if err := writeAtomic(path(name), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "updater: %v\n", err)
			return 1
		}
		return 0
	}
	single := func(text string) string {
		return strings.Map(func(r rune) rune {
			if r < 32 || r == 127 {
				return ' '
			}
			return r
		}, text)
	}
	switch {
	case args[0] == "host-watch" && len(args) == 2 && projectPattern.MatchString(args[1]):
		_ = os.Remove(path("deploy"))
		return write("watched", "protocol=2", "project="+args[1])
	case args[0] == "host-unwatch" && len(args) == 1:
		for _, name := range []string{"watched", "deploy", "progress", "result"} {
			_ = os.Remove(path(name))
		}
		return 0
	case args[0] == "host-request" && len(args) == 1:
		// Prints and takes the request; a request that is no plain file is
		// dropped unread.
		info, err := os.Lstat(path("deploy"))
		if err != nil {
			return 0
		}
		defer os.Remove(path("deploy"))
		if !info.Mode().IsRegular() || info.Size() > 4096 {
			return 0
		}
		data, err := os.ReadFile(path("deploy"))
		if err != nil {
			return 1
		}
		fmt.Print(string(data))
		return 0
	case args[0] == "host-report" && len(args) == 4 && args[1] == "progress":
		return write("progress", "id="+single(args[2]), "stage="+single(args[3]))
	case args[0] == "host-report" && (len(args) == 5 || len(args) == 6) && args[1] == "result":
		message := ""
		if len(args) == 6 {
			message = single(args[5])
		}
		if len(message) > 400 {
			message = message[:400]
		}
		return write("result", "id="+single(args[2]), "version="+single(args[3]), "result="+single(args[4]),
			"message="+message, fmt.Sprintf("finished=%d", time.Now().Unix()))
	}
	fmt.Fprintln(os.Stderr, usage)
	return 2
}

var projectPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
