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
# updater asks for: it pulls the image, dumps the database with the app's
# backup agent (the two newest dumps stay in
# /var/backups/meldgrove-updater/<project>), writes the version to the
# version variable in .env, runs docker compose up --wait, and goes back to
# the version before when the app does not become healthy. The updater
# keeps running without access to Docker. Running the setup again updates
# the deploy script.
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
# Protocol 2. The deploy script of updater 0.1 ($lib/deploy) stays as it is
# for apps on this server that still run updater 0.1.
deploy=$lib/deploy-2
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
  # An installation that runs keeps its backup of .env until it ends.
  while [ "$(systemctl show --property=ActiveState --value "$unit.service" 2> /dev/null)" = activating ] ||
    [ -n "$(systemctl list-jobs --no-legend "$unit.service" 2> /dev/null)" ]; do
    echo "Waiting for the running installation to end."
    sleep 5
  done
  rm -f "$units/$unit.path" "$units/$unit.service"
  systemctl daemon-reload
  if [ -n "$container" ] && docker exec "$container" updater host-unwatch 2> /dev/null; then
    :
  elif [ -n "$trigger" ]; then
    # rm removes links themselves, never what they point to.
    rm -f "$trigger/watched" "$trigger/deploy" "$trigger/progress" "$trigger/result"
  fi
  rm -rf "/var/lib/meldgrove-updater/$project"
  # A new updater container reads that no host watches it any more.
  if [ -n "$container" ] && [ -x "$deploy" ]; then
    "$deploy" "$project" updater
  elif [ -n "$container" ] && [ -x "$lib/deploy" ]; then
    "$lib/deploy" "$project" updater
  fi
  if ! ls "$units"/meldgrove-updater-*.path > /dev/null 2>&1; then
    rm -rf "$lib"
    rmdir /var/lib/meldgrove-updater 2> /dev/null || true
  fi
  echo "Automatic updates of $project are off."
  [ ! -d "/var/backups/meldgrove-updater/$project" ] ||
    echo "The database dumps before updates stay in /var/backups/meldgrove-updater/$project."
  exit 0
fi

[ -n "$trigger" ] || fail "The updater has no volume at /trigger. Update compose.yaml to a release of the app that has one, then run docker compose up -d."
case $trigger in *[!A-Za-z0-9._/@+-]*) fail "The volume's path has characters that a systemd unit cannot hold: $trigger" ;; esac

mkdir -p "$lib"
cat > "$deploy.new" << 'EOF'
#!/bin/sh
# Meldgrove Updater (host protocol 2), written by its host setup: installs
# what the updater of a Docker Compose project asks for. systemd starts it
# when the updater leaves its request in the volume at /trigger.
#
#   deploy <project>           install the requested version (or, for a
#                              request without version, the newest image of
#                              the tag)
#   deploy <project> updater   recreate only the updater container
#
# A version is installed by pulling the image, dumping the database, writing
# the version to the variable VERSION_VARIABLE (from the updater's
# environment) in the env file, and docker compose up --wait. When that
# fails, the env file goes back to what it was and the version before starts
# again; the database stays as the new version left it, and the message
# names the dump.
#
# The dump: when the project has a backup-agent service (the Meldgrove
# Backup Agent) and no env file sets UPDATE_BACKUP=off, the agent's image
# writes pg_dump of the app's database to
# /var/backups/meldgrove-updater/<project>/<project>-<version before>-<UTC time>.dump
# (root only); the two newest stay. When it fails, nothing is installed.
#
# The volume belongs to the unprivileged updater, so this script, which runs
# as root, never reads or writes it: it takes the request and reports the
# result with docker exec, as the updater's own user.
set -eu
project=${1:?Usage: deploy <project> [updater]}
mode=${2:-app}
state=/var/lib/meldgrove-updater/$project
# The updater container: the newest one, also while stopped.
# Waits up to about a minute: Compose may be recreating it.
container=""
for wait in 2 4 8 16 30 0; do
  container=$(docker ps --all --quiet --filter "label=com.docker.compose.project=$project" \
    --filter label=com.docker.compose.service=updater | head -n 1)
  [ -z "$container" ] || break
  sleep "$wait"
