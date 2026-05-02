#!/usr/bin/env bash
set -Eeuo pipefail

# Install DPS as an unmanaged Docker VolumeDriver plugin on Ubuntu 24.04 arm64.
# Intended for Dokploy-managed Linux servers where Docker is already installed.
#
# Default mode is "auto" so the script can install on ordinary ext4 hosts.
# For the best production-like path, pass a dedicated block device explicitly:
#
#   DPS_XFS_DEVICE=/dev/vdb DPS_FORMAT_XFS=true DPS_POOL_MODE=direct bash install-ubuntu-24.04-arm64-dokploy.sh
#
# The script never formats disks unless DPS_XFS_DEVICE is set and
# DPS_FORMAT_XFS=true is explicitly provided.

DPS_REPO_URL="${DPS_REPO_URL:-https://github.com/tiagobecker/docker-plugin-storage.git}"
DPS_REF="${DPS_REF:-main}"
DPS_INSTALL_DIR="${DPS_INSTALL_DIR:-/opt/docker-plugin-storage}"
DPS_ROOT="${DPS_ROOT:-/var/lib/dps}"
DPS_MOUNT_ROOT="${DPS_MOUNT_ROOT:-/mnt/dps}"
DPS_POOL_MODE="${DPS_POOL_MODE:-auto}"
DPS_POOL_SIZE="${DPS_POOL_SIZE:-100G}"
DPS_DEFAULT_VOLUME_SIZE="${DPS_DEFAULT_VOLUME_SIZE:-10G}"
DPS_DEFAULT_VOLUME_INODES="${DPS_DEFAULT_VOLUME_INODES:-200000}"
DPS_REQUIRE_LIMITS="${DPS_REQUIRE_LIMITS:-true}"
DPS_ARCHIVE_POLICY="${DPS_ARCHIVE_POLICY:-offline}"
DPS_SOCKET="${DPS_SOCKET:-/run/docker/plugins/dps.sock}"
DPS_SERVICE_NAME="${DPS_SERVICE_NAME:-dpsd}"
DPS_GO_IMAGE="${DPS_GO_IMAGE:-golang:1.24-alpine}"
DPS_TARGET_ARCH="${DPS_TARGET_ARCH:-arm64}"
DPS_ALLOW_UNSUPPORTED_OS="${DPS_ALLOW_UNSUPPORTED_OS:-false}"
DPS_XFS_DEVICE="${DPS_XFS_DEVICE:-}"
DPS_FORMAT_XFS="${DPS_FORMAT_XFS:-false}"
DPS_WRITE_FSTAB="${DPS_WRITE_FSTAB:-true}"

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
    util-linux \
    xfsprogs
}

check_docker() {
  need_cmd docker
  docker version >/dev/null || die "Docker is installed but not reachable. Run this script on the Docker host."
}

is_true() {
  case "$1" in
    true|1|yes|y|on) return 0 ;;
    *) return 1 ;;
  esac
}

prepare_xfs_mount() {
  [ "$DPS_XFS_DEVICE" != "" ] || return 0

  need_cmd blkid
  need_cmd findmnt
  need_cmd lsblk
  need_cmd mkfs.xfs
  need_cmd mount

  [ -b "$DPS_XFS_DEVICE" ] || die "DPS_XFS_DEVICE is not a block device: $DPS_XFS_DEVICE"

  if [ "$DPS_POOL_MODE" = "auto" ]; then
    log "DPS_XFS_DEVICE was provided; switching DPS_POOL_MODE from auto to direct"
    DPS_POOL_MODE="direct"
  fi
  [ "$DPS_POOL_MODE" = "direct" ] || die "DPS_XFS_DEVICE requires DPS_POOL_MODE=direct or auto"

  mkdir -p "$DPS_MOUNT_ROOT"

  current_mount="$(findmnt -rn --source "$DPS_XFS_DEVICE" --output TARGET | head -n 1 || true)"
  if [ "$current_mount" != "" ] && [ "$current_mount" != "$DPS_MOUNT_ROOT" ]; then
    die "$DPS_XFS_DEVICE is already mounted at $current_mount; refusing to reuse it for $DPS_MOUNT_ROOT"
  fi

  if mountpoint -q "$DPS_MOUNT_ROOT"; then
    mounted_source="$(findmnt -no SOURCE "$DPS_MOUNT_ROOT" || true)"
    [ "$mounted_source" = "$DPS_XFS_DEVICE" ] || die "$DPS_MOUNT_ROOT is already mounted from $mounted_source, not $DPS_XFS_DEVICE"
    ensure_fstab_entry "$DPS_XFS_DEVICE"
    return 0
  fi

  child_count="$(lsblk -rn -o NAME "$DPS_XFS_DEVICE" | wc -l | tr -d ' ')"
  if [ "$child_count" != "1" ]; then
    die "$DPS_XFS_DEVICE appears to contain child partitions. Pass the intended partition, for example /dev/vdb1."
  fi

  existing_fstype="$(blkid -o value -s TYPE "$DPS_XFS_DEVICE" 2>/dev/null || true)"
  if [ "$existing_fstype" != "xfs" ]; then
    if ! is_true "$DPS_FORMAT_XFS"; then
      die "$DPS_XFS_DEVICE is ${existing_fstype:-unformatted}; set DPS_FORMAT_XFS=true to format it as XFS"
    fi
    log "formatting $DPS_XFS_DEVICE as XFS; existing data on this device will be destroyed"
    systemctl stop "$DPS_SERVICE_NAME.service" >/dev/null 2>&1 || true
    mkfs.xfs -f "$DPS_XFS_DEVICE"
  else
    log "$DPS_XFS_DEVICE is already XFS; mounting without formatting"
  fi

  ensure_fstab_entry "$DPS_XFS_DEVICE"
  if is_true "$DPS_WRITE_FSTAB"; then
    mount "$DPS_MOUNT_ROOT"
  else
    mount -o prjquota "$DPS_XFS_DEVICE" "$DPS_MOUNT_ROOT"
  fi
}

