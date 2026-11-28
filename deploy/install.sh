#!/usr/bin/env bash
# fluid-agent installer for a Linux VM: checks the system, installs what is
# missing (Docker Engine + Compose plugin; optionally Go), and runs
# fluid-agent in a container.
#
#   sudo bash install.sh                 # interactive
#   curl -fsSL https://raw.githubusercontent.com/skpjr001/fluid-agent/main/deploy/install.sh | sudo bash
#   sudo bash install.sh --yes --expose-ui 127.0.0.1:9000     # unattended
#
# Nothing is published on the host unless you say so: the forward proxy
# (8080), the webhook listener (8081) and the console (9000) are asked about
# one by one, and each defaults to "not exposed".
#
# Re-running upgrades in place and keeps config, data and earlier answers.
# Options: run with --help.

set -euo pipefail

REPO="skpjr001/fluid-agent"
DIR="/opt/fluid-agent"
FA_VERSION="latest"       # release tag, or "latest"
MODE="release"         # release: verified release binary in a small image; source: build from git
ASSUME_YES=0
WITH_GO=""             # yes | no | "" (ask)
STORAGE=""             # volume | bind | "" (ask)
DATA_PATH=""           # host directory base for bind mounts
DEFAULT_DATA_PATH="/fluid/agent"   # default for --storage bind: <path>/data and <path>/dynamic
EXPOSE_PROXY="" EXPOSE_WEBHOOK="" EXPOSE_UI=""      # no | PORT | IP:PORT | "" (ask)
ALLOW_PROXY="" ALLOW_UI=""                          # comma-separated allowlists, "" = ask/keep
NETWORKS=""            # extra Docker networks, comma separated; "none" = only the default one; "" = ask/keep
UNINSTALL=0 PURGE=0 FORCE=0
CONTAINER="fluid-agent"
NONROOT_UID=65532      # distroless "nonroot"

# ------------------------------------------------------------------ output

if [ -t 1 ]; then B=$'\e[1m'; G=$'\e[32m'; Y=$'\e[33m'; R=$'\e[31m'; C=$'\e[36m'; N=$'\e[0m'; else B=""; G=""; Y=""; R=""; C=""; N=""; fi
info() { printf '%s==>%s %s\n' "$C" "$N" "$*"; }
ok()   { printf '%s ok%s  %s\n' "$G" "$N" "$*"; }
warn() { printf '%swarn%s %s\n' "$Y" "$N" "$*" >&2; }
die()  { printf '%serror%s %s\n' "$R" "$N" "$*" >&2; exit 1; }
hr()   { printf '%s\n' "------------------------------------------------------------------"; }

usage() {
  cat <<EOF
Usage: sudo bash install.sh [options]

  --dir PATH               install directory (default $DIR)
  --version TAG            release to install, e.g. v0.4.0 (default: latest)
  --source                 build the image from the git repository instead of the release binary
  --storage volume|bind    keep data in Docker volumes (default) or host directories
  --data-path PATH         base directory for --storage bind: PATH/data and PATH/dynamic (default /fluid/agent)
  --expose-proxy  no|PORT|IP:PORT   publish the forward proxy (container 8080)
  --expose-webhook no|PORT|IP:PORT  publish the webhook/ingest listener (container 8081)
  --expose-ui     no|PORT|IP:PORT   publish the console + API (container 9000)
                           PORT alone binds 127.0.0.1; use 0.0.0.0:PORT for all interfaces
  --allow-proxy LIST       client IPs allowed to use the proxy (comma separated; CIDRs, 10.0.*.* wildcards)
  --allow-ui LIST          client IPs allowed to use the console
  --networks LIST          also attach the container to these existing Docker networks (comma separated),
                           e.g. a reverse proxy's or your applications' network; "none" detaches them
  --with-go | --no-go      also install the Go toolchain on the host (not needed to run)
  --yes                    accept defaults, never prompt (nothing exposed unless --expose-*)
  --uninstall              stop and remove the container (keeps data); add --purge to delete data too
  --force                  continue even if there is too little free disk space
  -h, --help
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --dir) DIR="$2"; shift ;;
    --version) FA_VERSION="$2"; shift ;;
    --source) MODE="source" ;;
    --storage) STORAGE="$2"; shift ;;
    --data-path) DATA_PATH="$2"; shift ;;
    --expose-proxy) EXPOSE_PROXY="$2"; shift ;;
    --expose-webhook) EXPOSE_WEBHOOK="$2"; shift ;;
    --expose-ui) EXPOSE_UI="$2"; shift ;;
    --allow-proxy) ALLOW_PROXY="$2"; shift ;;
    --allow-ui) ALLOW_UI="$2"; shift ;;
    --networks) NETWORKS="$2"; shift ;;
    --with-go) WITH_GO="yes" ;;
    --no-go) WITH_GO="no" ;;
    --yes|-y) ASSUME_YES=1 ;;
    --uninstall) UNINSTALL=1 ;;
    --purge) PURGE=1 ;;
    --force) FORCE=1 ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; die "unknown option: $1" ;;
  esac
  shift
