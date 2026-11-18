#!/usr/bin/env bash
# Shared host-agent setup. Can also repair an existing installation without rebuilding images.
update_agent_systemd_available() {
  command -v systemctl >/dev/null 2>&1 && [[ -d /run/systemd/system ]]
}

update_agent_ready() {
  update_agent_systemd_available && {
    systemctl is-active --quiet intrabin-update.path || systemctl is-active --quiet intrabin-update.timer
  }
}

install_update_agent_units() { # INSTALL_DIR [unit directory; tests only]
  local install_dir="$1" unit_dir="${2:-/etc/systemd/system}"
  [[ ! -L "$install_dir/run" && ! -L "$install_dir/run/update" ]] || return 1
  install -d -m 0755 "$install_dir/run" || return
  install -d -m 0700 "$install_dir/run/update" || return
  chown 65532:65532 "$install_dir/run/update" || return
  cat >"$unit_dir/intrabin-update.service" <<EOF || return
[Unit]
Description=Install an Intrabin update requested from the web UI
After=docker.service

[Service]
Type=oneshot
Environment="INTRABIN_DIR=$install_dir"
ExecStart=/usr/local/bin/intrabin apply-update
TimeoutStartSec=3600
EOF
  cat >"$unit_dir/intrabin-update.path" <<EOF || return
[Unit]
Description=Watch for Intrabin update requests from the web UI

[Path]
PathExists=$install_dir/run/update/request
Unit=intrabin-update.service

[Install]
WantedBy=multi-user.target
EOF
  cat >"$unit_dir/intrabin-update.timer" <<'EOF' || return
[Unit]
Description=Recover queued Intrabin updates if the path watcher misses a request

[Timer]
OnBootSec=30s
OnUnitInactiveSec=30s
AccuracySec=5s
Unit=intrabin-update.service

[Install]
WantedBy=timers.target
EOF
  systemctl daemon-reload || return
  systemctl reset-failed intrabin-update.path intrabin-update.service >/dev/null 2>&1 || true
  systemctl enable --now intrabin-update.path intrabin-update.timer || return
  systemctl restart intrabin-update.path || return
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  set -Eeuo pipefail
  [[ $EUID -eq 0 ]] || { echo "Run as root." >&2; exit 1; }
  install_dir=$(cd "${1:-/opt/intrabin}" && pwd)
  [[ -f "$install_dir/.env" && -f "$install_dir/compose.yaml" && -f "$install_dir/scripts/intrabin-cli.sh" ]] || { echo "No installation in $install_dir" >&2; exit 1; }
  update_agent_systemd_available || { echo "systemd is not available on this host." >&2; exit 1; }
  chmod +x "$install_dir/scripts/intrabin-cli.sh"
  ln -sf "$install_dir/scripts/intrabin-cli.sh" /usr/local/bin/intrabin
  install_update_agent_units "$install_dir"
  INTRABIN_DIR="$install_dir" /usr/local/bin/intrabin agent-info
  echo "Update agent enabled. Existing queued requests will be picked up automatically."
fi
