#!/usr/bin/env bash
# End-to-end test of the updater image against a local registry and a fake
# deploy link. Needs Docker, Python 3, GNU date, and internet access to ghcr.io.
# Usage: test/run.sh [image]   (without an image, it builds updater:test)
set -euo pipefail
cd "$(dirname "$0")/.."

image=${1:-}
if [ -z "$image" ]; then
  docker build --quiet --tag updater:test . > /dev/null
  image=updater:test
fi
registry=127.0.0.1:5555
app=localhost:5555/app:1.0
hook_port=5556
hook_url="http://127.0.0.1:$hook_port/deploy?uuid=abc&force=false"
state=$(mktemp -d)
echo 200 > "$state/status"
: > "$state/calls"
hook=""

cleanup() {
  docker rm --force updater-test-registry updater-test > /dev/null 2>&1 || true
  [ -z "$hook" ] || kill "$hook" 2> /dev/null || true
  rm -rf "$state"
}
trap cleanup EXIT

fail() {
  echo "FAIL: $*"
  docker logs updater-test 2>&1 | tail -n 20 || true
  exit 1
}
pass() { echo "ok: $*"; }
calls() { wc -l < "$state/calls" | tr -d ' '; }
# eventually <seconds> <command...>: true once the command succeeds in time.
eventually() {
  local deadline=$((SECONDS + $1))
  shift
  until "$@"; do
    [ "$SECONDS" -lt "$deadline" ] || return 1
    sleep 0.5
  done
}
calls_are() { [ "$(calls)" = "$1" ]; }
calls_at_least() { [ "$(calls)" -ge "$1" ]; }
logged() { docker logs updater-test 2>&1 | grep -q "$1"; }
# publish <n>: push a new image under the app's tag.
publish() {
  printf 'FROM %s\nLABEL test.revision=%s\n' "$image" "$1" | docker build --quiet --tag "$app" - > /dev/null
  docker push --quiet "$app" > /dev/null
}
# release <version>: push the app's image under a release tag as well.
release() {
  docker tag "$app" "${app%:*}:$1"
  docker push --quiet "${app%:*}:$1" > /dev/null
}
run() {
  docker run --detach --name updater-test --network host --env CHECK_INTERVAL=1 "$@" "$image" > /dev/null
}
# Stops the updater and fails unless it exits within 3 seconds.
stop_quickly() {
  local start=$SECONDS
  docker stop updater-test > /dev/null
  [ $((SECONDS - start)) -le 3 ] || fail "docker stop took $((SECONDS - start))s"
  docker rm updater-test > /dev/null
}

docker run --detach --name updater-test-registry --publish "$registry:5000" \
  registry:3.1.2@sha256:82f6ff4a7f982960c8aa51617fea3aabc485df6e55cce80d22deb1de36f1bc3c > /dev/null
python3 test/fake_hook.py "$hook_port" "$state" &
hook=$!
eventually 30 curl --silent --fail "http://$registry/v2/" > /dev/null || fail "the registry did not start"
publish 1
release 1.0.7

# check prints the digest the registry reports, for a local and a public image.
expected=$(curl --silent --head --header "Accept: application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.v2+json" \
  "http://$registry/v2/app/manifests/1.0" | tr -d '\r' | sed -n 's/^[Dd]ocker-[Cc]ontent-[Dd]igest: *//p')
got=$(docker run --rm --network host --env IMAGE="$app" "$image" check)
[ "$got" = "$expected" ] || fail "check printed '$got', the registry reports '$expected'"
pass "check prints the digest of a local image"
got=$(docker run --rm --env IMAGE=ghcr.io/meldgrove/reimbursements:latest "$image" check)
[[ $got =~ ^sha256:[0-9a-f]{64}$ ]] || fail "check printed '$got' for a public GHCR image"
pass "check signs in to GHCR anonymously"
for name in alpine:3 docker.io/library/alpine:3; do
  got=$(docker run --rm --env IMAGE="$name" "$image" check)
  [[ $got =~ ^sha256:[0-9a-f]{64}$ ]] || fail "check printed '$got' for $name"
done
pass "check reads Docker Hub, with and without docker.io"

# Without a deploy link the updater only says it is off.
run --env IMAGE="$app"
eventually 10 logged "Automatic updates are off" || fail "no 'off' message"
stop_quickly
pass "off without a deploy link, and stops at once"

# Misconfiguration ends the updater with a message.
for setting in "IMAGE=$app@sha256:0000" CHECK_INTERVAL=hourly COOLDOWN=soon WINDOW=2-5 WINDOW=02:00-02:00 WINDOW=25:00-03:00; do
  if docker run --rm --env IMAGE="$app" --env "$setting" --env DEPLOY_HOOK_URL=http://127.0.0.1:1/ "$image" > /dev/null 2>&1; then
    fail "the updater accepted $setting"
  fi
