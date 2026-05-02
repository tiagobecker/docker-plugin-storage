#!/usr/bin/env bash
set -Eeuo pipefail

# Destructive DPS reset for disposable Linux/Dokploy test hosts.
# It removes DPS service files, binaries, metadata, images, mountpoints,
# old plugin specs, sockets, and the local source checkout.
#
# For a deeper cleanup that also removes Docker volume metadata using the DPS
# driver, managed Docker plugin instances, and loop devices, use:
# scripts/uninstall-dps-host.sh

DPS_INSTALL_DIR="${DPS_INSTALL_DIR:-/opt/docker-plugin-storage}"
DPS_ROOT="${DPS_ROOT:-/var/lib/dps}"
DPS_MOUNT_ROOT="${DPS_MOUNT_ROOT:-/mnt/dps}"
DPS_IMAGE_ROOT="${DPS_IMAGE_ROOT:-$DPS_ROOT/volume-images}"
DPS_SOCKET="${DPS_SOCKET:-/run/docker/plugins/dps.sock}"
DPS_SERVICE_NAME="${DPS_SERVICE_NAME:-dpsd}"
DPS_PLUGIN_SPEC="${DPS_PLUGIN_SPEC:-/etc/docker/plugins/dps.spec}"
DPS_RESET_CONFIRM="${DPS_RESET_CONFIRM:-}"
DPS_RESET_REMOVE_BINARIES="${DPS_RESET_REMOVE_BINARIES:-true}"

log() {
  printf '[dps-reset] %s\n' "$*"
}

die() {
  printf '[dps-reset] ERROR: %s\n' "$*" >&2
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

is_true() {
  case "$1" in
    true|1|yes|y|on) return 0 ;;
    *) return 1 ;;
  esac
}

confirm_reset() {
  if [ "$DPS_RESET_CONFIRM" = "erase-dps" ]; then
    return 0
  fi

  cat <<EOF
[dps-reset] This will remove DPS data and installation files:
  service:      $DPS_SERVICE_NAME
  root:         $DPS_ROOT
  image root:   $DPS_IMAGE_ROOT
  mount root:   $DPS_MOUNT_ROOT
  install dir:  $DPS_INSTALL_DIR
  socket:       $DPS_SOCKET
  plugin spec:  $DPS_PLUGIN_SPEC

Type exactly 'erase-dps' to continue.
EOF
  if [ ! -t 0 ]; then
    die "non-interactive reset requires DPS_RESET_CONFIRM=erase-dps"
  fi
  read -r answer
  [ "$answer" = "erase-dps" ] || die "reset cancelled"
}

stop_service() {
  log "stopping systemd service if present"
  systemctl stop "$DPS_SERVICE_NAME.service" >/dev/null 2>&1 || true
  systemctl disable "$DPS_SERVICE_NAME.service" >/dev/null 2>&1 || true
}

unmount_tree() {
  target="$1"
  [ -d "$target" ] || return 0
  need_cmd findmnt

  log "unmounting mounts below $target"
  mounts="$(
    findmnt -rn -o TARGET 2>/dev/null | while IFS= read -r mountpoint; do
      case "$mountpoint" in
        "$target"|"$target"/*) printf '%s\n' "$mountpoint" ;;
      esac
    done | sort -r
  )"
  [ "$mounts" != "" ] || return 0

  while IFS= read -r mountpoint; do
    [ "$mountpoint" != "" ] || continue
    if ! umount "$mountpoint" 2>/dev/null; then
      log "could not unmount $mountpoint; check for remaining containers/processes"
      return 1
    fi
  done <<EOF
$mounts
EOF
}

remove_path() {
  path="$1"
  case "$path" in
    ""|"/"|"/var"|"/var/lib"|"/mnt"|"/opt"|"/usr"|"/usr/local"|"/usr/local/bin"|"/etc"|"/etc/systemd"|"/etc/systemd/system"|"/run"|"/run/docker"|"/run/docker/plugins")
      die "refusing to remove unsafe path: $path"
      ;;
  esac
  if [ -e "$path" ] || [ -L "$path" ]; then
    log "removing $path"
    rm -rf "$path"
  fi
}

remove_files() {
  log "removing DPS service and socket files"
  rm -f "/etc/systemd/system/$DPS_SERVICE_NAME.service"
  rm -f "$DPS_SOCKET"
  rm -f "$DPS_PLUGIN_SPEC"
  rm -rf /etc/dps

  if is_true "$DPS_RESET_REMOVE_BINARIES"; then
    rm -f /usr/local/bin/dpsd /usr/local/bin/dpsctl
  fi

  systemctl daemon-reload >/dev/null 2>&1 || true
  systemctl reset-failed "$DPS_SERVICE_NAME.service" >/dev/null 2>&1 || true
}

print_next_steps() {
  cat <<EOF

[dps-reset] DPS reset completed.

Install the current portable image-backed DPS:

  curl -fsSL https://raw.githubusercontent.com/tiagobecker/docker-plugin-storage/main/scripts/install-ubuntu-24.04-arm64-dokploy.sh -o install-dps.sh
  sudo bash install-dps.sh

If Docker still lists old DPS volumes, remove them before redeploying apps:

  docker volume ls
  docker volume rm <old-volume>

EOF
}

main() {
  as_root "$@"
  confirm_reset
  stop_service
  unmount_tree "$DPS_MOUNT_ROOT"
  remove_files
  remove_path "$DPS_ROOT"
  if [ "$DPS_IMAGE_ROOT" != "$DPS_ROOT/volume-images" ]; then
    remove_path "$DPS_IMAGE_ROOT"
  fi
  remove_path "$DPS_MOUNT_ROOT"
  remove_path "$DPS_INSTALL_DIR"
  print_next_steps
}

main "$@"
