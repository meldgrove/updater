#!/bin/sh
# Meldgrove Updater, host setup: automatic updates on a server that runs the
# app with Docker Compose and systemd, such as Debian or Ubuntu, without a
# hosting platform. See README.md, "Debian and similar systems".
#
# In the app's folder, once the app runs (docker compose up -d):
#
#   docker compose run --rm -T --pull always updater host-setup > updater-setup.sh
#   sudo sh updater-setup.sh           # switch automatic updates on
#   sudo sh updater-setup.sh remove    # switch them off again
#
# It installs a systemd path unit that waits for the updater's request in the
# volume at /trigger, and a service that then installs the version the
# updater asks for: it writes it to the version variable in .env, pulls the
# image, runs docker compose up --wait, and goes back to the version before
# when the app does not become healthy. The updater keeps running without
# access to Docker. Running the setup again updates the deploy script.
set -eu

fail() {
  printf 'updater-setup: %s\n' "$*" >&2
  exit 1
}

action=${1:-install}
case $action in install | remove) ;; *) fail "Usage: sudo sh updater-setup.sh [remove]" ;; esac
[ "$(id -u)" = 0 ] || fail "Run it as root: sudo sh $0 $*"
if ! command -v systemctl > /dev/null || [ ! -d /run/systemd/system ]; then
  fail "This server does not run systemd."
fi
command -v docker > /dev/null || fail "Docker is not installed."
docker compose version > /dev/null 2>&1 || fail "The Docker Compose plugin (docker compose) is missing."
docker compose up --help 2> /dev/null | grep -q -- '--wait-timeout' ||
  fail "Docker Compose is too old: it needs 'up --wait-timeout' (Compose 2.20 or later)."

lib=/usr/local/lib/meldgrove-updater
units=/etc/systemd/system
found=$(docker compose ps --all --quiet updater) ||
  fail "Docker Compose cannot read the app here. Run this in the app's folder."
container=$(printf '%s\n' "$found" | head -n 1)
inspect() { docker inspect --format "$1" "$container"; }
if [ -n "$container" ]; then
  project=$(inspect '{{index .Config.Labels "com.docker.compose.project"}}')
  trigger=$(inspect '{{range .Mounts}}{{if eq .Destination "/trigger"}}{{.Source}}{{end}}{{end}}')
elif [ "$action" = remove ]; then
  # After docker compose down: the project from the Compose files, the volume
  # from the installed unit.
  project=$(docker compose config | sed -n 's/^name: //p' | head -n 1)
  [ -n "$project" ] || fail "Docker Compose names no project here. Run this in the app's folder."
  trigger=$(sed -n 's|^PathExists=\(.*\)/deploy$|\1|p' "$units/meldgrove-updater-$project.path" 2> /dev/null || true)
else
  fail "No updater container here. Run this in the app's folder, once the app runs (docker compose up -d)."
fi
case $project in *[!a-z0-9_-]* | '') fail "The Docker Compose project name is unusual: $project" ;; esac
unit=meldgrove-updater-$project

if [ "$action" = remove ]; then
  systemctl disable --now "$unit.path" > /dev/null 2>&1 || true
  rm -f "$units/$unit.path" "$units/$unit.service"
  systemctl daemon-reload
  if [ -n "$trigger" ]; then
    rm -f "$trigger/watched" "$trigger/deploy" "$trigger/progress" "$trigger/result"
  fi
  rm -rf "/var/lib/meldgrove-updater/$project"
  # A new updater container reads that no host watches it any more.
  if [ -n "$container" ] && [ -x "$lib/deploy" ]; then
    "$lib/deploy" "$project" updater
  fi
  if ! ls "$units"/meldgrove-updater-*.path > /dev/null 2>&1; then
    rm -rf "$lib"
    rmdir /var/lib/meldgrove-updater 2> /dev/null || true
  fi
  echo "Automatic updates of $project are off."
  exit 0
fi

[ -n "$trigger" ] || fail "The updater has no volume at /trigger. Update compose.yaml to a release of the app that has one, then run docker compose up -d."
case $trigger in *[!A-Za-z0-9._/@+-]*) fail "The volume's path has characters that a systemd unit cannot hold: $trigger" ;; esac

mkdir -p "$lib"
cat > "$lib/deploy.new" << 'EOF'
#!/bin/sh
# Meldgrove Updater (host protocol 2), written by its host setup: installs
# what the updater of a Docker Compose project asks for. systemd starts it
# when the updater leaves its request in the volume at /trigger.
#
#   deploy <project>           install the requested version (or, for an
#                              updater 0.1 request, the newest image of the tag)
#   deploy <project> updater   recreate only the updater container
#
# A version is installed by writing it to the variable VERSION_VARIABLE (from
# the updater's environment) in .env, pulling the image, and docker compose up
# --wait. When that fails, .env goes back to what it was and the version
# before starts again. The result goes to /trigger/result for the updater.
set -eu
project=${1:?Usage: deploy <project> [updater]}
mode=${2:-app}
state=/var/lib/meldgrove-updater/$project
container=$(docker ps --all --quiet --filter "label=com.docker.compose.project=$project" \
  --filter label=com.docker.compose.service=updater | head -n 1)