done
if docker run --rm --env IMAGE="$app" --env DEPLOY_HOOK_URL=ftp://example.invalid/ "$image" > /dev/null 2>&1; then
  fail "the updater accepted an ftp deploy link"
fi
pass "rejects a digest, a bad interval, cooldown or window, and a non-HTTP deploy link"

# An unchanged tag deploys nothing; a moved tag deploys once, with the token.
run --env IMAGE="$app" --env "DEPLOY_HOOK_URL=$hook_url" --env DEPLOY_HOOK_TOKEN=secret
eventually 15 logged "Current image" || fail "the updater did not read the current image"
sleep 3
calls_are 0 || fail "deployed although the tag did not move"
! logged "is out" || fail "announced 1.0.7, which the tag 1.0 reaches"
pass "an unchanged tag deploys nothing"
publish 2
eventually 15 calls_are 1 || fail "no deployment after the tag moved"
[ "$(cat "$state/calls")" = "/deploy?uuid=abc&force=false Bearer secret" ] || fail "unexpected call: $(cat "$state/calls")"
sleep 3
calls_are 1 || fail "deployed the same image again"
pass "a moved tag deploys once, with POST and the bearer token"

# A refused deployment is tried again until the platform accepts it.
echo 401 > "$state/status"
publish 3
eventually 15 calls_at_least 3 || fail "a refused deployment was not tried again"
logged "answered HTTP 401." || fail "the refusal was not logged"
! logged "Unauthenticated" || fail "the deploy link's answer was logged"
echo 200 > "$state/status"
sleep 3
settled=$(calls)
sleep 3
calls_are "$settled" || fail "kept deploying after the platform accepted"
pass "a refused deployment is tried again until it is accepted"

# A restart of the same container pulls nothing, so the updater must still know
# which image is installed: a tag that moved meanwhile deploys.
docker stop updater-test > /dev/null
publish 4
docker start updater-test > /dev/null
eventually 15 logged "as before the restart" || fail "the restarted updater did not remember the installed image"
eventually 15 calls_are $((settled + 1)) || fail "no deployment of an image that came out while the updater was stopped"
sleep 3
calls_are $((settled + 1)) || fail "deployed the same image again after the restart"
pass "a restarted updater remembers the installed image and installs what came out meanwhile"
stop_quickly
pass "stops at once while watching"

# With a cooldown, a new image waits. A newer image starts the time again.
before=$(calls)
run --env IMAGE="$app" --env "DEPLOY_HOOK_URL=$hook_url" --env COOLDOWN=15
eventually 15 logged "Current image" || fail "the updater did not read the current image"
publish 5
sleep 6
publish 6
sleep 12
calls_are "$before" || fail "deployed although the newest image was out for less than the cooldown"
logged "once it has been out for 15" || fail "the cooldown was not logged"
eventually 20 calls_are $((before + 1)) || fail "no deployment after the cooldown"
sleep 3
calls_are $((before + 1)) || fail "deployed twice after the cooldown"
stop_quickly
pass "a new image installs after the cooldown, counted from the newest image"

# With a window, a new image waits until the window opens. The window is read
# in the time zone TZ: one around the current time in Tokyo is closed in UTC.
before=$(calls)
run --env IMAGE="$app" --env "DEPLOY_HOOK_URL=$hook_url" \
  --env "WINDOW=$(date --utc --date '+2 hours' +%H:%M)-$(date --utc --date '+3 hours' +%H:%M)"
eventually 15 logged "Current image" || fail "the updater did not read the current image"
publish 7
eventually 15 logged "Waiting for the window" || fail "the closed window was not logged"
sleep 3
calls_are "$before" || fail "deployed outside the window"
stop_quickly
pass "a new image waits while the window is closed"
run --env IMAGE="$app" --env "DEPLOY_HOOK_URL=$hook_url" --env TZ=Asia/Tokyo \
  --env "WINDOW=$(TZ=Asia/Tokyo date --date '-1 hour' +%H:%M)-$(TZ=Asia/Tokyo date --date '+1 hour' +%H:%M)"
eventually 15 logged "Current image" || fail "the updater did not read the current image"
publish 8
eventually 15 calls_are $((before + 1)) || fail "no deployment inside the window"
stop_quickly
pass "a new image installs inside the window, in the time zone TZ"

# A release that the watched tag never reaches is announced, also on GHCR.
release 1.2.0
run --env IMAGE="$app" --env "DEPLOY_HOOK_URL=$hook_url"
eventually 15 logged "Release 1.2.0 is out, and the tag 1.0 does not reach it" || fail "1.2.0 was not announced for the tag 1.0"
stop_quickly
run --env IMAGE=ghcr.io/meldgrove/reimbursements:0.2 --env "DEPLOY_HOOK_URL=$hook_url"
eventually 30 logged "is out, and the tag 0.2 does not reach it" || fail "no newer release was announced for a GHCR image"
stop_quickly
pass "announces a release that the watched tag does not reach"

echo "All tests passed."
