#!/usr/bin/env bash
# End-to-end test of the host setup (host-setup.sh) on a server with systemd,
# Docker and the Compose plugin, such as Debian or Ubuntu. It installs and
# removes systemd units, so it runs as root or with sudo; CI runs it on a
# GitHub runner. Usage: test/host.sh [image]   (without an image, it builds
# updater:test)
set -euo pipefail
cd "$(dirname "$0")/.."

image=${1:-}
if [ -z "$image" ]; then
  docker build --quiet --tag updater:test . > /dev/null
  image=updater:test
fi
registry=127.0.0.1:5557
app=localhost:5557/app
project=updater-host-test
unit=meldgrove-updater-$project
dir=$(mktemp -d)
# The second app: versions chosen by channel, pinned in .env (protocol 2).
pinned=updater-host-pinned
pinned_unit=meldgrove-updater-$pinned
pinned_dir=$(mktemp -d)

as_root() {
  if [ "$(id -u)" = 0 ]; then "$@"; else sudo "$@"; fi
}
compose() { docker compose --project-directory "$dir" "$@"; }
cleanup() {
  (cd "$dir" && as_root sh updater-setup.sh remove > /dev/null 2>&1) || true
  compose down --volumes --timeout 0 > /dev/null 2>&1 || true
  (cd "$pinned_dir" && as_root sh updater-setup.sh remove > /dev/null 2>&1) || true
  docker compose --project-directory "$pinned_dir" down --volumes --timeout 0 > /dev/null 2>&1 || true
  rm -rf "$pinned_dir"
  docker rm --force updater-host-test-registry > /dev/null 2>&1 || true
  rm -rf "$dir"
}
trap cleanup EXIT

fail() {
  echo "FAIL: $*"
  compose logs updater 2>&1 | tail -n 20 || true
  as_root journalctl --unit "$unit.service" --no-pager 2>&1 | tail -n 20 || true
  docker compose --project-directory "$pinned_dir" logs updater 2>&1 | tail -n 30 || true
  as_root journalctl --unit "$pinned_unit.service" --no-pager 2>&1 | tail -n 40 || true
  exit 1
}
pass() { echo "ok: $*"; }
eventually() {
  local deadline=$((SECONDS + $1))
  shift
  until "$@"; do
    [ "$SECONDS" -lt "$deadline" ] || return 1
    sleep 0.5
  done
}
logged() { compose logs updater 2>&1 | grep "$1" > /dev/null; }
# revision <service>: the test revision of the image that the service's container runs.
revision() {
  docker inspect --format '{{index .Config.Labels "test.revision"}}' "$(compose ps --all --quiet "$1")" 2> /dev/null
}
revision_is() { [ "$(revision "$2")" = "$1" ]; }
# publish <n>: push a new app image under the tag 1.0.
publish() {
  printf 'FROM %s\nLABEL test.revision=%s\nENTRYPOINT ["sleep"]\nCMD ["3600"]\n' "$image" "$1" |
    docker build --quiet --tag "$app:1.0" - > /dev/null
  docker push --quiet "$app:1.0" > /dev/null
}

docker run --detach --name updater-host-test-registry --publish "$registry:5000" \
  registry:3.1.2@sha256:82f6ff4a7f982960c8aa51617fea3aabc485df6e55cce80d22deb1de36f1bc3c > /dev/null
eventually 30 curl --silent --fail "http://$registry/v2/" > /dev/null || fail "the registry did not start"
publish 1

# An app as Meldgrove apps run it: a migration job, the app that waits for it,
# and the updater with its volume.
cat > "$dir/compose.yaml" << EOF
services:
  migrate:
    image: $app:\${APP_TAG:?}
    command: ["0"]
    restart: "no"
  app:
    image: $app:\${APP_TAG:?}
    restart: unless-stopped
    depends_on:
      migrate:
        condition: service_completed_successfully
  updater:
    image: $image
    restart: unless-stopped
    network_mode: host
    environment:
      IMAGE: $app:\${APP_TAG:?}
      DEPLOY_HOOK_URL: \${UPDATE_DEPLOY_HOOK_URL:-}
      CHECK_INTERVAL: 1
      LISTEN: 127.0.0.1:18091
    volumes:
      - updater:/trigger
volumes:
  updater:
EOF
printf 'COMPOSE_PROJECT_NAME=%s\nAPP_TAG=1.0\n' "$project" > "$dir/.env"
cd "$dir"
compose up --detach --quiet-pull 2> /dev/null
eventually 15 logged "no host watches /trigger" || fail "the updater did not say that it is off"
pass "off without a deploy link or a host that watches"

docker compose run --rm -T updater host-setup > updater-setup.sh
head -n 1 updater-setup.sh | grep -q '^#!/bin/sh' || fail "host-setup printed no script"
pass "host-setup prints the setup script"

if [ "$(id -u)" != 0 ] && sh updater-setup.sh > /dev/null 2>&1; then
  fail "the setup ran without root"
fi
(cd / && ! as_root sh "$dir/updater-setup.sh" > /dev/null 2>&1) || fail "the setup ran outside the app's folder"
pass "the setup needs root and the app's folder"

