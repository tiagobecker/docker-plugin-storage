#!/usr/bin/env bash
set -Eeuo pipefail

# Install DPS as an unmanaged Docker VolumeDriver plugin on Ubuntu 24.04 arm64.
# Intended for Dokploy-managed Linux servers where Docker is already installed.
#
# DPS uses one portable backend: one ext4 filesystem image per Docker volume,
# mounted through a loop device under DPS_MOUNT_ROOT.

DPS_REPO_URL="${DPS_REPO_URL:-https://github.com/tiagobecker/docker-plugin-storage.git}"
DPS_REF="${DPS_REF:-main}"
DPS_INSTALL_DIR="${DPS_INSTALL_DIR:-/opt/docker-plugin-storage}"
DPS_ROOT="${DPS_ROOT:-/var/lib/dps}"
DPS_MOUNT_ROOT="${DPS_MOUNT_ROOT:-/mnt/dps}"
DPS_IMAGE_ROOT="${DPS_IMAGE_ROOT:-$DPS_ROOT/volume-images}"
DPS_DEFAULT_VOLUME_SIZE="${DPS_DEFAULT_VOLUME_SIZE:-10G}"
DPS_DEFAULT_VOLUME_INODES="${DPS_DEFAULT_VOLUME_INODES:-200000}"
DPS_ARCHIVE_POLICY="${DPS_ARCHIVE_POLICY:-offline}"
DPS_SOCKET="${DPS_SOCKET:-/run/docker/plugins/dps.sock}"
DPS_SERVICE_NAME="${DPS_SERVICE_NAME:-dpsd}"
DPS_GO_IMAGE="${DPS_GO_IMAGE:-golang:1.24-alpine}"
DPS_TARGET_ARCH="${DPS_TARGET_ARCH:-arm64}"
DPS_ALLOW_UNSUPPORTED_OS="${DPS_ALLOW_UNSUPPORTED_OS:-false}"

log() {
  printf '[dps-install] %s\n' "$*"
}

die() {
  printf '[dps-install] ERROR: %s\n' "$*" >&2
  exit 1
}

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"
}

as_root() {
  if [ "${EUID:-$(id -u)}" -ne 0 ]; then
    need_cmd sudo
    exec sudo -E bash "$0" "$@"
  fi
}

check_os() {
  [ -r /etc/os-release ] || die "/etc/os-release not found"
  # shellcheck disable=SC1091
  . /etc/os-release
  if [ "${ID:-}" != "ubuntu" ] || [ "${VERSION_ID:-}" != "24.04" ]; then
    if [ "$DPS_ALLOW_UNSUPPORTED_OS" != "true" ]; then
      die "expected Ubuntu 24.04; got ${PRETTY_NAME:-unknown}. Set DPS_ALLOW_UNSUPPORTED_OS=true to continue."
    fi
    log "continuing on unsupported OS: ${PRETTY_NAME:-unknown}"
  fi
}

check_arch() {
  arch="$(dpkg --print-architecture 2>/dev/null || true)"
  if [ "$arch" = "" ]; then
    case "$(uname -m)" in
      aarch64|arm64) arch="arm64" ;;
      *) arch="$(uname -m)" ;;
    esac
  fi
  [ "$arch" = "arm64" ] || die "expected arm64/aarch64; got $arch"
}

install_packages() {
  log "installing host packages"
  apt-get update
  DEBIAN_FRONTEND=noninteractive apt-get install -y \
    ca-certificates \
    e2fsprogs \
    git \
    util-linux
}

check_docker() {
  need_cmd docker
  docker version >/dev/null || die "Docker is installed but not reachable. Run this script on the Docker host."
}

checkout_source() {
  log "checking out DPS source into $DPS_INSTALL_DIR"
  if [ -d "$DPS_INSTALL_DIR/.git" ]; then
    git -C "$DPS_INSTALL_DIR" fetch --tags origin
    git -C "$DPS_INSTALL_DIR" checkout "$DPS_REF"
    git -C "$DPS_INSTALL_DIR" pull --ff-only origin "$DPS_REF" || true
  else
    mkdir -p "$(dirname "$DPS_INSTALL_DIR")"
    git clone --branch "$DPS_REF" "$DPS_REPO_URL" "$DPS_INSTALL_DIR"
  fi
}

