#!/usr/bin/env bash
# Shared by the installer and management CLI. A separate invocation for each service also
# serializes builds on older Buildx versions that do not honor Compose's concurrency limit.
dc() { COMPOSE_PARALLEL_LIMIT=1 docker compose --parallel 1 "$@"; }

compose_serial_images() { # build|pull; enabled services come from Compose (including overrides).
  local mode="$1" services service total=0 done=0
  services=$(dc config --services) || return
  while IFS= read -r service; do [[ -z "$service" ]] || total=$((total + 1)); done <<<"$services"
  [[ $total -gt 0 ]] || { echo "No enabled Compose services" >&2; return 1; }
  while IFS= read -r service; do
    [[ -n "$service" ]] || continue
    compose_serial_progress "$mode" "$service" "$done" "$total" || return
    echo "==> $mode image: $service ($((done + 1))/$total)"
    if [[ "$mode" == "build" ]]; then
      dc build "$service" || return
      # Image-only services (e.g. Caddy) must be fetched before up --pull never.
      dc pull --ignore-buildable --policy missing "$service" || return
    else
      dc pull --policy missing "$service" || compose_release_fallback "$service" || return
    fi
    done=$((done + 1))
    compose_serial_progress "$mode" "$service" "$done" "$total" || return
  done <<<"$services"
}

compose_serial_up() {
  local services service total=0 done=0
  services=$(dc config --services) || return
  while IFS= read -r service; do [[ -z "$service" ]] || total=$((total + 1)); done <<<"$services"
  [[ $total -gt 0 ]] || return 1
  while IFS= read -r service; do
    [[ -n "$service" ]] || continue
    compose_serial_progress restart "$service" "$done" "$total" || return
    echo "==> Starting container: $service ($((done + 1))/$total)"
    # Dependencies remain managed by Compose, also with concurrency=1. Image preparation has
    # already finished: up must never trigger an implicit build or parallel image download.
    dc up -d --no-build --pull never --remove-orphans "$service" || return
    done=$((done + 1))
    compose_serial_progress restart "$service" "$done" "$total" || return
  done <<<"$services"
}

# Installations without a web update in progress only need the terminal messages.
compose_serial_progress() { :; }
compose_release_fallback() { return 1; }
