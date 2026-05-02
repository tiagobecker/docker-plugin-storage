#!/usr/bin/env bash
set -Eeuo pipefail

# Deep DPS uninstall for Linux/Dokploy hosts.
#
# This removes DPS host services, unmanaged plugin sockets/specs, managed DPS
# Docker plugins, Docker volumes that use the DPS driver, DPS loop mounts,
# loop devices backed by DPS image files, binaries, config, state, and checkout.
#
# It does not remove Docker, Dokploy, ordinary Docker volumes, or containers
# unrelated to DPS.

DPS_ENV_FILE="${DPS_ENV_FILE:-/etc/dps/dpsd.env}"
if [ -r "$DPS_ENV_FILE" ]; then
  # shellcheck disable=SC1090
  . "$DPS_ENV_FILE"
fi

DPS_INSTALL_DIR="${DPS_INSTALL_DIR:-/opt/docker-plugin-storage}"
DPS_ROOT="${DPS_ROOT:-/var/lib/dps}"
DPS_MOUNT_ROOT="${DPS_MOUNT_ROOT:-/mnt/dps}"
DPS_IMAGE_ROOT="${DPS_IMAGE_ROOT:-$DPS_ROOT/volume-images}"
DPS_SOCKET="${DPS_SOCKET:-/run/docker/plugins/dps.sock}"
DPS_SERVICE_NAMES="${DPS_SERVICE_NAMES:-dpsd dps}"
DPS_PLUGIN_SPEC_DIR="${DPS_PLUGIN_SPEC_DIR:-/etc/docker/plugins}"
DPS_UNINSTALL_CONFIRM="${DPS_UNINSTALL_CONFIRM:-}"
DPS_UNINSTALL_REMOVE_DOCKER_VOLUMES="${DPS_UNINSTALL_REMOVE_DOCKER_VOLUMES:-true}"
DPS_UNINSTALL_REMOVE_MANAGED_PLUGINS="${DPS_UNINSTALL_REMOVE_MANAGED_PLUGINS:-true}"
DPS_UNINSTALL_REMOVE_BINARIES="${DPS_UNINSTALL_REMOVE_BINARIES:-true}"
DPS_UNINSTALL_DETACH_LOOPS="${DPS_UNINSTALL_DETACH_LOOPS:-true}"
DPS_UNINSTALL_LAZY_UNMOUNT="${DPS_UNINSTALL_LAZY_UNMOUNT:-false}"
DPS_UNINSTALL_RESTART_DOCKER="${DPS_UNINSTALL_RESTART_DOCKER:-false}"

log() {
  printf '[dps-uninstall] %s\n' "$*"
}

warn() {
  printf '[dps-uninstall] WARN: %s\n' "$*" >&2
}

die() {
  printf '[dps-uninstall] ERROR: %s\n' "$*" >&2
  exit 1
}

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"
}

have_cmd() {
  command -v "$1" >/dev/null 2>&1
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

safe_path() {
  case "$1" in
    ""|"/"|"/bin"|"/boot"|"/dev"|"/etc"|"/home"|"/lib"|"/lib64"|"/mnt"|"/opt"|"/proc"|"/root"|"/run"|"/sbin"|"/srv"|"/sys"|"/tmp"|"/usr"|"/usr/bin"|"/usr/local"|"/usr/local/bin"|"/var"|"/var/lib"|"/var/run")
      return 1
      ;;
    *)
      return 0
      ;;
  esac
}

remove_path() {
  path="$1"
  safe_path "$path" || die "refusing to remove unsafe path: $path"
  if [ -e "$path" ] || [ -L "$path" ]; then
    log "removing $path"
    rm -rf "$path"
  fi
}

confirm_uninstall() {
  if [ "$DPS_UNINSTALL_CONFIRM" = "erase-dps" ]; then
    return 0
  fi

  cat <<EOF
[dps-uninstall] This will remove DPS from this host.

It may remove:
  Docker volumes using driver: dps or dps:*
  managed Docker plugins named like dps or docker-plugin-storage
  systemd services: $DPS_SERVICE_NAMES
  env/config: $DPS_ENV_FILE and /etc/dps
  socket: $DPS_SOCKET
  plugin specs under: $DPS_PLUGIN_SPEC_DIR
  root: $DPS_ROOT
  image root: $DPS_IMAGE_ROOT
  mount root: $DPS_MOUNT_ROOT
  install dir: $DPS_INSTALL_DIR
  binaries: /usr/local/bin/dpsd and /usr/local/bin/dpsctl

It will not remove Docker, Dokploy, local Docker volumes, or unrelated containers.

Type exactly 'erase-dps' to continue.
EOF

  if [ ! -t 0 ]; then
    die "non-interactive uninstall requires DPS_UNINSTALL_CONFIRM=erase-dps"
  fi
  read -r answer
  [ "$answer" = "erase-dps" ] || die "uninstall cancelled"
}