build_binaries() {
  log "building linux/$DPS_TARGET_ARCH binaries with Docker image $DPS_GO_IMAGE"
  docker run --rm \
    --platform "linux/$DPS_TARGET_ARCH" \
    -e CGO_ENABLED=0 \
    -e GOOS=linux \
    -e GOARCH="$DPS_TARGET_ARCH" \
    -v "$DPS_INSTALL_DIR:/src" \
    -w /src \
    "$DPS_GO_IMAGE" \
    sh -ec '
      mkdir -p bin
      go build -trimpath -ldflags="-s -w" -o bin/dpsd ./cmd/dpsd
      go build -trimpath -ldflags="-s -w" -o bin/dpsctl ./cmd/dpsctl
    '

  install -m 0755 "$DPS_INSTALL_DIR/bin/dpsd" /usr/local/bin/dpsd
  install -m 0755 "$DPS_INSTALL_DIR/bin/dpsctl" /usr/local/bin/dpsctl
}

validate_mount_root() {
  mkdir -p "$DPS_ROOT" "$DPS_IMAGE_ROOT" "$DPS_MOUNT_ROOT" "$(dirname "$DPS_SOCKET")"
  log "DPS will store volume images under $DPS_IMAGE_ROOT and mount volumes under $DPS_MOUNT_ROOT/volumes"
}

write_environment() {
  log "writing /etc/dps/dpsd.env"
  mkdir -p /etc/dps
  cat >/etc/dps/dpsd.env <<EOF
DPS_ROOT=$DPS_ROOT
DPS_MOUNT_ROOT=$DPS_MOUNT_ROOT
DPS_IMAGE_ROOT=$DPS_IMAGE_ROOT
DPS_DEFAULT_VOLUME_SIZE=$DPS_DEFAULT_VOLUME_SIZE
DPS_DEFAULT_VOLUME_INODES=$DPS_DEFAULT_VOLUME_INODES
DPS_ARCHIVE_POLICY=$DPS_ARCHIVE_POLICY
DPS_SOCKET=$DPS_SOCKET
EOF
  chmod 0644 /etc/dps/dpsd.env
}

write_systemd_service() {
  log "writing systemd service $DPS_SERVICE_NAME.service"
  cat >"/etc/systemd/system/$DPS_SERVICE_NAME.service" <<'EOF'
[Unit]
Description=DPS Docker Volume Driver
Documentation=https://github.com/tiagobecker/docker-plugin-storage
After=docker.service
Requires=docker.service

[Service]
Type=simple
EnvironmentFile=/etc/dps/dpsd.env
ExecStart=/usr/local/bin/dpsd \
  --root ${DPS_ROOT} \
  --mount-root ${DPS_MOUNT_ROOT} \
  --image-root ${DPS_IMAGE_ROOT} \
  --default-volume-size ${DPS_DEFAULT_VOLUME_SIZE} \
  --default-volume-inodes ${DPS_DEFAULT_VOLUME_INODES} \
  --archive-policy ${DPS_ARCHIVE_POLICY} \
  --socket ${DPS_SOCKET}
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
EOF
}

restart_service() {
  log "enabling and starting $DPS_SERVICE_NAME"
  systemctl daemon-reload
  systemctl enable "$DPS_SERVICE_NAME.service" >/dev/null
  systemctl restart "$DPS_SERVICE_NAME.service"
  systemctl --no-pager --full status "$DPS_SERVICE_NAME.service" || {
    journalctl -u "$DPS_SERVICE_NAME.service" --no-pager -n 80
    die "$DPS_SERVICE_NAME failed to start"
  }
}

test_driver() {
  log "testing Docker volume driver"
  test_volume="dps_install_test_$(date +%s)"
  docker volume create --driver dps --opt size=64M --opt inodes=4096 "$test_volume" >/dev/null
  docker run --rm -v "$test_volume:/data" alpine:3.22 sh -ec 'df -h /data; df -i /data'
  docker volume rm "$test_volume" >/dev/null
}

print_next_steps() {
  cat <<EOF

[dps-install] DPS installed successfully.

Docker Compose / Dokploy volume example:

volumes:
  pgdata:
    driver: dps
    driver_opts:
      size: 20G
      inodes: "500000"

Validate inside an app container:

  docker exec -it <container> df -h /path/to/volume
  docker exec -it <container> df -i /path/to/volume

Service logs:

  journalctl -u $DPS_SERVICE_NAME -f

Current DPS config:

  source /etc/dps/dpsd.env
  printf 'images=%s mount=%s root=%s\\n' "\$DPS_IMAGE_ROOT" "\$DPS_MOUNT_ROOT" "\$DPS_ROOT"

EOF
}

main() {
  as_root "$@"
  check_os
  check_arch
  install_packages
  check_docker
  checkout_source
  build_binaries
  validate_mount_root
  write_environment
  write_systemd_service
  restart_service
  test_driver
  print_next_steps
}

main "$@"
