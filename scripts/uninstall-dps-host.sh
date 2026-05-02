#!/usr/bin/env bash
set -Eeuo pipefail

# Conservative DPS uninstall for Linux/Dokploy hosts.
#
# Default behavior removes only DPS software/integration points:
# service, sockets/specs, managed plugin object, binaries, config, and checkout.
# It does not remove Dokploy apps, containers, Docker volumes, or DPS volume data.

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
DPS_UNINSTALL_FORCE="${DPS_UNINSTALL_FORCE:-false}"
DPS_UNINSTALL_REMOVE_DATA="${DPS_UNINSTALL_REMOVE_DATA:-false}"
DPS_UNINSTALL_REMOVE_DOCKER_VOLUMES="${DPS_UNINSTALL_REMOVE_DOCKER_VOLUMES:-false}"
DPS_UNINSTALL_REMOVE_MANAGED_PLUGINS="${DPS_UNINSTALL_REMOVE_MANAGED_PLUGINS:-true}"
DPS_UNINSTALL_REMOVE_BINARIES="${DPS_UNINSTALL_REMOVE_BINARIES:-true}"
DPS_UNINSTALL_LAZY_UNMOUNT="${DPS_UNINSTALL_LAZY_UNMOUNT:-false}"
DPS_UNINSTALL_RESTART_DOCKER="${DPS_UNINSTALL_RESTART_DOCKER:-false}"
CURRENT_STEP="starting"

banner() {
  cat <<'EOF'
======================================================================
 DPS Uninstaller
======================================================================
 Removes DPS from this host without touching Dokploy deployments by
 default. Docker volumes and DPS image data are preserved unless you
 explicitly enable data removal.
======================================================================
EOF
}

section() {
  CURRENT_STEP="$1"
  printf '\n[dps-uninstall] == %s ==\n' "$CURRENT_STEP"
}

log() {
  printf '[dps-uninstall] %s\n' "$*"
}

success() {
  printf '[dps-uninstall] OK: %s\n' "$*"
}

warn() {
  printf '[dps-uninstall] WARN: %s\n' "$*" >&2
}

die() {
  cat >&2 <<EOF

======================================================================
 DPS UNINSTALL FAILED
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
 DPS UNINSTALL FAILED
======================================================================
 Step: $CURRENT_STEP
 Line: $line
 Exit code: $code

Useful diagnostics:
  systemctl status docker --no-pager --full
  systemctl status dpsd --no-pager --full
  journalctl -u dpsd -n 120 --no-pager --full
======================================================================
EOF
  exit "$code"
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

list_dps_volumes() {
  docker_available || return 0
  docker volume ls -q 2>/dev/null | while IFS= read -r volume; do
    [ "$volume" != "" ] || continue
    driver="$(docker_volume_driver "$volume")"
    if is_dps_driver "$driver"; then
      printf '%s %s\n' "$volume" "$driver"
    fi
  done
}

list_dps_containers() {
  docker_available || return 0
  list_dps_volumes | while read -r volume _driver; do
    [ "$volume" != "" ] || continue
    docker ps -a --filter "volume=$volume" --format "{{.ID}} {{.Names}} {{.Status}} volume=$volume" 2>/dev/null || true
  done | sort -u
}

confirm_uninstall() {
  if [ "$DPS_UNINSTALL_CONFIRM" = "erase-dps" ]; then
    return 0
  fi

  cat <<EOF
[dps-uninstall] This will remove DPS software from this host.

Will remove:
  systemd services: $DPS_SERVICE_NAMES
  env/config:       $DPS_ENV_FILE and /etc/dps
  socket:           $DPS_SOCKET
  plugin specs:     $DPS_PLUGIN_SPEC_DIR/dps*.spec
  install dir:      $DPS_INSTALL_DIR
  binaries:         /usr/local/bin/dpsd and /usr/local/bin/dpsctl

Will preserve by default:
  Dokploy apps and containers
  Docker volumes
  DPS image data under: $DPS_IMAGE_ROOT
  DPS state under:      $DPS_ROOT
  DPS mount root:       $DPS_MOUNT_ROOT

Dangerous options, disabled by default:
  DPS_UNINSTALL_REMOVE_DATA=true
  DPS_UNINSTALL_REMOVE_DOCKER_VOLUMES=true

Type exactly 'erase-dps' to continue.
EOF

  if [ ! -t 0 ]; then
    die "non-interactive uninstall requires DPS_UNINSTALL_CONFIRM=erase-dps"
  fi
  read -r answer
  [ "$answer" = "erase-dps" ] || die "uninstall cancelled"
}

print_detected_state() {
  section "Detected DPS state"
  if ! docker_available; then
    warn "Docker is not reachable; Docker volume/plugin inspection skipped"
    return 0
  fi

  volumes="$(list_dps_volumes || true)"
  containers="$(list_dps_containers || true)"

  if [ "$volumes" = "" ]; then
    success "No Docker volumes using the DPS driver were detected"
  else
    printf '[dps-uninstall] Docker volumes using DPS driver:\n%s\n' "$volumes"
  fi

  if [ "$containers" = "" ]; then
    success "No containers referencing DPS volumes were detected"
  else
    printf '[dps-uninstall] Containers referencing DPS volumes:\n%s\n' "$containers"
    if ! is_true "$DPS_UNINSTALL_FORCE"; then
      die "DPS volumes are still referenced by containers. Stop/remove the app in Dokploy first, or set DPS_UNINSTALL_FORCE=true if you understand the impact."
    fi
  fi
}

remove_dps_docker_volumes() {
  is_true "$DPS_UNINSTALL_REMOVE_DOCKER_VOLUMES" || {
    log "Docker volume removal skipped; DPS_UNINSTALL_REMOVE_DOCKER_VOLUMES=false"
    return 0
  }
  docker_available || {
    warn "Docker is not reachable; skipping Docker volume metadata cleanup"
    return 0
  }

  section "Remove DPS Docker volumes"
  list_dps_volumes | while read -r volume driver; do
    [ "$volume" != "" ] || continue
    log "removing Docker volume $volume using driver $driver"
    docker volume rm "$volume" || warn "could not remove Docker volume $volume"
  done
}

remove_managed_plugins() {
  is_true "$DPS_UNINSTALL_REMOVE_MANAGED_PLUGINS" || return 0
  docker_available || {
    warn "Docker is not reachable; skipping managed Docker plugin cleanup"
    return 0
  }

  section "Remove managed Docker plugins"
  found="false"
  while IFS= read -r plugin; do
    [ "$plugin" != "" ] || continue
    case "$plugin" in
      dps|dps:*|*/dps|*/dps:*|docker-plugin-storage|docker-plugin-storage:*|*/docker-plugin-storage|*/docker-plugin-storage:*)
        found="true"
        log "disabling/removing managed Docker plugin $plugin"
        docker plugin disable -f "$plugin" >/dev/null 2>&1 || true
        docker plugin rm -f "$plugin" >/dev/null 2>&1 || warn "could not remove managed Docker plugin $plugin"
        ;;
    esac
  done < <(docker plugin ls --format '{{.Name}}' 2>/dev/null || true)
  if [ "$found" = "false" ]; then
    success "No managed DPS plugin found"
  fi
}

