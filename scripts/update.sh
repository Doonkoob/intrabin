#!/usr/bin/env bash
# Bootstrap a current updater without replacing the old installation before its backup.
# Usage: sudo bash update.sh vX.Y.Z [/opt/intrabin]
set -Eeuo pipefail
die() { echo "Update: $*" >&2; exit 1; }
[[ $EUID -eq 0 ]] || die "run as root"
tag="${1:-}"
install_dir="${2:-/opt/intrabin}"
[[ $# -le 2 && "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[a-zA-Z0-9.-]+)?$ ]] || die "usage: update.sh vX.Y.Z [/opt/intrabin]"
[[ -f "$install_dir/.env" && -f "$install_dir/compose.yaml" ]] || die "no installation in $install_dir"
git -C "$install_dir" rev-parse --git-dir >/dev/null || die "installation is not a Git checkout"
git -C "$install_dir" fetch --depth 1 origin "refs/tags/$tag:refs/tags/$tag"
commit=$(git -C "$install_dir" rev-parse "refs/tags/$tag^{commit}")
task_dir=$(mktemp -d)
trap 'rm -rf "$task_dir"' EXIT
for script in intrabin-cli.sh compose-serial.sh; do
  git -C "$install_dir" show "$commit:scripts/$script" >"$task_dir/$script"
done
INTRABIN_DIR="$install_dir" bash "$task_dir/intrabin-cli.sh" update --to "$tag" --prebuilt
