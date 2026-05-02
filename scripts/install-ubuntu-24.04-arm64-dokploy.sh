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
DPS_DEFAULT_VOLUME_SIZE="${DPS_DEFAULT_VOLUME_SIZE:-5G}"
DPS_DEFAULT_VOLUME_INODES="${DPS_DEFAULT_VOLUME_INODES:-200000}"
DPS_ARCHIVE_POLICY="${DPS_ARCHIVE_POLICY:-offline}"
DPS_SOCKET="${DPS_SOCKET:-/run/docker/plugins/dps.sock}"
DPS_SERVICE_NAME="${DPS_SERVICE_NAME:-dpsd}"
DPS_GO_IMAGE="${DPS_GO_IMAGE:-golang:1.24-alpine}"
DPS_TARGET_ARCH="${DPS_TARGET_ARCH:-arm64}"
DPS_ALLOW_UNSUPPORTED_OS="${DPS_ALLOW_UNSUPPORTED_OS:-false}"
DPS_INSTALL_ALLOW_MANAGED_PLUGIN_CONFLICT="${DPS_INSTALL_ALLOW_MANAGED_PLUGIN_CONFLICT:-false}"
DPS_INSTALL_REMOVE_STALE_PLUGIN_SPECS="${DPS_INSTALL_REMOVE_STALE_PLUGIN_SPECS:-true}"
DPS_INSTALL_ROLLBACK_ON_TEST_FAILURE="${DPS_INSTALL_ROLLBACK_ON_TEST_FAILURE:-true}"
CURRENT_STEP="starting"

banner() {
  cat <<'EOF'
======================================================================
 DPS Installer
======================================================================
 Installs DPS as an unmanaged Docker VolumeDriver service.
 Target: Ubuntu 24.04 arm64 with Docker already installed.
 Storage: one ext4 image per Docker volume, mounted through loop.
======================================================================
EOF
}

section() {
  CURRENT_STEP="$1"
  printf '\n[dps-install] == %s ==\n' "$CURRENT_STEP"
}

log() {
  printf '[dps-install] %s\n' "$*"
}

success() {
  printf '[dps-install] OK: %s\n' "$*"
}

die() {
  cat >&2 <<EOF

======================================================================
 DPS INSTALL FAILED
======================================================================
 Step: $CURRENT_STEP
 Error: $*
======================================================================
EOF
  exit 1
}

on_error() {
  code="$?"
  line="$1"
  cat >&2 <<EOF

======================================================================
 DPS INSTALL FAILED
======================================================================
 Step: $CURRENT_STEP
 Line: $line
 Exit code: $code

Useful diagnostics:
  systemctl status $DPS_SERVICE_NAME --no-pager --full
  journalctl -u $DPS_SERVICE_NAME -n 120 --no-pager --full
  docker volume ls | grep dps || true
======================================================================
EOF
  exit "$code"
}

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"
}

is_true() {
  case "$1" in
    true|1|yes|y|on) return 0 ;;
    *) return 1 ;;
  esac
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
  section "Install host packages"
  log "installing host packages"
  apt-get update
  DEBIAN_FRONTEND=noninteractive apt-get install -y \
    ca-certificates \
    e2fsprogs \
    git \
    util-linux
}

check_docker() {
  section "Check Docker"
  need_cmd docker
  docker version >/dev/null || die "Docker is installed but not reachable. Run this script on the Docker host."
  success "Docker is reachable"
}

check_managed_plugin_conflict() {
  section "Check plugin conflicts"
  is_true "$DPS_INSTALL_ALLOW_MANAGED_PLUGIN_CONFLICT" && return 0

  conflicts="$(
    docker plugin ls --format '{{.Name}}' 2>/dev/null | while IFS= read -r plugin; do
      case "$plugin" in
        dps|dps:*|*/dps|*/dps:*|docker-plugin-storage|docker-plugin-storage:*|*/docker-plugin-storage|*/docker-plugin-storage:*)
          printf '%s\n' "$plugin"
          ;;
      esac
    done
  )"

  if [ "$conflicts" != "" ]; then
    cat >&2 <<EOF
[dps-install] ERROR: managed Docker plugin conflict detected:
$conflicts

This installer runs DPS as an unmanaged host service using the driver name "dps".
Remove old managed DPS plugins first, or set DPS_INSTALL_ALLOW_MANAGED_PLUGIN_CONFLICT=true if you know this is intentional.

Recommended cleanup on disposable test hosts:
  curl -fsSL https://raw.githubusercontent.com/tiagobecker/docker-plugin-storage/main/scripts/uninstall-dps-host.sh -o uninstall-dps-host.sh
  sudo env DPS_UNINSTALL_CONFIRM=erase-dps DPS_UNINSTALL_RESTART_DOCKER=true bash uninstall-dps-host.sh
EOF
    exit 1
  fi
  success "No managed DPS plugin conflict detected"
}