docker_available() {
  have_cmd docker && docker version >/dev/null 2>&1
}

docker_volume_driver() {
  docker volume inspect -f '{{.Driver}}' "$1" 2>/dev/null || true
}

is_dps_driver() {
  case "$1" in
    dps|dps:*|docker-plugin-storage|docker-plugin-storage:*) return 0 ;;
    *) return 1 ;;
  esac
}

remove_dps_docker_volumes() {
  is_true "$DPS_UNINSTALL_REMOVE_DOCKER_VOLUMES" || return 0
  docker_available || {
    warn "Docker is not reachable; skipping Docker volume metadata cleanup"
    return 0
  }

  log "removing Docker volumes that use the DPS driver"
  found="false"
  while IFS= read -r volume; do
    [ "$volume" != "" ] || continue
    driver="$(docker_volume_driver "$volume")"
    if is_dps_driver "$driver"; then
      found="true"
      log "removing Docker volume $volume using driver $driver"
      if ! docker volume rm "$volume"; then
        warn "could not remove Docker volume $volume; it may still be used by a container"
        docker ps -a --filter "volume=$volume" --format '  {{.ID}} {{.Names}} {{.Status}}' || true
      fi
    fi
  done < <(docker volume ls -q)
  if [ "$found" = "false" ]; then
    log "no Docker volumes with DPS driver found"
  fi
}

remove_managed_plugins() {
  is_true "$DPS_UNINSTALL_REMOVE_MANAGED_PLUGINS" || return 0
  docker_available || {
    warn "Docker is not reachable; skipping managed Docker plugin cleanup"
    return 0
  }

  log "removing managed Docker plugins that look like DPS"
  while IFS= read -r plugin; do
    [ "$plugin" != "" ] || continue
    case "$plugin" in
      dps|dps:*|*/dps|*/dps:*|docker-plugin-storage|docker-plugin-storage:*|*/docker-plugin-storage|*/docker-plugin-storage:*)
        log "disabling/removing managed Docker plugin $plugin"
        docker plugin disable -f "$plugin" >/dev/null 2>&1 || true
        docker plugin rm -f "$plugin" >/dev/null 2>&1 || warn "could not remove managed Docker plugin $plugin"
        ;;
    esac
  done < <(docker plugin ls --format '{{.Name}}' 2>/dev/null || true)
}

stop_systemd_services() {
  have_cmd systemctl || return 0
  for service in $DPS_SERVICE_NAMES; do
    log "stopping systemd service $service if present"
    systemctl stop "$service.service" >/dev/null 2>&1 || true
    systemctl disable "$service.service" >/dev/null 2>&1 || true
  done
}