done
case "$STORAGE" in ""|volume|bind) ;; *) die "--storage must be volume or bind" ;; esac
case "$DIR" in /*) ;; *) DIR="$(pwd)/$DIR" ;; esac
DIR=${DIR%/}
case "$DIR" in ""|/|/bin|/boot|/dev|/etc|/home|/lib|/lib64|/opt|/proc|/root|/run|/sbin|/srv|/sys|/tmp|/usr|/var) die "--dir $DIR is not a safe install directory; use e.g. /opt/fluid-agent" ;; esac

# ------------------------------------------------------------------ prompts
# Prompts read from the terminal, so "curl ... | sudo bash" still works.

TTY=""
if [ "$ASSUME_YES" = 0 ] && { : </dev/tty; } 2>/dev/null; then TTY=/dev/tty; fi

ask() { # ask VAR "question" default
  local __var=$1 q=$2 def=${3-} ans=""
  if [ -n "$TTY" ]; then
    printf '%s%s%s [%s]: ' "$B" "$q" "$N" "$def" >"$TTY"
    IFS= read -r ans <"$TTY" || true
  fi
  printf -v "$__var" '%s' "${ans:-$def}"
}
yesno() { # yesno "question" y|n -> exit status
  local a
  ask a "$1 (y/n)" "$2"
  case "$a" in y|Y|yes|YES|Yes) return 0 ;; *) return 1 ;; esac
}

# ------------------------------------------------------------------ root

SELF=""
[ -f "$0" ] && SELF=$(readlink -f "$0")
if [ "$(id -u)" -ne 0 ]; then
  [ -n "$SELF" ] || die "run it as root: curl -fsSL <url> | sudo bash"
  command -v sudo >/dev/null || die "run as root (sudo is not installed)"
  info "re-running with sudo"
  exec sudo -E bash "$SELF" "$@"
fi

# ------------------------------------------------------------------ previous answers

STATE="$DIR/.install.env"
if [ -f "$STATE" ]; then
  # shellcheck disable=SC1090
  . "$STATE"
  [ "$UNINSTALL" = 1 ] || info "found an existing installation in $DIR (version ${PREV_VERSION:-?}); upgrading in place"
fi

# ------------------------------------------------------------------ uninstall

compose() { docker compose -f "$DIR/docker-compose.yml" --project-name fluid-agent "$@"; }

if [ "$UNINSTALL" = 1 ]; then
  [ -f "$DIR/docker-compose.yml" ] || die "no installation found in $DIR"
  info "stopping and removing the container"
  if [ "$PURGE" = 1 ]; then
    where="$DIR"
    [ "${PREV_STORAGE:-}" = bind ] && [ -n "${PREV_DATA_PATH:-}" ] && where="$DIR and $PREV_DATA_PATH"
    yesno "Delete ALL data (database, dynamic config, volumes) in $where? This cannot be undone." n || die "aborted"
    compose down --volumes --rmi local || true
    if [ "${PREV_STORAGE:-}" = bind ] && [ -n "${PREV_DATA_PATH:-}" ]; then
      rm -rf "${PREV_DATA_PATH:?}/data" "${PREV_DATA_PATH:?}/dynamic"
      rmdir "$PREV_DATA_PATH" 2>/dev/null || true
      [ "$PREV_DATA_PATH" = "$DEFAULT_DATA_PATH" ] && { rmdir "$(dirname "$PREV_DATA_PATH")" 2>/dev/null || true; }   # /fluid, when empty
    fi
    rm -rf "${DIR:?}"
    ok "removed everything"
  else
    compose down --rmi local || true
    ok "container removed; data kept in $DIR and its volumes (re-run the installer to start again)"
  fi
  exit 0
fi

# ------------------------------------------------------------------ system checks

hr; info "${B}checking the system${N}"
[ "$(uname -s)" = "Linux" ] || die "this installer is for Linux"
case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) die "unsupported CPU architecture $(uname -m) (amd64 and arm64 are supported)" ;;
esac
[ -r /etc/os-release ] || die "cannot read /etc/os-release"
# shellcheck disable=SC1091
. /etc/os-release
OS_ID="${ID:-unknown}"; OS_LIKE="${ID_LIKE:-}"
ok "OS: ${PRETTY_NAME:-$OS_ID} ($ARCH)"

HAS_SYSTEMD=0
[ -d /run/systemd/system ] && HAS_SYSTEMD=1
ok "init: $([ "$HAS_SYSTEMD" = 1 ] && echo systemd || echo 'no systemd (Docker will be started directly)')"

mem_mb=$(awk '/MemTotal/ {print int($2/1024)}' /proc/meminfo 2>/dev/null || echo 0)
[ "$mem_mb" -ge 512 ] && ok "memory: ${mem_mb} MiB" || warn "memory: ${mem_mb} MiB (512 MiB or more recommended)"
mkdir -p "$DIR"
# Docker images and the build need space where Docker keeps its data.
need_mb=1024; [ "$MODE" = source ] && need_mb=3072
docker_root=/var/lib/docker
command -v docker >/dev/null && docker_root=$(docker info -f '{{.DockerRootDir}}' 2>/dev/null || echo /var/lib/docker)
mkdir -p "$docker_root" 2>/dev/null || true
for d in "$DIR" "$docker_root"; do
  free=$(df -Pm "$d" 2>/dev/null | awk 'NR==2 {print $4}')
  if [ "${free:-0}" -ge "$need_mb" ]; then
    ok "free disk for $d: ${free} MiB"
  elif [ "$FORCE" = 1 ]; then
    warn "free disk for $d: ${free:-?} MiB (needs about $need_mb MiB); continuing because of --force"
  else
    die "only ${free:-?} MiB free for $d; about $need_mb MiB is needed ($MODE mode). Free some space, or use --force"
  fi
done

PKG=""
for p in apt-get dnf yum zypper apk; do command -v "$p" >/dev/null && { PKG=$p; break; }; done
[ -n "$PKG" ] || die "no supported package manager found (apt-get, dnf, yum, zypper, apk)"
ok "package manager: $PKG"

pkg_install() {
  case "$PKG" in
    apt-get) DEBIAN_FRONTEND=noninteractive apt-get install -y -q "$@" >/dev/null ;;
    dnf|yum) "$PKG" install -y -q "$@" >/dev/null ;;
    zypper) zypper --non-interactive install "$@" >/dev/null ;;
    apk) apk add --no-cache "$@" >/dev/null ;;
  esac
}
APT_UPDATED=0
need_cmds() { # need_cmds cmd:package ...
  local missing=() c
  for c in "$@"; do command -v "${c%%:*}" >/dev/null || missing+=("${c#*:}"); done
  [ ${#missing[@]} -eq 0 ] && return 0
  info "installing ${missing[*]}"
  if [ "$PKG" = apt-get ] && [ "$APT_UPDATED" = 0 ]; then apt-get update -q >/dev/null; APT_UPDATED=1; fi
  pkg_install "${missing[@]}"
}
need_cmds curl:curl tar:tar sha256sum:coreutils awk:gawk
[ "$MODE" = source ] && need_cmds git:git
ca_ok=0; [ -e /etc/ssl/certs/ca-certificates.crt ] || [ -e /etc/pki/tls/certs/ca-bundle.crt ] && ca_ok=1
[ "$ca_ok" = 1 ] || need_cmds update-ca-certificates:ca-certificates

curl -fsS --max-time 15 -o /dev/null https://github.com && ok "internet: github.com reachable" \
  || die "cannot reach https://github.com (check DNS, proxy or firewall)"

# ------------------------------------------------------------------ Docker

docker_running() { docker info >/dev/null 2>&1; }

install_docker() {
  info "installing Docker Engine + Compose plugin (official Docker packages)"
  local like="$OS_ID $OS_LIKE"
  case "$like" in
    *ubuntu*|*debian*)
      local repo_os=debian codename="${VERSION_CODENAME:-}"
      case "$like" in *ubuntu*) repo_os=ubuntu; codename="${UBUNTU_CODENAME:-$codename}" ;; esac
      [ "$OS_ID" = debian ] && repo_os=debian
      [ -n "$codename" ] || die "cannot determine the release codename for the Docker apt repository"
      [ "$APT_UPDATED" = 1 ] || { apt-get update -q >/dev/null; APT_UPDATED=1; }
      pkg_install ca-certificates curl gnupg
      install -m 0755 -d /etc/apt/keyrings
      curl -fsSL "https://download.docker.com/linux/$repo_os/gpg" -o /etc/apt/keyrings/docker.asc
      chmod a+r /etc/apt/keyrings/docker.asc
      echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/$repo_os $codename stable" \
        >/etc/apt/sources.list.d/docker.list
      apt-get update -q >/dev/null
      pkg_install docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
      ;;
    *amzn*)
      pkg_install docker
      install_compose_plugin
      ;;
    *fedora*|*rhel*|*centos*|*rocky*|*almalinux*|*ol*)
      local repo=centos
      case "$OS_ID" in fedora) repo=fedora ;; rhel) repo=rhel ;; esac
      local url="https://download.docker.com/linux/$repo/docker-ce.repo"
      if [ "$PKG" = dnf ]; then
        pkg_install dnf-plugins-core || true
        dnf config-manager --add-repo "$url" >/dev/null 2>&1 || dnf config-manager addrepo --from-repofile="$url" >/dev/null
      else
        pkg_install yum-utils; yum-config-manager --add-repo "$url" >/dev/null
      fi
      pkg_install docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
      ;;
    *suse*|*sles*)
      pkg_install docker
      install_compose_plugin
      ;;
    *alpine*)
      pkg_install docker docker-cli-compose
      ;;
    *) die "automatic Docker installation is not supported on $OS_ID; install Docker Engine and the Compose plugin (https://docs.docker.com/engine/install/), then re-run" ;;
  esac
}

# The Compose v2 plugin as a verified static binary, for distros without a package.
install_compose_plugin() {
  docker compose version >/dev/null 2>&1 && return 0
  local tag a url dest=/usr/local/lib/docker/cli-plugins
  tag=$(curl -fsSL https://api.github.com/repos/docker/compose/releases/latest | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1)
  [ -n "$tag" ] || die "cannot find the latest Docker Compose release"
  a=$([ "$ARCH" = amd64 ] && echo x86_64 || echo aarch64)
  url="https://github.com/docker/compose/releases/download/$tag/docker-compose-linux-$a"
  mkdir -p "$dest"
  curl -fsSL "$url" -o "$dest/docker-compose"
  curl -fsSL "$url.sha256" | awk '{print $1"  '"$dest"'/docker-compose"}' | sha256sum -c --quiet - || die "Docker Compose checksum mismatch"
  chmod 0755 "$dest/docker-compose"
}

start_docker() {
  docker_running && return 0
  if [ "$HAS_SYSTEMD" = 1 ]; then
    systemctl enable --now docker >/dev/null 2>&1 || systemctl start docker
  elif command -v rc-service >/dev/null; then
    rc-update add docker default >/dev/null 2>&1 || true; rc-service docker start
  else
    info "starting dockerd in the background (no init system); log: /var/log/dockerd.log"
    nohup dockerd >/var/log/dockerd.log 2>&1 &
  fi
  for _ in $(seq 1 30); do docker_running && return 0; sleep 1; done
  die "Docker did not start (see 'journalctl -u docker' or /var/log/dockerd.log)"
}

if command -v docker >/dev/null; then
  if docker --version 2>/dev/null | grep -qi podman; then
    die "'docker' here is Podman's compatibility wrapper; install Docker Engine (and remove podman-docker), or run fluid-agent with Podman by hand (see deploy/README.md)"
  fi
  ok "Docker: $(docker --version | sed 's/,.*//')"
