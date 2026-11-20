# Meldgrove Updater

Automatic updates for self-hosted Meldgrove apps such as
[Logbook](https://github.com/meldgrove/logbook),
[Reimbursements](https://github.com/meldgrove/reimbursements) and
[Library](https://github.com/meldgrove/library). The updater runs as one more
service in the app's `compose.yaml`. Once an hour it asks the image registry
for new releases (or, on the Preview channel, previews) and installs them: on
a Linux server, such as a VPS with Debian or Ubuntu, through a systemd unit
that the server runs; on a platform such as Coolify, through its deploy link.
The app shows its state to its admins and lets them check, install and choose
the channel, through a small API of the updater.

```text
release ─► ghcr.io/meldgrove/app:0.2.1          preview ─► ghcr.io/meldgrove/app:0.2.2-preview.3
updater: hourly, which versions are newer? ─► cooldown over, window open?
  ─► Server:  request in the volume /trigger ─► systemd unit: 0.2.1 into .env, pull,
              docker compose up --wait, back to the version before if it is not healthy
  ─► Coolify: POST deploy link ─► Coolify redeploys the image tag
app (admins): update button, Updates settings ─► updater API on the Compose network
```

The updater needs no access to Docker: only to the registry, GitHub (release
notes), and the deploy link or its volume. It is one Go program without
dependencies and runs as `nobody`.

## Channels

| Channel | Installs automatically |
| --- | --- |
| **Stable** (default) | Each release of the installed series, once it has been out for the cooldown (apps default to 3 days). |
| **Preview** | Each preview and release, within the hour, without cooldown. |

A preview is every commit on `main` that passed CI, published as
`X.Y.Z-preview.N`: N commits past the release before X.Y.Z. Previews bring new
features first, and now and then a new bug. Switching back to Stable installs
nothing and never goes back to an older version: the app keeps the preview
until a release is newer.

Meldgrove apps start a new series when a release needs manual steps: the next
minor version below 1.0.0 (0.3 → 0.4), the next major version from 1.0.0
(1 → 2). Features and fixes come within the series. On Stable, a release of a
new series is shown in the app as needing upgrade steps and never installs on
its own: take its steps, then install it from the app (or with
`docker compose exec updater updater install 0.4.0`).

Admins choose the channel in the app's admin settings, or on the server:
`docker compose exec updater updater channel preview`.

## Modes

| Mode | When | Who picks the version |
| --- | --- | --- |
| Server (`host`) | the [host setup](#debian-and-similar-systems) watches the volume, and `VERSION_VARIABLE` is set | the updater, by channel; the server writes it to `.env` |
| Platform | `DEPLOY_HOOK_URL` is set ([Coolify](#coolify)) | the image tag set on the platform |
| Off | neither | nothing installs; the app still shows new releases |

## Settings

Meldgrove apps already contain the service. For another app, add it to
`compose.yaml`:

```yaml
x-image: &image ghcr.io/example/app:${APP_VERSION_TAG:?}

services:
  updater:
    image: ghcr.io/meldgrove/updater:0.2
    restart: unless-stopped
    environment:
      IMAGE: *image
      VERSION_VARIABLE: APP_VERSION_TAG
      # The app sends the same token (its own setting, e.g. UPDATER_TOKEN).
      API_TOKEN: ${UPDATER_TOKEN:-}
      COOLDOWN: ${UPDATE_COOLDOWN:-3d}
      WINDOW: ${UPDATE_WINDOW:-}
      TZ: ${TZ:-UTC}
    volumes:
      - updater:/trigger

volumes:
  updater:
```

On a platform with a deploy link, such as Coolify, also set `DEPLOY_HOOK_URL`
and `DEPLOY_HOOK_TOKEN` ([Coolify](#coolify)).

| Setting | Meaning |
| --- | --- |
| `IMAGE` | The image and tag the app runs, as in its `image:` line. |
| `VERSION_VARIABLE` | The variable in `.env` that holds the app's version. On a server, the host setup writes each version it installs there. Without it, the updater follows the image tag, and the channel cannot be chosen in the app. |
| `DEPLOY_HOOK_URL` | The platform's deploy link. Empty: the server installs, if its host setup watches `/trigger`; otherwise nothing installs. |
| `DEPLOY_HOOK_TOKEN` | Bearer token for the deploy link, if the platform needs one. |
| `COOLDOWN` | A Stable release installs once it has been out this long (from its GitHub release date, or from when the updater first saw it): `3d`, `12h`, `30m`, or seconds. Default `0`. |
| `WINDOW` | Install automatically only between two times of day, such as `02:00-05:00`, in the time zone `TZ`. Installs from the app ignore it. |
| `TZ` | Time zone of `WINDOW`, such as `Europe/Berlin`. Default UTC. |
| `REGISTRY_USERNAME`, `REGISTRY_PASSWORD` | Only for a private image: a user and a token that may read it. |
| `GITHUB_TOKEN` | Only for release notes of a private repository. |
| `CHECK_INTERVAL` | Seconds between checks. Default 3600. |
| `LISTEN` | Address of the API, default `:8080`; `off` switches it off. |
| `API_TOKEN` | The token the app sends with every API request ([The app's update button](#the-apps-update-button)), such as the output of `openssl rand -base64 32`. Empty: the API takes requests from anything that reaches it. |

### Debian and similar systems

On a server that runs the app with Docker Compose and systemd, such as Debian
or Ubuntu, the server installs the releases itself. Keep
`UPDATE_DEPLOY_HOOK_URL` empty and the settings in `.env` next to
`compose.yaml`. Then, as root in the app's folder:

```sh
docker compose up --detach --pull always
docker compose run --rm -T --pull always updater host-setup > updater-setup.sh
sh updater-setup.sh
```

The second line writes the setup script from the updater's image; read it
before you run it as root. It installs `meldgrove-updater-<project>.path` and
`.service` in `/etc/systemd/system` and their deploy script
`/usr/local/lib/meldgrove-updater/deploy-2` (`<project>` is the Docker Compose
project, by default the folder's name; apps on the server that still run
updater 0.1 keep their `deploy`). The deploy script runs as root but never
touches the updater's volume: it takes the request and reports back with
`docker exec`, as the updater's own user. For each installation it:

1. takes the version from the updater's request (it accepts nothing but a
   version number) and the image repository and `VERSION_VARIABLE` from the
   updater's own configuration;
2. pulls the image (and the updater's, and the
   [backup agent](https://github.com/meldgrove/backup-agent)'s when the app
   has the `backup-agent` service), saves `.env` and writes the version to
   `VERSION_VARIABLE`;
3. runs `docker compose up --detach --wait`: migrations first, then the app,
   until every service is healthy (at most 15 minutes);
4. when that fails, puts `.env` back and starts the version before, and
   reports why to the updater, which shows it in the app and does not try
   that version again on its own;
5. removes older images of the app, keeping the new version and the one
   before.

- Each installation: `journalctl -u meldgrove-updater-<project>.service`.
- It installs images, not files: a changed `compose.yaml` reaches the server
  when you update the folder (`git pull`).
- A private image: sign in as root (`docker login ghcr.io`); the unit pulls
  as root.
- Run the setup again after an updater release that changes it; it replaces
  the deploy script.
- Switch it off: `sh updater-setup.sh remove`.

**From updater 0.1:** a host setup of 0.1 keeps working (the updater follows
the image tag), and the app says that the channel needs the new setup. To get
it: update the app's folder to a release with updater 0.2 (`git pull`), run
`docker compose up -d`, and run the three lines above again.

### Coolify

Meldgrove apps are made for a server; on Coolify they run from their
`compose.coolify.yaml`, which adds `DEPLOY_HOOK_URL` and `DEPLOY_HOOK_TOKEN`
(from `UPDATE_DEPLOY_HOOK_URL` and `UPDATE_DEPLOY_HOOK_TOKEN`) to the
`updater` service. Coolify pulls the images whenever it deploys. The image
tag set there decides which updates install: a series such as `0.3` (Stable
without upgrade steps), `latest` (every release), or `preview` (every
preview).

1. Under Settings > Advanced, turn on API Access if it is off.
2. Under Keys & Tokens > API Tokens, create a token with only the `deploy`
   permission; clear `read`. Coolify shows it only once.
3. Open the app's resource, select Webhooks, and copy Deploy Webhook. It looks
   like `https://coolify.example.org/api/v1/deploy?uuid=<resource>&force=false`.
4. In the resource's environment variables, set `UPDATE_DEPLOY_HOOK_URL` to
   the webhook and `UPDATE_DEPLOY_HOOK_TOKEN` to the token. Deploy once by
   hand.

Another platform that redeploys on a `POST` to a link works the same way, if
its deployments pull the images again. Set `DEPLOY_HOOK_TOKEN` only when it
expects `Authorization: Bearer <token>`, and use an `https://` link: the
updater does not follow redirects, and the token travels with every call.

## The app's update button

Apps reach the updater at `http://updater:8080` on the Compose network. With
`API_TOKEN` set, every request but `GET /healthz` needs it as
`Authorization: Bearer <token>`; others get `401`. Give the app the same
token. Without `API_TOKEN`, the API takes requests from anything that
reaches it, and the updater says so in its log. Never publish its port. Only
the app's admins see the update button, and the app checks that before it
calls the API. The commands `updater status`, `install` and `channel` in the
container send the token themselves.

| Request | Answer |
| --- | --- |
| `GET /v1/status` | `200` Status (no network calls) |
| `POST /v1/check` | checks now (up to 30 s), then `200` Status |
| `POST /v1/install` `{"version": "0.2.1"}` | `202` Status (within 8 s, also while a deploy link still answers); `409` when the version is not offered, an installation runs, or nothing can install |
| `POST /v1/channel` `{"channel": "stable"}` | saves and checks, then `200` Status; `409` when the platform's tag decides |
| `GET /healthz` | `200 ok` |

Errors are `{"error": "<sentence for admins>"}`. The Status:

```ts
type Status = {
  updater: string;                        // e.g. "0.2.0"
  mode: "host" | "platform" | "off";
  channel: "stable" | "preview";
  channelLocked: string | null;           // why the channel cannot be changed here
  installed: string | null;               // e.g. "0.2.0", "0.2.1-preview.3"
  available: Update | null;               // the update this installation gets
  attention: Update | null;               // Stable: a new series, needs upgrade steps
  installing: { version: string; startedAt: string; manual: boolean;
                stage: "requested" | "pulling" | "starting" | "rolling-back" | "deploying" } | null;
  last: { version: string; result: "installed" | "failed" | "rolled-back" | "accepted";
          message: string | null; finishedAt: string } | null;
  checking: boolean;
  checkedAt: string | null;
  error: string | null;                   // why the last check failed
  automatic: boolean;                     // false while off
  cooldown: string | null;                // COOLDOWN of Stable releases, e.g. "3d"
  window: string | null;                  // e.g. "02:00-05:00"
  timezone: string;
};

type Update = {
  version: string;                        // "sha256:<12 hex>" for an image without version
  preview: boolean;
  publishedAt: string | null;
  url: string | null;                     // release notes, or the changes of a preview
  notes: string[];                        // change titles, newest first, at most 15
  changes: number;
  installsAt: string | null;              // automatic install; null: not automatically
  failed: string | null;                  // why installing it failed before
};
```

The updater keeps its state in its volume (`/trigger/state.json`), so the
channel, a running installation and earlier failures survive the new
containers of each deployment.

## Check it

The updater's log says what it does:

```text
Updater 0.2.0 watches ghcr.io/meldgrove/logbook:0.2.3 every 3600s.
A new release installs once it has been out for 3d.
The host deploys: it watches /trigger.
Versions are chosen by the channel set in the app, and the host writes them to LOGBOOK_VERSION.
Asking the host to install 0.2.4.
0.2.4 is installed.
```

- `docker compose exec updater updater status` prints the status as the app
  sees it.
- `docker compose run --rm updater check` prints the digest the tag points to
  now, or why it cannot read it.

## Good to know

- **Backups:** an unattended update runs database migrations without anyone
  watching. Schedule backups first, as the app's deployment docs describe.
  Going back to the version before after a failed start does not undo
  migrations that succeeded.
- **Cooldown:** security fixes wait as well; install them from the app at
  once. An installation without cooldown runs each release first.
- **Window:** the app is unavailable while it restarts. A window such as
  `02:00-05:00` keeps automatic installs out of working hours; `22:00-02:00`
  spans midnight.
- **Failures:** a version that did not install is not installed again
  automatically; a newer one is. If no server picks up a request within 10
  minutes, the updater takes it back and asks again at the next check. With
  a platform, the updater only knows that the deploy link accepted the call;
  the platform's notifications report failed deployments.
- **Privileges:** the updater runs as `nobody` without the Docker socket. Its
  token can only start deployments. On a server, the systemd unit runs the
  deploy script as root; the updater can only ask it to install a version of
  the app's own image.

## Development

```sh
go test ./...            # unit tests (Go 1.27; or in docker run golang:1.27-alpine)
test/run.sh              # end to end against a local registry (Docker)
sudo test/host.sh        # the host setup with systemd: installs, rollbacks, channels
```

## Releases

Maintainers release through Actions > Release > Run workflow, with the
[release workflow](https://github.com/meldgrove/.github#release-workflow) that
every Meldgrove project shares: patch, minor or major from the latest `vX.Y.Z`
tag, an image for amd64 and arm64 as `X.Y.Z`, `X.Y`, from 1.0.0 also `X`, and
`latest`, and a GitHub Release. Below 1.0.0, patch is for everything that
operators need not act on, features included, and minor only for a release
that operators must act on; label such a pull request `breaking`. Apps refer
to `ghcr.io/meldgrove/updater:0.2`, so patch releases reach them with their
next deploy.

## License

[Apache License 2.0](LICENSE)
