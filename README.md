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
notes), Sigstore's TUF repository ([Signatures](#signatures)), and the deploy
link or its volume. It is one Go program and runs as `nobody`.

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
| `API_TOKEN` | The token the app sends with every API request ([The app's update button](#the-apps-update-button)), such as the output of `openssl rand -base64 32`. Spaces around it do not count, as in the apps. Empty (or only spaces): the API takes requests from anything that reaches it. |

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
   version number, and the image's digest) and the image repository and
   `VERSION_VARIABLE` from the updater's own configuration;
2. pulls the image (and the updater's, and the
   [backup agent](https://github.com/meldgrove/backup-agent)'s when the app
   has the `backup-agent` service); when the request names the image whose
   [signature](#signatures) the updater checked, the pulled image must be
   that one, or nothing changes;
3. dumps the database ([Before each installation](#before-each-installation));
4. saves `.env` and writes the version to `VERSION_VARIABLE`;
5. runs `docker compose up --detach --wait`: migrations first, then the app,
   until every service is healthy (at most 15 minutes);
6. when that fails, puts `.env` back and starts the version before, and
   reports why to the updater, which shows it in the app and does not try
   that version again on its own;
7. removes older images of the app, keeping the new version and the one
   before.

- Each installation: `journalctl -u meldgrove-updater-<project>.service`.
- It installs images, not files: a changed `compose.yaml` reaches the server
  when you update the folder (`git pull`).
- A private image: sign in as root (`docker login ghcr.io`); the unit pulls
  as root.
- Run the setup again after an updater release that changes it; it replaces
  the deploy script.
- Switch it off: `sh updater-setup.sh remove`.

#### Before each installation

Before it changes anything, the deploy script dumps the app's database with
the image of the [backup agent](https://github.com/meldgrove/backup-agent) it
has just pulled (`backup-agent dump-database`), when the app has the
`backup-agent` service. When that pull fails, the image on the server dumps. Only servers do this; on a platform such as Coolify,
the platform deploys.

- The dump is
  `/var/backups/meldgrove-updater/<project>/<project>-<version before>-<UTC time>.dump`,
  readable only by root. The two newest stay; older ones are deleted.
- When the dump fails, or Docker Compose cannot list the app's services,
  nothing is installed: the version before keeps running, and the app says
  why.
- When the installation fails or goes back to the version before, the
  message in the app starts with the dump's path. Going back restores
  `.env` and the images, not the database: migrations that ran stay. The
  journal names each dump.
- `UPDATE_BACKUP=off` in `.env` installs without a dump. Apps without the
  `backup-agent` service install without one too.
- Host setups written by updater 0.2.3 or older take no dump. To get it,
  write and run the setup again, as root in the app's folder:

  ```sh
  docker compose run --rm -T --pull always updater host-setup > updater-setup.sh
  sh updater-setup.sh
  ```

To restore a dump, as root in the app's folder:

1. Stop the app's services, but not its database, such as
   `docker compose stop web worker`.
2. Restore the dump with the agent image's `pg_restore` and `psql`:

   ```sh
   docker compose run --rm -T --no-deps --entrypoint sh backup-agent -c '
     pg_restore --clean --if-exists --no-owner --file=/tmp/restore.sql &&
     psql --dbname="$DATABASE_URL" --single-transaction --set=ON_ERROR_STOP=1 --quiet --output=/dev/null \
       --command="SET client_min_messages = warning; DROP SCHEMA public CASCADE;
         CREATE SCHEMA public AUTHORIZATION pg_database_owner; GRANT USAGE ON SCHEMA public TO PUBLIC;" \
       --file=/tmp/restore.sql' < /var/backups/meldgrove-updater/<project>/<dump>
   ```

   It empties the schema `public`, so tables that the newer version added
   go as well, then restores the dump; all in one transaction. When anything
   fails, the database stays as it was. It needs PostgreSQL 15 or newer and
   a `DATABASE_URL` whose user owns the database.
3. Start the version that wrote the dump, the one in its name:
   `docker compose up -d`. After a rollback, `.env` names it already;
   otherwise set the version variable in `.env` back to it first, or the
   newer version migrates the database again.

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

## Managing updates yourself

Without a host setup and without a deploy link, the updater's mode is `off`:
nothing installs, and the app still shows new releases to its admins.

- Pin an exact version in `.env`, such as `HELPDESK_VERSION=0.3.1`, so that
  nothing changes until you change it.
- To update by hand: read the release notes and dump the database; go on
  only when the dump succeeds. Then set the new version in `.env` and run
  `docker compose up -d --pull always`.

  ```sh
  (umask 077 && rm -f before.dump && docker compose run --rm -T --no-deps backup-agent dump-database > before.dump)
  ```
- A tool such as Renovate or Dependabot in your own infrastructure
  repository can propose each new version as a pull request.
- Read the release notes before a new series (0.3 → 0.4, 1 → 2): it needs
  upgrade steps.

## Signatures

Before it installs a version, the updater checks that Meldgrove's release
workflow built its image. The
[release workflow](https://github.com/meldgrove/.github#release-workflow)
that every Meldgrove repository shares signs a provenance attestation for
each image of a public repository (SLSA provenance, signed in GitHub Actions
through Sigstore's public-good instance, stored next to the image on GHCR).
The updater verifies it with [sigstore-go](https://github.com/sigstore/sigstore-go),
with the policy of
`gh attestation verify oci://IMAGE:VERSION --repo meldgrove/<app> --signer-repo meldgrove/.github`:

- the attestation is SLSA provenance for the image's digest;
- its certificate comes from GitHub Actions
  (`https://token.actions.githubusercontent.com`) for the workflow
  `https://github.com/meldgrove/.github/.github/workflows/release.yml` (any
  ref), and names the same repository as the installed image, such as
  `https://github.com/meldgrove/handbook`;
- the image at that digest names the version that is installed
  (`org.opencontainers.image.version`): the signature covers the image, not
  its tag, so the tag `0.2.5` pointing to the signed image of `0.2.1` is
  refused.

**When it applies:** no setting. When the installed image carries a valid
attestation of the release workflow, every version after it must carry one
for the same repository; `signature` in the Status is `"required"`, and the
updater keeps that in its volume, so an attestation removed later does not
switch the check off. When the installed image carries none (an app whose
repository is private, such as CRM; an own build; releases signed before
the shared release workflow), nothing is checked, and `signature` is
`"not-signed"`.

**When an update does not pass:**

- Its signature does not verify, comes from another workflow or another
  repository, or its image names another version: it is not installed and
  counts as failed, like a failed installation. It does not install again
  automatically; a newer version does. The message says why, such as
  `0.2.5 was not installed: its signature is not from Meldgrove's release
  workflow for meldgrove/handbook (…). Ask the maintainers.`
- Its image carries no attestation: the release workflow publishes an image
  before it attests it, so for up to a minute a new version has none.
  Nothing installs, the Status `error` says why, and the updater looks again
  within a minute.
- The check cannot run (registry, Sigstore's TUF repository, a bundle format
  this updater does not know): nothing installs, the Status `error` says
  why, and the next check tries again. The updater never installs unchecked
  because a lookup failed.

**What it reads:** the attestation from the registry, with the same
sign-in (the referrers API, or on GHCR the tag `sha256-<hex>`), and
Sigstore's trusted root through TUF from `https://tuf-repo-cdn.sigstore.dev`,
starting from the root of trust built into sigstore-go. The TUF metadata is
cached in the volume (`/trigger/sigstore`) and read again once a day; when
the TUF repository does not answer, the cache serves while it is valid
(about a week). A server whose outgoing connections are limited needs
`tuf-repo-cdn.sigstore.dev` besides the registry.

**On a server,** the request names the digest whose signature was checked
(`digest=sha256:…`), and the deploy script installs only that image: when
the pulled image is another one, the installation fails with "its tag moved
after the check" before it dumps the database or changes `.env`. Servers
get this when their host setup is written again from updater 0.2.5 or later
(as root in the app's folder):

```sh
docker compose run --rm -T --pull always updater host-setup > updater-setup.sh
sh updater-setup.sh
```

Deploy scripts of older host setups ignore the digest and pull the tag. On
a platform such as Coolify, and with a host setup that follows the tag
(updater 0.1, or no `VERSION_VARIABLE`), the platform or server pulls the
tag itself: the updater checks the image the tag points to right before it
calls the deploy link or asks the server, and the tag could still move in
between.

**What it protects against:** an image tag overwritten or added on GHCR
outside the release workflow, for example with a leaked token that may
write packages. **What it does not:** a compromised release workflow or
repository (the workflow signs what it builds); the moment between the check
and the pull on a platform; an installed image whose attestation was
removed before updater 0.2.5 first looked at it.

**Way out, for the server's operator only:** to install one version without
the check, run in the app's folder

```sh
docker compose exec updater updater install --unsigned 0.2.5
```

It counts for that version, once. The API cannot do this, also not without
`API_TOKEN`: only a shell on the server can. When the check wrongly refuses
every version (a fault of the updater), update the updater itself; the
server pulls the updater's image only during an installation, so an
updater that installs nothing would otherwise stay:

```sh
docker compose pull updater && docker compose up -d updater
```

**Check one image** with the same policy; exit code 0 when it passes:

```sh
docker run --rm ghcr.io/meldgrove/updater:0.2 verify-image ghcr.io/meldgrove/handbook:0.2.3 --repo meldgrove/handbook
# signed: ghcr.io/meldgrove/handbook:0.2.3 (sha256:22f0…) is 0.2.3, signed by Meldgrove's release workflow for meldgrove/handbook (…/release.yml@b021c42…).
```

`IMAGE@sha256:…` checks a digest; without `--repo`, the line names the
repository. A tag that is a version must point to the signed image of that
version. `REGISTRY_USERNAME` and `REGISTRY_PASSWORD` sign in to a private
registry.

## Your own build

A fork that publishes its own images can use the updater unchanged:

- `IMAGE` points at its registry, such as `ghcr.io/example/app:${APP_VERSION}`
  (HTTPS; plain HTTP only on `localhost`). `REGISTRY_USERNAME` and
  `REGISTRY_PASSWORD` sign in to a private registry.
- Versions are image tags `X.Y.Z`, and `X.Y.Z-preview.N` for previews. Other
  tags are no versions; `IMAGE` may follow one, such as `0.3` or `latest`.
- The image's OCI annotations (on the image index, or as labels of the
  image) describe a version: `org.opencontainers.image.version` (the version
  behind a tag such as `0.3` or `latest`), `.revision` (the commit),
  `.created` (the date the app shows, unless a GitHub Release has one) and
  `.source` (the repository).
- When `.source` is a GitHub repository, the release notes are its GitHub
  Release `vX.Y.Z`, and a preview lists the changes since the installed
  version. `SOURCE` names another repository; `GITHUB_TOKEN` reads a private
  one.
- Its images carry no signature of Meldgrove's release workflow, so the
  updater checks none ([Signatures](#signatures)).

## The app's update button

Apps reach the updater at `http://updater:8080` on the Compose network. With
`API_TOKEN` set, every request but `GET /healthz` needs it as
`Authorization: Bearer <token>`; others get `401`. Give the app the same
token. Without `API_TOKEN`, the API takes requests from anything that
reaches it, and the updater says so in its log. Never publish its port. Only
the app's admins see the update button, and the app checks that before it
calls the API. The commands `updater status`, `install` and `channel` in the
container send the token themselves. `apiToken` in the Status says whether
the API requires the token, so the app can warn its admins while it does
not.

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
                stage: "requested" | "pulling" | "backing-up" | "starting" | "rolling-back" | "deploying" } | null;
  last: { version: string; result: "installed" | "failed" | "rolled-back" | "accepted";
          message: string | null; finishedAt: string } | null;
  checking: boolean;
  checkedAt: string | null;
  error: string | null;                   // why the last check failed
  automatic: boolean;                     // false while off
  cooldown: string | null;                // COOLDOWN of Stable releases, e.g. "3d"
  window: string | null;                  // e.g. "02:00-05:00"
  timezone: string;
  apiToken: boolean;                      // API_TOKEN is set: the API requires it
  signature: "required" | "not-signed" | null; // updates must be signed (since 0.2.5); null: not known yet
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
- `docker compose run --rm updater verify-image IMAGE` checks an image's
  signature ([Signatures](#signatures)).

## Good to know

- **Backups:** an unattended update runs database migrations without anyone
  watching. Schedule backups first, as the app's deployment docs describe.
  Going back to the version before after a failed start does not undo
  migrations that succeeded; on a server, the dump taken before the
  installation does ([Before each installation](#before-each-installation)).
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
go test ./...            # unit tests (Go 1.27; or in docker run golang:1.27-alpine); signatures from testdata/
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
