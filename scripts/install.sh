#!/usr/bin/env bash
# Intrabin one-command installer for Ubuntu (run as root):
#
#   bash <(curl -fsSL https://raw.githubusercontent.com/Doonkoob/intrabin/main/scripts/install.sh)
#
# It installs Docker + Docker Compose (if missing), clones Intrabin to /opt/intrabin, asks for an
# optional domain (HTTPS via Caddy + Let's Encrypt with automatic renewal), builds and starts the
# containers, creates an owner account with a random username and password, and installs the
# `intrabin` management menu. Running it again on an installed server updates it in place.
#
# Non-interactive use:
#   install.sh --yes --domain status.example.com --email ops@example.com
#   install.sh --yes --no-domain --expose          # plain HTTP on port 8080 (not recommended)
#
# Options:
#   --domain NAME        serve on https://NAME (the DNS A/AAAA record must point to this server)
#   --email ADDRESS      Let's Encrypt account e-mail (optional)
#   --no-domain          no domain: listen on port 8080
#   --expose             with --no-domain: publish port 8080 on all interfaces (default: ask)
#   --local-only         with --no-domain: bind to 127.0.0.1 only (use an SSH tunnel)
#   --port N             host port for plain HTTP mode (default 8080)
#   --no-runner          do not install the Action Runner (workflow script steps)
#   --docker-mirror URL  Docker registry mirror (e.g. https://docker.arvancloud.ir)
#   --goproxy URL        Go module proxy for the image build (e.g. https://goproxy.io,direct)
#   --npm-registry URL   npm registry for the image build
#   --release TAG        pull the published images for TAG (e.g. v1.0.0) instead of building
#                        (also checks out that tag); without it the images are built from source
#   --skip-build         use images that are already present (e.g. loaded with `docker load`)
#   --dir PATH           install directory (default /opt/intrabin)
#   --repo URL           git repository (default https://github.com/Doonkoob/intrabin)
#   --branch NAME        git branch (default main)
#   --yes                accept defaults, never prompt
set -Eeuo pipefail

REPO_URL="https://github.com/Doonkoob/intrabin"
BRANCH="main"
INSTALL_DIR="/opt/intrabin"
DOMAIN="" EMAIL="" MODE="" EXPOSE="" PORT="8080" RUNNER="yes" ASSUME_YES="no"
DOCKER_MIRROR="" GOPROXY_URL="" NPM_REGISTRY="" SKIP_BUILD="no" RELEASE=""

if [[ -t 1 ]]; then
  R=$'\e[31m' G=$'\e[32m' Y=$'\e[33m' B=$'\e[34m' C=$'\e[36m' W=$'\e[1m' N=$'\e[0m'
else
  R="" G="" Y="" B="" C="" W="" N=""
fi
info() { printf '%s==>%s %s\n' "$B" "$N" "$*"; }
ok() { printf '%s ✔%s %s\n' "$G" "$N" "$*"; }
warn() { printf '%s !%s %s\n' "$Y" "$N" "$*" >&2; }
die() { printf '%s ✘ %s%s\n' "$R" "$*" "$N" >&2; exit 1; }
trap 'die "installation failed at line $LINENO (command: $BASH_COMMAND). Re-run the installer after fixing the cause; it is safe to run again."' ERR

