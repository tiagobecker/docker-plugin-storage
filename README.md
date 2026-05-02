# DPS - Docker Plugin Storage

DPS is a portable Docker volume driver for local volumes with per-volume size and inode limits, snapshots, restore, local backup, and S3-compatible backup.

The core storage model is intentionally simple:

```text
Docker volume
  -> DPS creates one ext4 filesystem image file
  -> DPS mounts that image through a loop device
  -> Docker receives a clean data subdirectory inside that mount
```

This works on ordinary Linux hosts that boot from ext4, which is the common case for VPS and cloud images. Docker application images do not need XFS or any special filesystem.
DPS keeps filesystem metadata such as `lost+found` outside the path returned to
Docker, so first-start database images such as Postgres can initialize into an
empty data directory.

## Features

- Docker `VolumeDriver` API over `/run/docker/plugins/dps.sock`.
- One filesystem image per Docker volume.
- Per-volume size limit visible through `df -h`.
- Per-volume inode limit visible through `df -i`.
- Default size and inode limits for Compose/Dokploy templates.
- Offline resize up and down with data-fit checks.
- Local snapshots with manifest, byte count, and SHA-256 verification.
- Local and S3-compatible backups with manifest, checksum, and read-back verification.
- Archive policies for databases: `offline`, `crash-consistent`, and `hooked`.

## Compose Example

```yaml
services:
  postgres:
    image: postgres:16
    environment:
      POSTGRES_PASSWORD: example
    volumes:
      - pgdata:/var/lib/postgresql/data

volumes:
  pgdata:
    driver: dps
    driver_opts:
      size: 5G
      inodes: "500000"
```

Validate from inside the container:

```sh
docker exec -it <container> df -h /var/lib/postgresql/data
docker exec -it <container> df -i /var/lib/postgresql/data
```

The filesystem will usually appear as `/dev/loopX`, because DPS mounted the volume image through a loop device.

## Upgrade Note

This architecture is intentionally different from earlier DPS MVP builds that experimented with multiple storage backends. Recreate test volumes after upgrading to this version. For production data, back up with the old version first, deploy this version, create new DPS volumes, and restore into them.

## Ubuntu 24.04 arm64 / Dokploy

For Ubuntu 24.04 arm64 servers managed by Dokploy:

```sh
curl -fsSL https://raw.githubusercontent.com/tiagobecker/docker-plugin-storage/main/scripts/install-ubuntu-24.04-arm64-dokploy.sh -o install-dps.sh
sudo bash install-dps.sh
```

The installer:

- installs host packages required for image-backed volumes;
- refuses to continue if an old managed Docker plugin named `dps` is present;
- removes stale DPS plugin spec files before starting the host service;
- builds `dpsd` and `dpsctl` for linux/arm64;
- installs a systemd service named `dpsd`;
- uses `/var/lib/dps/volume-images` for volume image files by default;
- uses `/mnt/dps/volumes/<volume>/data` for Docker-visible mountpoints;
- sets default volume limits to `5G` and `200000` inodes;
- creates a small test volume and runs `df -h` and `df -i`;
- rolls back the service/socket if that test volume cannot be created and mounted.

Run it on every Dokploy-managed Docker host that should support `driver: dps`.

To uninstall DPS software without removing Dokploy apps, containers, Docker
volumes, or DPS volume image data:

```sh
curl -fsSL https://raw.githubusercontent.com/tiagobecker/docker-plugin-storage/main/scripts/uninstall-dps-host.sh -o uninstall-dps-host.sh
sudo bash uninstall-dps-host.sh
```

On a test host where Docker should be restarted after uninstalling DPS:

```sh
sudo env DPS_UNINSTALL_RESTART_DOCKER=true bash uninstall-dps-host.sh
```

To remove DPS data too, opt in explicitly:

```sh
sudo env DPS_UNINSTALL_CONFIRM=erase-dps DPS_UNINSTALL_REMOVE_DATA=true bash uninstall-dps-host.sh
```

If Dokploy shows only `Error starting compose`, collect host diagnostics:

```sh
curl -fsSL https://raw.githubusercontent.com/tiagobecker/docker-plugin-storage/main/scripts/diagnose-dokploy-dps.sh -o diagnose-dokploy-dps.sh
sudo bash diagnose-dokploy-dps.sh
```

To include a small create/mount/remove test of the DPS driver:

```sh
sudo env DPS_DIAG_RUN_VOLUME_TEST=true bash diagnose-dokploy-dps.sh
```

## Host Service

Build:

```sh
make build
```

Run locally as an unmanaged Docker plugin:

```sh
sudo ./bin/dpsd \
  --root /var/lib/dps \
  --mount-root /mnt/dps \
  --image-root /var/lib/dps/volume-images \
  --default-volume-size 5G \
  --default-volume-inodes 200000 \
  --socket /run/docker/plugins/dps.sock
```

Important paths:

- `--root`: metadata, snapshots, temporary files.
- `--image-root`: ext4 image files that hold real volume data.
- `--mount-root`: mountpoints returned to Docker.

## Configuration

Environment variables:

