#!/usr/bin/env bash
# Deploys one server: copies its folder under deploy/, writes its .env, pulls
# the images and restarts what changed. Run from the repo root by CI.
#
#   deploy/deploy.sh <folder> <host> [VAR ...]
#
# Each VAR is copied from this script's environment into the server's .env.
# A VAR ending in "?" may be empty (an optional setting); the rest must be set.
# SSH as the `deploy` user must already be set up (CI does that from secrets).
set -euo pipefail

dir=$1 host=$2
shift 2
remote=/opt/filing-radar/$dir

echo "::group::deploy $dir to $host"
ssh "$host" "mkdir -p $remote"
scp -q deploy/"$dir"/* "$host:$remote/"

for var in "$@"; do
  if [[ $var == *\? ]]; then
    var=${var%\?}
    printf '%s=%s\n' "$var" "${!var:-}"
  else
    printf '%s=%s\n' "$var" "${!var:?$var is not set}"
  fi
done | ssh "$host" "umask 077 && cat > $remote/.env"

# Our images live in a private registry; log in just long enough to pull.
if [ -n "${GHCR_TOKEN:-}" ]; then
  echo "$GHCR_TOKEN" | ssh "$host" "docker login ghcr.io -u '$GHCR_USER' --password-stdin" >/dev/null
fi
ssh "$host" "cd $remote && docker compose pull -q && docker compose up -d --remove-orphans; s=\$?; docker logout ghcr.io >/dev/null 2>&1; exit \$s"
ssh "$host" "cd $remote && docker compose ps --format '{{.Service}}: {{.Status}}'"
echo "::endgroup::"