done
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
# updater <command…>: run the updater's command in its running container.
updater() {
  running=$(docker ps --quiet --filter "label=com.docker.compose.project=$project" \
    --filter label=com.docker.compose.service=updater | head -n 1)
  [ -n "$running" ] && docker exec "$running" updater "$@"
}

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
env_list=""
if [ -n "$env_files" ]; then
  IFS=,
  for env_file in $env_files; do
    case $env_file in /*) ;; *) env_file=$directory/$env_file ;; esac
    env_list="$env_list$env_file
"
    set -- "$@" --env-file "$env_file"
  done
  IFS=$old_ifs
fi
set +f
compose() {
  docker compose --ansi never --project-name "$project" --project-directory "$directory" "$@"
}
# has_agent: whether the project has the Meldgrove Backup Agent.
# Without interpolation as well, so that a setting the env file lacks hides
# no service.
has_agent() {
  { compose "$@" config --services 2> /dev/null || compose "$@" config --no-interpolate --services 2> /dev/null; } |
    grep -x backup-agent > /dev/null
}
# pull_agent: pull the backup agent's image, when the project has one. Its
# tag moves (0.1), and docker compose up pulls only missing images, so
# without this it would never update.
pull_agent() {
  if has_agent "$@"; then
    compose "$@" pull --quiet backup-agent > /dev/null 2>&1 ||
      echo "Could not pull the backup agent's image; it keeps the one it has." >&2
  fi
}

if [ "$mode" = updater ]; then
  # "$@" holds the --file and --env-file arguments.
  compose "$@" up --detach --no-deps --force-recreate --quiet-pull updater
  exit
fi

# The request, taken from the updater. docker exec fails while the updater
# restarts: try again for about a minute, then fail, and systemd starts this
# again while the request waits.
request=""
taken=false
for wait in 2 4 8 16 30 0; do
  if request=$(updater host-request 2> /dev/null); then
    taken=true
    break
  fi
  sleep "$wait"
done
if [ "$taken" != true ]; then
  echo "The updater did not hand over its request; trying again." >&2
  exit 1
fi
field() { printf '%s\n' "$request" | sed -n "s/^$1=//p" | head -n 1; }

if [ "$(field protocol)" != 2 ]; then
  # A request without version (updater 0.1, or no VERSION_VARIABLE): the
  # newest image of the tag.
  if [ "$(printf '%s\n' "$request" | wc -l)" != 1 ] || ! printf '%s\n' "$request" | grep -Eqx 'sha256:[0-9a-f]{64}'; then
    echo "No valid request from the updater; nothing to install."
    exit 0
  fi
  echo "Installing the newest image of $image."
  docker pull --quiet "$image"
  docker pull --quiet "$updater_image" || echo "Could not pull the updater's image; it keeps the one it has." >&2
  pull_agent "$@"
  compose "$@" up --detach --quiet-pull
  exit
fi

id=$(field id)
version=$(field version)
case $id in '' | *[!0-9a-f]*) echo "The request has no valid id." >&2 && exit 1 ;; esac

progress() { updater host-report progress "$id" "$1" > /dev/null 2>&1 || true; }
# The database dump of this installation, once written.
dump=""
# finish <result> <message>: the outcome for the updater, also in the journal.
# A failure names the dump first, so that the cut to 400 characters keeps it.
# Tried a few times: the updater container may still be starting.
finish() {
  text=$2
  if [ "$1" != installed ] && [ -n "$dump" ]; then
    text="Database before the update: $dump. $text"
  fi
  message=$(printf '%s' "$text" | tr '\r\n\t' '   ' | tr -cd '[:print:]' | cut -c 1-400)
  echo "$1: $version${message:+ ($message)}"
  for _ in 1 2 3 4 5 6; do
    if updater host-report result "$id" "$version" "$1" "$message" > /dev/null 2>&1; then
      return 0
    fi
    sleep 5
  done
  echo "Could not report the result to the updater." >&2
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
# The image's repository and the version that runs now, from the updater's
# IMAGE, never from the request.
name=${image%@*}
case ${name##*/} in *:*) repository=${name%:*} before=${name##*:} ;; *) repository=$name before="" ;; esac
target=$repository:$version

# The env file to change: the last one that sets the variable (Compose reads
# them in order), else the first, else .env.
assignment="^[[:space:]]*(export[[:space:]]+)?${variable}[[:space:]]*[=:]"
env_file=""
first_env=""
while IFS= read -r candidate; do
  [ -n "$candidate" ] || continue
  [ -n "$first_env" ] || first_env=$candidate
  if [ -f "$candidate" ] && grep -Eq "$assignment" "$candidate"; then
    env_file=$candidate
  fi
done << LIST
$env_list
LIST
env_file=${env_file:-${first_env:-$directory/.env}}
if [ ! -f "$env_file" ]; then
  finish failed "There is no $env_file to write $variable to."
  exit 1
fi
env_file=$(readlink -f "$env_file")
# env_value <name>: the value of the last assignment of <name> in the env
# files (the last file that sets it wins, as for the version variable), as
# Compose reads it: a quoted value up to its closing quote, otherwise
# without a comment and spaces at the end.
env_value() {
  line=""
  while IFS= read -r candidate; do
    [ -f "$candidate" ] || continue
    found=$(grep -E "^[[:space:]]*(export[[:space:]]+)?$1[[:space:]]*[=:]" "$candidate" | tail -n 1)
    [ -z "$found" ] || line=$found
  done << LIST
${env_list:-$directory/.env}
LIST
  value=$(printf '%s\n' "$line" | sed -E 's/^[^=:]*[=:][[:space:]]*//')
  case $value in
    \"*) value=${value#\"}; value=${value%%\"*} ;;
    \'*) value=${value#\'}; value=${value%%\'*} ;;
    *) value=$(printf '%s\n' "$value" | sed -E 's/[[:space:]]+#.*//; s/[[:space:]]*$//') ;;
  esac
  printf '%s\n' "$value"
}