as_root sh updater-setup.sh > setup.log || fail "the setup failed: $(cat setup.log)"
grep -q "Automatic updates of $project are on" setup.log || fail "the setup did not confirm: $(cat setup.log)"
systemctl is-active --quiet "$unit.path" || fail "$unit.path is not active"
eventually 15 logged "The host deploys: it watches /trigger" || fail "the updater did not switch to the host"
eventually 15 logged "Current image" || fail "the updater did not read the current image"
pass "the setup installs the systemd units, and the updater hands deployments to the host"

publish 2
eventually 60 revision_is 2 app || fail "the host did not install the new image"
revision_is 2 migrate || fail "the migration job did not run with the new image"
[ "$(docker inspect --format '{{.State.ExitCode}}' "$(compose ps --all --quiet migrate)")" = 0 ] || fail "the migration job failed"
logged "Asking the host to deploy" || fail "the updater did not ask the host"
eventually 15 systemctl is-active --quiet "$unit.path" || fail "$unit.path is not active after the deployment"
# grep without -q reads all input: with pipefail, -q can fail journalctl on SIGPIPE.
journal_has() { as_root journalctl --unit "$1" --no-pager | grep "$2" > /dev/null; }
eventually 15 journal_has "$unit.service" "Installing the newest image of $app:1.0" ||
  fail "the journal does not show the installation"
pass "a moved tag installs: migration first, then the app"

publish 3
eventually 60 revision_is 3 app || fail "the host did not install a second new image"
pass "the next release installs as well"

as_root sh updater-setup.sh remove > remove.log || fail "remove failed: $(cat remove.log)"
grep -q "Automatic updates of $project are off" remove.log || fail "remove did not confirm"
[ ! -e "/etc/systemd/system/$unit.path" ] || fail "remove left $unit.path"
[ ! -e /usr/local/lib/meldgrove-updater ] || fail "remove left the deploy script"
eventually 15 logged "no host watches /trigger" || fail "the updater did not switch off"
publish 4
sleep 5
revision_is 3 app || fail "installed although the setup was removed"
pass "remove switches automatic updates off"

as_root sh updater-setup.sh > /dev/null 2>&1 || fail "the second setup failed"
compose down > /dev/null 2>&1
as_root sh updater-setup.sh remove > remove.log 2>&1 || fail "remove after docker compose down failed: $(cat remove.log)"
[ ! -e "/etc/systemd/system/$unit.path" ] || fail "remove after docker compose down left $unit.path"
pass "remove works after docker compose down"

# Protocol 2: the updater picks the version and the host pins it in .env,
# waits until the app is healthy, and goes back when it is not.
# version <v> [unhealthy] [migration exit code]: push an app release.
version() {
  # shellcheck disable=SC2016 # the entrypoint's shell expands $0, not this one
  printf 'FROM %s\nUSER 0\nLABEL test.revision=%s\nENV MIGRATE_EXIT=%s\nRUN %s\nENTRYPOINT ["sh", "-c", "if [ \\"$0\\" = migrate ]; then exit $MIGRATE_EXIT; fi; exec sleep 3600"]\nHEALTHCHECK --interval=1s --timeout=2s --retries=2 --start-period=1s CMD test -e /healthy\n' \
    "$image" "$1" "${3:-0}" "$([ "${2:-}" = unhealthy ] && echo true || echo 'touch /healthy')" |
    docker build --quiet --tag "$app:$1" - > /dev/null
  docker push --quiet "$app:$1" > /dev/null
}
pinned_compose() { docker compose --project-directory "$pinned_dir" "$@"; }
pinned_revision() {
  docker inspect --format '{{index .Config.Labels "test.revision"}}' "$(pinned_compose ps --all --quiet app)" 2> /dev/null
}
pinned_revision_is() { [ "$(pinned_revision)" = "$1" ]; }
env_tag() { sed -n 's/^APP_TAG=//p' "$pinned_dir/.env"; }
env_tag_is() { [ "$(env_tag)" = "$1" ]; }
status() { pinned_compose exec -T updater updater status; }
status_has() { status 2> /dev/null | jq -e "$1" > /dev/null; }
pinned_logged() { pinned_compose logs updater 2>&1 | grep "$1" > /dev/null; }

version 1.0.0
cat > "$pinned_dir/compose.yaml" << EOF
services:
  migrate:
    image: $app:\${APP_TAG:?}
    command: ["migrate"]
    restart: "no"
  app:
    image: $app:\${APP_TAG:?}
    command: ["app"]
    restart: unless-stopped
    depends_on:
      migrate:
        condition: service_completed_successfully
  updater:
    image: $image
    restart: unless-stopped
    network_mode: host
    environment:
      IMAGE: $app:\${APP_TAG:?}
      VERSION_VARIABLE: APP_TAG
      CHECK_INTERVAL: 1
      LISTEN: 127.0.0.1:18092
    volumes:
      - updater:/trigger
volumes:
  updater:
EOF
printf '# The app of the host test\nCOMPOSE_PROJECT_NAME=%s\nAPP_TAG=1.0.0\nOTHER=kept\n' "$pinned" > "$pinned_dir/.env"
chmod 0640 "$pinned_dir/.env"
cd "$pinned_dir"
pinned_compose up --detach --wait --quiet-pull 2> /dev/null || fail "the pinned app did not start"
docker compose run --rm -T updater host-setup > updater-setup.sh
as_root sh updater-setup.sh > setup.log || fail "the setup of the pinned app failed: $(cat setup.log)"
grep -q "writes each version it installs to APP_TAG" setup.log || fail "the setup did not name the variable: $(cat setup.log)"
eventually 20 status_has '.mode == "host" and .channelLocked == null and .installed == "1.0.0"' || fail "unexpected status: $(status)"
pass "a host setup of protocol 2 lets the app choose versions"

version 1.0.1
eventually 90 status_has '.last.version == "1.0.1" and .last.result == "installed"' || fail "1.0.1 did not install: $(status)"
env_tag_is 1.0.1 || fail ".env has APP_TAG=$(env_tag)"
pinned_revision_is 1.0.1 || fail "the app runs $(pinned_revision)"
if ! grep -qx "OTHER=kept" .env || ! grep -q "^# The app of the host test" .env; then
  fail ".env lost its other lines: $(cat .env)"
fi
[ "$(stat -c %a .env)" = 640 ] || fail ".env changed its mode to $(stat -c %a .env)"
pass "a release installs: pinned in .env, migrated, started, healthy, reported"

# The volume belongs to the unprivileged updater: links it places there must
# never make the root deploy script write elsewhere.
canary=$(mktemp)
echo untouched > "$canary"
chmod 0600 "$canary"
pinned_compose exec -T updater sh -c "ln -sf '$canary' /trigger/progress.new && ln -sf '$canary' /trigger/result.new" ||
  fail "could not place the links"

version 1.0.2 unhealthy
eventually 120 status_has '.last.version == "1.0.2" and .last.result == "rolled-back"' || fail "1.0.2 was not rolled back: $(status)"
env_tag_is 1.0.1 || fail ".env has APP_TAG=$(env_tag) after the rollback"
eventually 30 pinned_revision_is 1.0.1 || fail "the app runs $(pinned_revision) after the rollback"
status_has '.last.message | test("unhealthy")' || fail "the rollback does not say why: $(status)"
sleep 5
status_has '.installing == null and .available.failed != null' || fail "the failed version is tried again: $(status)"
pass "an unhealthy release goes back to the version before and is not tried again"
[ "$(cat "$canary")" = untouched ] && [ "$(stat -c %a "$canary")" = 600 ] || fail "the deploy script followed a link in the volume"
rm -f "$canary"
pass "links in the volume do not redirect the root deploy script"

version 1.0.3 healthy 3
eventually 120 status_has '.last.version == "1.0.3" and .last.result == "rolled-back"' || fail "1.0.3 was not rolled back: $(status)"
status_has '.last.message | test("migrate")' || fail "the failed migration is not named: $(status)"
env_tag_is 1.0.1 || fail ".env has APP_TAG=$(env_tag) after the failed migration"
pass "a failed migration goes back to the version before"

pinned_compose exec -T updater updater channel preview > /dev/null || fail "could not switch to preview"
version 1.0.4-preview.1
eventually 90 status_has '.last.version == "1.0.4-preview.1" and .last.result == "installed"' || fail "the preview did not install: $(status)"
env_tag_is 1.0.4-preview.1 || fail ".env has APP_TAG=$(env_tag)"
pass "the Preview channel installs previews"

pinned_compose exec -T updater updater channel stable > /dev/null || fail "could not switch back to stable"
version 2.0.0
eventually 30 status_has '.channel == "stable" and .attention.version == "2.0.0" and .available == null' || fail "2.0.0 is not shown as needing upgrade steps: $(status)"
sleep 3
env_tag_is 1.0.4-preview.1 || fail "a new series installed on its own"
pinned_compose exec -T updater updater install 2.0.0 > /dev/null || fail "the manual install was refused"
eventually 90 status_has '.last.version == "2.0.0" and .last.result == "installed"' || fail "2.0.0 did not install: $(status)"
pinned_revision_is 2.0.0 || fail "the app runs $(pinned_revision)"
pass "a new series waits for an admin, who installs it"

docker image ls "$app" --format "{{.Tag}}" | grep -x 1.0.0 > /dev/null && fail "the oldest release was not removed"
docker image ls "$app" --format "{{.Tag}}" | grep -x 1.0.4-preview.1 > /dev/null || fail "the version before was removed"
eventually 15 journal_has "$pinned_unit.service" "rolled-back: 1.0.2" || fail "the journal does not show the rollback"
pass "older releases are removed, the one before is kept, the journal tells"

as_root sh updater-setup.sh remove > remove.log || fail "remove failed: $(cat remove.log)"
eventually 20 status_has '.mode == "off"' || fail "the updater did not switch off: $(status)"
pass "remove switches the pinned app off"

echo "All host tests passed."