else
  install_docker
  ok "Docker installed: $(docker --version | sed 's/,.*//')"
fi
start_docker
docker compose version >/dev/null 2>&1 || install_compose_plugin
ok "Compose: $(docker compose version --short 2>/dev/null || echo installed)"

# ------------------------------------------------------------------ Go (optional)

install_go() {
  local v url sum
  v=$(curl -fsSL 'https://go.dev/VERSION?m=text' | head -1)
  [ -n "$v" ] || die "cannot determine the latest Go version"
  if command -v go >/dev/null && [ "$(go env GOVERSION 2>/dev/null)" = "$v" ]; then ok "Go: $v already installed"; return; fi
  url="https://dl.google.com/go/$v.linux-$ARCH.tar.gz"
  info "installing Go $v"
  curl -fsSL "$url" -o /tmp/go.tgz
  sum=$(curl -fsSL "$url.sha256")
  echo "$sum  /tmp/go.tgz" | sha256sum -c --quiet - || die "Go checksum mismatch"
  rm -rf /usr/local/go && tar -C /usr/local -xzf /tmp/go.tgz && rm -f /tmp/go.tgz
  echo 'export PATH=$PATH:/usr/local/go/bin' >/etc/profile.d/go.sh
  ok "Go $v in /usr/local/go (new shells get it on PATH)"
}
if [ -z "$WITH_GO" ]; then
  if yesno "Also install the Go toolchain on this host? It is NOT needed to run fluid-agent (the image is built in Docker)" n; then WITH_GO=yes; else WITH_GO=no; fi