mkdir -p "$state"
chmod 0700 "$state"
log=$state/install.log
# replace_env <file>: put the file's content in place of the env file, in
# one step, with the env file's owner and mode.
replace_env() {
  next=$(mktemp "$env_file.XXXXXX")
  if cat "$1" > "$next" && chmod --reference="$env_file" "$next" && chown --reference="$env_file" "$next" &&
    mv -f "$next" "$env_file"; then
    return 0
  fi
  rm -f "$next"
  return 1
}
# restart <log>: what docker compose up says, and whether it succeeded.
restart() { compose "$@" up --detach --wait --wait-timeout 900 --quiet-pull > "$log" 2>&1; }

echo "Installing $target."
progress pulling
if ! docker pull --quiet "$target" > "$log" 2>&1; then
  cat "$log" >&2
  finish failed "Could not download $target: $(last_error "$log")"
  exit 1
fi
docker pull --quiet "$updater_image" > /dev/null 2>&1 || echo "Could not pull the updater's image; it keeps the one it has." >&2
pull_agent "$@"

# The database before the update, dumped by the backup agent's image just
# pulled (or, when that pull failed, the one the server has). Nothing has
# changed yet: when the dump fails, the installation stops here.
backups=/var/backups/meldgrove-updater/$project
if [ "$(env_value UPDATE_BACKUP | tr '[:upper:]' '[:lower:]')" = off ]; then
  echo "UPDATE_BACKUP=off: no database backup before the update."
elif ! services=$(compose "$@" config --services 2> "$log") &&
  ! services=$(compose "$@" config --no-interpolate --services 2> /dev/null); then
  # Without the list, a missing backup-agent service and a missing setting
  # look the same: never skip the dump on a guess.
  cat "$log" >&2
  finish failed "Docker Compose cannot list the services, so the server cannot tell whether to back up the database: $(last_error "$log" | cut -c 1-200). To install without a backup, set UPDATE_BACKUP=off in .env."
  exit 1
elif ! printf '%s\n' "$services" | grep -x backup-agent > /dev/null; then
  echo "The project has no backup-agent service: no database backup before the update."
