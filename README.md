# Meldgrove Updater

Automatic updates for self-hosted Meldgrove apps such as
[Logbuch](https://github.com/meldgrove/logbuch),
[Reimbursements](https://github.com/meldgrove/reimbursements) and
[CRM](https://github.com/meldgrove/crm). The updater runs as one more, optional
service in the app's `compose.yaml`. Once an hour it asks the image registry
whether the image tag the app runs points to a new image. When it does, the
updater calls the hosting platform's deploy link, and the platform pulls the new
image and restarts the app.

```text
release ─► image tags move (1.2.3 → 1.2, 1, latest)
updater: hourly, did the app's tag move? ─► yes ─► cooldown over, window open?
  ─► POST deploy link ─► platform redeploys
```

It needs no access to Docker, only to the registry and the deploy link. It works
with any image on any registry, and with any platform that redeploys on a `POST`
to a link: Coolify, Dokploy, Render and others.

## Choose what to install

The tag the app runs decides which releases install automatically:

| Tag | Installs automatically |
| --- | --- |
| `0.3` | below 1.0.0: every release of the 0.3 series (0.3.1, 0.3.2, …) |
| `1` | from 1.0.0: every release of version 1 (1.0.1, 1.1.0, …) |
| `latest` | every release, including those with upgrade steps |
| `1.2.3` | nothing |

Meldgrove apps start a new series when a release needs manual steps: the next
minor version below 1.0.0 (0.3 → 0.4), the next major version from 1.0.0
(1 → 2). Features and fixes come within the series. A series tag is therefore
safe to update unattended, and it stays current until such a release. From
1.0.0 a tag such as `1.2` is safe too, but only receives fixes.

When a release is out that the tag does not reach, the updater logs it once a
day. Read
the release notes of every release since yours, take their upgrade steps, and
change the tag. With `latest` the updater installs such a release before anyone
has taken its steps, so read each release's notes as it comes out (Watch >
Custom > Releases on GitHub).

## Switch it on

Meldgrove apps already contain the service; it does nothing until the deploy link
is set. For another app, add it to `compose.yaml`:

```yaml
services:
  updater:
    image: ghcr.io/meldgrove/updater:0.1
    pull_policy: always
    restart: unless-stopped
    environment:
      IMAGE: ghcr.io/example/app:${APP_VERSION_TAG}
      DEPLOY_HOOK_URL: ${UPDATE_DEPLOY_HOOK_URL:-}
      DEPLOY_HOOK_TOKEN: ${UPDATE_DEPLOY_HOOK_TOKEN:-}
      COOLDOWN: ${UPDATE_COOLDOWN:-3d}
      WINDOW: ${UPDATE_WINDOW:-}
      TZ: ${TZ:-UTC}
```

| Setting | Meaning |
| --- | --- |
| `IMAGE` | The image and tag the app runs, as in its `image:` line. |
| `DEPLOY_HOOK_URL` | The platform's deploy link. Empty: the updater does nothing. |
| `DEPLOY_HOOK_TOKEN` | Bearer token for the deploy link, if the platform needs one. |
| `COOLDOWN` | Install a new image only after it has been out this long: `3d`, `12h`, `30m`, or a number of seconds. Default `0`: at once. |
| `WINDOW` | Install only between two times of day, such as `02:00-05:00`, in the time zone `TZ`. Default: at any time. |
| `TZ` | Time zone of `WINDOW`, such as `Europe/Berlin`. Default UTC. |
| `REGISTRY_USERNAME`, `REGISTRY_PASSWORD` | Only for a private image: a user and a token that may read it. |
| `CHECK_INTERVAL` | Seconds between checks. Default 3600. |

The app's services need `pull_policy: always`, so that a redeploy downloads the
moved tag instead of reusing the old image.

### Coolify

1. Under Settings > Advanced, turn on API Access if it is off.
2. Under Keys & Tokens > API Tokens, create a token with only the `deploy`
   permission; clear `read`. Coolify shows it only once.
3. Open the app's resource, select Webhooks, and copy Deploy Webhook. It looks like
   `https://coolify.example.org/api/v1/deploy?uuid=<resource>&force=false`.
4. In the resource's environment variables, set `UPDATE_DEPLOY_HOOK_URL` to the
   webhook and `UPDATE_DEPLOY_HOOK_TOKEN` to the token. Deploy once by hand.

### Other platforms

Use the platform's deploy hook or redeploy webhook. Set `DEPLOY_HOOK_TOKEN` only
when the platform expects `Authorization: Bearer <token>`. Use an `https://` link:
the updater does not follow redirects, and the token travels with every call.

### Docker Compose without a platform

Without a deploy link, leave `UPDATE_DEPLOY_HOOK_URL` empty and let cron run
Docker Compose every hour instead:

```cron
0 * * * * cd /srv/app && docker compose up --detach --quiet-pull
```

It pulls the tags and replaces only the containers whose image changed. The
cooldown and the window do not apply to it.

## Check it

The updater's log says what it does:

```text
Updater 0.1.1 watches ghcr.io/meldgrove/logbuch:0.2 every 3600s.
A new image installs once it has been out for 3d.
Current image: sha256:…
ghcr.io/meldgrove/logbuch:0.2 points to a new image: sha256:…. It installs once it has been out for 3d.
Calling the deploy link.
The platform accepted the deployment.
Release 0.3.0 is out, and the tag 0.2 does not reach it. To install it, read the release notes of every release since yours, then change the tag.
```

`docker compose run --rm updater check` prints the digest the tag points to now,
or the reason it cannot read it.

## Good to know

- **Backups:** an unattended update runs database migrations without anyone
  watching. Schedule backups first, as the app's deployment docs describe.
- **Cooldown:** the time counts from the moment the updater first sees the new
  image. A newer image starts it again, so a release that is replaced within
  the cooldown never installs. Security fixes wait as well: deploy by hand to
  install a release at once. An installation without cooldown runs each
  release first and shows whether it is sound.
- **Window:** many platforms stop the app while they deploy. A window such as
  `02:00-05:00` keeps that out of working hours; `22:00-02:00` spans midnight.
- **What counts as installed:** a new container takes the image the tag points
  to as the installed one, since a deployment has just pulled it. The updater
  keeps what it knows over a restart of its container or of the server, when
  nothing was pulled: a release that came out meanwhile still installs.
- **Failures:** if the platform refuses the call, the updater tries again every
  interval and logs the answer. It does not check that the deployment succeeded;
  the platform's notifications do that.
- **Privileges:** the updater runs as `nobody` without the Docker socket. Its
  token can only start deployments.

## Releases

Maintainers release through Actions > Release > Run workflow, with the
[release workflow](https://github.com/meldgrove/.github#release-workflow) that
every Meldgrove project shares: patch, minor or major from the latest `vX.Y.Z`
tag, an image for amd64 and arm64 as `X.Y.Z`, `X.Y`, from 1.0.0 also `X`, and
`latest`, and a GitHub Release. Below 1.0.0, patch is for everything that
operators need not act on, features included, and minor only for a release
that operators must act on; label such a pull request `breaking`. Apps refer
to `ghcr.io/meldgrove/updater:0.1`, so patch releases reach them with their
next deploy.

## License

[Apache License 2.0](LICENSE)
