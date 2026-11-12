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
release ─► image tags move (1.2.3 → 1.2, latest)
updater: hourly, did the app's tag move? ─► yes ─► POST deploy link ─► platform redeploys
```

It needs no access to Docker, only to the registry and the deploy link. It works
with any image on any registry, and with any platform that redeploys on a `POST`
to a link: Coolify, Dokploy, Render and others.

## Choose what to install

The tag the app runs decides which releases install automatically:

| Tag | Installs automatically |
| --- | --- |
| `1.2` | patch releases of 1.2 (1.2.4, 1.2.5, …), which need no manual steps |
| `latest` | every release, including those with upgrade steps |
| `1.2.3` | nothing |

Meldgrove apps put manual upgrade steps only into minor and major releases, at the
top of their release notes. A minor version tag such as `1.2` is therefore safe
to update unattended. With `latest`, read each release's notes as it comes out
(Watch > Custom > Releases on GitHub), since the updater installs it before anyone
has taken its steps. A major version tag such as `1` gets minor releases too and
carries the same risk.

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
```

| Setting | Meaning |
| --- | --- |
| `IMAGE` | The image and tag the app runs, as in its `image:` line. |
| `DEPLOY_HOOK_URL` | The platform's deploy link. Empty: the updater does nothing. |
| `DEPLOY_HOOK_TOKEN` | Bearer token for the deploy link, if the platform needs one. |
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

It pulls the tags and replaces only the containers whose image changed.

## Check it

The updater's log says what it does:

```text
Updater 0.1.0 watches ghcr.io/meldgrove/logbuch:0.2 every 3600s.
Current image: sha256:…
ghcr.io/meldgrove/logbuch:0.2 points to a new image: sha256:…. Calling the deploy link.
The platform accepted the deployment.
```

`docker compose run --rm updater check` prints the digest the tag points to now,
or the reason it cannot read it.

## Good to know

- **Backups:** an unattended update runs database migrations without anyone
  watching. Schedule backups first, as the app's deployment docs describe.
- **What counts as current:** at start the updater takes the image the tag points
  to as the running one, since a deploy has just pulled it. If the tag moves while
  the updater is stopped, that release installs with the next one or a manual
  deploy.
- **Failures:** if the platform refuses the call, the updater tries again every
  interval and logs the answer. It does not check that the deployment succeeded;
  the platform's notifications do that.
- **Privileges:** the updater runs as `nobody` without the Docker socket. Its
  token can only start deployments.

## Releases

Maintainers release through Actions > Release > Run workflow, the same workflow
as every Meldgrove project: patch, minor or major from the latest `vX.Y.Z` tag,
an image for amd64 and arm64 as `X.Y.Z`, `X.Y`, from 1.0.0 also `X`, and
`latest`, and a GitHub Release. Patch for fixes, minor for anything new or
anything operators must act on, major only for really big changes. Apps refer to
`ghcr.io/meldgrove/updater:X.Y`, so patch releases reach them with their next
deploy.

## License

[Apache License 2.0](LICENSE)