usage() {
  cat <<'EOF'
Intrabin one-command installer for Ubuntu (run as root):

  bash <(curl -fsSL https://raw.githubusercontent.com/Doonkoob/intrabin/main/scripts/install.sh)

It installs Docker + Docker Compose (if missing), clones Intrabin to /opt/intrabin, asks for an
optional domain (HTTPS via Caddy + Let's Encrypt with automatic renewal), builds and starts the
containers, creates an owner account with a random username and password, and installs the
`intrabin` management menu. Running it again on an installed server updates it in place.

Non-interactive use:
  install.sh --yes --domain status.example.com --email ops@example.com
  install.sh --yes --no-domain --expose          # plain HTTP on port 8080 (not recommended)

Options:
  --domain NAME        serve on https://NAME (the DNS A/AAAA record must point to this server)
  --email ADDRESS      Let's Encrypt account e-mail (optional)
  --no-domain          no domain: listen on port 8080
  --expose             with --no-domain: publish port 8080 on all interfaces (default: ask)
  --local-only         with --no-domain: bind to 127.0.0.1 only (use an SSH tunnel)
  --port N             host port for plain HTTP mode (default 8080)
  --no-runner          do not install the Action Runner (workflow script steps)
  --docker-mirror URL  Docker registry mirror (e.g. https://docker.arvancloud.ir)
  --goproxy URL        Go module proxy for the image build (e.g. https://goproxy.io,direct)
  --npm-registry URL   npm registry for the image build
  --skip-build         use images that are already present (e.g. loaded with `docker load`)
  --dir PATH           install directory (default /opt/intrabin)
  --repo URL           git repository (default https://github.com/Doonkoob/intrabin)
  --branch NAME        git branch (default main)
  --yes                accept defaults, never prompt
EOF
  exit 0
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --domain) DOMAIN="${2:-}"; MODE="domain"; shift 2 ;;
    --email) EMAIL="${2:-}"; shift 2 ;;
    --no-domain) MODE="ip"; shift ;;
    --expose) EXPOSE="yes"; shift ;;
    --local-only) EXPOSE="no"; shift ;;
    --port) PORT="${2:-}"; shift 2 ;;
    --no-runner) RUNNER="no"; shift ;;
    --docker-mirror) DOCKER_MIRROR="${2:-}"; shift 2 ;;
    --goproxy) GOPROXY_URL="${2:-}"; shift 2 ;;
    --npm-registry) NPM_REGISTRY="${2:-}"; shift 2 ;;
    --skip-build) SKIP_BUILD="yes"; shift ;;
    --release) RELEASE="${2:-}"; shift 2 ;;
    --dir) INSTALL_DIR="${2:-}"; shift 2 ;;
    --repo) REPO_URL="${2:-}"; shift 2 ;;
    --branch) BRANCH="${2:-}"; shift 2 ;;
    --yes|-y) ASSUME_YES="yes"; shift ;;
    -h|--help) usage ;;
    *) die "unknown option: $1 (see --help)" ;;
  esac
done

# Prompts read from the terminal even when the script itself comes from a pipe.
TTY=""
if [[ "$ASSUME_YES" != "yes" ]] && { exec 3</dev/tty; } 2>/dev/null; then TTY="yes"; fi
ask() { # ask VAR "question" "default"
  local __var="$1" __q="$2" __def="${3:-}" __ans=""
  if [[ -n "$TTY" ]]; then
    read -r -u 3 -p "$(printf '%s?%s %s%s: ' "$C" "$N" "$__q" "${__def:+ [$__def]}")" __ans || true
  fi
  printf -v "$__var" '%s' "${__ans:-$__def}"
}
confirm() { # confirm "question" default(y|n)
  local a
  ask a "$1 (y/n)" "$2"
  [[ "$a" =~ ^[Yy] ]]
}

