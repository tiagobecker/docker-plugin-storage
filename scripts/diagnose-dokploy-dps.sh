#!/usr/bin/env bash
set -uo pipefail

# Non-destructive diagnostics for Docker, Dokploy, and DPS on Linux hosts.
# It intentionally keeps going after failed commands so the output can show
# whether the failure is Docker daemon, Dokploy, or the DPS volume driver.

DPS_SERVICE_NAME="${DPS_SERVICE_NAME:-dpsd}"
DPS_ENV_FILE="${DPS_ENV_FILE:-/etc/dps/dpsd.env}"
DPS_SOCKET="${DPS_SOCKET:-/run/docker/plugins/dps.sock}"
DPS_MOUNT_ROOT="${DPS_MOUNT_ROOT:-/mnt/dps}"
DPS_VOLUME_FILTER="${DPS_VOLUME_FILTER:-documenso|docuseal|dps|postgres|mysql|mariadb|redis}"
DPS_DIAG_RUN_VOLUME_TEST="${DPS_DIAG_RUN_VOLUME_TEST:-false}"
DPS_DIAG_LOG_LINES="${DPS_DIAG_LOG_LINES:-160}"

section() {
  printf '\n========== %s ==========\n' "$*"
}

run() {
  printf '\n$ %s\n' "$*"
  "$@"
  code=$?
  if [ "$code" -ne 0 ]; then
    printf '[dps-diag] command exited with code %s\n' "$code"
  fi
  return 0
}

run_shell() {
  printf '\n$ %s\n' "$*"
  sh -c "$*"
  code=$?
  if [ "$code" -ne 0 ]; then
    printf '[dps-diag] command exited with code %s\n' "$code"
  fi
  return 0
}

has_cmd() {
  command -v "$1" >/dev/null 2>&1
}

bool_true() {
  case "$1" in
    true|1|yes|y|on) return 0 ;;
    *) return 1 ;;
  esac
}

print_header() {
  section "Host"
  run date -Is
  run uname -a
  if [ -r /etc/os-release ]; then
    run_shell "cat /etc/os-release"
  fi
  run id
  run uptime
}

print_systemd_state() {
  section "Systemd"
  if has_cmd systemctl; then
    run systemctl is-active docker
    run systemctl --no-pager --full status docker
    run systemctl is-active containerd
    run systemctl --no-pager --full status containerd
    run systemctl is-active "$DPS_SERVICE_NAME"
    run systemctl --no-pager --full status "$DPS_SERVICE_NAME"
  else
    printf '[dps-diag] systemctl not found\n'
  fi
}

print_docker_state() {
  section "Docker"
  if ! has_cmd docker; then
    printf '[dps-diag] docker command not found\n'
    return 0
  fi

  run docker version
  run docker info
  run docker ps -a --no-trunc
  run docker volume ls
  run docker network ls
  run docker plugin ls
}

print_swarm_and_dokploy_state() {
  section "Dokploy And Swarm"
  if ! has_cmd docker; then
    return 0
  fi

  run docker service ls
  run_shell "docker service ps dokploy --no-trunc 2>/dev/null || true"
  run_shell "docker service logs dokploy --tail '$DPS_DIAG_LOG_LINES' --no-trunc 2>/dev/null || true"
  run_shell "docker ps -a --format '{{.Names}}' | grep -E '^dokploy(\\.|-|$)' | while read -r c; do printf '\\n--- docker logs %s ---\\n' \"\$c\"; docker logs --tail '$DPS_DIAG_LOG_LINES' \"\$c\" 2>&1 || true; done"
}