unmount_tree() {
  target="$1"
  [ -d "$target" ] || return 0
  have_cmd findmnt || {
    warn "findmnt not found; skipping mount discovery under $target"
    return 0
  }

  log "unmounting mounts under $target"
  mounts="$(
    findmnt -rn -o TARGET 2>/dev/null | while IFS= read -r mountpoint; do
      case "$mountpoint" in
        "$target"|"$target"/*) printf '%s\n' "$mountpoint" ;;
      esac
    done | sort -r
  )"

  if [ "$mounts" = "" ]; then
    log "no mounts found under $target"
    return 0
  fi

  while IFS= read -r mountpoint; do
    [ "$mountpoint" != "" ] || continue
    log "unmounting $mountpoint"
    if umount "$mountpoint" 2>/dev/null; then
      continue
    fi
    if is_true "$DPS_UNINSTALL_LAZY_UNMOUNT"; then
      warn "normal unmount failed for $mountpoint; trying lazy unmount"
      umount -l "$mountpoint" 2>/dev/null || warn "lazy unmount also failed for $mountpoint"
    else
      warn "could not unmount $mountpoint; set DPS_UNINSTALL_LAZY_UNMOUNT=true only on disposable hosts if needed"
    fi
  done <<EOF
$mounts
EOF
}

detach_loop_for_file() {
  image="$1"
  [ -e "$image" ] || return 0
  have_cmd losetup || return 0

  losetup -j "$image" 2>/dev/null | while IFS= read -r line; do
    loopdev="${line%%:*}"
    [ "$loopdev" != "" ] || continue
    log "detaching loop device $loopdev for $image"
    losetup -d "$loopdev" 2>/dev/null || warn "could not detach $loopdev"
  done
}

detach_dps_loops() {
  is_true "$DPS_UNINSTALL_DETACH_LOOPS" || return 0
  have_cmd losetup || {
    warn "losetup not found; skipping loop cleanup"
    return 0
  }

  log "detaching loop devices backed by DPS image files"
  detach_loop_for_file "$DPS_ROOT/pool.img"

  if [ -d "$DPS_IMAGE_ROOT" ]; then
    find "$DPS_IMAGE_ROOT" -type f -name '*.img' -print 2>/dev/null | while IFS= read -r image; do
      detach_loop_for_file "$image"
    done
  fi

  losetup -a 2>/dev/null | grep -E "($DPS_ROOT|$DPS_IMAGE_ROOT|$DPS_MOUNT_ROOT)" || true
}

remove_plugin_specs_and_sockets() {
  log "removing unmanaged Docker plugin socket/spec files"
  rm -f "$DPS_SOCKET"

  if [ -d "$DPS_PLUGIN_SPEC_DIR" ]; then
    find "$DPS_PLUGIN_SPEC_DIR" -maxdepth 1 -type f \
      \( -name 'dps.spec' -o -name 'dps*.spec' -o -name 'docker-plugin-storage*.spec' \) \
      -print -delete 2>/dev/null || true
  fi
}

remove_systemd_files() {
  have_cmd systemctl || return 0
  log "removing systemd unit files"
  for service in $DPS_SERVICE_NAMES; do
    rm -f "/etc/systemd/system/$service.service"
    rm -rf "/etc/systemd/system/$service.service.d"
    systemctl reset-failed "$service.service" >/dev/null 2>&1 || true
  done
  systemctl daemon-reload >/dev/null 2>&1 || true
}

remove_files() {
  log "removing DPS files"
  rm -rf /etc/dps

  if is_true "$DPS_UNINSTALL_REMOVE_BINARIES"; then
    rm -f /usr/local/bin/dpsd /usr/local/bin/dpsctl
  fi

  remove_path "$DPS_ROOT"
  if [ "$DPS_IMAGE_ROOT" != "$DPS_ROOT/volume-images" ]; then
    remove_path "$DPS_IMAGE_ROOT"
  fi
  remove_path "$DPS_MOUNT_ROOT"
  remove_path "$DPS_INSTALL_DIR"
}

restart_docker_if_requested() {
  is_true "$DPS_UNINSTALL_RESTART_DOCKER" || {
    log "Docker restart skipped. Set DPS_UNINSTALL_RESTART_DOCKER=true if you want the script to restart Docker."
    return 0
  }

  have_cmd systemctl || {
    warn "systemctl not found; cannot restart Docker"
    return 0
  }

  log "restarting Docker"
  systemctl restart docker
  systemctl --no-pager --full status docker || true
}

print_summary() {
  cat <<EOF

[dps-uninstall] DPS uninstall completed.

Recommended validation:

  systemctl status docker --no-pager --full
  docker ps
  docker volume ls | grep -E 'dps|documenso|docuseal' || true
  findmnt -R "$DPS_MOUNT_ROOT" || true
  losetup -a | grep -E 'dps|volume-images|pool' || true

If this was a disposable Dokploy host and deploys still do not start, rebooting
the server is a reasonable next cleanup step after this uninstall.

EOF
}

main() {
  as_root "$@"
  confirm_uninstall
  remove_dps_docker_volumes
  remove_managed_plugins
  stop_systemd_services
  unmount_tree "$DPS_MOUNT_ROOT"
  detach_dps_loops
  remove_plugin_specs_and_sockets
  remove_systemd_files
  remove_files
  restart_docker_if_requested
  print_summary
}

main "$@"