ensure_fstab_entry() {
  device="$1"
  uuid="$(blkid -s UUID -o value "$device" 2>/dev/null || true)"
  [ "$uuid" != "" ] || die "could not read UUID from $device"

  if ! is_true "$DPS_WRITE_FSTAB"; then
    return 0
  fi

  if grep -qs "[[:space:]]$DPS_MOUNT_ROOT[[:space:]]" /etc/fstab; then
    if ! grep -qsE "^(UUID=$uuid|$device)[[:space:]]+$DPS_MOUNT_ROOT[[:space:]]" /etc/fstab; then
      die "/etc/fstab already has an entry for $DPS_MOUNT_ROOT, but not for $device. Adjust it manually before continuing."
    fi
    log "/etc/fstab already has an entry for $DPS_MOUNT_ROOT"
    return 0
  fi

  backup="/etc/fstab.dps.$(date +%Y%m%d%H%M%S).bak"
  cp /etc/fstab "$backup"
  printf 'UUID=%s %s xfs defaults,prjquota 0 2\n' "$uuid" "$DPS_MOUNT_ROOT" >>/etc/fstab
  log "added $DPS_MOUNT_ROOT to /etc/fstab; backup saved at $backup"
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
  mkdir -p "$DPS_ROOT" "$DPS_MOUNT_ROOT" "$(dirname "$DPS_SOCKET")"

  if [ "$DPS_POOL_MODE" = "direct" ]; then
    mountpoint -q "$DPS_MOUNT_ROOT" || die "DPS_POOL_MODE=direct requires $DPS_MOUNT_ROOT to be a mounted XFS filesystem"
    fstype="$(findmnt -no FSTYPE "$DPS_MOUNT_ROOT" || true)"
    opts="$(findmnt -no OPTIONS "$DPS_MOUNT_ROOT" || true)"
    [ "$fstype" = "xfs" ] || die "DPS_POOL_MODE=direct requires XFS at $DPS_MOUNT_ROOT; got $fstype"
    case ",$opts," in
      *,prjquota,*|*,pquota,*) ;;
      *) die "$DPS_MOUNT_ROOT is XFS but missing prjquota/pquota mount option" ;;
    esac
  else
    log "DPS_POOL_MODE=$DPS_POOL_MODE; DPS may use loopback/fixed-image fallback when project quotas are unavailable"
  fi
}

write_environment() {
  log "writing /etc/dps/dpsd.env"
  mkdir -p /etc/dps
  cat >/etc/dps/dpsd.env <<EOF
DPS_ROOT=$DPS_ROOT
DPS_MOUNT_ROOT=$DPS_MOUNT_ROOT
DPS_POOL_MODE=$DPS_POOL_MODE
DPS_POOL_SIZE=$DPS_POOL_SIZE
DPS_DEFAULT_VOLUME_SIZE=$DPS_DEFAULT_VOLUME_SIZE
DPS_DEFAULT_VOLUME_INODES=$DPS_DEFAULT_VOLUME_INODES
DPS_REQUIRE_LIMITS=$DPS_REQUIRE_LIMITS
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
  --pool-mode ${DPS_POOL_MODE} \
  --pool-size ${DPS_POOL_SIZE} \
  --default-volume-size ${DPS_DEFAULT_VOLUME_SIZE} \
  --default-volume-inodes ${DPS_DEFAULT_VOLUME_INODES} \
  --require-limits=${DPS_REQUIRE_LIMITS} \
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
  printf 'mode=%s mount=%s root=%s\\n' "\$DPS_POOL_MODE" "\$DPS_MOUNT_ROOT" "\$DPS_ROOT"

EOF
}

main() {
  as_root "$@"
  check_os
  check_arch
  install_packages
  prepare_xfs_mount
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