else
  progress backing-up
  # The version that runs now names the dump; for a tag such as 0.3, the
  # version label of the image a running container of the app was made from.
  label=$before
  if ! printf '%s\n' "$label" | grep -Eqx '[0-9]+\.[0-9]+\.[0-9]+(-preview\.[0-9]+)?'; then
    exact=$(docker ps --quiet --filter "label=com.docker.compose.project=$project" | while read -r running_container; do
      docker inspect --format '{{.Config.Image}} {{index .Config.Labels "org.opencontainers.image.version"}}' "$running_container"
    done | awk -v image="$name" '$1 == image { print $2; exit }')
    if printf '%s\n' "$exact" | grep -Eqx '[0-9]+\.[0-9]+\.[0-9]+(-preview\.[0-9]+)?'; then label=$exact; fi
  fi
  case $label in '' | *[!A-Za-z0-9._-]*) label=unknown ;; esac
  next=$backups/$project-$label-$(date -u +%Y%m%dT%H%M%SZ).dump
  echo "Backing up the database to $next."
  # Every error of these steps goes to the log, which names the reason.
  if {
    mkdir -p "$backups" && chmod 0700 /var/backups/meldgrove-updater "$backups" && rm -f "$backups"/*.partial &&
      (umask 077 && compose "$@" run --rm --no-deps -T backup-agent dump-database < /dev/null > "$next.partial") &&
      mv -f "$next.partial" "$next"
  } 2> "$log"; then
    dump=$next
    echo "Database before the update: $dump ($(du -h "$dump" | cut -f 1))."
    # Keep the two newest dumps; their names end in the time.
    for file in "$backups/$project"-*-????????T??????Z.dump; do
      printf '%s %s\n' "${file##*-}" "$file"
    done | sort -r | tail -n +3 | while read -r _ file; do
      rm -f "$file" || echo "Could not delete the old dump $file." >&2
    done
  else
    rm -f "$next.partial" 2> /dev/null || true
    cat "$log" >&2
    if grep -q '^Usage: backup-agent' "$log"; then
      reason="the backup agent's image is too old: it has no dump-database command"
    else
      reason=$(grep -v '^[[:space:]]*$' "$log" | tail -n 1 | cut -c 1-200 | sed 's/[.[:space:]]*$//')
    fi
    finish failed "The database backup before the update failed: ${reason:-no reason given}. ${before:-The version before} keeps running. To install without it, set UPDATE_BACKUP=off in .env."
    exit 1
  fi
fi

# Every assignment of the variable gets the version; it is added when missing.
cp "$env_file" "$state/env.before"
awk -v name="$variable" -v value="$version" '
  BEGIN { pattern = "^[ \t]*(export[ \t]+)?" name "[ \t]*[=:]" }
  $0 ~ pattern { print name "=" value; found = 1; next }
  { print }
  END { if (!found) print name "=" value }
' "$state/env.before" > "$state/env.next"
if ! replace_env "$state/env.next"; then
  finish failed "Could not write $env_file."
  exit 1
fi
# Compose must now run the version: another env file, the shell or a value
# that spans lines could still decide otherwise.
# The images of the app: references whose repository (without tag or
# digest) is the app's.
images=$(compose "$@" config --images 2> "$log" | awk -v repository="$repository" '{
    name = $0
    sub(/@.*/, "", name)
    if (match(name, /:[^\/]*$/)) name = substr(name, 1, RSTART - 1)
    if (name == repository) print
  }')
if [ -z "$images" ] || printf '%s\n' "$images" | grep -Fvxq "$target"; then
  replace_env "$state/env.before" || echo "Could not restore $env_file from $state/env.before." >&2
  finish failed "After writing $variable=$version to $env_file, Docker Compose still does not run $target. Check where $variable is set."
  exit 1
fi

progress starting
if restart "$@"; then
  cat "$log"
  finish installed ""
  # Keep this version and the one before; untag older images of the app.
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
if ! replace_env "$state/env.before"; then
  finish failed "$failure. Could not restore $env_file; the backup is $state/env.before."
  exit 1
fi
if restart "$@"; then
  finish rolled-back "$failure"
else
  cat "$log" >&2
  finish failed "$failure. The version before did not start again either: $(last_error "$log")"
fi
exit 1
EOF
chmod 0755 "$deploy.new"
mv "$deploy.new" "$deploy"

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
# The deploy script waits and retries itself; the path unit must never give up.
StartLimitIntervalSec=0

[Service]
Type=oneshot
ExecStart=$deploy $project
TimeoutStartSec=45min
EOF

# The updater runs as an unprivileged user and writes its requests here. It
# marks the volume as watched itself: root writes nothing into it.
variable=$(inspect '{{range .Config.Env}}{{println .}}{{end}}' | sed -n 's/^VERSION_VARIABLE=//p' | head -n 1)
owner=$(inspect '{{.Config.User}}')
chown "${owner:-65534:65534}" "$trigger"
[ "$(inspect '{{.State.Running}}')" = true ] || fail "The updater container does not run. Start the app first (docker compose up -d)."
docker exec "$container" updater host-watch "$project" ||
  fail "The updater cannot take the host setup. Update compose.yaml to updater 0.2 or later, then run docker compose up -d."
systemctl daemon-reload
systemctl enable --now "$unit.path" > /dev/null 2>&1
# A new updater container reads that the host watches it.
"$deploy" "$project" updater

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
  if docker compose config --services 2> /dev/null | grep -x backup-agent > /dev/null; then
    echo "- Before each installation, the server dumps the database to /var/backups/meldgrove-updater/$project and keeps the two newest dumps. UPDATE_BACKUP=off in .env switches this off."
  else
    echo "- compose.yaml has no backup-agent service, so the server dumps no database before an installation."
  fi
fi