rand_hex() { openssl rand -hex "$1"; }
rand_pass() { # 20 characters from an unambiguous alphabet
  local p=""
  while [[ ${#p} -lt 20 ]]; do p+=$(openssl rand -base64 48 | tr -dc 'A-HJ-NP-Za-km-z2-9'); done
  printf '%s' "${p:0:20}"
}

# set_env FILE KEY VALUE: replace (or append) KEY=VALUE, keeping the file's other lines.
set_env() {
  local f="$1" k="$2" v="$3" tmp
  tmp=$(mktemp)
  if grep -qE "^#? ?${k}=" "$f"; then
    awk -v k="$k" -v v="$v" 'BEGIN{done=0} { if (!done && $0 ~ "^#? ?" k "=") { print k "=" v; done=1 } else print }' "$f" >"$tmp"
  else
    cat "$f" >"$tmp"; printf '%s=%s\n' "$k" "$v" >>"$tmp"
  fi
  cat "$tmp" >"$f"; rm -f "$tmp"
}
get_env() { grep -E "^${2}=" "$1" 2>/dev/null | tail -n1 | cut -d= -f2- || true; }

valid_domain() { [[ "$1" =~ ^([A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?\.)+[A-Za-z]{2,63}$ ]]; }

# ---------------------------------------------------------------------------------------------
check_system() {
  [[ $EUID -eq 0 ]] || die "run this installer as root (for example: sudo -i, then paste the command again)"
  [[ -r /etc/os-release ]] || die "cannot detect the operating system (/etc/os-release missing)"
  # shellcheck disable=SC1091
  . /etc/os-release
  OS_ID="${ID:-unknown}" OS_VERSION="${VERSION_ID:-}" OS_CODENAME="${VERSION_CODENAME:-${UBUNTU_CODENAME:-}}"
  case "$OS_ID" in
    ubuntu)
      case "$OS_VERSION" in
        20.04|22.04|24.04|24.10|25.04|25.10) ok "Detected Ubuntu $OS_VERSION ($OS_CODENAME)" ;;
        *) warn "Ubuntu $OS_VERSION is not tested; supported: 20.04, 22.04, 24.04 and newer. Continuing." ;;
      esac ;;
    debian) warn "Detected Debian $OS_VERSION; this installer targets Ubuntu but should work." ;;
    *) die "unsupported OS: ${PRETTY_NAME:-$OS_ID}. Use Ubuntu 20.04, 22.04 or 24.04." ;;
  esac
  ARCH=$(dpkg --print-architecture)
  [[ "$ARCH" == "amd64" || "$ARCH" == "arm64" ]] || die "unsupported CPU architecture: $ARCH (amd64 or arm64 required)"
  local mem_mb
  mem_mb=$(awk '/MemTotal/ {print int($2/1024)}' /proc/meminfo)
  [[ "$mem_mb" -ge 900 ]] || warn "only ${mem_mb} MB RAM: building the images needs about 1.5 GB (add swap if the build fails)"
  local free_gb
  free_gb=$(df -Pk / | awk 'NR==2 {print int($4/1048576)}')
  [[ "$free_gb" -ge 3 ]] || warn "only ${free_gb} GB free disk space; at least 3 GB is recommended"
}

apt_install() { DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends "$@" >/dev/null; }

install_base_packages() {
  info "Installing base packages (git, curl, openssl, ca-certificates)…"
  DEBIAN_FRONTEND=noninteractive apt-get update -qq >/dev/null || warn "apt-get update reported errors; continuing"
  apt_install ca-certificates curl git openssl gnupg iproute2 || die "could not install base packages"
  ok "Base packages installed"
}

install_docker() {
  if command -v docker >/dev/null 2>&1; then
    ok "Docker is installed ($(docker --version | cut -d, -f1))"
  else
    info "Installing Docker Engine…"
    local repo_ok="no"
    install -m 0755 -d /etc/apt/keyrings
    if curl -fsSL --max-time 30 "https://download.docker.com/linux/${OS_ID}/gpg" -o /etc/apt/keyrings/docker.asc; then
      chmod a+r /etc/apt/keyrings/docker.asc
      echo "deb [arch=${ARCH} signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/${OS_ID} ${OS_CODENAME} stable" >/etc/apt/sources.list.d/docker.list
      if DEBIAN_FRONTEND=noninteractive apt-get update -qq >/dev/null 2>&1 &&
        apt_install docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin; then
        repo_ok="yes"
      else
        rm -f /etc/apt/sources.list.d/docker.list
      fi
    fi
    if [[ "$repo_ok" != "yes" ]]; then
      warn "download.docker.com is not reachable; installing Docker from the Ubuntu repositories instead"
      DEBIAN_FRONTEND=noninteractive apt-get update -qq >/dev/null || true
      apt_install docker.io || die "could not install Docker"
    fi
    ok "Docker installed"
  fi
  systemctl enable --now docker >/dev/null 2>&1 || service docker start >/dev/null 2>&1 || true
  if ! docker compose version >/dev/null 2>&1; then
    info "Installing the Docker Compose plugin…"
    apt_install docker-compose-plugin 2>/dev/null || apt_install docker-compose-v2 2>/dev/null ||
      die "could not install Docker Compose v2 (docker compose). Install it and re-run the installer."
  fi
  ok "Docker Compose $(docker compose version --short 2>/dev/null || echo v2) is available"
  if ! docker buildx version >/dev/null 2>&1; then
    apt_install docker-buildx-plugin 2>/dev/null || apt_install docker-buildx 2>/dev/null ||
      warn "docker buildx is missing; image builds may fail on old Docker versions"
  fi
  docker info >/dev/null 2>&1 || die "the Docker daemon is not running (check: systemctl status docker)"
}