print_dps_state() {
  section "DPS"
  if [ -r "$DPS_ENV_FILE" ]; then
    run_shell "cat '$DPS_ENV_FILE'"
    # shellcheck disable=SC1090
    . "$DPS_ENV_FILE"
  else
    printf '[dps-diag] %s not readable\n' "$DPS_ENV_FILE"
  fi

  DPS_SOCKET="${DPS_SOCKET:-/run/docker/plugins/dps.sock}"
  DPS_MOUNT_ROOT="${DPS_MOUNT_ROOT:-/mnt/dps}"
  DPS_IMAGE_ROOT="${DPS_IMAGE_ROOT:-/var/lib/dps/volume-images}"
  DPS_ROOT="${DPS_ROOT:-/var/lib/dps}"

  run_shell "ls -ld '$(dirname "$DPS_SOCKET")' '$DPS_SOCKET' 2>/dev/null || true"
  run_shell "ls -ld '$DPS_ROOT' '$DPS_IMAGE_ROOT' '$DPS_MOUNT_ROOT' 2>/dev/null || true"

  if has_cmd findmnt; then
    run findmnt -R "$DPS_MOUNT_ROOT"
  fi

  if has_cmd losetup; then
    run_shell "losetup -a | grep -Ei 'dps|volume-images|pool|documenso|docuseal' || true"
  fi
}

print_relevant_volumes() {
  section "Relevant Docker Volumes"
  if ! has_cmd docker; then
    return 0
  fi

  run_shell "docker volume ls --format '{{.Name}} {{.Driver}}' | grep -Ei '$DPS_VOLUME_FILTER' || true"
  run_shell "docker volume ls --format '{{.Name}}' | grep -Ei '$DPS_VOLUME_FILTER' | while read -r v; do printf '\\n--- docker volume inspect %s ---\\n' \"\$v\"; docker volume inspect \"\$v\" || true; done"
  run_shell "docker ps -a --format '{{.Names}} {{.Mounts}}' | grep -Ei '$DPS_VOLUME_FILTER' || true"
}

print_recent_logs() {
  section "Recent Logs"
  if has_cmd journalctl; then
    run journalctl -u docker -n "$DPS_DIAG_LOG_LINES" --no-pager --full
    run journalctl -u containerd -n "$DPS_DIAG_LOG_LINES" --no-pager --full
    run journalctl -u "$DPS_SERVICE_NAME" -n "$DPS_DIAG_LOG_LINES" --no-pager --full
  else
    printf '[dps-diag] journalctl not found\n'
  fi
}

run_optional_volume_test() {
  section "Optional DPS Volume Test"
  if ! bool_true "$DPS_DIAG_RUN_VOLUME_TEST"; then
    printf '[dps-diag] skipped. Re-run with DPS_DIAG_RUN_VOLUME_TEST=true to create and remove a small DPS test volume.\n'
    return 0
  fi

  if ! has_cmd docker; then
    printf '[dps-diag] docker command not found\n'
    return 0
  fi

  test_volume="dps_diag_test_$(date +%s)"
  run docker volume create --driver dps --opt size=64M --opt inodes=4096 "$test_volume"
  run docker run --rm -v "$test_volume:/data" alpine:3.22 sh -c "df -h /data; df -i /data"
  run docker volume rm "$test_volume"
}

print_next_steps() {
  section "How To Read This"
  cat <<'EOF'
If "docker version" or "docker info" cannot reach the daemon, fix Docker first:
  systemctl restart docker
  journalctl -u docker -n 200 --no-pager --full

If Docker is healthy but Dokploy fails, look at "Dokploy And Swarm" logs.

If manual DPS volumes work but a Dokploy deploy fails, inspect the real volume
name shown above. A common cause is an existing volume created earlier with the
local driver; Docker cannot change an existing volume from local to dps.

If the inspected volume shows "Driver": "local", remove the stopped app/service
and that volume, then redeploy with:
  driver: dps
  driver_opts:
    size: 2G
    inodes: "50000"
EOF
}

main() {
  print_header
  print_systemd_state
  print_docker_state
  print_swarm_and_dokploy_state
  print_dps_state
  print_relevant_volumes
  print_recent_logs
  run_optional_volume_test
  print_next_steps
}

main "$@"
