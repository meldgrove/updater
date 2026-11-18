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
# volume at /trigger, and a service that then pulls the app's image and runs
# docker compose up. The updater keeps running without access to Docker.
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
unit=meldgrove-updater-$project

if [ "$action" = remove ]; then
  systemctl disable --now "$unit.path" > /dev/null 2>&1 || true
  rm -f "$units/$unit.path" "$units/$unit.service"
  systemctl daemon-reload
  if [ -n "$trigger" ]; then
    rm -f "$trigger/watched" "$trigger/deploy"
  fi
  # A new updater container reads that no host watches it any more.
  if [ -n "$container" ] && [ -x "$lib/deploy" ]; then
    "$lib/deploy" "$project" updater
  fi
  if ! ls "$units"/meldgrove-updater-*.path > /dev/null 2>&1; then
    rm -rf "$lib"
  fi
  echo "Automatic updates of $project are off."
  exit 0
fi

[ -n "$trigger" ] || fail "The updater has no volume at /trigger. Update compose.yaml to a release of the app that has one, then run docker compose up -d."
case $trigger in *[!A-Za-z0-9._/@+-]*) fail "The volume's path has characters that a systemd unit cannot hold: $trigger" ;; esac

mkdir -p "$lib"
cat > "$lib/deploy.new" << 'EOF'
#!/bin/sh
# Meldgrove Updater, written by its host setup: installs the release that the
# updater of a Docker Compose project asks for. systemd starts it when the
# updater leaves its request in the volume at /trigger.
#
#   deploy <project>           pull the app's and the updater's images, then
#                              docker compose up
#   deploy <project> updater   recreate only the updater container
set -eu
project=${1:?Usage: deploy <project> [updater]}
mode=${2:-app}
container=$(docker ps --all --quiet --filter "label=com.docker.compose.project=$project" \
  --filter label=com.docker.compose.service=updater | head -n 1)
if [ -z "$container" ]; then
  echo "The Docker Compose project $project has no updater container." >&2
  exit 1
fi
inspect() { docker inspect --format "$1" "$container"; }
directory=$(inspect '{{index .Config.Labels "com.docker.compose.project.working_dir"}}')
files=$(inspect '{{index .Config.Labels "com.docker.compose.project.config_files"}}')

# The Compose files of the last docker compose up, as --file arguments.
set -f
old_ifs=$IFS
IFS=,
# shellcheck disable=SC2086 # the label separates the files with commas
set -- $files
IFS=$old_ifs
count=$#
for file do
  set -- "$@" --file "$file"
done
shift "$count"

if [ "$mode" = updater ]; then
  exec docker compose --project-name "$project" --project-directory "$directory" "$@" \
    up --detach --no-deps --force-recreate --quiet-pull updater
fi

trigger=$(inspect '{{range .Mounts}}{{if eq .Destination "/trigger"}}{{.Source}}{{end}}{{end}}')
[ -z "$trigger" ] || rm -f "$trigger/deploy"
# The image the updater watches, as the last docker compose up set it.
image=$(inspect '{{range .Config.Env}}{{println .}}{{end}}' | sed -n 's/^IMAGE=//p' | head -n 1)
echo "Installing the newest image of $image."
docker pull --quiet "$image"
docker pull --quiet "$(inspect '{{.Config.Image}}')" ||
  echo "Could not pull the updater's image; it keeps the one it has." >&2
docker compose --project-name "$project" --project-directory "$directory" "$@" up --detach --quiet-pull
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
EOF

# The updater runs as an unprivileged user and writes its request here.
owner=$(inspect '{{.Config.User}}')
chown "${owner:-65534:65534}" "$trigger"
rm -f "$trigger/deploy"
touch "$trigger/watched"
systemctl daemon-reload
systemctl enable --now "$unit.path" > /dev/null 2>&1
# A new updater container reads that the host watches it.
"$lib/deploy" "$project" updater

cat << EOF
Automatic updates of $project are on.
- The updater checks the image tag every hour; its log names the tag:
    docker compose logs updater
- Each installation: journalctl -u $unit.service
- Switch them off: sudo sh $0 remove
EOF