if [ -z "$container" ]; then
  echo "The Docker Compose project $project has no updater container." >&2
  exit 1
fi
inspect() { docker inspect --format "$1" "$container"; }
directory=$(inspect '{{index .Config.Labels "com.docker.compose.project.working_dir"}}')
files=$(inspect '{{index .Config.Labels "com.docker.compose.project.config_files"}}')
env_files=$(inspect '{{index .Config.Labels "com.docker.compose.project.environment_file"}}')
setting() { inspect '{{range .Config.Env}}{{println .}}{{end}}' | sed -n "s/^$1=//p" | head -n 1; }
image=$(setting IMAGE)
variable=$(setting VERSION_VARIABLE)
updater_image=$(inspect '{{.Config.Image}}')
trigger=$(inspect '{{range .Mounts}}{{if eq .Destination "/trigger"}}{{.Source}}{{end}}{{end}}')

# The Compose files and env files of the last docker compose up, as arguments.
set -f
old_ifs=$IFS
IFS=,
# shellcheck disable=SC2086 # the labels separate the files with commas
set -- $files
IFS=$old_ifs
count=$#
for file do
  set -- "$@" --file "$file"
done
shift "$count"
first_env=""
if [ -n "$env_files" ]; then
  IFS=,
  for env_file in $env_files; do
    [ -n "$first_env" ] || first_env=$env_file
    set -- "$@" --env-file "$env_file"
  done
  IFS=$old_ifs
fi
set +f
compose() {
  docker compose --ansi never --project-name "$project" --project-directory "$directory" "$@"
}

if [ "$mode" = updater ]; then
  # "$@" holds the --file and --env-file arguments.
  docker compose --ansi never --project-name "$project" --project-directory "$directory" "$@" \
    up --detach --no-deps --force-recreate --quiet-pull updater
  exit
fi

request=""
if [ -n "$trigger" ] && [ -f "$trigger/deploy" ]; then
  request=$(head -c 4096 "$trigger/deploy")
  rm -f "$trigger/deploy"
fi
field() { printf '%s\n' "$request" | sed -n "s/^$1=//p" | head -n 1; }

if [ "$(field protocol)" != 2 ]; then
  # A request of updater 0.1: the newest image of the tag.
  echo "Installing the newest image of $image."
  docker pull --quiet "$image"
  docker pull --quiet "$updater_image" || echo "Could not pull the updater's image; it keeps the one it has." >&2
  compose "$@" up --detach --quiet-pull
  exit
fi

id=$(field id)
version=$(field version)
case $id in '' | *[!0-9a-f]*) echo "The request has no valid id." >&2 && exit 1 ;; esac

# report <file> <lines…>: a file for the updater, which runs as another user.
# (POSIX sh has no local variables: names here must not clash.)
report() {
  report_file=$trigger/$1
  shift
  printf '%s\n' "$@" > "$report_file.new"
  chmod 0644 "$report_file.new"
  mv "$report_file.new" "$report_file"
}
progress() { report progress "id=$id" "stage=$1"; }
# finish <result> <message>: the outcome, also in the journal.
finish() {
  message=$(printf '%s' "$2" | tr '\r\n\t' '   ' | tr -cd '[:print:]' | cut -c 1-400)
  report result "id=$id" "version=$version" "result=$1" "message=$message" "finished=$(date +%s)"
  echo "$1: $version${message:+ ($message)}"
}
# last_error <log>: the line of a docker compose log that says what failed.
last_error() {
  line=$(grep -iE 'error|unhealthy|did not|didn.t|failed|exited|invalid|required|not found|denied' "$1" | tail -n 1)
  [ -n "$line" ] || line=$(tail -n 1 "$1")
  printf '%s' "$line"
}

if ! printf '%s\n' "$version" | grep -Eqx '[0-9]+\.[0-9]+\.[0-9]+(-preview\.[0-9]+)?'; then
  finish failed "The updater asked for '$version', which is no version."
  exit 1
fi
if ! printf '%s\n' "$variable" | grep -Eqx '[A-Za-z_][A-Za-z0-9_]*'; then
  finish failed "The updater has no VERSION_VARIABLE, so the server cannot choose a version. Update compose.yaml."
  exit 1
