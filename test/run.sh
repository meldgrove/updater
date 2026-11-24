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
trigger=$(mktemp -d)
echo 200 > "$state/status"
: > "$state/calls"
hook=""

cleanup() {
  docker rm --force updater-test-registry updater-test > /dev/null 2>&1 || true
  [ -z "$hook" ] || kill "$hook" 2> /dev/null || true
  rm -rf "$state" "$trigger"
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
logged() { docker logs updater-test 2>&1 | grep "$1" > /dev/null; }
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

# The digest that the registry reports for the app's tag.
registry_digest() {
  curl --silent --head --header "Accept: application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.v2+json" \
    "http://$registry/v2/app/manifests/1.0" | tr -d '\r' | sed -n 's/^[Dd]ocker-[Cc]ontent-[Dd]igest: *//p'
}

# check prints the digest the registry reports, for a local and a public image.
expected=$(registry_digest)
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

# Without a deploy link the updater says it is off, and still answers the
# app: the API is up and the container is healthy.
run --env IMAGE="$app"
eventually 10 logged "Automatic updates are off" || fail "no 'off' message"
api() { docker exec updater-test updater "$@"; }
eventually 20 sh -c 'docker inspect --format "{{.State.Health.Status}}" updater-test | grep -qx healthy' || fail "the container is not healthy"
[ "$(api status | jq -r .mode)" = off ] || fail "the status does not say off: $(api status)"
api install 9.9.9 2> /dev/null && fail "installed while off"
logged "The API takes requests without a token" || fail "no warning about the API without a token"
stop_quickly
pass "off without a deploy link, answers the API, and stops at once"

# With API_TOKEN, the API answers only requests with the token; the commands
# in the container send it, and the health check needs none.
run --env IMAGE="$app" --env API_TOKEN=test-token-0123456789
eventually 20 sh -c 'docker inspect --format "{{.State.Health.Status}}" updater-test | grep -qx healthy' || fail "the container is not healthy with a token"
answer() { curl --silent --output /dev/null --write-out '%{http_code}' "$@" http://127.0.0.1:8080/v1/status; }
[ "$(answer)" = 401 ] || fail "the API answered without the token"
[ "$(answer --header 'Authorization: Bearer wrong')" = 401 ] || fail "the API answered a wrong token"
[ "$(answer --header 'Authorization: Bearer test-token-0123456789')" = 200 ] || fail "the API refused the token"
[ "$(api status | jq -r .mode)" = off ] || fail "the status command did not send the token"
! logged "The API takes requests without a token" || fail "warned although the API has a token"
stop_quickly
pass "with API_TOKEN, the API refuses requests without the token"

# A token of only spaces is no token, as in the apps, which trim it.
run --env IMAGE="$app" --env "API_TOKEN=   "
eventually 20 sh -c 'docker inspect --format "{{.State.Health.Status}}" updater-test | grep -qx healthy' || fail "the container is not healthy with a blank token"
[ "$(answer)" = 200 ] || fail "the API refused a request although the token is blank"
[ "$(api status | jq -r .apiToken)" = false ] || fail "apiToken is true for a blank token: $(api status)"
logged "The API takes requests without a token" || fail "no warning about the API without a token"
stop_quickly
pass "a token of only spaces is no token"

# Misconfiguration ends the updater with a message.
for setting in "IMAGE=$app@sha256:0000" CHECK_INTERVAL=hourly COOLDOWN=soon WINDOW=2-5 WINDOW=02:00-02:00 WINDOW=25:00-03:00 "VERSION_VARIABLE=APP VERSION" "API_TOKEN=two words"; do
  if docker run --rm --env IMAGE="$app" --env "$setting" --env DEPLOY_HOOK_URL=http://127.0.0.1:1/ "$image" > /dev/null 2>&1; then
    fail "the updater accepted $setting"
  fi
done
if docker run --rm --env IMAGE="$app" --env DEPLOY_HOOK_URL=ftp://example.invalid/ "$image" > /dev/null 2>&1; then
  fail "the updater accepted an ftp deploy link"
fi
pass "rejects a digest, a bad interval, cooldown or window, a token with spaces, and a non-HTTP deploy link"

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

# Without a deploy link, a host that ran the setup marks /trigger as watched.
# The updater then leaves its request there instead of calling a link.
docker run --rm "$image" host-setup | head -n 1 | grep -q '^#!/bin/sh' || fail "host-setup printed no script"
pass "host-setup prints the setup script"
chmod 0777 "$trigger"
touch "$trigger/watched"
run --env IMAGE="$app" --volume "$trigger:/trigger"
eventually 15 logged "The host deploys: it watches /trigger" || fail "the updater did not see that the host watches"
eventually 15 logged "Current image" || fail "the updater did not read the current image"
publish 9
eventually 15 test -e "$trigger/deploy" || fail "no request for the host after the tag moved"
[ "$(cat "$trigger/deploy")" = "$(registry_digest)" ] || fail "the request names $(cat "$trigger/deploy"), not the new image"
logged "Asking the host to deploy" || fail "the request was not logged"
rm "$trigger/deploy"
eventually 10 logged "The host deploys it now" || fail "the pickup was not logged"
sleep 3
[ ! -e "$trigger/deploy" ] || fail "asked the host again for the same image"
pass "a moved tag leaves one request for the host"
rm "$trigger/watched"
publish 10
eventually 15 logged "no host watches /trigger any more" || fail "a request without a watching host was not refused"
[ ! -e "$trigger/deploy" ] || fail "left a request although no host watches"
stop_quickly
pass "no request once the host stops watching"

# A host setup of protocol 2: the updater picks versions by channel and asks
# the host to install the exact version; the host reports the result.
rm -rf "${trigger:?}"/*
printf 'protocol=2\nproject=app\n' > "$trigger/watched"
publish 11
release 2.0.0
run --env IMAGE="${app%:*}:2.0.0" --env VERSION_VARIABLE=APP_TAG --volume "$trigger:/trigger"
eventually 15 logged "Versions are chosen by the channel" || fail "the updater does not pin versions"
eventually 15 sh -c "docker exec updater-test updater status | jq -e '.installed == \"2.0.0\" and .available == null' > /dev/null" ||
  fail "unexpected status: $(api status)"
release 2.0.1
release 2.1.0-preview.1
eventually 15 grep -sqx version=2.0.1 "$trigger/deploy" || fail "no request for 2.0.1: $(cat "$trigger/deploy" 2> /dev/null)"
grep -qx protocol=2 "$trigger/deploy" || fail "the request has no protocol"
id=$(sed -n 's/^id=//p' "$trigger/deploy")
[ "$(api status | jq -r .installing.version)" = 2.0.1 ] || fail "the status shows no installation"
rm "$trigger/deploy"
printf 'id=%s\nstage=starting\n' "$id" > "$trigger/progress"
eventually 10 sh -c "docker exec updater-test updater status | jq -e '.installing.stage == \"starting\"' > /dev/null" || fail "the stage was not read"
printf 'id=%s\nversion=2.0.1\nresult=rolled-back\nmessage=container app-web-1 is unhealthy\nfinished=%s\n' "$id" "$(date +%s)" > "$trigger/result"
eventually 10 sh -c "docker exec updater-test updater status | jq -e '.last.result == \"rolled-back\" and .available.failed != null' > /dev/null" ||
  fail "the result was not read: $(api status)"
sleep 3
[ ! -e "$trigger/deploy" ] || fail "tried the failed version again"
pass "asks the host for an exact version, reads its stages and result, and does not retry a failure"
[ "$(api channel)" = stable ] || fail "the channel is not stable"
api channel preview > /dev/null
eventually 15 grep -sqx version=2.1.0-preview.1 "$trigger/deploy" || fail "no request for the preview"
pass "the Preview channel installs previews"
docker restart updater-test > /dev/null
eventually 15 sh -c "docker exec updater-test updater status | jq -e '.channel == \"preview\" and .installing.version == \"2.1.0-preview.1\"' > /dev/null" ||
  fail "the restarted updater forgot its state: $(api status)"
stop_quickly
pass "keeps channel and installation over a restart"

echo "All tests passed."
