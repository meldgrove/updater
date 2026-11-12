#!/bin/sh
# Meldgrove Updater: redeploys an app when the image tag it runs points to a
# new image. It asks the registry for the tag's digest once per interval, and
# when the digest changes, it calls the hosting platform's deploy link. The
# platform then pulls the image and restarts the app. See README.md.
#
#   IMAGE              the image and tag the app runs, e.g. ghcr.io/meldgrove/logbuch:0.2
#   DEPLOY_HOOK_URL    deploy link that redeploys the app on POST; empty: off
#   DEPLOY_HOOK_TOKEN  bearer token for the deploy link, if it needs one
#   REGISTRY_USERNAME  with REGISTRY_PASSWORD: sign in to a private registry
#   CHECK_INTERVAL     seconds between checks (default 3600)
#
# "updater.sh check" prints the tag's digest once and exits.
set -eu

# Messages go to stderr: stdout carries only the digest of "check".
log() { printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" >&2; }

# Sleep in the background, so that docker stop ends the container at once.
trap 'exit 0' TERM INT
pause() {
  sleep "$1" &
  wait $!
}

if [ "${1:-}" != check ] && [ -z "${DEPLOY_HOOK_URL:-}" ]; then
  log "Automatic updates are off: DEPLOY_HOOK_URL is empty."
  while :; do pause 86400; done
fi

image=${IMAGE:?Set IMAGE to the image and tag the app runs.}
interval=${CHECK_INTERVAL:-3600}
case $interval in '' | *[!0-9]* | 0)
  log "CHECK_INTERVAL must be a number of seconds."
  exit 1
  ;;
esac

# Registry, repository and tag of the image, the way Docker reads them.
case $image in *@*)
  log "IMAGE names a digest, which never changes. Name a tag instead."
  exit 1
  ;;
esac
case ${image##*/} in
  *:*) name=${image%:*} tag=${image##*:} ;;
  *) name=$image tag=latest ;;
esac
case ${name%%/*} in
  *.* | *:* | localhost) registry=${name%%/*} repository=${name#*/} ;;
  *) registry=registry-1.docker.io repository=$name ;;
esac
if [ "$registry" = registry-1.docker.io ] && [ "${repository#*/}" = "$repository" ]; then
  repository=library/$repository
fi
case $registry in localhost | localhost:* | 127.0.0.1:*) scheme=http ;; *) scheme=https ;; esac
manifest="$scheme://$registry/v2/$repository/manifests/$tag"
accept=application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.v2+json

work=$(mktemp -d)
headers=$work/headers
answer=$work/answer

# HEAD of the tag's manifest, with a token when there is one. Prints the status.
head_manifest() {
  if [ -n "${1:-}" ]; then
    curl --silent --show-error --max-time 30 --head --output /dev/null --dump-header "$headers" \
      --write-out '%{http_code}' --header "Accept: $accept" --header "Authorization: Bearer $1" "$manifest"
  else
    curl --silent --show-error --max-time 30 --head --output /dev/null --dump-header "$headers" \
      --write-out '%{http_code}' --header "Accept: $accept" "$manifest"
  fi
}

# A pull token for the challenge in the 401 answer of the registry.
token() {
  challenge=$(tr -d '\r' < "$headers" | grep -i '^www-authenticate: *bearer ' || true)
  realm=$(printf '%s' "$challenge" | sed -n 's/.*realm="\([^"]*\)".*/\1/p')
  service=$(printf '%s' "$challenge" | sed -n 's/.*service="\([^"]*\)".*/\1/p')
  [ -n "$realm" ] || return 1
  if [ -n "${REGISTRY_USERNAME:-}" ]; then
    curl --silent --fail --max-time 30 --get "$realm" --user "$REGISTRY_USERNAME:${REGISTRY_PASSWORD:-}" \
      --data-urlencode "service=$service" --data-urlencode "scope=repository:$repository:pull"
  else
    curl --silent --fail --max-time 30 --get "$realm" \
      --data-urlencode "service=$service" --data-urlencode "scope=repository:$repository:pull"
  fi | jq -r '.token // .access_token // empty'
}

# The digest the tag points to now.
digest() {
  status=$(head_manifest) || return 1
  if [ "$status" = 401 ]; then
    bearer=$(token) || bearer=""
    [ -n "$bearer" ] || {
      log "The registry refused access to $image: it does not exist, or it is private and needs REGISTRY_USERNAME and REGISTRY_PASSWORD."
      return 1
    }
    status=$(head_manifest "$bearer") || return 1
  fi
  if [ "$status" != 200 ]; then
    log "The registry answered HTTP $status for $image."
    return 1
  fi
  value=$(tr -d '\r' < "$headers" | sed -n 's/^[Dd]ocker-[Cc]ontent-[Dd]igest: *//p')
  [ -n "$value" ] || {
    log "The registry sent no digest for $image."
    return 1
  }
  printf '%s\n' "$value"
}

# POST to the deploy link. Prints nothing of the link or its answer on success.
deploy() {
  if [ -n "${DEPLOY_HOOK_TOKEN:-}" ]; then
    status=$(curl --silent --show-error --max-time 60 --request POST --output "$answer" --write-out '%{http_code}' \
      --header "Authorization: Bearer $DEPLOY_HOOK_TOKEN" "$DEPLOY_HOOK_URL") || return 1
  else
    status=$(curl --silent --show-error --max-time 60 --request POST --output "$answer" --write-out '%{http_code}' \
      "$DEPLOY_HOOK_URL") || return 1
  fi
  case $status in 2??) return 0 ;; esac
  log "The deploy link answered HTTP $status: $(head -c 300 "$answer")"
  return 1
}

if [ "${1:-}" = check ]; then
  digest
  exit
fi

case $DEPLOY_HOOK_URL in http://* | https://*) ;; *)
  log "DEPLOY_HOOK_URL must start with http:// or https://."
  exit 1
  ;;
esac

# The app was deployed with the image the tag points to now.
log "Updater ${APP_VERSION:-(no version)} watches $image every ${interval}s."
retry=$((interval < 60 ? interval : 60))
until current=$(digest); do
  pause "$retry"
done
log "Current image: $current"

while :; do
  pause "$interval"
  latest=$(digest) || continue
  [ "$latest" != "$current" ] || continue
  log "$image points to a new image: $latest. Calling the deploy link."
  if deploy; then
    current=$latest
    log "The platform accepted the deployment."
  else
    log "The deployment did not start; trying again in ${interval}s."
  fi
done
