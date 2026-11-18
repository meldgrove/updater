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

as_root() {
  if [ "$(id -u)" = 0 ]; then "$@"; else sudo "$@"; fi
}
compose() { docker compose --project-directory "$dir" "$@"; }
cleanup() {
  (cd "$dir" && as_root sh updater-setup.sh remove > /dev/null 2>&1) || true
  compose down --volumes --timeout 0 > /dev/null 2>&1 || true
  docker rm --force updater-host-test-registry > /dev/null 2>&1 || true
  rm -rf "$dir"
}
trap cleanup EXIT

fail() {
  echo "FAIL: $*"
  compose logs updater 2>&1 | tail -n 20 || true
  as_root journalctl --unit "$unit.service" --no-pager 2>&1 | tail -n 20 || true
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
logged() { compose logs updater 2>&1 | grep -q "$1"; }
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
as_root journalctl --unit "$unit.service" --no-pager | grep -q "Installing the newest image of $app:1.0" ||
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

echo "All host tests passed."