| Variable | Default | Purpose |
| --- | --- | --- |
| `DPS_ROOT` | `/var/lib/dps` | Metadata, snapshots, temporary files. |
| `DPS_IMAGE_ROOT` | `<DPS_ROOT>/volume-images` | Volume image files. Put this on the storage path where data should live. |
| `DPS_MOUNT_ROOT` | `/mnt/dps` | Docker-visible mount root. Volumes mount under `<DPS_MOUNT_ROOT>/volumes`. |
| `DPS_DEFAULT_VOLUME_SIZE` | `5G` in packaged daemon/install script | Default size when Compose omits `driver_opts.size`. |
| `DPS_DEFAULT_VOLUME_INODES` | `200000` in packaged daemon/install script | Default inode count when Compose omits `driver_opts.inodes`. |
| `DPS_ARCHIVE_POLICY` | `offline` through installer | Snapshot/backup consistency policy. |
| `DPS_PRE_ARCHIVE_HOOK` | empty | Hook command for `hooked` policy. |
| `DPS_POST_ARCHIVE_HOOK` | empty | Hook command for `hooked` policy. |
| `DPS_ARCHIVE_HOOK_TIMEOUT` | `10m` | Hook timeout. |
| `DPS_SOCKET` | `/run/docker/plugins/dps.sock` | Docker plugin socket. |

## Resize

Resize is offline: stop the workload or ensure Docker has unmounted the volume first.

```sh
dpsctl resize pgdata 40G 800000
```

Behavior:

- increasing size uses filesystem growth when possible;
- decreasing size recreates the image, restores data, and refuses the operation unless current usage fits with at least 10% headroom;
- changing inode count recreates the image because ext4 inode count is fixed at filesystem creation;
- mounted volumes are refused for resize.

## Snapshots

Create a snapshot:

```sh
dpsctl snapshot pgdata before-upgrade
```

Restore:

```sh
dpsctl restore before-upgrade pgdata
```

Snapshots are local tar.gz archives with a manifest and SHA-256 checksum. DPS verifies the snapshot before restore and refuses restore into a mounted volume.

## Backups

Back up a snapshot locally:

```sh
dpsctl backup before-upgrade /srv/dps-backups
```

Stream a volume backup without first writing a snapshot file:

```sh
dpsctl backup-volume pgdata /srv/dps-backups pgdata-manual
```

S3-compatible target:

```sh
export AWS_ACCESS_KEY_ID=...
export AWS_SECRET_ACCESS_KEY=...
export AWS_REGION=us-east-1

dpsctl backup-volume pgdata s3://bucket/prod pgdata-manual
dpsctl backup-verify s3://bucket/prod pgdata-manual
```

For MinIO or another compatible endpoint:

```sh
export AWS_ENDPOINT_URL=https://minio.example.com
export AWS_S3_FORCE_PATH_STYLE=true
```

## Database Consistency

Default installer policy is `offline`: DPS refuses `snapshot` and `backup-volume` while the volume has active Docker references.

For databases, prefer one of these flows:

- Stop writes in Dokploy, snapshot or backup, then start the app.
- Use `DPS_ARCHIVE_POLICY=hooked` with tested pre/post hooks that quiesce the database.
- Use logical database backups in addition to DPS volume backups for critical production databases.

Mounted capture is explicit:

```sh
dpsctl --archive-policy crash-consistent backup-volume pgdata /srv/dps-backups pgdata-crash
```

Hooked capture:

```sh
dpsctl --archive-policy hooked \
  --pre-archive-hook '/etc/dps/hooks/postgres-pre.sh' \
  --post-archive-hook '/etc/dps/hooks/postgres-post.sh' \
  backup-volume pgdata s3://bucket/prod pgdata-hooked
```

## Managed Docker Plugin

Build and create locally:

```sh
make plugin-create
docker plugin enable dps:latest
```

Managed plugin configuration:

```sh
docker plugin set dps:latest DPS_DEFAULT_VOLUME_SIZE=5G
docker plugin set dps:latest DPS_DEFAULT_VOLUME_INODES=200000
docker plugin set dps:latest DPS_ARCHIVE_POLICY=offline
docker plugin enable dps:latest
```

Compose with managed plugin name:

```yaml
volumes:
  pgdata:
    driver: dps:latest
    driver_opts:
      size: 5G
      inodes: "500000"
```

The managed plugin requests `CAP_SYS_ADMIN` and device access because loop mounts require privileged kernel operations.
Its package declares `/dev/loop-control` and `/dev/loop0` through `/dev/loop7`
explicitly, which is required on Docker Desktop managed plugin tests.

## Operational Notes

- Monitor real disk usage under `DPS_IMAGE_ROOT`.
- Sparse images can overcommit the host if total configured volume sizes exceed available disk.
- Monitor active loop devices and mount count on hosts with many volumes.
- For heavy databases, keep `DPS_IMAGE_ROOT` on fast local SSD/NVMe storage.
- S3-compatible storage is for backup/sync, not as a live POSIX filesystem.

## Current Scope

This is still an MVP. Good next steps:

- integration tests in a Linux VM for real loop-mount create/resize/remove;
- total-capacity overcommit guard for `DPS_IMAGE_ROOT`;
- safer migration tooling between DPS state roots;
- published multi-arch managed plugin artifacts.