fi
# The image's repository from the updater's IMAGE, never from the request.
name=${image%@*}
case ${name##*/} in *:*) repository=${name%:*} ;; *) repository=$name ;; esac
target=$repository:$version
env_file=${first_env:-$directory/.env}
case $env_file in /*) ;; *) env_file=$directory/$env_file ;; esac
if [ ! -f "$env_file" ]; then
  finish failed "There is no $env_file to write $variable to."
  exit 1
fi

mkdir -p "$state"
chmod 0700 "$state"
log=$state/install.log
echo "Installing $target."
progress pulling
if ! docker pull --quiet "$target" > "$log" 2>&1; then
  cat "$log" >&2
  finish failed "Could not download $target: $(last_error "$log")"
  exit 1
fi
docker pull --quiet "$updater_image" > /dev/null 2>&1 || echo "Could not pull the updater's image; it keeps the one it has." >&2

# .env keeps its owner, mode and comments; every line of the variable gets
# the version, and the variable is added when it is missing.
cp "$env_file" "$state/env.before"
awk -v name="$variable" -v value="$version" '
  BEGIN { pattern = "^[ \t]*(export[ \t]+)?" name "[ \t]*=" }
  $0 ~ pattern { print name "=" value; found = 1; next }
  { print }
  END { if (!found) print name "=" value }
' "$state/env.before" > "$state/env.next"
cat "$state/env.next" > "$env_file"
rm -f "$state/env.next"

progress starting
if compose "$@" up --detach --wait --wait-timeout 900 --quiet-pull > "$log" 2>&1; then
  cat "$log"
  finish installed ""
  # Keep this version and the one before; untag older images of the app.
  before=$(sed -n "s/^[[:space:]]*\(export[[:space:]]*\)\{0,1\}${variable}[[:space:]]*=[[:space:]]*//p" "$state/env.before" | tail -n 1 | tr -d "\"'")
  docker image ls "$repository" --format '{{.Tag}}' | while read -r tag; do
    case $tag in "$version" | "$before" | '<none>') continue ;; esac
    if printf '%s\n' "$tag" | grep -Eqx '[0-9]+\.[0-9]+\.[0-9]+(-preview\.[0-9]+)?'; then
      docker image rm "$repository:$tag" > /dev/null 2>&1 || true
    fi
  done
  exit 0
fi
cat "$log" >&2
failure=$(last_error "$log")
echo "$target did not start: $failure. Going back." >&2
progress rolling-back
cat "$state/env.before" > "$env_file"
if compose "$@" up --detach --wait --wait-timeout 900 --quiet-pull > "$state/rollback.log" 2>&1; then
  finish rolled-back "$failure"
else
  cat "$state/rollback.log" >&2
  finish failed "$failure. The version before did not start again either: $(last_error "$state/rollback.log")"
fi
exit 1
EOF
chmod 0755 "$lib/deploy.new"
mv "$lib/deploy.new" "$lib/deploy"

cat > "$units/$unit.path" << EOF
# Meldgrove Updater: waits for the updater of $project to ask for a deployment.
[Unit]
Description=Meldgrove Updater: wait for new releases of $project

[Path]
PathExists=$trigger/deploy
Unit=$unit.service

[Install]
WantedBy=multi-user.target
EOF
cat > "$units/$unit.service" << EOF
# Meldgrove Updater: installs the release that the updater of $project asks for.
[Unit]
Description=Meldgrove Updater: install a new release of $project
Requires=docker.service
After=docker.service

[Service]
Type=oneshot
ExecStart=$lib/deploy $project
TimeoutStartSec=45min
EOF

# The updater runs as an unprivileged user and writes its requests here.
variable=$(inspect '{{range .Config.Env}}{{println .}}{{end}}' | sed -n 's/^VERSION_VARIABLE=//p' | head -n 1)
owner=$(inspect '{{.Config.User}}')
chown "${owner:-65534:65534}" "$trigger"
rm -f "$trigger/deploy"
printf 'protocol=2\nproject=%s\n' "$project" > "$trigger/watched"
chmod 0644 "$trigger/watched"
systemctl daemon-reload
systemctl enable --now "$unit.path" > /dev/null 2>&1
# A new updater container reads that the host watches it.
"$lib/deploy" "$project" updater

cat << EOF
Automatic updates of $project are on.
- The updater checks for new releases every hour; its log says what it does:
    docker compose logs updater
- Each installation: journalctl -u $unit.service
- Switch them off: sudo sh $0 remove
EOF
if [ -z "$variable" ]; then
  echo "- compose.yaml sets no VERSION_VARIABLE: the updater follows the image tag, and the app cannot choose the channel."
else
  echo "- The server writes each version it installs to $variable in .env."
fi
