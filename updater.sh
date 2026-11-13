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
#   COOLDOWN           install a new image only after it has been out this
#                      long: 3d, 12h, 30m, or seconds (default 0: at once)
#   WINDOW             install only between two times of day, e.g. 02:00-05:00,
#                      in the time zone TZ (default: at any time)
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
case $registry in docker.io | index.docker.io) registry="registry-1.docker.io" ;; esac
if [ "$registry" = registry-1.docker.io ] && [ "${repository#*/}" = "$repository" ]; then
  repository=library/$repository
fi
case $registry in localhost | localhost:* | 127.0.0.1:*) scheme=http ;; *) scheme=https ;; esac
manifest="$scheme://$registry/v2/$repository/manifests/$tag"
accept=application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.v2+json

work=$(mktemp -d)
headers=$work/headers
body=$work/body

# fetch <token> <curl arguments>: one request to the registry, with the token
# when there is one. Prints the status; the answer's headers go to $headers.
fetch() {
  bearer=$1
  shift
  if [ -n "$bearer" ]; then
    set -- --header "Authorization: Bearer $bearer" "$@"
  fi
  curl --silent --show-error --max-time 30 --dump-header "$headers" --write-out '%{http_code}' "$@"
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

# ask <curl arguments>: fetch, signed in when the registry asks for it.
# Prints the status; 401 when the registry gives no token or refuses it.
ask() {
  status=$(fetch "" "$@") || return 1
  if [ "$status" = 401 ]; then
    bearer=$(token) || bearer=""
    if [ -n "$bearer" ]; then
      status=$(fetch "$bearer" "$@") || return 1
    fi
  fi
  printf '%s\n' "$status"
}

# The digest the tag points to now.
digest() {
  status=$(ask --head --output /dev/null --header "Accept: $accept" "$manifest") || return 1
  if [ "$status" = 401 ]; then
    log "The registry refused access to $image: it does not exist, or it is private and needs REGISTRY_USERNAME and REGISTRY_PASSWORD."
    return 1
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

# The repository's release tags (X.Y.Z), one per line, as far as the registry
# lists them. Failures only shorten the list.
releases() {
  url="$scheme://$registry/v2/$repository/tags/list?n=1000"
  pages=0
  while [ -n "$url" ] && [ "$pages" -lt 20 ]; do
    pages=$((pages + 1))
    status=$(ask --output "$body" "$url" 2> /dev/null) || return 0
    [ "$status" = 200 ] || return 0
    jq -r '.tags[]?' "$body" 2> /dev/null | grep -E '^[0-9]+\.[0-9]+\.[0-9]+$' || true
    # A long list continues on the page that the Link header names.
    next=$(tr -d '\r' < "$headers" | sed -n 's/^[Ll]ink: *<\([^>]*\)>.*rel="next".*/\1/p')
    case $next in
      '') url="" ;;
      http://* | https://*) url=$next ;;
      *) url="$scheme://$registry$next" ;;
    esac
  done
}

# Logs, once per release, that a release is out which the watched tag never
# reaches: 0.4.0 for the tag 0.3, 2.0.0 for the tag 1. Someone has to read its
# notes and change the tag. Tags that are no version (latest) follow everything.
noted=""
note_newer() {
  case $tag in '' | *[!0-9.]* | .* | *. | *..*) return 0 ;; esac
  newest=$(releases | sort -t . -k 1,1n -k 2,2n -k 3,3n | tail -n 1)
  if [ -z "$newest" ] || [ "$newest" = "$noted" ]; then
    return 0
  fi
  old_ifs=$IFS
  IFS=.
  # shellcheck disable=SC2086 # split the tag and the release at their dots
  set -- $tag $newest
  IFS=$old_ifs
  # The tag has one to three parts, the release three: is the release greater
  # in the parts that the tag names?
  case $# in
    4) [ "$2" -gt "$1" ] || return 0 ;;
    5) [ "$3" -gt "$1" ] || { [ "$3" -eq "$1" ] && [ "$4" -gt "$2" ]; } || return 0 ;;
    6) [ "$4" -gt "$1" ] || { [ "$4" -eq "$1" ] && [ "$5" -gt "$2" ]; } ||
      { [ "$4" -eq "$1" ] && [ "$5" -eq "$2" ] && [ "$6" -gt "$3" ]; } || return 0 ;;
    *) return 0 ;;
  esac
  noted=$newest
  log "Release $newest is out, and the tag $tag does not reach it. To install it, read the release notes of every release since yours, then change the tag."
}

# POST to the deploy link. Logs only the status: an error page may repeat the link.
deploy() {
  if [ -n "${DEPLOY_HOOK_TOKEN:-}" ]; then
    status=$(curl --silent --show-error --max-time 60 --request POST --output /dev/null --write-out '%{http_code}' \
      --header "Authorization: Bearer $DEPLOY_HOOK_TOKEN" "$DEPLOY_HOOK_URL") || return 1
  else
    status=$(curl --silent --show-error --max-time 60 --request POST --output /dev/null --write-out '%{http_code}' \
      "$DEPLOY_HOOK_URL") || return 1
  fi
  case $status in 2??) return 0 ;; esac
  log "The deploy link answered HTTP $status."
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