stop_systemd_services() {
  section "Stop systemd services"
  have_cmd systemctl || return 0
  for service in $DPS_SERVICE_NAMES; do
    log "stopping systemd service $service if present"
    systemctl stop "$service.service" >/dev/null 2>&1 || true
    systemctl disable "$service.service" >/dev/null 2>&1 || true
  done
  success "DPS systemd services stopped/disabled when present"
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
    success "No mounts found under $target"
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
      warn "could not unmount $mountpoint"
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
}

remove_plugin_specs_and_sockets() {
  section "Remove plugin sockets/specs"
  rm -f "$DPS_SOCKET"

  if [ -d "$DPS_PLUGIN_SPEC_DIR" ]; then
    find "$DPS_PLUGIN_SPEC_DIR" -maxdepth 1 -type f \
      \( -name 'dps.spec' -o -name 'dps*.spec' -o -name 'docker-plugin-storage*.spec' \) \
      -print -delete 2>/dev/null || true
  fi
  success "DPS sockets/specs removed when present"
}

remove_systemd_files() {
  section "Remove systemd unit files"
  have_cmd systemctl || return 0
  for service in $DPS_SERVICE_NAMES; do
    rm -f "/etc/systemd/system/$service.service"
    rm -rf "/etc/systemd/system/$service.service.d"
    systemctl reset-failed "$service.service" >/dev/null 2>&1 || true
  done
  systemctl daemon-reload >/dev/null 2>&1 || true
  success "DPS systemd unit files removed when present"
}

remove_software_files() {
  section "Remove DPS software files"
  rm -rf /etc/dps

  if is_true "$DPS_UNINSTALL_REMOVE_BINARIES"; then
    rm -f /usr/local/bin/dpsd /usr/local/bin/dpsctl
  fi

  remove_path "$DPS_INSTALL_DIR"
  success "DPS config, binaries, and checkout removed when present"
}

remove_data_if_requested() {
  is_true "$DPS_UNINSTALL_REMOVE_DATA" || {
    log "DPS data removal skipped; DPS_UNINSTALL_REMOVE_DATA=false"
    return 0
  }

  section "Remove DPS data"
  warn "DPS_UNINSTALL_REMOVE_DATA=true: removing DPS image/state/mount data"
  unmount_tree "$DPS_MOUNT_ROOT"
  detach_dps_loops
  remove_path "$DPS_ROOT"
  if [ "$DPS_IMAGE_ROOT" != "$DPS_ROOT/volume-images" ]; then
    remove_path "$DPS_IMAGE_ROOT"
  fi
  remove_path "$DPS_MOUNT_ROOT"
}

restart_docker_if_requested() {
  is_true "$DPS_UNINSTALL_RESTART_DOCKER" || {
    log "Docker restart skipped; DPS_UNINSTALL_RESTART_DOCKER=false"
    return 0
  }

  have_cmd systemctl || {
    warn "systemctl not found; cannot restart Docker"
    return 0
  }

  section "Restart Docker"
  systemctl restart docker
  success "Docker restarted"
}

print_summary() {
  cat <<EOF

======================================================================
 DPS UNINSTALL COMPLETED SUCCESSFULLY
======================================================================

Removed:
  DPS service/socket/spec/config/binaries/checkout when present.

Preserved by default:
  Docker volumes
  Dokploy apps and containers
  DPS volume image data: $DPS_IMAGE_ROOT
  DPS state root:        $DPS_ROOT

Validation:
  systemctl status docker --no-pager --full
  docker ps
  docker volume ls
  docker plugin ls

To remove DPS data too, run explicitly:
  sudo env DPS_UNINSTALL_CONFIRM=erase-dps DPS_UNINSTALL_REMOVE_DATA=true bash uninstall-dps-host.sh

======================================================================

EOF
}

main() {
  trap 'on_error "$LINENO"' ERR
  banner
  as_root "$@"
  confirm_uninstall
  print_detected_state
  remove_dps_docker_volumes
  remove_managed_plugins
  stop_systemd_services
  remove_plugin_specs_and_sockets
  remove_systemd_files
  remove_software_files
  remove_data_if_requested
  restart_docker_if_requested
  print_summary
}

main "$@"