remove_stale_plugin_specs() {
  is_true "$DPS_INSTALL_REMOVE_STALE_PLUGIN_SPECS" || return 0

  log "removing stale unmanaged DPS plugin spec files if present"
  rm -f \
    /etc/docker/plugins/dps.spec \
    /etc/docker/plugins/docker-plugin-storage.spec \
    /usr/lib/docker/plugins/dps.spec \
    /usr/lib/docker/plugins/docker-plugin-storage.spec
}

checkout_source() {
  section "Fetch DPS source"
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
  section "Build binaries"
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
  success "Installed /usr/local/bin/dpsd and /usr/local/bin/dpsctl"
}

validate_mount_root() {
  section "Prepare DPS paths"
  mkdir -p "$DPS_ROOT" "$DPS_IMAGE_ROOT" "$DPS_MOUNT_ROOT" "$(dirname "$DPS_SOCKET")"
  rm -f "$DPS_SOCKET"
  log "DPS will store volume images under $DPS_IMAGE_ROOT and mount volumes under $DPS_MOUNT_ROOT/volumes"
}

write_environment() {
  section "Write configuration"
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
  success "Configuration written to /etc/dps/dpsd.env"
}

write_systemd_service() {
  section "Write systemd service"
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
  section "Start DPS service"
  log "enabling and starting $DPS_SERVICE_NAME"
  systemctl daemon-reload
  systemctl enable "$DPS_SERVICE_NAME.service" >/dev/null
  systemctl restart "$DPS_SERVICE_NAME.service"
  systemctl --no-pager --full status "$DPS_SERVICE_NAME.service" || {
    journalctl -u "$DPS_SERVICE_NAME.service" --no-pager -n 80
    die "$DPS_SERVICE_NAME failed to start"
  }
  success "$DPS_SERVICE_NAME is active"
}

test_driver() {
  section "Validate Docker volume driver"
  log "testing Docker volume driver"
  test_volume="dps_install_test_$(date +%s)"
  if ! docker volume create --driver dps --opt size=64M --opt inodes=4096 "$test_volume" >/dev/null; then
    docker volume rm "$test_volume" >/dev/null 2>&1 || true
    return 1
  fi
  if ! docker run --rm -v "$test_volume:/data" alpine:3.22 sh -ec 'df -h /data; df -i /data'; then
    docker volume rm "$test_volume" >/dev/null 2>&1 || true
    return 1
  fi
  docker volume rm "$test_volume" >/dev/null 2>&1 || true
  success "DPS test volume created, mounted, inspected, and removed"
}

rollback_after_failed_test() {
  is_true "$DPS_INSTALL_ROLLBACK_ON_TEST_FAILURE" || return 0

  log "rolling back DPS service/socket because the Docker volume test failed"
  systemctl stop "$DPS_SERVICE_NAME.service" >/dev/null 2>&1 || true
  systemctl disable "$DPS_SERVICE_NAME.service" >/dev/null 2>&1 || true
  rm -f "/etc/systemd/system/$DPS_SERVICE_NAME.service"
  rm -f "$DPS_SOCKET"
  systemctl daemon-reload >/dev/null 2>&1 || true
  systemctl reset-failed "$DPS_SERVICE_NAME.service" >/dev/null 2>&1 || true
}

run_test_driver_or_rollback() {
  if test_driver; then
    return 0
  fi

  journalctl -u "$DPS_SERVICE_NAME.service" --no-pager -n 120 || true
  rollback_after_failed_test
  die "DPS service started, but Docker could not create and mount a DPS test volume"
}

print_next_steps() {
  cat <<EOF

======================================================================
 DPS INSTALL COMPLETED SUCCESSFULLY
======================================================================

Service:
  name:       $DPS_SERVICE_NAME
  status:     active
  socket:     $DPS_SOCKET

Storage:
  image root: $DPS_IMAGE_ROOT
  mount root: $DPS_MOUNT_ROOT/volumes

Defaults:
  size:       $DPS_DEFAULT_VOLUME_SIZE
  inodes:     $DPS_DEFAULT_VOLUME_INODES
  archives:   $DPS_ARCHIVE_POLICY

Docker Compose / Dokploy volume example:

volumes:
  pgdata:
    driver: dps
    driver_opts:
      size: 5G
      inodes: "500000"

Validate inside an app container:

  docker exec -it <container> df -h /path/to/volume
  docker exec -it <container> df -i /path/to/volume

Service logs:

  journalctl -u $DPS_SERVICE_NAME -f

Current DPS config:

  source /etc/dps/dpsd.env
  printf 'images=%s mount=%s root=%s\\n' "\$DPS_IMAGE_ROOT" "\$DPS_MOUNT_ROOT" "\$DPS_ROOT"

======================================================================

EOF
}

main() {
  trap 'on_error "$LINENO"' ERR
  banner
  as_root "$@"
  section "Check operating system"
  check_os
  section "Check CPU architecture"
  check_arch
  install_packages
  check_docker
  check_managed_plugin_conflict
  remove_stale_plugin_specs
  checkout_source
  build_binaries
  validate_mount_root
  write_environment
  write_systemd_service
  restart_service
  run_test_driver_or_rollback
  print_next_steps
}

main "$@"