configure_docker_mirror() {
  [[ -n "$DOCKER_MIRROR" ]] || return 0
  local f=/etc/docker/daemon.json
  if [[ -s "$f" ]] && ! grep -q '^[[:space:]]*{[[:space:]]*}[[:space:]]*$' "$f"; then
    if grep -q "$DOCKER_MIRROR" "$f"; then ok "Docker registry mirror already configured"; return 0; fi
    warn "$f already exists; add \"registry-mirrors\": [\"$DOCKER_MIRROR\"] to it yourself, then restart Docker"
    return 0
  fi
  mkdir -p /etc/docker
  printf '{\n  "registry-mirrors": ["%s"]\n}\n' "$DOCKER_MIRROR" >"$f"
  systemctl restart docker
  ok "Docker registry mirror set to $DOCKER_MIRROR"
}

fetch_source() {
  if [[ -n "$RELEASE" ]]; then
    [[ "$RELEASE" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$ ]] || die "invalid release tag: $RELEASE (expected e.g. v1.0.0)"
    BRANCH="$RELEASE"
  fi
  if [[ -d "$INSTALL_DIR/.git" ]]; then
    info "Updating the source in $INSTALL_DIR…"
    git -C "$INSTALL_DIR" fetch --depth 1 origin "$BRANCH" -q
    local local_branch="$BRANCH"
    [[ -n "$RELEASE" ]] && local_branch="release"  # a tag is checked out on a local branch of its own
    git -C "$INSTALL_DIR" checkout -q -f -B "$local_branch" FETCH_HEAD
  elif [[ -e "$INSTALL_DIR" && -n "$(ls -A "$INSTALL_DIR" 2>/dev/null)" ]]; then
    die "$INSTALL_DIR exists and is not an Intrabin checkout; choose another --dir"
  else
    info "Downloading Intrabin into $INSTALL_DIR…"
    git clone -q --depth 1 --branch "$BRANCH" "$REPO_URL" "$INSTALL_DIR" || die "could not clone $REPO_URL"
  fi
  chmod +x "$INSTALL_DIR"/scripts/*.sh
  ok "Source ready ($(git -C "$INSTALL_DIR" rev-parse --short HEAD))"
}

public_ip() {
  curl -fsS4 --max-time 5 https://api.ipify.org 2>/dev/null || curl -fsS4 --max-time 5 https://ifconfig.me 2>/dev/null ||
    ip -4 route get 1.1.1.1 2>/dev/null | awk '{for(i=1;i<=NF;i++) if ($i=="src") print $(i+1)}' || true
}

port_busy() { ss -ltnH "sport = :$1" 2>/dev/null | grep -q . ; }

choose_mode() {
  if [[ -z "$MODE" ]]; then
    echo
    printf '%sAccess%s\n' "$W" "$N"
    echo "  With a domain, Intrabin is served over HTTPS with a free Let's Encrypt certificate that renews automatically."
    echo "  The domain's DNS A (or AAAA) record must already point to this server."
    ask DOMAIN "Domain for Intrabin (leave empty to use the server IP)" ""
    if [[ -n "$DOMAIN" ]]; then MODE="domain"; else MODE="ip"; fi
  fi
  if [[ "$MODE" == "domain" ]]; then
    DOMAIN="${DOMAIN,,}"; DOMAIN="${DOMAIN%.}"
    valid_domain "$DOMAIN" || die "invalid domain: $DOMAIN"
    [[ -n "$EMAIL" ]] || ask EMAIL "E-mail for Let's Encrypt expiry notices (optional)" ""
    if [[ -n "$EMAIL" && ! "$EMAIL" =~ ^[^@[:space:]]+@[^@[:space:]]+\.[^@[:space:]]+$ ]]; then die "invalid e-mail: $EMAIL"; fi
    local me resolved
    me=$(public_ip)
    resolved=$(getent ahostsv4 "$DOMAIN" 2>/dev/null | awk 'NR==1 {print $1}' || true)
    if [[ -z "$resolved" ]]; then
      warn "$DOMAIN does not resolve yet. The certificate is issued once its A record points to ${me:-this server}."
    elif [[ -n "$me" && "$resolved" != "$me" ]]; then
      warn "$DOMAIN resolves to $resolved but this server's public IP looks like $me."
      warn "If the domain is behind a CDN proxy (Cloudflare orange cloud / ArvanCloud cloud), certificate issuance can fail; use DNS-only while installing."
    else
      ok "$DOMAIN points to this server ($resolved)"
    fi
    for p in 80 443; do
      if port_busy "$p" && ! docker ps --format '{{.Names}}' 2>/dev/null | grep -q '^intrabin-caddy'; then
        die "port $p is already in use (another web server?). Free ports 80 and 443, or install without a domain."
      fi
    done
  else
    [[ "$PORT" =~ ^[0-9]+$ && "$PORT" -ge 1 && "$PORT" -le 65535 ]] || die "invalid port: $PORT"
    if [[ -z "$EXPOSE" ]]; then
      warn "Without a domain the UI is served over plain HTTP: passwords cross the network unencrypted."
      if confirm "Publish port $PORT on all interfaces so the UI is reachable at http://<server-ip>:$PORT" "y"; then EXPOSE="yes"; else EXPOSE="no"; fi
    fi
  fi
  if [[ "$ASSUME_YES" != "yes" && "$RUNNER" == "yes" ]]; then
    confirm "Install the Action Runner (lets workflows run Bash/Python scripts in an isolated container)" "y" || RUNNER="no"
  fi
}

write_config() {
  local env="$INSTALL_DIR/.env" profiles=()
  umask 077
  if [[ ! -f "$env" ]]; then
    cp "$INSTALL_DIR/.env.example" "$env"
    set_env "$env" INTRABIN_MASTER_KEY "$(rand_hex 32)"
    set_env "$env" INTRABIN_NODE_NAME "$(hostname -s 2>/dev/null || echo intrabin-1)"
  fi
  set_env "$env" INTRABIN_SETUP_TOKEN "$(rand_hex 16)"
  if [[ "$MODE" == "domain" ]]; then
    set_env "$env" INTRABIN_DOMAIN "$DOMAIN"
    set_env "$env" ACME_EMAIL "$EMAIL"
    set_env "$env" INTRABIN_BIND 127.0.0.1
    set_env "$env" INTRABIN_PORT 8080
    set_env "$env" INTRABIN_TRUSTED_PROXIES "172.16.0.0/12,192.168.0.0/16,10.0.0.0/8"
    set_env "$env" INTRABIN_COOKIE_SECURE true
    set_env "$env" INTRABIN_ALLOWED_HOSTS "$DOMAIN,127.0.0.1,localhost"  # DNS-rebinding protection
    profiles+=(proxy)
  else
    set_env "$env" INTRABIN_DOMAIN ""
    set_env "$env" INTRABIN_ALLOWED_HOSTS ""
    set_env "$env" INTRABIN_PORT "$PORT"
    if [[ "$EXPOSE" == "yes" ]]; then set_env "$env" INTRABIN_BIND 0.0.0.0; else set_env "$env" INTRABIN_BIND 127.0.0.1; fi
    set_env "$env" INTRABIN_COOKIE_SECURE false
  fi
  if [[ "$RUNNER" == "yes" ]]; then
    local tok
    tok=$(get_env "$env" INTRABIN_RUNNER_TOKEN)
    [[ ${#tok} -ge 24 ]] || tok=$(rand_hex 32)
    set_env "$env" INTRABIN_RUNNER_URL "http://runner:8090"
    set_env "$env" INTRABIN_RUNNER_TOKEN "$tok"
    profiles+=(automation)
  else
    set_env "$env" INTRABIN_RUNNER_URL ""
  fi
  [[ -n "$GOPROXY_URL" ]] && set_env "$env" GOPROXY_URL "$GOPROXY_URL"
  [[ -n "$NPM_REGISTRY" ]] && set_env "$env" NPM_REGISTRY "$NPM_REGISTRY"
  set_env "$env" COMPOSE_PROFILES "$(IFS=,; echo "${profiles[*]}")"
  set_release_images "$env"
  chmod 600 "$env"
  write_caddyfile
  umask 022
  ok "Configuration written to $env (mode 600; it contains the master key — back it up)"
}

# set_release_images FILE: point Compose at the published images of --release, or back to local builds.
set_release_images() {
  local env="$1" owner
  if [[ -n "$RELEASE" ]]; then
    owner=$(sed -E 's#^https?://github\.com/([^/]+)/.*#\1#' <<<"$REPO_URL" | tr '[:upper:]' '[:lower:]')
    set_env "$env" INTRABIN_IMAGE "ghcr.io/$owner/intrabin:$RELEASE"
    set_env "$env" INTRABIN_RUNNER_IMAGE "ghcr.io/$owner/intrabin-runner:$RELEASE"
    set_env "$env" INTRABIN_VERSION "$RELEASE"
  else
    set_env "$env" INTRABIN_IMAGE ""
    set_env "$env" INTRABIN_RUNNER_IMAGE ""
    set_env "$env" INTRABIN_VERSION "$(git -C "$INSTALL_DIR" rev-parse --short HEAD)"
  fi
}

write_caddyfile() {
  local f="$INSTALL_DIR/deploy/Caddyfile" d e
  d=$(get_env "$INSTALL_DIR/.env" INTRABIN_DOMAIN)
  e=$(get_env "$INSTALL_DIR/.env" ACME_EMAIL)
  mkdir -p "$INSTALL_DIR/deploy"
  {
    echo "# Generated by Intrabin's installer / CLI. Certificates are obtained and renewed automatically."
    if [[ -n "$e" ]]; then printf '{\n\temail %s\n}\n\n' "$e"; fi
    printf '%s {\n\tencode zstd gzip\n\treverse_proxy intrabin:8080\n}\n' "${d:-localhost}"
  } >"$f"
  chmod 644 "$f"
}

open_firewall() {
  command -v ufw >/dev/null 2>&1 || return 0
  ufw status 2>/dev/null | grep -q "Status: active" || return 0
  if [[ "$MODE" == "domain" ]]; then
    ufw allow 80/tcp >/dev/null && ufw allow 443/tcp >/dev/null && ufw allow 443/udp >/dev/null
    ok "UFW: opened ports 80 and 443"
  elif [[ "$EXPOSE" == "yes" ]]; then
    ufw allow "$PORT/tcp" >/dev/null
    ok "UFW: opened port $PORT"
  fi
}

start_services() {
  cd "$INSTALL_DIR"
  if [[ "$SKIP_BUILD" == "yes" ]]; then
    info "Skipping the image build (--skip-build)"
  elif [[ -n "$RELEASE" ]]; then
    info "Pulling the $RELEASE images…"
    docker compose pull || die "could not pull the $RELEASE images (does the release exist? Use --docker-mirror behind restricted networks)"
  else
    info "Building the images (first install takes a few minutes)…"
    docker compose build || die "image build failed. Behind restricted networks use --docker-mirror, --goproxy and --npm-registry."
  fi
  info "Starting the containers…"
  docker compose up -d --remove-orphans
  info "Waiting for Intrabin to become healthy…"
  local port
  port=$(get_env .env INTRABIN_PORT)
  for _ in $(seq 1 90); do
    if curl -fsS --max-time 2 "http://127.0.0.1:${port:-8080}/api/v1/health" >/dev/null 2>&1; then
      ok "Intrabin is running"
      return 0
    fi
    sleep 2
  done
  docker compose logs --tail 50 intrabin || true
  die "Intrabin did not become healthy within 3 minutes (logs above)"
}

ADMIN_USER="" ADMIN_PASS=""
create_owner() {
  local port status token body code
  port=$(get_env "$INSTALL_DIR/.env" INTRABIN_PORT)
  status=$(curl -fsS "http://127.0.0.1:${port:-8080}/api/v1/setup/status")
  if ! grep -q '"needs_setup":true' <<<"$status"; then
    ok "An owner account already exists (use 'intrabin' → change password if you lost it)"
    return 0
  fi
  token=$(get_env "$INSTALL_DIR/.env" INTRABIN_SETUP_TOKEN)
  ADMIN_USER="admin-$(rand_hex 3)"
  ADMIN_PASS=$(rand_pass)
  body=$(printf '{"setup_token":"%s","username":"%s","password":"%s","language":"en"}' "$token" "$ADMIN_USER" "$ADMIN_PASS")
  # The body (token + password) goes through stdin so it never appears in the process list.
  code=$(printf '%s' "$body" | curl -sS -o /dev/null -w '%{http_code}' -H 'Content-Type: application/json' -X POST --data-binary @- \
    "http://127.0.0.1:${port:-8080}/api/v1/setup")
  [[ "$code" == "201" ]] || die "could not create the owner account (HTTP $code)"
  # The setup token is useless once an owner exists; remove it from the configuration.
  set_env "$INSTALL_DIR/.env" INTRABIN_SETUP_TOKEN ""
  ok "Owner account created"
}

install_cli() {
  ln -sf "$INSTALL_DIR/scripts/intrabin-cli.sh" /usr/local/bin/intrabin
  chmod +x "$INSTALL_DIR/scripts/intrabin-cli.sh"
  {
    echo "INSTALL_DIR=$INSTALL_DIR"
    echo "INSTALLED_AT=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    [[ -n "$ADMIN_USER" ]] && echo "OWNER_USERNAME=$ADMIN_USER"
    grep -E '^OWNER_USERNAME=' "$INSTALL_DIR/.install-info" 2>/dev/null | head -n1 || true
  } | awk -F= '!seen[$1]++' >"$INSTALL_DIR/.install-info.tmp"
  mv "$INSTALL_DIR/.install-info.tmp" "$INSTALL_DIR/.install-info"
  chmod 600 "$INSTALL_DIR/.install-info"
  ok "Management menu installed: run 'intrabin'"
}

summary() {
  local url env="$INSTALL_DIR/.env" bind port d
  d=$(get_env "$env" INTRABIN_DOMAIN); bind=$(get_env "$env" INTRABIN_BIND); port=$(get_env "$env" INTRABIN_PORT)
  if [[ -n "$d" ]]; then url="https://$d"
  elif [[ "$bind" == "0.0.0.0" ]]; then url="http://$(public_ip):${port}"
  else url="http://127.0.0.1:${port}  (local only: ssh -L ${port}:127.0.0.1:${port} root@<server>)"; fi
  echo
  printf '%s╔══════════════════════════════════════════════════════════════╗%s\n' "$G" "$N"
  printf '%s║%s  %sIntrabin is ready%s\n' "$G" "$N" "$W" "$N"
  printf '%s╚══════════════════════════════════════════════════════════════╝%s\n' "$G" "$N"
  printf '  URL:       %s%s%s\n' "$W" "$url" "$N"
  if [[ -n "$ADMIN_USER" ]]; then
    printf '  Username:  %s%s%s\n' "$W" "$ADMIN_USER" "$N"
    printf '  Password:  %s%s%s\n' "$W" "$ADMIN_PASS" "$N"
    printf '  %sSave this password now: it is not stored anywhere and is shown only once.%s\n' "$Y" "$N"
  fi
  [[ -n "$d" ]] && printf '  The HTTPS certificate is issued on first access and renewed automatically.\n'
  printf '  Manage the service: %sintrabin%s  (status, logs, password, domain, backup, update…)\n' "$W" "$N"
  printf '  Back up %s/.env (master key) together with your backups.\n\n' "$INSTALL_DIR"
}

main() {
  printf '\n%sIntrabin installer%s — uptime monitoring, incidents and recovery automation\n\n' "$W" "$N"
  check_system
  install_base_packages
  install_docker
  configure_docker_mirror
  local existing="no"
  [[ -f "$INSTALL_DIR/.env" ]] && existing="yes"
  fetch_source
  if [[ "$existing" == "yes" && -z "$MODE" ]]; then
    info "Existing installation found: keeping its configuration and updating"
    set_release_images "$INSTALL_DIR/.env"
    write_caddyfile
  else
    choose_mode
    write_config
    open_firewall
  fi
  start_services
  create_owner
  install_cli
  summary
}

main "$@"