fi
[ "$WITH_GO" = yes ] && install_go

# ------------------------------------------------------------------ questions: storage and ports

hr; info "${B}setup${N}"

if [ -z "$STORAGE" ]; then
  STORAGE=${PREV_STORAGE:-volume}
  ask STORAGE "Keep the database and dynamic config in Docker volumes or host directories? (volume/bind)" "$STORAGE"
fi
case "$STORAGE" in volume|bind) ;; *) die "storage must be volume or bind" ;; esac
if [ -n "${PREV_STORAGE:-}" ] && [ "$STORAGE" != "$PREV_STORAGE" ]; then
  warn "switching storage from $PREV_STORAGE to $STORAGE does not move existing data: the agent starts empty (the old data stays where it is)"
  yesno "Continue with $STORAGE storage?" n || die "aborted"
fi
if [ "$STORAGE" = bind ]; then
  [ -n "$DATA_PATH" ] || { DATA_PATH=${PREV_DATA_PATH:-$DEFAULT_DATA_PATH}; ask DATA_PATH "Host directory for data/ and dynamic/ (created if missing)" "$DATA_PATH"; }
  case "$DATA_PATH" in /*) ;; *) die "the data directory must be an absolute path, got $DATA_PATH" ;; esac
  DATA_PATH=${DATA_PATH%/}
  case "$DATA_PATH" in ""|/bin|/boot|/dev|/etc|/lib|/lib64|/proc|/root|/run|/sbin|/sys|/usr|/var) die "refusing to use $DATA_PATH/ as the data directory; pick a dedicated one such as $DEFAULT_DATA_PATH" ;; esac
  ok "storage: host directories $DATA_PATH/data and $DATA_PATH/dynamic"
else
  ok "storage: Docker volumes fluid-agent-data and fluid-agent-dynamic"
fi

# expose_question VAR label container_port previous
expose_question() {
  local __var=$1 label=$2 port=$3 prev=${4:-no} val=${!1} addr p
  if [ -z "$val" ]; then
    if [ -n "$TTY" ]; then
      local def=n; [ "$prev" != no ] && def=y
      if yesno "Expose the $label (container port $port) on this host?" "$def"; then
        addr=127.0.0.1; p=$port
        if [ "$prev" != no ]; then addr=${prev%:*}; p=${prev##*:}; fi
        ask addr "  bind address (127.0.0.1 = this VM only, 0.0.0.0 = all interfaces)" "$addr"
        ask p "  host port" "$p"
        val="$addr:$p"
      else
        val=no
      fi
    else
      val=$prev
    fi
  fi
  case "$val" in
    no|"") val=no ;;
    *:*) ;;
    *) val="127.0.0.1:$val" ;;
  esac
  if [ "$val" != no ]; then
    p=${val##*:}
    [[ "$p" =~ ^[0-9]+$ ]] && [ "$p" -ge 1 ] && [ "$p" -le 65535 ] || die "$label: bad port in $val"
  fi
  printf -v "$__var" '%s' "$val"
}
expose_question EXPOSE_PROXY "forward proxy (HTTP/CONNECT proxy and POST /forward)" 8080 "${PREV_EXPOSE_PROXY:-no}"
expose_question EXPOSE_WEBHOOK "webhook / ingest listener" 8081 "${PREV_EXPOSE_WEBHOOK:-no}"
expose_question EXPOSE_UI "console + API" 9000 "${PREV_EXPOSE_UI:-no}"

port_free() { ! ss -Hltn "sport = :$1" 2>/dev/null | grep -q .; }
for e in "$EXPOSE_PROXY" "$EXPOSE_WEBHOOK" "$EXPOSE_UI"; do
  [ "$e" = no ] && continue
  p=${e##*:}
  if command -v ss >/dev/null && ! port_free "$p"; then
    docker ps --format '{{.Names}} {{.Ports}}' 2>/dev/null | grep -q "^$CONTAINER .*:$p->" || die "host port $p is already in use"
  fi
done

describe() { [ "$1" = no ] && echo "not exposed" || echo "$1"; }
ok "proxy:   $(describe "$EXPOSE_PROXY")"
ok "webhook: $(describe "$EXPOSE_WEBHOOK")"
ok "console: $(describe "$EXPOSE_UI")"
[[ "$EXPOSE_UI" == 0.0.0.0:* ]] && warn "the console is published on all interfaces over plain HTTP: prefer 127.0.0.1 with an SSH tunnel, or a TLS reverse proxy"
[[ "$EXPOSE_PROXY" == 0.0.0.0:* ]] && warn "the forward proxy is published on all interfaces: firewall it to your application servers and set its allowlist"
[[ "$EXPOSE_WEBHOOK" == 0.0.0.0:* ]] && warn "webhooks over plain HTTP: most providers require HTTPS; put a TLS reverse proxy in front or set webhook_tls"

# Extra Docker networks: lets containers on them (a reverse proxy, your applications) reach the agent
# by name (http://fluid-agent:8081) without publishing ports. The default network is always kept.
if [ -z "$NETWORKS" ]; then
  NETWORKS=${PREV_NETWORKS:-none}
  if [ -n "$TTY" ]; then
    others=$(docker network ls --format '{{.Name}}' 2>/dev/null | grep -vxE 'bridge|host|none|fluid-agent_default' | paste -sd, - | sed 's/,/, /g')
    echo "  Docker networks on this host: ${others:-none besides the defaults}"
    ask NETWORKS "Attach the container to additional Docker networks? (comma separated names, none = no)" "$NETWORKS"
  fi
fi
NETWORKS=$(printf '%s' "$NETWORKS" | tr -d '[:space:]')
case "$NETWORKS" in ""|none|no|NONE) NETWORKS=none ;; esac
NET_LIST=()
if [ "$NETWORKS" != none ]; then
  IFS=, read -r -a parts <<<"$NETWORKS"
  for n in "${parts[@]}"; do
    [ -n "$n" ] || continue
    [[ "$n" =~ ^[A-Za-z0-9][A-Za-z0-9_.-]*$ ]] || die "bad Docker network name: $n"
    case "$n" in bridge|host|none) die "network $n cannot be added: the container already uses the project's default network" ;; esac
    [[ " ${NET_LIST[*]} " == *" $n "* ]] && continue
    if ! docker network inspect "$n" >/dev/null 2>&1; then
      if [ -n "$TTY" ] && yesno "  Docker network $n does not exist. Create it (bridge)?" n; then
        docker network create "$n" >/dev/null || die "creating network $n failed"
        ok "created Docker network $n"
      else
        die "Docker network $n does not exist (create it with: docker network create $n)"
      fi
    fi
    NET_LIST+=("$n")
  done
  [ ${#NET_LIST[@]} -gt 0 ] || NETWORKS=none
  [ ${#NET_LIST[@]} -gt 0 ] && NETWORKS=$(IFS=,; echo "${NET_LIST[*]}")
fi
if [ "$NETWORKS" = none ]; then
  ok "networks: the project's default network only"
else
  ok "networks: default + ${NETWORKS//,/, } (containers there reach it as fluid-agent:8080 / :8081 / :9000)"
fi

# Allowlists for the proxy and the console (webhook senders are set per route, in each webhook/ingest file).
if [ -z "$ALLOW_PROXY$ALLOW_UI" ] && [ -n "$TTY" ] && { [ "$EXPOSE_PROXY" != no ] || [ "$EXPOSE_UI" != no ]; }; then
  echo "  Allowlists: who may connect. Empty keeps the current list (first install: loopback and private networks only)."
  echo "  Use IPs, CIDRs or wildcards, comma separated: 203.0.113.7, 10.0.0.0/8, 52.66.*.*"
  [ "$EXPOSE_PROXY" != no ] && ask ALLOW_PROXY "  clients allowed to use the proxy" ""
  [ "$EXPOSE_UI" != no ] && ask ALLOW_UI "  clients allowed to use the console" ""
fi

# ------------------------------------------------------------------ image

hr; info "${B}preparing the image${N}"
mkdir -p "$DIR"
if [ "$MODE" = release ]; then
  if [ "$FA_VERSION" = latest ]; then
    FA_VERSION=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1)
    [ -n "$FA_VERSION" ] || die "cannot find the latest release of $REPO"
  fi
  BUILD="$DIR/build"
  rm -rf "${BUILD:?}"; mkdir -p "$BUILD/skel/app/data" "$BUILD/skel/app/dynamic"
  tarball="fluid-agent_${FA_VERSION}_linux_${ARCH}.tar.gz"
  base="https://github.com/$REPO/releases/download/$FA_VERSION"
  info "downloading $tarball"
  curl -fsSL "$base/$tarball" -o "$BUILD/$tarball"
  curl -fsSL "$base/checksums.txt" -o "$BUILD/checksums.txt"
  (cd "$BUILD" && grep " $tarball\$" checksums.txt | sha256sum -c --quiet -) || die "checksum mismatch for $tarball"
  ok "checksum verified"
  tar -xzf "$BUILD/$tarball" -C "$BUILD"
  cp "$BUILD/fluid-agent_${FA_VERSION}_linux_${ARCH}/fluid-agent" "$BUILD/fluid-agent"
  chmod 0755 "$BUILD/fluid-agent"
  rm -rf "${BUILD:?}/${tarball:?}" "$BUILD/checksums.txt" "$BUILD/fluid-agent_${FA_VERSION}_linux_${ARCH}"
  cat >"$BUILD/Dockerfile" <<'EOF'
FROM gcr.io/distroless/static:nonroot
COPY fluid-agent /fluid-agent
COPY --chown=65532:65532 skel/app /app
WORKDIR /app
USER nonroot
EXPOSE 8080 8081 9000
HEALTHCHECK --interval=15s --timeout=5s --start-period=10s CMD ["/fluid-agent", "healthcheck", "-config", "/app/config.json"]
ENTRYPOINT ["/fluid-agent", "-config", "/app/config.json"]
EOF
  BUILD_CTX=./build
else
  SRC="$DIR/src"
  ref=$FA_VERSION; [ "$ref" = latest ] && ref=main
  if [ -d "$SRC/.git" ]; then
    info "updating the source in $SRC to $ref"
    git -C "$SRC" fetch -q --depth 1 origin "$ref" && git -C "$SRC" checkout -q FETCH_HEAD
  else
    info "cloning $REPO ($ref)"
    git clone -q --depth 1 --branch "$ref" "https://github.com/$REPO.git" "$SRC"
  fi
  FA_VERSION="$ref@$(git -C "$SRC" rev-parse --short HEAD)"
  BUILD_CTX=./src
fi
ok "version: $FA_VERSION ($MODE)"

# ------------------------------------------------------------------ config + compose

# config.json: the listener addresses inside the container (all interfaces;
# what reaches them is decided by the published ports above). Everything
# else uses the agent's defaults; edit this file to change them.
if [ ! -f "$DIR/config.json" ]; then
  cat >"$DIR/config.json" <<'EOF'
{
  "$schema": "https://raw.githubusercontent.com/skpjr001/fluid-agent/main/schema/config.schema.json",
  "proxy_addr": ":8080",
  "webhook_addr": ":8081",
  "admin_addr": ":9000",
  "dynamic_dir": "/app/dynamic",
  "database": {
    "path": "/app/data/fluid-agent.db",
    "retention_days": 14,
    "log_level": "info",
    "store_event_bodies": true,
    "capture_payloads": true,
    "capture_max_body_bytes": 10485760
  }
}
EOF
  chmod 0644 "$DIR/config.json"
  ok "wrote $DIR/config.json"
else
  ok "keeping $DIR/config.json"
fi

if [ "$STORAGE" = bind ]; then
  # The base (and any missing parent, e.g. /fluid) belongs to root and is
  # world-traversable; data/ and dynamic/ belong to the container's user
  # (uid 65532) and nobody else, including files copied in by hand.
  parent=$(dirname "$DATA_PATH")
  [ -d "$parent" ] || { mkdir -p "$parent" && chmod 0755 "$parent" && ok "created $parent"; }
  [ -d "$DATA_PATH" ] || { mkdir -p "$DATA_PATH" && ok "created $DATA_PATH"; }
  chmod 0755 "$DATA_PATH"
  mkdir -p "$DATA_PATH/data" "$DATA_PATH/dynamic"
  chown -R "$NONROOT_UID:$NONROOT_UID" "$DATA_PATH/data" "$DATA_PATH/dynamic"
  chmod 0750 "$DATA_PATH/data" "$DATA_PATH/dynamic"
  ok "permissions: $DATA_PATH/data and $DATA_PATH/dynamic owned by uid $NONROOT_UID (mode 0750)"
  DATA_MOUNT="$DATA_PATH/data"; DYN_MOUNT="$DATA_PATH/dynamic"
else
  DATA_MOUNT="fluid-agent-data"; DYN_MOUNT="fluid-agent-dynamic"
fi

{
  echo "# Generated by deploy/install.sh; re-run the installer to change it."
  echo "services:"
  echo "  fluid-agent:"
  echo "    build: $BUILD_CTX"
  echo "    image: fluid-agent:local"
  echo "    container_name: $CONTAINER"
  echo "    restart: unless-stopped"
  echo "    stop_grace_period: 30s   # longer than the agent's shutdown_timeout (15s), so in-flight work finishes"
  echo "    volumes:"
  echo "      - ./config.json:/app/config.json:ro"
  echo "      - $DYN_MOUNT:/app/dynamic"
  echo "      - $DATA_MOUNT:/app/data"
  if [ "$EXPOSE_PROXY$EXPOSE_WEBHOOK$EXPOSE_UI" != nonono ]; then
    echo "    ports:"
    [ "$EXPOSE_PROXY" != no ] && echo "      - \"$EXPOSE_PROXY:8080\"     # forward proxy + /forward"
    [ "$EXPOSE_WEBHOOK" != no ] && echo "      - \"$EXPOSE_WEBHOOK:8081\"     # webhooks + ingest"
    [ "$EXPOSE_UI" != no ] && echo "      - \"$EXPOSE_UI:9000\"     # console + API"
  else
    echo "    # no ports published (re-run the installer to expose the proxy, webhooks or console)"
  fi
  if [ ${#NET_LIST[@]} -gt 0 ]; then
    echo "    networks:"
    echo "      - default"
    for n in "${NET_LIST[@]}"; do echo "      - \"$n\""; done
  fi
  if [ ${#NET_LIST[@]} -gt 0 ]; then
    echo "networks:"
    echo "  default: {}"
    for n in "${NET_LIST[@]}"; do
      echo "  \"$n\":"
      echo "    external: true   # created outside this install; never removed by --uninstall"
      echo "    name: \"$n\""
    done
  fi
  if [ "$STORAGE" = volume ]; then
    echo "volumes:"
    echo "  fluid-agent-data:"
    echo "    name: fluid-agent-data"
    echo "  fluid-agent-dynamic:"
    echo "    name: fluid-agent-dynamic"
  fi
} >"$DIR/docker-compose.yml"
ok "wrote $DIR/docker-compose.yml"

# Keep a copy of the installer next to the install, for upgrades and --uninstall.
if [ -n "$SELF" ] && [ "$SELF" != "$DIR/install.sh" ]; then
  cp "$SELF" "$DIR/install.sh"
elif [ -z "$SELF" ]; then
  curl -fsSL "https://raw.githubusercontent.com/$REPO/main/deploy/install.sh" -o "$DIR/install.sh" || true
fi
chmod 0755 "$DIR/install.sh" 2>/dev/null || true

cat >"$STATE" <<EOF
PREV_VERSION='$FA_VERSION'
PREV_STORAGE='$STORAGE'
PREV_DATA_PATH='$DATA_PATH'
PREV_EXPOSE_PROXY='$EXPOSE_PROXY'
PREV_EXPOSE_WEBHOOK='$EXPOSE_WEBHOOK'
PREV_EXPOSE_UI='$EXPOSE_UI'
PREV_NETWORKS='$NETWORKS'
EOF

# ------------------------------------------------------------------ start

hr; info "${B}building and starting${N}"
compose build -q || die "building the image failed (see above); free disk: $(df -Pm "$DIR" | awk 'NR==2 {print $4}') MiB"
compose up -d --remove-orphans || die "starting the container failed (see above)"
info "waiting for the agent to report healthy"
health=""
for _ in $(seq 1 60); do
  health=$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$CONTAINER" 2>/dev/null || true)
  [ "$health" = healthy ] && break
  [ "$health" = exited ] && break
  sleep 2
done
if [ "$health" != healthy ]; then
  docker logs --tail 30 "$CONTAINER" >&2 || true
  die "the container is not healthy ($health); see the log above"
fi
ok "fluid-agent is running ($FA_VERSION)"

# Allowlists: written into the dynamic config (the agent reloads it at once).
json_list() { local IFS=, out="" x; for x in $1; do x=$(echo "$x" | tr -d '[:space:]'); [ -n "$x" ] && out="$out${out:+, }\"$x\""; done; echo "[$out]"; }
read_dynamic() { docker cp "$CONTAINER:/app/dynamic/$1" - 2>/dev/null | tar -xO 2>/dev/null; }
if [ -n "$ALLOW_PROXY$ALLOW_UI" ]; then
  cur=$(read_dynamic access.json || true)
  keep() { # current list for a section, as JSON, or the default
    printf '%s' "$cur" | tr -d '\n' | tr -s ' ' | sed -n "s/.*\"$1\" *: *{ *\"allow\" *: *\(\[[^]]*\]\).*/\1/p" | sed 's/\[ /[/; s/ \]/]/' 
  }
  p_list=$([ -n "$ALLOW_PROXY" ] && json_list "$ALLOW_PROXY" || keep proxy)
  a_list=$([ -n "$ALLOW_UI" ] && json_list "$ALLOW_UI" || keep admin)
  # The console must stay reachable through the published port, which arrives from Docker's private network.
  tmp=$(mktemp)
  {
    echo '{'
    echo '  "$schema": "https://raw.githubusercontent.com/skpjr001/fluid-agent/main/schema/access.schema.json",'
    [ -n "$p_list" ] && echo "  \"proxy\": { \"allow\": $p_list }${a_list:+,}"
    [ -n "$a_list" ] && echo "  \"admin\": { \"allow\": $a_list }"
    echo '}'
  } >"$tmp"
  chmod 0644 "$tmp"
  docker cp "$tmp" "$CONTAINER:/app/dynamic/access.json" >/dev/null
  rm -f "$tmp"
  ok "access allowlists updated (proxy: ${p_list:-default}, console: ${a_list:-default})"