# seconds <time>: 90, 90s, 30m, 12h or 3d in seconds.
seconds() {
  number=${1%[smhd]}
  case $number in '' | *[!0-9]*) return 1 ;; esac
  number=$(printf '%s' "$number" | sed 's/^0*//')
  number=${number:-0}
  case $1 in
    *d) echo $((number * 86400)) ;;
    *h) echo $((number * 3600)) ;;
    *m) echo $((number * 60)) ;;
    *) echo "$number" ;;
  esac
}
cooldown=$(seconds "${COOLDOWN:-0}") || {
  log "COOLDOWN must be a time such as 3d, 12h, 30m or 90 (seconds)."
  exit 1
}

# minutes <HH:MM>: the minutes since midnight.
minutes() {
  case $1 in [0-9][0-9]:[0-5][0-9]) ;; *) return 1 ;; esac
  hour=${1%:*}
  minute=${1#*:}
  hour=${hour#0}
  minute=${minute#0}
  [ "$hour" -le 23 ] || return 1
  echo $((hour * 60 + minute))
}
window=${WINDOW:-}
if [ -n "$window" ]; then
  if ! opens=$(minutes "${window%-*}") || ! closes=$(minutes "${window#*-}") || [ "$opens" = "$closes" ]; then
    log "WINDOW must be two different times of day such as 02:00-05:00."
    exit 1
  fi
fi
# True while the window is open. A window such as 22:00-02:00 spans midnight.
in_window() {
  [ -n "$window" ] || return 0
  clock=$(minutes "$(date +%H:%M)")
  if [ "$opens" -lt "$closes" ]; then
    [ "$clock" -ge "$opens" ] && [ "$clock" -lt "$closes" ]
  else
    [ "$clock" -ge "$opens" ] || [ "$clock" -lt "$closes" ]
  fi
}
# Seconds until the window opens, counted from the start of this minute.
until_window() {
  clock=$(minutes "$(date +%H:%M)")
  echo $((((opens - clock + 1440) % 1440) * 60))
}

# What the updater knows: the image it takes as installed, and a newer one that
# waits for its cooldown or the window, with the time it was first seen. The
# file is in the container's own file system: it is still there after a restart
# of the container or the server, when no deployment has pulled anything. A
# deployment makes a new container, which starts without it.
state=/tmp/updater-state
installed="" pending="" seen=""
save() {
  {
    printf 'image=%s\ninstalled=%s\npending=%s\nseen=%s\n' "$image" "$installed" "$pending" "$seen" > "$state.new" &&
      mv "$state.new" "$state"
  } 2> /dev/null || true
}
saved() { sed -n "s/^$1=//p" "$state" 2> /dev/null | head -n 1; }

log "Updater ${APP_VERSION:-(no version)} watches $image every ${interval}s."
[ "$cooldown" = 0 ] || log "A new image installs once it has been out for $COOLDOWN."
[ -z "$window" ] || log "New images install only between ${window%-*} and ${window#*-} (time zone ${TZ:-UTC})."
retry=$((interval < 60 ? interval : 60))
if [ "$(saved image)" = "$image" ] && [ -n "$(saved installed)" ]; then
  installed=$(saved installed)
  pending=$(saved pending)
  seen=$(saved seen)
  case $seen in '' | *[!0-9]*) pending="" seen="" ;; esac
  log "Current image: $installed, as before the restart."
else
  # A deployment has just pulled the image the tag points to now.
  until installed=$(digest); do
    pause "$retry"
  done
  save
  log "Current image: $installed"
fi

note_due=0
waiting=""
nap=$interval
while :; do
  now=$(date +%s)
  if [ "$now" -ge "$note_due" ]; then
    note_due=$((now + 86400))
    note_newer
  fi
  pause "$nap"
  nap=$interval
  latest=$(digest) || continue
  if [ "$latest" = "$installed" ]; then
    if [ -n "$pending" ]; then
      pending="" seen=""
      save
    fi
    continue
  fi
  now=$(date +%s)
  if [ "$latest" != "$pending" ]; then
    pending=$latest seen=$now
    save
    if [ "$cooldown" = 0 ]; then
      log "$image points to a new image: $latest."
    else
      log "$image points to a new image: $latest. It installs once it has been out for $COOLDOWN."
    fi
  fi
  ready=$((seen + cooldown))
  if [ "$now" -lt "$ready" ]; then
    [ $((ready - now)) -ge "$interval" ] || nap=$((ready - now))
    continue
  fi
  if ! in_window; then
    if [ "$waiting" != "$latest" ]; then
      waiting=$latest
      log "Waiting for the window that opens at ${window%-*}."
    fi
    left=$(until_window)
    [ "$left" -ge "$interval" ] || nap=$left
    continue
  fi
  log "Calling the deploy link."
  # Saved as installed before the call: a platform may stop this container
  # while it answers, and the next start must not deploy the same image again.
  before=$installed since=$seen
  installed=$latest pending="" seen=""
  save
  if deploy; then
    log "The platform accepted the deployment."
  else
    installed=$before pending=$latest seen=$since
    save
    log "The deployment did not start; trying again in ${interval}s."
  fi
done