fi

# ------------------------------------------------------------------ summary

hr
echo "${B}fluid-agent $FA_VERSION is running in container '$CONTAINER'.${N}"
echo
url() { local a=${1%:*} p=${1##*:}; [ "$a" = 0.0.0.0 ] && a=$(hostname -I 2>/dev/null | awk '{print $1}'); echo "$2://${a:-localhost}:$p"; }
if [ "$EXPOSE_UI" != no ]; then
  echo "  Console:   $(url "$EXPOSE_UI" http)/"
  [[ "$EXPOSE_UI" == 127.0.0.1:* ]] && echo "             from your machine: ssh -L ${EXPOSE_UI##*:}:127.0.0.1:${EXPOSE_UI##*:} <this-vm>, then http://localhost:${EXPOSE_UI##*:}/"
else
  echo "  Console:   not exposed (re-run with --expose-ui 127.0.0.1:9000 to reach it)"
fi
[ "$EXPOSE_WEBHOOK" != no ] && echo "  Webhooks:  $(url "$EXPOSE_WEBHOOK" http)/hooks/<id>" || echo "  Webhooks:  not exposed"
[ "$EXPOSE_PROXY" != no ] && echo "  Proxy:     HTTPS_PROXY=http://<client>:<token>@$(url "$EXPOSE_PROXY" http | sed 's|^http://||')   and POST $(url "$EXPOSE_PROXY" http)/forward" || echo "  Proxy:     not exposed"
echo
echo "  Editor token (generated on first start; keep it secret):"
echo "    sudo docker cp $CONTAINER:/app/dynamic/admin.json - | tar -xO"
echo
[ "$NETWORKS" != none ] && echo "  Networks:  also on ${NETWORKS//,/, }: other containers there use http://$CONTAINER:8081 (webhooks), :8080 (proxy), :9000 (console)"
echo "  Manage:    cd $DIR && sudo docker compose -p fluid-agent ps | logs -f | restart"
echo "  Logs:      sudo docker exec $CONTAINER /fluid-agent logs -config /app/config.json -since 1h"
echo "  Check:     sudo docker exec $CONTAINER /fluid-agent check -config /app/config.json"
echo "  Upgrade:   sudo bash $DIR/install.sh   (config, data and answers are kept)"
echo "  Remove:    sudo bash $DIR/install.sh --uninstall   (add --purge to delete data)"
if [ "$STORAGE" = volume ]; then
  echo "  Data:      Docker volumes fluid-agent-data (SQLite) and fluid-agent-dynamic (JSON config; edit in the console)"
else
  echo "  Data:      $DATA_PATH/data (SQLite) and $DATA_PATH/dynamic (JSON config, hot-reloaded)"
fi
echo
echo "  Next: add webhooks with allowed_ips for your providers, proxy clients and allowed hosts in the console."
echo "  Startup notes (warnings about this setup): sudo docker logs $CONTAINER 2>&1 | grep '\"kind\":\"deploy\"'"
